package fuse

import (
	"context"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
)

// Write implements fs.NodeWriter for writing to files.
//
// The primary write path goes through the monofsFileHandle (set up by Open
// or Create). This fallback path is only hit when go-fuse dispatches a
// Write without a valid file handle (e.g. re-opened inodes).
//
// Performance notes:
//   - We cache the local file handle on the node (n.localHandle) to avoid
//     re-opening the file on every write syscall.
//   - TrackChange is NOT called per-write; the dirty flag (isLocalWrite)
//     is set and the tracking is deferred to Flush/Release.
func (n *MonoNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	defer n.recoverPanic("Write")
	n.logger.Debug("write", "path", n.path, "offset", off, "len", len(data))

	// Use file handle if available (the fast, common path)
	if gfh, ok := fh.(*monofsFileHandle); ok {
		return gfh.Write(ctx, data, off)
	}

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("write: no session manager, read-only mode")
		return 0, syscall.EROFS
	}

	// Ensure local copy exists (short-circuits if already on disk)
	if err := n.ensureLocalCopy(ctx); err != nil {
		n.logger.Error("write: failed to create local copy", "error", err)
		return 0, syscall.EIO
	}

	// Reuse the cached local file handle when possible. Opening the file
	// once and keeping it open across writes avoids an open+close syscall
	// pair per write chunk (~25,000 for a 100 MB file at 4 KB writes).
	n.mu.Lock()
	f := n.localHandle
	n.mu.Unlock()

	if f == nil {
		localPath, _ := n.sessionMgr.GetLocalPath(n.path)
		var err error
		f, err = os.OpenFile(localPath, os.O_RDWR, 0644)
		if err != nil {
			n.logger.Error("write: failed to open local file", "error", err)
			return 0, syscall.EIO
		}
		n.mu.Lock()
		n.localHandle = f
		n.mu.Unlock()
	}

	// Write at offset
	written, err := f.WriteAt(data, off)
	if err != nil {
		n.logger.Error("write: write failed", "error", err)
		return 0, syscall.EIO
	}

	// Update size — single lock acquisition for all fields
	n.mu.Lock()
	newSize := uint64(off) + uint64(written)
	if newSize > n.size {
		n.size = newSize
	}
	n.isLocalWrite = true
	n.mu.Unlock()

	// NOTE: TrackChange is intentionally NOT called here. The dirty flag
	// (isLocalWrite) is set above; the actual DB write is deferred to
	// Flush/Release via the file handle path, or to the next explicit
	// TrackChange call. This avoids a NutsDB transaction + os.Lstat +
	// JSON marshal on every 4 KB write chunk.

	return uint32(written), 0
}
