package fuse

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	pb "github.com/radryc/monofs/api/proto"
)

// mockClient implements MonoFSClient for testing
type mockClient struct {
	shouldFail bool
	failError  error
	entries    []*pb.DirEntry
}

func (m *mockClient) Lookup(ctx context.Context, path string) (*pb.LookupResponse, error) {
	if m.shouldFail {
		return nil, m.failError
	}
	return &pb.LookupResponse{
		Found: true,
		Ino:   1,
		Mode:  0755 | uint32(syscall.S_IFDIR),
		Size:  0,
		Mtime: time.Now().Unix(),
	}, nil
}

func (m *mockClient) GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
	if m.shouldFail {
		return nil, m.failError
	}
	return &pb.GetAttrResponse{
		Found: true,
		Ino:   1,
		Mode:  0644 | uint32(syscall.S_IFREG),
		Size:  100,
		Mtime: time.Now().Unix(),
		Atime: time.Now().Unix(),
		Ctime: time.Now().Unix(),
		Nlink: 1,
		Uid:   1000,
		Gid:   1000,
	}, nil
}

func (m *mockClient) ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error) {
	if m.shouldFail {
		return nil, m.failError
	}
	if m.entries != nil {
		return m.entries, nil
	}
	return []*pb.DirEntry{
		{Name: "file1.txt", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 2},
		{Name: "file2.txt", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 3},
	}, nil
}

func (m *mockClient) Read(ctx context.Context, path string, offset, size int64) ([]byte, error) {
	if m.shouldFail {
		return nil, m.failError
	}
	return []byte("test content"), nil
}

func (m *mockClient) GetHealthyNodes() []string {
	if m.shouldFail {
		return []string{}
	}
	return []string{"node1", "node2"}
}

func (m *mockClient) RecordOperation() {}

func (m *mockClient) Close() error {
	return nil
}

func (m *mockClient) RecordBytesRead(n int64) {
	// No-op for mock
}

// TestErrorFile tests that FS_ERROR.txt appears when backend fails
func TestErrorFileAppearsOnBackendFailure(t *testing.T) {
	// Create mock client that fails
	mockCli := &mockClient{
		shouldFail: true,
		failError:  fmt.Errorf("connection refused"),
	}

	// Create root node
	root := NewRoot(mockCli, nil, nil)

	// Try to list root directory - should return error file
	ctx := context.Background()
	stream, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir failed with errno: %v", errno)
	}

	// Collect entries by calling HasNext/Next
	var foundErrorFile bool
	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 {
			break
		}
		if entry.Name == "FS_ERROR.txt" {
			foundErrorFile = true
			if entry.Mode&uint32(syscall.S_IFREG) == 0 {
				t.Error("FS_ERROR.txt should be a regular file")
			}
			break
		}
	}

	if !foundErrorFile {
		t.Error("FS_ERROR.txt not found in root directory when backend is failing")
	}
}

// TestErrorFileDisappearsOnRecovery tests that FS_ERROR.txt disappears when backend recovers
func TestErrorFileDisappearsOnRecovery(t *testing.T) {
	// Create mock client that initially fails
	mockCli := &mockClient{
		shouldFail: true,
		failError:  fmt.Errorf("connection refused"),
	}

	root := NewRoot(mockCli, nil, nil)
	ctx := context.Background()

	// First readdir - should show error file
	stream1, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("First Readdir failed with errno: %v", errno)
	}

	foundErrorFile := false
	for stream1.HasNext() {
		entry, errno := stream1.Next()
		if errno != 0 {
			break
		}
		if entry.Name == "FS_ERROR.txt" {
			foundErrorFile = true
			break
		}
	}

	if !foundErrorFile {
		t.Error("FS_ERROR.txt should appear when backend is down")
	}

	// Now "fix" the backend
	mockCli.shouldFail = false
	mockCli.entries = []*pb.DirEntry{
		{Name: "normal_file.txt", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 10},
	}

	// Second readdir - error file should NOT appear
	stream2, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Second Readdir failed with errno: %v", errno)
	}

	foundErrorFile = false
	foundNormalFile := false
	for stream2.HasNext() {
		entry, errno := stream2.Next()
		if errno != 0 {
			break
		}
		if entry.Name == "FS_ERROR.txt" {
			foundErrorFile = true
		}
		if entry.Name == "normal_file.txt" {
			foundNormalFile = true
		}
	}

	if foundErrorFile {
		t.Error("FS_ERROR.txt should disappear when backend recovers")
	}
	if !foundNormalFile {
		t.Error("Normal files should be visible when backend recovers")
	}
}

