package fuse

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	monoclient "github.com/radryc/monofs/internal/client"
	"github.com/radryc/monofs/internal/fsstat"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeFSClient is a tiny in-memory MonoFS backend for headless filesystem tests.
type fakeFSClient struct {
	files map[string][]byte
	dirs  map[string][]*pb.DirEntry
	repos []monoclient.WorkspaceRepository
}

func newFakeFSClient() *fakeFSClient {
	return &fakeFSClient{
		files: make(map[string][]byte),
		dirs:  make(map[string][]*pb.DirEntry),
	}
}

func (f *fakeFSClient) Lookup(ctx context.Context, path string) (*pb.LookupResponse, error) {
	attr, err := f.GetAttr(ctx, path)
	if err != nil {
		return nil, err
	}
	return &pb.LookupResponse{Found: true, Ino: attr.Ino, Mode: attr.Mode, Size: attr.Size, Mtime: attr.Mtime}, nil
}

func (f *fakeFSClient) GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error) {
	if content, ok := f.files[path]; ok {
		return &pb.GetAttrResponse{
			Found: true,
			Ino:   1,
			Mode:  0644 | uint32(syscall.S_IFREG),
			Size:  uint64(len(content)),
			Mtime: 1700000000,
		}, nil
	}
	if _, ok := f.dirs[path]; ok {
		return &pb.GetAttrResponse{
			Found: true,
			Ino:   2,
			Mode:  0755 | uint32(syscall.S_IFDIR),
			Mtime: 1700000000,
		}, nil
	}
	return nil, status.Error(codes.NotFound, "not found")
}

func (f *fakeFSClient) ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error) {
	entries, ok := f.dirs[path]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return entries, nil
}

func (f *fakeFSClient) Read(ctx context.Context, path string, offset, size int64) ([]byte, error) {
	content, ok := f.files[path]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	if offset >= int64(len(content)) {
		return nil, nil
	}
	end := int64(len(content))
	if size > 0 && offset+size < end {
		end = offset + size
	}
	return content[offset:end], nil
}

func (f *fakeFSClient) StatFS(ctx context.Context) (fsstat.Snapshot, error) {
	return fsstat.FromUsage(0, 0), nil
}

func (f *fakeFSClient) QueryLogs(ctx context.Context, query string) ([]byte, error) {
	return nil, nil
}

func (f *fakeFSClient) WriteQueryLogs(ctx context.Context, query string, writer io.Writer) error {
	return nil
}

func (f *fakeFSClient) Close() error            { return nil }
func (f *fakeFSClient) RecordOperation()        {}
func (f *fakeFSClient) RecordBytesRead(n int64) {}
func (f *fakeFSClient) RecordError()            {}
func (f *fakeFSClient) IsGuardianVisible() bool { return false }

func (f *fakeFSClient) ListWorkspaceRepositories(ctx context.Context) ([]monoclient.WorkspaceRepository, error) {
	return append([]monoclient.WorkspaceRepository(nil), f.repos...), nil
}

func (f *fakeFSClient) ResolveWorkspacePath(ctx context.Context, path string) (*monoclient.WorkspaceRepository, error) {
	trimmed := strings.Trim(path, "/")
	var match *monoclient.WorkspaceRepository
	for i := range f.repos {
		repo := f.repos[i]
		display := strings.Trim(repo.DisplayPath, "/")
		if trimmed != display && !strings.HasPrefix(trimmed, display+"/") {
			continue
		}
		if match == nil || len(display) > len(strings.Trim(match.DisplayPath, "/")) {
			candidate := repo
			match = &candidate
		}
	}
	if match == nil {
		return nil, monoclient.ErrWorkspacePathNotFound
	}
	return match, nil
}

func newTestWorkspaceFS(t *testing.T, backend *fakeFSClient, virtualMonorepo bool) (*WorkspaceFS, *SessionManager) {
	t.Helper()
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	var manifest *WorkspaceManifest
	if virtualMonorepo {
		manifest = NewWorkspaceManifest(backend)
	}
	return NewWorkspaceFS(backend, sessionMgr, manifest, testLogger()), sessionMgr
}

