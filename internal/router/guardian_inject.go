package router

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"github.com/radryc/monofs/internal/telemetry"
	"github.com/radryc/monofs/pkg/grpcx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type guardianNodeTarget struct {
	id          string
	address     string
	client      pb.MonoFSClient
	kvsStatus   *pb.KVSNodeStatus
	healthy     bool
	failedUntil time.Time
}

const guardianNodeFailureCooldown = 15 * time.Second

// guardianNodeAvailable reports whether the node may be used for a Guardian
// mutation right now. A node is unavailable when the router marked it
// unhealthy or when a recent transport failure put it in cooldown.
func (t guardianNodeTarget) available(now time.Time) bool {
	if !t.healthy {
		return false
	}
	return !t.coolingDown(now)
}

// coolingDown reports whether a recent transport failure quarantined the node.
// KVS nodes are selected from their own KVS health even when the generic probe
// is failing (a slow probe must not hide a healthy leader), so they only need
// to respect the transport-failure cooldown.
func (t guardianNodeTarget) coolingDown(now time.Time) bool {
	return !t.failedUntil.IsZero() && now.Before(t.failedUntil)
}

// InjectGuardianPartition stores inline YAML files for a guardian partition
// directly on all cluster nodes, bypassing the git/S3 ingestion pipeline.
// Requires a valid guardian_token from a registered guardian client.
func (r *Router) InjectGuardianPartition(ctx context.Context, req *pb.InjectGuardianPartitionRequest) (*pb.InjectGuardianPartitionResponse, error) {
	if req.PartitionName == "" {
		return nil, fmt.Errorf("partition_name is required")
	}
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("at least one file is required")
	}

	writes := make([]*pb.GuardianPathWrite, 0, len(req.Files))
	for _, file := range req.Files {
		relPath := cleanGuardianRelativePath(file.GetPath())
		if relPath == "" {
			return nil, fmt.Errorf("guardian file path %q is invalid", file.GetPath())
		}
		writes = append(writes, &pb.GuardianPathWrite{
			LogicalPath:       "/partitions/" + req.PartitionName + "/" + relPath,
			Content:           append([]byte(nil), file.GetContent()...),
			ExpectedVersionId: "",
		})
	}

	upsertResp, err := r.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
		GuardianToken: req.GuardianToken,
		Writes:        writes,
		Context: &pb.GuardianMutationContext{
			Reason:        "inject guardian partition",
			CorrelationId: fmt.Sprintf("inject-%d", time.Now().UnixNano()),
		},
	})
	if err != nil {
		return &pb.InjectGuardianPartitionResponse{
			Success: false,
			Message: err.Error(),
		}, err
	}

	storageID := sharding.GenerateStorageID("guardian/" + req.PartitionName)
	return &pb.InjectGuardianPartitionResponse{
		Success:       upsertResp.GetSuccess(),
		StorageId:     storageID,
		Message:       upsertResp.GetMessage(),
		FilesIngested: int32(len(upsertResp.GetVersions())),
	}, nil
}

func (r *Router) collectGuardianNodes() []guardianNodeTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()

	nodes := make([]guardianNodeTarget, 0, len(r.nodes))
	for id, state := range r.nodes {
		nodes = append(nodes, guardianNodeTarget{
			id:          id,
			address:     state.info.Address,
			client:      state.client,
			kvsStatus:   normalizedKVSNodeStatus(state.kvsStatus),
			healthy:     state.info.Healthy,
			failedUntil: state.guardianFailedUntil,
		})
	}

	return nodes
}

