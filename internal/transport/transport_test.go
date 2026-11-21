package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/radryc/tlsx"
	"github.com/radryc/tlsx/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

const testTrustDomain = "strata.local"

// generatePKI creates a CA with a server certificate for 127.0.0.1 and client
// certificates for the given service names.
func generatePKI(t *testing.T, services ...pki.Service) *pki.Bundle {
	t.Helper()
	bundle, err := pki.Generate(pki.Options{
		Dir:         t.TempDir(),
		TrustDomain: testTrustDomain,
		Services:    services,
	})
	if err != nil {
		t.Fatalf("pki.Generate: %v", err)
	}
	return bundle
}

func serverService() pki.Service {
	return pki.Service{
		Name:      "server",
		Namespace: "monofs",
		DNSNames:  []string{"localhost"},
		IPs:       []string{"127.0.0.1"},
	}
}

func clientService(name string) pki.Service {
	return pki.Service{Name: name, Namespace: "monofs"}
}

func requiredServerConfig(bundle *pki.Bundle) tlsx.EnvConfig {
	svc := bundle.Services["server"]
	return tlsx.EnvConfig{
		Mode:              tlsx.ModeRequired,
		CertFile:          svc.CertFile,
		KeyFile:           svc.KeyFile,
		CAFile:            bundle.CAFile,
		RequireClientCert: true,
	}
}

func requiredClientConfig(bundle *pki.Bundle, service string) tlsx.EnvConfig {
	svc := bundle.Services[service]
	return tlsx.EnvConfig{
		Mode:       tlsx.ModeRequired,
		CertFile:   svc.CertFile,
		KeyFile:    svc.KeyFile,
		CAFile:     bundle.CAFile,
		ServerName: "localhost",
	}
}

