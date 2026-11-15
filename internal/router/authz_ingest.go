package router

import (
	"context"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AddBreakGlassAdmin registers a client ID that bypasses partition-level
// authorization. The same principal is recorded as the router's service
// identity so internal, non-request work (auto-refresh re-ingestion, workspace
// sync) is attributed rather than treated as anonymous.
func (r *Router) AddBreakGlassAdmin(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := strings.TrimSpace(clientID); s != "" {
		r.breakGlassAdmins[s] = true
		r.serviceIdentity = authz.Identity{ClientID: s, Subject: s}
	}
}

// internalIdentityContext returns a detached context carrying the router's
// service identity, for internal work that runs outside an authenticated
// request. When no break-glass/service identity is configured the identity is
// anonymous and ingest authorization fails closed under enforcement.
func (r *Router) internalIdentityContext() context.Context {
	r.mu.RLock()
	id := r.serviceIdentity
	r.mu.RUnlock()
	return authz.ContextWithIdentity(context.Background(), id)
}

// ingestContext preserves a caller identity already attached to ctx and falls
// back to the router service identity for internal callers (workspace sync)
// that do not carry one.
func (r *Router) ingestContext(ctx context.Context) context.Context {
	if id, ok := authz.IdentityFromContext(ctx); ok && !id.IsAnonymous() {
		return ctx
	}
	r.mu.RLock()
	id := r.serviceIdentity
	r.mu.RUnlock()
	return authz.ContextWithIdentity(ctx, id)
}

func (r *Router) isBreakGlassAdmin(id authz.Identity) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.breakGlassAdmins[id.ClientID] || r.breakGlassAdmins[id.Subject]
}

// authorizeIngest enforces the repo-managed access policy on writes. It is a
// no-op when no policy is configured or when the policy is in observe mode.
// Under enforcement a missing bundle, an anonymous caller, or a role below
// ingester is denied.
func (r *Router) authorizeIngest(ctx context.Context, req *pb.IngestRequest, displayPath string) error {
	state := r.accessPolicySnapshot()
	if state == nil || !state.enforce.Load() {
		return nil
	}
	p := state.loader.Get()
	if p == nil {
		return status.Errorf(codes.PermissionDenied, "ingest authorization enforced but the access policy is not loaded")
	}
	id, _ := authz.IdentityFromContext(ctx)
	if r.isBreakGlassAdmin(id) {
		return nil
	}
	if id.IsAnonymous() {
		return status.Errorf(codes.PermissionDenied, "anonymous ingest denied; authentication required")
	}

	partition := ingestPartitionForGrant(req, displayPath)
	if p.Role(id, partition).Allows(authz.ActionIngest) {
		return nil
	}

	r.logger.Warn("ingest denied by access policy",
		"principal", id.PrincipalID(), "partition", partition, "policy", p.Version())
	return status.Errorf(codes.PermissionDenied,
		"principal %q is not authorized to ingest into partition %q", id.PrincipalID(), partition)
}

func ingestPartitionForGrant(req *pb.IngestRequest, displayPath string) string {
	if req != nil && req.IngestionType == pb.IngestionType_INGESTION_GUARDIAN && req.SourceId != "" {
		return strings.SplitN(req.SourceId, "/", 2)[0]
	}
	displayPath = strings.TrimPrefix(displayPath, "/")
	displayPath = strings.TrimPrefix(displayPath, "guardian/")
	if req != nil && strings.Contains(displayPath, "/") && req.IngestionType == pb.IngestionType_INGESTION_GIT {
		return strings.SplitN(displayPath, "/", 2)[0]
	}
	return displayPath
}

// RecordAuthOutcome records an authentication outcome metric.
func RecordAuthOutcome(outcome, protocol string) {
	authOutcomes.WithLabelValues(outcome, protocol).Inc()
}
