package router

import (
	"context"

	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// authorizeRead enforces the viewer role on read/mount access to a partition
// (authz epic C2). It is a no-op unless AuthzEnforceRead is set and a grant
// evaluator is present. Identity is read from ctx (populated by the auth
// interceptor); anonymous callers are denied when enforcement is on.
func (r *Router) authorizeRead(ctx context.Context, partition string) error {
	if !r.authzEnforceRead || r.grantEvaluator == nil {
		return nil
	}
	id, _ := authz.IdentityFromContext(ctx)
	if r.isBreakGlassAdmin(id) {
		return nil
	}
	if r.grantEvaluator.Can(ctx, id, partition, authz.ActionView) {
		return nil
	}
	r.logger.Warn("read denied by partition authz",
		"principal", id.PrincipalID(), "partition", partition)
	return status.Errorf(codes.PermissionDenied,
		"principal %q is not authorized to read partition %q", id.PrincipalID(), partition)
}

// SetAuthzEnforceRead toggles read/mount viewer enforcement (tests and wiring).
func (r *Router) SetAuthzEnforceRead(enforce bool) {
	r.authzEnforceRead = enforce
}
