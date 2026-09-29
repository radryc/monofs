package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/metastore"
	"google.golang.org/grpc"
)

func inferDirectoryMode(mode uint32) uint32 {
	if mode&0222 == 0 {
		return 0555 | uint32(syscall.S_IFDIR)
	}
	return 0755 | uint32(syscall.S_IFDIR)
}

func normalizeExplicitDirectoryMode(mode uint32) uint32 {
	perm := mode & 0777
	if perm == 0 {
		perm = inferDirectoryMode(mode) & 0777
	}
	return perm | uint32(syscall.S_IFDIR)
}

func (s *Server) upsertDirectoryMetadata(tx metastore.Tx, storageID, dirPath string, mode uint32, mtime int64, explicit bool) error {
	if dirPath == "" {
		return nil
	}

	meta := dirMetadata{
		Path:     dirPath,
		Mode:     inferDirectoryMode(mode),
		Mtime:    mtime,
		Explicit: explicit,
	}
	if explicit {
		meta.Mode = normalizeExplicitDirectoryMode(mode)
	}

	key := makeDirMetaKey(storageID, dirPath)
	if existingValue, err := tx.Get(bucketDirMeta, key); err == nil {
		var existing dirMetadata
		if err := json.Unmarshal(existingValue, &existing); err == nil {
			if existing.Path != "" {
				meta.Path = existing.Path
			}
			if existing.Mtime > meta.Mtime {
				meta.Mtime = existing.Mtime
			}
			if existing.Explicit {
				meta.Explicit = true
				if !explicit {
					meta.Mode = existing.Mode
				}
			}
		}
	}

	value, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal dir metadata for %q: %w", dirPath, err)
	}
	if err := tx.Put(bucketDirMeta, key, value, 0); err != nil {
		return fmt.Errorf("store dir metadata for %q: %w", dirPath, err)
	}
	return nil
}

