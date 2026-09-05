package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type tokenVerifierFunc func(context.Context, string) (Identity, error)

func (f tokenVerifierFunc) Verify(ctx context.Context, raw string) (Identity, error) {
	return f(ctx, raw)
}

// verifier that accepts "good" -> alice, rejects everything else.
func aliceVerifier() TokenVerifier {
	return tokenVerifierFunc(func(_ context.Context, raw string) (Identity, error) {
		if raw == "good" {
			return Identity{Subject: "alice", Groups: []string{"team-a"}}, nil
		}
		return Identity{}, errors.New("bad token")
	})
}

func mdCtx(token string) context.Context {
	if token == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
}

func TestUnaryObserveModeNeverRejects(t *testing.T) {
	a := NewAuthenticator(aliceVerifier(), nil, false)
	var seen Identity
	handler := func(ctx context.Context, _ any) (any, error) {
		seen, _ = IdentityFromContext(ctx)
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/M"}

	// Valid token -> identity attached.
	if _, err := a.UnaryServerInterceptor()(mdCtx("good"), nil, info, handler); err != nil {
		t.Fatalf("valid token errored: %v", err)
	}
	if seen.Subject != "alice" {
		t.Fatalf("expected alice, got %+v", seen)
	}

	// Invalid token -> anonymous, no error.
	if _, err := a.UnaryServerInterceptor()(mdCtx("bad"), nil, info, handler); err != nil {
		t.Fatalf("invalid token should not reject in observe mode: %v", err)
	}
	if !seen.IsAnonymous() {
		t.Fatalf("expected anonymous, got %+v", seen)
	}

	// No token -> anonymous, no error.
	if _, err := a.UnaryServerInterceptor()(mdCtx(""), nil, info, handler); err != nil {
		t.Fatalf("missing token should not reject in observe mode: %v", err)
	}
	if !seen.IsAnonymous() {
		t.Fatalf("expected anonymous, got %+v", seen)
	}
}

func TestUnaryEnforceModeRejects(t *testing.T) {
	a := NewAuthenticator(aliceVerifier(), nil, true)
	handler := func(context.Context, any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/M"}

	_, err := a.UnaryServerInterceptor()(mdCtx(""), nil, info, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token: want Unauthenticated, got %v", err)
	}

	_, err = a.UnaryServerInterceptor()(mdCtx("bad"), nil, info, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalid token: want Unauthenticated, got %v", err)
	}

	if _, err := a.UnaryServerInterceptor()(mdCtx("good"), nil, info, handler); err != nil {
		t.Fatalf("valid token should pass: %v", err)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeStream) Context() context.Context { return s.ctx }

func TestStreamInterceptorAttachesIdentity(t *testing.T) {
	a := NewAuthenticator(aliceVerifier(), nil, false)
	var seen Identity
	handler := func(_ any, ss grpc.ServerStream) error {
		seen, _ = IdentityFromContext(ss.Context())
		return nil
	}
	info := &grpc.StreamServerInfo{FullMethod: "/svc/S"}
	err := a.StreamServerInterceptor()(nil, &fakeStream{ctx: mdCtx("good")}, info, handler)
	if err != nil {
		t.Fatalf("stream interceptor errored: %v", err)
	}
	if seen.Subject != "alice" {
		t.Fatalf("expected alice in stream ctx, got %+v", seen)
	}
}

func TestHTTPMiddleware(t *testing.T) {
	// Observe mode: bad token still serves.
	observe := NewAuthenticator(aliceVerifier(), nil, false)
	var seen Identity
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = IdentityFromContext(r.Context())
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer good")
	observe.HTTPMiddleware(next).ServeHTTP(rec, req)
	if seen.Subject != "alice" {
		t.Fatalf("expected alice, got %+v", seen)
	}

	// Enforce mode: no token -> 401.
	enforce := NewAuthenticator(aliceVerifier(), nil, true)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	enforce.HTTPMiddleware(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestParseBearer(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"BEARER  abc": "abc",
		"abc":         "",
		"":            "",
		"Basic xyz":   "",
	}
	for in, want := range cases {
		if got := parseBearer(in); got != want {
			t.Errorf("parseBearer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestObserveHook(t *testing.T) {
	counts := map[string]int{}
	a := NewAuthenticator(aliceVerifier(), nil, false)
	a.Observe = func(outcome string) { counts[outcome]++ }
	handler := func(context.Context, any) (any, error) { return nil, nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/M"}

	_, _ = a.UnaryServerInterceptor()(mdCtx("good"), nil, info, handler)
	_, _ = a.UnaryServerInterceptor()(mdCtx("bad"), nil, info, handler)
	_, _ = a.UnaryServerInterceptor()(mdCtx(""), nil, info, handler)

	if counts["authenticated"] != 1 || counts["anonymous"] != 2 {
		t.Fatalf("unexpected outcome counts: %v", counts)
	}
}