// startServer starts a gRPC server with the health service using cfg.
func startServer(t *testing.T, cfg Config) string {
	t.Helper()
	opt, err := cfg.ServerOption()
	if err != nil {
		t.Fatalf("ServerOption: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(opt)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, healthServer)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func checkHealth(ctx context.Context, addr string, dialOpt grpc.DialOption) error {
	conn, err := grpc.NewClient(addr, dialOpt)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	return err
}

func TestOffKeepsPlaintext(t *testing.T) {
	var cfg Config // zero value must behave like STRATA_TLS_MODE=off
	if !cfg.Plaintext() {
		t.Fatalf("zero Config mode = %q, want off", cfg.Mode())
	}
	addr := startServer(t, cfg)
	opt, err := cfg.DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkHealth(ctx, addr, opt); err != nil {
		t.Fatalf("plaintext health check failed: %v", err)
	}
}

func TestLoadEnvInvalidMode(t *testing.T) {
	t.Setenv("STRATA_TLS_MODE", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted an invalid STRATA_TLS_MODE")
	}
}

func TestRequiredMutualTLS(t *testing.T) {
	bundle := generatePKI(t, serverService(), clientService("client"))
	addr := startServer(t, FromEnv(requiredServerConfig(bundle)))

	opt, err := FromEnv(requiredClientConfig(bundle, "client")).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkHealth(ctx, addr, opt); err != nil {
		t.Fatalf("mTLS health check failed: %v", err)
	}
}

func TestRequiredRejectsPlaintextClient(t *testing.T) {
	bundle := generatePKI(t, serverService(), clientService("client"))
	addr := startServer(t, FromEnv(requiredServerConfig(bundle)))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := checkHealth(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials())); err == nil {
		t.Fatal("plaintext client unexpectedly reached an mTLS server")
	}
}

func TestRequiredRejectsUntrustedCA(t *testing.T) {
	bundle := generatePKI(t, serverService(), clientService("client"))
	addr := startServer(t, FromEnv(requiredServerConfig(bundle)))

	// A second, independent CA must not be trusted by the server or client.
	rogue := generatePKI(t, serverService(), clientService("client"))
	cfg := requiredClientConfig(rogue, "client")
	cfg.CAFile = bundle.CAFile // trusts the server...
	opt, err := FromEnv(cfg).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := checkHealth(ctx, addr, opt); err == nil {
		t.Fatal("client certificate from an untrusted CA was accepted")
	}
}

func TestRequiredRejectsClientWithoutCertificate(t *testing.T) {
	bundle := generatePKI(t, serverService(), clientService("client"))
	addr := startServer(t, FromEnv(requiredServerConfig(bundle)))

	cfg := requiredClientConfig(bundle, "client")
	cfg.CertFile, cfg.KeyFile = "", "" // one-way TLS client against an mTLS server
	opt, err := FromEnv(cfg).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := checkHealth(ctx, addr, opt); err == nil {
		t.Fatal("client without a certificate was accepted by an mTLS server")
	}
}

func TestRequiredEnforcesSPIFFEAllowlist(t *testing.T) {
	bundle := generatePKI(t, serverService(), clientService("client"), clientService("other"))
	serverCfg := requiredServerConfig(bundle)
	serverCfg.AllowedSPIFFEIDs = []string{"spiffe://" + testTrustDomain + "/ns/monofs/sa/client"}
	addr := startServer(t, FromEnv(serverCfg))

	allowed, err := FromEnv(requiredClientConfig(bundle, "client")).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkHealth(ctx, addr, allowed); err != nil {
		t.Fatalf("allowlisted SPIFFE client failed: %v", err)
	}

	denied, err := FromEnv(requiredClientConfig(bundle, "other")).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	deniedCtx, deniedCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer deniedCancel()
	if err := checkHealth(deniedCtx, addr, denied); err == nil {
		t.Fatal("client with a non-allowlisted SPIFFE ID was accepted")
	}
}

func TestOptionalFallsBackToPlaintextWithoutCerts(t *testing.T) {
	cfg := FromEnv(tlsx.EnvConfig{
		Mode:     tlsx.ModeOptional,
		CertFile: "/nonexistent/tls.crt",
		KeyFile:  "/nonexistent/tls.key",
		CAFile:   "/nonexistent/ca.crt",
	})
	if cfg.ServerUsesTLS() {
		t.Fatal("optional mode with no certificates should serve plaintext")
	}
	if cfg.ClientUsesTLS() {
		t.Fatal("optional mode with no CA bundle should dial plaintext")
	}
	addr := startServer(t, cfg)
	opt, err := cfg.DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkHealth(ctx, addr, opt); err != nil {
		t.Fatalf("optional plaintext health check failed: %v", err)
	}
}

func TestOptionalServesTLSWhenConfigured(t *testing.T) {
	bundle := generatePKI(t, serverService(), clientService("client"))
	svc := bundle.Services["server"]
	serverCfg := tlsx.EnvConfig{
		Mode:              tlsx.ModeOptional,
		CertFile:          svc.CertFile,
		KeyFile:           svc.KeyFile,
		CAFile:            bundle.CAFile,
		RequireClientCert: true, // must be relaxed by optional mode
	}
	addr := startServer(t, FromEnv(serverCfg))

	clientCfg := requiredClientConfig(bundle, "client")
	clientCfg.Mode = tlsx.ModeOptional
	opt, err := FromEnv(clientCfg).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkHealth(ctx, addr, opt); err != nil {
		t.Fatalf("optional TLS health check failed: %v", err)
	}

	// Optional mode does not require a client certificate, so a one-way TLS
	// client trusting the CA must also succeed.
	oneWay := requiredClientConfig(bundle, "client")
	oneWay.Mode = tlsx.ModeOptional
	oneWay.CertFile, oneWay.KeyFile = "", ""
	oneWayOpt, err := FromEnv(oneWay).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	oneWayCtx, oneWayCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer oneWayCancel()
	if err := checkHealth(oneWayCtx, addr, oneWayOpt); err != nil {
		t.Fatalf("optional TLS without a client certificate failed: %v", err)
	}

	// Optional mode does not force clients: without a CA bundle the client
	// stays plaintext, which a TLS listener rejects.
	plainClient := FromEnv(tlsx.EnvConfig{Mode: tlsx.ModeOptional, CAFile: "/nonexistent/ca.crt"})
	if plainClient.ClientUsesTLS() {
		t.Fatal("optional client without a CA bundle should not use TLS")
	}
	plainOpt, err := plainClient.DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	plainCtx, plainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer plainCancel()
	if err := checkHealth(plainCtx, addr, plainOpt); err == nil {
		t.Fatal("plaintext client unexpectedly reached an optional TLS listener")
	}
}

func TestHTTPURLUsesConfiguredScheme(t *testing.T) {
	plain := Config{}
	if got := plain.HTTPURL("localhost:8080"); got != "http://localhost:8080" {
		t.Fatalf("off HTTPURL = %q", got)
	}
	if got := plain.HTTPURL("http://localhost:8080"); got != "http://localhost:8080" {
		t.Fatalf("off HTTPURL with scheme = %q", got)
	}

	bundle := generatePKI(t, serverService(), clientService("client"))
	tlsCfg := FromEnv(requiredClientConfig(bundle, "client"))
	if got := tlsCfg.HTTPURL("localhost:8080"); got != "https://localhost:8080" {
		t.Fatalf("required HTTPURL = %q", got)
	}
	if got := tlsCfg.HTTPURL("http://localhost:8080"); got != "https://localhost:8080" {
		t.Fatalf("required HTTPURL upgrade = %q", got)
	}
}
