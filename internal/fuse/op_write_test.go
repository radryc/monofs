package fuse

import (
	"context"
	"os"
	"syscall"
	"testing"
)

func TestMonoNode_Write_NoSessionReturnsEROFS(t *testing.T) {
	node := &MonoNode{path: "repo/file.txt", logger: testLogger()}
	if _, errno := node.Write(context.Background(), nil, []byte("x"), 0); errno != syscall.EROFS {
		t.Fatalf("Write errno = %v, want EROFS", errno)
	}
}

func TestMonoNode_Write_ReadOnlyWorkspaceReturnsEROFS(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	node := &MonoNode{path: ".monofs/system/config.json", sessionMgr: sessionMgr, logger: testLogger()}
	if _, errno := node.Write(context.Background(), nil, []byte("x"), 0); errno != syscall.EROFS {
		t.Fatalf("Write errno = %v, want EROFS", errno)
	}
}

func TestMonoNode_Write_FallbackWritesAndTracks(t *testing.T) {
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	client := &pathMockClient{
		readFunc: func(context.Context, string, int64, int64) ([]byte, error) {
			return []byte("old content"), nil
		},
	}
	node := &MonoNode{
		path:       "repo/file.txt",
		mode:       0644 | uint32(syscall.S_IFREG),
		size:       11,
		client:     client,
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}

	if n, errno := node.Write(context.Background(), nil, []byte("NEW"), 0); errno != 0 || n != 3 {
		t.Fatalf("Write = %d, errno = %v", n, errno)
	}
	if n, errno := node.Write(context.Background(), nil, []byte("!"), 11); errno != 0 || n != 1 {
		t.Fatalf("second Write = %d, errno = %v", n, errno)
	}

	if !node.isLocalWrite {
		t.Fatal("isLocalWrite was not set")
	}
	if node.size != 12 {
		t.Fatalf("node size = %d, want 12", node.size)
	}
	if node.localHandle == nil {
		t.Fatal("fallback did not cache a local handle")
	}
	firstHandle := node.localHandle
	if _, errno := node.Write(context.Background(), nil, []byte("?"), 12); errno != 0 {
		t.Fatalf("third Write errno = %v", errno)
	}
	if node.localHandle != firstHandle {
		t.Fatal("fallback reopened the local file instead of reusing the cached handle")
	}

	localPath, err := sessionMgr.GetLocalPath("repo/file.txt")
	if err != nil {
		t.Fatalf("GetLocalPath: %v", err)
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "NEW content!?" {
		t.Fatalf("content = %q", content)
	}

	// The fallback path has no file handle to flush, so it must track the
	// change itself.
	changes := sessionMgr.GetChanges()
	if len(changes) == 0 {
		t.Fatal("fallback write did not track the change")
	}
	if changes[0].Path != "repo/file.txt" {
		t.Fatalf("tracked path = %q", changes[0].Path)
	}
}

func TestMonoNode_Write_ClosedCachedHandleReturnsEIO(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	closed, err := os.Open(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	node.localHandle = closed

	if _, errno := node.Write(context.Background(), nil, []byte("x"), 0); errno != syscall.EIO {
		t.Fatalf("Write errno = %v, want EIO", errno)
	}
}

func TestMonoNode_Write_DelegatesToFileHandle(t *testing.T) {
	node, sessionMgr, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	handle := openHandle(t, node, localPath)
	defer handle.Release(context.Background())

	node.mu.Lock()
	node.localHandle = nil
	node.mu.Unlock()

	n, errno := node.Write(context.Background(), handle, []byte("Z"), 0)
	if errno != 0 || n != 1 {
		t.Fatalf("Write = %d, errno = %v", n, errno)
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "Zbc" {
		t.Fatalf("content = %q", content)
	}
	node.mu.RLock()
	cached := node.localHandle
	node.mu.RUnlock()
	if cached != nil {
		t.Fatal("handle delegation unexpectedly set node.localHandle")
	}
	// The handle keeps the session alive for the tracked change on Release.
	if sessionMgr.current == nil {
		t.Fatal("session disappeared")
	}
}
