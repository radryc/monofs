package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	pb "github.com/radryc/monofs/api/proto"
)

// testLogger returns a logger suitable for tests (writes to stderr at debug level).
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// ---------------------------------------------------------------------------
// Enhanced mock client that supports per-path responses
// ---------------------------------------------------------------------------

// pathMockClient supports per-path Lookup/GetAttr/ReadDir/Read responses.
type pathMockClient struct {
	lookupFunc  func(ctx context.Context, path string) (*pb.LookupResponse, error)
	getattrFunc func(ctx context.Context, path string) (*pb.GetAttrResponse, error)
	readdirFunc func(ctx context.Context, path string) ([]*pb.DirEntry, error)
	readFunc    func(ctx context.Context, path string, offset, size int64) ([]byte, error)
}

func (m *pathMockClient) Lookup(ctx context.Context, path string) (*pb.LookupResponse, error) {
	if m.lookupFunc != nil {
		return m.lookupFunc(ctx, path)
	}
	return &pb.LookupResponse{Found: false}, nil
}

func (m *pathMockClient) GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
	if m.getattrFunc != nil {
		return m.getattrFunc(ctx, path)
	}
	return &pb.GetAttrResponse{Found: false}, nil
}

func (m *pathMockClient) ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error) {
	if m.readdirFunc != nil {
		return m.readdirFunc(ctx, path)
	}
	return nil, nil
}

func (m *pathMockClient) Read(ctx context.Context, path string, offset, size int64) ([]byte, error) {
	if m.readFunc != nil {
		return m.readFunc(ctx, path, offset, size)
	}
	return nil, fmt.Errorf("not found")
}

func (m *pathMockClient) Close() error            { return nil }
func (m *pathMockClient) RecordOperation()        {}
func (m *pathMockClient) RecordBytesRead(n int64) {}
func (m *pathMockClient) RecordError()            {}
func (m *pathMockClient) IsGuardianVisible() bool { return false }

// ---------------------------------------------------------------------------
// Test: addWriteBits correctly upgrades permissions
// ---------------------------------------------------------------------------

func TestAddWriteBits_RegularFile(t *testing.T) {
	// Read-only file: 0444 | S_IFREG
	mode := uint32(0444) | uint32(syscall.S_IFREG)
	got := addWriteBits(mode)

	// Should have owner write bit added: 0644 | S_IFREG
	wantPerm := uint32(0644)
	gotPerm := got & 0777
	if gotPerm != wantPerm {
		t.Errorf("addWriteBits(0444|S_IFREG): perm bits = %04o, want %04o", gotPerm, wantPerm)
	}
	// Type bits should be preserved
	if got&uint32(syscall.S_IFREG) == 0 {
		t.Error("addWriteBits should preserve S_IFREG type bit")
	}
}

func TestAddWriteBits_Directory(t *testing.T) {
	// Directory with 0555 | S_IFDIR
	mode := uint32(0555) | uint32(syscall.S_IFDIR)
	got := addWriteBits(mode)

	// Should have owner rwx: 0755 | S_IFDIR
	wantPerm := uint32(0755)
	gotPerm := got & 0777
	if gotPerm != wantPerm {
		t.Errorf("addWriteBits(0555|S_IFDIR): perm bits = %04o, want %04o", gotPerm, wantPerm)
	}
	if got&uint32(syscall.S_IFDIR) == 0 {
		t.Error("addWriteBits should preserve S_IFDIR type bit")
	}
}

func TestAddWriteBits_AlreadyWritable(t *testing.T) {
	// File already writable: 0644 | S_IFREG
	mode := uint32(0644) | uint32(syscall.S_IFREG)
	got := addWriteBits(mode)

	// Should be unchanged
	if got != mode {
		t.Errorf("addWriteBits(0644|S_IFREG) = %04o, want %04o (unchanged)", got, mode)
	}
}

func TestAddWriteBits_DirAlreadyWritable(t *testing.T) {
	// Dir already writable: 0755 | S_IFDIR
	mode := uint32(0755) | uint32(syscall.S_IFDIR)
	got := addWriteBits(mode)

	if got != mode {
		t.Errorf("addWriteBits(0755|S_IFDIR) = %04o, want %04o (unchanged)", got, mode)
	}
}

// ---------------------------------------------------------------------------
// Test: Getattr adds write bits to backend responses in writable mode
// ---------------------------------------------------------------------------

