package fuse

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionManager_StartSession(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Initially no session
	if sm.HasActiveSession() {
		t.Error("expected no active session initially")
	}

	// Start session
	session, err := sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	if session.ID == "" {
		t.Error("session ID should not be empty")
	}

	if !sm.HasActiveSession() {
		t.Error("expected active session after start")
	}

	// Starting again should return same session
	session2, err := sm.StartSession()
	if err != nil {
		t.Fatalf("second StartSession failed: %v", err)
	}

	if session2.ID != session.ID {
		t.Error("expected same session ID on second start")
	}
}

func TestSessionManager_TrackChange(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Track change without session should fail
	err = sm.TrackChange(ChangeCreate, "test/file.txt", "")
	if err == nil {
		t.Error("expected error when tracking change without session")
	}

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// Track some changes
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

	// Verify changes
	changes := sm.GetChanges()
	if len(changes) != 3 {
		t.Errorf("expected 3 changes, got %d", len(changes))
	}

	if changes[0].Type != ChangeCreate {
		t.Errorf("expected first change to be create, got %s", changes[0].Type)
	}
	if changes[1].Type != ChangeModify {
		t.Errorf("expected second change to be modify, got %s", changes[1].Type)
	}
	if changes[2].Type != ChangeDelete {
		t.Errorf("expected third change to be delete, got %s", changes[2].Type)
	}
}

func TestSessionManager_CommitSession(t *testing.T) {
	tmpDir := t.TempDir()

	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Commit without session should fail
	err = sm.CommitSession()
	if err == nil {
		t.Error("expected error when committing without session")
	}

	// Start session
	session, err := sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	sessionID := session.ID

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

	// Start session
	session, err := sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	sessionPath := session.BasePath

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

	// No override without session
	if sm.HasLocalOverride("test/file.txt") {
		t.Error("should not have override without session")
	}

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// No override for non-existent file
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

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

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

	// Create first session manager and start a session
	sm1, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	session, err := sm1.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	sessionID := session.ID

	// Track a change
	err = sm1.TrackChange(ChangeCreate, "test/file.txt", "")
	if err != nil {
		t.Fatalf("TrackChange failed: %v", err)
	}

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

	// No session initially
	_, _, _, ok := sm.GetSessionInfo()
	if ok {
		t.Error("expected no session info initially")
	}

	// Start session
	beforeStart := time.Now()
	session, err := sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	afterStart := time.Now()

	// Get session info
	id, createdAt, changeCount, ok := sm.GetSessionInfo()
	if !ok {
		t.Fatal("expected session info")
	}

	if id != session.ID {
		t.Errorf("expected session ID %s, got %s", session.ID, id)
	}

	if createdAt.Before(beforeStart) || createdAt.After(afterStart) {
		t.Error("createdAt should be between beforeStart and afterStart")
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

	// Create user root dir without session should fail
	err = sm.CreateUserRootDir("mydir")
	if err == nil {
		t.Error("expected error when creating user root dir without session")
	}

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// Initially no user root dirs
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

	// Create symlink without session should fail
	err = sm.CreateSymlink("repo/link", "/target/path")
	if err == nil {
		t.Error("expected error when creating symlink without session")
	}

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// Create symlink
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
