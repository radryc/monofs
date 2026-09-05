package authz

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testIssuer   = "https://idp.example.com"
	testAudience = "monofs"
	testKID      = "test-key-1"
)

type testIDP struct {
	priv      *rsa.PrivateKey
	server    *httptest.Server
	jwksFetch int64
}

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	idp := &testIDP{priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&idp.jwksFetch, 1)
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       &priv.PublicKey,
			KeyID:     testKID,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *testIDP) sign(t *testing.T, std jwt.Claims, custom map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: idp.priv},
		(&jose.SignerOptions{}).WithHeader("kid", testKID).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(std).Claims(custom).Serialize()
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

func fixedClock(base time.Time) func() time.Time {
	return func() time.Time { return base }
}

func newTestVerifier(t *testing.T, idp *testIDP, base time.Time) *OIDCVerifier {
	t.Helper()
	v, err := NewOIDCVerifier(OIDCConfig{
		Issuer:   testIssuer,
		Audience: testAudience,
		JWKSURL:  idp.server.URL + "/jwks",
		now:      fixedClock(base),
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v
}

func TestOIDCVerifyHappyPath(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, idp, base)

	raw := idp.sign(t, jwt.Claims{
		Issuer:   testIssuer,
		Subject:  "alice",
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(base.Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(base.Add(-time.Minute)),
	}, map[string]any{
		"email":  "alice@example.com",
		"groups": []string{"strata-platform-eng", "everyone"},
	})

	id, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Subject != "alice" || id.Email != "alice@example.com" {
		t.Fatalf("unexpected identity: %+v", id)
	}
	if !id.HasGroup("strata-platform-eng") {
		t.Fatalf("expected group membership, got %v", id.Groups)
	}
}

func TestOIDCVerifyExpired(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, idp, base)

	raw := idp.sign(t, jwt.Claims{
		Issuer:   testIssuer,
		Subject:  "alice",
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(base.Add(-time.Hour)),
	}, nil)

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestOIDCVerifyWrongAudience(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, idp, base)

	raw := idp.sign(t, jwt.Claims{
		Issuer:   testIssuer,
		Subject:  "alice",
		Audience: jwt.Audience{"someone-else"},
		Expiry:   jwt.NewNumericDate(base.Add(time.Hour)),
	}, nil)

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestOIDCVerifyWrongIssuer(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, idp, base)

	raw := idp.sign(t, jwt.Claims{
		Issuer:   "https://evil.example.com",
		Subject:  "alice",
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(base.Add(time.Hour)),
	}, nil)

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestOIDCGroupsAsSpaceDelimitedString(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, idp, base)

	raw := idp.sign(t, jwt.Claims{
		Issuer:   testIssuer,
		Subject:  "svc",
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(base.Add(time.Hour)),
	}, map[string]any{
		"client_id": "ci-bot",
		"groups":    "team-a team-b",
	})

	id, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.ClientID != "ci-bot" {
		t.Fatalf("expected client id, got %q", id.ClientID)
	}
	if !id.HasGroup("team-a") || !id.HasGroup("team-b") {
		t.Fatalf("expected parsed groups, got %v", id.Groups)
	}
}

func TestOIDCJWKSCachedAndRefreshed(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	v, err := NewOIDCVerifier(OIDCConfig{
		Issuer:          testIssuer,
		Audience:        testAudience,
		JWKSURL:         idp.server.URL + "/jwks",
		RefreshInterval: 10 * time.Minute,
		now:             func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	mkToken := func() string {
		return idp.sign(t, jwt.Claims{
			Issuer:   testIssuer,
			Subject:  "alice",
			Audience: jwt.Audience{testAudience},
			Expiry:   jwt.NewNumericDate(clock.Add(time.Hour)),
		}, nil)
	}

	if _, err := v.Verify(context.Background(), mkToken()); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := v.Verify(context.Background(), mkToken()); err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if got := atomic.LoadInt64(&idp.jwksFetch); got != 1 {
		t.Fatalf("expected 1 JWKS fetch (cached), got %d", got)
	}

	// Advance beyond the refresh interval; next verify must refetch.
	clock = base.Add(20 * time.Minute)
	if _, err := v.Verify(context.Background(), mkToken()); err != nil {
		t.Fatalf("third verify: %v", err)
	}
	if got := atomic.LoadInt64(&idp.jwksFetch); got != 2 {
		t.Fatalf("expected 2 JWKS fetches (refreshed), got %d", got)
	}
}

func TestNewOIDCVerifierValidation(t *testing.T) {
	if _, err := NewOIDCVerifier(OIDCConfig{Audience: "a"}); err == nil {
		t.Error("expected error when issuer missing")
	}
	if _, err := NewOIDCVerifier(OIDCConfig{Issuer: "i"}); err == nil {
		t.Error("expected error when audience missing")
	}
}

func TestOIDCTokenCacheHit(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, idp, base)

	raw := idp.sign(t, jwt.Claims{
		Issuer:   testIssuer,
		Subject:  "alice",
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(base.Add(time.Hour)),
	}, nil)

	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("second verify: %v", err)
	}
	// Only 1 JWKS fetch: second call hit the token cache.
	if got := atomic.LoadInt64(&idp.jwksFetch); got != 1 {
		t.Fatalf("expected 1 JWKS fetch (token cached), got %d", got)
	}
}

func TestOIDCTokenCacheExpiresAfterTTL(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	v, err := NewOIDCVerifier(OIDCConfig{
		Issuer:        testIssuer,
		Audience:      testAudience,
		JWKSURL:       idp.server.URL + "/jwks",
		TokenCacheTTL: 2 * time.Minute,
		now:           func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	raw := idp.sign(t, jwt.Claims{
		Issuer:   testIssuer,
		Subject:  "alice",
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(base.Add(time.Hour)),
	}, nil)

	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("second verify (cached): %v", err)
	}
	if got := atomic.LoadInt64(&idp.jwksFetch); got != 1 {
		t.Fatalf("expected 1 JWKS fetch, got %d", got)
	}

	// Advance past the 2min token cache TTL.
	clock = base.Add(3 * time.Minute)
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("third verify (after cache expiry): %v", err)
	}
	// Must have re-verified (token cache miss, but JWKS still cached).
	if got := atomic.LoadInt64(&idp.jwksFetch); got != 1 {
		t.Fatalf("expected 1 JWKS fetch (JWKS still cached), got %d", got)
	}
	// Verify again — should be cached now.
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("fourth verify (re-cached): %v", err)
	}
}

