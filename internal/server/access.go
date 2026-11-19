package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/radryc/monofs/internal/monopath"
	"github.com/radryc/monofs/internal/transport"
	"github.com/radryc/monofs/pkg/audit"
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
// RPCs. It is nil (or disabled) unless --access-enforce and a policy
// source are configured.
type AccessControl struct {
	verifier      authz.TokenVerifier
	internalToken string
	enforce       bool
	loader        *policy.Loader
	logger        *slog.Logger
	auditEmitter  atomic.Pointer[audit.WALSpoolEmitter]
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

// SetTransport configures the STRATA_TLS_* contract used for outgoing
// connections (router topology refresh, peer forwarding, fetcher traffic).
// Call it before EnableForwarding and ConfigureFetcher.
func (s *Server) SetTransport(cfg transport.Config) { s.transport = cfg }

// SetAuditEmitter attaches an optional WAL-backed audit emitter to AccessControl.
func (a *AccessControl) SetAuditEmitter(emitter *audit.WALSpoolEmitter) {
	if a == nil {
		return
	}
	a.auditEmitter.Store(emitter)
}

func (a *AccessControl) emitAudit(ctx context.Context, id authz.Identity, action audit.Action, resource string, decision audit.Decision) error {
	if a == nil {
		return nil
	}
	emitter := a.auditEmitter.Load()
	if emitter == nil {
		return nil
	}
	err := emitter.Emit(ctx, audit.Event{
		ActorSubject:  id.Subject,
		ActorClientID: id.ClientID,
		Action:        action,
		Resource:      resource,
		Decision:      decision,
	})
	if err != nil {
		a.logger.Warn("access control: audit emit failed", "error", err)
	}
	return err
}

func (a *AccessControl) emitGRPCDecision(ctx context.Context, fullMethod string, err error) {
	if a == nil || a.auditEmitter.Load() == nil {
		return
	}
	id, _ := authz.IdentityFromContext(ctx)
	code := status.Code(err)
	switch {
	case code == codes.PermissionDenied || code == codes.Unauthenticated:
		_ = a.emitAudit(ctx, id, audit.ActionAuth, fullMethod, audit.DecisionDeny)
	case err == nil:
		_ = a.emitAudit(ctx, id, audit.ActionAuth, fullMethod, audit.DecisionAllow)
	}
}

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
		authCtx := a.authenticate(ctx)
		resp, err := handler(authCtx, req)
		var method string
		if info != nil {
			method = info.FullMethod
		}
		a.emitGRPCDecision(authCtx, method, err)
		return resp, err
	}
}

// StreamInterceptor authenticates streaming calls.
func (a *AccessControl) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		authCtx := a.authenticate(ss.Context())
		err := handler(srv, &accessServerStream{ServerStream: ss, ctx: authCtx})
		var method string
		if info != nil {
			method = info.FullMethod
		}
		a.emitGRPCDecision(authCtx, method, err)
		return err
	}
}

type accessServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *accessServerStream) Context() context.Context { return s.ctx }

// isAdmin reports whether the identity bypasses filtering.
func (a *AccessControl) isAdmin(id authz.Identity) bool {
	if id.Subject == "break-glass-admin" || id.ClientID == "break-glass-admin" {
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
	id, _ := authz.IdentityFromContext(ctx)
	if isInternalCall(ctx) {
		if err := a.emitAudit(ctx, id, audit.ActionRead, fullPath, audit.DecisionAllow); err != nil {
			return status.Errorf(codes.Internal, "audit log failure: %v", err)
		}
		return nil
	}
	if a.isAdmin(id) {
		if err := a.emitAudit(ctx, id, audit.ActionRead, fullPath, audit.DecisionAllow); err != nil {
			return status.Errorf(codes.Internal, "audit log failure: %v", err)
		}
		return nil
	}
	p := a.loader.Get()
	if p == nil {
		_ = a.emitAudit(ctx, id, audit.ActionRead, fullPath, audit.DecisionDeny)
		return status.Error(codes.PermissionDenied, "access policy unavailable")
	}
	if PolicyAllowsPath(p, id, fullPath) {
		if err := a.emitAudit(ctx, id, audit.ActionRead, fullPath, audit.DecisionAllow); err != nil {
			return status.Errorf(codes.Internal, "audit log failure: %v", err)
		}
		return nil
	}
	_ = a.emitAudit(ctx, id, audit.ActionRead, fullPath, audit.DecisionDeny)
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
