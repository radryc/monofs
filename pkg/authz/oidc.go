package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// OIDCConfig configures an OIDCVerifier.
type OIDCConfig struct {
	// Issuer is the expected token issuer (OIDC "iss"). Required.
	Issuer string
	// Audience is the expected audience (OIDC "aud"). Required.
	Audience string
	// JWKSURL is the JSON Web Key Set endpoint. When empty it is discovered
	// from the issuer's /.well-known/openid-configuration document.
	JWKSURL string
	// GroupsClaim is the claim carrying group membership. Defaults to "groups".
	GroupsClaim string
	// EmailClaim is the claim carrying the email. Defaults to "email".
	EmailClaim string
	// ClientIDClaim is the claim carrying a machine client id. Defaults to
	// "client_id"; falls back to "azp" when absent.
	ClientIDClaim string
	// RefreshInterval bounds how long a fetched JWKS is cached before a
	// refresh is allowed. Defaults to 15 minutes.
	RefreshInterval time.Duration
	// AllowedAlgorithms restricts accepted signature algorithms. Defaults to
	// RS256, RS384, RS512, ES256, ES384, ES512, PS256.
	AllowedAlgorithms []jose.SignatureAlgorithm
	// HTTPClient is used for JWKS/discovery requests. Defaults to a client
	// with a 10s timeout.
	HTTPClient *http.Client
	// TokenCacheTTL is how long a verified token→Identity mapping is cached.
	// The effective TTL is min(TokenCacheTTL, token.exp - now). Defaults to
	// 5 minutes. Set to 0 to disable token caching.
	TokenCacheTTL time.Duration
	// TokenCacheSize is the maximum number of token→Identity entries kept
	// in the cache. Defaults to 256.
	TokenCacheSize int
	// now overrides the clock for tests.
	now func() time.Time
}

// OIDCVerifier verifies OIDC JWT access tokens against a cached JWKS.
type OIDCVerifier struct {
	cfg     OIDCConfig
	jwksURL string

	mu        sync.RWMutex
	keys      *jose.JSONWebKeySet
	fetchedAt time.Time

	tokenMu    sync.RWMutex
	tokenCache map[string]cachedIdentity
}

// cachedIdentity holds a verified Identity with its expiry time.
type cachedIdentity struct {
	identity  Identity
	expiresAt time.Time
}

const (
	defaultGroupsClaim     = "groups"
	defaultEmailClaim      = "email"
	defaultClientIDClaim   = "client_id"
	defaultRefreshInterval = 15 * time.Minute
	defaultTokenCacheTTL   = 5 * time.Minute
	defaultTokenCacheSize  = 256
)

var defaultAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256,
}

// NewOIDCVerifier validates the config and returns a verifier. The JWKS is
// fetched lazily on first verification.
func NewOIDCVerifier(cfg OIDCConfig) (*OIDCVerifier, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, fmt.Errorf("authz: OIDC issuer is required")
	}
	if strings.TrimSpace(cfg.Audience) == "" {
		return nil, fmt.Errorf("authz: OIDC audience is required")
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = defaultGroupsClaim
	}
	if cfg.EmailClaim == "" {
		cfg.EmailClaim = defaultEmailClaim
	}
	if cfg.ClientIDClaim == "" {
		cfg.ClientIDClaim = defaultClientIDClaim
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if len(cfg.AllowedAlgorithms) == 0 {
		cfg.AllowedAlgorithms = defaultAlgorithms
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.TokenCacheTTL <= 0 {
		cfg.TokenCacheTTL = defaultTokenCacheTTL
	}
	if cfg.TokenCacheSize <= 0 {
		cfg.TokenCacheSize = defaultTokenCacheSize
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &OIDCVerifier{
		cfg:        cfg,
		jwksURL:    strings.TrimSpace(cfg.JWKSURL),
		tokenCache: make(map[string]cachedIdentity, cfg.TokenCacheSize),
	}, nil
}

// Verify implements TokenVerifier.
func (v *OIDCVerifier) Verify(ctx context.Context, rawToken string) (Identity, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return Identity{}, fmt.Errorf("authz: empty token")
	}

	// Check token cache before doing expensive crypto.
	if id, ok := v.tokenCacheLookup(rawToken); ok {
		return id, nil
	}

	parsed, err := jwt.ParseSigned(rawToken, v.cfg.AllowedAlgorithms)
	if err != nil {
		return Identity{}, fmt.Errorf("authz: parse token: %w", err)
	}
	if len(parsed.Headers) == 0 {
		return Identity{}, fmt.Errorf("authz: token missing headers")
	}
	kid := parsed.Headers[0].KeyID

	key, err := v.keyForKID(ctx, kid)
	if err != nil {
		return Identity{}, err
	}

	var std jwt.Claims
	custom := map[string]any{}
	if err := parsed.Claims(key.Key, &std, &custom); err != nil {
		return Identity{}, fmt.Errorf("authz: verify signature: %w", err)
	}

	if err := std.Validate(jwt.Expected{
		Issuer:      v.cfg.Issuer,
		AnyAudience: jwt.Audience{v.cfg.Audience},
		Time:        v.cfg.now(),
	}); err != nil {
		return Identity{}, fmt.Errorf("authz: invalid claims: %w", err)
	}

	id := Identity{
		Subject:  std.Subject,
		Email:    stringClaim(custom, v.cfg.EmailClaim),
		Groups:   stringSliceClaim(custom, v.cfg.GroupsClaim),
		ClientID: clientIDClaim(custom, v.cfg.ClientIDClaim),
	}

	// Cache the verified identity. TTL is min(config TTL, token lifetime).
	v.tokenCacheStore(rawToken, id, std.Expiry)

	return id, nil
}

// keyForKID returns the JWKS key matching kid, refreshing the cache when the
// key is unknown or the cache is stale.
func (v *OIDCVerifier) keyForKID(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	if key, ok := v.lookup(kid); ok {
		return key, nil
	}
	if err := v.refresh(ctx); err != nil {
		return jose.JSONWebKey{}, err
	}
	if key, ok := v.lookup(kid); ok {
		return key, nil
	}
	return jose.JSONWebKey{}, fmt.Errorf("authz: no JWKS key for kid %q", kid)
}

func (v *OIDCVerifier) lookup(kid string) (jose.JSONWebKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.keys == nil {
		return jose.JSONWebKey{}, false
	}
	// Honor cache TTL: a stale cache forces a refresh even on hit.
	if v.cfg.now().Sub(v.fetchedAt) > v.cfg.RefreshInterval {
		return jose.JSONWebKey{}, false
	}
	matches := v.keys.Key(kid)
	// When there is a single unnamed key (no kid), accept it.
	if len(matches) == 0 && kid == "" && len(v.keys.Keys) == 1 {
		return v.keys.Keys[0], true
	}
	if len(matches) == 0 {
		return jose.JSONWebKey{}, false
	}
	return matches[0], true
}

func (v *OIDCVerifier) refresh(ctx context.Context) error {
	jwksURL, err := v.resolveJWKSURL(ctx)
	if err != nil {
		return err
	}
	set, err := v.fetchJWKS(ctx, jwksURL)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys = set
	v.fetchedAt = v.cfg.now()
	v.mu.Unlock()
	return nil
}

func (v *OIDCVerifier) resolveJWKSURL(ctx context.Context) (string, error) {
	if v.jwksURL != "" {
		return v.jwksURL, nil
	}
	discoveryURL := strings.TrimRight(v.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", fmt.Errorf("authz: build discovery request: %w", err)
	}
	resp, err := v.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("authz: fetch discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authz: discovery returned %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("authz: decode discovery: %w", err)
	}
	if strings.TrimSpace(doc.JWKSURI) == "" {
		return "", fmt.Errorf("authz: discovery missing jwks_uri")
	}
	v.jwksURL = doc.JWKSURI
	return doc.JWKSURI, nil
}

func (v *OIDCVerifier) fetchJWKS(ctx context.Context, jwksURL string) (*jose.JSONWebKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("authz: build JWKS request: %w", err)
	}
	resp, err := v.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authz: fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authz: JWKS returned %d", resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("authz: decode JWKS: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, fmt.Errorf("authz: JWKS contained no keys")
	}
	return &set, nil
}

