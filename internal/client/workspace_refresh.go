package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

type WorkspaceRefreshResult struct {
	Requested int
	Refreshed int
	Failed    int
	Results   []WorkspaceRepositoryRefresh
}

type WorkspaceRepositoryRefresh struct {
	StorageID   string
	DisplayPath string
	Refreshed   bool
	Message     string
	Error       string
}

func refreshRequestForRepository(repo WorkspaceRepository) (*pb.WorkspaceRepositoryRef, error) {
	if strings.TrimSpace(repo.DisplayPath) == "" {
		return nil, fmt.Errorf("repository display path is required")
	}
	if strings.TrimSpace(repo.Source) == "" {
		return nil, fmt.Errorf("repository %q has no source configured", repo.DisplayPath)
	}
	if strings.TrimSpace(repo.Ref) == "" {
		return nil, fmt.Errorf("repository %q has no branch configured", repo.DisplayPath)
	}
	if strings.TrimSpace(repo.CommitHash) == "" {
		return nil, fmt.Errorf("repository %q has no base commit configured", repo.DisplayPath)
	}

	return &pb.WorkspaceRepositoryRef{
		StorageId:   repo.StorageID,
		DisplayPath: repo.DisplayPath,
		RepoUrl:     repo.Source,
		Branch:      repo.Ref,
		BaseCommit:  repo.CommitHash,
	}, nil
}

func (sc *ShardedClient) RefreshWorkspaceRepositories(ctx context.Context, repos []WorkspaceRepository) (*WorkspaceRefreshResult, error) {
	result := &WorkspaceRefreshResult{}
	if len(repos) == 0 {
		return result, nil
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

	requested := dedupeWorkspaceReposForRefresh(repos)
	result.Requested = len(requested)

	refreshRefs := make([]*pb.WorkspaceRepositoryRef, 0, len(requested))
	var failures []string
	for _, repo := range requested {
		request, err := refreshRequestForRepository(repo)
		if err != nil {
			result.Failed++
			result.Results = append(result.Results, WorkspaceRepositoryRefresh{
				StorageID:   repo.StorageID,
				DisplayPath: repo.DisplayPath,
				Error:       err.Error(),
			})
			failures = append(failures, fmt.Sprintf("%s: %v", repo.DisplayPath, err))
			continue
		}
		refreshRefs = append(refreshRefs, request)
	}

	if len(refreshRefs) > 0 {
		callCtx, cancel := context.WithTimeout(ctx, 30*rpcTimeout)
		stream, err := routerClient.RefreshWorkspace(callCtx, &pb.RefreshWorkspaceRequest{
			WorkspaceId:  sc.clientID,
			Repositories: refreshRefs,
		})
		if err != nil {
			cancel()
			return result, fmt.Errorf("start workspace refresh: %w", err)
		}

		job, err := consumeWorkspaceRefreshStream(stream)
		cancel()
		if job != nil {
			appendWorkspaceRefreshResults(result, job)
		}
		if err != nil {
			failures = append(failures, err.Error())
		}
	}

	refreshCtx, cancel := context.WithTimeout(ctx, 3*rpcTimeout)
	defer cancel()
	if err := sc.refreshClusterInfo(refreshCtx); err != nil && sc.logger != nil {
		sc.logger.Warn("workspace refresh completed but cluster refresh failed", "error", err)
	}

	if len(failures) > 0 {
		return result, fmt.Errorf("workspace refresh failed: %s", strings.Join(failures, "; "))
	}
	return result, nil
}

func dedupeWorkspaceReposForRefresh(repos []WorkspaceRepository) []WorkspaceRepository {
	seen := make(map[string]struct{}, len(repos))
	unique := make([]WorkspaceRepository, 0, len(repos))
	for _, repo := range repos {
		key := repo.StorageID
		if key == "" {
			key = repo.DisplayPath
		}
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, repo)
	}
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].DisplayPath < unique[j].DisplayPath
	})
	return unique
}

func consumeWorkspaceRefreshStream(stream pb.MonoFSRouter_RefreshWorkspaceClient) (*pb.WorkspaceSyncJob, error) {
	var lastJob *pb.WorkspaceSyncJob
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			if lastJob == nil {
				return nil, errors.New("workspace refresh stream ended before completion")
			}
			if lastJob.GetState() != pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED {
				return lastJob, workspaceRefreshJobError(lastJob)
			}
			return lastJob, nil
		}
		if err != nil {
			return lastJob, err
		}
		if event.GetJob() != nil {
			lastJob = event.GetJob()
		}
	}
}

func appendWorkspaceRefreshResults(result *WorkspaceRefreshResult, job *pb.WorkspaceSyncJob) {
	if result == nil || job == nil {
		return
	}
	if summary := job.GetSummary(); summary != nil {
		result.Refreshed += int(summary.GetRepositoriesSucceeded())
		result.Failed += int(summary.GetRepositoriesFailed() + summary.GetRepositoriesConflicted())
	}
	for _, repo := range job.GetRepositories() {
		entry := WorkspaceRepositoryRefresh{
			StorageID:   repo.GetStorageId(),
			DisplayPath: repo.GetDisplayPath(),
			Message:     repo.GetMessage(),
		}
		switch repo.GetStatus() {
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED,
			pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT,
			pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CANCELLED:
			entry.Error = repo.GetMessage()
		default:
			entry.Refreshed = true
		}
		result.Results = append(result.Results, entry)
	}
}

func workspaceRefreshJobError(job *pb.WorkspaceSyncJob) error {
	if job == nil {
		return fmt.Errorf("workspace refresh failed")
	}
	if message := strings.TrimSpace(job.GetErrorMessage()); message != "" {
		return fmt.Errorf("workspace refresh failed: %s", message)
	}
	issues := make([]string, 0, len(job.GetRepositories()))
	for _, repo := range job.GetRepositories() {
		switch repo.GetStatus() {
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED,
			pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT,
			pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CANCELLED:
			message := repo.GetDisplayPath()
			if detail := strings.TrimSpace(repo.GetMessage()); detail != "" {
				message += ": " + detail
			}
			issues = append(issues, message)
		}
	}
	if len(issues) > 0 {
		return fmt.Errorf("workspace refresh failed: %s", strings.Join(issues, "; "))
	}
	return fmt.Errorf("workspace refresh ended in state %s", strings.ToLower(job.GetState().String()))
}
