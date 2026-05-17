// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChangeType represents the type of file change
type ChangeType string

const (
	// ChangeCreate indicates a new file was created
	ChangeCreate ChangeType = "create"
	// ChangeModify indicates an existing file was modified
	ChangeModify ChangeType = "modify"
	// ChangeDelete indicates a file was deleted
	ChangeDelete ChangeType = "delete"
	// ChangeMkdir indicates a directory was created
	ChangeMkdir ChangeType = "mkdir"
	// ChangeRmdir indicates a directory was removed
	ChangeRmdir ChangeType = "rmdir"
	// ChangeSymlink indicates a symlink was created
	ChangeSymlink ChangeType = "symlink"
	// ChangeUserRootDir indicates a user-created directory at filesystem root
	ChangeUserRootDir ChangeType = "user_root_dir"
	// ChangeRemoveUserRootDir indicates removal of a user-created root directory
	ChangeRemoveUserRootDir ChangeType = "remove_user_root_dir"
)

// Change represents a single file change in a write session.
// Kept for backward compatibility with commit/socket APIs.
type Change struct {
	Type          ChangeType `json:"type"`
	Path          string     `json:"path"`
	LocalPath     string     `json:"local_path"`
	OrigHash      string     `json:"orig_hash"`
	SymlinkTarget string     `json:"symlink_target,omitempty"`
	Timestamp     time.Time  `json:"timestamp"`
}

// WriteSession represents an active write session.
// Session metadata is stored in NutsDB via OverlayDB.
type WriteSession struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	BasePath  string    `json:"base_path"`

	mu sync.RWMutex
}

// SessionManager manages write sessions for the FUSE client.
// All overlay state is persisted in NutsDB via OverlayDB, eliminating
// the need for JSON persistence, in-memory index maps, and os.Stat fallbacks.
type SessionManager struct {
	overlayBase string
	current     *WriteSession
	db          *OverlayDB
	mu          sync.RWMutex
	logger      *slog.Logger

	// depsPushedAt records when the last dependency push completed.
	// For a short window after push (depsPushBypassDuration) the FUSE
	// layer forces FOPEN_DIRECT_IO on dependency/ paths so the kernel
	// cannot serve stale page-cache content from the pre-push overlay.
	depsPushedAt time.Time
}

// depsPushBypassDuration is how long after a dependency push we force
// FOPEN_DIRECT_IO on dependency/ paths to avoid stale kernel page cache.
const depsPushBypassDuration = 60 * time.Second

// MarkDepsPushed records that a dependency push just completed.
// Called by handleRemoveBlobChanges after overlay cleanup.
func (sm *SessionManager) MarkDepsPushed() {
	sm.mu.Lock()
	sm.depsPushedAt = time.Now()
	sm.mu.Unlock()
}

// DepsPushedRecently returns true if a dependency push completed within
// depsPushBypassDuration. During this window FUSE should bypass the
// kernel page cache for dependency/ paths.
func (sm *SessionManager) DepsPushedRecently() bool {
	sm.mu.RLock()
	t := sm.depsPushedAt
	sm.mu.RUnlock()
	return !t.IsZero() && time.Since(t) < depsPushBypassDuration
}

// NewSessionManager creates a new session manager.
func NewSessionManager(overlayBase string, logger *slog.Logger) (*SessionManager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	logger.Info("initializing session manager", "overlay_base", overlayBase)

	sm := &SessionManager{
		overlayBase: overlayBase,
		logger:      logger.With("component", "session"),
	}

	// Create directory structure
	dirs := []string{
		filepath.Join(overlayBase, "sessions"),
		filepath.Join(overlayBase, "committed"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create overlay dir %s: %w", dir, err)
		}
	}

	// Check for existing active session (recovery after crash)
	currentLink := filepath.Join(overlayBase, "current")
	if target, err := os.Readlink(currentLink); err == nil {
		logger.Info("found existing session symlink", "target", target)
	} else {
		logger.Info("no existing session found", "link", currentLink)
	}

	if err := sm.recoverSession(); err != nil {
		logger.Warn("session recovery failed", "error", err)
	} else if sm.current != nil {
		logger.Info("session manager ready with recovered session",
			"session_id", sm.current.ID,
			"session_path", sm.current.BasePath,
			"db_open", sm.db != nil)
	} else {
		logger.Info("session manager ready, no active session")
	}

	// Auto-start a session if none was recovered.
	// This ensures overlay tracking works immediately when --writable is enabled,
	// without requiring an explicit "monofs-session start".
	if sm.current == nil {
		logger.Info("auto-starting overlay session")
		if _, err := sm.StartSession(); err != nil {
			logger.Warn("auto-start session failed", "error", err)
		}
	}

	return sm, nil
}

