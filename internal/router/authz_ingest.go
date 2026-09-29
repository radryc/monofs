package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/pkg/authz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// buildGrantEvaluator constructs the partition grant evaluator from router
// configuration. Precedence: inline JSON > explicit path > default path under
// the guardian state dir. On a configured-but-unloadable source it returns a
// deny-all evaluator so enforcement fails closed rather than silently opening.
func buildGrantEvaluator(cfg RouterConfig, logger *slog.Logger) authz.GrantEvaluator {
	if raw := strings.TrimSpace(cfg.AuthzGrantsJSON); raw != "" {
		var grants []authz.Grant
		if err := json.Unmarshal([]byte(raw), &grants); err != nil {
			logger.Error("failed to parse authz grants JSON; ingest authorization will deny all", "error", err)
			return authz.DenyAllEvaluator{}
		}
		store, err := authz.NewGrantStore("")
		if err != nil {
			logger.Error("failed to create authz grant store; ingest authorization will deny all", "error", err)
			return authz.DenyAllEvaluator{}
		}
		if err := store.Replace(grants); err != nil {
			logger.Error("invalid authz grants JSON; ingest authorization will deny all", "error", err)
			return authz.DenyAllEvaluator{}
		}
		logger.Info("loaded authz grants from JSON", "count", len(grants))
		return store
	}

	path := strings.TrimSpace(cfg.AuthzGrantsPath)
	if path == "" && strings.TrimSpace(cfg.GuardianStateDir) != "" {
		path = filepath.Join(cfg.GuardianStateDir, "authz_grants.json")
	}
	if path == "" {
		return nil
	}
	store, err := authz.NewGrantStore(path)
	if err != nil {
		logger.Error("failed to load authz grants; ingest authorization will deny all", "path", path, "error", err)
		return authz.DenyAllEvaluator{}
	}
	logger.Info("loaded authz grants", "path", path, "count", len(store.Grants()))
	return store
}

// SetGrantEvaluator installs a grant evaluator and toggles ingest enforcement.
func (r *Router) SetGrantEvaluator(store authz.GrantEvaluator, enforce bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grantEvaluator = store
	r.authzEnforceIngest = enforce
}

// AddBreakGlassAdmin registers a client ID that bypasses partition-level
// authorization for ingest and read operations. The same principal is recorded
// as the router's service identity so internal, non-request work (auto-refresh
// re-ingestion, workspace sync) is attributed rather than treated as anonymous.
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
// that do not carry one. This keeps ingest authorization meaningful while
// ensuring trusted in-process re-ingestion is not rejected as anonymous.
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

func (r *Router) authorizeIngest(ctx context.Context, req *pb.IngestRequest, displayPath string) error {
	r.mu.RLock()
	enforce := r.authzEnforceIngest
	evaluator := r.grantEvaluator
	r.mu.RUnlock()

	if !enforce {
		return nil
	}
	if evaluator == nil {
		// Enforcement is on but no grants could be loaded: fail closed.
		return status.Errorf(codes.PermissionDenied, "ingest authorization enforced but no grant evaluator is configured")
	}

	id, _ := authz.IdentityFromContext(ctx)
	if id.IsAnonymous() {
		return status.Errorf(codes.PermissionDenied, "anonymous ingest denied; authentication required")
	}

	partition := ingestPartitionForGrant(req, displayPath)

	if r.isBreakGlassAdmin(id) {
		return nil
	}

	if evaluator.Can(ctx, id, partition, authz.ActionIngest) {
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
