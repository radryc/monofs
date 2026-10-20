package repowatch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultLeaseDuration = 15 * time.Second
	defaultLeaseRenew    = 5 * time.Second
	leaseAPIVersion      = "coordination.k8s.io/v1"
	leaseKind            = "Lease"
	// Kubernetes Lease acquireTime/renewTime are metav1.MicroTime values.
	// The API rejects RFC3339Nano timestamps that are not exactly six
	// fractional digits, so always format with microseconds.
	leaseTimeLayout = "2006-01-02T15:04:05.000000Z07:00"
)

// LeaseConfig configures a Kubernetes Lease based elector. The zero value is
// disabled; DefaultLeaseConfig fills in the in-cluster defaults.
type LeaseConfig struct {
	Namespace string
	Name      string
	Identity  string
	Duration  time.Duration
	Renew     time.Duration
	APIServer string
	TokenFile string
	CAFile    string
	Insecure  bool
	Client    *http.Client
	// OnError is called when acquisition fails. It is intended for logging;
	// callers should keep it cheap and non-blocking.
	OnError func(error)
}

// Enabled reports whether enough configuration is present to attempt the
// Kubernetes API. Callers fall back to static ownership when disabled.
func (c LeaseConfig) Enabled() bool {
	return strings.TrimSpace(c.APIServer) != "" && strings.TrimSpace(c.Name) != ""
}

// DefaultLeaseConfig derives the in-cluster API endpoint and identity from the
// downward API / service-account files. It is safe to call outside Kubernetes:
// APIServer is left empty when the environment is not available.
func DefaultLeaseConfig(name string) LeaseConfig {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if host == "" {
		return LeaseConfig{Name: name}
	}
	if port == "" {
		port = "443"
	}
	return LeaseConfig{
		Namespace: serviceAccountNamespace(),
		Name:      name,
		Identity:  identity(),
		Duration:  defaultLeaseDuration,
		Renew:     defaultLeaseRenew,
		APIServer: "https://" + net.JoinHostPort(host, port),
		TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		CAFile:    "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
	}
}

func serviceAccountNamespace() string {
	if value := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); value != "" {
		return value
	}
	raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return strings.TrimSpace(string(raw))
	}
	return "default"
}

func identity() string {
	if value := strings.TrimSpace(os.Getenv("POD_NAME")); value != "" {
		return value
	}
	host, err := os.Hostname()
	if err == nil {
		return host
	}
	return "repowatch"
}

// Elector acquires a Kubernetes Lease and runs a leader callback while the
// lease is held. It retries acquisition until ctx is cancelled.
type Elector struct {
	cfg    LeaseConfig
	client *http.Client

	mu               sync.Mutex
	resourceVersion  string
	leaseTransitions int
}

// NewElector builds an elector. It validates the configuration and prepares
// the HTTP client (service-account CA, optional insecure mode).
func NewElector(cfg LeaseConfig) (*Elector, error) {
	if !cfg.Enabled() {
		return nil, errors.New("repowatch: Kubernetes lease is not configured")
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.Identity == "" {
		cfg.Identity = identity()
	}
	if cfg.Duration <= 0 {
		cfg.Duration = defaultLeaseDuration
	}
	if cfg.Renew <= 0 {
		cfg.Renew = defaultLeaseRenew
	}
	client := cfg.Client
	if client == nil {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12} // #nosec G402 -- controlled in-cluster endpoint
		if cfg.Insecure {
			tlsConfig.InsecureSkipVerify = true // #nosec G402 -- explicit opt-in
		} else if strings.TrimSpace(cfg.CAFile) != "" {
			if raw, err := os.ReadFile(filepath.Clean(cfg.CAFile)); err == nil {
				pool := x509.NewCertPool()
				if pool.AppendCertsFromPEM(raw) {
					tlsConfig.RootCAs = pool
				}
			}
		}
		timeout := cfg.Renew / 2
		if timeout < time.Second {
			timeout = time.Second
		}
		if timeout > cfg.Duration/2 {
			timeout = cfg.Duration / 2
		}
		client = &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		}
	}
	return &Elector{cfg: cfg, client: client}, nil
}

// Run blocks, acquiring the lease and invoking onLeader while leadership is
// held. onLost is called when a previously held lease is lost. The callback
// receives a context that is cancelled when leadership ends.
func (e *Elector) Run(ctx context.Context, onLeader func(context.Context), onLost func()) error {
	if e == nil {
		return errors.New("repowatch: nil elector")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		acquired, err := e.tryAcquire(ctx)
		if err != nil && e.cfg.OnError != nil {
			e.cfg.OnError(err)
		}
		if err == nil && acquired {
			leaderCtx, cancel := context.WithCancel(ctx)
			go onLeader(leaderCtx)
			lost := e.renewLoop(leaderCtx)
			cancel()
			if onLost != nil {
				onLost()
			}
			if !lost {
				return nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.cfg.Renew):
		}
	}
}

func (e *Elector) renewLoop(ctx context.Context) bool {
	ticker := time.NewTicker(e.cfg.Renew)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return true
		case <-ticker.C:
			if err := e.renew(ctx); err != nil {
				failures++
				if failures >= 2 {
					return true
				}
				continue
			}
			failures = 0
		}
	}
}