// recoverSession attempts to recover an active session after a crash.
func (sm *SessionManager) recoverSession() error {
	currentLink := filepath.Join(sm.overlayBase, "current")

	target, err := os.Readlink(currentLink)
	if err != nil {
		sm.logger.Info("recoverSession: no current symlink")
		return nil // No current session
	}

	sessionPath := target
	if !filepath.IsAbs(target) {
		sessionPath = filepath.Join(sm.overlayBase, target)
	}

	sm.logger.Info("recoverSession: found session", "path", sessionPath)

	// Verify session directory exists
	if _, err := os.Stat(sessionPath); err != nil {
		sm.logger.Warn("recoverSession: session directory missing, cleaning up", "path", sessionPath)
		os.Remove(currentLink)
		return nil
	}

	// Read session ID from directory name
	sessionID := filepath.Base(sessionPath)

	session := &WriteSession{
		ID:        sessionID,
		CreatedAt: time.Now(), // Best effort; exact time is lost
		BasePath:  sessionPath,
	}

	// Open or rebuild overlay DB
	sm.logger.Info("recoverSession: opening overlay database", "session_path", sessionPath)
	db, err := OpenOverlayDB(sessionPath, sm.logger)
	if err != nil {
		return fmt.Errorf("open overlay db: %w", err)
	}

	fileCount := db.FileCount()
	deletedCount := db.DeletedCount()
	userDirs := db.ListUserDirs()

	sm.logger.Info("recoverSession: overlay database opened",
		"files", fileCount,
		"deleted", deletedCount,
		"user_dirs", len(userDirs),
		"user_dir_names", userDirs)

	// Check if DB has data; if not, rebuild from disk
	if fileCount == 0 && deletedCount == 0 && len(userDirs) == 0 {
		sm.logger.Info("recoverSession: overlay database empty, rebuilding from disk scan")
		if err := db.RebuildFromDisk(sessionPath); err != nil {
			sm.logger.Warn("recoverSession: rebuild from disk failed", "error", err)
		} else {
			sm.logger.Info("recoverSession: rebuild complete",
				"files", db.FileCount(),
				"deleted", db.DeletedCount())
		}
	}

	sm.current = session
	sm.db = db
	sm.logger.Info("recovered active session",
		"id", session.ID,
		"files", db.FileCount(),
		"deleted", db.DeletedCount(),
		"user_dirs", len(db.ListUserDirs()))

	return nil
}

// StartSession creates a new write session or returns the existing one.
// Uses a read-lock fast path for the common case (session already exists)
// to avoid serialising concurrent FUSE operations behind an exclusive lock.
func (sm *SessionManager) StartSession() (*WriteSession, error) {
	// Fast path: session already exists — read lock only.
	sm.mu.RLock()
	if sm.current != nil {
		s := sm.current
		sm.mu.RUnlock()
		return s, nil
	}
	sm.mu.RUnlock()

	// Slow path: need to create — acquire exclusive lock.
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Double-check after acquiring write lock.
	if sm.current != nil {
		return sm.current, nil
	}

	id := generateSessionID()
	basePath := filepath.Join(sm.overlayBase, "sessions", id)

	session := &WriteSession{
		ID:        id,
		CreatedAt: time.Now(),
		BasePath:  basePath,
	}

	// Create session directory
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	// Open overlay DB
	db, err := OpenOverlayDB(basePath, sm.logger)
	if err != nil {
		return nil, fmt.Errorf("open overlay db: %w", err)
	}

	// Update current symlink
	currentLink := filepath.Join(sm.overlayBase, "current")
	os.Remove(currentLink)
	if err := os.Symlink(basePath, currentLink); err != nil {
		sm.logger.Warn("failed to create current symlink", "error", err)
	}

	sm.current = session
	sm.db = db
	sm.logger.Info("started write session",
		"id", id,
		"path", basePath,
		"db_path", filepath.Join(basePath, "overlay.db"))

	return session, nil
}

