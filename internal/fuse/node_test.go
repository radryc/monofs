package fuse

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	pb "github.com/radryc/monofs/api/proto"
	monoclient "github.com/radryc/monofs/internal/client"
	"github.com/radryc/monofs/internal/fsstat"
	"github.com/radryc/monofs/internal/workspacebundle"
)

// mockClient implements MonoFSClient for testing
type mockClient struct {
	shouldFail     bool
	failError      error
	entries        []*pb.DirEntry
	statfs         fsstat.Snapshot
	workspaceRepos []monoclient.WorkspaceRepository
	listCalls      int
	resolveCalls   int
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

func (m *mockClient) StatFS(ctx context.Context) (fsstat.Snapshot, error) {
	if m.shouldFail {
		return fsstat.Snapshot{}, m.failError
	}
	if m.statfs != (fsstat.Snapshot{}) {
		return m.statfs, nil
	}
	return fsstat.FromUsage(0, 0), nil
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

func (m *mockClient) RecordError() {
	// No-op for mock
}

func (m *mockClient) IsGuardianVisible() bool {
	return false
}

func (m *mockClient) QueryLogs(ctx context.Context, query string) ([]byte, error) {
	return nil, nil
}

func (m *mockClient) WriteQueryLogs(ctx context.Context, query string, writer io.Writer) error {
	_, err := writer.Write(nil)
	return err
}

func (m *mockClient) ListWorkspaceRepositories(ctx context.Context) ([]monoclient.WorkspaceRepository, error) {
	m.listCalls++
	return append([]monoclient.WorkspaceRepository(nil), m.workspaceRepos...), nil
}

func (m *mockClient) ResolveWorkspacePath(ctx context.Context, path string) (*monoclient.WorkspaceRepository, error) {
	m.resolveCalls++
	trimmed := strings.Trim(path, "/")
	var match *monoclient.WorkspaceRepository
	for i := range m.workspaceRepos {
		repo := m.workspaceRepos[i]
		if trimmed != repo.DisplayPath && !strings.HasPrefix(trimmed, repo.DisplayPath+"/") {
			continue
		}
		if match == nil || len(repo.DisplayPath) > len(match.DisplayPath) {
			candidate := repo
			match = &candidate
		}
	}
	if match == nil {
		return nil, monoclient.ErrWorkspacePathNotFound
	}
	return match, nil
}

func (m *mockClient) PublishWorkspaceBundle(ctx context.Context, bundle *workspacebundle.Bundle, opts monoclient.WorkspacePublishOptions) (*monoclient.WorkspacePublishResult, error) {
	return nil, fmt.Errorf("workspace publish not configured")
}

func (m *mockClient) PushWorkspaceCommitBundle(ctx context.Context, bundle *workspacebundle.SourceCommitBundle) (*monoclient.WorkspaceSourcePushResult, error) {
	return nil, fmt.Errorf("workspace source push not configured")
}

func collectDirEntryNames(stream fs.DirStream) []string {
	var names []string
	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 {
			break
		}
		names = append(names, entry.Name)
	}
	return names
}

func TestStatfsUsesClientSnapshot(t *testing.T) {
	mockCli := &mockClient{
		statfs: fsstat.FromUsage(128*1024, 42),
	}

	root := NewRoot(mockCli, nil, nil)
	var out fuse.StatfsOut

	if errno := root.Statfs(context.Background(), &out); errno != 0 {
		t.Fatalf("Statfs() errno = %v", errno)
	}

	if got, want := out.Blocks, mockCli.statfs.Blocks; got != want {
		t.Fatalf("Blocks = %d, want %d", got, want)
	}
	if got, want := out.Bfree, mockCli.statfs.Bfree; got != want {
		t.Fatalf("Bfree = %d, want %d", got, want)
	}
	if got, want := out.Files, mockCli.statfs.Files; got != want {
		t.Fatalf("Files = %d, want %d", got, want)
	}
}

func TestStatfsMapsBackendErrorsToEIO(t *testing.T) {
	root := NewRoot(&mockClient{
		shouldFail: true,
		failError:  fmt.Errorf("router unavailable"),
	}, nil, nil)

	var out fuse.StatfsOut
	if errno := root.Statfs(context.Background(), &out); errno != syscall.EIO {
		t.Fatalf("Statfs() errno = %v, want %v", errno, syscall.EIO)
	}
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

	errTime, err := root.getBackendError()
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
	_, err := root.getBackendError()
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

func TestVirtualMonorepoRootFiltersNamespacesAndAddsGitignore(t *testing.T) {
	root := NewRoot(&mockClient{
		entries: []*pb.DirEntry{
			{Name: "github.com", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 1},
			{Name: "gitlab.com", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 2},
			{Name: "dependency", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 3},
			{Name: "guardian", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 4},
			{Name: "guardian-system", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 5},
			{Name: "doctor", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 6},
		},
	}, nil, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}

	stream, errno := root.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir() errno = %v", errno)
	}

	names := collectDirEntryNames(stream)
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, syntheticGitignoreName) {
		t.Fatalf("root listing missing %q: %v", syntheticGitignoreName, names)
	}
	if !strings.Contains(joined, "github.com") || !strings.Contains(joined, "gitlab.com") {
		t.Fatalf("root listing missing expected source namespaces: %v", names)
	}
	if !strings.Contains(joined, "dependency") {
		t.Fatalf("root listing missing visible dependency namespace: %v", names)
	}
	for _, hidden := range []string{"doctor", "guardian", "guardian-system"} {
		if strings.Contains(joined, hidden) {
			t.Fatalf("root listing should hide %q: %v", hidden, names)
		}
	}
}

