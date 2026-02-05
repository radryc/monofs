// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// OverlayManager handles merging local changes with backend data
type OverlayManager struct {
	sessionMgr *SessionManager
}

// NewOverlayManager creates a new overlay manager
func NewOverlayManager(sessionMgr *SessionManager) *OverlayManager {
	return &OverlayManager{
		sessionMgr: sessionMgr,
	}
}

// MergeReadDir merges backend directory entries with local overlay changes
func (om *OverlayManager) MergeReadDir(backendEntries []fuse.DirEntry, dirPath string) []fuse.DirEntry {
	if om.sessionMgr == nil || !om.sessionMgr.HasActiveSession() {
		return backendEntries
	}

	// Build a map of entries for easy merging
	entries := make(map[string]fuse.DirEntry)
	for _, e := range backendEntries {
		entries[e.Name] = e
	}

	// At root level, add user-created directories
	if dirPath == "" {
		userDirs := om.sessionMgr.ListUserRootDirs()
		for _, name := range userDirs {
			entries[name] = fuse.DirEntry{
				Name: name,
				Mode: 0755 | uint32(fuse.S_IFDIR),
				Ino:  hashPathForNode(name),
			}
		}
	}

	// Check for local directory
	localPath, err := om.sessionMgr.GetLocalPath(dirPath)
	if err != nil {
		return om.toSlice(entries)
	}

	// Read local directory entries
	localEntries, err := os.ReadDir(localPath)
	if err == nil {
		for _, de := range localEntries {
			// Skip hidden session files
			if strings.HasPrefix(de.Name(), ".") {
				continue
			}

			info, err := de.Info()
			if err != nil {
				continue
			}

			mode := uint32(info.Mode() & 0777)
			if de.IsDir() {
				mode |= uint32(fuse.S_IFDIR)
			} else if info.Mode()&os.ModeSymlink != 0 {
				mode |= uint32(fuse.S_IFLNK)
			} else {
				mode |= uint32(fuse.S_IFREG)
			}

			fullPath := de.Name()
			if dirPath != "" {
				fullPath = dirPath + "/" + de.Name()
			}

			entries[de.Name()] = fuse.DirEntry{
				Name: de.Name(),
				Mode: mode,
				Ino:  hashPathForNode(fullPath),
			}
		}
	}

	// Remove deleted entries
	session := om.sessionMgr.GetCurrentSession()
	if session != nil {
		session.mu.RLock()
		for _, change := range session.Changes {
			if change.Type == ChangeDelete || change.Type == ChangeRmdir || change.Type == ChangeRemoveUserRootDir {
				// Check if this deletion is in the current directory
				changeDir := filepath.Dir(change.Path)
				if changeDir == "." {
					changeDir = ""
				}
				if changeDir == dirPath {
					changeName := filepath.Base(change.Path)
					delete(entries, changeName)
				}
				// Handle root level deletions (path has no directory component)
				if dirPath == "" && changeDir == "" {
					delete(entries, change.Path)
				}
			}
		}
		session.mu.RUnlock()
	}

	return om.toSlice(entries)
}

// toSlice converts entry map to slice
func (om *OverlayManager) toSlice(entries map[string]fuse.DirEntry) []fuse.DirEntry {
	result := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		result = append(result, e)
	}
	return result
}

// ShouldUseLocalFile checks if a file should be read from local overlay
func (om *OverlayManager) ShouldUseLocalFile(monofsPath string) bool {
	if om.sessionMgr == nil {
		return false
	}
	return om.sessionMgr.HasLocalOverride(monofsPath)
}

// GetLocalContent reads file content from local overlay
func (om *OverlayManager) GetLocalContent(monofsPath string) ([]byte, error) {
	localPath, err := om.sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(localPath)
}

// GetLocalAttr gets file attributes from local overlay
func (om *OverlayManager) GetLocalAttr(monofsPath string) (*LocalAttr, error) {
	localPath, err := om.sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(localPath)
	if err != nil {
		return nil, err
	}

	attr := &LocalAttr{
		Size:  uint64(info.Size()),
		Mode:  uint32(info.Mode() & 0777),
		Mtime: info.ModTime().Unix(),
		IsDir: info.IsDir(),
	}

	if attr.IsDir {
		attr.Mode |= uint32(fuse.S_IFDIR)
	} else {
		attr.Mode |= uint32(fuse.S_IFREG)
	}

	return attr, nil
}

// IsPathDeleted checks if a path has been deleted in the current session
func (om *OverlayManager) IsPathDeleted(monofsPath string) bool {
	if om.sessionMgr == nil {
		return false
	}
	return om.sessionMgr.IsDeleted(monofsPath)
}

// LocalAttr represents file attributes from local overlay
type LocalAttr struct {
	Size  uint64
	Mode  uint32
	Mtime int64
	IsDir bool
}