// GetCurrentSession returns the current active session (nil if none).
func (sm *SessionManager) GetCurrentSession() *WriteSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current
}

// HasActiveSession returns true if there's an active write session.
func (sm *SessionManager) HasActiveSession() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current != nil
}

// FlushChanges is a no-op with NutsDB — all writes are immediately persisted.
func (sm *SessionManager) FlushChanges() error {
	return nil
}

// CommitSession finalizes the current session and archives it.
func (sm *SessionManager) CommitSession() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return fmt.Errorf("no active session")
	}

	session := sm.current

	// Close the overlay DB before moving the directory
	if sm.db != nil {
		sm.db.Close()
		sm.db = nil
	}

	// Archive to committed/
	timestamp := time.Now().Format("20060102-150405")
	archiveName := fmt.Sprintf("%s-%s", timestamp, session.ID[:8])
	archivePath := filepath.Join(sm.overlayBase, "committed", archiveName)

	if err := os.Rename(session.BasePath, archivePath); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}

	os.Remove(filepath.Join(sm.overlayBase, "current"))

	sm.logger.Info("committed session", "id", session.ID, "archive", archiveName)
	sm.current = nil

	return nil
}

// DiscardSession abandons the current session and removes all local changes.
func (sm *SessionManager) DiscardSession() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return nil
	}

	// Close the overlay DB before removing directory
	if sm.db != nil {
		sm.db.Close()
		sm.db = nil
	}

	// Make all files writable before removal (Go modules are read-only)
	basePath := sm.current.BasePath
	filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			os.Chmod(path, 0755)
		} else if info.Mode()&0200 == 0 {
			os.Chmod(path, 0644)
		}
		return nil
	})

	if err := os.RemoveAll(basePath); err != nil {
		return fmt.Errorf("remove session: %w", err)
	}

	os.Remove(filepath.Join(sm.overlayBase, "current"))

	sm.logger.Info("discarded session", "id", sm.current.ID)
	sm.current = nil

	return nil
}

// PathState contains consolidated session state for a path.
type PathState struct {
	HasSession    bool
	IsDeleted     bool
	IsSymlink     bool
	SymlinkTarget string
	HasOverride   bool
	IsUserRootDir bool
	LocalPath     string
}

// GetPathState returns consolidated session state for a path.
// Uses a single NutsDB transaction for all three lookups (IsDeleted,
// GetFile, IsUserDir) instead of three separate transactions.
func (sm *SessionManager) GetPathState(monofsPath string) PathState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return PathState{HasSession: false}
	}

	// Single batched DB transaction instead of 3 separate ones.
	batch := sm.db.GetPathStatBatch(monofsPath)

	state := PathState{
		HasSession:    true,
		LocalPath:     filepath.Join(sm.current.BasePath, monofsPath),
		IsDeleted:     batch.IsDeleted,
		HasOverride:   batch.HasOverride,
		IsUserRootDir: batch.IsUserRootDir,
	}

	if batch.HasOverride && batch.FileEntry.Type == FileEntrySymlink {
		state.IsSymlink = true
		state.SymlinkTarget = batch.FileEntry.SymlinkTarget
	}

	return state
}

// GetLocalPath returns the local overlay path for a MonoFS path.
func (sm *SessionManager) GetLocalPath(monofsPath string) (string, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return "", fmt.Errorf("no active session")
	}

	return filepath.Join(sm.current.BasePath, monofsPath), nil
}

// HasLocalOverride checks if a file has been modified locally.
// With NutsDB this is a single key lookup — no filesystem fallback needed.
func (sm *SessionManager) HasLocalOverride(monofsPath string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return false
	}

	_, found, _ := sm.db.GetFile(monofsPath)
	return found
}

// IsDeleted checks if a path has been marked as deleted in the current session.
func (sm *SessionManager) IsDeleted(monofsPath string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return false
	}

	return sm.db.IsDeleted(monofsPath)
}

// RenameChildren re-keys all overlay entries under oldPath to sit under
// newPath instead.  This must be called when a directory is renamed so
// that files inside the directory are tracked at their new paths.
func (sm *SessionManager) RenameChildren(oldPath, newPath string) (int, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil || sm.db == nil {
		return 0, nil
	}

	n, err := sm.db.RenamePrefix(oldPath, newPath)
	if err != nil {
		sm.logger.Error("RenameChildren failed", "old", oldPath, "new", newPath, "error", err)
		return 0, err
	}
	if n > 0 {
		sm.logger.Info("RenameChildren", "old", oldPath, "new", newPath, "renamed", n)
	}
	return n, nil
}

