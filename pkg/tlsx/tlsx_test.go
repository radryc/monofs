package tlsx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

const (
	serverSPIFFEID      = "spiffe://strata.local/ns/monofs/sa/router"
	authorizedClientID  = "spiffe://strata.local/ns/monofs/sa/fetcher"
	unauthorizedClientID = "spiffe://strata.local/ns/other/sa/intruder"
)

type testPKI struct {
	dir        string
	caCert     *x509.Certificate
	caKey      *ecdsa.PrivateKey
	caFile     string
	serverCert string
	serverKey  string
	clientCert string
	clientKey  string
	badCert    string
	badKey     string
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("generate CA serial: %v", err)
	}
	now := time.Now().Add(-time.Minute)
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "strata-test-ca"},
		NotBefore:             now,
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	caFile := filepath.Join(dir, "ca.crt")
	writePEM(t, caFile, "CERTIFICATE", caDER)

	pki := &testPKI{
		dir:    dir,
		caCert: caCert,
		caKey:  caKey,
		caFile: caFile,
	}

	pki.serverCert, pki.serverKey = pki.issueCert(t, "server", serverSPIFFEID, true)
	pki.clientCert, pki.clientKey = pki.issueCert(t, "client-auth", authorizedClientID, false)
	pki.badCert, pki.badKey = pki.issueCert(t, "client-unauth", unauthorizedClientID, false)
	return pki
}

func (p *testPKI) issueCert(t *testing.T, name, spiffeID string, isServer bool) (string, string) {
	t.Helper()
	certFile := filepath.Join(p.dir, name+".crt")
	keyFile := filepath.Join(p.dir, name+".key")
	p.writeLeafCert(t, certFile, keyFile, spiffeID, isServer)
	return certFile, keyFile
}

func (p *testPKI) writeLeafCert(t *testing.T, certFile, keyFile, spiffeID string, isServer bool) *big.Int {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("generate leaf serial: %v", err)
	}

	uri, err := url.Parse(spiffeID)
	if err != nil {
		t.Fatalf("parse SPIFFE URI %q: %v", spiffeID, err)
	}

	now := time.Now().Add(-time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: spiffeID},
		NotBefore:    now,
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		URIs:         []*url.URL{uri},
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}

	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
	return serial
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func startEchoTLSServer(t *testing.T, tlsCfg *tls.Config) (string, func()) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	return ln.Addr().String(), func() {
		_ = ln.Close()
		<-done
	}
}

