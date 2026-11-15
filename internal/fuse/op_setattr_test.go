package fuse

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func setAttrIn(valid uint32, size uint64, mode uint32) *fuse.SetAttrIn {
	return &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{Valid: valid, Size: size, Mode: mode}}
}

func TestMonoNode_Setattr_NoSessionReturnsEROFS(t *testing.T) {
	node := &MonoNode{path: "repo/file.txt", logger: testLogger()}
	out := &fuse.AttrOut{}
	errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_SIZE, 0, 0), out)
	if errno != syscall.EROFS {
		t.Fatalf("Setattr errno = %v, want EROFS", errno)
	}
}

func TestMonoNode_Setattr_ReadOnlyWorkspaceReturnsEROFS(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	node := &MonoNode{path: ".monofs/system/config.json", sessionMgr: sessionMgr, logger: testLogger()}
	errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_MODE, 0, 0644), &fuse.AttrOut{})
	if errno != syscall.EROFS {
		t.Fatalf("Setattr errno = %v, want EROFS", errno)
	}
}

func TestMonoNode_Setattr_TruncateToZeroSkipsBackend(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	if _, err := sessionMgr.StartSession(); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	client := &pathMockClient{
		readFunc: func(context.Context, string, int64, int64) ([]byte, error) {
			t.Fatal("truncate-to-zero must not fetch backend content")
			return nil, nil
		},
	}
	node := &MonoNode{
		path:       "repo/file.txt",
		mode:       0644 | uint32(syscall.S_IFREG),
		size:       12,
		client:     client,
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}

	out := &fuse.AttrOut{}
	errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_SIZE, 0, 0), out)
	if errno != 0 {
		t.Fatalf("Setattr errno = %v", errno)
	}
	if out.Size != 0 || node.size != 0 {
		t.Fatalf("size = %d / %d, want 0", out.Size, node.size)
	}
	localPath, err := sessionMgr.GetLocalPath("repo/file.txt")
	if err != nil {
		t.Fatalf("GetLocalPath: %v", err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("local file missing: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("local file size = %d, want 0", info.Size())
	}
}

func TestMonoNode_Setattr_NonZeroTruncate(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "0123456789")

	if errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_SIZE, 4, 0), &fuse.AttrOut{}); errno != 0 {
		t.Fatalf("shrink errno = %v", errno)
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "0123" {
		t.Fatalf("content after shrink = %q", content)
	}

	if errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_SIZE, 20, 0), &fuse.AttrOut{}); errno != 0 {
		t.Fatalf("grow errno = %v", errno)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 20 || node.size != 20 {
		t.Fatalf("size = %d / %d, want 20", info.Size(), node.size)
	}
}

func TestMonoNode_Setattr_ChmodPreservesFileType(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")

	out := &fuse.AttrOut{}
	errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_MODE, 0, 0600), out)
	if errno != 0 {
		t.Fatalf("Setattr errno = %v", errno)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("local mode = %04o, want 0600", info.Mode().Perm())
	}
	if out.Mode&uint32(syscall.S_IFMT) != uint32(syscall.S_IFREG) {
		t.Fatalf("out.Mode = %#o lost the regular-file type bits", out.Mode)
	}
	if out.Mode&0777 != 0600 {
		t.Fatalf("out.Mode perms = %04o, want 0600", out.Mode&0777)
	}
}

func TestMonoNode_Setattr_ChmodDirectoryCreatesDirectory(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	if _, err := sessionMgr.StartSession(); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	node := &MonoNode{
		path:       "workspace-dir",
		isDir:      true,
		mode:       0755 | uint32(syscall.S_IFDIR),
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}

	errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_MODE, 0, 0700), &fuse.AttrOut{})
	if errno != 0 {
		t.Fatalf("Setattr errno = %v", errno)
	}
	localPath, err := sessionMgr.GetLocalPath("workspace-dir")
	if err != nil {
		t.Fatalf("GetLocalPath: %v", err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("local directory missing: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("chmod on a directory created a %s instead of a directory", info.Mode().Type())
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode = %04o, want 0700", info.Mode().Perm())
	}
}

func TestMonoNode_Setattr_DirectorySizeReturnsEINVAL(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	node := &MonoNode{path: "workspace-dir", isDir: true, sessionMgr: sessionMgr, logger: testLogger()}
	errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_SIZE, 4, 0), &fuse.AttrOut{})
	if errno != syscall.EINVAL {
		t.Fatalf("Setattr errno = %v, want EINVAL", errno)
	}
}

func TestMonoNode_Setattr_TrackFailureReturnsEIO(t *testing.T) {
	node, sessionMgr, localPath := newFileHandleTestNode(t, "repo/file.txt", "abcdef")

	sessionMgr.mu.Lock()
	db := sessionMgr.db
	sessionMgr.db = nil
	sessionMgr.mu.Unlock()
	defer func() {
		sessionMgr.mu.Lock()
		sessionMgr.db = db
		sessionMgr.mu.Unlock()
	}()

	if errno := node.Setattr(context.Background(), nil, setAttrIn(fuse.FATTR_SIZE, 2, 0), &fuse.AttrOut{}); errno != syscall.EIO {
		t.Fatalf("Setattr errno = %v, want EIO", errno)
	}
	// The mutation was applied; only persistence failed.
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2 {
		t.Fatalf("size = %d, want 2", info.Size())
	}
}

func TestMonoNode_Setattr_TimestampsAndOwner(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")

	atime := time.Unix(1_600_000_000, 123)
	mtime := time.Unix(1_600_000_100, 456)
	in := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{
		Valid:     fuse.FATTR_ATIME | fuse.FATTR_MTIME,
		Atime:     uint64(atime.Unix()),
		Atimensec: uint32(atime.Nanosecond()),
		Mtime:     uint64(mtime.Unix()),
		Mtimensec: uint32(mtime.Nanosecond()),
	}}
	out := &fuse.AttrOut{}
	if errno := node.Setattr(context.Background(), nil, in, out); errno != 0 {
		t.Fatalf("Setattr errno = %v", errno)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ModTime().Unix(); got != mtime.Unix() {
		t.Fatalf("mtime = %d, want %d", got, mtime.Unix())
	}
	if out.Mtime != uint64(mtime.Unix()) {
		t.Fatalf("out.Mtime = %d, want %d", out.Mtime, mtime.Unix())
	}

	// Chown to the current owner should succeed (no-op) when permitted.
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	in = &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{
		Valid: fuse.FATTR_UID | fuse.FATTR_GID,
		Owner: fuse.Owner{Uid: uid, Gid: gid},
	}}
	if errno := node.Setattr(context.Background(), nil, in, &fuse.AttrOut{}); errno != 0 {
		// Rootless sandboxes may not permit chown even to the current owner.
		if errno != syscall.EPERM {
			t.Fatalf("Setattr chown errno = %v", errno)
		}
	}
}
