package authz

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DiscoverEndpoints fetches OpenID provider metadata (authorization, token and
// device endpoints) for the given issuer.
func DiscoverEndpoints(ctx context.Context, issuer string, client *http.Client) (OAuthEndpoints, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	discoveryURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return OAuthEndpoints{}, fmt.Errorf("authz: build discovery request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return OAuthEndpoints{}, fmt.Errorf("authz: fetch discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return OAuthEndpoints{}, fmt.Errorf("authz: discovery returned %d", resp.StatusCode)
	}
	var doc struct {
		AuthorizationEndpoint       string `json:"authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return OAuthEndpoints{}, fmt.Errorf("authz: decode discovery: %w", err)
	}
	return OAuthEndpoints{
		AuthURL:       doc.AuthorizationEndpoint,
		TokenURL:      doc.TokenEndpoint,
		DeviceAuthURL: doc.DeviceAuthorizationEndpoint,
	}, nil
}

// sessionStore abstracts the session storage backend (in-memory or persistent).
type sessionStore interface {
	Create(Identity) (string, error)
	Get(string) (Identity, bool)
	Delete(string)
}

// WebAuthConfig configures the browser Authorization Code + PKCE login flow.
type WebAuthConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	// Endpoints may be provided explicitly (tests) or discovered from Issuer.
	Endpoints OAuthEndpoints
	// Verifier validates the returned id_token; its audience must be ClientID.
	Verifier TokenVerifier
	Sessions sessionStore
	// RoutePrefix defaults to "/auth" (=> /auth/login, /auth/callback, /auth/logout).
	RoutePrefix string
	// CookieName is the session cookie name (default "monofs_session").
	CookieName string
	Secure     bool
	// PostLoginRedirect is where the user lands after login (default "/").
	PostLoginRedirect string
	// RequireLogin, when true, challenges anonymous callers (browser -> redirect
	// to login, others -> 401). Callers with a valid session cookie or bearer
	// token pass through, so machine/service access is unaffected.
	RequireLogin bool
	// ExemptPaths are additional path prefixes always reachable without login
	// (health/metrics defaults are always exempt).
	ExemptPaths []string
	HTTPClient  *http.Client
	now         func() time.Time
}

// WebAuthenticator implements the browser OIDC login flow and a cookie/bearer
// aware middleware that attaches the resolved Identity to the request context.
type WebAuthenticator struct {
	cfg WebAuthConfig

	mu      sync.Mutex
	pending map[string]pendingLogin // state -> pkce verifier
}

type pendingLogin struct {
	verifier string
	created  time.Time
}

const (
	stateCookieName       = "monofs_oauth_state"
	defaultSessionCK      = "monofs_session"
	pendingLoginMaxAge    = 10 * time.Minute
	sessionCookieMaxAge   = 12 * 3600 // 12 hours in seconds
)

// NewWebAuthenticator validates config, discovering endpoints from the issuer
// when they are not provided.
func NewWebAuthenticator(ctx context.Context, cfg WebAuthConfig) (*WebAuthenticator, error) {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, fmt.Errorf("authz: web auth requires client id")
	}
	if strings.TrimSpace(cfg.RedirectURL) == "" {
		return nil, fmt.Errorf("authz: web auth requires redirect URL")
	}
	if cfg.Verifier == nil {
		return nil, fmt.Errorf("authz: web auth requires a token verifier")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "email", "groups", "profile"}
	}
	if cfg.RoutePrefix == "" {
		cfg.RoutePrefix = "/auth"
	}
	if cfg.CookieName == "" {
		cfg.CookieName = defaultSessionCK
	}
	if cfg.PostLoginRedirect == "" {
		cfg.PostLoginRedirect = "/"
	}
	if cfg.Sessions == nil {
		cfg.Sessions = NewSessionStore(12 * time.Hour)
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.Endpoints.AuthURL == "" || cfg.Endpoints.TokenURL == "" {
		eps, err := DiscoverEndpoints(ctx, cfg.Issuer, cfg.HTTPClient)
		if err != nil {
			return nil, err
		}
		if cfg.Endpoints.AuthURL == "" {
			cfg.Endpoints.AuthURL = eps.AuthURL
		}
		if cfg.Endpoints.TokenURL == "" {
			cfg.Endpoints.TokenURL = eps.TokenURL
		}
	}
	return &WebAuthenticator{cfg: cfg, pending: make(map[string]pendingLogin)}, nil
}

func randToken() string {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// LoginHandler starts the login flow: generates PKCE + state, stores the
// verifier, and redirects the browser to the IdP.
func (w *WebAuthenticator) LoginHandler(rw http.ResponseWriter, r *http.Request) {
	pkce, err := GeneratePKCE()
	if err != nil {
		http.Error(rw, "login init failed", http.StatusInternalServerError)
		return
	}
	state := randToken()
	w.mu.Lock()
	// prune stale pending entries
	for s, p := range w.pending {
		if w.cfg.now().Sub(p.created) > pendingLoginMaxAge {
			delete(w.pending, s)
		}
	}
	w.pending[state] = pendingLogin{verifier: pkce.Verifier, created: w.cfg.now()}
	w.mu.Unlock()

	http.SetCookie(rw, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   w.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(pendingLoginMaxAge / time.Second),
	})
	http.Redirect(rw, r, AuthCodeURL(w.cfg.Endpoints.AuthURL, w.cfg.ClientID, w.cfg.RedirectURL, state, w.cfg.Scopes, pkce), http.StatusFound)
}

// CallbackHandler completes the flow: validates state, exchanges the code,
// verifies the id_token, creates a session and sets the session cookie.
func (w *WebAuthenticator) CallbackHandler(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(rw, "login denied: "+e, http.StatusUnauthorized)
		return
	}
	state := q.Get("state")
	code := q.Get("code")
	if state == "" || code == "" {
		http.Error(rw, "missing state or code", http.StatusBadRequest)
		return
	}
	if c, err := r.Cookie(stateCookieName); err != nil || c.Value != state {
		http.Error(rw, "state mismatch", http.StatusBadRequest)
		return
	}

	w.mu.Lock()
	pending, ok := w.pending[state]
	delete(w.pending, state)
	w.mu.Unlock()
	if !ok {
		http.Error(rw, "unknown or expired login state", http.StatusBadRequest)
		return
	}

	tok, err := ExchangeCode(r.Context(), PKCEExchangeConfig{
		ClientID:     w.cfg.ClientID,
		ClientSecret: w.cfg.ClientSecret,
		RedirectURI:  w.cfg.RedirectURL,
		TokenURL:     w.cfg.Endpoints.TokenURL,
		HTTPClient:   w.cfg.HTTPClient,
	}, code, pending.verifier)
	if err != nil {
		http.Error(rw, "code exchange failed", http.StatusUnauthorized)
		return
	}
	if tok.IDToken == "" {
		http.Error(rw, "no id_token returned", http.StatusUnauthorized)
		return
	}
	id, err := w.cfg.Verifier.Verify(r.Context(), tok.IDToken)
	if err != nil {
		http.Error(rw, "id_token verification failed", http.StatusUnauthorized)
		return
	}

	sid, err := w.cfg.Sessions.Create(id)
	if err != nil {
		http.Error(rw, "session creation failed", http.StatusInternalServerError)
		return
	}
	// Clear the state cookie and set the session cookie.
	http.SetCookie(rw, &http.Cookie{Name: stateCookieName, Path: "/", MaxAge: -1})
	http.SetCookie(rw, &http.Cookie{
		Name:     w.cfg.CookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   w.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   sessionCookieMaxAge,
	})
	http.Redirect(rw, r, w.cfg.PostLoginRedirect, http.StatusFound)
}

// LogoutHandler clears the session.
func (w *WebAuthenticator) LogoutHandler(rw http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(w.cfg.CookieName); err == nil {
		w.cfg.Sessions.Delete(c.Value)
	}
	http.SetCookie(rw, &http.Cookie{Name: w.cfg.CookieName, Path: "/", MaxAge: -1})
	http.Redirect(rw, r, w.cfg.PostLoginRedirect, http.StatusFound)
}

// identityFromRequest resolves the caller identity from the session cookie, then
// from a bearer token, else anonymous.
func (w *WebAuthenticator) identityFromRequest(r *http.Request) Identity {
	if c, err := r.Cookie(w.cfg.CookieName); err == nil && c.Value != "" {
		if id, ok := w.cfg.Sessions.Get(c.Value); ok {
			return id
		}
	}
	if bearer := bearerFromHeader(r.Header.Get("Authorization")); bearer != "" {
		if id, err := w.cfg.Verifier.Verify(r.Context(), bearer); err == nil {
			return id
		}
	}
	return Identity{}
}

// Middleware attaches the resolved identity (cookie or bearer) to the request
// context. It never rejects (observe mode); gate specific routes with the
// identity in context.
func (w *WebAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ctx := ContextWithIdentity(r.Context(), w.identityFromRequest(r))
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

// defaultExemptUIPaths are always reachable without a login.
// This keeps probes/scrapers working and allows public static bundles to load
// when the main HTML route is login-gated.
var defaultExemptUIPaths = []string{
	"/healthz",
	"/livez",
	"/readyz",
	"/-/health",
	"/metrics",
	"/favicon.ico",
	"/assets",
	"/static",
	"/debug",
}

func (w *WebAuthenticator) isExempt(path string) bool {
	for _, p := range append(defaultExemptUIPaths, w.cfg.ExemptPaths...) {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func wantsHTML(r *http.Request) bool {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		return true
	}
	// Browser top-level navigations set Sec-Fetch-Mode: navigate.
	return r.Header.Get("Sec-Fetch-Mode") == "navigate"
}

// Handler mounts the auth routes and wraps everything else with the identity
// middleware. When RequireLogin is set, anonymous callers are challenged:
// browser navigations are redirected to /login, other requests get 401. Callers
// presenting a valid session cookie OR bearer token (machines/services) are
// always allowed — so machine-to-machine access is unaffected.
func (w *WebAuthenticator) Handler(next http.Handler) http.Handler {
	login := w.cfg.RoutePrefix + "/login"
	callback := w.cfg.RoutePrefix + "/callback"
	logout := w.cfg.RoutePrefix + "/logout"
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case login:
			w.LoginHandler(rw, r)
			return
		case callback:
			w.CallbackHandler(rw, r)
			return
		case logout:
			w.LogoutHandler(rw, r)
			return
		}

		id := w.identityFromRequest(r)
		if w.cfg.RequireLogin && id.IsAnonymous() && !w.isExempt(r.URL.Path) {
			if wantsHTML(r) {
				http.Redirect(rw, r, login, http.StatusFound)
			} else {
				http.Error(rw, "unauthorized", http.StatusUnauthorized)
			}
			return
		}
		next.ServeHTTP(rw, r.WithContext(ContextWithIdentity(r.Context(), id)))
	})
}
