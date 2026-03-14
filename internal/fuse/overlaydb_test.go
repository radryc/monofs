package fuse

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverlayDB_OpenClose(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	// DB should exist
	dbPath := filepath.Join(dir, "overlay.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("overlay.db directory should exist: %v", err)
	}
}

func TestOverlayDB_PutGetFile(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	// Get non-existent file
	_, found, err := odb.GetFile("github.com/user/repo/file.txt")
	if err != nil {
		t.Fatalf("GetFile failed: %v", err)
	}
	if found {
		t.Error("expected file not found")
	}

	// Put a file
	entry := FileEntry{
		Type:       FileEntryRegular,
		LocalPath:  "/tmp/overlay/file.txt",
		Mode:       0644,
		Size:       100,
		Mtime:      1234567890,
		ChangeType: ChangeCreate,
	}
	if err := odb.PutFile("github.com/user/repo/file.txt", entry); err != nil {
		t.Fatalf("PutFile failed: %v", err)
	}

	// Get it back
	got, found, err := odb.GetFile("github.com/user/repo/file.txt")
	if err != nil {
		t.Fatalf("GetFile failed: %v", err)
	}
	if !found {
		t.Fatal("expected file to be found")
	}
	if got.Type != FileEntryRegular {
		t.Errorf("expected type regular, got %s", got.Type)
	}
	if got.Size != 100 {
		t.Errorf("expected size 100, got %d", got.Size)
	}
	if got.Mode != 0644 {
		t.Errorf("expected mode 0644, got %o", got.Mode)
	}
}

func TestOverlayDB_DeleteFile(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	// Put then delete
	entry := FileEntry{Type: FileEntryRegular, Size: 50}
	odb.PutFile("test/file.txt", entry)

	if err := odb.DeleteFile("test/file.txt"); err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}

	_, found, _ := odb.GetFile("test/file.txt")
	if found {
		t.Error("file should not be found after delete")
	}

	// Delete non-existent should not error
	if err := odb.DeleteFile("nonexistent"); err != nil {
		t.Errorf("DeleteFile of nonexistent should not error: %v", err)
	}
}

func TestOverlayDB_MarkDeleted(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	path := "github.com/user/repo/deleted.txt"

	if odb.IsDeleted(path) {
		t.Error("path should not be deleted initially")
	}

	if err := odb.MarkDeleted(path); err != nil {
		t.Fatalf("MarkDeleted failed: %v", err)
	}

	if !odb.IsDeleted(path) {
		t.Error("path should be deleted after MarkDeleted")
	}

	// Unmark
	if err := odb.UnmarkDeleted(path); err != nil {
		t.Fatalf("UnmarkDeleted failed: %v", err)
	}

	if odb.IsDeleted(path) {
		t.Error("path should not be deleted after UnmarkDeleted")
	}
}

func TestOverlayDB_MarkDeletedRemovesFile(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	path := "test/file.txt"

	// Put a file, then mark deleted — should remove from files bucket too
	odb.PutFile(path, FileEntry{Type: FileEntryRegular, Size: 10})
	odb.MarkDeleted(path)

	_, found, _ := odb.GetFile(path)
	if found {
		t.Error("file should be removed from files bucket after MarkDeleted")
	}

	if !odb.IsDeleted(path) {
		t.Error("path should be in deleted set")
	}
}

func TestOverlayDB_PutFileUnmarksDeleted(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	path := "test/file.txt"

	odb.MarkDeleted(path)
	if !odb.IsDeleted(path) {
		t.Fatal("should be deleted")
	}

	// Put file should remove from deleted
	odb.PutFile(path, FileEntry{Type: FileEntryRegular, Size: 20})
	if odb.IsDeleted(path) {
		t.Error("path should no longer be deleted after PutFile")
	}
}

func TestOverlayDB_UserDirs(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	// Initially empty
	dirs := odb.ListUserDirs()
	if len(dirs) != 0 {
		t.Errorf("expected 0 user dirs, got %d", len(dirs))
	}

	if odb.IsUserDir("mydir") {
		t.Error("mydir should not be a user dir initially")
	}

	// Add user dir
	if err := odb.PutUserDir("mydir"); err != nil {
		t.Fatalf("PutUserDir failed: %v", err)
	}

	if !odb.IsUserDir("mydir") {
		t.Error("mydir should be a user dir")
	}

	dirs = odb.ListUserDirs()
	if len(dirs) != 1 {
		t.Errorf("expected 1 user dir, got %d", len(dirs))
	}

	// Add another
	odb.PutUserDir("otherdir")

	dirs = odb.ListUserDirs()
	if len(dirs) != 2 {
		t.Errorf("expected 2 user dirs, got %d", len(dirs))
	}

	// Remove
	if err := odb.RemoveUserDir("mydir"); err != nil {
		t.Fatalf("RemoveUserDir failed: %v", err)
	}

	if odb.IsUserDir("mydir") {
		t.Error("mydir should no longer be a user dir")
	}

	dirs = odb.ListUserDirs()
	if len(dirs) != 1 {
		t.Errorf("expected 1 user dir, got %d", len(dirs))
	}
}

