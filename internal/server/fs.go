package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/fetcher"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Lookup implements the Lookup RPC.
func (s *Server) Lookup(ctx context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	path := req.ParentPath
	if path == "" && req.Name != "" {
		path = req.Name
	} else if path != "" && req.Name != "" {
		path = path + "/" + req.Name
	}

	s.logger.Debug("lookup",
		"parent_path", req.ParentPath,
		"name", req.Name,
		"resolved_path", path)

	// Handle root directory
	if path == "" {
		return &pb.LookupResponse{
			Ino:   1,
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: time.Now().Unix(),
			Found: true,
		}, nil
	}

	// Check if path is a full repo ID or an intermediate directory
	// First check if it's a complete repo ID (display path)
	if s.repoExists(path) {
		return &pb.LookupResponse{
			Ino:   hashPath(path),
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: time.Now().Unix(),
			Found: true,
		}, nil
	}

	// Check if it's an intermediate directory (cached check)
	if s.isIntermediateDir(path) {
		return &pb.LookupResponse{
			Ino:   hashPath(path),
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: time.Now().Unix(),
			Found: true,
		}, nil
	}

	// Try to resolve path to (storageID, filePath)
	storageID, filePath, ok := s.resolvePathToStorage(path)
	if !ok {
		// No matching repository found
		return &pb.LookupResponse{Found: false}, nil
	}

	// If we matched a repo but have no file path, it's the repo directory itself
	if filePath == "" {
		return &pb.LookupResponse{
			Ino:   hashPath(path),
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: time.Now().Unix(),
			Found: true,
		}, nil
	}

	// Lookup file within repository using path index
	key, cached := s.getHashFromPath(storageID, filePath)

	s.logger.Debug("lookup file in repo",
		"path", path,
		"storage_id", storageID,
		"file_path", filePath,
		"hash_key", string(key),
		"cached", cached)

	var found *pb.LookupResponse
	err := s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketMetadata, key)
		if err != nil {
			s.logger.Debug("lookup key not found in db",
				"key", string(key),
				"error", err)
			return err
		}

		var stored storedMetadata
		if err := json.Unmarshal(value, &stored); err != nil {
			return err
		}

		mode := stored.Mode
		if stored.IsDir {
			mode = mode | uint32(syscall.S_IFDIR)
		} else {
			mode = mode | uint32(syscall.S_IFREG)
		}

		found = &pb.LookupResponse{
			Ino:   hashPath(path),
			Mode:  mode,
			Size:  stored.Size,
			Mtime: stored.Mtime,
			Found: true,
		}
		s.logger.Debug("lookup found",
			"path", path,
			"key", string(key),
			"mode", fmt.Sprintf("0%o", mode),
			"size", stored.Size)
		return nil
	})

	if err == nil && found != nil {
		return found, nil
	}

	// NEW: Check if it's a virtual directory in the directory index
	// This handles directories that don't have explicit metadata entries
	if virtualDir := s.checkVirtualDirectory(storageID, filePath); virtualDir != nil {
		return virtualDir, nil
	}

	// NEW: Check if it's a virtual file in the directory index
	// This handles files that may not have explicit metadata entries but are
	// listed in their parent's directory index (e.g., after overlay cleanup)
	if virtualFile := s.checkVirtualFile(storageID, filePath); virtualFile != nil {
		return virtualFile, nil
	}

	// Check failover cache (for files from failed nodes)
	if failoverMeta, ok := s.checkFailoverCache(storageID, filePath); ok {
		s.logger.Debug("serving from failover cache",
			"path", path,
			"storage_id", storageID,
			"file_path", filePath)

		mode := failoverMeta.Mode
		if failoverMeta.IsDir {
			mode = mode | uint32(syscall.S_IFDIR)
		} else {
			mode = mode | uint32(syscall.S_IFREG)
		}

		return &pb.LookupResponse{
			Ino:   hashPath(path),
			Mode:  mode,
			Size:  failoverMeta.Size,
			Mtime: failoverMeta.Mtime,
			Found: true,
		}, nil
	}


	// NEW: Forward to correct node if not handling locally (and not already forwarded)
	if s.enableForwarding && !isAlreadyForwarded(ctx) {
		targetNode := s.getTargetNode(storageID, filePath)
		
		// Try primary node first if healthy
		if targetNode != nil && targetNode.ID != s.nodeID && s.isNodeHealthy(targetNode.ID) {
			s.logger.Debug("lookup forwarding to primary node",
				"path", path,
				"storage_id", storageID,
				"file_path", filePath,
				"target_node", targetNode.ID)
			return s.forwardLookup(ctx, req, targetNode)
		}
		
		// Primary is unhealthy, try backup nodes
		if targetNode != nil && !s.isNodeHealthy(targetNode.ID) {
			backupNodes := s.getBackupNodes(storageID, filePath)
			for _, backup := range backupNodes {
				if backup.ID == s.nodeID {
					// This node is a backup, handle locally
					break
				}
				s.logger.Debug("lookup forwarding to backup node",
					"path", path,
					"primary", targetNode.ID,
					"backup", backup.ID)
				resp, err := s.forwardLookup(ctx, req, backup)
				if err == nil && resp.Found {
					return resp, nil
				}
				// Try next backup
			}
		}
	}
	// Not found
	s.logger.Debug("lookup not found", "path", path)
	return &pb.LookupResponse{Found: false}, nil
}

