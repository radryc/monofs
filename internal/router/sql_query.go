package router

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sqlfederation"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"github.com/radryc/sqlagregator/pkg/sqlag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// QuerySQL federates one generic SELECT across the healthy local shards.
// It is deliberately unavailable when telemetry reads are shared (ClickHouse),
// because in that mode the shards are replicas, not disjoint partitions.
func (r *Router) QuerySQL(ctx context.Context, req *pb.QuerySQLRequest) (*pb.QuerySQLResponse, error) {
	dataset := strings.ToLower(strings.TrimSpace(req.GetDataset()))
	signal, ok := datasetSignal(dataset)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported SQL dataset %q", dataset)
	}
	partitions, restricted, err := r.querySQLScope(ctx, signal, req.GetVisiblePartitions(), req.GetVisiblePartitionsSet())
	if err != nil {
		return nil, err
	}
	scope := sqlag.Scope{Restricted: restricted, Partitions: partitions}
	from := unixNanoTime(req.GetFromUnixNano())
	to := unixNanoTime(req.GetToUnixNano())
	limit := int(req.GetLimit())
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	if pol := r.policyInstance(); pol != nil && len(pol.Partitions()) > 0 {
		// Policy-routed shards are explicit backend placements, not node
		// replicas, so they remain valid when telemetry reads are shared via
		// ClickHouse. The legacy node fan-out below does not.
		return r.querySQLPolicy(ctx, req, pol, dataset, scope, from, to, limit)
	}
	if r.sharedTelemetryReads() {
		return nil, status.Error(codes.Unimplemented,
			"federated SQL is not enabled for shared telemetry backends; use the direct ClickHouse driver")
	}
	nodes := r.healthyFederationNodes()
	if len(nodes) == 0 {
		return nil, status.Error(codes.Unavailable, "no healthy nodes available")
	}
	cat, err := sqlfederation.BuildCatalog(dataset, nodes, r.federationMembers())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build SQL catalog: %v", err)
	}
	coord := sqlag.New(cat)
	for _, node := range nodes {
		coord.Register(&sqlfederation.NodeBackend{Node: node})
	}

	result, err := coord.Query(ctx, sqlag.Request{
		SQL:          req.GetQuery(),
		From:         from,
		To:           to,
		Limit:        limit,
		Selector:     sqlfederation.ConvertSelector(req.GetBackends()),
		AllowPartial: req.GetAllowPartial(),
		Scope:        scope,
	})
	if err != nil {
		code := codes.InvalidArgument
		if strings.Contains(err.Error(), "endpoint ") || strings.Contains(err.Error(), "connection") {
			code = codes.Unavailable
		}
		return nil, status.Errorf(code, "federated SQL: %v", err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal federated SQL result: %v", err)
	}
	return &pb.QuerySQLResponse{ResultsJson: string(data)}, nil
}

// SetPlacementBackendResolver installs the executor factory for policy-routed
// placements. Without one, a policy with partition definitions makes QuerySQL
// fail closed with Unimplemented.
func (r *Router) SetPlacementBackendResolver(resolver sqlfederation.BackendResolver) {
	r.placementMu.Lock()
	r.placementResolver = resolver
	r.placementMu.Unlock()
}

func (r *Router) placementBackendResolver() sqlfederation.BackendResolver {
	r.placementMu.RLock()
	defer r.placementMu.RUnlock()
	return r.placementResolver
}

func (r *Router) querySQLPolicy(ctx context.Context, req *pb.QuerySQLRequest, pol *policy.Policy, dataset string, scope sqlag.Scope, from, to time.Time, limit int) (*pb.QuerySQLResponse, error) {
	if scope.Restricted && len(scope.Partitions) == 0 {
		return nil, status.Error(codes.PermissionDenied, "federated SQL: no visible partitions")
	}
	visible := pol.SortedPartitionNames()
	if scope.Restricted {
		visible = intersectPartitions(visible, scope.Partitions)
	}
	if req.GetVisiblePartitionsSet() {
		visible = intersectPartitions(visible, req.GetVisiblePartitions())
	}
	if len(visible) == 0 {
		return nil, status.Error(codes.PermissionDenied, "federated SQL: no visible partitions")
	}
	id, _ := authz.IdentityFromContext(ctx)
	plan, err := sqlfederation.Route(pol, id, sqlfederation.RouteRequest{
		Dataset: dataset,
		Visible: visible,
	})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "federated SQL: %v", err)
	}
	if len(plan.Endpoints) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "federated SQL: no route for the visible partitions")
	}
	resolver := r.placementBackendResolver()
	if resolver == nil {
		return nil, status.Error(codes.Unimplemented, "federated SQL: placement backend resolver is not configured")
	}
	cat, err := plan.Catalog(dataset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "federated SQL: %v", err)
	}
	coord := sqlag.New(cat)
	for _, routed := range plan.Endpoints {
		backend, err := resolver.Resolve(ctx, routed)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "federated SQL: endpoint %s: %v", routed.Endpoint.ID, err)
		}
		coord.Register(backend)
	}
	result, err := coord.Query(ctx, sqlag.Request{
		SQL:          req.GetQuery(),
		From:         from,
		To:           to,
		Limit:        limit,
		Selector:     sqlfederation.ConvertSelector(req.GetBackends()),
		AllowPartial: req.GetAllowPartial(),
		Scope:        scope,
	})
	if err != nil {
		code := codes.InvalidArgument
		if strings.Contains(err.Error(), "endpoint ") || strings.Contains(err.Error(), "connection") {
			code = codes.Unavailable
		}
		return nil, status.Errorf(code, "federated SQL: %v", err)
	}
	if len(plan.Warnings) > 0 {
		result.Warnings = append(result.Warnings, plan.Warnings...)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal federated SQL result: %v", err)
	}
	return &pb.QuerySQLResponse{ResultsJson: string(data)}, nil
}

func datasetSignal(dataset string) (policy.Signal, bool) {
	switch dataset {
	case "logs":
		return policy.SignalLogs, true
	case "traces":
		return policy.SignalTraces, true
	case "metrics":
		return policy.SignalMetrics, true
	default:
		return "", false
	}
}

// FederationNodes returns the healthy nodes as SQL shards. Placement
// resolvers use it to fan a monofs_local shard out over the cluster.
func (r *Router) FederationNodes() []sqlfederation.Node { return r.healthyFederationNodes() }

func (r *Router) healthyFederationNodes() []sqlfederation.Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nodes := make([]sqlfederation.Node, 0, len(r.nodes))
	for nodeID, state := range r.nodes {
		if state == nil || state.info == nil || !state.info.Healthy || state.status != NodeActive || state.client == nil {
			continue
		}
		nodes = append(nodes, sqlfederation.Node{
			ID:      nodeID,
			Address: state.externalAddress,
			Client:  state.client,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes
}

// federationMembers counts active shards, including unhealthy ones, so a
// missing shard becomes a coverage gap rather than a low aggregate.
func (r *Router) federationMembers() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, state := range r.nodes {
		if state != nil && state.status == NodeActive {
			count++
		}
	}
	return count
}

func unixNanoTime(nanos int64) time.Time {
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos).UTC()
}
