package fuse

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMonoNode_EnsureLocalCopyTransientErrorCreatesNoFile(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	if _, err := sessionMgr.StartSession(); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	client := &pathMockClient{
		readFunc: func(context.Context, string, int64, int64) ([]byte, error) {
			return nil, errors.New("transport unavailable")
		},
	}
	node := &MonoNode{
		path:       "repo/file.txt",
		mode:       0644 | uint32(0o100000),
		client:     client,
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}

	if err := node.ensureLocalCopy(context.Background()); err == nil {
		t.Fatal("transient backend error was swallowed")
	}
	localPath, err := sessionMgr.GetLocalPath("repo/file.txt")
	if err != nil {
		t.Fatalf("GetLocalPath: %v", err)
	}
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Fatalf("an empty local copy was created after a transient error: %v", err)
	}
}

func TestMonoNode_EnsureLocalCopyNotFoundCreatesEmptyFile(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	if _, err := sessionMgr.StartSession(); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	client := &pathMockClient{
		readFunc: func(context.Context, string, int64, int64) ([]byte, error) {
			return nil, status.Error(codes.NotFound, "missing")
		},
	}
	node := &MonoNode{
		path:       "repo/new.txt",
		client:     client,
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}

	if err := node.ensureLocalCopy(context.Background()); err != nil {
		t.Fatalf("ensureLocalCopy: %v", err)
	}
	localPath, err := sessionMgr.GetLocalPath("repo/new.txt")
	if err != nil {
		t.Fatalf("GetLocalPath: %v", err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("empty local file missing: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("local file size = %d, want 0", info.Size())
	}
}

func TestMonoNode_PanicReturnsEIO(t *testing.T) {
	client := &pathMockClient{
		readFunc: func(context.Context, string, int64, int64) ([]byte, error) {
			panic("boom")
		},
	}
	node := &MonoNode{path: "repo/file.txt", client: client, logger: testLogger()}

	if _, _, errno := node.Open(context.Background(), syscall.O_RDONLY); errno != syscall.EIO {
		t.Fatalf("Open after panic errno = %v, want EIO", errno)
	}
	node.catErrorMu.RLock()
	recorded := node.catastrophicError
	node.catErrorMu.RUnlock()
	if recorded == "" {
		t.Fatal("panic was not recorded as a catastrophic error")
	}
}

// Compile-time assertion that MonoNode still satisfies the opener interface.
var _ fs.NodeOpener = (*MonoNode)(nil)