// TrackChangeWithMeta records a change using caller-supplied metadata, skipping
// the os.Lstat that TrackChange normally performs. Use this on hot paths where
// the caller already knows the file size (e.g. after a Write or Truncate).
// A negative knownSize causes a fallback to os.Lstat.
func (sm *SessionManager) TrackChangeWithMeta(changeType ChangeType, monofsPath, origHash string, knownSize int64) error {
	return sm.trackChangeInternal(changeType, monofsPath, origHash, knownSize)
}

// TrackChange records a change in the session's overlay database.
func (sm *SessionManager) TrackChange(changeType ChangeType, monofsPath, origHash string) error {
	return sm.trackChangeInternal(changeType, monofsPath, origHash, -1)
}

func (sm *SessionManager) trackChangeInternal(changeType ChangeType, monofsPath, origHash string, knownSize int64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil || sm.db == nil {
		sm.logger.Warn("TrackChange: no active session or db", "type", changeType, "path", monofsPath)
		return fmt.Errorf("no active session")
	}

	localPath := filepath.Join(sm.current.BasePath, monofsPath)

	sm.logger.Info("TrackChange", "type", changeType, "path", monofsPath, "local", localPath)

	switch changeType {
	case ChangeDelete, ChangeRmdir:
		// If the file/dir was created in this session (only exists in the
		// overlay, never in the base layer), just remove it from the overlay
		// without marking it as deleted. This avoids phantom "[-]" entries for
		// transient items (e.g. .tmp files renamed to their final name, or
		// directories created and then removed within the same session).
		entry, found, _ := sm.db.GetFile(monofsPath)
		if found && (entry.ChangeType == ChangeCreate || entry.ChangeType == ChangeMkdir) {
			if err := sm.db.DeleteFile(monofsPath); err != nil {
				sm.logger.Error("TrackChange: DeleteFile (session-only) failed", "path", monofsPath, "error", err)
				return err
			}
			sm.logger.Info("TrackChange: removed session-only entry", "path", monofsPath,
				"files_total", sm.db.FileCount(), "deleted_total", sm.db.DeletedCount())
			return nil
		}

		if err := sm.db.MarkDeleted(monofsPath); err != nil {
			sm.logger.Error("TrackChange: MarkDeleted failed", "path", monofsPath, "error", err)
			return err
		}
		sm.logger.Info("TrackChange: marked deleted", "path", monofsPath,
			"files_total", sm.db.FileCount(), "deleted_total", sm.db.DeletedCount())
		return nil

	case ChangeCreate, ChangeModify, ChangeMkdir:
		// If the file was already tracked as ChangeCreate and a write comes
		// in (ChangeModify), keep it as ChangeCreate — the file never
		// existed on the cluster, so it's still a creation.
		effectiveType := changeType
		if changeType == ChangeModify {
			if existing, found, _ := sm.db.GetFile(monofsPath); found && existing.ChangeType == ChangeCreate {
				effectiveType = ChangeCreate
			}
		}

		// Get file metadata. When the caller provides a known size (>= 0)
		// we skip the os.Lstat syscall — this is the hot path during
		// sequential writes where the caller already knows the size.
		entryType := FileEntryRegular
		var mode uint32
		var size uint64
		var mtime int64

		if knownSize >= 0 {
			// Fast path: caller-provided metadata, no stat needed
			size = uint64(knownSize)
			mode = 0644
			mtime = time.Now().Unix()
		} else if info, err := os.Lstat(localPath); err == nil {
			if info.IsDir() {
				entryType = FileEntryDir
			}
			mode = uint32(info.Mode() & 0777)
			size = uint64(info.Size())
			mtime = info.ModTime().Unix()
		} else {
			// File might not exist yet (e.g. being created)
			mode = 0644
			mtime = time.Now().Unix()
		}

		entry := FileEntry{
			Type:       entryType,
			LocalPath:  localPath,
			Mode:       mode,
			Size:       size,
			Mtime:      mtime,
			OrigHash:   origHash,
			ChangeType: effectiveType,
			Timestamp:  time.Now(),
		}
		if err := sm.db.PutFile(monofsPath, entry); err != nil {
			sm.logger.Error("TrackChange: PutFile failed", "path", monofsPath, "error", err)
			return err
		}
		sm.logger.Info("TrackChange: recorded", "path", monofsPath, "entry_type", entryType,
			"size", size, "files_total", sm.db.FileCount())
		return nil

	case ChangeSymlink:
		// Symlink tracking is handled by CreateSymlink
		return nil

	case ChangeUserRootDir:
		return sm.db.PutUserDir(monofsPath)

	case ChangeRemoveUserRootDir:
		return sm.db.RemoveUserDir(monofsPath)

	default:
		return fmt.Errorf("unknown change type: %s", changeType)
	}
}

