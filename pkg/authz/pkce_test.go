package authz

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGeneratePKCE(t *testing.T) {
	p, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if p.Method != "S256" || p.Verifier == "" || p.Challenge == "" {
		t.Fatalf("unexpected pkce: %+v", p)
	}
	// Challenge must equal base64url(sha256(verifier)).
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Fatalf("challenge mismatch: got %q want %q", p.Challenge, want)
	}
	// Two generations differ.
	p2, _ := GeneratePKCE()
	if p2.Verifier == p.Verifier {
		t.Fatal("expected distinct verifiers")
	}
}

func TestAuthCodeURL(t *testing.T) {
	pkce := PKCE{Challenge: "chal", Method: "S256"}
	raw := AuthCodeURL("https://idp/authorize", "web", "https://app/cb", "state-1", []string{"openid", "groups"}, pkce)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "web" {
		t.Fatalf("missing base params: %v", q)
	}
	if q.Get("code_challenge") != "chal" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("missing pkce params: %v", q)
	}
	if q.Get("scope") != "openid groups" || q.Get("state") != "state-1" {
		t.Fatalf("unexpected scope/state: %v", q)
	}
}

func TestExchangeCode(t *testing.T) {
	var gotVerifier string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotVerifier = r.Form.Get("code_verifier")
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "at", TokenType: "Bearer", IDToken: "idt"})
	}))
	defer srv.Close()

	tok, err := ExchangeCode(context.Background(), PKCEExchangeConfig{
		ClientID:    "web",
		RedirectURI: "https://app/cb",
		TokenURL:    srv.URL,
	}, "code-1", "verifier-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "at" || tok.IDToken != "idt" || tok.ObtainedAt == 0 {
		t.Fatalf("unexpected token: %+v", tok)
	}
	if gotVerifier != "verifier-1" {
		t.Fatalf("server did not receive verifier, got %q", gotVerifier)
	}
}

func TestExchangeCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := ExchangeCode(context.Background(), PKCEExchangeConfig{TokenURL: srv.URL}, "c", "v"); err == nil {
		t.Fatal("expected error on non-200")
	}
	_ = strings.TrimSpace("")
}

func TestSessionStore(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	s := NewSessionStore(time.Hour)
	s.now = func() time.Time { return clock }

	sid, err := s.Create(Identity{Subject: "alice"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, ok := s.Get(sid)
	if !ok || id.Subject != "alice" {
		t.Fatalf("get: ok=%v id=%+v", ok, id)
	}

	// Expire.
	clock = base.Add(2 * time.Hour)
	if _, ok := s.Get(sid); ok {
		t.Fatal("expected session to be expired")
	}

	// Delete/logout.
	clock = base
	sid2, _ := s.Create(Identity{Subject: "bob"})
	s.Delete(sid2)
	if _, ok := s.Get(sid2); ok {
		t.Fatal("expected session deleted")
	}
}
