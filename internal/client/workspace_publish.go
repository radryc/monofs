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

type WorkspacePublishOptions struct {
	WorkspaceID             string
	LogicalCommitMessage    string
	AuthorName              string
	AuthorEmail             string
	RequestedBranchStrategy string
}

type WorkspacePublishResult struct {
	BundleID              string
	Job                   *pb.WorkspaceSyncJob
	Events                []*pb.WorkspaceSyncEvent
	PublishedRepositories []WorkspaceRepository
	RefreshedRepositories []WorkspaceRepository
	Warning               string
}

func (sc *ShardedClient) PublishWorkspaceBundle(ctx context.Context, bundle *workspacebundle.Bundle, opts WorkspacePublishOptions) (*WorkspacePublishResult, error) {
	normalizedBundle, err := normalizeWorkspaceBundleForPublish(bundle, opts.WorkspaceID, sc.clientID)
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
		return nil, fmt.Errorf("marshal workspace bundle: %w", err)
	}

	uploadCtx, cancelUpload := context.WithTimeout(ctx, 12*rpcTimeout)
	uploadStream, err := routerClient.UploadWorkspaceBundle(uploadCtx)
	if err != nil {
		cancelUpload()
		return nil, fmt.Errorf("open workspace bundle upload stream: %w", err)
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
			return nil, fmt.Errorf("send workspace bundle chunk: %w", err)
		}
	}
	uploadResp, err := uploadStream.CloseAndRecv()
	cancelUpload()
	if err != nil {
		return nil, fmt.Errorf("close workspace bundle upload: %w", err)
	}

	publishCtx, cancelPublish := context.WithTimeout(ctx, 30*rpcTimeout)
	defer cancelPublish()

	publishStream, err := routerClient.PublishWorkspace(publishCtx, &pb.PublishWorkspaceRequest{
		WorkspaceId:             normalizedBundle.WorkspaceID,
		BundleId:                uploadResp.GetBundleId(),
		LogicalCommitMessage:    opts.LogicalCommitMessage,
		AuthorName:              opts.AuthorName,
		AuthorEmail:             opts.AuthorEmail,
		RequestedBranchStrategy: opts.RequestedBranchStrategy,
	})
	if err != nil {
		return nil, fmt.Errorf("start workspace publish: %w", err)
	}

	result := &WorkspacePublishResult{BundleID: uploadResp.GetBundleId()}
	for {
		event, err := publishStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("receive workspace publish event: %w", err)
		}
		result.Events = append(result.Events, event)
		if event.GetJob() != nil {
			result.Job = event.GetJob()
		}
	}

	if result.Job == nil {
		return result, fmt.Errorf("workspace publish finished without a job summary")
	}

	result.PublishedRepositories = publishedWorkspaceRepositoriesFromJob(result.Job, normalizedBundle)
	if result.Job.GetState() != pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED {
		return result, workspacePublishJobError(result.Job)
	}

	if opts.RequestedBranchStrategy == "" || opts.RequestedBranchStrategy == "direct" {
		if len(result.PublishedRepositories) > 0 {
			refreshCtx, cancelRefresh := context.WithTimeout(ctx, 12*rpcTimeout)
			_, refreshErr := sc.RefreshWorkspaceRepositories(refreshCtx, result.PublishedRepositories)
			cancelRefresh()
			if refreshErr != nil {
				result.Warning = fmt.Sprintf("published upstream, but workspace refresh failed: %v; run monofs-session pull", refreshErr)
				return result, nil
			}
			result.RefreshedRepositories = append(result.RefreshedRepositories, result.PublishedRepositories...)
		}
	}

	return result, nil
}

func normalizeWorkspaceBundleForPublish(bundle *workspacebundle.Bundle, requestedWorkspaceID, clientID string) (*workspacebundle.Bundle, error) {
	if bundle == nil {
		return nil, fmt.Errorf("workspace bundle is required")
	}

	normalized := &workspacebundle.Bundle{
		WorkspaceID:  strings.TrimSpace(bundle.WorkspaceID),
		Repositories: append([]workspacebundle.RepositoryBundle(nil), bundle.Repositories...),
	}
	if normalized.WorkspaceID == "" {
		normalized.WorkspaceID = strings.TrimSpace(requestedWorkspaceID)
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

func publishedWorkspaceRepositoriesFromJob(job *pb.WorkspaceSyncJob, bundle *workspacebundle.Bundle) []WorkspaceRepository {
	if job == nil || bundle == nil {
		return nil
	}

	seen := make(map[string]struct{}, len(job.GetRepositories()))
	repos := make([]WorkspaceRepository, 0, len(job.GetRepositories()))
	for _, repoResult := range job.GetRepositories() {
		if repoResult.GetStatus() != pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED {
			continue
		}
		key := repoResult.GetStorageId()
		if key == "" {
			key = repoResult.GetDisplayPath()
		}
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		bundleRepo := bundle.RepositoryByStorageID(repoResult.GetStorageId())
		repo := WorkspaceRepository{
			StorageID:   repoResult.GetStorageId(),
			DisplayPath: repoResult.GetDisplayPath(),
			CommitHash:  repoResult.GetPushedCommit(),
		}
		if bundleRepo != nil {
			repo.Source = bundleRepo.RepoURL
			repo.Ref = bundleRepo.Branch
		}
		if repo.Source == "" {
			repo.Source = repoResult.GetRepoUrl()
		}
		if repo.Ref == "" {
			repo.Ref = repoResult.GetBranch()
		}
		repos = append(repos, repo)
	}
	return repos
}

func workspacePublishJobError(job *pb.WorkspaceSyncJob) error {
	if job == nil {
		return fmt.Errorf("workspace publish failed")
	}
	if message := strings.TrimSpace(job.GetErrorMessage()); message != "" {
		return fmt.Errorf("workspace publish failed: %s", message)
	}

	issues := make([]string, 0, len(job.GetRepositories()))
	for _, repo := range job.GetRepositories() {
		switch repo.GetStatus() {
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT,
			pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED,
			pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CANCELLED:
			message := repo.GetDisplayPath()
			if detail := strings.TrimSpace(repo.GetMessage()); detail != "" {
				message += ": " + detail
			}
			issues = append(issues, message)
		}
	}
	if len(issues) > 0 {
		if len(issues) > 3 {
			issues = append(issues[:3], fmt.Sprintf("and %d more", len(issues)-3))
		}
		return fmt.Errorf("workspace publish failed: %s", strings.Join(issues, "; "))
	}

	return fmt.Errorf("workspace publish ended in state %s", strings.ToLower(job.GetState().String()))
}
