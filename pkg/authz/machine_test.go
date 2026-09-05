package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientCredentialsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "svc" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "mach-at", TokenType: "Bearer", ExpiresIn: 300})
	}))
	defer srv.Close()

	c := &ClientCredentialsClient{ClientID: "svc", ClientSecret: "sh", Scopes: []string{"monofs"}, TokenURL: srv.URL}
	tok, err := c.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "mach-at" || tok.ObtainedAt == 0 {
		t.Fatalf("unexpected token: %+v", tok)
	}
}

func TestStaticTokenVerifier(t *testing.T) {
	v := NewStaticTokenVerifier()
	v.Add("svc-token", Identity{ClientID: "ci-bot", Groups: []string{"strata-ci"}})

	id, err := v.Verify(context.Background(), "svc-token")
	if err != nil || id.ClientID != "ci-bot" || !id.HasGroup("strata-ci") {
		t.Fatalf("verify known token: id=%+v err=%v", id, err)
	}
	if _, err := v.Verify(context.Background(), "nope"); err == nil {
		t.Fatal("unknown token should error")
	}
	if _, err := v.Verify(context.Background(), ""); err == nil {
		t.Fatal("empty token should error")
	}
}

func TestBreakGlassVerifier(t *testing.T) {
	v := NewBreakGlassVerifier("MONOFS_TOKEN_VALUE", "break-glass-admin")
	id, err := v.Verify(context.Background(), "MONOFS_TOKEN_VALUE")
	if err != nil || id.ClientID != "break-glass-admin" {
		t.Fatalf("break-glass verify: id=%+v err=%v", id, err)
	}

	// Pairing with an admin wildcard grant authorizes everything.
	store, _ := NewGrantStore("")
	_ = store.Add(Grant{Subject: "break-glass-admin", Partition: WildcardPartition, Role: RoleAdmin})
	if !store.Can(context.Background(), id, "any-partition", ActionModify) {
		t.Fatal("break-glass admin should be able to modify any partition")
	}
}

// errVerifier always errors.
type errVerifier struct{}

func (errVerifier) Verify(context.Context, string) (Identity, error) {
	return Identity{}, errors.New("nope")
}

func TestChainVerifier(t *testing.T) {
	static := NewStaticTokenVerifier()
	static.Add("svc", Identity{ClientID: "ci"})

	// NoopVerifier returns anonymous (no error) -> chain should skip it and try next.
	chain := NewChainVerifier(NoopVerifier{}, errVerifier{}, static)

	id, err := chain.Verify(context.Background(), "svc")
	if err != nil || id.ClientID != "ci" {
		t.Fatalf("chain should resolve via static verifier: id=%+v err=%v", id, err)
	}

	// Unknown token -> all fail -> error.
	if _, err := chain.Verify(context.Background(), "unknown"); err == nil {
		t.Fatal("chain should error when nothing resolves")
	}
}

func TestCachedTokenReturnsCachedValue(t *testing.T) {
	var reqCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqCount.Add(1)
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "cached-at", TokenType: "Bearer", ExpiresIn: 300})
	}))
	defer srv.Close()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := &ClientCredentialsClient{
		ClientID:     "svc",
		ClientSecret: "sh",
		TokenURL:     srv.URL,
		now:          func() time.Time { return base },
	}

	tok1, err := c.CachedToken(context.Background())
	if err != nil {
		t.Fatalf("first CachedToken: %v", err)
	}
	tok2, err := c.CachedToken(context.Background())
	if err != nil {
		t.Fatalf("second CachedToken: %v", err)
	}
	if tok1.AccessToken != "cached-at" || tok2.AccessToken != "cached-at" {
		t.Fatalf("unexpected tokens: %s, %s", tok1.AccessToken, tok2.AccessToken)
	}
	if got := reqCount.Load(); got != 1 {
		t.Fatalf("expected 1 HTTP request (cached), got %d", got)
	}
}

func TestCachedTokenRefreshesAfterExpiry(t *testing.T) {
	var reqCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := reqCount.Add(1)
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "at-" + string(rune('0'+n)), TokenType: "Bearer", ExpiresIn: 300})
	}))
	defer srv.Close()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	c := &ClientCredentialsClient{
		ClientID:      "svc",
		ClientSecret:  "sh",
		TokenURL:      srv.URL,
		RefreshBuffer: 30 * time.Second,
		now:           func() time.Time { return clock },
	}

	if _, err := c.CachedToken(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}

	// Advance past expiry (300s) + buffer (30s).
	clock = base.Add(340 * time.Second)
	if _, err := c.CachedToken(context.Background()); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if got := reqCount.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP requests (refreshed), got %d", got)
	}
}

func TestCachedTokenRefreshesWithinBufferWindow(t *testing.T) {
	var reqCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqCount.Add(1)
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "at", TokenType: "Bearer", ExpiresIn: 60})
	}))
	defer srv.Close()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := &ClientCredentialsClient{
		ClientID:      "svc",
		ClientSecret:  "sh",
		TokenURL:      srv.URL,
		RefreshBuffer: 30 * time.Second,
		now:           func() time.Time { return base },
	}

	if _, err := c.CachedToken(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}

	// At 40s: within the 60s expiry but past the 30s buffer -> should refresh.
	c.now = func() time.Time { return base.Add(40 * time.Second) }
	if _, err := c.CachedToken(context.Background()); err != nil {
		t.Fatalf("at 40s: %v", err)
	}
	if got := reqCount.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP requests (buffer refresh), got %d", got)
	}
}

func TestCachedTokenPropagatesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &ClientCredentialsClient{ClientID: "svc", ClientSecret: "sh", TokenURL: srv.URL}
	if _, err := c.CachedToken(context.Background()); err == nil {
		t.Fatal("expected error from server failure")
	}
}
