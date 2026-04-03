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

	// Step 2: Delete from all backend nodes (synchronous, parallel)
	totalFilesDeleted, totalDirsDeleted, nodeErrors := r.deleteRepositoryFromAllNodes(ctx, storageID)

	// Step 3: Delete search index
	r.deleteSearchIndex(ctx, storageID)

	if exists && r.isGuardianRepo(repo.repoID) {
		r.publishGuardianChange(&pb.ChangeEvent{
			StorageId: storageID,
			Type:      pb.ChangeType_DELETED,
		})
	}

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

// deleteRepositoryInternal performs the repository deletion and returns the response.
// Used by both the gRPC handler and the HTTP guardian API.
func (r *Router) deleteRepositoryInternal(ctx context.Context, storageID string) (*pb.DeleteRepositoryResponse, error) {
	return r.DeleteRepository(ctx, &pb.DeleteRepositoryRequest{StorageId: storageID})
}

// deleteGuardianFileFromAllNodes deletes a single file from a guardian partition across all nodes.
func (r *Router) deleteGuardianFileFromAllNodes(storageID, filePath string) (map[string]interface{}, error) {
	r.mu.RLock()
	nodesSnapshot := make(map[string]*nodeState)
	for nodeID, state := range r.nodes {
		nodesSnapshot[nodeID] = state
	}
	r.mu.RUnlock()

	var wg sync.WaitGroup
	var successCount int64
	var errorCount int64

	for nodeID, state := range nodesSnapshot {
		wg.Add(1)
		go func(nID string, s *nodeState) {
			defer wg.Done()
			if s.client == nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := s.client.DeleteFile(ctx, &pb.DeleteFileRequest{
				StorageId: storageID,
				FilePath:  filePath,
			})
			if err != nil {
				r.logger.Warn("failed to delete guardian file from node", "node_id", nID, "error", err)
				atomic.AddInt64(&errorCount, 1)
				return
			}
			if resp.Success {
				atomic.AddInt64(&successCount, 1)
			}
		}(nodeID, state)
	}

	wg.Wait()
	return map[string]interface{}{
		"success":       true,
		"message":       fmt.Sprintf("file deleted from %d nodes", successCount),
		"nodes_success": successCount,
		"nodes_error":   errorCount,
	}, nil
}

// deleteGuardianDirFromAllNodes deletes a directory recursively from a guardian partition across all nodes.
func (r *Router) deleteGuardianDirFromAllNodes(storageID, dirPath string) (map[string]interface{}, error) {
	r.mu.RLock()
	nodesSnapshot := make(map[string]*nodeState)
	for nodeID, state := range r.nodes {
		nodesSnapshot[nodeID] = state
	}
	r.mu.RUnlock()

	var wg sync.WaitGroup
	var totalFilesDeleted int64
	var totalDirsDeleted int64
	var errorCount int64

	for nodeID, state := range nodesSnapshot {
		wg.Add(1)
		go func(nID string, s *nodeState) {
			defer wg.Done()
			if s.client == nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			resp, err := s.client.DeleteDirectoryRecursive(ctx, &pb.DeleteDirectoryRecursiveRequest{
				StorageId: storageID,
				DirPath:   dirPath,
			})
			if err != nil {
				r.logger.Warn("failed to delete guardian dir from node", "node_id", nID, "error", err)
				atomic.AddInt64(&errorCount, 1)
				return
			}
			if resp.Success {
				atomic.AddInt64(&totalFilesDeleted, resp.FilesDeleted)
				atomic.AddInt64(&totalDirsDeleted, resp.DirsDeleted)
			}
		}(nodeID, state)
	}

	wg.Wait()
	return map[string]interface{}{
		"success":       true,
		"message":       fmt.Sprintf("deleted %d files and %d dirs", totalFilesDeleted, totalDirsDeleted),
		"files_deleted": totalFilesDeleted,
		"dirs_deleted":  totalDirsDeleted,
		"nodes_error":   errorCount,
	}, nil
}

// DeleteGuardianFile is the gRPC handler for deleting a file from a guardian partition.
func (r *Router) DeleteGuardianFile(ctx context.Context, req *pb.DeleteGuardianFileRequest) (*pb.DeleteGuardianFileResponse, error) {
	if _, ok := r.validateGuardianToken(req.Token); !ok {
		return &pb.DeleteGuardianFileResponse{
			Success: false,
			Message: "invalid guardian token",
		}, fmt.Errorf("invalid guardian token")
	}

	result, err := r.deleteGuardianFileFromAllNodes(req.StorageId, req.FilePath)
	if err != nil {
		return &pb.DeleteGuardianFileResponse{Success: false, Message: err.Error()}, err
	}
	r.publishGuardianChange(&pb.ChangeEvent{
		StorageId: req.StorageId,
		FilePath:  cleanGuardianRelativePath(req.FilePath),
		Type:      pb.ChangeType_DELETED,
	})

	return &pb.DeleteGuardianFileResponse{
		Success: true,
		Message: result["message"].(string),
	}, nil
}

// DeleteGuardianDirectory is the gRPC handler for deleting a directory from a guardian partition.
func (r *Router) DeleteGuardianDirectory(ctx context.Context, req *pb.DeleteGuardianDirectoryRequest) (*pb.DeleteGuardianDirectoryResponse, error) {
	if _, ok := r.validateGuardianToken(req.Token); !ok {
		return &pb.DeleteGuardianDirectoryResponse{
			Success: false,
			Message: "invalid guardian token",
		}, fmt.Errorf("invalid guardian token")
	}

	result, err := r.deleteGuardianDirFromAllNodes(req.StorageId, req.DirPath)
	if err != nil {
		return &pb.DeleteGuardianDirectoryResponse{Success: false, Message: err.Error()}, err
	}
	r.publishGuardianChange(&pb.ChangeEvent{
		StorageId: req.StorageId,
		FilePath:  cleanGuardianRelativePath(req.DirPath),
		Type:      pb.ChangeType_DELETED,
	})

	return &pb.DeleteGuardianDirectoryResponse{
		Success:      true,
		Message:      result["message"].(string),
		FilesDeleted: result["files_deleted"].(int64),
		DirsDeleted:  result["dirs_deleted"].(int64),
	}, nil
}
