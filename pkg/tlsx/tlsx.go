// Package tlsx provides shared TLS/mTLS configuration, hot-reloading certificate
// management, SPIFFE URI SAN peer verification, and gRPC transport credentials.
package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
)

// ServerConfig configures a TLS or mTLS server.
type ServerConfig struct {
	// CertFile is the path to the PEM-encoded server certificate.
	CertFile string
	// KeyFile is the path to the PEM-encoded server private key.
	KeyFile string
	// CAFile is the path to the PEM-encoded CA bundle used to verify client certificates.
	// Required when RequireClientCert is true.
	CAFile string
	// RequireClientCert enforces mutual TLS by requiring and verifying client certificates.
	RequireClientCert bool
	// MinVersion sets the minimum TLS version (defaults to tls.VersionTLS12;
	// tls.VersionTLS13 is also supported).
	MinVersion uint16
	// AllowedSPIFFEIDs is an optional allowlist of SPIFFE URI SANs. When non-empty,
	// at least one URI SAN on the verified peer certificate must match.
	AllowedSPIFFEIDs []string
}

// ClientConfig configures a TLS or mTLS client.
type ClientConfig struct {
	// CertFile is the path to the PEM-encoded client certificate (optional for one-way TLS).
	CertFile string
	// KeyFile is the path to the PEM-encoded client private key (optional for one-way TLS).
	KeyFile string
	// CAFile is the path to the PEM-encoded CA bundle used to verify the server certificate.
	CAFile string
	// ServerName overrides the server name used for hostname verification.
	ServerName string
	// MinVersion sets the minimum TLS version (defaults to tls.VersionTLS12;
	// tls.VersionTLS13 is also supported).
	MinVersion uint16
	// ExpectedSPIFFEIDs is an optional allowlist of SPIFFE URI SANs. When non-empty,
	// at least one URI SAN on the verified server certificate must match.
	ExpectedSPIFFEIDs []string
}

// certReloader hot-reloads a TLS keypair from disk whenever the certificate or
// key file modification time changes.
type certReloader struct {
	certFile string
	keyFile  string

	mu          sync.RWMutex
	cert        *tls.Certificate
	certModTime time.Time
	keyModTime  time.Time
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("tlsx: both CertFile and KeyFile must be provided")
	}
	r := &certReloader{
		certFile: certFile,
		keyFile:  keyFile,
	}
	if _, err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) reload() (*tls.Certificate, error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return nil, fmt.Errorf("tlsx: stat cert file %q: %w", r.certFile, err)
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsx: stat key file %q: %w", r.keyFile, err)
	}

	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsx: load keypair (%q, %q): %w", r.certFile, r.keyFile, err)
	}
	if pair.Leaf == nil && len(pair.Certificate) > 0 {
		if leaf, parseErr := x509.ParseCertificate(pair.Certificate[0]); parseErr == nil {
			pair.Leaf = leaf
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.cert = &pair
	r.certModTime = certInfo.ModTime()
	r.keyModTime = keyInfo.ModTime()
	return r.cert, nil
}

func (r *certReloader) getCertificate() (*tls.Certificate, error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return nil, fmt.Errorf("tlsx: stat cert file %q: %w", r.certFile, err)
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsx: stat key file %q: %w", r.keyFile, err)
	}

	r.mu.RLock()
	cached := r.cert
	unchanged := cached != nil &&
		certInfo.ModTime().Equal(r.certModTime) &&
		keyInfo.ModTime().Equal(r.keyModTime)
	r.mu.RUnlock()

	if unchanged {
		return cached, nil
	}
	return r.reload()
}

// GetCertificate implements the tls.Config.GetCertificate callback.
func (r *certReloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.getCertificate()
}

// GetClientCertificate implements the tls.Config.GetClientCertificate callback.
func (r *certReloader) GetClientCertificate(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return r.getCertificate()
}

