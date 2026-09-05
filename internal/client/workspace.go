package client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// WorkspaceRepository describes a repository visible in the mounted workspace.
type WorkspaceRepository struct {
	StorageID     string
	DisplayPath   string
	Source        string
	Ref           string
	CommitHash    string
	CommitTime    int64
	CommitMessage string
}

// WorkspaceMetadataProvider exposes repository discovery and path resolution
// for clients that can support a synthetic monorepo view.
type WorkspaceMetadataProvider interface {
	ListWorkspaceRepositories(ctx context.Context) ([]WorkspaceRepository, error)
	ResolveWorkspacePath(ctx context.Context, path string) (*WorkspaceRepository, error)
}

// ErrWorkspacePathNotFound indicates that a path does not belong to any known
// repository in the current workspace view.
var ErrWorkspacePathNotFound = errors.New("workspace path not found")

// ListWorkspaceRepositories discovers repositories currently visible through
// the cluster by asking backend nodes for their registered repository metadata.
func (sc *ShardedClient) ListWorkspaceRepositories(ctx context.Context) ([]WorkspaceRepository, error) {
	sc.mu.RLock()
	nodeClients := make(map[string]pb.MonoFSClient, len(sc.clients))
	for nodeID, nodeClient := range sc.clients {
		nodeClients[nodeID] = nodeClient
	}
	callTimeout := sc.rpcTimeout
	sc.mu.RUnlock()

	if len(nodeClients) == 0 {
		return nil, fmt.Errorf("not connected to cluster")
	}
	if callTimeout <= 0 {
		callTimeout = 10 * time.Second
	}

	discovered := make(map[string]WorkspaceRepository)
	var discoveredMu sync.Mutex
	var firstErr error
	var firstErrMu sync.Mutex
	var successfulNodes atomic.Int32
	var wg sync.WaitGroup

	for nodeID, nodeClient := range nodeClients {
		wg.Add(1)
		go func(nodeID string, nodeClient pb.MonoFSClient) {
			defer wg.Done()

			listCtx, listCancel := context.WithTimeout(ctx, callTimeout)
			listResp, err := nodeClient.ListRepositories(listCtx, &pb.ListRepositoriesRequest{})
			listCancel()
			if err != nil {
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("list repositories from %s: %w", nodeID, err)
				}
				firstErrMu.Unlock()
				return
			}

			successfulNodes.Add(1)

			for _, storageID := range listResp.RepositoryIds {
				discoveredMu.Lock()
				_, exists := discovered[storageID]
				discoveredMu.Unlock()
				if exists {
					continue
				}

				infoCtx, infoCancel := context.WithTimeout(ctx, callTimeout)
				infoResp, err := nodeClient.GetRepositoryInfo(infoCtx, &pb.GetRepositoryInfoRequest{
					StorageId: storageID,
				})
				infoCancel()
				if err != nil {
					firstErrMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("get repository info for %s from %s: %w", storageID, nodeID, err)
					}
					firstErrMu.Unlock()
					continue
				}

				discoveredMu.Lock()
				if _, exists := discovered[storageID]; !exists {
					discovered[storageID] = WorkspaceRepository{
						StorageID:     storageID,
						DisplayPath:   infoResp.DisplayPath,
						Source:        infoResp.Source,
						Ref:           infoResp.Ref,
						CommitHash:    infoResp.CommitHash,
						CommitTime:    infoResp.CommitTime,
						CommitMessage: infoResp.CommitMessage,
					}
				}
				discoveredMu.Unlock()
			}
		}(nodeID, nodeClient)
	}

	wg.Wait()

	if len(discovered) == 0 {
		firstErrMu.Lock()
		defer firstErrMu.Unlock()
		if firstErr != nil {
			return nil, firstErr
		}
		if successfulNodes.Load() == 0 {
			return nil, fmt.Errorf("no healthy repository discovery responses")
		}
	}

	repos := make([]WorkspaceRepository, 0, len(discovered))
	for _, repo := range discovered {
		repos = append(repos, repo)
	}

	sort.Slice(repos, func(i, j int) bool {
		if repos[i].DisplayPath == repos[j].DisplayPath {
			return repos[i].StorageID < repos[j].StorageID
		}
		return repos[i].DisplayPath < repos[j].DisplayPath
	})

	return repos, nil
}

// ResolveWorkspacePath resolves a user-visible path to the repository that owns
// it by longest-prefix matching against discovered display paths.
func (sc *ShardedClient) ResolveWorkspacePath(ctx context.Context, path string) (*WorkspaceRepository, error) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil, ErrWorkspacePathNotFound
	}

	repos, err := sc.ListWorkspaceRepositories(ctx)
	if err != nil {
		return nil, err
	}

	var match *WorkspaceRepository
	for i := range repos {
		repo := repos[i]
		if trimmed != repo.DisplayPath && !strings.HasPrefix(trimmed, repo.DisplayPath+"/") {
			continue
		}
		if match == nil || len(repo.DisplayPath) > len(match.DisplayPath) {
			candidate := repo
			match = &candidate
		}
	}

	if match == nil {
		return nil, ErrWorkspacePathNotFound
	}

	return match, nil
}
