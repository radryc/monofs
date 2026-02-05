// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// Change represents a single file change in a write session
type Change struct {
	Type          ChangeType `json:"type"`                     // Type of change
	Path          string     `json:"path"`                     // Full MonoFS path (github.com/user/repo/file.txt)
	LocalPath     string     `json:"local_path"`               // Path in session directory
	OrigHash      string     `json:"orig_hash"`                // Original blob hash (for modify operations)
	SymlinkTarget string     `json:"symlink_target,omitempty"` // Target path for symlinks
	Timestamp     time.Time  `json:"timestamp"`                // When the change occurred
}

// WriteSession represents an active write session
type WriteSession struct {
	ID        string    `json:"id"`         // Unique session identifier (UUID)
	CreatedAt time.Time `json:"created_at"` // Session creation time
	BasePath  string    `json:"base_path"`  // Path to session directory
	Changes   []Change  `json:"changes"`    // Tracked changes

	mu sync.RWMutex

	// Performance indexes - O(1) lookup instead of O(n) iteration
	// These are rebuilt from Changes on load and updated on each AddChange
	deletedPaths  map[string]bool   `json:"-"` // paths marked as deleted
	symlinkPaths  map[string]string `json:"-"` // path -> symlink target
	userRootDirs  map[string]bool   `json:"-"` // user-created root directories
	localOverride map[string]bool   `json:"-"` // cached HasLocalOverride results
}

// SessionManager manages write sessions for the FUSE client
type SessionManager struct {
	overlayBase string        // Base path for overlay storage (~/.monofs/overlay)
	current     *WriteSession // Currently active session
	mu          sync.RWMutex
	logger      *slog.Logger
}

// NewSessionManager creates a new session manager
func NewSessionManager(overlayBase string, logger *slog.Logger) (*SessionManager, error) {
	if logger == nil {
		logger = slog.Default()
	}

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
	if err := sm.recoverSession(); err != nil {
		logger.Warn("session recovery failed", "error", err)
	}

	return sm, nil
}

// recoverSession attempts to recover an active session after a crash
func (sm *SessionManager) recoverSession() error {
	currentLink := filepath.Join(sm.overlayBase, "current")

	// Check if current symlink exists
	target, err := os.Readlink(currentLink)
	if err != nil {
		// No current session
		return nil
	}

	// Load session metadata
	sessionPath := target
	if !filepath.IsAbs(target) {
		sessionPath = filepath.Join(sm.overlayBase, target)
	}

	metaPath := filepath.Join(sessionPath, ".session.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		sm.logger.Warn("failed to read session metadata", "path", metaPath, "error", err)
		return err
	}

	var session WriteSession
	if err := json.Unmarshal(data, &session); err != nil {
		sm.logger.Warn("failed to parse session metadata", "error", err)
		return err
	}

	session.BasePath = sessionPath
	session.rebuildIndexes() // Rebuild O(1) lookup indexes from changes
	sm.current = &session
	sm.logger.Info("recovered active session", "id", session.ID, "changes", len(session.Changes))

	return nil
}

// StartSession creates a new write session or returns the existing one
func (sm *SessionManager) StartSession() (*WriteSession, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current != nil {
		return sm.current, nil // Session already active
	}

	// Generate UUID
	id := generateSessionID()
	basePath := filepath.Join(sm.overlayBase, "sessions", id)

	session := &WriteSession{
		ID:            id,
		CreatedAt:     time.Now(),
		BasePath:      basePath,
		Changes:       []Change{},
		deletedPaths:  make(map[string]bool),
		symlinkPaths:  make(map[string]string),
		userRootDirs:  make(map[string]bool),
		localOverride: make(map[string]bool),
	}

	// Create session directory
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(basePath, ".changes"), 0755); err != nil {
		return nil, fmt.Errorf("create changes dir: %w", err)
	}

	// Write session metadata
	if err := session.save(); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	// Update current symlink
	currentLink := filepath.Join(sm.overlayBase, "current")
	os.Remove(currentLink) // Ignore error
	if err := os.Symlink(basePath, currentLink); err != nil {
		sm.logger.Warn("failed to create current symlink", "error", err)
	}

	sm.current = session
	sm.logger.Info("started write session", "id", id)

	return session, nil
}

// GetCurrentSession returns the current active session (nil if none)
func (sm *SessionManager) GetCurrentSession() *WriteSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current
}

