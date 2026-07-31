package authz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ClientCredentialsClient obtains machine tokens via the OAuth 2.0
// client-credentials grant (authz epic G1).
type ClientCredentialsClient struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
	TokenURL     string
	HTTPClient   *http.Client
	// RefreshBuffer is how long before token expiry to proactively refresh.
	// Defaults to 30s.
	RefreshBuffer time.Duration
	// now overrides the clock for tests.
	now func() time.Time

	mu        sync.Mutex
	cached    *TokenResponse
	expiresAt time.Time
}

const defaultRefreshBuffer = 30 * time.Second

// Token requests an access token using the client-credentials grant.
func (c *ClientCredentialsClient) Token(ctx context.Context) (*TokenResponse, error) {
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	if len(c.Scopes) > 0 {
		form.Set("scope", strings.Join(c.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("authz: build client-credentials request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authz: client-credentials request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authz: client-credentials failed with status %d", resp.StatusCode)
	}
	var tok TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("authz: decode client-credentials token: %w", err)
	}
	now := c.nowFunc()
	tok.ObtainedAt = now.Unix()
	return &tok, nil
}

// CachedToken returns a cached access token, refreshing it from the token
// endpoint when expired or nearing expiry. Concurrent calls are serialized;
// only one refresh happens at a time.
func (c *ClientCredentialsClient) CachedToken(ctx context.Context) (*TokenResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != nil && !c.isExpired() {
		return c.cached, nil
	}

	tok, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}

	c.cached = tok
	c.expiresAt = c.computeExpiry(tok)
	return tok, nil
}

func (c *ClientCredentialsClient) nowFunc() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *ClientCredentialsClient) refreshBuffer() time.Duration {
	if c.RefreshBuffer > 0 {
		return c.RefreshBuffer
	}
	return defaultRefreshBuffer
}

func (c *ClientCredentialsClient) isExpired() bool {
	return c.nowFunc().After(c.expiresAt)
}

func (c *ClientCredentialsClient) computeExpiry(tok *TokenResponse) time.Time {
	if tok.ExpiresIn <= 0 {
		// No expiry reported — treat as single-use, always refresh.
		return c.nowFunc()
	}
	expiry := time.Unix(tok.ObtainedAt, 0).Add(time.Duration(tok.ExpiresIn) * time.Second)
	buffered := expiry.Add(-c.refreshBuffer())
	if buffered.Before(c.nowFunc()) {
		// Already within the buffer window; use the actual expiry.
		return expiry
	}
	return buffered
}

// StaticTokenVerifier resolves opaque bearer tokens to identities from a fixed
// table. It supports machine/service principals and break-glass admin tokens
// without an IdP round-trip (authz epics G1/G2). Tokens are stored hashed.
type StaticTokenVerifier struct {
	mu     sync.RWMutex
	tokens map[string]Identity // sha256(token) -> identity
}

// NewStaticTokenVerifier returns an empty static token verifier.
func NewStaticTokenVerifier() *StaticTokenVerifier {
	return &StaticTokenVerifier{tokens: make(map[string]Identity)}
}

// Add registers a token that resolves to the given identity.
func (v *StaticTokenVerifier) Add(token string, id Identity) {
	if strings.TrimSpace(token) == "" {
		return
	}
	v.mu.Lock()
	v.tokens[hashToken(token)] = id
	v.mu.Unlock()
}

// Verify implements TokenVerifier.
func (v *StaticTokenVerifier) Verify(_ context.Context, rawToken string) (Identity, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return Identity{}, fmt.Errorf("authz: empty token")
	}
	v.mu.RLock()
	id, ok := v.tokens[hashToken(rawToken)]
	v.mu.RUnlock()
	if !ok {
		return Identity{}, fmt.Errorf("authz: unknown service token")
	}
	return id, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewBreakGlassVerifier maps a single admin token to an admin service identity
// (authz epic G2). Pair it with an admin grant on WildcardPartition for the same
// client id (e.g. via the grant store) so the break-glass principal can act
// everywhere. Intended to back the MONOFS_TOKEN break-glass path; every use
// should be audit-logged by the interceptor's Observe hook.
func NewBreakGlassVerifier(adminToken, clientID string) *StaticTokenVerifier {
	v := NewStaticTokenVerifier()
	v.Add(adminToken, Identity{ClientID: clientID})
	return v
}

// ChainVerifier tries multiple verifiers in order and returns the first
// non-anonymous identity. It lets a service accept OIDC tokens, static service
// tokens, and a break-glass token through a single TokenVerifier.
type ChainVerifier struct {
	verifiers []TokenVerifier
}

// NewChainVerifier builds a chain from the given verifiers (nil entries skipped).
func NewChainVerifier(verifiers ...TokenVerifier) *ChainVerifier {
	filtered := make([]TokenVerifier, 0, len(verifiers))
	for _, v := range verifiers {
		if v != nil {
			filtered = append(filtered, v)
		}
	}
	return &ChainVerifier{verifiers: filtered}
}

// Verify tries each verifier; the first that returns a non-anonymous identity
// without error wins. If none succeed, the last error is returned.
func (c *ChainVerifier) Verify(ctx context.Context, rawToken string) (Identity, error) {
	var lastErr error
	for _, v := range c.verifiers {
		id, err := v.Verify(ctx, rawToken)
		if err == nil && !id.IsAnonymous() {
			return id, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("authz: no verifier accepted the token")
	}
	return Identity{}, lastErr
}
