// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nutsdb/nutsdb"
)

// Bucket names for overlay database
const (
	bucketOverlayFiles   = "files"    // monofs path -> FileEntry JSON
	bucketOverlayDeleted = "deleted"  // monofs path -> deletion timestamp
	bucketOverlayDirs    = "userdirs" // root dir name -> creation timestamp
)

// FileEntryType represents the type of an overlay file entry
type FileEntryType string

const (
	FileEntryRegular FileEntryType = "file"
	FileEntryDir     FileEntryType = "dir"
	FileEntrySymlink FileEntryType = "symlink"
)

// FileEntry represents metadata about an overlayed file stored in NutsDB.
// Actual file content remains on disk; the DB only tracks what is overlayed.
type FileEntry struct {
	Type          FileEntryType `json:"type"`
	LocalPath     string        `json:"local_path"`
	Mode          uint32        `json:"mode"`
	Size          uint64        `json:"size"`
	Mtime         int64         `json:"mtime"`
	SymlinkTarget string        `json:"symlink_target,omitempty"`
	OrigHash      string        `json:"orig_hash,omitempty"`
	ChangeType    ChangeType    `json:"change_type"` // create, modify, etc.
	Timestamp     time.Time     `json:"timestamp"`
}

// OverlayDB wraps NutsDB to track which files are overlayed in a session.
// It replaces the in-memory maps + JSON persistence + os.Stat fallback
// approach with a single embedded database.
type OverlayDB struct {
	db     *nutsdb.DB
	dbPath string
	logger *slog.Logger
}

// OpenOverlayDB opens (or creates) the overlay database at the given directory.
func OpenOverlayDB(dir string, logger *slog.Logger) (*OverlayDB, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "overlaydb")

	dbPath := filepath.Join(dir, "overlay.db")
	if err := os.MkdirAll(dbPath, 0755); err != nil {
		return nil, fmt.Errorf("create overlay db dir: %w", err)
	}

	opt := nutsdb.DefaultOptions
	opt.Dir = dbPath
	opt.SegmentSize = 16 * 1024 * 1024 // 16MB segments (smaller than server)
	opt.SyncEnable = false             // async writes

	db, err := nutsdb.Open(opt)
	if err != nil {
		return nil, fmt.Errorf("open overlay db: %w", err)
	}

	odb := &OverlayDB{
		db:     db,
		dbPath: dbPath,
		logger: logger,
	}

	// Initialize buckets
	if err := odb.initBuckets(); err != nil {
		db.Close()
		return nil, err
	}

	logger.Info("overlay database opened", "path", dbPath)
	return odb, nil
}

func (odb *OverlayDB) initBuckets() error {
	buckets := []string{bucketOverlayFiles, bucketOverlayDeleted, bucketOverlayDirs}
	for _, bucket := range buckets {
		if err := odb.db.Update(func(tx *nutsdb.Tx) error {
			return tx.NewBucket(nutsdb.DataStructureBTree, bucket)
		}); err != nil && err != nutsdb.ErrBucketAlreadyExist {
			return fmt.Errorf("create bucket %s: %w", bucket, err)
		}
	}
	return nil
}

// Close closes the overlay database.
func (odb *OverlayDB) Close() error {
	if odb.db != nil {
		odb.logger.Info("closing overlay database")
		return odb.db.Close()
	}
	return nil
}

// PutFile records a file entry in the overlay database.
func (odb *OverlayDB) PutFile(monofsPath string, entry FileEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal file entry: %w", err)
	}

	return odb.db.Update(func(tx *nutsdb.Tx) error {
		// Remove from deleted if it was previously deleted
		_ = tx.Delete(bucketOverlayDeleted, []byte(monofsPath))
		return tx.Put(bucketOverlayFiles, []byte(monofsPath), data, 0)
	})
}

// GetFile retrieves a file entry from the overlay database.
// Returns the entry, whether it was found, and any error.
func (odb *OverlayDB) GetFile(monofsPath string) (FileEntry, bool, error) {
	var entry FileEntry
	var found bool

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketOverlayFiles, []byte(monofsPath))
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		if err := json.Unmarshal(value, &entry); err != nil {
			return fmt.Errorf("unmarshal file entry: %w", err)
		}
		found = true
		return nil
	})

	return entry, found, err
}