func roundTripTLS(addr string, clientCfg *tls.Config) error {
	conn, err := tls.Dial("tcp", addr, clientCfg)
	if err != nil {
		return err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		return err
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	return nil
}

func TestMTLSHandshakeSuccess(t *testing.T) {
	pki := newTestPKI(t)

	srvCfg, err := NewServerTLSConfig(ServerConfig{
		CertFile:          pki.serverCert,
		KeyFile:           pki.serverKey,
		CAFile:            pki.caFile,
		RequireClientCert: true,
		AllowedSPIFFEIDs:  []string{authorizedClientID},
	})
	if err != nil {
		t.Fatalf("NewServerTLSConfig: %v", err)
	}
	addr, stop := startEchoTLSServer(t, srvCfg)
	defer stop()

	cliCfg, err := NewClientTLSConfig(ClientConfig{
		CertFile:          pki.clientCert,
		KeyFile:           pki.clientKey,
		CAFile:            pki.caFile,
		ServerName:        "localhost",
		MinVersion:        tls.VersionTLS13,
		ExpectedSPIFFEIDs: []string{serverSPIFFEID},
	})
	if err != nil {
		t.Fatalf("NewClientTLSConfig: %v", err)
	}

	if err := roundTripTLS(addr, cliCfg); err != nil {
		t.Fatalf("expected mTLS handshake to succeed, got: %v", err)
	}
}

func TestMTLSFailsWithoutClientCert(t *testing.T) {
	pki := newTestPKI(t)

	srvCfg, err := NewServerTLSConfig(ServerConfig{
		CertFile:          pki.serverCert,
		KeyFile:           pki.serverKey,
		CAFile:            pki.caFile,
		RequireClientCert: true,
	})
	if err != nil {
		t.Fatalf("NewServerTLSConfig: %v", err)
	}
	addr, stop := startEchoTLSServer(t, srvCfg)
	defer stop()

	cliCfg, err := NewClientTLSConfig(ClientConfig{
		CAFile:     pki.caFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("NewClientTLSConfig: %v", err)
	}

	if err := roundTripTLS(addr, cliCfg); err == nil {
		t.Fatal("expected mTLS handshake to fail when client presents no certificate")
	}
}

func TestMTLSFailsWithUnauthorizedSPIFFEID(t *testing.T) {
	pki := newTestPKI(t)

	srvCfg, err := NewServerTLSConfig(ServerConfig{
		CertFile:          pki.serverCert,
		KeyFile:           pki.serverKey,
		CAFile:            pki.caFile,
		RequireClientCert: true,
		AllowedSPIFFEIDs:  []string{authorizedClientID},
	})
	if err != nil {
		t.Fatalf("NewServerTLSConfig: %v", err)
	}
	addr, stop := startEchoTLSServer(t, srvCfg)
	defer stop()

	cliCfg, err := NewClientTLSConfig(ClientConfig{
		CertFile:   pki.badCert,
		KeyFile:    pki.badKey,
		CAFile:     pki.caFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("NewClientTLSConfig: %v", err)
	}

	if err := roundTripTLS(addr, cliCfg); err == nil {
		t.Fatal("expected mTLS handshake to fail for unauthorized SPIFFE ID")
	}
}

func TestHotReloadCertificates(t *testing.T) {
	pki := newTestPKI(t)

	srvCfg, err := NewServerTLSConfig(ServerConfig{
		CertFile: pki.serverCert,
		KeyFile:  pki.serverKey,
		CAFile:   pki.caFile,
	})
	if err != nil {
		t.Fatalf("NewServerTLSConfig: %v", err)
	}
	cliCfg, err := NewClientTLSConfig(ClientConfig{
		CertFile: pki.clientCert,
		KeyFile:  pki.clientKey,
		CAFile:   pki.caFile,
	})
	if err != nil {
		t.Fatalf("NewClientTLSConfig: %v", err)
	}

	initialSrv, err := srvCfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("initial GetCertificate: %v", err)
	}
	initialCli, err := cliCfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("initial GetClientCertificate: %v", err)
	}

	// Rotate both server and client certificates on disk and bump mtimes.
	newSrvSerial := pki.writeLeafCert(t, pki.serverCert, pki.serverKey, serverSPIFFEID, true)
	newCliSerial := pki.writeLeafCert(t, pki.clientCert, pki.clientKey, authorizedClientID, false)

	future := time.Now().Add(2 * time.Second)
	for _, p := range []string{pki.serverCert, pki.serverKey, pki.clientCert, pki.clientKey} {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatalf("Chtimes(%s): %v", p, err)
		}
	}

	reloadedSrv, err := srvCfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("reloaded GetCertificate: %v", err)
	}
	if reloadedSrv.Leaf.SerialNumber.Cmp(initialSrv.Leaf.SerialNumber) == 0 {
		t.Fatal("expected server GetCertificate to serve newly rotated certificate")
	}
	if reloadedSrv.Leaf.SerialNumber.Cmp(newSrvSerial) != 0 {
		t.Fatalf("server serial mismatch: got %v, want %v", reloadedSrv.Leaf.SerialNumber, newSrvSerial)
	}

	reloadedCli, err := cliCfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("reloaded GetClientCertificate: %v", err)
	}
	if reloadedCli.Leaf.SerialNumber.Cmp(initialCli.Leaf.SerialNumber) == 0 {
		t.Fatal("expected client GetClientCertificate to serve newly rotated certificate")
	}
	if reloadedCli.Leaf.SerialNumber.Cmp(newCliSerial) != 0 {
		t.Fatalf("client serial mismatch: got %v, want %v", reloadedCli.Leaf.SerialNumber, newCliSerial)
	}
}

func TestGRPCCredentialsEndToEnd(t *testing.T) {
	pki := newTestPKI(t)

	srvCreds, err := ServerCredentials(ServerConfig{
		CertFile:          pki.serverCert,
		KeyFile:           pki.serverKey,
		CAFile:            pki.caFile,
		RequireClientCert: true,
		AllowedSPIFFEIDs:  []string{authorizedClientID},
	})
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	grpcSrv := grpc.NewServer(grpc.Creds(srvCreds))
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(grpcSrv, healthSrv)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- grpcSrv.Serve(ln)
	}()
	defer grpcSrv.Stop()

	// Authorized client succeeds.
	authCreds, err := ClientCredentials(ClientConfig{
		CertFile:          pki.clientCert,
		KeyFile:           pki.clientKey,
		CAFile:            pki.caFile,
		ServerName:        "localhost",
		ExpectedSPIFFEIDs: []string{serverSPIFFEID},
	})
	if err != nil {
		t.Fatalf("ClientCredentials (authorized): %v", err)
	}

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(authCreds))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("authorized gRPC health check failed: %v", err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("unexpected health status: %v", resp.GetStatus())
	}

	// Unauthorized client SPIFFE ID is rejected.
	badCreds, err := ClientCredentials(ClientConfig{
		CertFile:   pki.badCert,
		KeyFile:    pki.badKey,
		CAFile:     pki.caFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("ClientCredentials (unauthorized): %v", err)
	}

	badConn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(badCreds))
	if err != nil {
		t.Fatalf("grpc.NewClient (unauthorized): %v", err)
	}
	defer badConn.Close()

	badCtx, badCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer badCancel()
	if _, err := grpc_health_v1.NewHealthClient(badConn).Check(badCtx, &grpc_health_v1.HealthCheckRequest{}); err == nil {
		t.Fatal("expected gRPC health check with unauthorized SPIFFE ID to fail")
	}
}