func TestOIDCTokenCacheRespectsTokenExpiry(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	v, err := NewOIDCVerifier(OIDCConfig{
		Issuer:        testIssuer,
		Audience:      testAudience,
		JWKSURL:       idp.server.URL + "/jwks",
		TokenCacheTTL: 10 * time.Minute, // longer than token lifetime
		now:           func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	// Token expires in 3 minutes — cache TTL should be capped to that.
	raw := idp.sign(t, jwt.Claims{
		Issuer:   testIssuer,
		Subject:  "alice",
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(base.Add(3 * time.Minute)),
	}, nil)

	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("first verify: %v", err)
	}

	// At 2min: still cached (token exp 3min, cache TTL capped to 3min).
	clock = base.Add(2 * time.Minute)
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("verify at 2min: %v", err)
	}
	if got := atomic.LoadInt64(&idp.jwksFetch); got != 1 {
		t.Fatalf("expected 1 JWKS fetch at 2min, got %d", got)
	}

	// At 4min: token cache expired (past the 3min cap).
	clock = base.Add(4 * time.Minute)
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("verify at 4min: %v", err)
	}
}

func TestOIDCTokenCacheDoesNotCacheErrors(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, idp, base)

	// Wrong audience — should fail.
	raw := idp.sign(t, jwt.Claims{
		Issuer:   testIssuer,
		Subject:  "alice",
		Audience: jwt.Audience{"wrong"},
		Expiry:   jwt.NewNumericDate(base.Add(time.Hour)),
	}, nil)

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("expected error for wrong audience")
	}
	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("expected error on retry (errors must not be cached)")
	}
}

func TestOIDCTokenCacheEviction(t *testing.T) {
	idp := newTestIDP(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v, err := NewOIDCVerifier(OIDCConfig{
		Issuer:         testIssuer,
		Audience:       testAudience,
		JWKSURL:        idp.server.URL + "/jwks",
		TokenCacheTTL:  10 * time.Minute,
		TokenCacheSize: 2, // tiny cache for testing
		now:            fixedClock(base),
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	sign := func(sub string) string {
		return idp.sign(t, jwt.Claims{
			Issuer:   testIssuer,
			Subject:  sub,
			Audience: jwt.Audience{testAudience},
			Expiry:   jwt.NewNumericDate(base.Add(time.Hour)),
		}, nil)
	}

	tok1 := sign("alice")
	tok2 := sign("bob")
	tok3 := sign("charlie")

	if _, err := v.Verify(context.Background(), tok1); err != nil {
		t.Fatalf("verify alice: %v", err)
	}
	if _, err := v.Verify(context.Background(), tok2); err != nil {
		t.Fatalf("verify bob: %v", err)
	}

	// Cache is full (size=2). Adding charlie should not panic.
	if _, err := v.Verify(context.Background(), tok3); err != nil {
		t.Fatalf("verify charlie: %v", err)
	}

	v.tokenMu.RLock()
	size := len(v.tokenCache)
	v.tokenMu.RUnlock()
	if size > 2 {
		t.Fatalf("cache size %d exceeds limit 2", size)
	}
}