// DeleteFile removes a file entry from the overlay database.
// This does NOT mark it as deleted from the backend; use MarkDeleted for that.
func (odb *OverlayDB) DeleteFile(monofsPath string) error {
	return odb.db.Update(func(tx *nutsdb.Tx) error {
		err := tx.Delete(bucketOverlayFiles, []byte(monofsPath))
		if err != nil && !isNotFound(err) {
			return err
		}
		return nil
	})
}

// MarkDeleted records that a backend file was deleted in this session.
func (odb *OverlayDB) MarkDeleted(monofsPath string) error {
	ts := []byte(time.Now().Format(time.RFC3339))
	return odb.db.Update(func(tx *nutsdb.Tx) error {
		// Also remove from files if it was there
		_ = tx.Delete(bucketOverlayFiles, []byte(monofsPath))
		return tx.Put(bucketOverlayDeleted, []byte(monofsPath), ts, 0)
	})
}

// UnmarkDeleted removes a path from the deleted set.
func (odb *OverlayDB) UnmarkDeleted(monofsPath string) error {
	return odb.db.Update(func(tx *nutsdb.Tx) error {
		err := tx.Delete(bucketOverlayDeleted, []byte(monofsPath))
		if err != nil && !isNotFound(err) {
			return err
		}
		return nil
	})
}

// IsDeleted checks if a path has been marked as deleted.
func (odb *OverlayDB) IsDeleted(monofsPath string) bool {
	var deleted bool
	_ = odb.db.View(func(tx *nutsdb.Tx) error {
		_, err := tx.Get(bucketOverlayDeleted, []byte(monofsPath))
		if err == nil {
			deleted = true
		}
		return nil
	})
	return deleted
}

// PutUserDir records a user-created root directory.
func (odb *OverlayDB) PutUserDir(name string) error {
	ts := []byte(time.Now().Format(time.RFC3339))
	return odb.db.Update(func(tx *nutsdb.Tx) error {
		return tx.Put(bucketOverlayDirs, []byte(name), ts, 0)
	})
}

// RemoveUserDir removes a user-created root directory record.
func (odb *OverlayDB) RemoveUserDir(name string) error {
	return odb.db.Update(func(tx *nutsdb.Tx) error {
		err := tx.Delete(bucketOverlayDirs, []byte(name))
		if err != nil && !isNotFound(err) {
			return err
		}
		return nil
	})
}

// IsUserDir checks if a name is a user-created root directory.
func (odb *OverlayDB) IsUserDir(name string) bool {
	var found bool
	_ = odb.db.View(func(tx *nutsdb.Tx) error {
		_, err := tx.Get(bucketOverlayDirs, []byte(name))
		if err == nil {
			found = true
		}
		return nil
	})
	return found
}

// PathStatResult holds the batched result of a path state query.
type PathStatResult struct {
	IsDeleted     bool
	FileEntry     FileEntry
	HasOverride   bool
	IsUserRootDir bool
}

// GetPathStatBatch performs all path-state lookups (IsDeleted, GetFile,
// IsUserDir) in a single NutsDB View transaction instead of three separate
// ones. This reduces transaction overhead by ~3x on every Lookup/Getattr.
func (odb *OverlayDB) GetPathStatBatch(monofsPath string) PathStatResult {
	var result PathStatResult
	_ = odb.db.View(func(tx *nutsdb.Tx) error {
		// Check deleted
		if _, err := tx.Get(bucketOverlayDeleted, []byte(monofsPath)); err == nil {
			result.IsDeleted = true
		}

		// Check file entry
		if value, err := tx.Get(bucketOverlayFiles, []byte(monofsPath)); err == nil {
			if err := json.Unmarshal(value, &result.FileEntry); err == nil {
				result.HasOverride = true
			}
		}

		// Check user root dir
		if _, err := tx.Get(bucketOverlayDirs, []byte(monofsPath)); err == nil {
			result.IsUserRootDir = true
		}

		return nil
	})
	return result
}

