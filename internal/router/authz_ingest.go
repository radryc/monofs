package router

import (
	"context"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SetGrantEvaluator installs a grant evaluator and toggles ingest enforcement.
func (r *Router) SetGrantEvaluator(store authz.GrantEvaluator, enforce bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grantEvaluator = store
	r.authzEnforceIngest = enforce
}

// AddBreakGlassAdmin registers a client ID that bypasses partition-level
// authorization for ingest and read operations.
func (r *Router) AddBreakGlassAdmin(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := strings.TrimSpace(clientID); s != "" {
		r.breakGlassAdmins[s] = true
	}
}

func (r *Router) isBreakGlassAdmin(id authz.Identity) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.breakGlassAdmins[id.ClientID] || r.breakGlassAdmins[id.Subject]
}

func (r *Router) authorizeIngest(ctx context.Context, req *pb.IngestRequest, displayPath string) error {
	if !r.authzEnforceIngest || r.grantEvaluator == nil {
		return nil
	}

	id, _ := authz.IdentityFromContext(ctx)
	if id.IsAnonymous() {
		return status.Errorf(codes.PermissionDenied, "anonymous ingest denied; authentication required")
	}

	partition := ingestPartitionForGrant(req, displayPath)

	if r.isBreakGlassAdmin(id) {
		return nil
	}

	if r.grantEvaluator.Can(ctx, id, partition, authz.ActionIngest) {
		return nil
	}

	r.logger.Warn("ingest denied by partition authz",
		"principal", id.PrincipalID(), "partition", partition)
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
