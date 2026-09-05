package fuse

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionManager_StartSession(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Auto-start creates a session immediately
	if !sm.HasActiveSession() {
		t.Error("expected active session from auto-start")
	}

	// Get the auto-started session
	id1, _, _, ok := sm.GetSessionInfo()
	if !ok {
		t.Fatal("expected session info")
	}
	if id1 == "" {
		t.Error("session ID should not be empty")
	}

	// Starting again should return same session
	session2, err := sm.StartSession()
	if err != nil {
		t.Fatalf("second StartSession failed: %v", err)
	}

	if session2.ID != id1 {
		t.Error("expected same session ID on second start")
	}
}

func TestSessionManager_TrackChange(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Session is auto-started, track some changes directly
	err = sm.TrackChange(ChangeCreate, "github.com/user/repo/new.txt", "")
	if err != nil {
		t.Fatalf("TrackChange failed: %v", err)
	}

	err = sm.TrackChange(ChangeModify, "github.com/user/repo/existing.txt", "abc123")
	if err != nil {
		t.Fatalf("TrackChange failed: %v", err)
	}

	err = sm.TrackChange(ChangeDelete, "github.com/user/repo/deleted.txt", "")
	if err != nil {
		t.Fatalf("TrackChange failed: %v", err)
	}

	// Verify changes (NutsDB BTree orders by key, not insertion order)
	changes := sm.GetChanges()
	if len(changes) != 3 {
		t.Errorf("expected 3 changes, got %d", len(changes))
	}

	// Verify all change types are present
	typeSet := map[ChangeType]bool{}
	for _, c := range changes {
		typeSet[c.Type] = true
	}
	if !typeSet[ChangeCreate] {
		t.Error("expected a create change")
	}
	if !typeSet[ChangeModify] {
		t.Error("expected a modify change")
	}
	if !typeSet[ChangeDelete] {
		t.Error("expected a delete change")
	}
}

func TestSessionManager_CommitSession(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Session is auto-started, get its ID
	sessionID, _, _, ok := sm.GetSessionInfo()
	if !ok {
		t.Fatal("expected auto-started session")
	}

	// Commit
	err = sm.CommitSession()
	if err != nil {
		t.Fatalf("CommitSession failed: %v", err)
	}

	// Should no longer have active session
	if sm.HasActiveSession() {
		t.Error("expected no active session after commit")
	}

	// Check committed directory exists
	committedDir := filepath.Join(tmpDir, "committed")
	entries, err := os.ReadDir(committedDir)
	if err != nil {
		t.Fatalf("failed to read committed dir: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("expected 1 committed session, got %d", len(entries))
	}

	// Verify it contains our session ID (first 8 chars)
	if len(entries) > 0 && len(sessionID) >= 8 {
		archiveName := entries[0].Name()
		if archiveName[len(archiveName)-8:] != sessionID[:8] {
			t.Errorf("archive name should end with session ID prefix")
		}
	}
}

func TestSessionManager_DiscardSession(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Session is auto-started, get its path
	_, _, _, ok := sm.GetSessionInfo()
	if !ok {
		t.Fatal("expected auto-started session")
	}
	sessionPath := sm.current.BasePath

	// Verify session directory exists
	if _, err := os.Stat(sessionPath); err != nil {
		t.Errorf("session directory should exist: %v", err)
	}

	// Discard
	err = sm.DiscardSession()
	if err != nil {
		t.Fatalf("DiscardSession failed: %v", err)
	}

	// Should no longer have active session
	if sm.HasActiveSession() {
		t.Error("expected no active session after discard")
	}

	// Session directory should be removed
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Error("session directory should be removed after discard")
	}
}