// ListUserDirs returns all user-created root directory names.
func (odb *OverlayDB) ListUserDirs() []string {
	var dirs []string
	_ = odb.db.View(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.GetAll(bucketOverlayDirs)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for _, k := range keys {
			dirs = append(dirs, string(k))
		}
		return nil
	})
	return dirs
}

// ListFilesUnderDir returns all overlay file entries whose path starts with
// the given directory prefix. For root listing, pass "".
func (odb *OverlayDB) ListFilesUnderDir(dirPath string) ([]FileEntry, []string, error) {
	var entries []FileEntry
	var names []string

	prefix := dirPath
	if prefix != "" {
		prefix += "/"
	}

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		if prefix == "" {
			// For root, get all entries and filter to top-level
			allKeys, allValues, err := tx.GetAll(bucketOverlayFiles)
			if err != nil {
				if isNotFound(err) {
					return nil
				}
				return err
			}
			for i, k := range allKeys {
				path := string(k)
				// Only include top-level entries (no "/" in path)
				if !strings.Contains(path, "/") {
					var entry FileEntry
					if err := json.Unmarshal(allValues[i], &entry); err == nil {
						entries = append(entries, entry)
						names = append(names, path)
					}
				}
			}
			return nil
		}

		// Prefix scan for entries under this directory
		scanKeys, scanValues, err := tx.PrefixScanEntries(bucketOverlayFiles, []byte(prefix), "", 0, -1, true, true)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for i, k := range scanKeys {
			path := string(k)
			// Only include direct children (one level below dirPath)
			relPath := strings.TrimPrefix(path, prefix)
			if !strings.Contains(relPath, "/") {
				var entry FileEntry
				if err := json.Unmarshal(scanValues[i], &entry); err == nil {
					entries = append(entries, entry)
					names = append(names, relPath)
				}
			}
		}
		return nil
	})

	return entries, names, err
}

// ListDeletedUnderDir returns paths deleted under the given directory prefix.
func (odb *OverlayDB) ListDeletedUnderDir(dirPath string) ([]string, error) {
	var deleted []string

	prefix := dirPath
	if prefix != "" {
		prefix += "/"
	}

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		if prefix == "" {
			allKeys, _, err := tx.GetAll(bucketOverlayDeleted)
			if err != nil {
				if isNotFound(err) {
					return nil
				}
				return err
			}
			for _, k := range allKeys {
				path := string(k)
				if !strings.Contains(path, "/") {
					deleted = append(deleted, path)
				}
			}
			return nil
		}

		scanKeys, _, err := tx.PrefixScanEntries(bucketOverlayDeleted, []byte(prefix), "", 0, -1, true, false)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for _, k := range scanKeys {
			path := string(k)
			relPath := strings.TrimPrefix(path, prefix)
			if !strings.Contains(relPath, "/") {
				deleted = append(deleted, relPath)
			}
		}
		return nil
	})

	return deleted, err
}

// GetAllFiles returns all overlay file entries. Used for commit operations.
func (odb *OverlayDB) GetAllFiles() (map[string]FileEntry, error) {
	result := make(map[string]FileEntry)

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		keys, values, err := tx.GetAll(bucketOverlayFiles)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for i, k := range keys {
			var entry FileEntry
			if err := json.Unmarshal(values[i], &entry); err == nil {
				result[string(k)] = entry
			}
		}
		return nil
	})

	return result, err
}

// GetAllDeleted returns all deleted paths. Used for commit operations.
func (odb *OverlayDB) GetAllDeleted() ([]string, error) {
	var deleted []string

	err := odb.db.View(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.GetAll(bucketOverlayDeleted)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for _, k := range keys {
			deleted = append(deleted, string(k))
		}
		return nil
	})

	return deleted, err
}

