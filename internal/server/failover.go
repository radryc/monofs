package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
)

const (
	bucketFailover          = "failover" // Temporary failover cache
	failoverCacheTTLSeconds = 600        // 10 minutes
)

// SyncMetadataFromNode implements failover metadata synchronization.
// Called by router when this node becomes a backup for a failed node.
// Copies replica metadata into failover cache for fast lookup.
func (s *Server) SyncMetadataFromNode(ctx context.Context, req *pb.SyncMetadataFromNodeRequest) (*pb.SyncMetadataFromNodeResponse, error) {
	failedNodeID := req.SourceNodeId
	fileList := req.Files

	s.logger.Info("starting failover metadata sync",
		"failed_node", failedNodeID,
		"backup_node", s.nodeID,
		"file_count", len(fileList))

	// Copy replica metadata to failover cache
	syncedCount := int64(0)
	missingCount := int64(0)

	err := s.db.Update(func(tx *nutsdb.Tx) error {
		for _, fileInfo := range fileList {
			// Check if we have replica metadata for this file
			replicaKey := makeReplicaKey(fileInfo.StorageId, fileInfo.FilePath, failedNodeID)

			metadata, err := tx.Get(bucketReplicaFiles, replicaKey)
			if err != nil {
				// We don't have this replica (expected if we're not a replica node)
				missingCount++
				s.logger.Debug("no replica metadata found",
					"file", fileInfo.FilePath,
					"storage_id", fileInfo.StorageId)
				continue
			}

			// Copy to failover cache with TTL
			failoverKey := makeFailoverKey(failedNodeID, fileInfo.StorageId, fileInfo.FilePath)

			if err := tx.Put(bucketFailover, failoverKey, metadata, failoverCacheTTLSeconds); err != nil {
				s.logger.Warn("failed to store in failover cache",
					"file", fileInfo.FilePath,
					"error", err)
				continue
			}

			syncedCount++
		}
		return nil
	})

	if err != nil {
		return &pb.SyncMetadataFromNodeResponse{
			Success:     false,
			FilesSynced: syncedCount,
			Message:     fmt.Sprintf("sync failed: %v", err),
		}, err
	}

	s.logger.Info("failover metadata sync completed",
		"failed_node", failedNodeID,
		"synced", syncedCount,
		"missing", missingCount,
		"total", len(fileList))

	return &pb.SyncMetadataFromNodeResponse{
		Success:     true,
		FilesSynced: syncedCount,
		Message:     fmt.Sprintf("Synced %d files to failover cache", syncedCount),
	}, nil
}

// ClearFailoverCache removes temporary failover metadata after node recovery.
// Called by router when a failed node comes back online.
func (s *Server) ClearFailoverCache(ctx context.Context, req *pb.ClearFailoverCacheRequest) (*pb.ClearFailoverCacheResponse, error) {
	recoveredNodeID := req.RecoveredNodeId

	s.logger.Info("clearing failover cache",
		"recovered_node", recoveredNodeID)

	deletedCount := int64(0)
	prefix := []byte(recoveredNodeID + ":")

	err := s.db.Update(func(tx *nutsdb.Tx) error {
		// Get all keys in failover bucket with prefix
		keys, _, err := tx.PrefixScanEntries(bucketFailover, prefix, "", 0, -1, true, false)
		if err != nil && err != nutsdb.ErrBucketNotFound && err != nutsdb.ErrPrefixScan {
			return err
		}

		// Delete entries for the recovered node
		for _, key := range keys {
			if err := tx.Delete(bucketFailover, key); err != nil {
				s.logger.Warn("failed to delete failover cache entry",
					"key", string(key),
					"error", err)
				continue
			}
			deletedCount++
		}
		return nil
	})

	if err != nil {
		return &pb.ClearFailoverCacheResponse{
			Success: false,
			Message: fmt.Sprintf("failed to clear cache: %v", err),
		}, err
	}

	s.logger.Info("failover cache cleared",
		"recovered_node", recoveredNodeID,
		"entries_deleted", deletedCount)

	return &pb.ClearFailoverCacheResponse{
		Success:        true,
		EntriesCleared: deletedCount,
		Message:        fmt.Sprintf("Cleared %d failover cache entries", deletedCount),
	}, nil
}

// checkFailoverCache looks up metadata in the failover cache
func (s *Server) checkFailoverCache(storageID, filePath string) (*storedMetadata, bool) {
	var metadata storedMetadata
	found := false

	err := s.db.View(func(tx *nutsdb.Tx) error {
		// Scan failover bucket for matching file
		// Key format: "failedNodeID:storageID:filePath"
		// We need to find entries with suffix ":storageID:filePath"

		// Get all keys in failover bucket
		keys, _, err := tx.PrefixScanEntries(bucketFailover, []byte(""), "", 0, -1, true, false)
		if err != nil && err != nutsdb.ErrBucketNotFound && err != nutsdb.ErrPrefixScan {
			return err
		}

		targetSuffix := ":" + storageID + ":" + filePath
		for _, key := range keys {
			if strings.HasSuffix(string(key), targetSuffix) {
				// Found a match, get the value
				value, err := tx.Get(bucketFailover, key)
				if err != nil {
					continue
				}

				if err := json.Unmarshal(value, &metadata); err != nil {
					return err
				}
				found = true
				return nil
			}
		}

		return nutsdb.ErrKeyNotFound
	})

	if err != nil || !found {
		return nil, false
	}

	return &metadata, true
}

// makeFailoverKey creates a failover cache key
// Format: "failedNodeID:storageID:filePath"
func makeFailoverKey(failedNodeID, storageID, filePath string) []byte {
	return []byte(fmt.Sprintf("%s:%s:%s", failedNodeID, storageID, filePath))
}

// makeReplicaKey creates a replica file key
// Format: "storageID:filePath:primary:primaryNodeID"
func makeReplicaKey(storageID, filePath, primaryNodeID string) []byte {
	return []byte(fmt.Sprintf("%s:%s:primary:%s", storageID, filePath, primaryNodeID))
}