// GetChanges reconstructs the change list from the overlay database.
// Used for commit operations and status display.
func (sm *SessionManager) GetChanges() []Change {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return nil
	}

	var changes []Change

	// Get all overlay files
	files, err := sm.db.GetAllFiles()
	if err == nil {
		for path, entry := range files {
			changes = append(changes, Change{
				Type:          entry.ChangeType,
				Path:          path,
				LocalPath:     entry.LocalPath,
				OrigHash:      entry.OrigHash,
				SymlinkTarget: entry.SymlinkTarget,
				Timestamp:     entry.Timestamp,
			})
		}
	}

	// Get all deletions
	deleted, err := sm.db.GetAllDeleted()
	if err == nil {
		for _, path := range deleted {
			changes = append(changes, Change{
				Type:      ChangeDelete,
				Path:      path,
				Timestamp: time.Now(), // Exact time not stored for deletions
			})
		}
	}

	return changes
}

// GetSessionInfo returns session information for status display.
func (sm *SessionManager) GetSessionInfo() (id string, createdAt time.Time, changeCount int, ok bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return "", time.Time{}, 0, false
	}

	count := 0
	if sm.db != nil {
		count = sm.db.FileCount() + sm.db.DeletedCount()
	}

	return sm.current.ID, sm.current.CreatedAt, count, true
}

// generateSessionID creates a random UUID for session identification.
func generateSessionID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

// CreateUserRootDir creates a user directory at filesystem root.
func (sm *SessionManager) CreateUserRootDir(name string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}

	// Create local directory
	localPath := filepath.Join(sm.current.BasePath, name)
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return fmt.Errorf("create user root dir: %w", err)
	}

	// Record in DB
	if err := sm.db.PutUserDir(name); err != nil {
		return fmt.Errorf("record user dir: %w", err)
	}

	// Also record as a directory entry
	entry := FileEntry{
		Type:       FileEntryDir,
		LocalPath:  localPath,
		Mode:       0755,
		Mtime:      time.Now().Unix(),
		ChangeType: ChangeUserRootDir,
		Timestamp:  time.Now(),
	}
	if err := sm.db.PutFile(name, entry); err != nil {
		return fmt.Errorf("record user dir entry: %w", err)
	}

	sm.logger.Info("created user root directory", "name", name)
	return nil
}

// RemoveUserRootDir removes a user-created root directory.
func (sm *SessionManager) RemoveUserRootDir(name string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}

	// Remove local directory
	localPath := filepath.Join(sm.current.BasePath, name)
	if err := os.RemoveAll(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove user root dir: %w", err)
	}

	// Remove from DB
	if err := sm.db.RemoveUserDir(name); err != nil {
		return fmt.Errorf("remove user dir record: %w", err)
	}

	// Remove file entry too
	sm.db.DeleteFile(name)

	sm.logger.Info("removed user root directory", "name", name)
	return nil
}

// IsUserRootDir checks if a name is a user-created root directory.
func (sm *SessionManager) IsUserRootDir(name string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return false
	}

	return sm.db.IsUserDir(name)
}

// ListUserRootDirs returns all active user-created root directories.
func (sm *SessionManager) ListUserRootDirs() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return nil
	}

	return sm.db.ListUserDirs()
}