// HasActiveSession returns true if there's an active write session
func (sm *SessionManager) HasActiveSession() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current != nil
}

// FlushChanges forces a save of pending changes to disk
func (sm *SessionManager) FlushChanges() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return nil
	}

	return sm.current.save()
}

// CommitSession finalizes the current session and archives it
func (sm *SessionManager) CommitSession() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return fmt.Errorf("no active session")
	}

	session := sm.current

	// Ensure all changes are saved before archiving
	if err := session.save(); err != nil {
		return fmt.Errorf("flush changes: %w", err)
	}

	// Archive to committed/
	timestamp := time.Now().Format("20060102-150405")
	archiveName := fmt.Sprintf("%s-%s", timestamp, session.ID[:8])
	archivePath := filepath.Join(sm.overlayBase, "committed", archiveName)

	if err := os.Rename(session.BasePath, archivePath); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}

	// Remove current symlink
	os.Remove(filepath.Join(sm.overlayBase, "current"))

	sm.logger.Info("committed session", "id", session.ID, "archive", archiveName)
	sm.current = nil

	return nil
}

// DiscardSession abandons the current session and removes all local changes
func (sm *SessionManager) DiscardSession() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return nil
	}

	// Remove session directory
	if err := os.RemoveAll(sm.current.BasePath); err != nil {
		return fmt.Errorf("remove session: %w", err)
	}

	os.Remove(filepath.Join(sm.overlayBase, "current"))

	sm.logger.Info("discarded session", "id", sm.current.ID)
	sm.current = nil

	return nil
}

// PathState contains consolidated session state for a path.
// Used to gather all session state in a single lock acquisition.
type PathState struct {
	HasSession    bool   // Whether there's an active session
	IsDeleted     bool   // Path is marked as deleted
	IsSymlink     bool   // Path is a symlink
	SymlinkTarget string // Symlink target (if IsSymlink)
	HasOverride   bool   // Path has local modifications
	IsUserRootDir bool   // Path is a user-created root directory (only valid for root-level names)
	LocalPath     string // Full local filesystem path
}

// GetPathState returns consolidated session state for a path in a single lock acquisition.
// This is much faster than calling IsDeleted, GetSymlinkTarget, HasLocalOverride separately.
func (sm *SessionManager) GetPathState(monofsPath string) PathState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return PathState{HasSession: false}
	}

	sm.current.mu.RLock()
	defer sm.current.mu.RUnlock()

	state := PathState{
		HasSession: true,
		LocalPath:  filepath.Join(sm.current.BasePath, monofsPath),
	}

	// O(1) lookups using indexed maps
	if sm.current.deletedPaths != nil {
		state.IsDeleted = sm.current.deletedPaths[monofsPath]
	}

	if sm.current.symlinkPaths != nil {
		if target, ok := sm.current.symlinkPaths[monofsPath]; ok {
			state.IsSymlink = true
			state.SymlinkTarget = target
		}
	}

	if sm.current.userRootDirs != nil {
		state.IsUserRootDir = sm.current.userRootDirs[monofsPath]
	}

	// Check localOverride cache
	if sm.current.localOverride != nil {
		if cached, ok := sm.current.localOverride[monofsPath]; ok {
			state.HasOverride = cached
		}
	}

	return state
}

// GetLocalPath returns the local overlay path for a MonoFS path
func (sm *SessionManager) GetLocalPath(monofsPath string) (string, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return "", fmt.Errorf("no active session")
	}

	return filepath.Join(sm.current.BasePath, monofsPath), nil
}

// HasLocalOverride checks if a file has been modified locally.
// Uses cached index for O(1) lookup, with fallback to disk check.
func (sm *SessionManager) HasLocalOverride(monofsPath string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return false
	}

	sm.current.mu.RLock()
	// Fast path: check cached index first
	if sm.current.localOverride != nil {
		if cached, ok := sm.current.localOverride[monofsPath]; ok {
			sm.current.mu.RUnlock()
			return cached
		}
	}
	sm.current.mu.RUnlock()

	// Fallback: check filesystem (for files created outside our tracking)
	localPath := filepath.Join(sm.current.BasePath, monofsPath)
	_, err := os.Stat(localPath)
	exists := err == nil

	// Cache the result
	if exists {
		sm.current.mu.Lock()
		if sm.current.localOverride == nil {
			sm.current.localOverride = make(map[string]bool)
		}
		sm.current.localOverride[monofsPath] = true
		sm.current.mu.Unlock()
	}

	return exists
}