// GetAttr implements the GetAttr RPC.
func (s *Server) GetAttr(ctx context.Context, req *pb.GetAttrRequest) (*pb.GetAttrResponse, error) {
	path := req.Path
	s.logger.Debug("getattr request", "path", path)

	// Handle root directory
	if path == "" {
		return &pb.GetAttrResponse{
			Ino:   1,
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: time.Now().Unix(),
			Atime: time.Now().Unix(),
			Ctime: time.Now().Unix(),
			Nlink: 2,
			Uid:   uint32(1000),
			Gid:   uint32(1000),
			Found: true,
		}, nil
	}

	// Check if path is a full repo ID or an intermediate directory
	// First check if it's a complete repo ID
	if s.repoExists(path) {
		return &pb.GetAttrResponse{
			Ino:   hashPath(path),
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: time.Now().Unix(),
			Atime: time.Now().Unix(),
			Ctime: time.Now().Unix(),
			Nlink: 2,
			Uid:   uint32(1000),
			Gid:   uint32(1000),
			Found: true,
		}, nil
	}

	// Check if it's an intermediate directory (cached check)
	if s.isIntermediateDir(path) {
		return &pb.GetAttrResponse{
			Ino:   hashPath(path),
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: time.Now().Unix(),
			Atime: time.Now().Unix(),
			Ctime: time.Now().Unix(),
			Nlink: 2,
			Uid:   uint32(1000),
			Gid:   uint32(1000),
			Found: true,
		}, nil
	}

	// Try to resolve path to (storageID, filePath)
	storageID, filePath, ok := s.resolvePathToStorage(path)

	s.logger.Debug("getattr path resolution",
		"path", path,
		"storage_id", storageID,
		"file_path", filePath,
		"resolved", ok)

	// If no repo matched, not found
	if !ok {
		s.logger.Debug("getattr no repo matched", "path", path)
		return &pb.GetAttrResponse{Found: false}, nil
	}

	// If we matched a repo but have no file path, it's the repo directory itself
	if filePath == "" {
		s.logger.Debug("getattr returning repo dir", "path", path, "storage_id", storageID)
		return &pb.GetAttrResponse{
			Ino:   hashPath(path),
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: time.Now().Unix(),
			Atime: time.Now().Unix(),
			Ctime: time.Now().Unix(),
			Nlink: 2,
			Uid:   uint32(1000),
			Gid:   uint32(1000),
			Found: true,
		}, nil
	}

	// Get file attributes using path index
	key, cached := s.getHashFromPath(storageID, filePath)
	s.logger.Debug("getattr looking up key",
		"path", path,
		"storage_id", storageID,
		"file_path", filePath,
		"hash_key", string(key),
		"cached", cached)

	var found *pb.GetAttrResponse
	err := s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketMetadata, key)
		if err != nil {
			s.logger.Debug("getattr key not found", "key", string(key), "error", err)
			return err
		}

		var stored storedMetadata
		if err := json.Unmarshal(value, &stored); err != nil {
			return err
		}

		mode := stored.Mode
		nlink := uint32(1)
		if stored.IsDir {
			mode = mode | uint32(syscall.S_IFDIR)
			nlink = 2
		} else {
			mode = mode | uint32(syscall.S_IFREG)
		}

		found = &pb.GetAttrResponse{
			Ino:   hashPath(path),
			Mode:  mode,
			Size:  stored.Size,
			Mtime: stored.Mtime,
			Atime: stored.Mtime,
			Ctime: stored.Mtime,
			Nlink: nlink,
			Uid:   uint32(1000),
			Gid:   uint32(1000),
			Found: true,
		}
		s.logger.Debug("getattr found",
			"path", path,
			"key", string(key),
			"mode", fmt.Sprintf("0%o", mode),
			"size", stored.Size,
			"is_dir", stored.IsDir)
		return nil
	})

	if err == nil && found != nil {
		return found, nil
	}

	// NEW: Check if it's a virtual directory in the directory index
	if virtualDir := s.checkVirtualDirectory(storageID, filePath); virtualDir != nil {
		return &pb.GetAttrResponse{
			Ino:   virtualDir.Ino,
			Mode:  virtualDir.Mode,
			Size:  virtualDir.Size,
			Mtime: virtualDir.Mtime,
			Atime: virtualDir.Mtime,
			Ctime: virtualDir.Mtime,
			Nlink: 2,
			Uid:   uint32(1000),
			Gid:   uint32(1000),
			Found: true,
		}, nil
	}

	// NEW: Check if it's a virtual file in the directory index
	if virtualFile := s.checkVirtualFile(storageID, filePath); virtualFile != nil {
		return &pb.GetAttrResponse{
			Ino:   virtualFile.Ino,
			Mode:  virtualFile.Mode,
			Size:  virtualFile.Size,
			Mtime: virtualFile.Mtime,
			Atime: virtualFile.Mtime,
			Ctime: virtualFile.Mtime,
			Nlink: 1,
			Uid:   uint32(1000),
			Gid:   uint32(1000),
			Found: true,
		}, nil
	}


	// NEW: Forward to correct node if not handling locally (and not already forwarded)
	if s.enableForwarding && !isAlreadyForwarded(ctx) {
		targetNode := s.getTargetNode(storageID, filePath)
		
		// Try primary node first if healthy
		if targetNode != nil && targetNode.ID != s.nodeID && s.isNodeHealthy(targetNode.ID) {
			s.logger.Debug("getattr forwarding to primary node",
				"path", path,
				"storage_id", storageID,
				"file_path", filePath,
				"target_node", targetNode.ID)
			return s.forwardGetAttr(ctx, req, targetNode)
		}
		
		// Primary is unhealthy, try backup nodes
		if targetNode != nil && !s.isNodeHealthy(targetNode.ID) {
			backupNodes := s.getBackupNodes(storageID, filePath)
			for _, backup := range backupNodes {
				if backup.ID == s.nodeID {
					break
				}
				s.logger.Debug("getattr forwarding to backup node",
					"path", path,
					"primary", targetNode.ID,
					"backup", backup.ID)
				resp, err := s.forwardGetAttr(ctx, req, backup)
				if err == nil && resp.Found {
					return resp, nil
				}
			}
		}
	}
	s.logger.Debug("getattr not found", "path", path, "key", string(key))
	return &pb.GetAttrResponse{Found: false}, nil
}

