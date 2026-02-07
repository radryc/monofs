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
	"google.golang.org/grpc"
)

// updateDirectoryIndexHierarchy updates ALL parent directories in the path hierarchy.
//
// HYBRID APPROACH: This function is used for:
// - Single file operations (IngestFile) - provides immediate directory consistency
// - Real-time incremental updates during individual file writes
//
// It is NOT used during:
// - Batch operations (IngestFileBatch) - deferred to BuildDirectoryIndexes for performance
// - Bulk ingestion - BuildDirectoryIndexes is called after all files are ingested
//
// This hybrid approach balances:
// - Immediate consistency for single-file operations
// - High performance for bulk operations
//
// For example, for file "cmd/thanos/main.go":
// - Updates "" (root) to include "cmd" as a directory
// - Updates "cmd" to include "thanos" as a directory
// - Updates "cmd/thanos" to include "main.go" as a file
func (s *Server) updateDirectoryIndexHierarchy(tx *nutsdb.Tx, storageID, filePath string, fileHashKey []byte, mode uint32, size uint64, mtime int64) error {
	// Build list of all directories in the path
	parts := strings.Split(filePath, "/")

	// Update each parent directory to include its child (either directory or file)
	for i := 0; i < len(parts); i++ {
		var dirPath string
		var entryName string
		var isDir bool

		if i == 0 {
			// Root directory - add first component
			dirPath = ""
			entryName = parts[0]
			isDir = (i < len(parts)-1) // It's a directory if not the last component
		} else {
			// Nested directory
			dirPath = strings.Join(parts[:i], "/")
			entryName = parts[i]
			isDir = (i < len(parts)-1)
		}

		// Get or create directory index
		dirIndexKey := makeDirIndexKey(storageID, dirPath)
		var dirIndex []dirIndexEntry

		existingValue, err := tx.Get(bucketDirIndex, dirIndexKey)
		if err == nil {
			if err := json.Unmarshal(existingValue, &dirIndex); err != nil {
				s.logger.Warn("failed to unmarshal dir index, creating new",
					"dir_path", dirPath, "error", err)
				dirIndex = []dirIndexEntry{}
			}
		}

		// Check if entry already exists
		found := false
		for j, entry := range dirIndex {
			if entry.Name == entryName {
				if !isDir {
					// Update existing file entry
					dirIndex[j] = dirIndexEntry{
						Name:    entryName,
						Mode:    mode,
						Size:    size,
						Mtime:   mtime,
						HashKey: string(fileHashKey),
						IsDir:   false,
					}
				} else {
					// Update directory Mtime if current file is newer
					if mtime > entry.Mtime {
						dirIndex[j].Mtime = mtime
					}
				}
				found = true
				break
			}
		}

		if !found {
			// Add new entry
			entry := dirIndexEntry{
				Name:  entryName,
				IsDir: isDir,
				Mtime: mtime, // Use file's mtime for both files and directories
			}

			if !isDir {
				// It's a file - include metadata
				entry.Mode = mode
				entry.Size = size
				entry.HashKey = string(fileHashKey)
			} else {
				// It's a directory - use default directory mode
				entry.Mode = 0755 | uint32(syscall.S_IFDIR)
			}

			dirIndex = append(dirIndex, entry)
		}

		// Store updated directory index
		dirIndexValue, err := json.Marshal(dirIndex)
		if err != nil {
			return fmt.Errorf("marshal dir index for %q: %w", dirPath, err)
		}
		if err := tx.Put(bucketDirIndex, dirIndexKey, dirIndexValue, 0); err != nil {
			return fmt.Errorf("store dir index for %q: %w", dirPath, err)
		}

		s.logger.Debug("updated directory index",
			"storage_id", storageID,
			"dir_path", dirPath,
			"entry_name", entryName,
			"is_dir", isDir,
			"total_entries", len(dirIndex))
	}

	return nil
}