// IsDeleted checks if a path has been marked as deleted in the current session.
// Uses O(1) indexed lookup instead of iterating through all changes.
func (sm *SessionManager) IsDeleted(monofsPath string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return false
	}

	sm.current.mu.RLock()
	defer sm.current.mu.RUnlock()

	if sm.current.deletedPaths == nil {
		return false
	}
	return sm.current.deletedPaths[monofsPath]
}

// TrackChange records a change in the session
func (sm *SessionManager) TrackChange(changeType ChangeType, monofsPath, origHash string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return fmt.Errorf("no active session")
	}

	localPath := filepath.Join(sm.current.BasePath, monofsPath)

	change := Change{
		Type:      changeType,
		Path:      monofsPath,
		LocalPath: localPath,
		OrigHash:  origHash,
		Timestamp: time.Now(),
	}

	sm.current.mu.Lock()
	sm.current.Changes = append(sm.current.Changes, change)
	sm.current.updateIndex(change) // Update O(1) indexes
	changeCount := len(sm.current.Changes)
	sm.current.mu.Unlock()

	// Batch saves: only save every 100 changes or for non-delete operations
	// This dramatically improves performance for bulk deletes while keeping safety for writes
	if changeType != ChangeDelete && changeType != ChangeRmdir {
		return sm.current.save()
	}

	// For deletes, batch save every 100 changes
	if changeCount%100 == 0 {
		return sm.current.save()
	}

	return nil
}

// GetChanges returns all changes in the current session
func (sm *SessionManager) GetChanges() []Change {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return nil
	}

	sm.current.mu.RLock()
	defer sm.current.mu.RUnlock()

	changes := make([]Change, len(sm.current.Changes))
	copy(changes, sm.current.Changes)
	return changes
}

// GetSessionInfo returns session information for status display
func (sm *SessionManager) GetSessionInfo() (id string, createdAt time.Time, changeCount int, ok bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return "", time.Time{}, 0, false
	}

	sm.current.mu.RLock()
	defer sm.current.mu.RUnlock()

	return sm.current.ID, sm.current.CreatedAt, len(sm.current.Changes), true
}

// save persists the session metadata to disk
func (s *WriteSession) save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(s.BasePath, ".session.json"), data, 0644)
}

// rebuildIndexes rebuilds O(1) lookup indexes from the Changes slice.
// Called after loading session from disk or after JSON unmarshal.
func (s *WriteSession) rebuildIndexes() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Initialize maps
	s.deletedPaths = make(map[string]bool)
	s.symlinkPaths = make(map[string]string)
	s.userRootDirs = make(map[string]bool)
	s.localOverride = make(map[string]bool)

	// Replay all changes to build current state
	for _, change := range s.Changes {
		switch change.Type {
		case ChangeDelete, ChangeRmdir:
			s.deletedPaths[change.Path] = true
			delete(s.symlinkPaths, change.Path)
		case ChangeCreate, ChangeModify, ChangeMkdir:
			delete(s.deletedPaths, change.Path)
			s.localOverride[change.Path] = true
		case ChangeSymlink:
			s.symlinkPaths[change.Path] = change.SymlinkTarget
			delete(s.deletedPaths, change.Path)
		case ChangeUserRootDir:
			s.userRootDirs[change.Path] = true
		case ChangeRemoveUserRootDir:
			delete(s.userRootDirs, change.Path)
		}
	}
}

// updateIndex updates indexes when a new change is added.
// Must be called with s.mu held for writing.
func (s *WriteSession) updateIndex(change Change) {
	switch change.Type {
	case ChangeDelete, ChangeRmdir:
		s.deletedPaths[change.Path] = true
		delete(s.symlinkPaths, change.Path)
		delete(s.localOverride, change.Path)
	case ChangeCreate, ChangeModify, ChangeMkdir:
		delete(s.deletedPaths, change.Path)
		s.localOverride[change.Path] = true
	case ChangeSymlink:
		s.symlinkPaths[change.Path] = change.SymlinkTarget
		delete(s.deletedPaths, change.Path)
	case ChangeUserRootDir:
		s.userRootDirs[change.Path] = true
	case ChangeRemoveUserRootDir:
		delete(s.userRootDirs, change.Path)
	}
}

