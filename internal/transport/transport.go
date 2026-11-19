// Package transport builds gRPC and HTTP transport security from the shared
// STRATA_TLS_* environment contract (github.com/radryc/tlsx).
//
// The zero Config is plaintext, which keeps every existing deployment and test
// working unchanged. Load reads the environment once; callers should resolve a
// Config at startup and pass it explicitly to the components that dial or
// serve traffic.
//
// Modes:
//
//	off       plaintext gRPC and HTTP (default)
//	optional  servers terminate TLS when a certificate and key are configured;
//	          clients use TLS when a CA bundle is configured. Missing CA
//	          bundles fall back to plaintext, so migration is not forced.
//	required  servers terminate mTLS (client certificates verified unless
//	          STRATA_TLS_REQUIRE_CLIENT_CERT=false); clients use TLS and
//	          verify the configured CA. Missing certificates are an error.
package transport

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/radryc/tlsx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Config is a resolved transport security configuration.
type Config struct {
	env tlsx.EnvConfig
}

// Load reads the STRATA_TLS_* environment contract.
func Load() (Config, error) {
	env, err := tlsx.LoadEnv()
	if err != nil {
		return Config{}, err
	}
	return Config{env: env}, nil
}

// FromEnv wraps an already-resolved tlsx environment configuration.
func FromEnv(env tlsx.EnvConfig) Config {
	return Config{env: env}
}

// Mode returns the effective TLS mode. The zero value is ModeOff.
func (c Config) Mode() tlsx.Mode {
	if c.env.Mode == "" {
		return tlsx.ModeOff
	}
	return c.env.Mode
}

// Plaintext reports whether TLS is disabled entirely.
func (c Config) Plaintext() bool { return c.Mode() == tlsx.ModeOff }

// Required reports whether TLS is mandatory.
func (c Config) Required() bool { return c.Mode() == tlsx.ModeRequired }

// ServerUsesTLS reports whether a server should terminate TLS. In optional
// mode TLS is only used when both the certificate and key are present.
func (c Config) ServerUsesTLS() bool {
	switch c.Mode() {
	case tlsx.ModeRequired:
		return true
	case tlsx.ModeOptional:
		return fileExists(c.env.CertFile) && fileExists(c.env.KeyFile)
	default:
		return false
	}
}

// ClientUsesTLS reports whether an outgoing connection should use TLS. In
// optional mode clients use TLS only when a CA bundle is configured.
func (c Config) ClientUsesTLS() bool {
	switch c.Mode() {
	case tlsx.ModeRequired:
		return true
	case tlsx.ModeOptional:
		return fileExists(c.env.CAFile)
	default:
		return false
	}
}

// ServerTLSConfig returns the server *tls.Config, or nil for plaintext.
func (c Config) ServerTLSConfig() (*tls.Config, error) {
	if !c.ServerUsesTLS() {
		return nil, nil
	}
	cfg := c.env.ServerConfig()
	if !fileExists(cfg.CAFile) {
		if c.Required() && cfg.RequireClientCert {
			return nil, fmt.Errorf("tlsx: STRATA_TLS_MODE=required needs a CA bundle at %q to verify clients", cfg.CAFile)
		}
		cfg.CAFile = ""
		cfg.RequireClientCert = false
	}
	if c.Mode() == tlsx.ModeOptional {
		// Optional mode must not force peers to present a client certificate.
		cfg.RequireClientCert = false
	}
	return tlsx.NewServerTLSConfig(cfg)
}

// ClientTLSConfig returns the client *tls.Config, or nil for plaintext.
func (c Config) ClientTLSConfig() (*tls.Config, error) {
	if !c.ClientUsesTLS() {
		return nil, nil
	}
	cfg := c.env.ClientConfig()
	if !fileExists(cfg.CAFile) {
		if c.Required() {
			return nil, fmt.Errorf("tlsx: STRATA_TLS_MODE=required needs a CA bundle at %q to verify servers", cfg.CAFile)
		}
		cfg.CAFile = ""
	}
	if !fileExists(cfg.CertFile) || !fileExists(cfg.KeyFile) {
		cfg.CertFile, cfg.KeyFile = "", ""
	}
	return tlsx.NewClientTLSConfig(cfg)
}

// ServerOption returns the gRPC server option for the configured mode.
func (c Config) ServerOption() (grpc.ServerOption, error) {
	tlsCfg, err := c.ServerTLSConfig()
	if err != nil {
		return nil, err
	}
	if tlsCfg == nil {
		return grpc.Creds(insecure.NewCredentials()), nil
	}
	return grpc.Creds(credentials.NewTLS(tlsCfg)), nil
}

// DialOption returns the gRPC dial option for the configured mode.
func (c Config) DialOption() (grpc.DialOption, error) {
	tlsCfg, err := c.ClientTLSConfig()
	if err != nil {
		return nil, err
	}
	if tlsCfg == nil {
		return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)), nil
}

// Scheme returns "https" when outgoing HTTP should use TLS, else "http".
func (c Config) Scheme() string {
	if c.ClientUsesTLS() {
		return "https"
	}
	return "http"
}

// HTTPURL normalizes raw into a URL with the transport's scheme. Raw values
// that already carry a scheme are upgraded from http to https when TLS is
// enabled.
func (c Config) HTTPURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		if c.ClientUsesTLS() && strings.EqualFold(raw[:i], "http") {
			return c.Scheme() + raw[i:]
		}
		return raw
	}
	return c.Scheme() + "://" + raw
}

// HTTPClient returns an HTTP client configured for TLS when enabled. The
// supplied base client is copied, never mutated; nil uses http.DefaultClient.
func (c Config) HTTPClient(base *http.Client) (*http.Client, error) {
	tlsCfg, err := c.ClientTLSConfig()
	if err != nil {
		return nil, err
	}
	if tlsCfg == nil {
		if base != nil {
			return base, nil
		}
		return http.DefaultClient, nil
	}
	client := &http.Client{}
	if base != nil {
		*client = *base
	}
	var tr *http.Transport
	if base != nil {
		if existing, ok := base.Transport.(*http.Transport); ok && existing != nil {
			tr = existing.Clone()
		}
	}
	if tr == nil {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}
	tr.TLSClientConfig = tlsCfg
	client.Transport = tr
	return client, nil
}

// ListenAndServe serves srv on srv.Addr, wrapping the listener with TLS when
// the server should terminate TLS.
func (c Config) ListenAndServe(srv *http.Server) error {
	lis, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	return c.Serve(srv, lis)
}

// Serve serves srv on lis, wrapping the listener with TLS when configured.
func (c Config) Serve(srv *http.Server, lis net.Listener) error {
	tlsCfg, err := c.ServerTLSConfig()
	if err != nil {
		_ = lis.Close()
		return err
	}
	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
		lis = tls.NewListener(lis, tlsCfg)
	}
	return srv.Serve(lis)
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