// Read implements the Read RPC - lazy loads from Git repo.
func (s *Server) Read(req *pb.ReadRequest, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	path := req.Path
	s.logger.Info("read request", "path", path, "offset", req.Offset, "size", req.Size)

	// Resolve path to (storageID, filePath)
	storageID, filePath, ok := s.resolvePathToStorage(path)

	if !ok || filePath == "" {
		s.logger.Error("read: path resolution failed", "path", path, "ok", ok, "storage_id", storageID, "file_path", filePath)
		return status.Errorf(codes.NotFound, "path resolution failed: %s", path)
	}


	// Try local metadata first (covers both owned files AND replicas from IngestReplicaBatch)
	var blobHash, repoURL, branch, displayPath string
	key, cached := s.getHashFromPath(storageID, filePath)

	s.logger.Debug("read file",
		"path", path,
		"storage_id", storageID,
		"file_path", filePath,
		"hash_key", string(key),
		"cached", cached)

	err := s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketMetadata, key)
		if err != nil {
			return err
		}

		var stored storedMetadata
		if err := json.Unmarshal(value, &stored); err != nil {
			return err
		}

		blobHash = stored.BlobHash
		branch = stored.Branch
		repoURL = stored.RepoURL
		displayPath = stored.DisplayPath
		return nil
	})

	// If not found in primary storage, try failover cache
	if err != nil {
		if failoverMeta, ok := s.checkFailoverCache(storageID, filePath); ok {
			s.logger.Debug("read: serving from failover cache",
				"path", path,
				"storage_id", storageID,
				"file_path", filePath)
			blobHash = failoverMeta.BlobHash
			branch = failoverMeta.Branch
			repoURL = failoverMeta.RepoURL
			displayPath = failoverMeta.DisplayPath
			err = nil // Clear error since we found it in failover cache
		}
	}

	// If not found locally, try forwarding to the correct node
	if err != nil && s.enableForwarding && !isAlreadyForwarded(stream.Context()) {
		targetNode := s.getTargetNode(storageID, filePath)

		// Try primary node first if healthy
		if targetNode != nil && targetNode.ID != s.nodeID && s.isNodeHealthy(targetNode.ID) {
			s.logger.Debug("read forwarding to primary node",
				"path", path,
				"storage_id", storageID,
				"file_path", filePath,
				"target_node", targetNode.ID)
			return s.forwardRead(req, stream, targetNode)
		}

		// Primary is unhealthy, try backup nodes
		if targetNode != nil && !s.isNodeHealthy(targetNode.ID) {
			backupNodes := s.getBackupNodes(storageID, filePath)
			for _, backup := range backupNodes {
				if backup.ID == s.nodeID {
					break
				}
				s.logger.Debug("read forwarding to backup node",
					"path", path,
					"primary", targetNode.ID,
					"backup", backup.ID)
				fwdErr := s.forwardRead(req, stream, backup)
				if fwdErr == nil {
					return nil
				}
			}
		}
	}

	// If not found anywhere, return error
	if err != nil {
		s.logger.Warn("read: metadata not found",
			"path", path,
			"storage_id", storageID,
			"file_path", filePath)
		return status.Errorf(codes.NotFound, "file not found: %s", path)
	}

	if blobHash == "" {
		s.logger.Warn("read: blob hash is empty", "path", path, "repo_url", repoURL, "branch", branch, "display_path", displayPath)
		return status.Errorf(codes.NotFound, "blob hash is empty for: %s", path)
	}

	// SHA-256 of empty content — no need to hit the fetcher for 0-byte files.
	const emptyBlobHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if blobHash == emptyBlobHash {
		// Empty file — nothing to stream
		return nil
	}

	// Read blob content via fetcher
	var content []byte
	var wasPrefetched bool
	ctx := stream.Context()

	s.logger.Debug("read: reading blob via fetcher", "blob_hash", blobHash, "repo_url", repoURL, "display_path", displayPath, "branch", branch)

	// Fetcher client is required for all blob reads
	if s.fetcherClient == nil {
		s.logger.Error("read: fetcher client not configured - cannot read blobs without fetchers")
		return status.Errorf(codes.FailedPrecondition, "storage node not configured: fetcher client required")
	}

	var fetchErr error
	content, wasPrefetched, fetchErr = s.readViaFetcher(ctx, storageID, blobHash, repoURL, filePath, branch)
	if fetchErr != nil {
		s.logger.Error("read: fetcher request failed", "path", path, "blob_hash", blobHash, "error", fetchErr)
		return status.Errorf(codes.Unavailable, "failed to read blob via fetcher: %v", fetchErr)
	}

	// Track prefetch hit/miss metrics
	if wasPrefetched {
		s.prefetchHits.Add(1)
	} else {
		s.prefetchMisses.Add(1)
	}

	s.logger.Info("read: blob retrieved successfully", "path", path, "content_size", len(content), "blob_hash", blobHash, "prefetch_hit", wasPrefetched)
	s.filesServed.Add(1)

	// Record access for predictor (asynchronously to not block response)
	if s.predictor != nil {
		go s.recordAccessForPredictor(ctx, storageID, filePath, blobHash, repoURL, branch)
	}

	// Handle offset and size
	offset := req.Offset
	size := req.Size

	if offset >= int64(len(content)) {
		return nil
	}
	content = content[offset:]

	if size > 0 && size < int64(len(content)) {
		content = content[:size]
	}

	// Stream in chunks
	chunkSize := 64 * 1024
	currentOffset := offset

	for len(content) > 0 {
		chunk := content
		if len(chunk) > chunkSize {
			chunk = chunk[:chunkSize]
		}

		if err := stream.Send(&pb.DataChunk{
			Data:   chunk,
			Offset: currentOffset,
		}); err != nil {
			return err
		}

		content = content[len(chunk):]
		currentOffset += int64(len(chunk))
	}

	return nil
}