func (s *Server) upsertDirectoryHierarchy(tx metastore.Tx, storageID, filePath string, mode uint32, mtime int64, explicitLeafDir bool) error {
	parts := strings.Split(filePath, "/")
	lastDirPart := len(parts) - 2
	if explicitLeafDir {
		lastDirPart = len(parts) - 1
	}
	for i := 0; i <= lastDirPart; i++ {
		if i < 0 {
			continue
		}
		dirPath := strings.Join(parts[:i+1], "/")
		if dirPath == "" {
			continue
		}
		if err := s.upsertDirectoryMetadata(tx, storageID, dirPath, mode, mtime, explicitLeafDir && i == len(parts)-1); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) getDirectoryMetadataTx(tx metastore.Tx, storageID, dirPath string) (*dirMetadata, error) {
	value, err := tx.Get(bucketDirMeta, makeDirMetaKey(storageID, dirPath))
	if err != nil {
		return nil, err
	}
	var meta dirMetadata
	if err := json.Unmarshal(value, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (s *Server) lookupCanonicalDirectory(storageID, dirPath string) *pb.LookupResponse {
	if dirPath == "" {
		return nil
	}

	var meta *dirMetadata
	err := s.db.View(func(tx metastore.Tx) error {
		var err error
		meta, err = s.getDirectoryMetadataTx(tx, storageID, dirPath)
		return err
	})
	if err != nil || meta == nil {
		return nil
	}

	mtime := meta.Mtime
	if mtime == 0 {
		mtime = time.Now().Unix()
	}

	return &pb.LookupResponse{
		Ino:   hashPath(storageID + ":" + dirPath),
		Mode:  meta.Mode,
		Size:  0,
		Mtime: mtime,
		Found: true,
	}
}

func (s *Server) lookupDirectorySummaryFile(storageID, filePath string) *pb.LookupResponse {
	parentDir := extractDirPath(filePath)
	entryName := extractFileName(filePath)

	var (
		entry dirIndexEntry
		found bool
	)
	err := s.db.View(func(tx metastore.Tx) error {
		var err error
		entry, found, err = getDirEntry(tx, storageID, parentDir, entryName)
		return err
	})
	if err != nil || !found || entry.IsDir {
		return nil
	}

	mtime := entry.Mtime
	if mtime == 0 {
		mtime = time.Now().Unix()
	}
	mode := entry.Mode
	if mode&uint32(syscall.S_IFMT) == 0 {
		mode |= uint32(syscall.S_IFREG)
	}

	return &pb.LookupResponse{
		Ino:   hashPath(storageID + ":" + filePath),
		Mode:  mode,
		Size:  entry.Size,
		Mtime: mtime,
		Found: true,
	}
}

func (s *Server) pruneImplicitDirectories(tx metastore.Tx, storageID, startDir string) error {
	for dirPath := startDir; dirPath != ""; dirPath = extractDirPath(dirPath) {
		meta, err := s.getDirectoryMetadataTx(tx, storageID, dirPath)
		if err != nil {
			continue
		}
		if meta.Explicit {
			return nil
		}

		hasChildren, err := hasDirEntries(tx, storageID, dirPath)
		if err != nil {
			return err
		}
		if hasChildren {
			return nil
		}

		if err := tx.Delete(bucketDirMeta, makeDirMetaKey(storageID, dirPath)); err != nil && err != metastore.ErrKeyNotFound {
			return err
		}
		parentDir := extractDirPath(dirPath)
		entryName := extractFileName(dirPath)
		if err := s.removeFromDirectoryIndex(tx, storageID, parentDir, entryName); err != nil {
			return err
		}
	}
	return nil
}

// updateDirectoryIndexHierarchy writes the directory entries for every ancestor
// of filePath (and the file's own entry) as individual records. Unlike the
// previous array-per-directory model this never rewrites a directory's whole
// listing, so cost is O(path depth).
func (s *Server) updateDirectoryIndexHierarchy(tx metastore.Tx, storageID, filePath string, fileHashKey []byte, mode uint32, size uint64, mtime int64, explicitLeafDir bool) error {
	if err := s.upsertDirectoryHierarchy(tx, storageID, filePath, mode, mtime, explicitLeafDir); err != nil {
		return err
	}
	pending := make(map[string]dirIndexEntry, 4)
	accumulateDirectoryIndexEntry(pending, storageID, filePath, mode, size, mtime, explicitLeafDir, string(fileHashKey))
	return flushDirectoryIndexEntries(tx, pending)
}

// checkVirtualDirectory checks if a path exists as a virtual directory in the directory index.
// Virtual directories are created automatically when files are stored in subdirectories.
func (s *Server) checkVirtualDirectory(storageID, dirPath string) *pb.LookupResponse {
	parentDir := extractDirPath(dirPath)
	entryName := extractFileName(dirPath)

	var (
		entry dirIndexEntry
		found bool
	)
	err := s.db.View(func(tx metastore.Tx) error {
		var err error
		entry, found, err = getDirEntry(tx, storageID, parentDir, entryName)
		return err
	})
	if err != nil || !found || !entry.IsDir {
		return nil
	}

	mtime := entry.Mtime
	if mtime == 0 {
		mtime = time.Now().Unix()
	}
	return &pb.LookupResponse{
		Ino:   hashPath(storageID + ":" + dirPath),
		Mode:  entry.Mode,
		Size:  0,
		Mtime: mtime,
		Found: true,
	}
}

// checkVirtualFile checks if a file path exists as a file entry in the directory index.
// This handles the case where a file's metadata may not be in bucketMetadata but the
// file is listed in its parent's directory index (e.g., after overlay cleanup before
// full metadata ingestion is complete).
func (s *Server) checkVirtualFile(storageID, filePath string) *pb.LookupResponse {
	parentDir := extractDirPath(filePath)
	entryName := extractFileName(filePath)

	var (
		entry dirIndexEntry
		found bool
	)
	err := s.db.View(func(tx metastore.Tx) error {
		var err error
		entry, found, err = getDirEntry(tx, storageID, parentDir, entryName)
		return err
	})
	if err != nil || !found || entry.IsDir {
		return nil
	}

	mtime := entry.Mtime
	if mtime == 0 {
		mtime = time.Now().Unix()
	}
	mode := entry.Mode
	if mode&uint32(syscall.S_IFMT) == 0 {
		mode |= uint32(syscall.S_IFREG)
	}
	return &pb.LookupResponse{
		Ino:   hashPath(storageID + ":" + filePath),
		Mode:  mode,
		Size:  entry.Size,
		Mtime: mtime,
		Found: true,
	}
}

// ReadDir implements the ReadDir RPC (streaming).
func (s *Server) ReadDir(req *pb.ReadDirRequest, stream grpc.ServerStreamingServer[pb.DirEntry]) error {
	startTime := time.Now()
	path := req.Path
	s.logger.Debug("readdir started", "path", path)

	// Handle root directory - list all top-level directories (e.g., "github.com")
	if path == "" {
		topLevelDirs := make(map[string]bool)
		for _, dir := range managedNamespaceEntries(path) {
			topLevelDirs[dir] = true
		}
		repoCount := 0

		s.db.View(func(tx metastore.Tx) error {
			// Use GetKeys first to get only keys (lighter), then batch Get values
			keys, err := tx.GetKeys(bucketRepos)
			if err != nil {
				return nil // Empty is okay
			}

			repoCount = len(keys)

			// Process in batches of 100
			const batchSize = 100
			for batchStart := 0; batchStart < len(keys); batchStart += batchSize {
				batchEnd := batchStart + batchSize
				if batchEnd > len(keys) {
					batchEnd = len(keys)
				}

				for i := batchStart; i < batchEnd; i++ {
					value, err := tx.Get(bucketRepos, keys[i])
					if err != nil {
						continue
					}

					var info repoInfo
					if err := json.Unmarshal(value, &info); err != nil {
						continue
					}

					displayPath := info.DisplayPath
					if idx := strings.Index(displayPath, "/"); idx > 0 {
						topLevelDirs[displayPath[:idx]] = true
					} else {
						// Repo without slash, show as-is
						stream.Send(&pb.DirEntry{
							Name: displayPath,
							Mode: 0755 | uint32(syscall.S_IFDIR),
							Ino:  hashPath(displayPath),
						})
					}
				}
			}
			return nil
		})

		if repoCount == 0 && len(topLevelDirs) == 0 {
			elapsed := time.Since(startTime)
			s.logger.Debug("readdir completed (root, empty)",
				"path", path,
				"repos", 0,
				"duration_ms", elapsed.Milliseconds())
			return nil
		}

		// Send top-level directories
		// Collect and sort entries for deterministic ordering
		dirs := make([]string, 0, len(topLevelDirs))
		for dir := range topLevelDirs {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			stream.Send(&pb.DirEntry{
				Name: dir,
				Mode: 0755 | uint32(syscall.S_IFDIR),
				Ino:  hashPath(dir),
			})
		}

		elapsed := time.Since(startTime)
		s.logger.Debug("readdir completed (root)",
			"path", path,
			"repos", repoCount,
			"top_level_dirs", len(topLevelDirs),
			"duration_ms", elapsed.Milliseconds())
		return nil
	}

	// Resolve path to (storageID, filePath)
	storageID, filePath, ok := s.resolvePathToStorage(path)

	// If no matching repo found, treat as intermediate directory
	if !ok {
		pathPrefix := path + "/"
		intermediateDirs := make(map[string]bool)
		for _, dir := range managedNamespaceEntries(path) {
			intermediateDirs[dir] = true
		}

		s.db.View(func(tx metastore.Tx) error {
			// Use GetKeys first, then batch Get values
			keys, err := tx.GetKeys(bucketRepos)
			if err != nil {
				return nil
			}

			// Process in batches of 100
			const batchSize = 100
			for batchStart := 0; batchStart < len(keys); batchStart += batchSize {
				batchEnd := batchStart + batchSize
				if batchEnd > len(keys) {
					batchEnd = len(keys)
				}

				for i := batchStart; i < batchEnd; i++ {
					value, err := tx.Get(bucketRepos, keys[i])
					if err != nil {
						continue
					}

					var info repoInfo
					if err := json.Unmarshal(value, &info); err != nil {
						continue
					}

					displayPath := info.DisplayPath
					if strings.HasPrefix(displayPath, pathPrefix) {
						remainder := strings.TrimPrefix(displayPath, pathPrefix)
						if idx := strings.Index(remainder, "/"); idx > 0 {
							intermediateDirs[remainder[:idx]] = true
						} else {
							intermediateDirs[remainder] = true
						}
					}
				}
			}
			return nil
		})

		// Collect and sort entries for deterministic ordering
		dirs := make([]string, 0, len(intermediateDirs))
		for dir := range intermediateDirs {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			stream.Send(&pb.DirEntry{
				Name: dir,
				Mode: 0755 | uint32(syscall.S_IFDIR),
				Ino:  hashPath(path + "/" + dir),
			})
		}

		elapsed := time.Since(startTime)
		s.logger.Debug("readdir completed (intermediate dir)",
			"path", path,
			"entries", len(intermediateDirs),
			"duration_ms", elapsed.Milliseconds())
		return nil
	}

	if resolved, handled, err := s.resolveKVSPath(stream.Context(), storageID, filePath); err != nil {
		return err
	} else if handled {
		if resolved == nil || !resolved.isDir {
			return nil
		}
		// Emit in name order so clients can merge node listings with a heap.
		sort.Slice(resolved.entries, func(i, j int) bool { return resolved.entries[i].Name < resolved.entries[j].Name })
		for _, entry := range resolved.entries {
			entryLogicalPath := kvsChildLogicalPath(resolved.logicalPath, entry.Name)
			if err := stream.Send(&pb.DirEntry{
				Name: entry.Name,
				Mode: kvsMode(entry.IsDir),
				Ino:  hashPath(strings.TrimPrefix(entryLogicalPath, "/")),
			}); err != nil {
				return err
			}
		}
		return nil
	}
	if resolved, handled, err := s.resolveCfgPath(stream.Context(), storageID, filePath); err != nil {
		return err
	} else if handled {
		if resolved == nil || !resolved.isDir {
			return nil
		}
		// Emit in name order so clients can merge node listings with a heap.
		sort.Slice(resolved.entries, func(i, j int) bool { return resolved.entries[i].Name < resolved.entries[j].Name })
		for _, entry := range resolved.entries {
			entryLogicalPath := kvsChildLogicalPath(resolved.logicalPath, entry.Name)
			if err := stream.Send(&pb.DirEntry{
				Name: entry.Name,
				Mode: cfgMode(entry.IsDir),
				Ino:  hashPath(strings.TrimPrefix(entryLogicalPath, "/")),
			}); err != nil {
				return err
			}
		}
		return nil
	}

	// Stream the directory's children directly from the per-entry index. This
	// never materializes the whole listing, so a directory with millions of
	// entries is consumed incrementally.
	sentEntries := 0
	dbStartTime := time.Now()
	hasMeta := false
	startAfter := req.GetStartAfter()
	limit := int(req.GetLimit())

	err := s.db.View(func(tx metastore.Tx) error {
		if filePath != "" {
			if _, metaErr := s.getDirectoryMetadataTx(tx, storageID, filePath); metaErr == nil {
				hasMeta = true
			}
		}
		dirPrefix := dirEntryPrefix(storageID, filePath)
		lower := dirPrefix
		if startAfter != "" {
			// Resume from the start-after key; names before it are below the
			// lower bound and are not returned. The exact key is skipped below.
			lower = append(append([]byte(nil), dirPrefix...), startAfter...)
		}
		upper := prefixUpperBound(dirPrefix)
		return tx.ScanRange(bucketDirEntries, lower, upper, func(key, value []byte) error {
			name := dirEntryNameFromKey(key)
			if startAfter != "" && name == startAfter {
				return nil
			}
			entry, decErr := decodeDirEntry(name, value)
			if decErr != nil {
				return nil // skip corrupt record
			}
			mode := entry.Mode
			if entry.IsDir {
				mode = mode | uint32(syscall.S_IFDIR)
			} else {
				mode = mode | uint32(syscall.S_IFREG)
			}
			if sendErr := stream.Send(&pb.DirEntry{
				Name: entry.Name,
				Mode: mode,
				Ino:  hashPath(path + "/" + entry.Name),
			}); sendErr != nil {
				return sendErr
			}
			sentEntries++
			if limit > 0 && sentEntries >= limit {
				return metastore.ErrStopIteration
			}
			return nil
		})
	})
	if err != nil {
		return err
	}

	// Empty first page with no metadata: the directory may be owned/known only
	// by another node, so forward (guarded against loops).
	if sentEntries == 0 && startAfter == "" && !hasMeta && s.enableForwarding && !isAlreadyForwarded(stream.Context()) {
		targetNode := s.getTargetNode(storageID, filePath)
		if targetNode != nil && targetNode.ID != s.nodeID && s.isNodeHealthy(targetNode.ID) {
			return s.forwardReadDir(req, stream, targetNode)
		}
		if targetNode != nil && !s.isNodeHealthy(targetNode.ID) {
			for _, backup := range s.getBackupNodes(storageID, filePath) {
				if backup.ID == s.nodeID {
					break
				}
				if fwdErr := s.forwardReadDir(req, stream, backup); fwdErr == nil {
					return nil
				}
			}
		}
	}

	elapsed := time.Since(dbStartTime)
	s.logger.Debug("readdir completed (from entry index)",
		"path", path,
		"entries_sent", sentEntries,
		"duration_ms", elapsed.Milliseconds())
	return nil
}

// BuildDirectoryIndexes rebuilds the per-entry directory index for a repository
// from its canonical records. Normal ingestion maintains the index
// incrementally; this is a repair/rebuild tool. It pages across transactions so
// memory stays bounded regardless of repository size.
func (s *Server) BuildDirectoryIndexes(ctx context.Context, req *pb.BuildDirectoryIndexesRequest) (*pb.BuildDirectoryIndexesResponse, error) {
	storageID := req.StorageId
	s.logger.Info("building directory indexes", "storage_id", storageID)

	const pageSize = 10000
	prefix := []byte(storageID + ":")
	upper := prefixUpperBound(prefix)
	var after []byte // exclusive resume cursor over bucketOwnedFiles
	var filesIndexed int64

	for {
		pending := make(map[string]dirIndexEntry, pageSize)
		var last []byte
		count := 0

		viewErr := s.db.View(func(tx metastore.Tx) error {
			lower := prefix
			if after != nil {
				lower = after
			}
			return tx.ScanRange(bucketOwnedFiles, lower, upper, func(key, value []byte) error {
				_, filePath, ok := splitOwnedFileKey(key)
				if !ok {
					return nil
				}
				metaKey := makeStorageKey(storageID, filePath)
				if hk, hErr := tx.Get(bucketPathIndex, key); hErr == nil {
					metaKey = hk
				}
				metaValue, err := tx.Get(bucketMetadata, metaKey)
				if err != nil {
					return nil
				}
				var meta storedMetadata
				if json.Unmarshal(metaValue, &meta) != nil {
					return nil
				}
				accumulateDirectoryIndexEntry(pending, storageID, filePath, meta.Mode, meta.Size, meta.Mtime, meta.IsDir, string(metaKey))
				last = append(last[:0], key...)
				count++
				if count >= pageSize {
					return metastore.ErrStopIteration
				}
				return nil
			})
		})
		if viewErr != nil {
			return &pb.BuildDirectoryIndexesResponse{Success: false, Message: viewErr.Error()}, viewErr
		}

		if len(pending) > 0 {
			if err := s.db.Update(func(tx metastore.Tx) error {
				return flushDirectoryIndexEntries(tx, pending)
			}); err != nil {
				return &pb.BuildDirectoryIndexesResponse{Success: false, Message: err.Error()}, err
			}
		}
		filesIndexed += int64(count)
		if count < pageSize || last == nil {
			break
		}
		after = append(last, 0x00)
	}

	s.logger.Info("directory index rebuild complete",
		"storage_id", storageID,
		"files_processed", filesIndexed)

	return &pb.BuildDirectoryIndexesResponse{
		Success:            true,
		DirectoriesIndexed: 0,
		Message:            fmt.Sprintf("Rebuilt directory index for %d files", filesIndexed),
	}, nil
}

// removeFromDirectoryIndex removes a single entry (file or subdirectory) from a
// parent directory's index. Must be called within a write transaction.
func (s *Server) removeFromDirectoryIndex(tx metastore.Tx, storageID, parentDir, entryName string) error {
	return deleteDirEntry(tx, storageID, parentDir, entryName)
}

// DeleteDirectoryRecursive removes a directory and all its contents from the node.
func (s *Server) DeleteDirectoryRecursive(ctx context.Context, req *pb.DeleteDirectoryRecursiveRequest) (*pb.DeleteDirectoryRecursiveResponse, error) {
	storageID := req.StorageId
	dirPath := req.DirPath
	if handledBackend := s.repositoryStorageBackend(storageID, ""); handledBackend == storageBackendKVS {
		filesDeleted, dirsDeleted, err := s.deleteKVSDirectory(ctx, storageID, dirPath)
		if err != nil {
			return nil, err
		}
		return &pb.DeleteDirectoryRecursiveResponse{
			Success:      true,
			Message:      fmt.Sprintf("Deleted %d files and %d directories", filesDeleted, dirsDeleted),
			FilesDeleted: filesDeleted,
			DirsDeleted:  dirsDeleted,
		}, nil
	}

	s.logger.Info("deleting directory recursively",
		"storage_id", storageID,
		"dir_path", dirPath)

	// Collect all paths to delete by walking the per-entry directory index
	// (read-only pass).
	var filePaths []string
	var dirPaths []string

	err := s.db.View(func(tx metastore.Tx) error {
		var walkDir func(path string) error
		walkDir = func(path string) error {
			entries, err := listDirEntries(tx, storageID, path)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				childPath := entry.Name
				if path != "" {
					childPath = path + "/" + entry.Name
				}
				if entry.IsDir {
					dirPaths = append(dirPaths, childPath)
					if err := walkDir(childPath); err != nil {
						return err
					}
				} else {
					filePaths = append(filePaths, childPath)
				}
			}
			return nil
		}
		dirPaths = append(dirPaths, dirPath)
		return walkDir(dirPath)
	})
	if err != nil {
		return nil, fmt.Errorf("walking directory tree: %w", err)
	}

	var filesDeleted, dirsDeleted int64

	err = s.db.Update(func(tx metastore.Tx) error {
		for _, fp := range filePaths {
			pathKey := []byte(storageID + ":" + fp)
			_ = tx.Delete(bucketMetadata, makeStorageKey(storageID, fp))
			_ = tx.Delete(bucketPathIndex, pathKey)
			_ = tx.Delete(bucketOwnedFiles, pathKey)
			_ = tx.Delete(bucketReplicaFiles, pathKey)
			filesDeleted++
		}

		for _, dp := range dirPaths {
			pathKey := []byte(storageID + ":" + dp)
			_ = tx.Delete(bucketMetadata, makeStorageKey(storageID, dp))
			_ = tx.Delete(bucketPathIndex, pathKey)
			_ = tx.Delete(bucketOwnedFiles, pathKey)
			_ = tx.Delete(bucketReplicaFiles, pathKey)
			_ = tx.Delete(bucketDirMeta, makeDirMetaKey(storageID, dp))
			if _, err := deleteDirEntries(tx, storageID, dp); err != nil {
				return err
			}
			dirsDeleted++
		}

		parentDir := extractDirPath(dirPath)
		entryName := extractFileName(dirPath)
		if err := s.removeFromDirectoryIndex(tx, storageID, parentDir, entryName); err != nil {
			s.logger.Warn("failed to remove dir from parent index", "dir_path", dirPath, "error", err)
		}
		if err := s.pruneImplicitDirectories(tx, storageID, parentDir); err != nil {
			s.logger.Warn("failed to prune parent directories", "dir_path", dirPath, "error", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("delete directory recursive: %w", err)
	}

	s.logger.Info("directory deleted recursively",
		"storage_id", storageID,
		"dir_path", dirPath,
		"files_deleted", filesDeleted,
		"dirs_deleted", dirsDeleted)

	return &pb.DeleteDirectoryRecursiveResponse{
		Success:      true,
		Message:      fmt.Sprintf("Deleted %d files and %d directories", filesDeleted, dirsDeleted),
		FilesDeleted: filesDeleted,
		DirsDeleted:  dirsDeleted,
	}, nil
}
