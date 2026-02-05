package router

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	pb "github.com/radryc/monofs/api/proto"
)

// DeleteRepository removes a repository from all nodes and memory.
// This is async and best-effort - failed nodes don't prevent cleanup continuation.
func (r *Router) DeleteRepository(ctx context.Context, req *pb.DeleteRepositoryRequest) (*pb.DeleteRepositoryResponse, error) {
	storageID := req.StorageId

	r.logger.Info("deleting repository", "storage_id", storageID)

	// Step 1: Remove from router's ingested repos (memory)
	r.mu.Lock()
	repo, exists := r.ingestedRepos[storageID]
	if !exists {
		r.mu.Unlock()
		return &pb.DeleteRepositoryResponse{
			Success:      false,
			Message:      "repository not found in router",
			FilesDeleted: 0,
		}, fmt.Errorf("repository not found: %s", storageID)
	}
	delete(r.ingestedRepos, storageID)
	r.mu.Unlock()

	r.logger.Info("removed repository from router memory", "storage_id", storageID)

	// Step 2: Async delete from all backend nodes (fire and forget)
	var filesDeleted int64
	go r.deleteRepositoryFromNodes(storageID, &filesDeleted)

	return &pb.DeleteRepositoryResponse{
		Success:      true,
		Message:      fmt.Sprintf("repository deletion started for %s (had %d files)", storageID, repo.filesCount),
		FilesDeleted: int64(repo.filesCount),
	}, nil
}

// deleteRepositoryFromNodes asynchronously deletes repository files from all backend nodes.
func (r *Router) deleteRepositoryFromNodes(storageID string, filesDeletedPtr *int64) {
	r.mu.RLock()
	nodesSnapshot := make(map[string]*nodeState)
	for nodeID, state := range r.nodes {
		nodesSnapshot[nodeID] = state
	}
	r.mu.RUnlock()

	var wg sync.WaitGroup
	var totalDeleted int64

	for nodeID, state := range nodesSnapshot {
		wg.Add(1)
		go func(nID string, s *nodeState) {
			defer wg.Done()

			if s.client == nil {
				r.logger.Warn("node client not available for deletion", "node_id", nID, "storage_id", storageID)
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), deleteTimeoutSeconds)
			defer cancel()

			// Get list of repository files from node
			resp, err := s.client.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
				StorageId: storageID,
			})
			if err != nil {
				r.logger.Warn("failed to get repository files from node for deletion",
					"node_id", nID,
					"storage_id", storageID,
					"error", err)
				return
			}

			// Delete each file
			deleted := 0
			for _, filePath := range resp.Files {
				delCtx, delCancel := context.WithTimeout(context.Background(), deleteFileTimeoutSeconds)
				_, delErr := s.client.DeleteFile(delCtx, &pb.DeleteFileRequest{
					StorageId: storageID,
					FilePath:  filePath,
				})
				delCancel()

				if delErr != nil {
					r.logger.Warn("failed to delete file from node",
						"node_id", nID,
						"storage_id", storageID,
						"file_path", filePath,
						"error", delErr)
					continue
				}
				deleted++
			}

			if deleted > 0 {
				atomic.AddInt64(&totalDeleted, int64(deleted))
				r.logger.Info("deleted repository files from node",
					"node_id", nID,
					"storage_id", storageID,
					"files_deleted", deleted)
			}
		}(nodeID, state)
	}

	wg.Wait()

	*filesDeletedPtr = atomic.LoadInt64(&totalDeleted)
	r.logger.Info("repository deletion complete", "storage_id", storageID, "total_files_deleted", *filesDeletedPtr)
}

const (
	deleteTimeoutSeconds     = 60
	deleteFileTimeoutSeconds = 5
)
