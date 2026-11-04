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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type guardianNodeTarget struct {
	id        string
	address   string
	client    pb.MonoFSClient
	kvsStatus *pb.KVSNodeStatus
	healthy   bool
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
			id:        id,
			address:   state.info.Address,
			client:    state.client,
			kvsStatus: normalizedKVSNodeStatus(state.kvsStatus),
			healthy:   state.info.Healthy,
		})
	}

	return nodes
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
		kvsNodes := make([]guardianNodeTarget, 0, len(nodes))
		for _, node := range nodes {
			status := normalizedKVSNodeStatus(node.kvsStatus)
			if status.GetEnabled() && status.GetHealthy() {
				kvsNodes = append(kvsNodes, node)
			}
		}
		if len(kvsNodes) == 0 {
			// Single-node/local deployments do not run raft and may report
			// KVS as disabled. Let the sole healthy node handle the write;
			// the local store will return a clear error if it cannot.
			if len(nodes) == 1 && nodes[0].healthy {
				return []guardianNodeTarget{nodes[0]}
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

	healthy := make([]guardianNodeTarget, 0, len(nodes))
	for _, node := range nodes {
		if node.healthy {
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

	conn, err := grpc.NewClient(target.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