// markGuardianNodeUnhealthy excludes a node from Guardian mutations for a
// cooldown window. The existing gRPC connection is intentionally preserved so
// the transport can reconnect in the background; closing it would discard
// gRPC's backoff state and force a fresh dial on every request.
func (r *Router) markGuardianNodeUnhealthy(nodeID, reason string) {
	if nodeID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.nodes[nodeID]
	if state == nil {
		return
	}
	now := time.Now()
	wasHealthy := state.info.Healthy
	state.info.Healthy = false
	state.kvsStatus = nil
	state.guardianFailedUntil = now.Add(guardianNodeFailureCooldown)
	if wasHealthy {
		r.version.Add(1)
	}
	r.logger.Warn("guardian node marked unhealthy after mutation failure",
		"node_id", nodeID,
		"reason", reason,
		"cooldown", guardianNodeFailureCooldown)
}

// clearGuardianNodeFailure resets the post-failure cooldown after a successful
// health probe or mutation.
func (r *Router) clearGuardianNodeFailure(nodeID string) {
	if nodeID == "" {
		return
	}
	r.mu.Lock()
	if state := r.nodes[nodeID]; state != nil {
		state.guardianFailedUntil = time.Time{}
	}
	r.mu.Unlock()
}

func (r *Router) collectHealthyGuardianNodes() []guardianNodeTarget {
	all := r.collectGuardianNodes()
	healthy := all[:0]
	for _, node := range all {
		if node.healthy {
			healthy = append(healthy, node)
		}
	}
	return healthy
}

// refreshGuardianKVSStatuses re-probes nodes after a retryable leadership
// error. The regular health loop updates KVS status every few seconds, but a
// mutation should not have to wait for the next tick after an election.
func (r *Router) refreshGuardianKVSStatuses(ctx context.Context) {
	type probe struct {
		id     string
		client pb.MonoFSClient
	}

	r.mu.RLock()
	probes := make([]probe, 0, len(r.nodes))
	for id, state := range r.nodes {
		if state.client != nil {
			probes = append(probes, probe{id: id, client: state.client})
		}
	}
	r.mu.RUnlock()

	for _, item := range probes {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		info, err := item.client.GetNodeInfo(probeCtx, &pb.NodeInfoRequest{})
		cancel()
		if err != nil {
			// A failed re-probe means the cached role is stale and must not be
			// used to target a mutation. Only quarantine on transport errors;
			// application errors (e.g. unimplemented probing) leave health
			// untouched.
			if isGuardianTransportError(err) {
				r.markGuardianNodeUnhealthy(item.id, "KVS status probe failed: "+err.Error())
			}
			continue
		}
		r.mu.Lock()
		if state := r.nodes[item.id]; state != nil {
			state.kvsStatus = normalizedKVSNodeStatus(info.GetKvs())
		}
		r.mu.Unlock()
	}
}