func TestWorkspaceFSWriteCreatesTopLevelParent(t *testing.T) {
	backend := newFakeFSClient()
	fs, sessionMgr := newTestWorkspaceFS(t, backend, false)
	ctx := context.Background()

	if _, err := fs.WriteWithParents(ctx, ".deps/lib.go", []byte("package lib\n"), 0644, true); err != nil {
		t.Fatalf("WriteWithParents() error = %v", err)
	}
	if !sessionMgr.IsUserRootDir(".deps") {
		t.Fatal(".deps was not registered as a user root directory")
	}
	entries, err := fs.List(ctx, ".deps")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "lib.go" {
		t.Fatalf("List(.deps) = %+v", entries)
	}

	// Without create_parents the write must not invent a user directory.
	if _, err := fs.WriteWithParents(ctx, ".cache/x.go", []byte("x"), 0644, false); err == nil {
		t.Fatal("WriteWithParents(createParents=false) created a missing top-level directory")
	}
}

func TestWorkspaceFSWriteReadStatList(t *testing.T) {
	backend := newFakeFSClient()
	backend.files["github.com/acme/repo/base.go"] = []byte("package base\n")
	backend.dirs["github.com/acme/repo"] = []*pb.DirEntry{
		{Name: "base.go", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 10},
	}
	backend.dirs["github.com/acme"] = []*pb.DirEntry{
		{Name: "repo", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 11},
	}
	fs, sessionMgr := newTestWorkspaceFS(t, backend, false)
	ctx := context.Background()

	const newPath = "github.com/acme/repo/new.go"
	if _, err := fs.Write(ctx, newPath, []byte("package new\n"), 0644); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	content, err := fs.Read(ctx, newPath, 0, 0)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if string(content) != "package new\n" {
		t.Fatalf("Read() = %q", content)
	}

	stat, err := fs.Stat(ctx, newPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if stat.IsDir || stat.Size != int64(len("package new\n")) {
		t.Fatalf("Stat() = %+v", stat)
	}

	entries, err := fs.List(ctx, "github.com/acme/repo")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	names := make(map[string]FileStat, len(entries))
	for _, entry := range entries {
		names[entry.Name] = entry
	}
	if _, ok := names["base.go"]; !ok {
		t.Fatalf("backend entry missing from listing: %+v", entries)
	}
	newEntry, ok := names["new.go"]
	if !ok {
		t.Fatalf("overlay entry missing from listing: %+v", entries)
	}
	if newEntry.Size != int64(len("package new\n")) {
		t.Fatalf("overlay entry size = %d", newEntry.Size)
	}

	change, found, err := sessionMgr.GetOverlayDB().GetFile(newPath)
	if err != nil || !found {
		t.Fatalf("overlay entry not tracked: found=%v err=%v", found, err)
	}
	if change.ChangeType != ChangeCreate {
		t.Fatalf("change type = %q, want create", change.ChangeType)
	}
}

func TestWorkspaceFSModifyBackendFile(t *testing.T) {
	backend := newFakeFSClient()
	const path = "github.com/acme/repo/base.go"
	backend.files[path] = []byte("package base\n")
	backend.dirs["github.com/acme/repo"] = nil
	fs, sessionMgr := newTestWorkspaceFS(t, backend, false)
	ctx := context.Background()

	if _, err := fs.Write(ctx, path, []byte("package changed\n"), 0644); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	content, err := fs.Read(ctx, path, 0, 0)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if string(content) != "package changed\n" {
		t.Fatalf("Read() = %q", content)
	}

	// The original backend content must not be mutated in place.
	if string(backend.files[path]) != "package base\n" {
		t.Fatalf("backend content mutated to %q", backend.files[path])
	}

	entry, found, err := sessionMgr.GetOverlayDB().GetFile(path)
	if err != nil || !found {
		t.Fatalf("overlay entry not tracked: found=%v err=%v", found, err)
	}
	if entry.ChangeType != ChangeModify {
		t.Fatalf("change type = %q, want modify", entry.ChangeType)
	}
}

func TestWorkspaceFSDeletedPathIsInvisible(t *testing.T) {
	backend := newFakeFSClient()
	const parent = "github.com/acme/repo"
	const path = parent + "/base.go"
	backend.files[path] = []byte("package base\n")
	backend.dirs[parent] = []*pb.DirEntry{{Name: "base.go", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 10}}
	fs, sessionMgr := newTestWorkspaceFS(t, backend, false)
	ctx := context.Background()

	if _, err := sessionMgr.StartSession(); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := sessionMgr.TrackChange(ChangeDelete, path, ""); err != nil {
		t.Fatalf("TrackChange() error = %v", err)
	}

	if _, err := fs.Read(ctx, path, 0, 0); !errors.Is(err, ErrFSNotFound) {
		t.Fatalf("Read() error = %v, want ErrFSNotFound", err)
	}
	if _, err := fs.Stat(ctx, path); !errors.Is(err, ErrFSNotFound) {
		t.Fatalf("Stat() error = %v, want ErrFSNotFound", err)
	}
	entries, err := fs.List(ctx, parent)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Name == "base.go" {
			t.Fatalf("deleted entry leaked into listing: %+v", entries)
		}
	}
}

