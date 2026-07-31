package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// stubIDP simulates an OIDC provider's token endpoint for the code exchange.
func newWebTestAuthenticator(t *testing.T, tokenSrvURL string) *WebAuthenticator {
	t.Helper()
	// Verifier that accepts the fixed id_token from the stub and returns alice.
	verifier := tokenVerifierFunc(func(_ context.Context, raw string) (Identity, error) {
		if raw == "id-token-alice" {
			return Identity{Subject: "alice", Email: "alice@example.com"}, nil
		}
		return Identity{}, http.ErrNoCookie
	})
	wa, err := NewWebAuthenticator(context.Background(), WebAuthConfig{
		ClientID:     "monofs",
		ClientSecret: "secret",
		RedirectURL:  "http://app/auth/callback",
		Endpoints: OAuthEndpoints{
			AuthURL:  "http://idp/authorize",
			TokenURL: tokenSrvURL,
		},
		Verifier: verifier,
		Sessions: NewSessionStore(0),
	})
	if err != nil {
		t.Fatalf("NewWebAuthenticator: %v", err)
	}
	return wa
}

func TestWebAuthLoginRedirect(t *testing.T) {
	wa := newWebTestAuthenticator(t, "http://idp/token")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	wa.LoginHandler(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("client_id") != "monofs" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("bad auth redirect: %v", q)
	}
	// A state cookie must be set.
	if !strings.Contains(rec.Header().Get("Set-Cookie"), stateCookieName) {
		t.Fatalf("state cookie not set: %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestWebAuthFullFlow(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_secret") != "secret" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "at", IDToken: "id-token-alice", TokenType: "Bearer"})
	}))
	defer tokenSrv.Close()

	wa := newWebTestAuthenticator(t, tokenSrv.URL)

	// 1. login -> capture state + state cookie.
	loginRec := httptest.NewRecorder()
	wa.LoginHandler(loginRec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	state := func() string {
		loc, _ := url.Parse(loginRec.Header().Get("Location"))
		return loc.Query().Get("state")
	}()
	stateCookie := loginRec.Result().Cookies()[0]

	// 2. callback with code + state (+ state cookie).
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+state, nil)
	cbReq.AddCookie(stateCookie)
	cbRec := httptest.NewRecorder()
	wa.CallbackHandler(cbRec, cbReq)
	if cbRec.Code != http.StatusFound {
		t.Fatalf("callback expected 302, got %d (%s)", cbRec.Code, cbRec.Body.String())
	}

	// A session cookie must be issued.
	var sessionCookie *http.Cookie
	for _, c := range cbRec.Result().Cookies() {
		if c.Name == defaultSessionCK && c.Value != "" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie issued")
	}

	// 3. subsequent request with the session cookie resolves the identity.
	var seen Identity
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = IdentityFromContext(r.Context())
	})
	protReq := httptest.NewRequest(http.MethodGet, "/", nil)
	protReq.AddCookie(sessionCookie)
	wa.Middleware(next).ServeHTTP(httptest.NewRecorder(), protReq)
	if seen.Email != "alice@example.com" {
		t.Fatalf("session identity not resolved, got %+v", seen)
	}

	// 4. logout clears the session.
	logoutReq := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	logoutReq.AddCookie(sessionCookie)
	wa.LogoutHandler(httptest.NewRecorder(), logoutReq)
	if _, ok := wa.cfg.Sessions.Get(sessionCookie.Value); ok {
		t.Fatal("session not cleared after logout")
	}
}

func TestWebAuthCallbackStateMismatch(t *testing.T) {
	wa := newWebTestAuthenticator(t, "http://idp/token")
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state=x", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "different"})
	rec := httptest.NewRecorder()
	wa.CallbackHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on state mismatch, got %d", rec.Code)
	}
}

func TestWebAuthMiddlewareBearerFallback(t *testing.T) {
	wa := newWebTestAuthenticator(t, "http://idp/token")
	var seen Identity
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = IdentityFromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer id-token-alice")
	wa.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	if seen.Subject != "alice" {
		t.Fatalf("bearer fallback failed, got %+v", seen)
	}
}

func TestWebAuthRequireLogin(t *testing.T) {
	wa := newWebTestAuthenticator(t, "http://idp/token")
	wa.cfg.RequireLogin = true
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true })
	h := wa.Handler(next)

	// 1. Anonymous browser navigation -> redirected to /auth/login.
	called = false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login" {
		t.Fatalf("anonymous browser should redirect to login, got %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if called {
		t.Fatal("handler should not run for anonymous browser")
	}

	// 2. Anonymous XHR to API path -> 401 (challenged by RequireLogin).
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("anonymous api xhr should 401, got %d, called=%v", rec.Code, called)
	}

	// 2b. Anonymous browser navigation to API path -> redirect to login.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login" {
		t.Fatalf("anonymous browser on api path should redirect, got %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	// 2c. Anonymous non-exempt XHR (no HTML) -> 401.
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous non-exempt xhr should 401, got %d", rec.Code)
	}

	// 3. Machine with a valid bearer token -> allowed (machines stay fine).
	called = false
	rec = httptest.NewRecorder()
	mreq := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	mreq.Header.Set("Authorization", "Bearer id-token-alice")
	h.ServeHTTP(rec, mreq)
	if !called {
		t.Fatal("machine with valid bearer should be allowed")
	}

	// 4. Health endpoint is exempt even when anonymous.
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !called {
		t.Fatal("health endpoint should be exempt from RequireLogin")
	}

	// 5. Static UI assets are exempt so browser bundles can load.
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/index.css", nil))
	if !called {
		t.Fatal("assets path should be exempt from RequireLogin")
	}
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/static/monofs.png", nil))
	if !called {
		t.Fatal("static path should be exempt from RequireLogin")
	}

	// 6. Login route itself is always reachable.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login route should work, got %d", rec.Code)
	}
}
