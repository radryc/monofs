package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceFlowAuthorizeAndPoll(t *testing.T) {
	var pollCount int64
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceAuthResponse{
			DeviceCode:      "dev-123",
			UserCode:        "WXYZ-1234",
			VerificationURI: "https://idp/activate",
			ExpiresIn:       600,
			Interval:        1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		// Pending on the first two polls, then success.
		if atomic.AddInt64(&pollCount, 1) < 3 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "at-1", TokenType: "Bearer", ExpiresIn: 3600})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewDeviceFlowClient("cli", []string{"openid", "groups"}, OAuthEndpoints{
		DeviceAuthURL: srv.URL + "/device",
		TokenURL:      srv.URL + "/token",
	})
	// Don't actually sleep in tests.
	c.sleep = func(time.Duration) {}

	dev, err := c.Authorize(context.Background())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if dev.UserCode != "WXYZ-1234" || dev.Interval != 1 {
		t.Fatalf("unexpected device auth: %+v", dev)
	}

	tok, err := c.Poll(context.Background(), dev)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if tok.AccessToken != "at-1" || tok.ObtainedAt == 0 {
		t.Fatalf("unexpected token: %+v", tok)
	}
	if atomic.LoadInt64(&pollCount) != 3 {
		t.Fatalf("expected 3 polls, got %d", pollCount)
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewDeviceFlowClient("cli", nil, OAuthEndpoints{TokenURL: srv.URL + "/token"})
	c.sleep = func(time.Duration) {}
	_, err := c.Poll(context.Background(), &DeviceAuthResponse{DeviceCode: "d", ExpiresIn: 600, Interval: 1})
	if err == nil {
		t.Fatal("expected denied error")
	}
}

func TestTokenFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "token.json")
	tok := &TokenResponse{AccessToken: "at", TokenType: "Bearer", ObtainedAt: 42}
	if err := SaveTokenFile(path, tok); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AccessToken != "at" || got.ObtainedAt != 42 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