func TestWorkspaceFSRejectsUnsafeWrites(t *testing.T) {
	backend := newFakeFSClient()
	fs, _ := newTestWorkspaceFS(t, backend, false)
	ctx := context.Background()

	cases := []struct {
		name string
		path string
	}{
		{name: "root file", path: "top-level.go"},
		{name: "dependency", path: "dependency/go/mod/cache/x.go"},
		{name: "escaping", path: "../outside.go"},
		{name: "manifest", path: ".monofs/workspace.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fs.Write(ctx, tc.path, []byte("x"), 0644); err == nil {
				t.Fatalf("Write(%q) succeeded, want error", tc.path)
			}
		})
	}
}

func TestWorkspaceFSUserRootDirectory(t *testing.T) {
	backend := newFakeFSClient()
	fs, _ := newTestWorkspaceFS(t, backend, false)
	ctx := context.Background()

	if _, err := fs.Mkdir(ctx, ".deps", 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if _, err := fs.Write(ctx, ".deps/lib.go", []byte("package lib\n"), 0644); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	entries, err := fs.List(ctx, ".deps")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "lib.go" || entries[0].Size != int64(len("package lib\n")) {
		t.Fatalf("List(.deps) = %+v", entries)
	}
}

func TestWorkspaceFSVirtualMonorepoView(t *testing.T) {
	backend := newFakeFSClient()
	backend.repos = []monoclient.WorkspaceRepository{
		{StorageID: "repo", DisplayPath: "github.com/acme/repo", Ref: "main", CommitHash: "abc"},
	}
	backend.dirs[""] = []*pb.DirEntry{
		{Name: "doctor", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 1},
		{Name: "guardian", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 2},
		{Name: "github.com", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 3},
	}
	fs, _ := newTestWorkspaceFS(t, backend, true)
	ctx := context.Background()

	entries, err := fs.List(ctx, "")
	if err != nil {
		t.Fatalf("List(root) error = %v", err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Name] = true
	}
	if names["doctor"] || names["guardian"] {
		t.Fatalf("hidden system namespaces leaked into root listing: %+v", entries)
	}
	if !names["github.com"] {
		t.Fatalf("github.com missing from root listing: %+v", entries)
	}
	if !names[syntheticGitignoreName] || !names[syntheticWorkspaceControlDirName] {
		t.Fatalf("synthetic workspace entries missing: %+v", entries)
	}
	if _, err := fs.List(ctx, "doctor"); !errors.Is(err, ErrFSNotFound) {
		t.Fatalf("List(doctor) error = %v, want ErrFSNotFound", err)
	}

	gitignore, err := fs.Read(ctx, syntheticGitignoreName, 0, 0)
	if err != nil {
		t.Fatalf("Read(.gitignore) error = %v", err)
	}
	if !strings.Contains(string(gitignore), "/dependency/") {
		t.Fatalf("unexpected .gitignore content: %q", gitignore)
	}

	manifest, err := fs.Read(ctx, syntheticWorkspaceManifestPath, 0, 0)
	if err != nil {
		t.Fatalf("Read(workspace.json) error = %v", err)
	}
	if !strings.Contains(string(manifest), "github.com/acme/repo") {
		t.Fatalf("manifest missing repository: %s", manifest)
	}
}

func TestWorkspaceFSSystemViewReadsBackendRoot(t *testing.T) {
	backend := newFakeFSClient()
	backend.files["doctor/app.go"] = []byte("package doctor\n")
	backend.dirs[""] = []*pb.DirEntry{
		{Name: "doctor", Mode: 0755 | uint32(syscall.S_IFDIR), Ino: 1},
	}
	backend.dirs["doctor"] = []*pb.DirEntry{
		{Name: "app.go", Mode: 0644 | uint32(syscall.S_IFREG), Ino: 2},
	}
	fs, _ := newTestWorkspaceFS(t, backend, true)
	ctx := context.Background()

	content, err := fs.Read(ctx, ".monofs/system/doctor/app.go", 0, 0)
	if err != nil {
		t.Fatalf("Read(system view) error = %v", err)
	}
	if string(content) != "package doctor\n" {
		t.Fatalf("Read(system view) = %q", content)
	}
	entries, err := fs.List(ctx, ".monofs/system")
	if err != nil {
		t.Fatalf("List(system view) error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "doctor" || entries[0].Path != ".monofs/system/doctor" {
		t.Fatalf("List(system view) = %+v", entries)
	}

	subEntries, err := fs.List(ctx, ".monofs/system/doctor")
	if err != nil {
		t.Fatalf("List(system subdir) error = %v", err)
	}
	if len(subEntries) != 1 || subEntries[0].Name != "app.go" || subEntries[0].Path != ".monofs/system/doctor/app.go" {
		t.Fatalf("List(system subdir) = %+v", subEntries)
	}
}

func TestWorkspaceFSReadRange(t *testing.T) {
	backend := newFakeFSClient()
	const path = "github.com/acme/repo/base.go"
	backend.files[path] = []byte("0123456789")
	backend.dirs["github.com/acme/repo"] = nil
	fs, _ := newTestWorkspaceFS(t, backend, false)
	ctx := context.Background()

	got, err := fs.Read(ctx, path, 2, 4)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if string(got) != "2345" {
		t.Fatalf("Read() = %q", got)
	}

	if _, err := fs.Write(ctx, path, []byte("abcdefghij"), 0644); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err = fs.Read(ctx, path, 3, 2)
	if err != nil {
		t.Fatalf("Read(overlay) error = %v", err)
	}
	if string(got) != "de" {
		t.Fatalf("Read(overlay) = %q", got)
	}
}

func TestWorkspaceFSMkdirNestedAndIdempotent(t *testing.T) {
	backend := newFakeFSClient()
	backend.dirs["github.com/acme/repo"] = nil
	fs, sessionMgr := newTestWorkspaceFS(t, backend, false)
	ctx := context.Background()

	stat, err := fs.Mkdir(ctx, "github.com/acme/repo/internal", 0755)
	if err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if !stat.IsDir {
		t.Fatalf("Mkdir() stat = %+v", stat)
	}
	if _, err := fs.Mkdir(ctx, "github.com/acme/repo/internal", 0); err != nil {
		t.Fatalf("Mkdir(existing) error = %v", err)
	}
	entry, found, err := sessionMgr.GetOverlayDB().GetFile("github.com/acme/repo/internal")
	if err != nil || !found {
		t.Fatalf("directory not tracked: found=%v err=%v", found, err)
	}
	if entry.Type != FileEntryDir {
		t.Fatalf("entry type = %q, want dir", entry.Type)
	}
}

func TestWorkspaceFSStatSyntheticFiles(t *testing.T) {
	backend := newFakeFSClient()
	backend.repos = []monoclient.WorkspaceRepository{{StorageID: "repo", DisplayPath: "github.com/acme/repo"}}
	fs, _ := newTestWorkspaceFS(t, backend, true)
	ctx := context.Background()

	stat, err := fs.Stat(ctx, syntheticGitignoreName)
	if err != nil {
		t.Fatalf("Stat(.gitignore) error = %v", err)
	}
	if stat.IsDir || stat.Mode&uint32(syscall.S_IFREG) == 0 {
		t.Fatalf("Stat(.gitignore) = %+v", stat)
	}
	stat, err = fs.Stat(ctx, syntheticWorkspaceManifestPath)
	if err != nil {
		t.Fatalf("Stat(workspace.json) error = %v", err)
	}
	if stat.IsDir || stat.Size == 0 {
		t.Fatalf("Stat(workspace.json) = %+v", stat)
	}
}
