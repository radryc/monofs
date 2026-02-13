package router

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// DeleteRepository removes a repository from all nodes, search index, and router memory.
// Performs synchronous cleanup of ALL references across the cluster.
func (r *Router) DeleteRepository(ctx context.Context, req *pb.DeleteRepositoryRequest) (*pb.DeleteRepositoryResponse, error) {
	storageID := req.StorageId

	r.logger.Info("deleting repository", "storage_id", storageID)

	// Step 1: Remove from router's ingested repos (memory)
	r.mu.Lock()
	repo, exists := r.ingestedRepos[storageID]
	var filesCount int64
	if exists {
		filesCount = repo.filesCount
		delete(r.ingestedRepos, storageID)
	}
	r.mu.Unlock()

	if exists {
		r.logger.Info("removed repository from router memory", "storage_id", storageID, "files_count", filesCount)
	} else {
		r.logger.Warn("repository not found in router memory, proceeding with node cleanup", "storage_id", storageID)
	}

	// Also clean up partial repo tracking
	r.partialReposMu.Lock()
	delete(r.partialRepos, storageID)
	r.partialReposMu.Unlock()

	// Step 2: Delete from all backend nodes (synchronous, parallel)
	totalFilesDeleted, totalDirsDeleted, nodeErrors := r.deleteRepositoryFromAllNodes(ctx, storageID)

	// Step 3: Delete search index
	r.deleteSearchIndex(ctx, storageID)

	message := fmt.Sprintf("repository deleted: %d files, %d dirs removed from nodes", totalFilesDeleted, totalDirsDeleted)
	if nodeErrors > 0 {
		message += fmt.Sprintf(" (%d node errors)", nodeErrors)
	}

	r.logger.Info("repository deletion complete",
		"storage_id", storageID,
		"files_deleted", totalFilesDeleted,
		"dirs_deleted", totalDirsDeleted,
		"node_errors", nodeErrors)

	return &pb.DeleteRepositoryResponse{
		Success:      true,
		Message:      message,
		FilesDeleted: totalFilesDeleted,
	}, nil
}

// deleteRepositoryFromAllNodes calls the node-level DeleteRepository RPC on every node.
// Returns total files deleted, total dirs deleted, and number of node errors.
func (r *Router) deleteRepositoryFromAllNodes(ctx context.Context, storageID string) (int64, int64, int) {
	r.mu.RLock()
	nodesSnapshot := make(map[string]*nodeState)
	for nodeID, state := range r.nodes {
		nodesSnapshot[nodeID] = state
	}
	r.mu.RUnlock()

	var wg sync.WaitGroup
	var totalFilesDeleted int64
	var totalDirsDeleted int64
	var nodeErrors int64

	for nodeID, state := range nodesSnapshot {
		wg.Add(1)
		go func(nID string, s *nodeState) {
			defer wg.Done()

			if s.client == nil {
				r.logger.Warn("node client not available for deletion", "node_id", nID, "storage_id", storageID)
				atomic.AddInt64(&nodeErrors, 1)
				return
			}

			// Use parent context with timeout
			deleteCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()

			resp, err := s.client.DeleteRepository(deleteCtx, &pb.DeleteRepositoryOnNodeRequest{
				StorageId: storageID,
			})
			if err != nil {
				r.logger.Warn("failed to delete repository from node",
					"node_id", nID,
					"storage_id", storageID,
					"error", err)
				atomic.AddInt64(&nodeErrors, 1)
				return
			}

			if resp.Success {
				atomic.AddInt64(&totalFilesDeleted, resp.FilesDeleted)
				atomic.AddInt64(&totalDirsDeleted, resp.DirsDeleted)
				r.logger.Info("deleted repository from node",
					"node_id", nID,
					"storage_id", storageID,
					"files_deleted", resp.FilesDeleted,
					"dirs_deleted", resp.DirsDeleted)
			} else {
				r.logger.Warn("node reported deletion failure",
					"node_id", nID,
					"storage_id", storageID,
					"message", resp.Message)
				atomic.AddInt64(&nodeErrors, 1)
			}
		}(nodeID, state)
	}

	wg.Wait()
	return totalFilesDeleted, totalDirsDeleted, int(nodeErrors)
}

// deleteRepositoryFromNodes is a compatibility wrapper used by cleanupStalePartialRepos.
func (r *Router) deleteRepositoryFromNodes(storageID string, filesDeletedPtr *int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	totalFiles, _, _ := r.deleteRepositoryFromAllNodes(ctx, storageID)
	*filesDeletedPtr = totalFiles
}

// deleteSearchIndex removes the search index for the repository.
func (r *Router) deleteSearchIndex(ctx context.Context, storageID string) {
	if r.searchClient == nil {
		return
	}

	deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := r.searchClient.DeleteIndex(deleteCtx, &pb.DeleteIndexRequest{
		StorageId: storageID,
	})
	if err != nil {
		r.logger.Warn("failed to delete search index", "storage_id", storageID, "error", err)
		return
	}

	r.logger.Info("deleted search index", "storage_id", storageID, "success", resp.Success)
}