// Create implements the Create RPC.
func (s *Server) Create(ctx context.Context, req *pb.CreateRequest) (*pb.CreateResponse, error) {
	s.logger.Debug("create not implemented", "path", req.ParentPath+"/"+req.Name)
	return &pb.CreateResponse{Success: false}, fmt.Errorf("create not implemented")
}

// Write implements the Write RPC (client streaming).
func (s *Server) Write(stream grpc.ClientStreamingServer[pb.WriteRequest, pb.WriteResponse]) error {
	return fmt.Errorf("write not implemented")
}

// DeleteFile removes a file's metadata after rebalancing (called by router).
// This is used to clean up old file copies after files have been moved to new nodes.
// Important: This is ONLY called during rebalancing cleanup, NOT during recovery.
func (s *Server) DeleteFile(ctx context.Context, req *pb.DeleteFileRequest) (*pb.DeleteFileResponse, error) {
	key := makeStorageKey(req.StorageId, req.FilePath)

	// Track if file existed to properly update counter
	var fileExisted bool
	var blobHash string

	err := s.db.Update(func(tx *nutsdb.Tx) error {
		// Check if file exists in owned files bucket
		ownershipKey := []byte(req.StorageId + ":" + req.FilePath)
		_, err := tx.Get(bucketOwnedFiles, ownershipKey)
		fileExisted = (err == nil)

		// Read metadata to get blob hash before deleting
		if metaData, getErr := tx.Get(bucketMetadata, key); getErr == nil {
			var meta storedMetadata
			if json.Unmarshal(metaData, &meta) == nil {
				blobHash = meta.BlobHash
			}
		}

		// Remove from main metadata bucket
		if err := tx.Delete(bucketMetadata, key); err != nil && err != nutsdb.ErrKeyNotFound {
			return fmt.Errorf("failed to delete metadata: %w", err)
		}

		// Remove ownership tracking
		if err := tx.Delete(bucketOwnedFiles, ownershipKey); err != nil && err != nutsdb.ErrKeyNotFound {
			return fmt.Errorf("failed to delete ownership: %w", err)
		}

		// Remove from path index
		pathIndexKey := []byte(req.StorageId + ":" + req.FilePath)
		if err := tx.Delete(bucketPathIndex, pathIndexKey); err != nil && err != nutsdb.ErrKeyNotFound {
			return fmt.Errorf("failed to delete path index: %w", err)
		}

		return nil
	})

	if err != nil {
		s.logger.Warn("failed to delete file during rebalancing cleanup",
			"storage_id", req.StorageId,
			"file_path", req.FilePath,
			"error", err)
		return &pb.DeleteFileResponse{
			Success: false,
			Message: err.Error(),
		}, err
	}

	// Decrement counter only if file actually existed
	if fileExisted {
		s.totalFiles.Add(-1)
	}

	// Forward blob deletion to fetcher (async, best-effort)
	if s.fetcherClient != nil && blobHash != "" {
		go func() {
			fwdCtx, fwdCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer fwdCancel()
			deleted, _, fwdErr := s.fetcherClient.DeleteBlobs(fwdCtx, []string{blobHash}, false)
			if fwdErr != nil {
				s.logger.Warn("failed to forward blob deletion to fetcher",
					"blob_hash", blobHash, "error", fwdErr)
			} else if deleted > 0 {
				s.logger.Debug("forwarded blob deletion to fetcher",
					"blob_hash", blobHash)
			}
		}()
	}

	s.logger.Debug("file deleted after rebalancing",
		"storage_id", req.StorageId,
		"file_path", req.FilePath)

	return &pb.DeleteFileResponse{
		Success: true,
		Message: "File deleted successfully",
	}, nil
}

