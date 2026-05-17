// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"os"
	"sort"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// OverlayManager handles merging local changes with backend data.
// Uses OverlayDB for all overlay queries — no filesystem scanning needed.
type OverlayManager struct {
	sessionMgr *SessionManager
}

// NewOverlayManager creates a new overlay manager.
func NewOverlayManager(sessionMgr *SessionManager) *OverlayManager {
	return &OverlayManager{
		sessionMgr: sessionMgr,
	}
}

// MergeReadDir merges backend directory entries with local overlay changes.
// Uses OverlayDB + disk scanning to produce a complete directory listing.
// For user-root-dir paths (where backend is nil), disk is always scanned
// as the authoritative source, with DB providing supplementary metadata.
func (om *OverlayManager) MergeReadDir(backendEntries []fuse.DirEntry, dirPath string) []fuse.DirEntry {
	if om.sessionMgr == nil {
		return backendEntries
	}

	// Build a map of entries for easy merging
	entries := make(map[string]fuse.DirEntry)
	for _, e := range backendEntries {
		entries[e.Name] = e
	}

	// For user-root-dir paths (backendEntries == nil), scan disk directly.
	// This is the authoritative source — any file on disk is visible,
	// regardless of whether it's been tracked in the DB yet.
	// This eliminates the race between file creation and DB tracking.
	if backendEntries == nil {
		localPath, err := om.sessionMgr.GetLocalPath(dirPath)
		if err == nil {
			if diskEntries, err := os.ReadDir(localPath); err == nil {
				for _, de := range diskEntries {
					name := de.Name()
					// Skip overlay.db directory
					if name == "overlay.db" {
						continue
					}
					fullPath := name
					if dirPath != "" {
						fullPath = dirPath + "/" + name
					}
					var mode uint32
					if de.IsDir() {
						mode = 0755 | uint32(fuse.S_IFDIR)
					} else if de.Type()&os.ModeSymlink != 0 {
						mode = 0777 | uint32(fuse.S_IFLNK)
					} else {
						mode = 0644 | uint32(fuse.S_IFREG)
					}
					entries[name] = fuse.DirEntry{
						Name: name,
						Mode: mode,
						Ino:  hashPathForNode(fullPath),
					}
				}
			}
		}
	}

	db := om.sessionMgr.GetOverlayDB()
	if db == nil {
		return om.toSlice(entries)
	}

	// At root level, add user-created directories
	if dirPath == "" {
		userDirs := db.ListUserDirs()
		for _, name := range userDirs {
			entries[name] = fuse.DirEntry{
				Name: name,
				Mode: 0755 | uint32(fuse.S_IFDIR),
				Ino:  hashPathForNode(name),
			}
		}
	}

	// Query overlay files under this directory from DB
	overlayEntries, overlayNames, err := db.ListFilesUnderDir(dirPath)
	if err == nil {
		for i, entry := range overlayEntries {
			name := overlayNames[i]

			mode := entry.Mode & 0777
			switch entry.Type {
			case FileEntryDir:
				mode |= uint32(fuse.S_IFDIR)
			case FileEntrySymlink:
				mode |= uint32(fuse.S_IFLNK)
			default:
				mode |= uint32(fuse.S_IFREG)
			}

			fullPath := name
			if dirPath != "" {
				fullPath = dirPath + "/" + name
			}

			entries[name] = fuse.DirEntry{
				Name: name,
				Mode: mode,
				Ino:  hashPathForNode(fullPath),
			}
		}
	}

	// Remove deleted entries
	deleted, err := db.ListDeletedUnderDir(dirPath)
	if err == nil {
		for _, name := range deleted {
			delete(entries, name)
		}
	}

	return om.toSlice(entries)
}

// toSlice converts entry map to slice.
func (om *OverlayManager) toSlice(entries map[string]fuse.DirEntry) []fuse.DirEntry {
	result := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		result = append(result, e)
	}
	// Sort by name for deterministic ordering required by go mod verify
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// ShouldUseLocalFile checks if a file should be read from local overlay.
func (om *OverlayManager) ShouldUseLocalFile(monofsPath string) bool {
	if om.sessionMgr == nil {
		return false
	}
	return om.sessionMgr.HasLocalOverride(monofsPath)
}

// GetLocalContent reads file content from local overlay.
func (om *OverlayManager) GetLocalContent(monofsPath string) ([]byte, error) {
	localPath, err := om.sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(localPath)
}

// GetLocalAttr gets file attributes from local overlay.
// First checks OverlayDB, falls back to os.Stat for freshness.
func (om *OverlayManager) GetLocalAttr(monofsPath string) (*LocalAttr, error) {
	localPath, err := om.sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		return nil, err
	}

	// Lstat the actual file for fresh size/mtime (Lstat to handle symlinks properly)
	info, err := os.Lstat(localPath)
	if err != nil {
		return nil, err
	}

	attr := &LocalAttr{
		Size:  uint64(info.Size()),
		Mode:  uint32(info.Mode() & 0777),
		Mtime: info.ModTime().Unix(),
		IsDir: info.IsDir(),
	}

	if info.Mode()&os.ModeSymlink != 0 {
		attr.Mode |= uint32(syscall.S_IFLNK)
	} else if attr.IsDir {
		attr.Mode |= uint32(fuse.S_IFDIR)
	} else {
		attr.Mode |= uint32(fuse.S_IFREG)
	}

	return attr, nil
}

// IsPathDeleted checks if a path has been deleted in the current session.
func (om *OverlayManager) IsPathDeleted(monofsPath string) bool {
	if om.sessionMgr == nil {
		return false
	}
	return om.sessionMgr.IsDeleted(monofsPath)
}

// LocalAttr represents file attributes from local overlay.
type LocalAttr struct {
	Size  uint64
	Mode  uint32
	Mtime int64
	IsDir bool
}