func TestSessionManager_LocalOverride(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Session is auto-started; no override for non-existent file
	if sm.HasLocalOverride("test/file.txt") {
		t.Error("should not have override for non-existent file")
	}

	// Create a local file
	localPath, err := sm.GetLocalPath("github.com/user/repo/test.txt")
	if err != nil {
		t.Fatalf("GetLocalPath failed: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	if err := os.WriteFile(localPath, []byte("test content"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Track the change so the DB knows about it
	err = sm.TrackChange(ChangeCreate, "github.com/user/repo/test.txt", "")
	if err != nil {
		t.Fatalf("TrackChange failed: %v", err)
	}

	// Should have override now
	if !sm.HasLocalOverride("github.com/user/repo/test.txt") {
		t.Error("should have override after creating local file")
	}
}

func TestSessionManager_IsDeleted(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Session is auto-started
	testPath := "github.com/user/repo/deleted.txt"

	// Not deleted initially
	if sm.IsDeleted(testPath) {
		t.Error("file should not be deleted initially")
	}

	// Track deletion
	err = sm.TrackChange(ChangeDelete, testPath, "")
	if err != nil {
		t.Fatalf("TrackChange failed: %v", err)
	}

	// Should be marked as deleted
	if !sm.IsDeleted(testPath) {
		t.Error("file should be marked as deleted after TrackChange")
	}
}

func TestSessionManager_RecoverSession(t *testing.T) {
	tmpDir := t.TempDir()

	// Create first session manager (auto-starts a session)
	sm1, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Get the auto-started session ID
	sessionID, _, _, ok := sm1.GetSessionInfo()
	if !ok {
		t.Fatal("expected auto-started session")
	}

	// Track a change — also create the file on disk so RebuildFromDisk
	// can rediscover it during recovery (matching what FUSE ops do).
	localPath := filepath.Join(sm1.GetCurrentSession().BasePath, "test", "file.txt")
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(localPath, []byte("content"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	err = sm1.TrackChange(ChangeCreate, "test/file.txt", "")
	if err != nil {
		t.Fatalf("TrackChange failed: %v", err)
	}

	// Close sm1's DB before creating sm2 (simulates process exit)
	if sm1.db != nil {
		sm1.db.Close()
		sm1.db = nil
	}
	sm1 = nil // help GC release any file locks

	// Create new session manager (simulates restart)
	sm2, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("second NewSessionManager failed: %v", err)
	}

	// Should have recovered the session
	if !sm2.HasActiveSession() {
		t.Error("expected session to be recovered")
	}

	// Session ID should match
	id, _, changeCount, ok := sm2.GetSessionInfo()
	if !ok {
		t.Fatal("expected to get session info")
	}

	if id != sessionID {
		t.Errorf("expected session ID %s, got %s", sessionID, id)
	}

	if changeCount != 1 {
		t.Errorf("expected 1 change, got %d", changeCount)
	}
}

func TestSessionManager_GetSessionInfo(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Session is auto-started, so we should have info immediately
	id, createdAt, changeCount, ok := sm.GetSessionInfo()
	if !ok {
		t.Fatal("expected session info from auto-started session")
	}

	if id == "" {
		t.Error("expected non-empty session ID")
	}

	if createdAt.IsZero() {
		t.Error("expected non-zero createdAt")
	}

	if changeCount != 0 {
		t.Errorf("expected 0 changes, got %d", changeCount)
	}
}

func TestSessionManager_UserRootDir(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Session is auto-started, so initially no user root dirs
	dirs := sm.ListUserRootDirs()
	if len(dirs) != 0 {
		t.Errorf("expected 0 user root dirs, got %d", len(dirs))
	}

	// Create user root dir
	err = sm.CreateUserRootDir("mydir")
	if err != nil {
		t.Fatalf("CreateUserRootDir failed: %v", err)
	}

	// Verify it exists
	if !sm.IsUserRootDir("mydir") {
		t.Error("expected mydir to be a user root dir")
	}

	dirs = sm.ListUserRootDirs()
	if len(dirs) != 1 {
		t.Errorf("expected 1 user root dir, got %d", len(dirs))
	}
	if dirs[0] != "mydir" {
		t.Errorf("expected mydir, got %s", dirs[0])
	}

	// Create another user root dir
	err = sm.CreateUserRootDir("otherdir")
	if err != nil {
		t.Fatalf("CreateUserRootDir failed: %v", err)
	}

	dirs = sm.ListUserRootDirs()
	if len(dirs) != 2 {
		t.Errorf("expected 2 user root dirs, got %d", len(dirs))
	}

	// Remove one
	err = sm.RemoveUserRootDir("mydir")
	if err != nil {
		t.Fatalf("RemoveUserRootDir failed: %v", err)
	}

	// Verify it's gone
	if sm.IsUserRootDir("mydir") {
		t.Error("expected mydir to no longer be a user root dir")
	}

	dirs = sm.ListUserRootDirs()
	if len(dirs) != 1 {
		t.Errorf("expected 1 user root dir after removal, got %d", len(dirs))
	}
	if dirs[0] != "otherdir" {
		t.Errorf("expected otherdir, got %s", dirs[0])
	}

	// Non-existent dir should not be a user root dir
	if sm.IsUserRootDir("nonexistent") {
		t.Error("nonexistent should not be a user root dir")
	}
}

func TestSessionManager_Symlink(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Session is auto-started, create symlink directly
	err = sm.CreateSymlink("repo/link", "/target/path")
	if err != nil {
		t.Fatalf("CreateSymlink failed: %v", err)
	}

	// Verify symlink exists
	if !sm.IsSymlink("repo/link") {
		t.Error("expected repo/link to be a symlink")
	}

	// Get symlink target
	target, ok := sm.GetSymlinkTarget("repo/link")
	if !ok {
		t.Error("expected to find symlink target")
	}
	if target != "/target/path" {
		t.Errorf("expected target /target/path, got %s", target)
	}

	// Non-symlink should return false
	if sm.IsSymlink("repo/notalink") {
		t.Error("repo/notalink should not be a symlink")
	}

	_, ok = sm.GetSymlinkTarget("repo/notalink")
	if ok {
		t.Error("expected no target for non-symlink")
	}

	// Create another symlink with relative target
	err = sm.CreateSymlink("repo/rellink", "../other/file")
	if err != nil {
		t.Fatalf("CreateSymlink failed: %v", err)
	}

	target, ok = sm.GetSymlinkTarget("repo/rellink")
	if !ok {
		t.Error("expected to find symlink target")
	}
	if target != "../other/file" {
		t.Errorf("expected target ../other/file, got %s", target)
	}

	// Verify local symlink was created
	localPath, _ := sm.GetLocalPath("repo/link")
	linkTarget, err := os.Readlink(localPath)
	if err != nil {
		t.Fatalf("failed to read local symlink: %v", err)
	}
	if linkTarget != "/target/path" {
		t.Errorf("expected local symlink target /target/path, got %s", linkTarget)
	}
}

func TestSessionManager_UserRootDir_RecreateAfterRemove(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// Create, remove, recreate
	sm.CreateUserRootDir("testdir")
	if !sm.IsUserRootDir("testdir") {
		t.Error("expected testdir to exist after create")
	}

	sm.RemoveUserRootDir("testdir")
	if sm.IsUserRootDir("testdir") {
		t.Error("expected testdir to not exist after remove")
	}

	sm.CreateUserRootDir("testdir")
	if !sm.IsUserRootDir("testdir") {
		t.Error("expected testdir to exist after recreate")
	}

	// Should be in the list
	dirs := sm.ListUserRootDirs()
	found := false
	for _, d := range dirs {
		if d == "testdir" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected testdir in list after recreate")
	}
}

// TestSessionManager_DeleteSessionOnlyFile verifies that deleting a file
// that was created in this session (never existed in the base layer) does
// NOT leave a phantom "[-]" deletion entry in the overlay.
// This matches the Go module download pattern: create .tmp → rename to .zip.
func TestSessionManager_DeleteSessionOnlyFile(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	tmpPath := "dependency/go/mod/cache/download/example.com/@v/v1.0.0.zip12345.tmp"
	finalPath := "dependency/go/mod/cache/download/example.com/@v/v1.0.0.zip"

	// Create the local .tmp file on disk so TrackChange can stat it
	localTmp, _ := sm.GetLocalPath(tmpPath)
	if err := os.MkdirAll(filepath.Dir(localTmp), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(localTmp, []byte("zipdata"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Step 1: create the .tmp file (simulates Go writing)
	if err := sm.TrackChange(ChangeCreate, tmpPath, ""); err != nil {
		t.Fatalf("TrackChange create .tmp: %v", err)
	}

	// Step 2: simulate rename → delete old + create new
	// Rename the file on disk first
	localFinal, _ := sm.GetLocalPath(finalPath)
	if err := os.Rename(localTmp, localFinal); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if err := sm.TrackChange(ChangeDelete, tmpPath, ""); err != nil {
		t.Fatalf("TrackChange delete .tmp: %v", err)
	}
	if err := sm.TrackChange(ChangeCreate, finalPath, ""); err != nil {
		t.Fatalf("TrackChange create final: %v", err)
	}

	// The .tmp path should NOT appear as deleted
	if sm.IsDeleted(tmpPath) {
		t.Error(".tmp file was created in this session; its deletion should not be tracked")
	}

	// The .tmp path should NOT appear in overlay files either
	if sm.HasLocalOverride(tmpPath) {
		t.Error(".tmp file should have been removed from overlay files")
	}

	// The final path should exist as a create
	if !sm.HasLocalOverride(finalPath) {
		t.Error("final path should exist in overlay")
	}

	// GetChanges should only contain the final file, not the .tmp deletion
	changes := sm.GetChanges()
	for _, c := range changes {
		if c.Path == tmpPath {
			t.Errorf("changes should not contain .tmp path, found: type=%s path=%s", c.Type, c.Path)
		}
	}

	// Verify total change count: should be 1 (create for final path)
	_, _, changeCount, _ := sm.GetSessionInfo()
	if changeCount != 1 {
		t.Errorf("expected 1 change (final file), got %d", changeCount)
	}
}
