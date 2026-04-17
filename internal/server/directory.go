package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
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

func (s *Server) upsertDirectoryMetadata(tx *nutsdb.Tx, storageID, dirPath string, mode uint32, mtime int64, explicit bool) error {
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

func (s *Server) upsertDirectoryHierarchy(tx *nutsdb.Tx, storageID, filePath string, mode uint32, mtime int64, explicitLeafDir bool) error {
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

func (s *Server) getDirectoryMetadataTx(tx *nutsdb.Tx, storageID, dirPath string) (*dirMetadata, error) {
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
	err := s.db.View(func(tx *nutsdb.Tx) error {
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

func directChildName(parentDir, candidate string) (string, bool) {
	if candidate == "" || candidate == parentDir {
		return "", false
	}

	if parentDir == "" {
		if idx := strings.Index(candidate, "/"); idx >= 0 {
			return candidate[:idx], true
		}
		return candidate, true
	}

	prefix := parentDir + "/"
	if !strings.HasPrefix(candidate, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(candidate, prefix)
	if remainder == "" {
		return "", false
	}
	if idx := strings.Index(remainder, "/"); idx >= 0 {
		return remainder[:idx], true
	}
	return remainder, true
}

func storeDirectoryIndex(tx *nutsdb.Tx, storageID, dirPath string, entries []dirIndexEntry) error {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	value, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal dir index for %q: %w", dirPath, err)
	}
	return tx.Put(bucketDirIndex, makeDirIndexKey(storageID, dirPath), value, 0)
}

func normalizeSummaryEntry(entry dirIndexEntry) dirIndexEntry {
	if entry.IsDir {
		entry.Size = 0
		entry.HashKey = ""
		entry.Mode = normalizeExplicitDirectoryMode(entry.Mode)
	} else if entry.Mode&uint32(syscall.S_IFMT) != 0 {
		entry.Mode = entry.Mode & 0777
	}
	return entry
}

func storeDirectorySummary(tx *nutsdb.Tx, storageID, dirPath string, entries []dirIndexEntry) error {
	normalized := make([]dirIndexEntry, 0, len(entries))
	for _, entry := range entries {
		normalized = append(normalized, normalizeSummaryEntry(entry))
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].Name < normalized[j].Name
	})
	value, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("marshal dir summary for %q: %w", dirPath, err)
	}
	return tx.Put(bucketDirSummary, makeDirMetaKey(storageID, dirPath), value, 0)
}

func (s *Server) loadDirectorySummaryTx(tx *nutsdb.Tx, storageID, dirPath string) ([]dirIndexEntry, error) {
	value, err := tx.Get(bucketDirSummary, makeDirMetaKey(storageID, dirPath))
	if err != nil {
		return nil, err
	}
	var entries []dirIndexEntry
	if err := json.Unmarshal(value, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *Server) upsertDirectorySummaryEntry(tx *nutsdb.Tx, storageID, dirPath string, entry dirIndexEntry) error {
	entry = normalizeSummaryEntry(entry)
	entries, err := s.loadDirectorySummaryTx(tx, storageID, dirPath)
	if err != nil && err != nutsdb.ErrKeyNotFound {
		return fmt.Errorf("load dir summary for %q: %w", dirPath, err)
	}
	if err == nutsdb.ErrKeyNotFound {
		entries = nil
	}

	found := false
	for i := range entries {
		if entries[i].Name != entry.Name {
			continue
		}
		entries[i] = entry
		found = true
		break
	}
	if !found {
		entries = append(entries, entry)
	}

	return storeDirectorySummary(tx, storageID, dirPath, entries)
}

func (s *Server) removeFromDirectorySummary(tx *nutsdb.Tx, storageID, parentDir, entryName string) error {
	entries, err := s.loadDirectorySummaryTx(tx, storageID, parentDir)
	if err == nutsdb.ErrKeyNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load dir summary for %q: %w", parentDir, err)
	}

	filtered := entries[:0]
	for _, entry := range entries {
		if entry.Name != entryName {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == len(entries) {
		return nil
	}
	if len(filtered) == 0 {
		return tx.Delete(bucketDirSummary, makeDirMetaKey(storageID, parentDir))
	}
	return storeDirectorySummary(tx, storageID, parentDir, filtered)
}

func (s *Server) upsertPathIntoDirectorySummary(tx *nutsdb.Tx, storageID, filePath string, mode uint32, size uint64, mtime int64, explicitLeafDir bool, hashKey string) error {
	parts := strings.Split(filePath, "/")
	for i := 0; i < len(parts); i++ {
		var dirPath string
		var entryName string
		var isDir bool

		if i == 0 {
			dirPath = ""
			entryName = parts[0]
			isDir = (i < len(parts)-1) || explicitLeafDir
		} else {
			dirPath = strings.Join(parts[:i], "/")
			entryName = parts[i]
			isDir = (i < len(parts)-1) || (explicitLeafDir && i == len(parts)-1)
		}

		entry := dirIndexEntry{
			Name:  entryName,
			IsDir: isDir,
			Mtime: mtime,
		}
		if isDir {
			if explicitLeafDir && i == len(parts)-1 {
				entry.Mode = normalizeExplicitDirectoryMode(mode)
			} else {
				entry.Mode = inferDirectoryMode(mode)
			}
		} else {
			entry.Mode = mode
			entry.Size = size
			entry.HashKey = hashKey
		}
		if err := s.upsertDirectorySummaryEntry(tx, storageID, dirPath, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) buildDirectoryEntriesFromCanonical(tx *nutsdb.Tx, storageID, dirPath string) ([]dirIndexEntry, bool, error) {
	entries := make(map[string]dirIndexEntry)
	directoryExists := dirPath == ""

	if dirPath != "" {
		if _, err := s.getDirectoryMetadataTx(tx, storageID, dirPath); err == nil {
			directoryExists = true
		}
	}

	if summaryEntries, err := s.loadDirectorySummaryTx(tx, storageID, dirPath); err == nil {
		directoryExists = true
		for _, entry := range summaryEntries {
			entries[entry.Name] = normalizeSummaryEntry(entry)
		}
	} else if err != nutsdb.ErrKeyNotFound {
		return nil, false, err
	}

	prefix := storageID + ":"
	if dirPath != "" {
		prefix += dirPath + "/"
	}

	keys, _, err := tx.PrefixScanEntries(bucketPathIndex, []byte(prefix), "", 0, -1, true, false)
	if err != nil && err != nutsdb.ErrBucketNotFound && err != nutsdb.ErrPrefixScan {
		return nil, false, err
	}
	for _, key := range keys {
		fullPath := strings.TrimPrefix(string(key), storageID+":")
		if fullPath == dirPath {
			continue
		}

		remainder := strings.TrimPrefix(string(key), prefix)
		if dirPath == "" {
			remainder = fullPath
		}
		if remainder == "" {
			continue
		}

		hashKey, metaErr := tx.Get(bucketPathIndex, key)
		if metaErr != nil {
			continue
		}
		metaValue, metaErr := tx.Get(bucketMetadata, hashKey)
		if metaErr != nil {
			continue
		}
		var meta storedMetadata
		if err := json.Unmarshal(metaValue, &meta); err != nil {
			continue
		}

		directoryExists = true
		if idx := strings.Index(remainder, "/"); idx >= 0 {
			name := remainder[:idx]
			childPath := name
			if dirPath != "" {
				childPath = dirPath + "/" + name
			}
			entry := entries[name]
			entry.Name = name
			entry.IsDir = true
			if dirMeta, err := s.getDirectoryMetadataTx(tx, storageID, childPath); err == nil {
				entry.Mode = dirMeta.Mode
				if dirMeta.Mtime > entry.Mtime {
					entry.Mtime = dirMeta.Mtime
				}
			} else {
				entry.Mode = inferDirectoryMode(meta.Mode)
				if meta.Mtime > entry.Mtime {
					entry.Mtime = meta.Mtime
				}
			}
			entries[name] = entry
			continue
		}

		mode := meta.Mode
		if meta.IsDir {
			mode = normalizeExplicitDirectoryMode(meta.Mode)
		}
		entries[remainder] = dirIndexEntry{
			Name:    remainder,
			Mode:    mode,
			Size:    meta.Size,
			Mtime:   meta.Mtime,
			HashKey: string(hashKey),
			IsDir:   meta.IsDir,
		}
	}

	dirKeys, err := tx.GetKeys(bucketDirMeta)
	if err != nil && err != nutsdb.ErrBucketNotFound {
		return nil, directoryExists, err
	}
	for _, key := range dirKeys {
		keyStr := string(key)
		if !strings.HasPrefix(keyStr, storageID+":") {
			continue
		}
		candidate := strings.TrimPrefix(keyStr, storageID+":")
		if candidate == "" {
			continue
		}

		childName, ok := directChildName(dirPath, candidate)
		if !ok {
			continue
		}

		value, err := tx.Get(bucketDirMeta, key)
		if err != nil {
			continue
		}
		var meta dirMetadata
		if err := json.Unmarshal(value, &meta); err != nil {
			continue
		}
		entry := entries[childName]
		entry.Name = childName
		entry.IsDir = true
		entry.Mode = meta.Mode
		if meta.Mtime > entry.Mtime {
			entry.Mtime = meta.Mtime
		}
		entries[childName] = entry
		if meta.Path == dirPath {
			directoryExists = true
		}
	}

	result := make([]dirIndexEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result, directoryExists, nil
}

func (s *Server) rebuildDirectoryIndexFromCanonical(storageID, dirPath string) ([]dirIndexEntry, bool, error) {
	var (
		entries []dirIndexEntry
		found   bool
	)
	err := s.db.Update(func(tx *nutsdb.Tx) error {
		var err error
		entries, found, err = s.buildDirectoryEntriesFromCanonical(tx, storageID, dirPath)
		if err != nil || !found {
			return err
		}
		if err := storeDirectorySummary(tx, storageID, dirPath, entries); err != nil {
			return err
		}
		return storeDirectoryIndex(tx, storageID, dirPath, entries)
	})
	return entries, found, err
}

func (s *Server) lookupDirectorySummaryFile(storageID, filePath string) *pb.LookupResponse {
	parentDir := extractDirPath(filePath)
	entryName := extractFileName(filePath)

	var foundEntry *dirIndexEntry
	err := s.db.View(func(tx *nutsdb.Tx) error {
		entries, err := s.loadDirectorySummaryTx(tx, storageID, parentDir)
		if err != nil {
			return err
		}
		for i, entry := range entries {
			if entry.Name == entryName && !entry.IsDir {
				foundEntry = &entries[i]
				return nil
			}
		}
		return fmt.Errorf("not found")
	})
	if err != nil || foundEntry == nil {
		return nil
	}

	mtime := foundEntry.Mtime
	if mtime == 0 {
		mtime = time.Now().Unix()
	}
	mode := foundEntry.Mode
	if mode&uint32(syscall.S_IFMT) == 0 {
		mode |= uint32(syscall.S_IFREG)
	}

	return &pb.LookupResponse{
		Ino:   hashPath(storageID + ":" + filePath),
		Mode:  mode,
		Size:  foundEntry.Size,
		Mtime: mtime,
		Found: true,
	}
}

func (s *Server) pruneImplicitDirectories(tx *nutsdb.Tx, storageID, startDir string) error {
	for dirPath := startDir; dirPath != ""; dirPath = extractDirPath(dirPath) {
		meta, err := s.getDirectoryMetadataTx(tx, storageID, dirPath)
		if err != nil {
			continue
		}
		if meta.Explicit {
			return nil
		}

		entries, found, err := s.buildDirectoryEntriesFromCanonical(tx, storageID, dirPath)
		if err != nil {
			return err
		}
		if found && len(entries) > 0 {
			return nil
		}

		if err := tx.Delete(bucketDirMeta, makeDirMetaKey(storageID, dirPath)); err != nil && err != nutsdb.ErrKeyNotFound {
			return err
		}
		if err := tx.Delete(bucketDirSummary, makeDirMetaKey(storageID, dirPath)); err != nil && err != nutsdb.ErrKeyNotFound {
			return err
		}
		if err := tx.Delete(bucketDirIndex, makeDirIndexKey(storageID, dirPath)); err != nil && err != nutsdb.ErrKeyNotFound {
			return err
		}
		parentDir := extractDirPath(dirPath)
		entryName := extractFileName(dirPath)
		if err := s.removeFromDirectoryIndex(tx, storageID, parentDir, entryName); err != nil {
			return err
		}
		if err := s.removeFromDirectorySummary(tx, storageID, parentDir, entryName); err != nil {
			return err
		}
	}
	return nil
}

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
func (s *Server) updateDirectoryIndexHierarchy(tx *nutsdb.Tx, storageID, filePath string, fileHashKey []byte, mode uint32, size uint64, mtime int64, explicitLeafDir bool) error {
	if err := s.upsertDirectoryHierarchy(tx, storageID, filePath, mode, mtime, explicitLeafDir); err != nil {
		return err
	}
	if err := s.upsertPathIntoDirectorySummary(tx, storageID, filePath, mode, size, mtime, explicitLeafDir, string(fileHashKey)); err != nil {
		return err
	}

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
			isDir = (i < len(parts)-1) || explicitLeafDir
		} else {
			// Nested directory
			dirPath = strings.Join(parts[:i], "/")
			entryName = parts[i]
			isDir = (i < len(parts)-1) || (explicitLeafDir && i == len(parts)-1)
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
				if explicitLeafDir && i == len(parts)-1 {
					entry.Mode = normalizeExplicitDirectoryMode(mode)
				} else {
					entry.Mode = inferDirectoryMode(mode)
				}
			}

			dirIndex = append(dirIndex, entry)
		}

		// Sort entries by name for deterministic ordering
		sort.Slice(dirIndex, func(i, j int) bool {
			return dirIndex[i].Name < dirIndex[j].Name
		})

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
			Mode:  foundEntry.Mode,
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

// checkVirtualFile checks if a file path exists as a file entry in the directory index.
// This handles the case where a file's metadata may not be in bucketMetadata but the
// file is listed in its parent's directory index (e.g., after overlay cleanup before
// full metadata ingestion is complete).
func (s *Server) checkVirtualFile(storageID, filePath string) *pb.LookupResponse {
	// Check parent directory to see if this path exists as a file entry
	parentDir := extractDirPath(filePath)
	entryName := extractFileName(filePath)

	dirIndexKey := makeDirIndexKey(storageID, parentDir)

	s.logger.Debug("checking virtual file",
		"storage_id", storageID,
		"file_path", filePath,
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

		s.logger.Debug("checking dir index for file",
			"parent_dir", parentDir,
			"entry_count", len(dirIndex),
			"looking_for", entryName)

		// Look for this entry in the parent directory
		for i, entry := range dirIndex {
			if entry.Name == entryName {
				s.logger.Debug("found matching entry",
					"entry_name", entry.Name,
					"is_dir", entry.IsDir)
				if !entry.IsDir {
					foundEntry = &dirIndex[i]
					return nil
				}
				return fmt.Errorf("entry exists but is a directory, not a file")
			}
		}

		s.logger.Debug("file entry not found in dir index",
			"looking_for", entryName,
			"entries", dirIndex)
		return fmt.Errorf("not found")
	})

	if err == nil && foundEntry != nil {
		s.logger.Debug("found virtual file",
			"storage_id", storageID,
			"file_path", filePath,
			"parent_dir", parentDir,
			"entry_name", entryName,
			"size", foundEntry.Size,
			"mtime", foundEntry.Mtime)

		// Use stored Mtime, fallback to now if not set
		mtime := foundEntry.Mtime
		if mtime == 0 {
			mtime = time.Now().Unix()
		}

		// Ensure the mode has the regular file bit set
		mode := foundEntry.Mode
		if mode&uint32(syscall.S_IFMT) == 0 {
			mode = mode | uint32(syscall.S_IFREG)
		}

		return &pb.LookupResponse{
			Ino:   hashPath(storageID + ":" + filePath),
			Mode:  mode,
			Size:  foundEntry.Size,
			Mtime: mtime,
			Found: true,
		}
	}

	s.logger.Debug("virtual file not found",
		"storage_id", storageID,
		"file_path", filePath,
		"error", err)
	return nil
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

		s.db.View(func(tx *nutsdb.Tx) error {
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

		s.db.View(func(tx *nutsdb.Tx) error {
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
		rebuiltIndex, foundDir, rebuildErr := s.rebuildDirectoryIndexFromCanonical(storageID, filePath)
		if rebuildErr == nil && foundDir {
			for _, entry := range rebuiltIndex {
				mode := entry.Mode
				if entry.IsDir {
					mode = mode | uint32(syscall.S_IFDIR)
				} else {
					mode = mode | uint32(syscall.S_IFREG)
				}

				stream.Send(&pb.DirEntry{
					Name: entry.Name,
					Mode: mode,
					Ino:  hashPath(path + "/" + entry.Name),
				})
			}

			elapsed := time.Since(startTime)
			s.logger.Debug("readdir completed (rebuilt from canonical state)",
				"path", path,
				"entries", len(rebuiltIndex),
				"duration_ms", elapsed.Milliseconds())
			return nil
		}

		// Directory index not found locally — try forwarding to the correct node
		if s.enableForwarding && !isAlreadyForwarded(stream.Context()) {
			targetNode := s.getTargetNode(storageID, filePath)

			// Try primary node first if healthy
			if targetNode != nil && targetNode.ID != s.nodeID && s.isNodeHealthy(targetNode.ID) {
				s.logger.Debug("readdir forwarding to primary node",
					"path", path,
					"storage_id", storageID,
					"file_path", filePath,
					"target_node", targetNode.ID)
				return s.forwardReadDir(req, stream, targetNode)
			}

			// Primary is unhealthy, try backup nodes
			if targetNode != nil && !s.isNodeHealthy(targetNode.ID) {
				backupNodes := s.getBackupNodes(storageID, filePath)
				for _, backup := range backupNodes {
					if backup.ID == s.nodeID {
						break
					}
					s.logger.Debug("readdir forwarding to backup node",
						"path", path,
						"primary", targetNode.ID,
						"backup", backup.ID)
					fwdErr := s.forwardReadDir(req, stream, backup)
					if fwdErr == nil {
						return nil
					}
				}
			}
		}

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
	var directories []dirMetadata
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

		dirKeys, err := tx.GetKeys(bucketDirMeta)
		if err != nil && err != nutsdb.ErrBucketNotFound {
			return err
		}
		for _, key := range dirKeys {
			keyStr := string(key)
			if !strings.HasPrefix(keyStr, storageID+":") {
				continue
			}
			value, err := tx.Get(bucketDirMeta, key)
			if err != nil {
				continue
			}
			var dir dirMetadata
			if err := json.Unmarshal(value, &dir); err != nil {
				continue
			}
			directories = append(directories, dir)
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

			// For the final component, respect the stored IsDir flag.
			// Explicitly-ingested directories have meta.IsDir=true even
			// though they are the last path component.
			if i == len(parts)-1 && meta.IsDir {
				isDir = true
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
						// Promote to directory if needed and update Mtime.
						if !entry.IsDir {
							entries[j].IsDir = true
							entries[j].Mode = 0755 | uint32(syscall.S_IFDIR)
							entries[j].Size = 0
							entries[j].HashKey = ""
						}
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
					// Infer directory mode from child file mode: if files
					// are read-only (e.g. Go module cache 0444), directories
					// should also be read-only (0555).
					if meta.Mode&0222 == 0 {
						entry.Mode = 0555 | uint32(syscall.S_IFDIR)
					} else {
						entry.Mode = 0755 | uint32(syscall.S_IFDIR)
					}
				}

				entries = append(entries, entry)
			}

			dirMap[dirPath] = entries
		}
	}

	for _, dir := range directories {
		if dir.Path == "" {
			continue
		}

		parentDir := extractDirPath(dir.Path)
		entryName := extractFileName(dir.Path)
		entries := dirMap[parentDir]
		found := false
		for i, entry := range entries {
			if entry.Name != entryName {
				continue
			}
			entries[i].IsDir = true
			entries[i].Mode = dir.Mode
			if dir.Mtime > entries[i].Mtime {
				entries[i].Mtime = dir.Mtime
			}
			found = true
			break
		}
		if !found {
			entries = append(entries, dirIndexEntry{
				Name:  entryName,
				IsDir: true,
				Mode:  dir.Mode,
				Mtime: dir.Mtime,
			})
		}
		dirMap[parentDir] = entries
		if _, ok := dirMap[dir.Path]; !ok {
			dirMap[dir.Path] = []dirIndexEntry{}
		}
	}

	// Write all directory indexes to database
	dirsIndexed := int64(0)
	err = s.db.Update(func(tx *nutsdb.Tx) error {
		for _, meta := range fileMetadata {
			if err := s.upsertDirectoryHierarchy(tx, storageID, meta.FilePath, meta.Mode, meta.Mtime, meta.IsDir); err != nil {
				return fmt.Errorf("backfill dir metadata for %q: %w", meta.FilePath, err)
			}
		}
		for _, dir := range directories {
			if err := s.upsertDirectoryMetadata(tx, storageID, dir.Path, dir.Mode, dir.Mtime, dir.Explicit); err != nil {
				return fmt.Errorf("backfill existing dir metadata for %q: %w", dir.Path, err)
			}
		}

		for dirPath, entries := range dirMap {
			// Sort entries by name before storing for deterministic ordering
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].Name < entries[j].Name
			})
			dirIndexKey := makeDirIndexKey(storageID, dirPath)

			// Merge with existing dir index entries (e.g. from dir-hint
			// batches sent for files owned by other nodes) instead of
			// overwriting. This ensures the complete directory listing is
			// preserved across HRW-sharded nodes.
			if existingVal, err := tx.Get(bucketDirIndex, dirIndexKey); err == nil {
				var existing []dirIndexEntry
				if json.Unmarshal(existingVal, &existing) == nil && len(existing) > 0 {
					// Build lookup of locally-built entries.
					localNames := make(map[string]struct{}, len(entries))
					for _, e := range entries {
						localNames[e.Name] = struct{}{}
					}
					// Keep entries from existing index that aren't in the local set.
					for _, e := range existing {
						if _, ok := localNames[e.Name]; !ok {
							entries = append(entries, e)
						}
					}
					sort.Slice(entries, func(i, j int) bool {
						return entries[i].Name < entries[j].Name
					})
				}
			}

			if err := storeDirectorySummary(tx, storageID, dirPath, entries); err != nil {
				return err
			}

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

// removeFromDirectoryIndex removes a single entry (file or subdirectory) from a parent
// directory's index. Must be called within a NutsDB transaction.
func (s *Server) removeFromDirectoryIndex(tx *nutsdb.Tx, storageID, parentDir, entryName string) error {
	dirIndexKey := makeDirIndexKey(storageID, parentDir)

	value, err := tx.Get(bucketDirIndex, dirIndexKey)
	if err != nil {
		// Parent dir index doesn't exist — nothing to remove
		return nil
	}

	var dirIndex []dirIndexEntry
	if err := json.Unmarshal(value, &dirIndex); err != nil {
		return fmt.Errorf("unmarshal dir index for %q: %w", parentDir, err)
	}

	// Filter out the target entry
	filtered := dirIndex[:0]
	for _, entry := range dirIndex {
		if entry.Name != entryName {
			filtered = append(filtered, entry)
		}
	}

	if len(filtered) == len(dirIndex) {
		// Entry not found — nothing to remove
		return nil
	}

	if len(filtered) == 0 {
		// Directory is now empty — remove the index entry entirely
		return tx.Delete(bucketDirIndex, dirIndexKey)
	}

	dirIndexValue, err := json.Marshal(filtered)
	if err != nil {
		return fmt.Errorf("marshal dir index for %q: %w", parentDir, err)
	}
	return tx.Put(bucketDirIndex, dirIndexKey, dirIndexValue, 0)
}

// DeleteDirectoryRecursive removes a directory and all its contents from the node.
func (s *Server) DeleteDirectoryRecursive(ctx context.Context, req *pb.DeleteDirectoryRecursiveRequest) (*pb.DeleteDirectoryRecursiveResponse, error) {
	storageID := req.StorageId
	dirPath := req.DirPath

	s.logger.Info("deleting directory recursively",
		"storage_id", storageID,
		"dir_path", dirPath)

	// Collect all paths to delete by walking the directory index tree (read-only pass)
	var filePaths []string
	var dirPaths []string

	err := s.db.View(func(tx *nutsdb.Tx) error {
		var walkDir func(path string) error
		walkDir = func(path string) error {
			dirIndexKey := makeDirIndexKey(storageID, path)
			value, err := tx.Get(bucketDirIndex, dirIndexKey)
			if err != nil {
				return nil // Directory not indexed — skip
			}

			var dirIndex []dirIndexEntry
			if err := json.Unmarshal(value, &dirIndex); err != nil {
				return fmt.Errorf("unmarshal dir index for %q: %w", path, err)
			}

			for _, entry := range dirIndex {
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

		// Add the target directory itself
		dirPaths = append(dirPaths, dirPath)
		return walkDir(dirPath)
	})

	if err != nil {
		return nil, fmt.Errorf("walking directory tree: %w", err)
	}

	var filesDeleted, dirsDeleted int64

	// Delete all collected entries in an update transaction
	err = s.db.Update(func(tx *nutsdb.Tx) error {
		// Delete all files
		for _, fp := range filePaths {
			fullPath := storageID + ":" + fp
			pathKey := []byte(fullPath)
			metaKey := makeStorageKey(storageID, fp)

			// Delete from metadata (uses SHA-256 hash key)
			_ = tx.Delete(bucketMetadata, metaKey)
			// Delete from path index
			_ = tx.Delete(bucketPathIndex, pathKey)
			// Delete from owned files
			_ = tx.Delete(bucketOwnedFiles, pathKey)
			// Delete from replica files
			_ = tx.Delete(bucketReplicaFiles, pathKey)
			filesDeleted++
		}

		// Delete all directory indexes
		for _, dp := range dirPaths {
			pathKey := []byte(storageID + ":" + dp)
			metaKey := makeStorageKey(storageID, dp)
			_ = tx.Delete(bucketMetadata, metaKey)
			_ = tx.Delete(bucketPathIndex, pathKey)
			_ = tx.Delete(bucketOwnedFiles, pathKey)
			_ = tx.Delete(bucketReplicaFiles, pathKey)
			_ = tx.Delete(bucketDirMeta, makeDirMetaKey(storageID, dp))
			_ = tx.Delete(bucketDirSummary, makeDirMetaKey(storageID, dp))

			dirIndexKey := makeDirIndexKey(storageID, dp)
			_ = tx.Delete(bucketDirIndex, dirIndexKey)
			dirsDeleted++
		}

		// Remove target directory from its parent's index
		parentDir := extractDirPath(dirPath)
		entryName := extractFileName(dirPath)
		if err := s.removeFromDirectoryIndex(tx, storageID, parentDir, entryName); err != nil {
			s.logger.Warn("failed to remove dir from parent index", "dir_path", dirPath, "error", err)
		}
		if err := s.removeFromDirectorySummary(tx, storageID, parentDir, entryName); err != nil {
			s.logger.Warn("failed to remove dir from parent summary", "dir_path", dirPath, "error", err)
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
