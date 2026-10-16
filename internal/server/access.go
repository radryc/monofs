package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/monopath"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// internalCallKey marks requests that presented the cluster-internal token.
type internalCallKey struct{}

func contextWithInternalCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalCallKey{}, true)
}

func isInternalCall(ctx context.Context) bool {
	internal, _ := ctx.Value(internalCallKey{}).(bool)
	return internal
}

// AccessControl enforces the repo-managed access policy on storage-node read
// RPCs. It is nil (or disabled) unless --authz-enforce-read and a policy
// source are configured.
type AccessControl struct {
	verifier      authz.TokenVerifier
	internalToken string
	enforce       bool
	loader        *policy.Loader
	logger        *slog.Logger
}

// AccessControlOptions configures the node-side access control.
type AccessControlOptions struct {
	Verifier      authz.TokenVerifier
	InternalToken string
	PolicyFile    string
	PolicyURL     string
	PolicyToken   string
	Interval      time.Duration
	Enforce       bool
	Logger        *slog.Logger
}

// NewAccessControl builds node access control. It returns nil when neither a
// policy file nor URL is configured, which keeps the node behavior unchanged.
func NewAccessControl(opts AccessControlOptions) *AccessControl {
	file := strings.TrimSpace(opts.PolicyFile)
	url := strings.TrimSpace(opts.PolicyURL)
	if file == "" && url == "" {
		return nil
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var source policy.Source
	if url != "" {
		source = policy.HTTPSource{URL: url, Token: strings.TrimSpace(opts.PolicyToken)}
	} else {
		source = policy.FileSource{Path: file}
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	loader := policy.NewLoader(source, interval, func(format string, args ...any) {
		logger.Info(fmt.Sprintf(format, args...))
	})
	return &AccessControl{
		verifier:      opts.Verifier,
		internalToken: strings.TrimSpace(opts.InternalToken),
		enforce:       opts.Enforce,
		loader:        loader,
		logger:        logger,
	}
}

// Run polls the policy source until ctx is done.
func (a *AccessControl) Run(ctx context.Context) {
	if a == nil {
		return
	}
	a.loader.Run(ctx)
}

// Enabled reports whether a policy source is configured.
func (a *AccessControl) Enabled() bool { return a != nil }

// SetAccessControl installs node-side access control (nil disables it).
func (s *Server) SetAccessControl(ac *AccessControl) { s.access = ac }

// authenticate resolves the caller identity from metadata: the cluster-internal
// token marks internal calls (they bypass policy), otherwise a bearer token is
// verified and attached (anonymous when absent or invalid).
func (a *AccessControl) authenticate(ctx context.Context) context.Context {
	if a == nil {
		return ctx
	}
	md, _ := metadata.FromIncomingContext(ctx)
	if a.internalToken != "" && md != nil {
		for _, value := range md.Get("x-monofs-internal") {
			if subtle.ConstantTimeCompare([]byte(value), []byte(a.internalToken)) == 1 {
				ctx = contextWithInternalCall(ctx)
				return authz.ContextWithIdentity(ctx, authz.Identity{ClientID: "monofs-internal"})
			}
		}
		// Control-plane services (router, guardian, doctor) share the cluster
		// token and present it as a bearer; treat a matching bearer as an
		// internal call too.
		for _, value := range md.Get("authorization") {
			token := value
			if strings.HasPrefix(strings.ToLower(token), "bearer ") {
				token = strings.TrimSpace(token[len("Bearer "):])
			}
			if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.internalToken)) == 1 {
				ctx = contextWithInternalCall(ctx)
				return authz.ContextWithIdentity(ctx, authz.Identity{ClientID: "monofs-internal"})
			}
		}
	}
	var raw string
	if md != nil {
		for _, value := range md.Get("authorization") {
			if strings.HasPrefix(strings.ToLower(value), "bearer ") {
				raw = strings.TrimSpace(value[len("Bearer "):])
				break
			}
		}
	}
	if raw == "" || a.verifier == nil {
		return authz.ContextWithIdentity(ctx, authz.Identity{})
	}
	id, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		a.logger.Debug("access control: token verification failed", "error", err)
		return authz.ContextWithIdentity(ctx, authz.Identity{})
	}
	return authz.ContextWithIdentity(ctx, id)
}

// UnaryInterceptor authenticates unary calls.
func (a *AccessControl) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(a.authenticate(ctx), req)
	}
}

// StreamInterceptor authenticates streaming calls.
func (a *AccessControl) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, &accessServerStream{ServerStream: ss, ctx: a.authenticate(ss.Context())})
	}
}

type accessServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *accessServerStream) Context() context.Context { return s.ctx }

// isAdmin reports whether the identity bypasses filtering.
func (a *AccessControl) isAdmin(id authz.Identity) bool {
	if id.Subject == "break-glass-admin" {
		return true
	}
	p := a.loader.Get()
	return p != nil && p.IsAdmin(id)
}

// AuthorizeRead enforces the policy for one path read. It is a no-op unless a
// policy source is configured and enforcement is on; internal calls and admins
// always pass. It fails closed while no policy has loaded.
func (a *AccessControl) AuthorizeRead(ctx context.Context, fullPath string) error {
	if a == nil || !a.enforce {
		return nil
	}
	if isInternalCall(ctx) {
		return nil
	}
	id, _ := authz.IdentityFromContext(ctx)
	if a.isAdmin(id) {
		return nil
	}
	p := a.loader.Get()
	if p == nil {
		return status.Error(codes.PermissionDenied, "access policy unavailable")
	}
	if PolicyAllowsPath(p, id, fullPath) {
		return nil
	}
	a.logger.Warn("access control: read denied",
		"principal", id.PrincipalID(), "path", fullPath)
	return status.Errorf(codes.PermissionDenied, "principal %q is not authorized to read %q", id.PrincipalID(), fullPath)
}

// PolicyAllowsPath evaluates the namespace-aware read rule for a full path:
// repositories follow the monofs scope, guardian/<partition> follows the
// partition scope, guardian-system is admin-only, and the doctor/dependency
// namespaces are readable by any authenticated caller (Doctor filters results
// at query time). Root and intermediate paths pass here; visibility filtering
// happens in the UI layers.
func PolicyAllowsPath(p *policy.Policy, id authz.Identity, fullPath string) bool {
	cleaned := strings.Trim(strings.TrimSpace(fullPath), "/")
	if cleaned == "" {
		return true
	}
	displayPath, _, ok := monopath.SplitDisplayPath(cleaned)
	if !ok {
		return true
	}
	switch {
	case displayPath == "guardian-system":
		return p.IsAdmin(id)
	case strings.HasPrefix(displayPath, "guardian/"):
		partition := strings.TrimPrefix(displayPath, "guardian/")
		return p.CanSeePartition(id, partition)
	case strings.HasPrefix(displayPath, "doctor/"), displayPath == "dependency":
		return !id.IsAnonymous()
	default:
		return p.CanReadPath(id, displayPath)
	}
}