func TestVirtualMonorepoServesSyntheticRootGitFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}

	mountPoint := t.TempDir()
	stateDir := t.TempDir()
	root := NewRoot(&mockClient{}, nil, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}
	if err := root.EnableWorkspaceGitProjection(mountPoint, stateDir); err != nil {
		t.Fatalf("EnableWorkspaceGitProjection() error = %v", err)
	}

	stream, errno := root.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir() errno = %v", errno)
	}
	if names := collectDirEntryNames(stream); !strings.Contains(strings.Join(names, ","), syntheticWorkspaceGitName) {
		t.Fatalf("root listing missing %q: %v", syntheticWorkspaceGitName, names)
	}

	var entryOut fuse.EntryOut
	_, errno = root.Lookup(context.Background(), syntheticWorkspaceGitName, &entryOut)
	if errno != 0 {
		t.Fatalf("Lookup(.git) errno = %v", errno)
	}
	content, ok := root.syntheticWorkspaceFileContent(syntheticWorkspaceGitName)
	if !ok {
		t.Fatal("synthetic .git content was not available")
	}
	if !strings.HasPrefix(string(content), "gitdir: ") {
		t.Fatalf("synthetic .git content = %q, want gitdir pointer", string(content))
	}
	if got, want := entryOut.Size, uint64(len(content)); got != want {
		t.Fatalf("Lookup(.git) size = %d, want %d", got, want)
	}
}

func TestVisibleOwnerAppliesToRootAndSyntheticWorkspaceFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}

	const wantUID = 4242
	const wantGID = 4343

	root := NewRoot(&mockClient{}, nil, testLogger())
	root.SetVisibleOwner(wantUID, wantGID)

	var rootAttr fuse.AttrOut
	if errno := root.Getattr(context.Background(), nil, &rootAttr); errno != 0 {
		t.Fatalf("Getattr(root) errno = %v", errno)
	}
	if rootAttr.Uid != wantUID || rootAttr.Gid != wantGID {
		t.Fatalf("Getattr(root) owner = (%d, %d), want (%d, %d)", rootAttr.Uid, rootAttr.Gid, wantUID, wantGID)
	}

	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}
	if err := root.EnableWorkspaceGitProjection(t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("EnableWorkspaceGitProjection() error = %v", err)
	}

	var gitignoreOut fuse.EntryOut
	if _, errno := root.Lookup(context.Background(), syntheticGitignoreName, &gitignoreOut); errno != 0 {
		t.Fatalf("Lookup(.gitignore) errno = %v", errno)
	}
	if gitignoreOut.Uid != wantUID || gitignoreOut.Gid != wantGID {
		t.Fatalf("Lookup(.gitignore) owner = (%d, %d), want (%d, %d)", gitignoreOut.Uid, gitignoreOut.Gid, wantUID, wantGID)
	}

	var gitOut fuse.EntryOut
	if _, errno := root.Lookup(context.Background(), syntheticWorkspaceGitName, &gitOut); errno != 0 {
		t.Fatalf("Lookup(.git) errno = %v", errno)
	}
	if gitOut.Uid != wantUID || gitOut.Gid != wantGID {
		t.Fatalf("Lookup(.git) owner = (%d, %d), want (%d, %d)", gitOut.Uid, gitOut.Gid, wantUID, wantGID)
	}
}