// DeleteRepository removes all data for a repository from this node.
// This includes: repo info, display path lookup, onboarding status,
// all owned files, all replica files, all directory indexes.
func (s *Server) DeleteRepository(ctx context.Context, req *pb.DeleteRepositoryOnNodeRequest) (*pb.DeleteRepositoryOnNodeResponse, error) {
	storageID := req.StorageId
	s.logger.Info("deleting repository from node", "storage_id", storageID)

	var filesDeleted int64
	var dirsDeleted int64

	err := s.db.Update(func(tx *nutsdb.Tx) error {
		// 1. Delete repo info
		repoKey := []byte(storageID)
		var displayPath string
		if val, err := tx.Get(bucketRepos, repoKey); err == nil {
			var info repoInfo
			if json.Unmarshal(val, &info) == nil {
				displayPath = info.DisplayPath
			}
		}
		if err := tx.Delete(bucketRepos, repoKey); err != nil && err != nutsdb.ErrKeyNotFound {
			return fmt.Errorf("delete repo info: %w", err)
		}

		// 2. Delete display path → storage_id lookup
		if displayPath != "" {
			lookupKey := []byte(displayPath)
			if err := tx.Delete(bucketRepoLookup, lookupKey); err != nil && err != nutsdb.ErrKeyNotFound {
				return fmt.Errorf("delete path lookup: %w", err)
			}
		}

		// 3. Delete onboarding status
		onboardKey := []byte(storageID)
		if err := tx.Delete(bucketOnboardingStatus, onboardKey); err != nil && err != nutsdb.ErrKeyNotFound {
			// Ignore: bucket may not exist
		}

		// 4. Delete all owned files
		prefix := storageID + ":"
		ownedKeys, err := tx.GetKeys(bucketOwnedFiles)
		if err == nil {
			for _, key := range ownedKeys {
				keyStr := string(key)
				if !strings.HasPrefix(keyStr, prefix) {
					continue
				}
				// Delete from metadata bucket
				if err := tx.Delete(bucketMetadata, key); err != nil && err != nutsdb.ErrKeyNotFound {
					s.logger.Warn("failed to delete owned file metadata", "key", keyStr)
				}
				// Delete ownership tracking
				if err := tx.Delete(bucketOwnedFiles, key); err != nil && err != nutsdb.ErrKeyNotFound {
					s.logger.Warn("failed to delete ownership key", "key", keyStr)
				}
				// Delete from path index
				if err := tx.Delete(bucketPathIndex, key); err != nil && err != nutsdb.ErrKeyNotFound {
					// path index may use different key format
				}
				filesDeleted++
			}
		}

		// 5. Delete all replica files
		replicaKeys, err := tx.GetKeys(bucketReplicaFiles)
		if err == nil {
			for _, key := range replicaKeys {
				keyStr := string(key)
				if !strings.HasPrefix(keyStr, prefix) {
					continue
				}
				if err := tx.Delete(bucketReplicaFiles, key); err != nil && err != nutsdb.ErrKeyNotFound {
					s.logger.Warn("failed to delete replica key", "key", keyStr)
				}
				// Also clean metadata for replicas
				if err := tx.Delete(bucketMetadata, key); err != nil && err != nutsdb.ErrKeyNotFound {
					// may not exist
				}
			}
		}

		// 6. Delete all directory indexes for this repo
		dirPrefix := storageID + "/"
		dirKeys, err := tx.GetKeys(bucketDirIndex)
		if err == nil {
			for _, key := range dirKeys {
				keyStr := string(key)
				if !strings.HasPrefix(keyStr, dirPrefix) {
					continue
				}
				if err := tx.Delete(bucketDirIndex, key); err != nil && err != nutsdb.ErrKeyNotFound {
					s.logger.Warn("failed to delete dir index", "key", keyStr)
				}
				dirsDeleted++
			}
		}

		return nil
	})

	if err != nil {
		s.logger.Error("failed to delete repository", "storage_id", storageID, "error", err)
		return &pb.DeleteRepositoryOnNodeResponse{
			Success: false,
			Message: err.Error(),
		}, err
	}

	// Update file counter
	s.totalFiles.Add(-filesDeleted)

	// Clear intermediate directory cache
	s.intermediateDirCache.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			if len(k) > len(storageID) && k[:len(storageID)] == storageID {
				s.intermediateDirCache.Delete(key)
			}
		}
		return true
	})

	s.logger.Info("repository deleted from node",
		"storage_id", storageID,
		"files_deleted", filesDeleted,
		"dirs_deleted", dirsDeleted)

	return &pb.DeleteRepositoryOnNodeResponse{
		Success:      true,
		Message:      "Repository deleted successfully",
		FilesDeleted: filesDeleted,
		DirsDeleted:  dirsDeleted,
	}, nil
}