func stringClaim(claims map[string]any, name string) string {
	if s, ok := claims[name].(string); ok {
		return s
	}
	return ""
}

func clientIDClaim(claims map[string]any, name string) string {
	if s := stringClaim(claims, name); s != "" {
		return s
	}
	// Common OIDC fallback for the authorized party.
	return stringClaim(claims, "azp")
}

func stringSliceClaim(claims map[string]any, name string) []string {
	switch val := claims[name].(type) {
	case []string:
		return val
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if val == "" {
			return nil
		}
		// Space-delimited groups claim.
		return strings.Fields(val)
	default:
		return nil
	}
}

// tokenCacheLookup returns a cached Identity for the raw token if present and
// not expired. Also performs lazy eviction of expired entries when the cache
// exceeds its size limit.
func (v *OIDCVerifier) tokenCacheLookup(rawToken string) (Identity, bool) {
	v.tokenMu.RLock()
	entry, ok := v.tokenCache[rawToken]
	v.tokenMu.RUnlock()
	if !ok {
		return Identity{}, false
	}
	if v.cfg.now().After(entry.expiresAt) {
		// Expired — evict lazily.
		v.tokenMu.Lock()
		delete(v.tokenCache, rawToken)
		v.tokenMu.Unlock()
		return Identity{}, false
	}
	return entry.identity, true
}

// tokenCacheStore caches a verified Identity. The effective TTL is
// min(TokenCacheTTL, time until token expiry). Errors are never cached.
// When the cache is full, expired entries are swept first; if still full,
// the store is a no-op (the next request will simply re-verify).
func (v *OIDCVerifier) tokenCacheStore(rawToken string, id Identity, tokenExpiry *jwt.NumericDate) {
	ttl := v.cfg.TokenCacheTTL
	if tokenExpiry != nil {
		tokenTTL := time.Until(tokenExpiry.Time())
		if tokenTTL > 0 && tokenTTL < ttl {
			ttl = tokenTTL
		}
	}
	if ttl <= 0 {
		return
	}

	expiresAt := v.cfg.now().Add(ttl)

	v.tokenMu.Lock()
	defer v.tokenMu.Unlock()

	// Evict expired entries when at capacity.
	if len(v.tokenCache) >= v.cfg.TokenCacheSize {
		v.evictExpired()
	}
	// If still full after eviction, drop the write (verified tokens are cheap
	// to re-verify, and the next sweep will free space).
	if len(v.tokenCache) >= v.cfg.TokenCacheSize {
		return
	}

	v.tokenCache[rawToken] = cachedIdentity{identity: id, expiresAt: expiresAt}
}

// evictExpired removes all expired entries from the cache. Caller must hold
// tokenMu write lock.
func (v *OIDCVerifier) evictExpired() {
	now := v.cfg.now()
	for k, entry := range v.tokenCache {
		if now.After(entry.expiresAt) {
			delete(v.tokenCache, k)
		}
	}
}