// checkVirtualDirectory checks if a path exists as a virtual directory in the directory index.
// Virtual directories are created automatically when files are stored in subdirectories.
func (s *Server) checkVirtualDirectory(storageID, dirPath string) *pb.LookupResponse {
	// Check parent directory to see if this path exists as a directory entry
	parentDir := extractDirPath(dirPath)
	entryName := extractFileName(dirPath)

	dirIndexKey := makeDirIndexKey(storageID, parentDir)

	s.logger.Debug("checking virtual directory",
		"storage_id", storageID,
		"dir_path", dirPath,
		"parent_dir", parentDir,
		"entry_name", entryName,
		"dir_index_key", string(dirIndexKey))

	var foundEntry *dirIndexEntry
	err := s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, dirIndexKey)
		if err != nil {
			s.logger.Debug("dir index not found for parent",
				"parent_dir", parentDir,
				"error", err)
			return err
		}

		var dirIndex []dirIndexEntry
		if err := json.Unmarshal(value, &dirIndex); err != nil {
			s.logger.Warn("failed to unmarshal dir index",
				"parent_dir", parentDir,
				"error", err)
			return err
		}

		s.logger.Debug("checking dir index entries",
			"parent_dir", parentDir,
			"entry_count", len(dirIndex),
			"looking_for", entryName)

		// Look for this entry in the parent directory
		for i, entry := range dirIndex {
			if entry.Name == entryName {
				s.logger.Debug("found matching entry",
					"entry_name", entry.Name,
					"is_dir", entry.IsDir)
				if entry.IsDir {
					foundEntry = &dirIndex[i]
					return nil
				}
				return fmt.Errorf("entry exists but is not a directory")
			}
		}

		s.logger.Debug("entry not found in dir index",
			"looking_for", entryName,
			"entries", dirIndex)
		return fmt.Errorf("not found")
	})

	if err == nil && foundEntry != nil {
		s.logger.Debug("found virtual directory",
			"storage_id", storageID,
			"dir_path", dirPath,
			"parent_dir", parentDir,
			"entry_name", entryName,
			"mtime", foundEntry.Mtime)

		// Use stored Mtime, fallback to now if not set (for backwards compatibility)
		mtime := foundEntry.Mtime
		if mtime == 0 {
			mtime = time.Now().Unix()
		}

		return &pb.LookupResponse{
			Ino:   hashPath(storageID + ":" + dirPath),
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: mtime,
			Found: true,
		}
	}

	s.logger.Debug("virtual directory not found",
		"storage_id", storageID,
		"dir_path", dirPath,
		"error", err)
	return nil
}

