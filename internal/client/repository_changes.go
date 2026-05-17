package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
)

type RepositoryChangeKind string

const (
	RepositoryChangeUpsert RepositoryChangeKind = "upsert"
	RepositoryChangeDelete RepositoryChangeKind = "delete"
)

type RepositoryChange struct {
	Kind    RepositoryChangeKind
	Path    string
	Content []byte
	Mode    uint32
	Mtime   int64
}

type ApplyRepositoryChangesResult struct {
	FilesUpserted int
	FilesDeleted  int
	FilesFailed   int
}

func (sc *ShardedClient) ApplyRepositoryChanges(ctx context.Context, repo WorkspaceRepository, changes []RepositoryChange) (*ApplyRepositoryChangesResult, error) {
	result := &ApplyRepositoryChangesResult{}
	if len(changes) == 0 {
		return result, nil
	}

	storageID := repo.StorageID
	if storageID == "" {
		if repo.DisplayPath == "" {
			return nil, fmt.Errorf("repository display path is required")
		}
		storageID = sharding.GenerateStorageID(repo.DisplayPath)
	}
	source := repo.Source
	if source == "" {
		source = repo.DisplayPath
	}

	sc.mu.RLock()
	hrw := sc.hrw
	nodeClients := make(map[string]pb.MonoFSClient, len(sc.clients))
	for nodeID, nodeClient := range sc.clients {
		nodeClients[nodeID] = nodeClient
	}
	rpcTimeout := sc.rpcTimeout
	sc.mu.RUnlock()

	if hrw == nil || len(nodeClients) == 0 {
		return nil, fmt.Errorf("not connected to cluster")
	}
	healthyNodes := hrw.GetHealthyNodes()
	if len(healthyNodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}
	if rpcTimeout <= 0 {
		rpcTimeout = 10 * time.Second
	}

	upsertBatches := make(map[string][]*pb.FileMetadata)
	deleteBatches := make(map[string][]string)
	hintFiles := make([]*pb.FileMetadata, 0, len(changes))
	now := time.Now().Unix()

	for _, change := range changes {
		if change.Path == "" {
			result.FilesFailed++
			continue
		}

		shardKey := sharding.BuildShardKey(storageID, change.Path)
		targetNodes := hrw.GetNodes(shardKey, 1)
		if len(targetNodes) == 0 {
			result.FilesFailed++
			continue
		}
		primaryNode := targetNodes[0]
		if _, ok := nodeClients[primaryNode.ID]; !ok {
			result.FilesFailed++
			continue
		}

		switch change.Kind {
		case RepositoryChangeUpsert:
			mode := change.Mode
			if mode == 0 {
				mode = 0644
			}
			mtime := change.Mtime
			if mtime == 0 {
				mtime = now
			}
			blobHash := sha256.Sum256(change.Content)
			meta := &pb.FileMetadata{
				Path:          change.Path,
				Size:          uint64(len(change.Content)),
				Mtime:         mtime,
				Mode:          mode,
				BlobHash:      hex.EncodeToString(blobHash[:]),
				Source:        source,
				StorageId:     storageID,
				DisplayPath:   repo.DisplayPath,
				Ref:           repo.Ref,
				InlineContent: change.Content,
			}
			upsertBatches[primaryNode.ID] = append(upsertBatches[primaryNode.ID], meta)
			hintFiles = append(hintFiles, &pb.FileMetadata{
				Path:  change.Path,
				Size:  uint64(len(change.Content)),
				Mtime: mtime,
				Mode:  mode,
				BackendMetadata: map[string]string{
					"dir_hint": "true",
				},
			})
		case RepositoryChangeDelete:
			deleteBatches[primaryNode.ID] = append(deleteBatches[primaryNode.ID], change.Path)
		default:
			return nil, fmt.Errorf("unsupported repository change kind %q", change.Kind)
		}
	}

	for nodeID, fileMetas := range upsertBatches {
		nodeClient, ok := nodeClients[nodeID]
		if !ok {
			result.FilesFailed += len(fileMetas)
			continue
		}
		batchCtx, cancel := context.WithTimeout(ctx, 2*rpcTimeout)
		resp, err := nodeClient.IngestFileBatch(batchCtx, &pb.IngestFileBatchRequest{
			Files:       fileMetas,
			StorageId:   storageID,
			DisplayPath: repo.DisplayPath,
			Source:      source,
			Ref:         repo.Ref,
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("ingest repository batch on %s: %w", nodeID, err)
		}
		result.FilesUpserted += int(resp.FilesIngested)
		result.FilesFailed += int(resp.FilesFailed)
		if !resp.Success || resp.ErrorMessage != "" {
			return nil, fmt.Errorf("ingest repository batch on %s failed: %s", nodeID, resp.ErrorMessage)
		}
	}

	if len(hintFiles) > 0 {
		for _, node := range healthyNodes {
			nodeClient, ok := nodeClients[node.ID]
			if !ok {
				continue
			}
			hintCtx, cancel := context.WithTimeout(ctx, 2*rpcTimeout)
			_, err := nodeClient.IngestFileBatch(hintCtx, &pb.IngestFileBatchRequest{
				Files:       hintFiles,
				StorageId:   storageID,
				DisplayPath: repo.DisplayPath,
				Source:      "dir-hint",
				Ref:         repo.Ref,
			})
			cancel()
			if err != nil && sc.logger != nil {
				sc.logger.Warn("failed to send repository dir hints",
					"node_id", node.ID,
					"storage_id", storageID,
					"error", err)
			}
		}
	}

	for nodeID, paths := range deleteBatches {
		nodeClient, ok := nodeClients[nodeID]
		if !ok {
			result.FilesFailed += len(paths)
			continue
		}
		for _, filePath := range paths {
			deleteCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
			resp, err := nodeClient.DeleteFile(deleteCtx, &pb.DeleteFileRequest{
				StorageId: storageID,
				FilePath:  filePath,
			})
			cancel()
			if err != nil {
				return nil, fmt.Errorf("delete repository path %q on %s: %w", filePath, nodeID, err)
			}
			if !resp.Success {
				result.FilesFailed++
				return nil, fmt.Errorf("delete repository path %q on %s failed: %s", filePath, nodeID, resp.Message)
			}
			result.FilesDeleted++
		}
	}

	for _, node := range healthyNodes {
		nodeClient, ok := nodeClients[node.ID]
		if !ok {
			continue
		}
		indexCtx, cancel := context.WithTimeout(ctx, 3*rpcTimeout)
		_, err := nodeClient.BuildDirectoryIndexes(indexCtx, &pb.BuildDirectoryIndexesRequest{StorageId: storageID})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("build repository indexes on %s: %w", node.ID, err)
		}
	}

	if result.FilesFailed > 0 {
		return result, fmt.Errorf("%d repository changes failed", result.FilesFailed)
	}
	return result, nil
}