// TestErrorFileLookup tests that FS_ERROR.txt can be looked up
func TestErrorFileLookup(t *testing.T) {
	mockCli := &mockClient{
		shouldFail: true,
		failError:  fmt.Errorf("connection refused"),
	}

	root := NewRoot(mockCli, nil, nil)
	ctx := context.Background()

	// Trigger an error to set backend error state
	_, _ = root.Readdir(ctx)

	// Verify the error state is set
	if !root.hasBackendError() {
		t.Fatal("Backend error should be set after failed Readdir")
	}

	err, errTime := root.getBackendError()
	if err == nil {
		t.Fatal("Backend error should not be nil")
	}
	if errTime.IsZero() {
		t.Error("Error time should be set")
	}
}

// TestErrorFileContent tests that FS_ERROR.txt contains error information
func TestErrorFileContent(t *testing.T) {
	mockCli := &mockClient{
		shouldFail: true,
		failError:  fmt.Errorf("backend connection timeout"),
	}

	root := NewRoot(mockCli, nil, nil)
	ctx := context.Background()

	// Trigger error
	_, _ = root.Readdir(ctx)

	// Get the error details
	err, _ := root.getBackendError()
	if err == nil {
		t.Fatal("Backend error should be set")
	}

	// Verify error message
	if err.Error() != "backend connection timeout" {
		t.Errorf("Expected error 'backend connection timeout', got: %v", err)
	}
}

// TestErrorFileNotPresentWhenHealthy tests that FS_ERROR.txt doesn't appear when backend is healthy
func TestErrorFileNotPresentWhenHealthy(t *testing.T) {
	mockCli := &mockClient{
		shouldFail: false,
		entries: []*pb.DirEntry{
			{Name: "file1.txt", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 2},
		},
	}

	root := NewRoot(mockCli, nil, nil)
	ctx := context.Background()

	// Readdir should not include error file
	stream, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir failed with errno: %v", errno)
	}

	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 {
			break
		}
		if entry.Name == "FS_ERROR.txt" {
			t.Error("FS_ERROR.txt should not appear when backend is healthy")
		}
	}

	// Lookup should return ENOENT
	var out fuse.EntryOut
	_, errno = root.Lookup(ctx, "FS_ERROR.txt", &out)
	if errno != syscall.ENOENT {
		t.Errorf("Lookup of FS_ERROR.txt should return ENOENT when healthy, got: %v", errno)
	}
}

func TestUserRootDir_CreateAndRemove(t *testing.T) {
	tmpDir := t.TempDir()

	// Create session manager
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// Create user root dir through session manager
	err = sm.CreateUserRootDir("myuserdir")
	if err != nil {
		t.Fatalf("CreateUserRootDir failed: %v", err)
	}

	// Verify directory is tracked
	if !sm.IsUserRootDir("myuserdir") {
		t.Error("Expected myuserdir to be tracked as user root dir")
	}

	// Verify it appears in list
	dirs := sm.ListUserRootDirs()
	found := false
	for _, d := range dirs {
		if d == "myuserdir" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected myuserdir in list")
	}

	// Remove should work
	err = sm.RemoveUserRootDir("myuserdir")
	if err != nil {
		t.Fatalf("RemoveUserRootDir failed: %v", err)
	}

	// Should no longer be tracked
	if sm.IsUserRootDir("myuserdir") {
		t.Error("Expected myuserdir to be removed from tracking")
	}
}