// ReadDir implements the ReadDir RPC (streaming).
func (s *Server) ReadDir(req *pb.ReadDirRequest, stream grpc.ServerStreamingServer[pb.DirEntry]) error {
	startTime := time.Now()
	path := req.Path
	s.logger.Debug("readdir started", "path", path)

	// Handle root directory - list all top-level directories (e.g., "github.com", "go-modules")
	if path == "" {
		topLevelDirs := make(map[string]bool)
		repoCount := 0

		s.db.View(func(tx *nutsdb.Tx) error {
			// Use bucketRepoLookup (display_path → storage_id), not bucketRepos
			// This is critical for showing virtual repos like go-modules/
			keys, err := tx.GetKeys(bucketRepoLookup)
			if err != nil {
				return nil // Empty is okay
			}

			repoCount = len(keys)

			// Process display paths directly from keys (no need to unmarshal)
			for _, key := range keys {
				displayPath := string(key)
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
			return nil
		})

		if repoCount == 0 {
			elapsed := time.Since(startTime)
			s.logger.Debug("readdir completed (root, empty)",
				"path", path,
				"repos", 0,
				"duration_ms", elapsed.Milliseconds())
			return nil
		}

		// Send top-level directories
		for dir := range topLevelDirs {
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

		s.db.View(func(tx *nutsdb.Tx) error {
			// Use bucketRepoLookup (display_path → storage_id)
			keys, err := tx.GetKeys(bucketRepoLookup)
			if err != nil {
				return nil
			}

			// Process display paths directly from keys
			for _, key := range keys {
				displayPath := string(key)
				if strings.HasPrefix(displayPath, pathPrefix) {
					remainder := strings.TrimPrefix(displayPath, pathPrefix)
					if idx := strings.Index(remainder, "/"); idx > 0 {
						intermediateDirs[remainder[:idx]] = true
					} else {
						intermediateDirs[remainder] = true
					}
				}
			}
			return nil
		})

		for dir := range intermediateDirs {
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

	// Use directory index for O(1) directory listing
	dirIndexKey := makeDirIndexKey(storageID, filePath)

	s.logger.Debug("readdir using directory index",
		"path", path,
		"storage_id", storageID,
		"file_path", filePath,
		"dir_index_key", string(dirIndexKey))

	var dirIndex []dirIndexEntry
	var sentEntries int
	dbStartTime := time.Now()

	err := s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, dirIndexKey)
		if err != nil {
			// Directory index not found - this could be a new directory or error
			s.logger.Debug("directory index not found",
				"path", path,
				"dir_index_key", string(dirIndexKey),
				"error", err)
			return err
		}

		if err := json.Unmarshal(value, &dirIndex); err != nil {
			s.logger.Error("failed to unmarshal directory index",
				"path", path,
				"error", err)
			return err
		}

		return nil
	})

	if err != nil {
		// Fallback: directory might be empty or index not built yet
		elapsed := time.Since(startTime)
		s.logger.Debug("readdir completed (empty or no index)",
			"path", path,
			"entries", 0,
			"duration_ms", elapsed.Milliseconds())
		return nil
	}

	// Stream directory entries from index
	for _, entry := range dirIndex {
		mode := entry.Mode
		if entry.IsDir {
			mode = mode | uint32(syscall.S_IFDIR)
		} else {
			mode = mode | uint32(syscall.S_IFREG)
		}

		s.logger.Debug("readdir sending entry from index",
			"path", path,
			"name", entry.Name,
			"mode", fmt.Sprintf("0%o", mode),
			"size", entry.Size,
			"is_dir", entry.IsDir)

		stream.Send(&pb.DirEntry{
			Name: entry.Name,
			Mode: mode,
			Ino:  hashPath(path + "/" + entry.Name),
		})
		sentEntries++
	}

	elapsed := time.Since(dbStartTime)
	s.logger.Debug("readdir completed (from index)",
		"path", path,
		"entries_sent", sentEntries,
		"duration_ms", elapsed.Milliseconds())

	return nil
}

// BuildDirectoryIndexes builds directory indexes for all files in a repository.
// This is called after ingestion completes to improve ingestion performance.
func (s *Server) BuildDirectoryIndexes(ctx context.Context, req *pb.BuildDirectoryIndexesRequest) (*pb.BuildDirectoryIndexesResponse, error) {
	storageID := req.StorageId

	s.logger.Info("building directory indexes", "storage_id", storageID)

	// Collect all file paths for this repository by scanning owned files
	var fileMetadata []storedMetadata
	prefix := []byte(storageID + ":")

	s.logger.Info("scanning for files", "prefix", string(prefix), "bucket", bucketOwnedFiles)

	err := s.db.View(func(tx *nutsdb.Tx) error {
		// Use PrefixScan to get all keys with this storageID prefix
		// Note: PrefixScan returns values, but we stored "1" as value and key contains the path
		// So we need PrefixScanEntries to get keys
		keys, _, err := tx.PrefixScanEntries(bucketOwnedFiles, prefix, "", 0, -1, true, false)
		if err != nil && err != nutsdb.ErrBucketNotFound && err != nutsdb.ErrPrefixScan {
			s.logger.Error("prefix scan failed", "error", err)
			return err
		}

		s.logger.Info("prefix scan result", "keys_count", len(keys), "error", err)

		for _, key := range keys {
			// Extract file path from key (key format: "storageID:filePath")
			keyStr := string(key)
			filePath := strings.TrimPrefix(keyStr, storageID+":")

			// Get metadata for this file
			hashKey := makeStorageKey(storageID, filePath)
			metaEntry, err := tx.Get(bucketMetadata, hashKey)
			if err != nil {
				s.logger.Warn("missing metadata for file", "file_path", filePath, "error", err)
				continue
			}

			var meta storedMetadata
			if err := json.Unmarshal(metaEntry, &meta); err != nil {
				s.logger.Warn("failed to unmarshal metadata", "file_path", filePath, "error", err)
				continue
			}

			fileMetadata = append(fileMetadata, meta)
		}

		return nil
	})

	if err != nil {
		return &pb.BuildDirectoryIndexesResponse{
			Success: false,
			Message: err.Error(),
		}, err
	}

	s.logger.Info("found files to index", "count", len(fileMetadata), "storage_id", storageID)

	// Build directory index mapping: dirPath -> []entries
	dirMap := make(map[string][]dirIndexEntry)

	for _, meta := range fileMetadata {
		hashKey := makeStorageKey(storageID, meta.FilePath)

		// Get all parent directories
		parts := strings.Split(meta.FilePath, "/")

		for i := 0; i < len(parts); i++ {
			var dirPath string
			var entryName string
			var isDir bool

			if i == 0 {
				dirPath = ""
				entryName = parts[0]
				isDir = (i < len(parts)-1)
			} else {
				dirPath = strings.Join(parts[:i], "/")
				entryName = parts[i]
				isDir = (i < len(parts)-1)
			}

			// Check if entry already exists in this directory
			entries := dirMap[dirPath]
			found := false
			for j, entry := range entries {
				if entry.Name == entryName {
					if !isDir {
						// Update existing file entry
						entries[j] = dirIndexEntry{
							Name:    entryName,
							Mode:    meta.Mode,
							Size:    meta.Size,
							Mtime:   meta.Mtime,
							HashKey: string(hashKey),
							IsDir:   false,
						}
					} else {
						// Update directory Mtime if current file is newer
						if meta.Mtime > entry.Mtime {
							entries[j].Mtime = meta.Mtime
						}
					}
					found = true
					break
				}
			}

			if !found {
				// Add new entry
				entry := dirIndexEntry{
					Name:  entryName,
					IsDir: isDir,
					Mtime: meta.Mtime, // Use file's mtime for both files and directories
				}

				if !isDir {
					entry.Mode = meta.Mode
					entry.Size = meta.Size
					entry.HashKey = string(hashKey)
				} else {
					entry.Mode = 0755 | uint32(syscall.S_IFDIR)
				}

				entries = append(entries, entry)
			}

			dirMap[dirPath] = entries
		}
	}

	// Write all directory indexes to database
	dirsIndexed := int64(0)
	err = s.db.Update(func(tx *nutsdb.Tx) error {
		for dirPath, entries := range dirMap {
			dirIndexKey := makeDirIndexKey(storageID, dirPath)
			dirIndexValue, err := json.Marshal(entries)
			if err != nil {
				return fmt.Errorf("marshal dir index for %q: %w", dirPath, err)
			}

			if err := tx.Put(bucketDirIndex, dirIndexKey, dirIndexValue, 0); err != nil {
				return fmt.Errorf("store dir index for %q: %w", dirPath, err)
			}

			dirsIndexed++
		}
		return nil
	})

	if err != nil {
		return &pb.BuildDirectoryIndexesResponse{
			Success:            false,
			DirectoriesIndexed: dirsIndexed,
			Message:            err.Error(),
		}, err
	}

	s.logger.Info("directory indexes built successfully",
		"storage_id", storageID,
		"directories_indexed", dirsIndexed,
		"files_processed", len(fileMetadata))

	return &pb.BuildDirectoryIndexesResponse{
		Success:            true,
		DirectoriesIndexed: dirsIndexed,
		Message:            fmt.Sprintf("Successfully indexed %d directories", dirsIndexed),
	}, nil
}