func TestVirtualMonorepoLookupHidesNestedGitAndServesSyntheticGitignore(t *testing.T) {
	mockCli := &mockClient{}
	root := NewRoot(mockCli, nil, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}

	var entryOut fuse.EntryOut
	_, errno := root.Lookup(context.Background(), syntheticGitignoreName, &entryOut)
	if errno != 0 {
		t.Fatalf("Lookup(.gitignore) errno = %v", errno)
	}
	if got, want := entryOut.Size, uint64(len(monorepoGitignore)); got != want {
		t.Fatalf("synthetic .gitignore size = %d, want %d", got, want)
	}
	content, ok := root.syntheticWorkspaceFileContent(syntheticGitignoreName)
	if !ok {
		t.Fatal("synthetic .gitignore content was not available")
	}
	if string(content) != monorepoGitignore {
		t.Fatalf("synthetic .gitignore content = %q, want %q", string(content), monorepoGitignore)
	}
	child := root.newChild(syntheticGitignoreName, false, 0444|uint32(syscall.S_IFREG), uint64(len(content)))
	child.content = content

	var attrOut fuse.AttrOut
	if errno := child.Getattr(context.Background(), nil, &attrOut); errno != 0 {
		t.Fatalf("Getattr(.gitignore) errno = %v", errno)
	}
	if attrOut.Size != uint64(len(monorepoGitignore)) {
		t.Fatalf("Getattr(.gitignore) size = %d, want %d", attrOut.Size, len(monorepoGitignore))
	}

	repoNode := root.newChild("github.com/acme/repo", true, 0755|uint32(syscall.S_IFDIR), 0)
	_, errno = repoNode.Lookup(context.Background(), ".git", &entryOut)
	if errno != syscall.ENOENT {
		t.Fatalf("Lookup(.git) errno = %v, want %v", errno, syscall.ENOENT)
	}
}

func TestVirtualMonorepoAllowsDependencyWritesAndRejectsReservedWrites(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}

	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}

	root := NewRootWithSession(&mockClient{}, nil, sessionMgr, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}
	if err := root.EnableWorkspaceGitProjection(t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("EnableWorkspaceGitProjection() error = %v", err)
	}

	var entryOut fuse.EntryOut
	if _, errno := root.Mkdir(context.Background(), "dependency", 0755, &entryOut); errno != 0 {
		t.Fatalf("Mkdir(dependency) errno = %v, want success", errno)
	}
	if !sessionMgr.IsUserRootDir("dependency") {
		t.Fatal("dependency should be tracked as a writable root directory")
	}
	if _, errno := root.Mkdir(context.Background(), "doctor", 0755, &entryOut); errno != syscall.EPERM {
		t.Fatalf("Mkdir(doctor) errno = %v, want %v", errno, syscall.EPERM)
	}
	if _, errno := root.Mkdir(context.Background(), syntheticWorkspaceGitName, 0755, &entryOut); errno != syscall.EPERM {
		t.Fatalf("Mkdir(.git) errno = %v, want %v", errno, syscall.EPERM)
	}

	repoNode := root.newChild("github.com/acme/repo", true, 0755|uint32(syscall.S_IFDIR), 0)
	if _, _, _, errno := repoNode.Create(context.Background(), ".git", 0, 0644, &entryOut); errno != syscall.EPERM {
		t.Fatalf("Create(.git) errno = %v, want %v", errno, syscall.EPERM)
	}
	if _, errno := repoNode.Mkdir(context.Background(), ".git", 0755, &entryOut); errno != syscall.EPERM {
		t.Fatalf("Mkdir(.git) errno = %v, want %v", errno, syscall.EPERM)
	}
}