func TestUserRootDir_ProtectRepoDirectories(t *testing.T) {
	tmpDir := t.TempDir()

	// Create session manager
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// github.com is NOT a user root dir (it's a repo directory)
	if sm.IsUserRootDir("github.com") {
		t.Error("github.com should not be a user root dir")
	}

	// Create a user dir
	sm.CreateUserRootDir("myuserdir")

	// myuserdir IS a user root dir
	if !sm.IsUserRootDir("myuserdir") {
		t.Error("myuserdir should be a user root dir")
	}

	// This distinction allows Rmdir to protect repo dirs
	// (tested at integration level with actual FUSE mount)
}

func TestSymlink_CreateAndRead(t *testing.T) {
	tmpDir := t.TempDir()

	// Create session manager
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// Create symlink through session manager
	err = sm.CreateSymlink("mydir/mylink", "/target/path")
	if err != nil {
		t.Fatalf("CreateSymlink failed: %v", err)
	}

	// Verify symlink is tracked
	if !sm.IsSymlink("mydir/mylink") {
		t.Error("Expected mydir/mylink to be tracked as symlink")
	}

	target, ok := sm.GetSymlinkTarget("mydir/mylink")
	if !ok {
		t.Error("Expected to find symlink target")
	}
	if target != "/target/path" {
		t.Errorf("Expected target /target/path, got %s", target)
	}

	// Verify local symlink was created
	localPath, _ := sm.GetLocalPath("mydir/mylink")
	linkTarget, err := os.Readlink(localPath)
	if err != nil {
		t.Fatalf("failed to read local symlink: %v", err)
	}
	if linkTarget != "/target/path" {
		t.Errorf("Expected local symlink target /target/path, got %s", linkTarget)
	}
}

func TestSymlink_NotAllowedAtRoot(t *testing.T) {
	tmpDir := t.TempDir()

	// Create session manager
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Create mock client
	mockCli := &mockClient{
		shouldFail: false,
	}

	// Create root node with session
	root := NewRootWithSession(mockCli, nil, sm, nil)

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	ctx := context.Background()
	var out fuse.EntryOut

	// Try to create symlink at root - should fail (doesn't require mounted FUSE)
	// Note: This test uses the node method directly, which will return error
	// before trying to create inode
	_, errno := root.Symlink(ctx, "/target", "rootlink", &out)
	if errno != syscall.EROFS {
		t.Errorf("Expected EROFS when creating symlink at root, got: %v", errno)
	}
}

func TestOverlay_MergeUserRootDirs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create session manager
	sm, err := NewSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Start session
	_, err = sm.StartSession()
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// Create user root dirs
	sm.CreateUserRootDir("userdir1")
	sm.CreateUserRootDir("userdir2")

	// Create overlay manager
	om := NewOverlayManager(sm)

	// Backend entries (simulating repos)
	backendEntries := []fuse.DirEntry{
		{Name: "github.com", Mode: 0755 | uint32(fuse.S_IFDIR), Ino: 1},
	}

	// Merge at root level
	merged := om.MergeReadDir(backendEntries, "")

	// Should have 3 entries: github.com + userdir1 + userdir2
	if len(merged) != 3 {
		t.Errorf("Expected 3 entries after merge, got %d", len(merged))
	}

	// Check all entries are present
	names := make(map[string]bool)
	for _, e := range merged {
		names[e.Name] = true
	}

	if !names["github.com"] {
		t.Error("Expected github.com in merged entries")
	}
	if !names["userdir1"] {
		t.Error("Expected userdir1 in merged entries")
	}
	if !names["userdir2"] {
		t.Error("Expected userdir2 in merged entries")
	}

	// Remove one user dir
	sm.RemoveUserRootDir("userdir1")

	// Merge again
	merged = om.MergeReadDir(backendEntries, "")

	// Should have 2 entries now
	if len(merged) != 2 {
		t.Errorf("Expected 2 entries after removal, got %d", len(merged))
	}

	names = make(map[string]bool)
	for _, e := range merged {
		names[e.Name] = true
	}

	if names["userdir1"] {
		t.Error("Expected userdir1 to be removed from merged entries")
	}
	if !names["userdir2"] {
		t.Error("Expected userdir2 to still be in merged entries")
	}
}
