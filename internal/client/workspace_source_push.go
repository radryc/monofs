package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
)

type WorkspaceSourcePushResult struct {
	BundleID string
	Job      *pb.WorkspaceSyncJob
	Events   []*pb.WorkspaceSyncEvent
}

func (sc *ShardedClient) PushWorkspaceCommitBundle(ctx context.Context, bundle *workspacebundle.SourceCommitBundle) (*WorkspaceSourcePushResult, error) {
	normalizedBundle, err := normalizeSourceCommitBundleForPush(bundle, sc.clientID)
	if err != nil {
		return nil, err
	}

	sc.mu.RLock()
	routerClient := sc.routerClient
	rpcTimeout := sc.rpcTimeout
	sc.mu.RUnlock()

	if routerClient == nil {
		return nil, fmt.Errorf("no router connection")
	}
	if rpcTimeout <= 0 {
		rpcTimeout = 10 * time.Second
	}

	bundleBytes, err := json.Marshal(normalizedBundle)
	if err != nil {
		return nil, fmt.Errorf("marshal source commit bundle: %w", err)
	}

	uploadCtx, cancelUpload := context.WithTimeout(ctx, 12*rpcTimeout)
	uploadStream, err := routerClient.UploadWorkspaceCommitBundle(uploadCtx)
	if err != nil {
		cancelUpload()
		return nil, fmt.Errorf("open source commit bundle upload stream: %w", err)
	}

	const chunkSize = 1024 * 1024
	for offset := 0; offset < len(bundleBytes); offset += chunkSize {
		end := offset + chunkSize
		if end > len(bundleBytes) {
			end = len(bundleBytes)
		}
		if err := uploadStream.Send(&pb.WorkspaceBundleChunk{
			WorkspaceId: normalizedBundle.WorkspaceID,
			Data:        bundleBytes[offset:end],
			IsLast:      end >= len(bundleBytes),
		}); err != nil {
			cancelUpload()
			return nil, fmt.Errorf("send source commit bundle chunk: %w", err)
		}
	}
	uploadResp, err := uploadStream.CloseAndRecv()
	cancelUpload()
	if err != nil {
		return nil, fmt.Errorf("close source commit bundle upload: %w", err)
	}

	pushCtx, cancelPush := context.WithTimeout(ctx, 30*rpcTimeout)
	defer cancelPush()

	pushStream, err := routerClient.PushWorkspaceCommits(pushCtx, &pb.PushWorkspaceCommitsRequest{
		WorkspaceId:   normalizedBundle.WorkspaceID,
		BundleId:      uploadResp.GetBundleId(),
		LogicalBranch: normalizedBundle.LogicalBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("start workspace source push: %w", err)
	}

	result := &WorkspaceSourcePushResult{BundleID: uploadResp.GetBundleId()}
	for {
		event, err := pushStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("receive workspace source push event: %w", err)
		}
		result.Events = append(result.Events, event)
		if event.GetJob() != nil {
			result.Job = event.GetJob()
		}
	}

	if result.Job == nil {
		return result, fmt.Errorf("workspace source push finished without a job summary")
	}
	if result.Job.GetState() != pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED {
		return result, workspaceSourcePushJobError(result.Job)
	}
	return result, nil
}

func normalizeSourceCommitBundleForPush(bundle *workspacebundle.SourceCommitBundle, clientID string) (*workspacebundle.SourceCommitBundle, error) {
	if bundle == nil {
		return nil, fmt.Errorf("source commit bundle is required")
	}

	normalized := &workspacebundle.SourceCommitBundle{
		WorkspaceID:   strings.TrimSpace(bundle.WorkspaceID),
		PrincipalID:   strings.TrimSpace(bundle.PrincipalID),
		LogicalBranch: strings.TrimSpace(bundle.LogicalBranch),
		Commits:       append([]workspacebundle.SourceCommit(nil), bundle.Commits...),
	}
	if normalized.WorkspaceID == "" {
		normalized.WorkspaceID = strings.TrimSpace(clientID)
	}
	if normalized.WorkspaceID == "" {
		normalized.WorkspaceID = fmt.Sprintf("workspace-%d", time.Now().UnixNano())
	}
	if err := normalized.Validate(); err != nil {
		return nil, err
	}
	return normalized, nil
}

func workspaceSourcePushJobError(job *pb.WorkspaceSyncJob) error {
	if job == nil {
		return fmt.Errorf("workspace source push failed")
	}
	if message := strings.TrimSpace(job.GetErrorMessage()); message != "" {
		return fmt.Errorf("workspace source push failed: %s", message)
	}

	issues := make([]string, 0, len(job.GetRepositories()))
	for _, repo := range job.GetRepositories() {
		switch repo.GetStatus() {
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT,
			pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED,
			pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CANCELLED:
			message := repo.GetDisplayPath()
			if repo.GetMessage() != "" {
				message = fmt.Sprintf("%s: %s", repo.GetDisplayPath(), repo.GetMessage())
			}
			issues = append(issues, message)
		}
	}
	if len(issues) == 0 {
		return fmt.Errorf("workspace source push failed")
	}
	return fmt.Errorf("workspace source push failed: %s", strings.Join(issues, "; "))
}