func TestWorkspaceManifestResolvePath(t *testing.T) {
	mockCli := &mockClient{
		workspaceRepos: []monoclient.WorkspaceRepository{
			{StorageID: "src-1", DisplayPath: "github.com/acme/repo"},
			{StorageID: "sys-1", DisplayPath: "dependency/go/mod/cache"},
		},
	}
	root := NewRoot(mockCli, nil, testLogger())
	if err := root.EnableVirtualMonorepo(); err != nil {
		t.Fatalf("EnableVirtualMonorepo() error = %v", err)
	}

	resolution, err := root.workspace.ResolvePath(context.Background(), "github.com/acme/repo/pkg/file.go")
	if err != nil {
		t.Fatalf("ResolvePath(source) error = %v", err)
	}
	if resolution.Repository == nil || resolution.Repository.StorageID != "src-1" {
		t.Fatalf("ResolvePath(source) repository = %+v", resolution.Repository)
	}
	if !resolution.Included {
		t.Fatalf("ResolvePath(source) should be included: %+v", resolution)
	}

	resolution, err = root.workspace.ResolvePath(context.Background(), "dependency/go/mod/cache/download")
	if err != nil {
		t.Fatalf("ResolvePath(system) error = %v", err)
	}
	if resolution.Repository == nil || resolution.Repository.StorageID != "sys-1" {
		t.Fatalf("ResolvePath(system) repository = %+v", resolution.Repository)
	}
	if resolution.Included {
		t.Fatalf("ResolvePath(system) should be excluded: %+v", resolution)
	}
	if resolution.ExclusionReason != WorkspaceExcludedSystemNamespace {
		t.Fatalf("ResolvePath(system) reason = %q, want %q", resolution.ExclusionReason, WorkspaceExcludedSystemNamespace)
	}
	if mockCli.listCalls != 1 {
		t.Fatalf("ListWorkspaceRepositories() calls = %d, want 1 cached lookup", mockCli.listCalls)
	}
	if mockCli.resolveCalls != 0 {
		t.Fatalf("ResolveWorkspacePath() calls = %d, want 0", mockCli.resolveCalls)
	}
}

func TestVisibleOwnerAppliesToOverlayEntries(t *testing.T) {
	const wantUID = 4242
	const wantGID = 4343

	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	root := NewRootWithSession(&pathMockClient{}, nil, sessionMgr, testLogger())
	root.SetVisibleOwner(wantUID, wantGID)

	if err := sessionMgr.CreateUserRootDir("scratch"); err != nil {
		t.Fatalf("CreateUserRootDir() error = %v", err)
	}

	scratchNode := root.newChild("scratch", true, 0755|uint32(syscall.S_IFDIR), 0)
	var scratchAttr fuse.AttrOut
	if errno := scratchNode.Getattr(context.Background(), nil, &scratchAttr); errno != 0 {
		t.Fatalf("Getattr(scratch) errno = %v", errno)
	}
	if scratchAttr.Uid != wantUID || scratchAttr.Gid != wantGID {
		t.Fatalf("Getattr(scratch) owner = (%d, %d), want (%d, %d)", scratchAttr.Uid, scratchAttr.Gid, wantUID, wantGID)
	}

	localPath, err := sessionMgr.GetLocalPath("scratch/note.txt")
	if err != nil {
		t.Fatalf("GetLocalPath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(localPath), err)
	}
	if err := os.WriteFile(localPath, []byte("note\n"), 0644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", localPath, err)
	}
	if err := sessionMgr.TrackChange(ChangeCreate, "scratch/note.txt", ""); err != nil {
		t.Fatalf("TrackChange() error = %v", err)
	}

	fileNode := scratchNode.newChild("note.txt", false, 0644|uint32(syscall.S_IFREG), 0)
	var fileAttr fuse.AttrOut
	if errno := fileNode.Getattr(context.Background(), nil, &fileAttr); errno != 0 {
		t.Fatalf("Getattr(scratch/note.txt) errno = %v", errno)
	}
	if fileAttr.Uid != wantUID || fileAttr.Gid != wantGID {
		t.Fatalf("Getattr(scratch/note.txt) owner = (%d, %d), want (%d, %d)", fileAttr.Uid, fileAttr.Gid, wantUID, wantGID)
	}
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
