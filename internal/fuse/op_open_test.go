package fuse

import (
	"context"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestMonoNode_Open_NoSessionWriteReturnsEROFS(t *testing.T) {
	node := &MonoNode{path: "repo/file.txt", logger: testLogger()}
	if _, _, errno := node.Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
		t.Fatalf("Open errno = %v, want EROFS", errno)
	}
}

func TestMonoNode_Open_ReadOnlyWorkspaceWriteReturnsEROFS(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	node := &MonoNode{path: ".monofs/system/config.json", sessionMgr: sessionMgr, logger: testLogger()}
	if _, _, errno := node.Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
		t.Fatalf("Open errno = %v, want EROFS", errno)
	}
}

func TestMonoNode_Open_WriteWithoutTruncateDoesNotDirty(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	client := &pathMockClient{
		readFunc: func(context.Context, string, int64, int64) ([]byte, error) {
			return []byte("backend content"), nil
		},
	}
	node := &MonoNode{
		path:       "repo/file.txt",
		mode:       0644 | uint32(syscall.S_IFREG),
		size:       16,
		client:     client,
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}

	fh, flags, errno := node.Open(context.Background(), syscall.O_WRONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	if fh == nil {
		t.Fatal("Open returned no handle")
	}
	if flags != fuse.FOPEN_DIRECT_IO {
		t.Fatalf("flags = %#x, want FOPEN_DIRECT_IO", flags)
	}
	if node.isLocalWrite {
		t.Fatal("opening for write marked the node dirty without a write")
	}
	if _, ok := fh.(*monofsFileHandle); !ok {
		t.Fatalf("handle type = %T", fh)
	}
	if errno := fh.(*monofsFileHandle).Release(context.Background()); errno != 0 {
		t.Fatalf("Release errno = %v", errno)
	}
}

func TestMonoNode_Open_TruncateMarksDirty(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	client := &pathMockClient{
		readFunc: func(context.Context, string, int64, int64) ([]byte, error) {
			return []byte("backend content"), nil
		},
	}
	node := &MonoNode{
		path:       "repo/file.txt",
		mode:       0644 | uint32(syscall.S_IFREG),
		size:       16,
		client:     client,
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}

	fh, _, errno := node.Open(context.Background(), syscall.O_WRONLY|syscall.O_TRUNC)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	if !node.isLocalWrite {
		t.Fatal("O_TRUNC did not mark the node dirty")
	}
	fh.(*monofsFileHandle).Release(context.Background())
}

func TestMonoNode_Open_DeletedOverlayPathReturnsENOENT(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	if _, err := sessionMgr.StartSession(); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.TrackChange(ChangeDelete, "repo/file.txt", ""); err != nil {
		t.Fatalf("TrackChange: %v", err)
	}
	node := &MonoNode{
		path:       "repo/file.txt",
		sessionMgr: sessionMgr,
		client:     &pathMockClient{},
		logger:     testLogger(),
	}
	if _, _, errno := node.Open(context.Background(), syscall.O_RDONLY); errno != syscall.ENOENT {
		t.Fatalf("Open errno = %v, want ENOENT", errno)
	}
}