func (e *Elector) tryAcquire(ctx context.Context) (bool, error) {
	lease, status, err := e.getLease(ctx)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	if status == http.StatusNotFound {
		obj := e.leaseObject(now, now)
		if _, err := e.writeLease(ctx, http.MethodPost, obj); err != nil {
			return false, err
		}
		return true, nil
	}
	if lease == nil {
		return false, fmt.Errorf("repowatch: lease response was empty")
	}
	holder := strings.TrimSpace(lease.Spec.HolderIdentity)
	expired := leaseExpired(lease, now)
	if holder != "" && holder != e.cfg.Identity && !expired {
		e.mu.Lock()
		e.resourceVersion = lease.Metadata.ResourceVersion
		e.mu.Unlock()
		return false, nil
	}
	acquireTime := lease.Spec.AcquireTime
	transitions := lease.Spec.LeaseTransitions
	if holder != e.cfg.Identity {
		acquireTime = formatLeaseTime(now)
		transitions++
	}
	obj := e.leaseObject(now, now)
	obj.Spec.AcquireTime = normalizeLeaseTime(acquireTime, now)
	obj.Spec.LeaseTransitions = transitions
	obj.Metadata.ResourceVersion = lease.Metadata.ResourceVersion
	if _, err := e.writeLease(ctx, http.MethodPut, obj); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Elector) renew(ctx context.Context) error {
	now := time.Now().UTC()
	lease, _, err := e.getLease(ctx)
	if err != nil {
		return err
	}
	if lease == nil || strings.TrimSpace(lease.Spec.HolderIdentity) != e.cfg.Identity {
		return fmt.Errorf("repowatch: lease is no longer held by %s", e.cfg.Identity)
	}
	obj := e.leaseObject(now, now)
	obj.Spec.AcquireTime = normalizeLeaseTime(lease.Spec.AcquireTime, now)
	obj.Spec.LeaseTransitions = lease.Spec.LeaseTransitions
	obj.Metadata.ResourceVersion = lease.Metadata.ResourceVersion
	_, err = e.writeLease(ctx, http.MethodPut, obj)
	return err
}

func (e *Elector) leaseObject(acquire, renew time.Time) *leaseObject {
	return &leaseObject{
		APIVersion: leaseAPIVersion,
		Kind:       leaseKind,
		Metadata: leaseMetadata{
			Name:      e.cfg.Name,
			Namespace: e.cfg.Namespace,
		},
		Spec: leaseSpec{
			HolderIdentity:       e.cfg.Identity,
			LeaseDurationSeconds: int(e.cfg.Duration.Seconds()),
			AcquireTime:          formatLeaseTime(acquire),
			RenewTime:            formatLeaseTime(renew),
		},
	}
}

func (e *Elector) leaseCollectionURL() string {
	return strings.TrimRight(e.cfg.APIServer, "/") +
		"/apis/coordination.k8s.io/v1/namespaces/" + url.PathEscape(e.cfg.Namespace) +
		"/leases"
}

func (e *Elector) leaseURL() string {
	return e.leaseCollectionURL() + "/" + url.PathEscape(e.cfg.Name)
}

func (e *Elector) getLease(ctx context.Context) (*leaseObject, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.leaseURL(), nil)
	if err != nil {
		return nil, 0, err
	}
	e.authorize(req)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, resp.StatusCode, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("repowatch: get lease HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var lease leaseObject
	if err := json.Unmarshal(body, &lease); err != nil {
		return nil, resp.StatusCode, err
	}
	e.mu.Lock()
	e.resourceVersion = lease.Metadata.ResourceVersion
	e.mu.Unlock()
	return &lease, resp.StatusCode, nil
}

func (e *Elector) writeLease(ctx context.Context, method string, obj *leaseObject) (*leaseObject, error) {
	payload, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	target := e.leaseURL()
	if method == http.MethodPost {
		target = e.leaseCollectionURL()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	e.authorize(req)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("repowatch: %s lease HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out leaseObject
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

func (e *Elector) authorize(req *http.Request) {
	if strings.TrimSpace(e.cfg.TokenFile) == "" {
		return
	}
	token, err := os.ReadFile(filepath.Clean(e.cfg.TokenFile))
	if err != nil {
		return
	}
	if value := strings.TrimSpace(string(token)); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
}

func formatLeaseTime(t time.Time) string {
	return t.UTC().Format(leaseTimeLayout)
}

func parseLeaseTime(value string) (time.Time, error) {
	if t, err := time.Parse(leaseTimeLayout, value); err == nil {
		return t, nil
	}
	// Tolerate leases written by earlier versions with nanosecond precision.
	return time.Parse(time.RFC3339Nano, value)
}

func normalizeLeaseTime(value string, fallback time.Time) string {
	if t, err := parseLeaseTime(value); err == nil {
		return formatLeaseTime(t)
	}
	return formatLeaseTime(fallback)
}

func leaseExpired(lease *leaseObject, now time.Time) bool {
	if lease == nil || strings.TrimSpace(lease.Spec.HolderIdentity) == "" {
		return true
	}
	renew, err := parseLeaseTime(lease.Spec.RenewTime)
	if err != nil {
		return true
	}
	duration := time.Duration(lease.Spec.LeaseDurationSeconds) * time.Second
	if duration <= 0 {
		duration = defaultLeaseDuration
	}
	return now.After(renew.Add(duration))
}

type leaseObject struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   leaseMetadata `json:"metadata"`
	Spec       leaseSpec     `json:"spec"`
}

type leaseMetadata struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type leaseSpec struct {
	HolderIdentity       string `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds int    `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          string `json:"acquireTime,omitempty"`
	RenewTime            string `json:"renewTime,omitempty"`
	LeaseTransitions     int    `json:"leaseTransitions,omitempty"`
}