// generateSessionID creates a random UUID for session identification
func generateSessionID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to timestamp-based ID
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

// CreateUserRootDir creates a user directory at filesystem root
func (sm *SessionManager) CreateUserRootDir(name string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return fmt.Errorf("no active session")
	}

	// Create local directory
	localPath := filepath.Join(sm.current.BasePath, name)
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return fmt.Errorf("create user root dir: %w", err)
	}

	change := Change{
		Type:      ChangeUserRootDir,
		Path:      name,
		LocalPath: localPath,
		Timestamp: time.Now(),
	}

	sm.current.mu.Lock()
	sm.current.Changes = append(sm.current.Changes, change)
	sm.current.updateIndex(change) // Update O(1) indexes
	sm.current.mu.Unlock()

	sm.logger.Info("created user root directory", "name", name)
	return sm.current.save()
}

// RemoveUserRootDir removes a user-created root directory
func (sm *SessionManager) RemoveUserRootDir(name string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return fmt.Errorf("no active session")
	}

	// Remove local directory
	localPath := filepath.Join(sm.current.BasePath, name)
	if err := os.RemoveAll(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove user root dir: %w", err)
	}

	change := Change{
		Type:      ChangeRemoveUserRootDir,
		Path:      name,
		LocalPath: localPath,
		Timestamp: time.Now(),
	}

	sm.current.mu.Lock()
	sm.current.Changes = append(sm.current.Changes, change)
	sm.current.updateIndex(change) // Update O(1) indexes
	sm.current.mu.Unlock()

	sm.logger.Info("removed user root directory", "name", name)
	return sm.current.save()
}

// IsUserRootDir checks if a name is a user-created root directory.
// Uses O(1) indexed lookup instead of iterating through all changes.
func (sm *SessionManager) IsUserRootDir(name string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return false
	}

	sm.current.mu.RLock()
	defer sm.current.mu.RUnlock()

	if sm.current.userRootDirs == nil {
		return false
	}
	return sm.current.userRootDirs[name]
}

// ListUserRootDirs returns all active user-created root directories.
// Uses O(1) indexed map instead of iterating through all changes.
func (sm *SessionManager) ListUserRootDirs() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return nil
	}

	sm.current.mu.RLock()
	defer sm.current.mu.RUnlock()

	if sm.current.userRootDirs == nil {
		return nil
	}

	result := make([]string, 0, len(sm.current.userRootDirs))
	for name := range sm.current.userRootDirs {
		result = append(result, name)
	}
	return result
}

// CreateSymlink creates a symlink with the given target
func (sm *SessionManager) CreateSymlink(linkPath, target string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return fmt.Errorf("no active session")
	}

	localPath := filepath.Join(sm.current.BasePath, linkPath)

	// Create parent directories
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("create symlink parent: %w", err)
	}

	// Create the symlink
	if err := os.Symlink(target, localPath); err != nil {
		return fmt.Errorf("create symlink: %w", err)
	}

	change := Change{
		Type:          ChangeSymlink,
		Path:          linkPath,
		LocalPath:     localPath,
		SymlinkTarget: target,
		Timestamp:     time.Now(),
	}

	sm.current.mu.Lock()
	sm.current.Changes = append(sm.current.Changes, change)
	sm.current.updateIndex(change) // Update O(1) indexes
	sm.current.mu.Unlock()

	sm.logger.Debug("created symlink", "path", linkPath, "target", target)
	return sm.current.save()
}

// GetSymlinkTarget returns the target of a symlink, or empty if not a symlink.
// Uses O(1) indexed lookup instead of reverse iterating through all changes.
func (sm *SessionManager) GetSymlinkTarget(linkPath string) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.current == nil {
		return "", false
	}

	sm.current.mu.RLock()
	defer sm.current.mu.RUnlock()

	if sm.current.symlinkPaths == nil {
		return "", false
	}
	target, ok := sm.current.symlinkPaths[linkPath]
	return target, ok
}

// IsSymlink checks if a path is a symlink in the current session
func (sm *SessionManager) IsSymlink(monofsPath string) bool {
	_, ok := sm.GetSymlinkTarget(monofsPath)
	return ok
}