// RebuildFromDisk scans the session directory and populates the database
// from the filesystem. Called when the DB doesn't exist on mount (recovery).
//
// All entries are collected during the walk phase and then written in a
// single NutsDB transaction to avoid write-lock contention with NutsDB's
// background merge worker goroutine.
func (odb *OverlayDB) RebuildFromDisk(sessionDir string) error {
	odb.logger.Info("rebuilding overlay database from disk", "dir", sessionDir)

	// Phase 1: collect entries from disk without touching the DB.
	type pendingFile struct {
		relPath string
		entry   FileEntry
	}
	var pendingFiles []pendingFile
	var pendingUserDirs []string

	err := filepath.Walk(sessionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		// Get relative path from session dir
		relPath, err := filepath.Rel(sessionDir, path)
		if err != nil {
			return nil
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Convert OS path separators to forward slashes
		relPath = filepath.ToSlash(relPath)

		// Skip the overlay database directory itself — it contains NutsDB
		// segment files that must not be indexed as overlay entries.
		if relPath == "overlay.db" || strings.HasPrefix(relPath, "overlay.db/") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip hidden files/directories (session metadata)
		parts := strings.Split(relPath, "/")
		for _, part := range parts {
			if strings.HasPrefix(part, ".") {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		// Determine entry type
		entryType := FileEntryRegular
		if info.IsDir() {
			entryType = FileEntryDir
		} else if info.Mode()&os.ModeSymlink != 0 {
			entryType = FileEntrySymlink
		}

		mode := uint32(info.Mode() & 0777)
		if info.IsDir() {
			mode |= uint32(os.ModeDir)
		}

		entry := FileEntry{
			Type:       entryType,
			LocalPath:  path,
			Mode:       mode,
			Size:       uint64(info.Size()),
			Mtime:      info.ModTime().Unix(),
			ChangeType: ChangeCreate, // Assume created since we're rebuilding
			Timestamp:  info.ModTime(),
		}

		// For symlinks, read the target
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err == nil {
				entry.SymlinkTarget = target
				entry.Type = FileEntrySymlink
			}
		}

		pendingFiles = append(pendingFiles, pendingFile{relPath: relPath, entry: entry})

		// Check if this is a top-level directory (potential user root dir)
		if info.IsDir() && !strings.Contains(relPath, "/") {
			if !strings.Contains(relPath, ".") {
				pendingUserDirs = append(pendingUserDirs, relPath)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Phase 2: write all entries in a single transaction.
	if len(pendingFiles) > 0 || len(pendingUserDirs) > 0 {
		if txErr := odb.db.Update(func(tx *nutsdb.Tx) error {
			for _, ud := range pendingUserDirs {
				val := []byte(time.Now().Format(time.RFC3339))
				if err := tx.Put(bucketOverlayDirs, []byte(ud), val, 0); err != nil {
					odb.logger.Warn("rebuild: failed to record user dir", "name", ud, "error", err)
				}
			}
			for _, f := range pendingFiles {
				data, err := json.Marshal(f.entry)
				if err != nil {
					odb.logger.Warn("rebuild: marshal failed", "path", f.relPath, "error", err)
					continue
				}
				if err := tx.Put(bucketOverlayFiles, []byte(f.relPath), data, 0); err != nil {
					odb.logger.Warn("rebuild: failed to record file", "path", f.relPath, "error", err)
				}
			}
			return nil
		}); txErr != nil {
			return fmt.Errorf("rebuild batch write: %w", txErr)
		}
	}

	odb.logger.Info("overlay database rebuilt", "files", len(pendingFiles), "user_dirs", len(pendingUserDirs))
	return nil
}

// RefreshEntry re-stats a file from disk and updates the DB entry.
// Useful when a local file may have been modified outside our tracking.
func (odb *OverlayDB) RefreshEntry(monofsPath string, localPath string) error {
	info, err := os.Lstat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return odb.DeleteFile(monofsPath)
		}
		return err
	}

	existing, found, _ := odb.GetFile(monofsPath)

	entryType := FileEntryRegular
	if info.IsDir() {
		entryType = FileEntryDir
	} else if info.Mode()&os.ModeSymlink != 0 {
		entryType = FileEntrySymlink
	}

	entry := FileEntry{
		Type:      entryType,
		LocalPath: localPath,
		Mode:      uint32(info.Mode() & 0777),
		Size:      uint64(info.Size()),
		Mtime:     info.ModTime().Unix(),
		Timestamp: time.Now(),
	}

	if found {
		entry.OrigHash = existing.OrigHash
		entry.ChangeType = existing.ChangeType
		entry.SymlinkTarget = existing.SymlinkTarget
	} else {
		entry.ChangeType = ChangeCreate
	}

	return odb.PutFile(monofsPath, entry)
}

// FileCount returns the number of files in the overlay.
func (odb *OverlayDB) FileCount() int {
	count := 0
	_ = odb.db.View(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.GetAll(bucketOverlayFiles)
		if err != nil {
			return nil
		}
		count = len(keys)
		return nil
	})
	return count
}

// DeletedCount returns the number of deleted paths.
func (odb *OverlayDB) DeletedCount() int {
	count := 0
	_ = odb.db.View(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.GetAll(bucketOverlayDeleted)
		if err != nil {
			return nil
		}
		count = len(keys)
		return nil
	})
	return count
}

// RenamePrefix re-keys all overlay file entries whose monofsPath starts
// with oldPrefix+"/" so they instead start with newPrefix+"/". It also
// rewrites the LocalPath field by applying the same substitution.
// This must be called when a directory is renamed so the overlay DB stays
// in sync with the physical file layout.
func (odb *OverlayDB) RenamePrefix(oldPrefix, newPrefix string) (int, error) {
	oldDir := oldPrefix + "/"
	newDir := newPrefix + "/"
	renamed := 0

	err := odb.db.Update(func(tx *nutsdb.Tx) error {
		// Use PrefixScanEntries to find only matching entries instead of
		// loading the entire bucket via GetAll. This is O(matches) instead
		// of O(total_entries).
		keys, values, err := tx.PrefixScanEntries(
			bucketOverlayFiles, []byte(oldDir), "", 0, -1, true, true)
		if err != nil {
			if isNotFound(err) {
				// No matching entries — nothing to rename
				return nil
			}
			return err
		}

		// Collect CHILD entries to rename (cannot mutate during iteration).
		type renameEntry struct {
			oldKey string
			entry  FileEntry
		}
		var toRename []renameEntry

		for i, k := range keys {
			key := string(k)
			var entry FileEntry
			if err := json.Unmarshal(values[i], &entry); err != nil {
				continue
			}
			toRename = append(toRename, renameEntry{oldKey: key, entry: entry})
		}

		for _, re := range toRename {
			// Compute new monofsPath.
			newKey := newDir + strings.TrimPrefix(re.oldKey, oldDir)

			// Rewrite LocalPath with the same prefix swap.
			if re.entry.LocalPath != "" {
				re.entry.LocalPath = strings.Replace(re.entry.LocalPath, "/"+oldPrefix, "/"+newPrefix, 1)
			}

			data, err := json.Marshal(re.entry)
			if err != nil {
				continue
			}

			// Delete old, put new.
			_ = tx.Delete(bucketOverlayFiles, []byte(re.oldKey))
			if err := tx.Put(bucketOverlayFiles, []byte(newKey), data, 0); err != nil {
				return fmt.Errorf("rename overlay entry %q -> %q: %w", re.oldKey, newKey, err)
			}
			renamed++
		}

		// Also rename any deletion markers under the old prefix.
		delKeys, delValues, delErr := tx.PrefixScanEntries(
			bucketOverlayDeleted, []byte(oldDir), "", 0, -1, true, true)
		if delErr != nil && !isNotFound(delErr) {
			return delErr
		}
		for i, k := range delKeys {
			key := string(k)
			newKey := newDir + strings.TrimPrefix(key, oldDir)
			_ = tx.Delete(bucketOverlayDeleted, []byte(key))
			_ = tx.Put(bucketOverlayDeleted, []byte(newKey), delValues[i], 0)
		}

		return nil
	})

	return renamed, err
}

// isNotFound checks if a NutsDB error indicates a missing key/bucket.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return err == nutsdb.ErrBucketNotFound ||
		err == nutsdb.ErrKeyNotFound ||
		err == nutsdb.ErrPrefixScan ||
		err == nutsdb.ErrBucketEmpty ||
		err == nutsdb.ErrNotFoundKey
}