// readViaFetcher attempts to read blob content via the fetcher service.
// Returns the content, whether it was a prefetch hit, and any error.
func (s *Server) readViaFetcher(ctx context.Context, storageID, blobHash, repoURL, filePath, branch string) ([]byte, bool, error) {
	// Default to blob source type (packager archive). Git is optional.
	sourceType := fetcher.SourceTypeBlob

	// Check prefetch cache
	var wasPrefetched bool
	inCache, err := s.fetcherClient.CheckCacheSimple(ctx, repoURL, blobHash)
	if err != nil {
		return nil, false, fmt.Errorf("check cache failed: %w", err)
	}
	wasPrefetched = inCache

	// Fetch via fetcher service
	content, err := s.fetcherClient.FetchBlobSimple(ctx, repoURL, blobHash, filePath, branch, sourceType)
	if err != nil {
		return nil, false, fmt.Errorf("fetch blob failed: %w", err)
	}

	return content, wasPrefetched, nil
}

// recordAccessForPredictor records file access for the predictor.
// The predictor handles prediction and prefetch triggering internally.
func (s *Server) recordAccessForPredictor(ctx context.Context, storageID, filePath, blobHash, repoURL, branch string) {
	// Default to blob source type
	sourceType := fetcher.SourceTypeBlob

	meta := &BlobMeta{
		BlobHash:   blobHash,
		RepoURL:    repoURL,
		Branch:     branch,
		SourceType: sourceType,
	}

	// Extract client ID from gRPC metadata if available
	clientID := "default"
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if ids := md.Get("x-client-id"); len(ids) > 0 {
			clientID = ids[0]
		}
	}

	// RecordAccess handles prediction and prefetching internally
	s.predictor.RecordAccess(ctx, storageID, filePath, clientID, meta)
}