// CreateSymlink creates a symlink with the given target.
func (sm *SessionManager) CreateSymlink(linkPath, target string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil || sm.db == nil {
		return fmt.Errorf("no active session")
	}

	localPath := filepath.Join(sm.current.BasePath, linkPath)

	// Create parent directories
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("create symlink parent: %w", err)
	}

	// Create the symlink on disk
	if err := os.Symlink(target, localPath); err != nil {
		return fmt.Errorf("create symlink: %w", err)
	}

	// Record in DB
	entry := FileEntry{
		Type:          FileEntrySymlink,
		LocalPath:     localPath,
		Mode:          0777,
		Size:          uint64(len(target)),
		SymlinkTarget: target,
		ChangeType:    ChangeSymlink,
		Timestamp:     time.Now(),
	}
	if err := sm.db.PutFile(linkPath, entry); err != nil {
		return fmt.Errorf("record symlink: %w", err)
	}

	sm.logger.Debug("created symlink", "path", linkPath, "target", target)
	return nil
}

// GetSymlinkTarget returns the target of a symlink, or empty if not a symlink.
func (sm *SessionManager) GetSymlinkTarget(linkPath string) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return "", false
	}

	entry, found, _ := sm.db.GetFile(linkPath)
	if !found || entry.Type != FileEntrySymlink {
		return "", false
	}
	return entry.SymlinkTarget, true
}

// IsSymlink checks if a path is a symlink in the current session.
func (sm *SessionManager) IsSymlink(monofsPath string) bool {
	_, ok := sm.GetSymlinkTarget(monofsPath)
	return ok
}

// GetOverlayDB returns the overlay database for direct access.
// Used by OverlayManager for directory listing queries.
func (sm *SessionManager) GetOverlayDB() *OverlayDB {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.db
}

// BlobFileEntry carries enough information for collectBlobFiles to handle
// regular files, symlinks (resolved to content), and empty directories.
type BlobFileEntry struct {
	LocalPath     string
	Type          FileEntryType
	SymlinkTarget string
	Mode          uint32
}

// GetAllBlobFiles returns all overlay entries whose monofs path starts with
// "dependency/". Includes regular files, symlinks, and directories so that
// push can ingest them all into the cluster backend.
func (sm *SessionManager) GetAllBlobFiles() map[string]BlobFileEntry {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make(map[string]BlobFileEntry)

	if sm.current == nil || sm.db == nil {
		return result
	}

	files, err := sm.db.GetAllFiles()
	if err != nil {
		sm.logger.Warn("GetAllBlobFiles: failed to read overlay", "error", err)
		return result
	}

	const depPrefix = "dependency/"
	for monofsPath, entry := range files {
		if len(monofsPath) > len(depPrefix) && monofsPath[:len(depPrefix)] == depPrefix {
			result[monofsPath] = BlobFileEntry{
				LocalPath:     entry.LocalPath,
				Type:          entry.Type,
				SymlinkTarget: entry.SymlinkTarget,
				Mode:          entry.Mode,
			}
		}
	}

	return result
}

// GetDeletedBlobPaths returns the monofs-relative paths of all dependency
// files that were deleted in the overlay (tracked in bucketOverlayDeleted).
// Each returned path has the leading "dependency/" prefix stripped, matching
// the convention used by GetAllBlobFiles / IngestBlobs.
func (sm *SessionManager) GetDeletedBlobPaths() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return nil
	}

	deleted, err := sm.db.GetAllDeleted()
	if err != nil {
		sm.logger.Warn("GetDeletedBlobPaths: failed to read overlay", "error", err)
		return nil
	}

	const depPrefix = "dependency/"
	var paths []string
	for _, p := range deleted {
		if len(p) > len(depPrefix) && p[:len(depPrefix)] == depPrefix {
			paths = append(paths, p[len(depPrefix):])
		}
	}
	return paths
}

