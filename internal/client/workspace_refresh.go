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

func refreshRequestForRepository(repo WorkspaceRepository) (*pb.IngestRequest, error) {
	if strings.TrimSpace(repo.DisplayPath) == "" {
		return nil, fmt.Errorf("repository display path is required")
	}
	if strings.TrimSpace(repo.Source) == "" {
		return nil, fmt.Errorf("repository %q has no source configured", repo.DisplayPath)
	}

	return &pb.IngestRequest{
		Source:        repo.Source,
		Ref:           repo.Ref,
		SourceId:      repo.DisplayPath,
		IngestionType: pb.IngestionType_INGESTION_GIT,
		FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
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

		callCtx, cancel := context.WithTimeout(ctx, 12*rpcTimeout)
		stream, err := routerClient.IngestRepository(callCtx, request)
		if err != nil {
			cancel()
			result.Failed++
			result.Results = append(result.Results, WorkspaceRepositoryRefresh{
				StorageID:   repo.StorageID,
				DisplayPath: repo.DisplayPath,
				Error:       err.Error(),
			})
			failures = append(failures, fmt.Sprintf("%s: %v", repo.DisplayPath, err))
			continue
		}

		refreshResult, err := consumeWorkspaceRefreshStream(stream)
		cancel()
		refreshResult.StorageID = repo.StorageID
		refreshResult.DisplayPath = repo.DisplayPath
		result.Results = append(result.Results, refreshResult)
		if err != nil {
			result.Failed++
			failures = append(failures, fmt.Sprintf("%s: %v", repo.DisplayPath, err))
			continue
		}
		result.Refreshed++
	}

	refreshCtx, cancel := context.WithTimeout(ctx, 3*rpcTimeout)
	defer cancel()
	if err := sc.refreshClusterInfo(refreshCtx); err != nil && sc.logger != nil {
		sc.logger.Warn("workspace refresh completed but cluster refresh failed", "error", err)
	}

	if len(failures) > 0 {
		return result, fmt.Errorf("workspace refresh failed for %d repositories: %s", len(failures), strings.Join(failures, "; "))
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

func consumeWorkspaceRefreshStream(stream pb.MonoFSRouter_IngestRepositoryClient) (WorkspaceRepositoryRefresh, error) {
	result := WorkspaceRepositoryRefresh{}
	for {
		progress, err := stream.Recv()
		if err == io.EOF {
			if result.Refreshed {
				return result, nil
			}
			if result.Error == "" {
				result.Error = "ingestion stream ended before completion"
			}
			return result, errors.New(result.Error)
		}
		if err != nil {
			if result.Error == "" {
				result.Error = err.Error()
			}
			return result, err
		}

		message := strings.TrimSpace(progress.GetMessage())
		if message != "" {
			result.Message = message
		}
		switch progress.GetStage() {
		case pb.IngestProgress_COMPLETED:
			if !progress.GetSuccess() {
				if result.Message == "" {
					result.Message = "workspace refresh failed"
				}
				result.Error = result.Message
				return result, errors.New(result.Message)
			}
			result.Refreshed = true
			return result, nil
		case pb.IngestProgress_FAILED:
			if result.Message == "" {
				result.Message = "workspace refresh failed"
			}
			result.Error = result.Message
			return result, errors.New(result.Message)
		}
	}
}