func TestOverlayDB_ListFilesUnderDir(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	// Add files at various levels
	odb.PutFile("github.com/user/repo/file1.txt", FileEntry{Type: FileEntryRegular, Size: 10})
	odb.PutFile("github.com/user/repo/file2.txt", FileEntry{Type: FileEntryRegular, Size: 20})
	odb.PutFile("github.com/user/repo/subdir/deep.txt", FileEntry{Type: FileEntryRegular, Size: 30})
	odb.PutFile("github.com/user/other/file.txt", FileEntry{Type: FileEntryRegular, Size: 40})

	// List direct children of github.com/user/repo
	entries, names, err := odb.ListFilesUnderDir("github.com/user/repo")
	if err != nil {
		t.Fatalf("ListFilesUnderDir failed: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("expected 2 direct children, got %d (names: %v)", len(entries), names)
	}

	// List direct children of github.com/user/repo/subdir
	entries, names, err = odb.ListFilesUnderDir("github.com/user/repo/subdir")
	if err != nil {
		t.Fatalf("ListFilesUnderDir failed: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("expected 1 entry in subdir, got %d (names: %v)", len(entries), names)
	}
}

func TestOverlayDB_ListDeletedUnderDir(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	odb.MarkDeleted("repo/file1.txt")
	odb.MarkDeleted("repo/file2.txt")
	odb.MarkDeleted("repo/sub/file3.txt")
	odb.MarkDeleted("other/file.txt")

	deleted, err := odb.ListDeletedUnderDir("repo")
	if err != nil {
		t.Fatalf("ListDeletedUnderDir failed: %v", err)
	}

	if len(deleted) != 2 {
		t.Errorf("expected 2 deleted under repo, got %d: %v", len(deleted), deleted)
	}
}

func TestOverlayDB_GetAllFiles(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	odb.PutFile("a/b.txt", FileEntry{Type: FileEntryRegular, Size: 1})
	odb.PutFile("c/d.txt", FileEntry{Type: FileEntryRegular, Size: 2})

	all, err := odb.GetAllFiles()
	if err != nil {
		t.Fatalf("GetAllFiles failed: %v", err)
	}

	if len(all) != 2 {
		t.Errorf("expected 2 files, got %d", len(all))
	}
}

func TestOverlayDB_GetAllDeleted(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	odb.MarkDeleted("a/b.txt")
	odb.MarkDeleted("c/d.txt")

	deleted, err := odb.GetAllDeleted()
	if err != nil {
		t.Fatalf("GetAllDeleted failed: %v", err)
	}

	if len(deleted) != 2 {
		t.Errorf("expected 2 deleted, got %d", len(deleted))
	}
}

func TestOverlayDB_Counts(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	if odb.FileCount() != 0 {
		t.Error("expected 0 files initially")
	}
	if odb.DeletedCount() != 0 {
		t.Error("expected 0 deleted initially")
	}

	odb.PutFile("a.txt", FileEntry{Type: FileEntryRegular})
	odb.PutFile("b.txt", FileEntry{Type: FileEntryRegular})
	odb.MarkDeleted("c.txt")

	if odb.FileCount() != 2 {
		t.Errorf("expected 2 files, got %d", odb.FileCount())
	}
	if odb.DeletedCount() != 1 {
		t.Errorf("expected 1 deleted, got %d", odb.DeletedCount())
	}
}

func TestOverlayDB_RebuildFromDisk(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "session")

	// Create some files in the session directory
	os.MkdirAll(filepath.Join(sessionDir, "github.com", "user", "repo"), 0755)
	os.WriteFile(filepath.Join(sessionDir, "github.com", "user", "repo", "file.txt"), []byte("hello"), 0644)
	os.MkdirAll(filepath.Join(sessionDir, "myuserdir"), 0755)
	os.WriteFile(filepath.Join(sessionDir, "myuserdir", "local.txt"), []byte("local"), 0644)

	// Also create hidden files that should be skipped
	os.MkdirAll(filepath.Join(sessionDir, ".changes"), 0755)
	os.WriteFile(filepath.Join(sessionDir, ".session.json"), []byte("{}"), 0644)

	// Open DB and rebuild
	dbDir := filepath.Join(dir, "db")
	odb, err := OpenOverlayDB(dbDir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	if err := odb.RebuildFromDisk(sessionDir); err != nil {
		t.Fatalf("RebuildFromDisk failed: %v", err)
	}

	// Check that files were found
	_, found, _ := odb.GetFile("github.com/user/repo/file.txt")
	if !found {
		t.Error("expected github.com/user/repo/file.txt to be found")
	}

	_, found, _ = odb.GetFile("myuserdir/local.txt")
	if !found {
		t.Error("expected myuserdir/local.txt to be found")
	}

	// Hidden files should not be in DB
	_, found, _ = odb.GetFile(".session.json")
	if found {
		t.Error("hidden file .session.json should not be in DB")
	}

	// myuserdir should be recognized as user dir
	if !odb.IsUserDir("myuserdir") {
		t.Error("myuserdir should be recognized as user dir after rebuild")
	}

	// github.com is not a user dir (contains dots)
	if odb.IsUserDir("github.com") {
		t.Error("github.com should not be a user dir")
	}
}

func TestOverlayDB_SymlinkEntry(t *testing.T) {
	dir := t.TempDir()

	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB failed: %v", err)
	}
	defer odb.Close()

	entry := FileEntry{
		Type:          FileEntrySymlink,
		LocalPath:     "/tmp/overlay/link",
		Mode:          0777,
		SymlinkTarget: "/target/path",
		ChangeType:    ChangeSymlink,
	}

	if err := odb.PutFile("repo/link", entry); err != nil {
		t.Fatalf("PutFile failed: %v", err)
	}

	got, found, err := odb.GetFile("repo/link")
	if err != nil {
		t.Fatalf("GetFile failed: %v", err)
	}
	if !found {
		t.Fatal("expected symlink to be found")
	}
	if got.Type != FileEntrySymlink {
		t.Errorf("expected symlink type, got %s", got.Type)
	}
	if got.SymlinkTarget != "/target/path" {
		t.Errorf("expected target /target/path, got %s", got.SymlinkTarget)
	}
}
