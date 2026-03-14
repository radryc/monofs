package fuse

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Read reads file content.
func (n *MonoNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	defer n.recoverPanic("Read")
	n.logger.Debug("read", "path", n.path, "offset", off, "len", len(dest))

	// Delegate to file handle if available (overlay files opened via Open/Create)
	if mfh, ok := f.(*monofsFileHandle); ok {
		return mfh.Read(ctx, dest, off)
	}

	// If the file is tracked in overlay, read from disk directly.
	// This handles the case where go-fuse dispatches to the node's Read
	// instead of the file handle's Read (e.g. after a re-open).
	if n.sessionMgr != nil && n.sessionMgr.HasLocalOverride(n.path) {
		localPath, err := n.sessionMgr.GetLocalPath(n.path)
		if err == nil {
			n.logger.Debug("read: serving from overlay", "path", n.path, "local", localPath)
			data, err := os.ReadFile(localPath)
			if err == nil {
				end := int(off) + len(dest)
				if end > len(data) {
					end = len(data)
				}
				if int(off) >= len(data) {
					n.client.RecordOperation()
					return fuse.ReadResultData(nil), 0
				}
				n.client.RecordOperation()
				n.client.RecordBytesRead(int64(end - int(off)))
				return fuse.ReadResultData(data[off:end]), 0
			}
			n.logger.Warn("read: overlay file read failed", "path", n.path, "error", err)
		}
	}

	n.mu.RLock()
	content := n.content
	n.mu.RUnlock()

	// If content is nil, try to reload it (handles race conditions and cleanup)
	if content == nil {
		n.logger.Debug("read: content nil, attempting reload", "path", n.path)

		// For paths under user root directories (e.g. .deps/), the backend
		// doesn't know about these files. Don't waste time routing reads
		// across the cluster — they will always fail with NotFound.
		if n.sessionMgr != nil {
			parts := splitPath(n.path)
			if len(parts) > 1 && n.sessionMgr.IsUserRootDir(parts[0]) {
				n.logger.Debug("read: content nil for user root dir path, returning EIO", "path", n.path)
				return nil, syscall.EIO
			}
		}

		// Try to reload content with retry
		const maxRetries = 3
		var err error

		for attempt := 0; attempt < maxRetries; attempt++ {
			content, err = n.client.Read(ctx, n.path, 0, 0)
			if err == nil {
				// Normalise nil → empty slice so we never store nil in
				// n.content again. The backend returns nil for zero-byte
				// files (empty stream, no error). nil in n.content means
				// "not loaded" and would re-trigger this reload loop.
				if content == nil {
					content = []byte{}
				}
				// Cache the content for future reads
				n.mu.Lock()
				n.content = content
				n.mu.Unlock()
				n.logger.Debug("read: content reloaded successfully", "path", n.path, "size", len(content))
				break
			}

			n.logger.Debug("read: reload retry", "path", n.path, "attempt", attempt+1, "error", err)

			if attempt < maxRetries-1 {
				select {
				case <-ctx.Done():
					return nil, syscall.EINTR
				case <-time.After(retryDelay(attempt)):
				}
			}
		}

		if err != nil {
			n.logger.Debug("read: reload failed after retries", "path", n.path, "error", err)
			n.updateBackendError(err)
			// Only count real errors, not context cancellations
			if err != context.Canceled && err != context.DeadlineExceeded {
				if n.client != nil {
					n.client.RecordError()
				}
			}
			return nil, syscall.EIO
		}

		// Clear backend error on success
		n.updateBackendError(nil)
	}

	end := int(off) + len(dest)
	if end > len(content) {
		end = len(content)
	}
	if int(off) >= len(content) {
		n.client.RecordOperation()
		return fuse.ReadResultData(nil), 0
	}

	bytesRead := int64(end - int(off))
	n.client.RecordOperation()
	n.client.RecordBytesRead(bytesRead)
	return fuse.ReadResultData(content[off:end]), 0
}