func (r *Router) guardianMutationTargets(nodes []guardianNodeTarget, displayPath string) []guardianNodeTarget {
	if len(nodes) == 0 {
		return nil
	}

	if guardianRepoStorageBackend(displayPath) == "kvs" {
		// KVS is a single replicated log. Any healthy KVS-enabled node can
		// accept the write and forward it to the current leader, so do not
		// make correctness depend on a cached leader or on the generic node
		// health probe. Prefer the leader when it is reachable, then fall
		// back to a deterministic peer.
		now := time.Now()
		kvsNodes := make([]guardianNodeTarget, 0, len(nodes))
		for _, node := range nodes {
			if node.coolingDown(now) {
				continue
			}
			status := normalizedKVSNodeStatus(node.kvsStatus)
			if status.GetEnabled() && status.GetHealthy() {
				kvsNodes = append(kvsNodes, node)
			}
		}
		if len(kvsNodes) == 0 {
			// KVS roles may not be known yet (router just started, status was
			// cleared after a failure). Any healthy cluster node can forward
			// to the leader, so fall back to one available node instead of
			// failing the write.
			available := make([]guardianNodeTarget, 0, len(nodes))
			for _, node := range nodes {
				if node.available(now) {
					available = append(available, node)
				}
			}
			if len(available) > 0 {
				sort.Slice(available, func(i, j int) bool {
					if available[i].id != available[j].id {
						return available[i].id < available[j].id
					}
					return available[i].address < available[j].address
				})
				return []guardianNodeTarget{available[0]}
			}
			return nil
		}
		if leader, ok := pickGuardianKVSLeader(kvsNodes); ok {
			return []guardianNodeTarget{leader}
		}
		sort.Slice(kvsNodes, func(i, j int) bool {
			if kvsNodes[i].id != kvsNodes[j].id {
				return kvsNodes[i].id < kvsNodes[j].id
			}
			return kvsNodes[i].address < kvsNodes[j].address
		})
		return []guardianNodeTarget{kvsNodes[0]}
	}

	now := time.Now()
	healthy := make([]guardianNodeTarget, 0, len(nodes))
	for _, node := range nodes {
		if node.available(now) {
			healthy = append(healthy, node)
		}
	}
	if len(healthy) <= 1 {
		return healthy
	}

	rf := r.config.GuardianReplicationFactor
	if rf <= 0 || len(healthy) <= rf {
		return healthy
	}
	shardNodes := make([]sharding.Node, 0, len(healthy))
	byID := make(map[string]guardianNodeTarget, len(healthy))
	for _, n := range healthy {
		byID[n.id] = n
		shardNodes = append(shardNodes, sharding.Node{
			ID:      n.id,
			Address: n.address,
			Weight:  100,
			Healthy: true,
		})
	}
	storageID := sharding.GenerateStorageID(displayPath)
	ranked := sharding.NewHRW(shardNodes).GetNodes(storageID, rf)
	if len(ranked) == 0 {
		return healthy
	}
	out := make([]guardianNodeTarget, 0, len(ranked))
	for _, rn := range ranked {
		if target, ok := byID[rn.ID]; ok {
			out = append(out, target)
		}
	}
	return out
}

func (r *Router) guardianKVSMutationTarget(displayPath string) (guardianNodeTarget, bool) {
	if guardianRepoStorageBackend(displayPath) != "kvs" {
		return guardianNodeTarget{}, false
	}
	targets := r.guardianMutationTargets(r.collectGuardianNodes(), displayPath)
	if len(targets) != 1 {
		return guardianNodeTarget{}, false
	}
	return targets[0], true
}

func (r *Router) guardianDisplayPathByStorageID(storageID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	repo := r.ingestedRepos[storageID]
	if repo == nil {
		return ""
	}
	return repo.repoID
}

func pickGuardianKVSLeader(nodes []guardianNodeTarget) (guardianNodeTarget, bool) {
	leaderID := ""
	for _, node := range nodes {
		status := normalizedKVSNodeStatus(node.kvsStatus)
		if status.GetEnabled() && status.GetHealthy() && strings.EqualFold(status.GetRole(), "leader") {
			return node, true
		}
		if leaderID == "" && status.GetLeaderId() != "" {
			leaderID = status.GetLeaderId()
		}
	}
	if leaderID == "" {
		return guardianNodeTarget{}, false
	}
	for _, node := range nodes {
		if node.id == leaderID {
			return node, true
		}
	}
	return guardianNodeTarget{}, false
}

func (r *Router) guardianNodeClient(target guardianNodeTarget) (pb.MonoFSClient, func(), error) {
	if target.client != nil {
		return target.client, func() {}, nil
	}

	conn, err := grpc.NewClient(target.address, append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcx.IPv4DialerOption(),
		grpc.WithStatsHandler(telemetry.NewGRPCClientStatsHandler()),
	}, r.internalDialOptions()...)...)
	if err != nil {
		return nil, nil, err
	}

	return pb.NewMonoFSClient(conn), func() {
		_ = conn.Close()
	}, nil
}

func cleanGuardianRelativePath(input string) string {
	cleaned := strings.TrimSpace(input)
	if cleaned == "" {
		return ""
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	cleaned = path.Clean(cleaned)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func guardianContentHash(content []byte) string {
	hash := sha256.Sum256(content)
	return fmt.Sprintf("%x", hash)
}
