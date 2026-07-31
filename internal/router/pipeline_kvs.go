package router

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

func (r *Router) writePipelinePath(logicalPath string, content []byte, expectedVersionID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := &pb.UpsertGuardianPathsRequest{
		GuardianToken: pipelinePrincipalToken,
		Writes: []*pb.GuardianPathWrite{
			{
				LogicalPath:       logicalPath,
				Content:           content,
				ExpectedVersionId: expectedVersionID,
			},
		},
	}
	resp, err := r.processGuardianUpsert(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.GetVersions()) > 0 {
		return resp.GetVersions()[0].GetVersionId(), nil
	}
	return "", nil
}

func (r *Router) readPipelinePath(logicalPath string) ([]byte, string, error) {
	mapped, err := mapGuardianLogicalPath(logicalPath)
	if err != nil {
		return nil, "", fmt.Errorf("map path: %w", err)
	}

	current, exists := r.guardianVersions.currentVersion(logicalPath)
	if !exists || current.Tombstone {
		return nil, "", fmt.Errorf("not found: %s", logicalPath)
	}

	if current.Content != nil {
		return current.Content, current.VersionID, nil
	}

	nodeClient, closeConn, err := r.guardianNodeClientForPath(mapped)
	if err != nil {
		return nil, "", fmt.Errorf("node client: %w", err)
	}
	if closeConn != nil {
		defer closeConn()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	content, err := readGuardianFileContent(ctx, nodeClient, mapped.RelativePath)
	if err != nil {
		return nil, "", fmt.Errorf("read file: %w", err)
	}
	return content, current.VersionID, nil
}

func (r *Router) deletePipelinePath(logicalPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := &pb.DeleteGuardianPathsRequest{
		GuardianToken: pipelinePrincipalToken,
		Deletes: []*pb.GuardianPathDelete{
			{
				LogicalPath:       logicalPath,
				ExpectedVersionId: "",
			},
		},
	}
	_, err := r.processGuardianDelete(ctx, req)
	return err
}

func (r *Router) listPipelinePath(logicalDir string) ([]string, error) {
	logicalDir = strings.TrimSuffix(logicalDir, "/")
	if !strings.HasPrefix(logicalDir, "/") {
		logicalDir = "/" + logicalDir
	}

	prefix := logicalDir + "/"
	var entries []string

	versions, _, err := r.guardianVersions.list(prefix, 1000, "")
	if err != nil {
		return nil, err
	}
	for _, version := range versions {
		if version == nil || version.GetTombstone() {
			continue
		}
		relName := strings.TrimPrefix(version.GetLogicalPath(), prefix)
		if relName == "" {
			continue
		}
		entries = append(entries, relName)
	}
	return entries, nil
}

func (r *Router) guardianNodeClientForPath(mapped guardianPhysicalPath) (pb.MonoFSClient, func(), error) {
	nodes := r.collectHealthyGuardianNodes()
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("no healthy guardian nodes available")
	}
	return r.guardianNodeClient(nodes[0])
}

func (r *Router) processGuardianDelete(ctx context.Context, req *pb.DeleteGuardianPathsRequest) (*pb.DeleteGuardianPathsResponse, error) {
	principal, ok := r.authenticateGuardianMutation(req.GetGuardianToken(), req.GetContext())
	if !ok {
		return nil, fmt.Errorf("unauthorized")
	}
	if len(req.GetDeletes()) == 0 {
		return nil, fmt.Errorf("at least one delete required")
	}

	now := time.Now()
	committedAt := now.Unix()
	batchRevisionID := guardianBatchRevisionID(now)

	nodes := r.collectHealthyGuardianNodes()
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no healthy guardian nodes available")
	}

	deleted := make([]*pb.GuardianFileVersion, 0, len(req.GetDeletes()))
	for _, del := range req.GetDeletes() {
		mapped, err := mapGuardianLogicalPath(del.GetLogicalPath())
		if err != nil {
			return nil, fmt.Errorf("invalid path %q: %w", del.GetLogicalPath(), err)
		}

		current, exists := r.guardianVersions.currentVersion(mapped.LogicalPath)
		if !exists || current.Tombstone {
			continue
		}

		nodeClient, closeConn, err := r.guardianNodeClient(nodes[0])
		if err != nil {
			return nil, fmt.Errorf("node client: %w", err)
		}

		_, err = nodeClient.DeleteFile(ctx, &pb.DeleteFileRequest{
			StorageId: mapped.StorageID,
			FilePath:  mapped.RelativePath,
		})
		closeConn()
		if err != nil {
			return nil, fmt.Errorf("delete file: %w", err)
		}

		version, err := r.guardianVersions.commit(guardianVersionCommit{
			LogicalPath:     mapped.LogicalPath,
			DisplayPath:     mapped.DisplayPath,
			StorageID:       mapped.StorageID,
			BatchRevisionID: batchRevisionID,
			PrincipalID:     principal.PrincipalID,
			CommittedAt:     committedAt,
			Tombstone:       true,
		})
		if err != nil {
			return nil, fmt.Errorf("record version: %w", err)
		}
		deleted = append(deleted, version)

		event := buildGuardianChangeEvent(version, mapped.LogicalPath, pb.ChangeType_DELETED, "", nil)
		r.publishGuardianLogicalChange(event)
		r.publishLegacyGuardianChange(event)
	}

	return &pb.DeleteGuardianPathsResponse{
		Success: true,
		Message: fmt.Sprintf("deleted %d path(s)", len(deleted)),
	}, nil
}
