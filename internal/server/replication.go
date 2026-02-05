package server

import (
	"context"
	"strings"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
)

// GetRepositoryFiles returns list of files this node owns for a repository.
func (s *Server) GetRepositoryFiles(ctx context.Context, req *pb.GetRepositoryFilesRequest) (*pb.GetRepositoryFilesResponse, error) {
	var files []string
	prefix := []byte(req.StorageId + ":")

	s.db.View(func(tx *nutsdb.Tx) error {
		// Use PrefixScanEntries to get keys matching the storageID prefix
		keys, _, err := tx.PrefixScanEntries(bucketOwnedFiles, prefix, "", 0, -1, true, false)
		if err != nil && err != nutsdb.ErrBucketNotFound && err != nutsdb.ErrPrefixScan {
			return nil // Ignore errors, empty result is okay
		}

		for _, key := range keys {
			// Extract file path from key (key format: "storageID:filePath")
			keyStr := string(key)
			prefixStr := req.StorageId + ":"
			if strings.HasPrefix(keyStr, prefixStr) {
				files = append(files, strings.TrimPrefix(keyStr, prefixStr))
			}
		}
		return nil
	})

	s.logger.Debug("retrieved repository files",
		"storage_id", req.StorageId,
		"file_count", len(files))

	return &pb.GetRepositoryFilesResponse{
		Files:  files,
		NodeId: s.nodeID,
	}, nil
}