// RemoveBlobChanges removes all dependency-prefixed entries from the overlay
// database and deletes their local files from disk. Called after a successful
// push-deps so the session only retains non-dependency changes.
//
// This is split into two phases so the caller can invalidate the kernel's
// dentry cache between the DB cleanup and the disk cleanup:
//
//  1. RemoveBlobChanges() — cleans overlay DB entries only.
//  2. RemoveBlobDisk() — bulk-removes the dependency/ tree from disk.
//
// The kernel MUST be told to forget dentries (NotifyEntry) AFTER phase 1
// (so FUSE lookups no longer resolve to overlay) but BEFORE phase 2 (so
// no in-flight FUSE reads hit half-deleted files).
func (sm *SessionManager) RemoveBlobChanges() (int, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil || sm.db == nil {
		return 0, nil
	}

	const depPrefix = "dependency/"
	removed := 0

	sm.logger.Info("removing blob changes from overlay DB", "session", sm.current.ID)

	isDep := func(p string) bool {
		return p == "dependency" || (len(p) > len(depPrefix) && p[:len(depPrefix)] == depPrefix)
	}

	// Remove dependency file entries from the overlay DB.
	// Disk files are NOT touched here — see RemoveBlobDisk().
	files, err := sm.db.GetAllFiles()
	if err == nil {
		for monofsPath := range files {
			if isDep(monofsPath) {
				sm.db.DeleteFile(monofsPath)
				removed++
			}
		}
	}

	// Remove dependency deletion markers from the overlay DB.
	deleted, err := sm.db.GetAllDeleted()
	if err == nil {
		for _, path := range deleted {
			if isDep(path) {
				sm.db.UnmarkDeleted(path)
				removed++
			}
		}
	}

	// Remove "dependency" from user root dirs so readdir falls through to backend.
	sm.db.RemoveUserDir("dependency")

	sm.logger.Info("blob DB entries removed", "session", sm.current.ID, "removed", removed)
	return removed, nil
}

// RemoveBlobDisk removes the entire dependency/ subtree from the overlay's
// on-disk session directory. Must be called AFTER the kernel dentry cache has
// been invalidated (so no in-flight FUSE operations reference these files).
//
// The directory is atomically renamed to a temp name first to prevent new
// FUSE writes from landing in it while removal is in progress.
func (sm *SessionManager) RemoveBlobDisk() {
	sm.mu.RLock()
	current := sm.current
	sm.mu.RUnlock()

	if current == nil {
		return
	}

	depDir := filepath.Join(current.BasePath, "dependency")
	if _, statErr := os.Stat(depDir); statErr != nil {
		return // nothing on disk
	}

	tmpDir := depDir + ".cleanup." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if renameErr := os.Rename(depDir, tmpDir); renameErr == nil {
		if err := forceRemoveAll(tmpDir); err != nil {
			sm.logger.Error("forceRemoveAll failed", "dir", tmpDir, "error", err)
		}
	} else {
		sm.logger.Warn("rename before removal failed, removing directly",
			"dir", depDir, "error", renameErr)
		if err := forceRemoveAll(depDir); err != nil {
			sm.logger.Error("forceRemoveAll failed", "dir", depDir, "error", err)
		}
	}

	sm.logger.Info("blob disk files removed", "session", current.ID)
}

// GetDependencyFilePaths returns a sample of dependency file paths for verification.
// This is used during atomic cleanup to verify the backend has the files before
// removing overlay entries. Returns up to maxFiles paths.
func (sm *SessionManager) GetDependencyFilePaths(maxFiles int) []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil || sm.db == nil {
		return nil
	}

	const depPrefix = "dependency/"
	var paths []string

	// Get all files from overlay DB
	files, err := sm.db.GetAllFiles()
	if err != nil {
		sm.logger.Debug("failed to get files for verification", "error", err)
		return nil
	}

	// Filter for dependency files
	for monofsPath := range files {
		if len(paths) >= maxFiles {
			break
		}
		if strings.HasPrefix(monofsPath, depPrefix) {
			// Strip the "dependency/" prefix for backend verification
			paths = append(paths, monofsPath)
		}
	}

	sm.logger.Debug("sampled dependency files for verification",
		"sample_size", len(paths), "requested", maxFiles)
	return paths
}

// forceRemoveFile removes a single file, making its parent directory writable
// first if needed. Go module cache files (0444) live inside 0555 directories —
// chmod-ing the file itself is not enough; the parent dir must be writable for
// the unlink syscall to succeed.
func forceRemoveFile(path string) error {
	if err := os.Remove(path); err == nil {
		return nil
	}
	// Make parent writable and retry.
	parent := filepath.Dir(path)
	_ = os.Chmod(parent, 0755)
	return os.Remove(path)
}

// forceRemoveAll is like os.RemoveAll but walks the tree first and makes every
// directory writable (0755) so that files inside read-only dirs (0555, e.g. Go
// module cache) can be unlinked.
func forceRemoveAll(root string) error {
	// First pass: chmod all dirs to 0755 so children are deletable.
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0755)
		}
		return nil
	})
	return os.RemoveAll(root)
}
