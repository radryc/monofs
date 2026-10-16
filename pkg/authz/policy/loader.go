package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// DefaultPollInterval is the default policy refresh interval. The design
// target is <=60s end-to-end propagation.
const DefaultPollInterval = 30 * time.Second

// Source fetches the compiled policy bundle and reports a change token.
type Source interface {
	// Fetch returns the bundle bytes and an opaque change token (etag).
	// Returning (nil, token, nil) means "unchanged".
	Fetch(ctx context.Context) (data []byte, etag string, err error)
	// Describe names the source for logs.
	Describe() string
}

// FileSource reads the compiled bundle from a local file (dev and tests).
type FileSource struct {
	Path string
}

// Fetch implements Source.
func (s FileSource) Fetch(_ context.Context) ([]byte, string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Describe implements Source.
func (s FileSource) Describe() string { return "file:" + s.Path }

// HTTPSource polls an HTTP endpoint (guardiand or the MonoFS router) for the
// compiled bundle.
type HTTPSource struct {
	URL    string
	Token  string
	Client *http.Client
}

// Fetch implements Source.
func (s HTTPSource) Fetch(ctx context.Context) ([]byte, string, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, "", err
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, resp.Header.Get("ETag"), nil
	case http.StatusOK:
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, "", err
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			etag = resp.Header.Get("X-Policy-Version")
		}
		return data, etag, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, "", fmt.Errorf("policy: fetch %s: HTTP %d: %s", s.URL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// Describe implements Source.
func (s HTTPSource) Describe() string { return "http:" + s.URL }

// Loader keeps the latest successfully fetched policy and polls the source.
// Fetch failures never drop the last good policy; consumers decide how to
// behave when Ready() is false (fail-closed when enforcement is on).
type Loader struct {
	source   Source
	interval time.Duration
	logf     func(format string, args ...any)

	mu       sync.RWMutex
	policy   *Policy
	etag     string
	loadedAt time.Time
	lastErr  error
	fetches  int
}

// NewLoader builds a loader. interval<=0 falls back to DefaultPollInterval.
func NewLoader(source Source, interval time.Duration, logf func(format string, args ...any)) *Loader {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Loader{source: source, interval: interval, logf: logf}
}

// Run refreshes immediately and then on every interval until ctx is done.
func (l *Loader) Run(ctx context.Context) {
	if l == nil || l.source == nil {
		return
	}
	if err := l.Refresh(ctx); err != nil {
		l.logf("access policy: initial load from %s failed: %v", l.source.Describe(), err)
	}
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.Refresh(ctx); err != nil {
				l.logf("access policy: refresh from %s failed: %v", l.source.Describe(), err)
			}
		}
	}
}

// Refresh performs one fetch and swaps the policy when it changed.
func (l *Loader) Refresh(ctx context.Context) error {
	data, etag, err := l.source.Fetch(ctx)
	if err != nil {
		l.mu.Lock()
		l.lastErr = err
		l.mu.Unlock()
		return err
	}
	l.mu.Lock()
	l.fetches++
	unchanged := data == nil || (etag != "" && etag == l.etag && l.policy != nil)
	if unchanged {
		l.lastErr = nil
		l.mu.Unlock()
		return nil
	}
	policy, perr := ParseBundle(data)
	if perr != nil {
		l.lastErr = perr
		l.mu.Unlock()
		return perr
	}
	oldVersion := ""
	if l.policy != nil {
		oldVersion = l.policy.Version()
	}
	l.policy = policy
	l.etag = etag
	l.loadedAt = time.Now()
	l.lastErr = nil
	l.mu.Unlock()
	if oldVersion != policy.Version() {
		l.logf("access policy: loaded version=%s source=%s", policy.Version(), l.source.Describe())
	}
	return nil
}

// Get returns the current policy or nil when none has been loaded yet.
func (l *Loader) Get() *Policy {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.policy
}

// Ready reports whether a policy has been loaded successfully.
func (l *Loader) Ready() bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.policy != nil
}

// LastError returns the most recent fetch/parse error.
func (l *Loader) LastError() error {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lastErr
}

// LoadedAt returns when the current policy was loaded.
func (l *Loader) LoadedAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.loadedAt
}

// Source describes the configured source.
func (l *Loader) Source() string {
	if l == nil || l.source == nil {
		return ""
	}
	return l.source.Describe()
}
