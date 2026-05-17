package router

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path"
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
			PrincipalId:   req.PartitionName,
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

func (r *Router) collectHealthyGuardianNodes() []guardianNodeTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()

	nodes := make([]guardianNodeTarget, 0, len(r.nodes))
	for id, state := range r.nodes {
		if state.info.Healthy {
			nodes = append(nodes, guardianNodeTarget{
				id:        id,
				address:   state.info.Address,
				client:    state.client,
				kvsStatus: normalizedKVSNodeStatus(state.kvsStatus),
			})
		}
	}

	return nodes
}

func (r *Router) guardianMutationTargets(nodes []guardianNodeTarget, displayPath string) []guardianNodeTarget {
	if len(nodes) <= 1 || guardianRepoStorageBackend(displayPath) != "kvs" {
		return nodes
	}

	if leader, ok := pickGuardianKVSLeader(nodes); ok {
		return []guardianNodeTarget{leader}
	}

	selected := nodes[0]
	for _, node := range nodes[1:] {
		if node.id < selected.id || (node.id == selected.id && node.address < selected.address) {
			selected = node
		}
	}
	return []guardianNodeTarget{selected}
}

func (r *Router) guardianKVSMutationTarget(displayPath string) (guardianNodeTarget, bool) {
	if guardianRepoStorageBackend(displayPath) != "kvs" {
		return guardianNodeTarget{}, false
	}
	targets := r.guardianMutationTargets(r.collectHealthyGuardianNodes(), displayPath)
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
		if status.GetEnabled() && strings.EqualFold(status.GetRole(), "leader") {
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

func (r *Router) lookupGuardianExistingFiles(ctx context.Context, nodes []guardianNodeTarget, displayPath string, files []*pb.InjectGuardianFile) map[string]bool {
	existing := make(map[string]bool, len(files))
	if len(nodes) == 0 || len(files) == 0 {
		return existing
	}

	nodeClient, closeConn, err := r.guardianNodeClient(nodes[0])
	if err != nil {
		r.logger.Warn("failed to initialize guardian lookup client", "node", nodes[0].id, "error", err)
		return existing
	}
	defer closeConn()

	for _, file := range files {
		relPath := cleanGuardianRelativePath(file.Path)
		if relPath == "" {
			continue
		}

		attrCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := nodeClient.GetAttr(attrCtx, &pb.GetAttrRequest{
			Path: displayPath + "/" + relPath,
		})
		cancel()
		if err == nil && resp != nil && resp.Found {
			existing[relPath] = true
		}
	}

	return existing
}

func (r *Router) publishGuardianInjectedFiles(storageID string, files []*pb.InjectGuardianFile, existing map[string]bool) {
	for _, file := range files {
		relPath := cleanGuardianRelativePath(file.Path)
		if relPath == "" {
			continue
		}

		changeType := pb.ChangeType_ADDED
		if existing[relPath] {
			changeType = pb.ChangeType_MODIFIED
		}

		event := &pb.ChangeEvent{
			StorageId:   storageID,
			FilePath:    relPath,
			Type:        changeType,
			NewBlobHash: guardianContentHash(file.Content),
		}
		if len(file.Content) < 64*1024 {
			event.InlineContent = append([]byte(nil), file.Content...)
		}

		r.publishGuardianChange(event)
	}
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