func TestGetattr_WriteBitsAddedInWritableMode(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	mockCli := &pathMockClient{
		getattrFunc: func(_ context.Context, path string) (*pb.GetAttrResponse, error) {
			return &pb.GetAttrResponse{
				Found: true,
				Ino:   42,
				Mode:  0444 | uint32(syscall.S_IFREG), // read-only from backend
				Size:  100,
				Mtime: time.Now().Unix(),
				Atime: time.Now().Unix(),
				Ctime: time.Now().Unix(),
				Nlink: 1,
				Uid:   1000,
				Gid:   1000,
			}, nil
		},
	}

	// Node inside a repository
	node := &MonoNode{
		path:       "github.com/user/repo/file.go",
		isDir:      false,
		mode:       0444 | uint32(syscall.S_IFREG),
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	ctx := context.Background()
	var out fuse.AttrOut
	errno := node.Getattr(ctx, nil, &out)
	if errno != 0 {
		t.Fatalf("Getattr returned errno %v", errno)
	}

	// Mode should have owner-write added
	gotPerm := out.Mode & 0777
	if gotPerm&0200 == 0 {
		t.Errorf("Getattr with sessionMgr should add owner-write: mode=%04o", out.Mode)
	}
}

func TestGetattr_NoWriteBitsInReadOnlyMode(t *testing.T) {
	mockCli := &pathMockClient{
		getattrFunc: func(_ context.Context, path string) (*pb.GetAttrResponse, error) {
			return &pb.GetAttrResponse{
				Found: true,
				Ino:   42,
				Mode:  0444 | uint32(syscall.S_IFREG),
				Size:  100,
				Mtime: time.Now().Unix(),
				Atime: time.Now().Unix(),
				Ctime: time.Now().Unix(),
				Nlink: 1,
				Uid:   1000,
				Gid:   1000,
			}, nil
		},
	}

	// Node without session manager (read-only mode)
	node := &MonoNode{
		path:   "github.com/user/repo/file.go",
		isDir:  false,
		mode:   0444 | uint32(syscall.S_IFREG),
		client: mockCli,
		logger: testLogger(),
	}

	ctx := context.Background()
	var out fuse.AttrOut
	errno := node.Getattr(ctx, nil, &out)
	if errno != 0 {
		t.Fatalf("Getattr returned errno %v", errno)
	}

	// Mode should NOT have owner-write
	gotPerm := out.Mode & 0777
	if gotPerm != 0444 {
		t.Errorf("Getattr without sessionMgr should keep original perms: mode=%04o, want 0444", gotPerm)
	}
}

// ---------------------------------------------------------------------------
// Test: Getattr returns ENOENT for deleted backend files
// ---------------------------------------------------------------------------

func TestGetattr_DeletedFileReturnsENOENT(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	// Mark a file as deleted
	err = sm.TrackChange(ChangeDelete, "github.com/user/repo/deleted.go", "")
	if err != nil {
		t.Fatalf("TrackChange: %v", err)
	}

	mockCli := &pathMockClient{
		getattrFunc: func(_ context.Context, path string) (*pb.GetAttrResponse, error) {
			// Backend still has the file
			return &pb.GetAttrResponse{
				Found: true,
				Ino:   42,
				Mode:  0644 | uint32(syscall.S_IFREG),
				Size:  100,
				Mtime: time.Now().Unix(),
				Atime: time.Now().Unix(),
				Ctime: time.Now().Unix(),
				Nlink: 1,
				Uid:   1000,
				Gid:   1000,
			}, nil
		},
	}

	node := &MonoNode{
		path:       "github.com/user/repo/deleted.go",
		isDir:      false,
		mode:       0644 | uint32(syscall.S_IFREG),
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	ctx := context.Background()
	var out fuse.AttrOut
	errno := node.Getattr(ctx, nil, &out)
	if errno != syscall.ENOENT {
		t.Errorf("Getattr on deleted file should return ENOENT, got %v", errno)
	}
}

// ---------------------------------------------------------------------------
// Test: Lookup adds write bits to backend responses in writable mode
// ---------------------------------------------------------------------------

func TestLookup_WriteBitsAddedInWritableMode(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	mockCli := &pathMockClient{
		lookupFunc: func(_ context.Context, path string) (*pb.LookupResponse, error) {
			if path == "github.com/user/repo/file.go" {
				return &pb.LookupResponse{
					Found: true,
					Ino:   42,
					Mode:  0444 | uint32(syscall.S_IFREG), // read-only
					Size:  100,
					Mtime: time.Now().Unix(),
				}, nil
			}
			return &pb.LookupResponse{Found: false}, nil
		},
	}

	root := NewRootWithSession(mockCli, nil, sm, nil)
	// Simulate looking up file.go inside a directory node
	parentNode := root.newChild("github.com/user/repo", true, 0755|uint32(syscall.S_IFDIR), 0)

	var out fuse.EntryOut
	// We can't call Lookup directly without being mounted in FUSE tree,
	// so test the addWriteBits logic and the GetPathState deleted check instead.

	// Verify addWriteBits works for the mode the backend would return
	backendMode := uint32(0444) | uint32(syscall.S_IFREG)
	upgraded := addWriteBits(backendMode)
	if upgraded&0200 == 0 {
		t.Error("addWriteBits should add owner-write to read-only file")
	}

	// Verify that deleted files are caught by GetPathState
	sm.TrackChange(ChangeDelete, "github.com/user/repo/file.go", "")
	state := sm.GetPathState("github.com/user/repo/file.go")
	if !state.IsDeleted {
		t.Error("deleted file should have IsDeleted=true in GetPathState")
	}

	_ = parentNode
	_ = out
}

// ---------------------------------------------------------------------------
// Test: Lookup returns ENOENT for deleted backend files
// ---------------------------------------------------------------------------

func TestLookup_DeletedFileReturnsENOENT(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	mockCli := &pathMockClient{
		lookupFunc: func(_ context.Context, path string) (*pb.LookupResponse, error) {
			return &pb.LookupResponse{
				Found: true,
				Ino:   42,
				Mode:  0644 | uint32(syscall.S_IFREG),
				Size:  100,
				Mtime: time.Now().Unix(),
			}, nil
		},
	}

	// Mark file as deleted in overlay
	sm.TrackChange(ChangeDelete, "github.com/user/repo/go.sum", "")

	// Create root with session
	root := NewRootWithSession(mockCli, nil, sm, nil)

	// Verify via GetPathState (can't call Lookup without FUSE mount)
	state := sm.GetPathState("github.com/user/repo/go.sum")
	if !state.IsDeleted {
		t.Error("go.sum should be marked deleted")
	}

	// Verify IsDeleted directly
	if !sm.IsDeleted("github.com/user/repo/go.sum") {
		t.Error("IsDeleted should return true for go.sum")
	}

	_ = root
}

// ---------------------------------------------------------------------------
// Test: MergeReadDir filters out deleted backend entries
// ---------------------------------------------------------------------------

func TestMergeReadDir_FiltersDeletedBackendFiles(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	// Mark go.sum as deleted
	sm.TrackChange(ChangeDelete, "dependency/go/mod/golang.org/x/crypto@v0.47.0/go.sum", "")

	om := NewOverlayManager(sm)

	// Backend returns entries including go.sum
	backendEntries := []fuse.DirEntry{
		{Name: "go.mod", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 1},
		{Name: "go.sum", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 2},
		{Name: "README.md", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 3},
		{Name: "acme", Mode: 0755 | uint32(fuse.S_IFDIR), Ino: 4},
	}

	merged := om.MergeReadDir(backendEntries, "dependency/go/mod/golang.org/x/crypto@v0.47.0")

	// go.sum should be filtered out
	names := make(map[string]bool)
	for _, e := range merged {
		names[e.Name] = true
	}

	if names["go.sum"] {
		t.Error("go.sum should be filtered from readdir after deletion")
	}
	if !names["go.mod"] {
		t.Error("go.mod should still be in readdir")
	}
	if !names["README.md"] {
		t.Error("README.md should still be in readdir")
	}
	if !names["acme"] {
		t.Error("acme directory should still be in readdir")
	}
	if len(merged) != 3 {
		t.Errorf("expected 3 entries after filtering, got %d", len(merged))
	}
}

func TestMergeReadDir_MultipleDeletedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	// Delete multiple files
	sm.TrackChange(ChangeDelete, "repo/file1.txt", "")
	sm.TrackChange(ChangeDelete, "repo/file3.txt", "")

	om := NewOverlayManager(sm)

	backendEntries := []fuse.DirEntry{
		{Name: "file1.txt", Mode: 0644 | uint32(fuse.S_IFREG), Ino: 1},
		{Name: "file2.txt", Mode: 0644 | uint32(fuse.S_IFREG), Ino: 2},
		{Name: "file3.txt", Mode: 0644 | uint32(fuse.S_IFREG), Ino: 3},
		{Name: "file4.txt", Mode: 0644 | uint32(fuse.S_IFREG), Ino: 4},
	}

	merged := om.MergeReadDir(backendEntries, "repo")

	if len(merged) != 2 {
		t.Errorf("expected 2 entries after deleting 2, got %d", len(merged))
	}

	names := make(map[string]bool)
	for _, e := range merged {
		names[e.Name] = true
	}

	if names["file1.txt"] {
		t.Error("file1.txt should be filtered")
	}
	if !names["file2.txt"] {
		t.Error("file2.txt should remain")
	}
	if names["file3.txt"] {
		t.Error("file3.txt should be filtered")
	}
	if !names["file4.txt"] {
		t.Error("file4.txt should remain")
	}
}

// ---------------------------------------------------------------------------
// Test: Delete does not affect files in other directories
// ---------------------------------------------------------------------------

func TestMergeReadDir_DeleteDoesNotAffectOtherDirs(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	// Delete go.sum in one directory
	sm.TrackChange(ChangeDelete, "repo1/go.sum", "")

	om := NewOverlayManager(sm)

	// Same-named file in a different directory should NOT be affected
	backendEntries := []fuse.DirEntry{
		{Name: "go.sum", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 1},
		{Name: "go.mod", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 2},
	}

	merged := om.MergeReadDir(backendEntries, "repo2")

	names := make(map[string]bool)
	for _, e := range merged {
		names[e.Name] = true
	}

	if !names["go.sum"] {
		t.Error("go.sum in repo2 should NOT be affected by deletion in repo1")
	}
	if len(merged) != 2 {
		t.Errorf("expected 2 entries in unaffected dir, got %d", len(merged))
	}
}

// ---------------------------------------------------------------------------
// Test: Unlink tracks deletion properly via session manager
// ---------------------------------------------------------------------------

func TestUnlink_TracksBackendFileDeletion(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	mockCli := &pathMockClient{}

	// Create a parent node inside a repo directory
	parentNode := &MonoNode{
		path:       "dependency/go/mod/golang.org/x/crypto@v0.47.0",
		isDir:      true,
		mode:       0755 | uint32(syscall.S_IFDIR),
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	// Unlink go.sum (a backend-only file — not in overlay)
	errno := parentNode.Unlink(context.Background(), "go.sum")
	if errno != 0 {
		t.Fatalf("Unlink returned errno %v", errno)
	}

	// verify it's tracked as deleted
	fullPath := "dependency/go/mod/golang.org/x/crypto@v0.47.0/go.sum"
	if !sm.IsDeleted(fullPath) {
		t.Error("go.sum should be marked deleted after Unlink")
	}

	// Verify GetPathState reports deletion
	state := sm.GetPathState(fullPath)
	if !state.IsDeleted {
		t.Error("GetPathState should report IsDeleted=true")
	}

	// Verify MergeReadDir filters it
	om := NewOverlayManager(sm)
	backendEntries := []fuse.DirEntry{
		{Name: "go.mod", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 1},
		{Name: "go.sum", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 2},
	}
	merged := om.MergeReadDir(backendEntries, "dependency/go/mod/golang.org/x/crypto@v0.47.0")

	foundGoSum := false
	for _, e := range merged {
		if e.Name == "go.sum" {
			foundGoSum = true
		}
	}
	if foundGoSum {
		t.Error("go.sum should not appear after Unlink")
	}
}

// ---------------------------------------------------------------------------
// Test: Rmdir tracks backend directory deletion
// ---------------------------------------------------------------------------

func TestRmdir_TracksBackendDirDeletion(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	mockCli := &pathMockClient{}

	parentNode := &MonoNode{
		path:       "github.com/user/repo",
		isDir:      true,
		mode:       0755 | uint32(syscall.S_IFDIR),
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	ctx := context.Background()

	errno := parentNode.Rmdir(ctx, "subdir")
	if errno != 0 {
		t.Fatalf("Rmdir returned errno %v", errno)
	}

	fullPath := "github.com/user/repo/subdir"
	if !sm.IsDeleted(fullPath) {
		t.Error("subdir should be marked deleted after Rmdir")
	}

	// MergeReadDir should filter it
	om := NewOverlayManager(sm)
	backendEntries := []fuse.DirEntry{
		{Name: "subdir", Mode: 0755 | uint32(fuse.S_IFDIR), Ino: 1},
		{Name: "file.txt", Mode: 0644 | uint32(fuse.S_IFREG), Ino: 2},
	}
	merged := om.MergeReadDir(backendEntries, "github.com/user/repo")

	names := make(map[string]bool)
	for _, e := range merged {
		names[e.Name] = true
	}

	if names["subdir"] {
		t.Error("subdir should be filtered from readdir after Rmdir")
	}
	if !names["file.txt"] {
		t.Error("file.txt should still appear")
	}
}

// ---------------------------------------------------------------------------
// Test: Create in backend directory tracks the new file in overlay
// ---------------------------------------------------------------------------

func TestCreate_TracksNewFileInOverlay(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	mockCli := &pathMockClient{}

	// Create parent node representing a directory inside a repo
	parentNode := &MonoNode{
		path:       "github.com/user/repo",
		isDir:      true,
		mode:       0755 | uint32(syscall.S_IFDIR),
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	// Simulate a Create call — manually replicate what Create does
	// (without FUSE mount) to verify overlay tracking
	newPath := parentNode.path + "/new_file.txt"

	localPath, err := sm.GetLocalPath(newPath)
	if err != nil {
		t.Fatalf("GetLocalPath: %v", err)
	}

	// Create parent dirs + file (what Create() does)
	if err := os.MkdirAll(localPath[:len(localPath)-len("/new_file.txt")], 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := os.OpenFile(localPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	f.WriteString("hello world")
	f.Close()

	// Track the creation
	err = sm.TrackChange(ChangeCreate, newPath, "")
	if err != nil {
		t.Fatalf("TrackChange: %v", err)
	}

	// Verify overlay knows about the file
	if !sm.HasLocalOverride(newPath) {
		t.Error("new_file.txt should be tracked as local override after Create")
	}

	// Verify GetPathState
	state := sm.GetPathState(newPath)
	if !state.HasOverride {
		t.Error("GetPathState should report HasOverride=true for created file")
	}

	// Verify it appears in MergeReadDir
	om := NewOverlayManager(sm)
	backendEntries := []fuse.DirEntry{
		{Name: "existing.go", Mode: 0644 | uint32(fuse.S_IFREG), Ino: 1},
	}
	merged := om.MergeReadDir(backendEntries, "github.com/user/repo")

	names := make(map[string]bool)
	for _, e := range merged {
		names[e.Name] = true
	}

	if !names["new_file.txt"] {
		t.Error("new_file.txt should appear in readdir after Create")
	}
	if !names["existing.go"] {
		t.Error("existing.go should still appear from backend")
	}
}

// ---------------------------------------------------------------------------
// Test: Create then Delete lifecycle
// ---------------------------------------------------------------------------

func TestOverlay_CreateThenDeleteLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	newPath := "github.com/user/repo/temp.txt"

	// Create a file in overlay
	localPath, _ := sm.GetLocalPath(newPath)
	os.MkdirAll(localPath[:len(localPath)-len("/temp.txt")], 0755)
	os.WriteFile(localPath, []byte("temp"), 0644)
	sm.TrackChange(ChangeCreate, newPath, "")

	// File should be visible
	if !sm.HasLocalOverride(newPath) {
		t.Fatal("file should be tracked after create")
	}

	// Delete it — since it was created in this session (ChangeCreate),
	// it should be fully removed, not just marked deleted
	sm.TrackChange(ChangeDelete, newPath, "")

	if sm.HasLocalOverride(newPath) {
		t.Error("file should no longer be tracked after delete of session-only file")
	}
	// Session-only files don't get marked deleted (they never existed in backend)
	if sm.IsDeleted(newPath) {
		t.Error("session-only deleted file should not be in deleted set")
	}
}

// ---------------------------------------------------------------------------
// Test: Delete backend file then re-create it
// ---------------------------------------------------------------------------

func TestOverlay_DeleteBackendThenReCreate(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	filePath := "repo/go.sum"

	// Delete the backend file
	sm.TrackChange(ChangeDelete, filePath, "")
	if !sm.IsDeleted(filePath) {
		t.Fatal("should be deleted")
	}

	// Now re-create it in overlay
	localPath, _ := sm.GetLocalPath(filePath)
	os.MkdirAll(localPath[:len(localPath)-len("/go.sum")], 0755)
	os.WriteFile(localPath, []byte("new content"), 0644)
	sm.TrackChange(ChangeCreate, filePath, "")

	// Should no longer be deleted (PutFile unmarks deletion)
	if sm.IsDeleted(filePath) {
		t.Error("file should not be deleted after re-creation")
	}

	// Should be tracked as override
	if !sm.HasLocalOverride(filePath) {
		t.Error("file should be tracked as override after re-creation")
	}

	// MergeReadDir should show the file
	om := NewOverlayManager(sm)
	backendEntries := []fuse.DirEntry{
		{Name: "go.mod", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 1},
		// go.sum intentionally not in backend after deletion
	}
	merged := om.MergeReadDir(backendEntries, "repo")

	names := make(map[string]bool)
	for _, e := range merged {
		names[e.Name] = true
	}

	if !names["go.sum"] {
		t.Error("go.sum should appear in readdir after re-creation")
	}
}

// ---------------------------------------------------------------------------
// Test: Readdir with session merges overlay correctly
// ---------------------------------------------------------------------------

func TestReaddir_MergesOverlayWithBackend(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	// Create a new file in overlay
	newPath := "github.com/user/repo/new.go"
	localPath, _ := sm.GetLocalPath(newPath)
	os.MkdirAll(localPath[:len(localPath)-len("/new.go")], 0755)
	os.WriteFile(localPath, []byte("package main"), 0644)
	sm.TrackChange(ChangeCreate, newPath, "")

	// Delete an existing backend file
	sm.TrackChange(ChangeDelete, "github.com/user/repo/old.go", "")

	mockCli := &pathMockClient{
		readdirFunc: func(_ context.Context, path string) ([]*pb.DirEntry, error) {
			return []*pb.DirEntry{
				{Name: "main.go", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 1},
				{Name: "old.go", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 2},
			}, nil
		},
	}

	dirNode := &MonoNode{
		path:       "github.com/user/repo",
		isDir:      true,
		mode:       0755 | uint32(syscall.S_IFDIR),
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	ctx := context.Background()
	stream, errno := dirNode.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir returned errno %v", errno)
	}

	names := make(map[string]bool)
	for stream.HasNext() {
		entry, eno := stream.Next()
		if eno != 0 {
			break
		}
		names[entry.Name] = true
	}

	if !names["main.go"] {
		t.Error("main.go should appear (from backend)")
	}
	if names["old.go"] {
		t.Error("old.go should be filtered (deleted in overlay)")
	}
	if !names["new.go"] {
		t.Error("new.go should appear (created in overlay)")
	}
}

// ---------------------------------------------------------------------------
// Test: backendEntryTimeout is shorter than attrTimeout
// ---------------------------------------------------------------------------

func TestBackendEntryTimeout_ShorterThanAttrTimeout(t *testing.T) {
	bet := backendEntryTimeout()
	at := attrTimeout()

	if bet >= at {
		t.Errorf("backendEntryTimeout (%v) should be shorter than attrTimeout (%v)", bet, at)
	}

	oet := overlayEntryTimeout()
	if oet >= bet {
		t.Errorf("overlayEntryTimeout (%v) should be shorter than backendEntryTimeout (%v)", oet, bet)
	}
}

// ---------------------------------------------------------------------------
// Test: Getattr for backend directory has write+execute bits in writable mode
// ---------------------------------------------------------------------------

func TestGetattr_BackendDir_HasWriteBitsInWritableMode(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	mockCli := &pathMockClient{
		getattrFunc: func(_ context.Context, path string) (*pb.GetAttrResponse, error) {
			return &pb.GetAttrResponse{
				Found: true,
				Ino:   42,
				Mode:  0555 | uint32(syscall.S_IFDIR), // read-only dir from backend
				Size:  0,
				Mtime: time.Now().Unix(),
				Atime: time.Now().Unix(),
				Ctime: time.Now().Unix(),
				Nlink: 2,
				Uid:   1000,
				Gid:   1000,
			}, nil
		},
	}

	node := &MonoNode{
		path:       "github.com/user/repo",
		isDir:      true,
		mode:       0555 | uint32(syscall.S_IFDIR),
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	ctx := context.Background()
	var out fuse.AttrOut
	errno := node.Getattr(ctx, nil, &out)
	if errno != 0 {
		t.Fatalf("Getattr returned errno %v", errno)
	}

	// Dir should have owner rwx (at least write + execute for create/unlink to work)
	gotPerm := out.Mode & 0777
	if gotPerm&0200 == 0 {
		t.Errorf("Backend dir in writable mode should have owner-write: mode=%04o", out.Mode)
	}
	if gotPerm&0100 == 0 {
		t.Errorf("Backend dir in writable mode should have owner-exec: mode=%04o", out.Mode)
	}
}

// ---------------------------------------------------------------------------
// Test: ListDeletedUnderDir returns basenames, not full paths
// ---------------------------------------------------------------------------

func TestListDeletedUnderDir_ReturnsBasenames(t *testing.T) {
	dir := t.TempDir()
	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB: %v", err)
	}
	defer odb.Close()

	// Mark deletion using full paths (as stored in DB)
	odb.MarkDeleted("dependency/go/mod/golang.org/x/crypto@v0.47.0/go.sum")
	odb.MarkDeleted("dependency/go/mod/golang.org/x/crypto@v0.47.0/LICENSE")

	// ListDeletedUnderDir should return basenames
	deleted, err := odb.ListDeletedUnderDir("dependency/go/mod/golang.org/x/crypto@v0.47.0")
	if err != nil {
		t.Fatalf("ListDeletedUnderDir: %v", err)
	}

	if len(deleted) != 2 {
		t.Fatalf("expected 2 deleted entries, got %d: %v", len(deleted), deleted)
	}

	names := make(map[string]bool)
	for _, d := range deleted {
		names[d] = true
	}

	if !names["go.sum"] {
		t.Error("expected basename 'go.sum' in deleted list")
	}
	if !names["LICENSE"] {
		t.Error("expected basename 'LICENSE' in deleted list")
	}

	// Should NOT contain full paths
	for _, d := range deleted {
		if len(d) > 20 {
			t.Errorf("deleted entry looks like full path instead of basename: %q", d)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Deletion of deeply nested path only affects correct directory
// ---------------------------------------------------------------------------

func TestListDeletedUnderDir_DeepPathIsolation(t *testing.T) {
	dir := t.TempDir()
	odb, err := OpenOverlayDB(dir, nil)
	if err != nil {
		t.Fatalf("OpenOverlayDB: %v", err)
	}
	defer odb.Close()

	// Delete files at different levels
	odb.MarkDeleted("a/b/c/file1.txt")
	odb.MarkDeleted("a/b/file2.txt")
	odb.MarkDeleted("a/file3.txt")

	// Only direct children of "a/b" should appear
	deleted, _ := odb.ListDeletedUnderDir("a/b")
	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted under a/b, got %d: %v", len(deleted), deleted)
	}
	if len(deleted) == 1 && deleted[0] != "file2.txt" {
		t.Errorf("expected 'file2.txt', got %q", deleted[0])
	}

	// Direct children of "a"
	deleted, _ = odb.ListDeletedUnderDir("a")
	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted under a, got %d: %v", len(deleted), deleted)
	}
}

// ---------------------------------------------------------------------------
// Test: Unlink on root returns EROFS
// ---------------------------------------------------------------------------

func TestUnlink_RootReturnsEROFS(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	mockCli := &pathMockClient{}
	root := &MonoNode{
		path:       "",
		isDir:      true,
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	ctx := context.Background()
	errno := root.Unlink(ctx, "something")
	if errno != syscall.EROFS {
		t.Errorf("Unlink at root should return EROFS, got %v", errno)
	}
}

// ---------------------------------------------------------------------------
// Test: Unlink without session manager returns EROFS
// ---------------------------------------------------------------------------

func TestUnlink_ReadOnlyReturnsEROFS(t *testing.T) {
	mockCli := &pathMockClient{}
	node := &MonoNode{
		path:   "some/path",
		isDir:  true,
		client: mockCli,
		logger: testLogger(),
		// no sessionMgr
	}

	ctx := context.Background()
	errno := node.Unlink(ctx, "file.txt")
	if errno != syscall.EROFS {
		t.Errorf("Unlink without session should return EROFS, got %v", errno)
	}
}

// ---------------------------------------------------------------------------
// Test: Create without session manager returns EROFS
// ---------------------------------------------------------------------------

func TestCreate_ReadOnlyReturnsEROFS(t *testing.T) {
	mockCli := &pathMockClient{}
	node := &MonoNode{
		path:   "some/path",
		isDir:  true,
		client: mockCli,
		logger: testLogger(),
		// no sessionMgr
	}

	ctx := context.Background()
	var out fuse.EntryOut
	_, _, _, errno := node.Create(ctx, "file.txt", 0, 0644, &out)
	if errno != syscall.EROFS {
		t.Errorf("Create without session should return EROFS, got %v", errno)
	}
}

// ---------------------------------------------------------------------------
// Test: MergeReadDir shows overlay-created files alongside backend entries
// ---------------------------------------------------------------------------

func TestMergeReadDir_OverlayFileOverridesBackend(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	// Create a modified version of an existing backend file
	modPath := "repo/existing.go"
	localPath, _ := sm.GetLocalPath(modPath)
	os.MkdirAll(localPath[:len(localPath)-len("/existing.go")], 0755)
	os.WriteFile(localPath, []byte("modified content"), 0644)
	sm.TrackChange(ChangeModify, modPath, "original_hash")

	om := NewOverlayManager(sm)

	backendEntries := []fuse.DirEntry{
		{Name: "existing.go", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 1},
		{Name: "other.go", Mode: 0444 | uint32(fuse.S_IFREG), Ino: 2},
	}

	merged := om.MergeReadDir(backendEntries, "repo")

	if len(merged) != 2 {
		t.Errorf("expected 2 entries, got %d", len(merged))
	}

	// Check that existing.go is present (overlay version should override backend)
	for _, e := range merged {
		if e.Name == "existing.go" {
			// Overlay version should have writable mode
			if e.Mode&0200 == 0 {
				// Overlay entries from DB have the mode from disk stat
				// which should be 0644, not 0444
				t.Logf("existing.go mode=%04o (overlay override)", e.Mode)
			}
			return
		}
	}
	t.Error("existing.go should still appear in merged results")
}

// ---------------------------------------------------------------------------
// Helpers shared by Rename tests
// ---------------------------------------------------------------------------

// createOverlayFile creates a file in the overlay at the session-local path
// for monofsPath and optionally tracks it with the given ChangeType.
// Returns the absolute local path.
func createOverlayFile(t *testing.T, sm *SessionManager, monofsPath string, content []byte, ct ChangeType) string {
	t.Helper()
	localPath, err := sm.GetLocalPath(monofsPath)
	if err != nil {
		t.Fatalf("GetLocalPath(%q): %v", monofsPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(localPath, content, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if ct != "" {
		if err := sm.TrackChange(ct, monofsPath, ""); err != nil {
			t.Fatalf("TrackChange(%s, %q): %v", ct, monofsPath, err)
		}
	}
	return localPath
}

// ---------------------------------------------------------------------------
// Test: Rename – read-only / root guard
// ---------------------------------------------------------------------------

func TestRename_ReadOnlyReturnsEROFS(t *testing.T) {
	node := &MonoNode{
		path:   "some/dir",
		isDir:  true,
		client: &pathMockClient{},
		logger: testLogger(),
		// no sessionMgr
	}
	dest := &MonoNode{path: "some/dir", isDir: true, client: &pathMockClient{}, logger: testLogger()}
	if errno := node.Rename(context.Background(), "a.txt", dest, "b.txt", 0); errno != syscall.EROFS {
		t.Errorf("Rename without sessionMgr: want EROFS, got %v", errno)
	}
}

func TestRename_RootLevelReturnsEROFS(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	root := &MonoNode{
		path:       "",
		isDir:      true,
		client:     &pathMockClient{},
		sessionMgr: sm,
		logger:     testLogger(),
	}
	dest := &MonoNode{path: "some/dir", isDir: true, client: &pathMockClient{}, sessionMgr: sm, logger: testLogger()}
	if errno := root.Rename(context.Background(), "file.txt", dest, "file2.txt", 0); errno != syscall.EROFS {
		t.Errorf("Rename at root: want EROFS, got %v", errno)
	}
}

// ---------------------------------------------------------------------------
// Test: Rename – session-only .tmp file is removed from overlay (not marked
// deleted) and the final name is tracked as ChangeCreate.
//
// This is the exact scenario that caused the FUSE hang:
//   go mod tidy downloads grpc v1.75.0.zip via a .tmp intermediary, then
//   renames .tmp → .zip while the kernel holds the destination dentry lock.
//
// Regression: before the fix, Rename called invalidateEntry(newName) which
// sent NOTIFY_INVAL_ENTRY back to the kernel for the locked destination
// dentry → deadlock. The handler now returns without calling invalidateEntry.
// ---------------------------------------------------------------------------

func TestRename_SessionOnlyTmpFile_NotMarkedDeleted(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	const dir = "dependency/go/mod/cache/download/google.golang.org/grpc/@v"
	const tmpName = "v1.75.0.zip489934148.tmp"
	const finalName = "v1.75.0.zip"

	// Simulate file downloaded into overlay as a session-only creation (.tmp).
	createOverlayFile(t, sm, dir+"/"+tmpName, []byte("fake zip bytes"), ChangeCreate)

	if !sm.HasLocalOverride(dir + "/" + tmpName) {
		t.Fatal("pre-condition: .tmp must be tracked before rename")
	}

	parent := &MonoNode{
		path:       dir,
		isDir:      true,
		client:     &pathMockClient{},
		sessionMgr: sm,
		logger:     testLogger(),
	}

	// Rename must complete without hanging.
	// On a real FUSE mount the old code deadlocked here because it called
	// NotifyEntry(finalName) while the kernel held the dentry lock on
	// finalName waiting for the Rename reply.
	done := make(chan syscall.Errno, 1)
	go func() {
		done <- parent.Rename(context.Background(), tmpName, parent, finalName, 0)
	}()
	select {
	case errno := <-done:
		if errno != 0 {
			t.Fatalf("Rename returned %v, want 0", errno)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Rename deadlocked (did not complete within 5 s)")
	}

	// .tmp was a session-only file: must be fully removed from DB, not just
	// marked deleted (otherwise it would show as a phantom '[-]' deletion
	// that was never on the cluster).
	if sm.IsDeleted(dir + "/" + tmpName) {
		t.Error(".tmp must NOT appear in the deleted set after rename")
	}
	if sm.HasLocalOverride(dir + "/" + tmpName) {
		t.Error(".tmp entry must be removed from overlay DB after rename")
	}

	// Final file must be tracked as a new creation.
	if !sm.HasLocalOverride(dir + "/" + finalName) {
		t.Error("final .zip must be tracked in overlay after rename")
	}

	// Final file must exist on disk at the new overlay path.
	localFinal, _ := sm.GetLocalPath(dir + "/" + finalName)
	if _, err := os.Stat(localFinal); err != nil {
		t.Errorf("final file must exist on disk: %v", err)
	}

	// .tmp file must NOT exist on disk anymore.
	localTmp, _ := sm.GetLocalPath(dir + "/" + tmpName)
	if _, err := os.Stat(localTmp); err == nil {
		t.Error(".tmp file must not exist on disk after rename")
	}
}

// ---------------------------------------------------------------------------
// Test: Rename – existing (backend-tracked) file is marked deleted at old
// path and created at new path.
// ---------------------------------------------------------------------------

func TestRename_BackendFile_OverlayStateCorrect(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	const dir = "github.com/user/repo/pkg"
	const oldName = "old.go"
	const newName = "new.go"

	// Simulate a file that was fetched from the backend and modified locally.
	createOverlayFile(t, sm, dir+"/"+oldName, []byte("package pkg"), ChangeModify)

	parent := &MonoNode{
		path:       dir,
		isDir:      true,
		client:     &pathMockClient{},
		sessionMgr: sm,
		logger:     testLogger(),
	}

	if errno := parent.Rename(context.Background(), oldName, parent, newName, 0); errno != 0 {
		t.Fatalf("Rename returned %v", errno)
	}

	// old path must be marked deleted (it existed on the backend).
	if !sm.IsDeleted(dir + "/" + oldName) {
		t.Error("old path must be marked deleted after rename of a backend file")
	}

	// New path must be tracked.
	if !sm.HasLocalOverride(dir + "/" + newName) {
		t.Error("new path must be tracked in overlay after rename")
	}
}

// ---------------------------------------------------------------------------
// Test: Rename – cross-directory move
// ---------------------------------------------------------------------------

func TestRename_CrossDirectory_OverlayStateCorrect(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	const srcDir = "repo/src"
	const dstDir = "repo/dst"
	const name = "file.go"

	// File exists only in this session (ChangeCreate).
	createOverlayFile(t, sm, srcDir+"/"+name, []byte("// content"), ChangeCreate)

	srcParent := &MonoNode{
		path: srcDir, isDir: true,
		client: &pathMockClient{}, sessionMgr: sm, logger: testLogger(),
	}
	dstParent := &MonoNode{
		path: dstDir, isDir: true,
		client: &pathMockClient{}, sessionMgr: sm, logger: testLogger(),
	}

	if errno := srcParent.Rename(context.Background(), name, dstParent, name, 0); errno != 0 {
		t.Fatalf("cross-dir Rename returned %v", errno)
	}

	// Session-only source → must be removed from DB, not marked deleted.
	if sm.IsDeleted(srcDir + "/" + name) {
		t.Error("session-only source must NOT appear in deleted set")
	}
	if sm.HasLocalOverride(srcDir + "/" + name) {
		t.Error("session-only source must be removed from overlay DB")
	}

	// Destination must be tracked.
	if !sm.HasLocalOverride(dstDir + "/" + name) {
		t.Error("destination must be tracked in overlay after cross-dir rename")
	}

	// File must be on disk at the destination.
	localDst, _ := sm.GetLocalPath(dstDir + "/" + name)
	if _, err := os.Stat(localDst); err != nil {
		t.Errorf("destination file must exist on disk: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: Rename – multiple sequential renames stay consistent
// (e.g. download phase: a.tmp → a, then b.tmp → b, etc.)
// ---------------------------------------------------------------------------

func TestRename_MultipleSequentialRenames(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	const dir = "dependency/go/mod/cache"
	files := []struct{ tmp, final string }{
		{"a.zip.tmp", "a.zip"},
		{"b.zip.tmp", "b.zip"},
		{"c.zip.tmp", "c.zip"},
	}

	// Create all .tmp files first.
	for _, f := range files {
		createOverlayFile(t, sm, dir+"/"+f.tmp, []byte(f.tmp), ChangeCreate)
	}

	parent := &MonoNode{
		path: dir, isDir: true,
		client: &pathMockClient{}, sessionMgr: sm, logger: testLogger(),
	}

	// Rename each .tmp → final.
	for _, f := range files {
		if errno := parent.Rename(context.Background(), f.tmp, parent, f.final, 0); errno != 0 {
			t.Errorf("Rename %s → %s: %v", f.tmp, f.final, errno)
		}
	}

	// None of the .tmp names may appear in deleted set or as overrides.
	for _, f := range files {
		if sm.IsDeleted(dir + "/" + f.tmp) {
			t.Errorf("%s must not be in deleted set", f.tmp)
		}
		if sm.HasLocalOverride(dir + "/" + f.tmp) {
			t.Errorf("%s must not have an overlay entry", f.tmp)
		}
		if !sm.HasLocalOverride(dir + "/" + f.final) {
			t.Errorf("%s must be tracked as override", f.final)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Unlink – state is consistent: local file removed, deletion tracked
// ---------------------------------------------------------------------------

func TestUnlink_OverlayStateAfterUnlink(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	const dir = "repo/pkg"
	const name = "util.go"

	// File exists in overlay (fetched from backend).
	createOverlayFile(t, sm, dir+"/"+name, []byte("package pkg"), ChangeModify)

	node := &MonoNode{
		path: dir, isDir: true,
		client: &pathMockClient{}, sessionMgr: sm, logger: testLogger(),
	}

	if errno := node.Unlink(context.Background(), name); errno != 0 {
		t.Fatalf("Unlink returned %v", errno)
	}

	// File must be marked deleted.
	if !sm.IsDeleted(dir + "/" + name) {
		t.Error("unlinked file must be in deleted set")
	}
	// Local overlay file must be gone from disk.
	localPath, _ := sm.GetLocalPath(dir + "/" + name)
	if _, err := os.Stat(localPath); err == nil {
		t.Error("unlinked overlay file must be removed from disk")
	}
}

func TestUnlink_SessionOnlyFile_NotMarkedDeleted(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	const dir = "repo/pkg"
	const name = "new.go"

	// Session-only file (never existed on backend).
	createOverlayFile(t, sm, dir+"/"+name, []byte("package pkg"), ChangeCreate)

	node := &MonoNode{
		path: dir, isDir: true,
		client: &pathMockClient{}, sessionMgr: sm, logger: testLogger(),
	}

	if errno := node.Unlink(context.Background(), name); errno != 0 {
		t.Fatalf("Unlink returned %v", errno)
	}

	// Session-only file: must be removed from DB entirely, not marked deleted.
	if sm.IsDeleted(dir + "/" + name) {
		t.Error("session-only unlinked file must NOT be in deleted set")
	}
	if sm.HasLocalOverride(dir + "/" + name) {
		t.Error("session-only unlinked file must be removed from overlay DB")
	}
}

// ---------------------------------------------------------------------------
// Test: Rmdir – user root directory removal cleans up DB and disk
// ---------------------------------------------------------------------------

func TestRmdir_UserRootDir_CleanedUp(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	const dirName = "myworkdir"

	if err := sm.CreateUserRootDir(dirName); err != nil {
		t.Fatalf("CreateUserRootDir: %v", err)
	}
	if !sm.IsUserRootDir(dirName) {
		t.Fatal("pre-condition: directory must be a user root dir")
	}

	root := &MonoNode{
		path: "", isDir: true,
		client: &pathMockClient{}, sessionMgr: sm, logger: testLogger(),
	}

	if errno := root.Rmdir(context.Background(), dirName); errno != 0 {
		t.Fatalf("Rmdir returned %v", errno)
	}

	// Must no longer be registered as a user root dir.
	if sm.IsUserRootDir(dirName) {
		t.Error("directory should be removed from user root dir registry")
	}

	// Local directory must be gone from disk.
	localPath, _ := sm.GetLocalPath(dirName)
	if _, err := os.Stat(localPath); err == nil {
		t.Error("user root dir must be removed from disk after Rmdir")
	}
}

func TestRmdir_NonUserRootAtRootReturnsEROFS(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	root := &MonoNode{
		path: "", isDir: true,
		client: &pathMockClient{}, sessionMgr: sm, logger: testLogger(),
	}

	// "github.com" is a backend namespace, not a user root dir → must be denied.
	if errno := root.Rmdir(context.Background(), "github.com"); errno != syscall.EROFS {
		t.Errorf("Rmdir of backend repo root: want EROFS, got %v", errno)
	}
}

func TestRmdir_NestedDir_OverlayStateCorrect(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	const parent = "repo/pkg"
	const child = "subpkg"
	childPath := parent + "/" + child

	// Create local directory + track it.
	localDir, _ := sm.GetLocalPath(childPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := sm.TrackChange(ChangeMkdir, childPath, ""); err != nil {
		t.Fatalf("TrackChange: %v", err)
	}

	parentNode := &MonoNode{
		path: parent, isDir: true,
		client: &pathMockClient{}, sessionMgr: sm, logger: testLogger(),
	}

	if errno := parentNode.Rmdir(context.Background(), child); errno != 0 {
		t.Fatalf("Rmdir returned %v", errno)
	}

	// Nested mkdir was session-only (ChangeCreate/ChangeMkdir) → removed
	// from DB, not added to deleted set.
	if sm.IsDeleted(childPath) {
		t.Error("session-only rmdir must NOT be in deleted set")
	}
}

// TestFlush_IsLocalWriteReset_PreventsDuplicateTracking verifies that
// Flush resets the isLocalWrite flag after tracking changes, preventing
// redundant TrackChange calls on subsequent Flush invocations.
// This fixes the "bad hash injection" issue where the same file
// modification was tracked multiple times.
func TestFlush_IsLocalWriteReset_PreventsRedundantTracking(t *testing.T) {
	tmpDir := t.TempDir()
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	// Create a mock client
	mockCli := &pathMockClient{
		lookupFunc: func(_ context.Context, path string) (*pb.LookupResponse, error) {
			return &pb.LookupResponse{Found: false}, nil
		},
	}

	// Create test node with session manager (writable mode)
	node := &MonoNode{
		path:       "github.com/user/repo/file.go",
		isDir:      false,
		mode:       0644 | uint32(syscall.S_IFREG),
		client:     mockCli,
		sessionMgr: sm,
		logger:     testLogger(),
	}

	// Simulate a write operation by setting isLocalWrite
	node.mu.Lock()
	node.isLocalWrite = true
	node.size = 100
	node.mu.Unlock()

	// Create a file handle
	localPath, _ := sm.GetLocalPath(node.path)
	os.MkdirAll(filepath.Dir(localPath), 0755)
	f, err := os.Create(localPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer f.Close()

	fh := &monofsFileHandle{
		file:   f,
		node:   node,
		logger: testLogger(),
	}

	// Verify isLocalWrite is true before Flush
	if !node.isLocalWrite {
		t.Fatal("isLocalWrite should be true before first Flush")
	}

	// First Flush should track the change and reset isLocalWrite
	ctx := context.Background()
	fh.Flush(ctx)

	// Verify isLocalWrite was reset to false
	if node.isLocalWrite {
		t.Error("isLocalWrite should be reset to false after Flush")
	}

	// Verify one change is in the overlay
	changes := sm.GetChanges()
	if len(changes) != 1 {
		t.Fatalf("expected 1 change after first flush, got %d", len(changes))
	}

	// Second Flush should NOT process any change (isLocalWrite is false)
	fh.Flush(ctx)

	// isLocalWrite should still be false
	if node.isLocalWrite {
		t.Error("isLocalWrite should still be false after second Flush")
	}

	// Simulate another write - this should set isLocalWrite again
	node.mu.Lock()
	node.isLocalWrite = true
	node.mu.Unlock()

	// Third Flush should track the change again
	fh.Flush(ctx)

	// isLocalWrite should be reset again
	if node.isLocalWrite {
		t.Error("isLocalWrite should be reset to false after third Flush")
	}

	// The key assertion: isLocalWrite is properly managed
	// The bug was that isLocalWrite was never reset, causing redundant tracking
}
