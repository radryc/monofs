package authz

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// identityContextKey is the private context key under which the authenticated
// Identity is stored.
type identityContextKey struct{}

// ContextWithIdentity returns a copy of ctx carrying the identity.
func ContextWithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}

// IdentityFromContext returns the authenticated Identity attached to ctx, if any.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(Identity)
	return id, ok
}

// Authenticator extracts and verifies bearer tokens, attaching the resulting
// Identity to the request context. In OBSERVE mode (RequireToken=false) it
// never rejects a request: unverifiable tokens yield an anonymous identity and
// a log line. In ENFORCE mode (RequireToken=true) missing or invalid tokens are
// rejected.
//
// Authorization (grant checks) is performed separately at call sites via a
// GrantEvaluator; this type only handles authentication.
type Authenticator struct {
	Verifier     TokenVerifier
	Logger       *slog.Logger
	RequireToken bool
	// Observe is an optional hook invoked with an outcome label
	// ("authenticated", "anonymous", "rejected") for metrics.
	Observe func(outcome string)
}

// NewAuthenticator builds an Authenticator. When verifier is nil a NoopVerifier
// is used. When logger is nil a discard logger is used.
func NewAuthenticator(verifier TokenVerifier, logger *slog.Logger, requireToken bool) *Authenticator {
	if verifier == nil {
		verifier = NoopVerifier{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Authenticator{Verifier: verifier, Logger: logger, RequireToken: requireToken}
}

func (a *Authenticator) observe(outcome string) {
	if a.Observe != nil {
		a.Observe(outcome)
	}
}

// authenticate verifies rawToken and returns a context annotated with the
// resulting Identity. In OBSERVE mode it never returns an error.
func (a *Authenticator) authenticate(ctx context.Context, rawToken, source string) (context.Context, error) {
	if strings.TrimSpace(rawToken) == "" {
		if a.RequireToken {
			a.observe("rejected")
			return ctx, status.Error(codes.Unauthenticated, "authz: missing bearer token")
		}
		a.observe("anonymous")
		return ContextWithIdentity(ctx, Identity{}), nil
	}

	id, err := a.Verifier.Verify(ctx, rawToken)
	if err != nil {
		if a.RequireToken {
			a.observe("rejected")
			return ctx, status.Error(codes.Unauthenticated, "authz: invalid token")
		}
		a.Logger.Warn("authz observe: token rejected", "source", source, "error", err)
		a.observe("anonymous")
		return ContextWithIdentity(ctx, Identity{}), nil
	}

	if id.IsAnonymous() {
		a.observe("anonymous")
	} else {
		a.Logger.Debug("authz observe: identity resolved",
			"source", source, "principal", id.PrincipalID(), "groups", len(id.Groups))
		a.observe("authenticated")
	}
	return ContextWithIdentity(ctx, id), nil
}

// UnaryServerInterceptor authenticates unary gRPC calls.
func (a *Authenticator) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := a.authenticate(ctx, bearerFromMetadata(ctx), "grpc:"+info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamServerInterceptor authenticates streaming gRPC calls.
func (a *Authenticator) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := a.authenticate(ss.Context(), bearerFromMetadata(ss.Context()), "grpc:"+info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &identityServerStream{ServerStream: ss, ctx: ctx})
	}
}

// HTTPMiddleware authenticates HTTP requests, attaching the Identity to the
// request context.
func (a *Authenticator) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := a.authenticate(r.Context(), bearerFromHeader(r.Header.Get("Authorization")), "http:"+r.URL.Path)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// identityServerStream overrides Context() so downstream handlers see the
// authenticated identity.
type identityServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *identityServerStream) Context() context.Context { return s.ctx }

// bearerFromMetadata reads a bearer token from the gRPC "authorization" metadata.
func bearerFromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, v := range md.Get("authorization") {
		if tok := parseBearer(v); tok != "" {
			return tok
		}
	}
	return ""
}

// bearerFromHeader parses a bearer token from an HTTP Authorization header value.
func bearerFromHeader(header string) string {
	return parseBearer(header)
}

func parseBearer(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	if len(header) >= 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}