func minTLSVersion(v uint16) (uint16, error) {
	if v == 0 {
		return tls.VersionTLS12, nil
	}
	if v != tls.VersionTLS12 && v != tls.VersionTLS13 {
		return 0, fmt.Errorf("tlsx: unsupported MinVersion 0x%04x (must be TLS 1.2 or TLS 1.3)", v)
	}
	return v, nil
}

func loadCAPool(caFile string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("tlsx: read CA file %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("tlsx: no valid certificates found in CA file %q", caFile)
	}
	return pool, nil
}

// VerifyPeerSPIFFE returns a tls.Config.VerifyPeerCertificate callback that
// ensures the verified leaf certificate contains at least one URI SAN matching
// the allowed SPIFFE IDs.
func VerifyPeerSPIFFE(allowedIDs []string) func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	allowed := make(map[string]struct{}, len(allowedIDs))
	for _, id := range allowedIDs {
		if id != "" {
			allowed[id] = struct{}{}
		}
	}

	return func(_ [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
			return errors.New("tlsx: no verified peer certificate chain")
		}
		leaf := verifiedChains[0][0]
		for _, uri := range leaf.URIs {
			if _, ok := allowed[uri.String()]; ok {
				return nil
			}
		}
		got := make([]string, 0, len(leaf.URIs))
		for _, uri := range leaf.URIs {
			got = append(got, uri.String())
		}
		return fmt.Errorf("tlsx: peer certificate SPIFFE IDs %v not in allowlist", got)
	}
}

// NewServerTLSConfig builds a *tls.Config for a TLS or mTLS server with
// hot-reloading certificates and optional SPIFFE URI SAN verification.
func NewServerTLSConfig(cfg ServerConfig) (*tls.Config, error) {
	minVer, err := minTLSVersion(cfg.MinVersion)
	if err != nil {
		return nil, err
	}

	reloader, err := newCertReloader(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}

	tlsCfg := &tls.Config{
		MinVersion:     minVer,
		GetCertificate: reloader.GetCertificate,
	}

	if cfg.CAFile != "" {
		caPool, err := loadCAPool(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.ClientCAs = caPool
	}

	if cfg.RequireClientCert {
		if cfg.CAFile == "" {
			return nil, errors.New("tlsx: CAFile is required when RequireClientCert is true")
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	} else if cfg.CAFile != "" {
		tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
	}

	if len(cfg.AllowedSPIFFEIDs) > 0 {
		tlsCfg.VerifyPeerCertificate = VerifyPeerSPIFFE(cfg.AllowedSPIFFEIDs)
	}

	return tlsCfg, nil
}

// NewClientTLSConfig builds a *tls.Config for a TLS or mTLS client with
// hot-reloading client certificates and optional SPIFFE URI SAN verification.
func NewClientTLSConfig(cfg ClientConfig) (*tls.Config, error) {
	minVer, err := minTLSVersion(cfg.MinVersion)
	if err != nil {
		return nil, err
	}

	tlsCfg := &tls.Config{
		MinVersion: minVer,
		ServerName: cfg.ServerName,
	}

	if cfg.CertFile != "" || cfg.KeyFile != "" {
		reloader, err := newCertReloader(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.GetClientCertificate = reloader.GetClientCertificate
	}

	if cfg.CAFile != "" {
		caPool, err := loadCAPool(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = caPool
	}

	if len(cfg.ExpectedSPIFFEIDs) > 0 {
		tlsCfg.VerifyPeerCertificate = VerifyPeerSPIFFE(cfg.ExpectedSPIFFEIDs)
	}

	return tlsCfg, nil
}

// ServerCredentials returns gRPC TransportCredentials configured from ServerConfig.
func ServerCredentials(cfg ServerConfig) (credentials.TransportCredentials, error) {
	tlsCfg, err := NewServerTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsCfg), nil
}

// ClientCredentials returns gRPC TransportCredentials configured from ClientConfig.
func ClientCredentials(cfg ClientConfig) (credentials.TransportCredentials, error) {
	tlsCfg, err := NewClientTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsCfg), nil
}
