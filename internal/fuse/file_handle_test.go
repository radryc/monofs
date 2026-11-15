package fuse

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func newFileHandleTestNode(t *testing.T, path, content string) (*MonoNode, *SessionManager, string) {
	t.Helper()
	sessionMgr, err := NewSessionManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	if _, err := sessionMgr.StartSession(); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	localPath, err := sessionMgr.GetLocalPath(path)
	if err != nil {
		t.Fatalf("GetLocalPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	node := &MonoNode{
		path:       path,
		mode:       0644 | uint32(syscall.S_IFREG),
		size:       uint64(len(content)),
		sessionMgr: sessionMgr,
		logger:     testLogger(),
	}
	return node, sessionMgr, localPath
}

func openHandle(t *testing.T, node *MonoNode, localPath string) *monofsFileHandle {
	t.Helper()
	file, err := os.OpenFile(localPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	return &monofsFileHandle{file: file, node: node, logger: testLogger()}
}

func TestMonofsFileHandle_Read(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abcdefgh")
	handle := openHandle(t, node, localPath)
	defer handle.Release(context.Background())

	result, errno := handle.Read(context.Background(), make([]byte, 4), 2)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	got, _ := result.Bytes(nil)
	if string(got) != "cdef" {
		t.Fatalf("Read = %q, want cdef", got)
	}

	// Reading exactly at EOF and past EOF returns an empty result.
	for _, off := range []int64{8, 20} {
		result, errno = handle.Read(context.Background(), make([]byte, 4), off)
		if errno != 0 {
			t.Fatalf("Read at %d errno = %v", off, errno)
		}
		if result.Size() != 0 {
			t.Fatalf("Read at %d returned %d bytes", off, result.Size())
		}
	}
}

func TestMonofsFileHandle_ReadClosedFile(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	handle := openHandle(t, node, localPath)
	if err := handle.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, errno := handle.Read(context.Background(), make([]byte, 4), 0); errno != syscall.EIO {
		t.Fatalf("Read errno = %v, want EIO", errno)
	}
}

func TestMonofsFileHandle_Write(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	handle := openHandle(t, node, localPath)
	defer handle.Release(context.Background())

	n, errno := handle.Write(context.Background(), []byte("XY"), 3)
	if errno != 0 || n != 2 {
		t.Fatalf("Write = %d, errno = %v", n, errno)
	}
	if node.size != 5 {
		t.Fatalf("node size = %d, want 5", node.size)
	}
	if !node.isLocalWrite {
		t.Fatal("isLocalWrite was not set")
	}

	// A shorter overwrite must not shrink the tracked size.
	if _, errno := handle.Write(context.Background(), []byte("Z"), 0); errno != 0 {
		t.Fatalf("Write errno = %v", errno)
	}
	if node.size != 5 {
		t.Fatalf("node size shrank to %d", node.size)
	}

	content, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "ZbcXY" {
		t.Fatalf("content = %q", content)
	}
}

func TestMonofsFileHandle_WriteClosedFile(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	handle := openHandle(t, node, localPath)
	if err := handle.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, errno := handle.Write(context.Background(), []byte("x"), 0); errno != syscall.EIO {
		t.Fatalf("Write errno = %v, want EIO", errno)
	}
}

func TestMonofsFileHandle_FlushTracksOnce(t *testing.T) {
	node, sessionMgr, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	handle := openHandle(t, node, localPath)
	defer handle.Release(context.Background())

	if _, errno := handle.Write(context.Background(), []byte("d"), 3); errno != 0 {
		t.Fatalf("Write errno = %v", errno)
	}
	if errno := handle.Flush(context.Background()); errno != 0 {
		t.Fatalf("Flush errno = %v", errno)
	}
	if node.isLocalWrite {
		t.Fatal("dirty flag was not cleared by Flush")
	}
	changes := sessionMgr.GetChanges()
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}

	// A duplicate close/flush is a no-op while the node is clean.
	if errno := handle.Flush(context.Background()); errno != 0 {
		t.Fatalf("second Flush errno = %v", errno)
	}
	if handle.flushCount != 2 {
		t.Fatalf("flushCount = %d, want 2", handle.flushCount)
	}
	if changes := sessionMgr.GetChanges(); len(changes) != 1 {
		t.Fatalf("duplicate Flush tracked a second change: %d", len(changes))
	}
}

func TestMonofsFileHandle_FlushRestoresDirtyOnTrackFailure(t *testing.T) {
	node, sessionMgr, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	handle := openHandle(t, node, localPath)
	defer func() {
		// Restore the DB so Release's Flush does not log against a nil DB.
		handle.Release(context.Background())
	}()

	node.mu.Lock()
	node.isLocalWrite = true
	node.mu.Unlock()

	// Force TrackChangeWithMeta to fail: it requires an open overlay DB.
	sessionMgr.mu.Lock()
	db := sessionMgr.db
	sessionMgr.db = nil
	sessionMgr.mu.Unlock()

	if errno := handle.Flush(context.Background()); errno != 0 {
		t.Fatalf("Flush errno = %v", errno)
	}
	node.mu.RLock()
	dirty := node.isLocalWrite
	node.mu.RUnlock()
	if !dirty {
		t.Fatal("Flush did not restore isLocalWrite after a tracking failure")
	}

	sessionMgr.mu.Lock()
	sessionMgr.db = db
	sessionMgr.mu.Unlock()
}

func TestMonofsFileHandle_FsyncAndRelease(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	handle := openHandle(t, node, localPath)

	if errno := handle.Fsync(context.Background(), 0); errno != 0 {
		t.Fatalf("Fsync errno = %v", errno)
	}

	// The node caches this handle so the Write fallback can reuse it.
	node.mu.Lock()
	node.localHandle = handle.file
	node.mu.Unlock()

	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release errno = %v", errno)
	}
	if handle.file != nil {
		t.Fatal("Release did not clear the file")
	}
	node.mu.RLock()
	cached := node.localHandle
	node.mu.RUnlock()
	if cached != nil {
		t.Fatal("Release did not clear node.localHandle")
	}

	// Fsync on a released handle reports EIO rather than panicking.
	if errno := handle.Fsync(context.Background(), 0); errno != syscall.EIO {
		t.Fatalf("Fsync after release errno = %v, want EIO", errno)
	}

	// Second Release is safe.
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("second Release errno = %v", errno)
	}
}

func TestMonofsFileHandle_ReleaseKeepsUnrelatedCachedHandle(t *testing.T) {
	node, _, localPath := newFileHandleTestNode(t, "repo/file.txt", "abc")
	handle := openHandle(t, node, localPath)

	other, err := os.Open(localPath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	node.mu.Lock()
	node.localHandle = other
	node.mu.Unlock()

	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release errno = %v", errno)
	}
	node.mu.RLock()
	cached := node.localHandle
	node.mu.RUnlock()
	if cached != other {
		t.Fatal("Release cleared a localHandle owned by another file")
	}
}
