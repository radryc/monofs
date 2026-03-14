package fuse

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Open opens a file.
func (n *MonoNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	defer n.recoverPanic("Open")
	n.logger.Debug("open", "path", n.path, "flags", flags)

	// Special handling for FS_ERROR.txt - content is already set
	if n.path == "FS_ERROR.txt" && len(n.content) > 0 {
		return nil, fuse.FOPEN_KEEP_CACHE, 0
	}

	// Check if this is a write/create operation.
	// O_CREAT on existing files comes through Open (not Create),
	// so it must be treated as a mutating operation.
	isWrite := flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_TRUNC|syscall.O_CREAT) != 0

	if isWrite {
		// Handle write mode
		if n.sessionMgr == nil {
			n.logger.Warn("open: write requested but no session manager")
			return nil, 0, syscall.EROFS
		}

		// Ensure session exists
		if _, err := n.sessionMgr.StartSession(); err != nil {
			n.logger.Error("open: failed to start session", "error", err)
			return nil, 0, syscall.EIO
		}

		// Create local copy for writing
		if err := n.ensureLocalCopy(ctx); err != nil {
			n.logger.Error("open: failed to create local copy", "error", err)
			return nil, 0, syscall.EIO
		}

		localPath, _ := n.sessionMgr.GetLocalPath(n.path)

		// Determine open flags
		openFlags := os.O_RDWR
		if flags&syscall.O_APPEND != 0 {
			openFlags |= os.O_APPEND
		}
		if flags&syscall.O_TRUNC != 0 {
			openFlags |= os.O_TRUNC
		}

		f, err := os.OpenFile(localPath, openFlags, 0644)
		if err != nil {
			n.logger.Error("open: failed to open local file", "error", err)
			return nil, 0, syscall.EIO
		}

		n.mu.Lock()
		n.isLocalWrite = true
		n.localHandle = f
		n.mu.Unlock()
		n.stampNode()

		fh := &monofsFileHandle{
			file:   f,
			node:   n,
			logger: n.logger,
		}

		return fh, fuse.FOPEN_DIRECT_IO, 0
	}

	// Stale-inode guard: after a Rename the kernel may still dispatch
	// operations to the old inode whose path is now marked deleted.
	// Return ENOENT immediately so callers see the file as gone.
	if n.sessionMgr != nil {
		state := n.sessionMgr.GetPathState(n.path)
		if state.IsDeleted {
			n.logger.Debug("open: path deleted in overlay", "path", n.path)
			return nil, 0, syscall.ENOENT
		}
	}

	// Read mode: check overlay — if tracked locally, serve from overlay.
	// Never fall through to backend for overlay-tracked files.
	if n.sessionMgr != nil && n.sessionMgr.HasLocalOverride(n.path) {
		localPath, _ := n.sessionMgr.GetLocalPath(n.path)

		f, err := os.OpenFile(localPath, os.O_RDONLY, 0)
		if err != nil {
			n.logger.Error("open: failed to open overlay file", "path", n.path, "error", err)
			return nil, 0, syscall.EIO
		}
		fh := &monofsFileHandle{
			file:   f,
			node:   n,
			logger: n.logger,
		}
		n.logger.Debug("open: serving from overlay", "path", n.path)
		// DIRECT_IO: kernel always asks FUSE, avoids stale page-cache
		return fh, fuse.FOPEN_DIRECT_IO, 0
	}

	// Paths under user root dirs: backend doesn't know about these.
	// Check disk directly — files may exist even if not tracked in DB yet
	// (e.g. created by os.Rename between TrackChange calls).
	if n.sessionMgr != nil {
		parts := splitPath(n.path)
		if len(parts) > 1 && n.sessionMgr.IsUserRootDir(parts[0]) {
			localPath, err := n.sessionMgr.GetLocalPath(n.path)
			if err != nil {
				n.logger.Error("open: no session for user root dir file", "path", n.path)
				return nil, 0, syscall.ENOENT
			}
			f, err := os.OpenFile(localPath, os.O_RDONLY, 0)
			if err != nil {
				n.logger.Debug("open: file under user root dir not found on disk", "path", n.path, "local", localPath)
				return nil, 0, syscall.ENOENT
			}
			fh := &monofsFileHandle{
				file:   f,
				node:   n,
				logger: n.logger,
			}
			n.logger.Debug("open: serving user-dir file from disk", "path", n.path)
			return fh, fuse.FOPEN_DIRECT_IO, 0
		}
	}

	// Read file content from backend with retry logic
	const maxRetries = 3
	var content []byte
	var err error

	for attempt := 0; attempt < maxRetries; attempt++ {
		n.logger.Debug("open calling client.Read", "path", n.path, "size", n.size, "attempt", attempt+1)
		content, err = n.client.Read(ctx, n.path, 0, 0) // 0, 0 means read entire file
		n.logger.Debug("open client.Read returned", "path", n.path, "content_len", len(content), "error", err)

		if err == nil {
			break
		}

		n.logger.Debug("open read retry", "path", n.path, "attempt", attempt+1, "error", err)

		// Wait before retry with exponential backoff + jitter
		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				n.logger.Debug("open cancelled during retry", "path", n.path)
				n.updateBackendError(ctx.Err())
				return nil, 0, syscall.EINTR
			case <-time.After(retryDelay(attempt)):
			}
		}
	}

	if err != nil {
		n.logger.Debug("open failed after retries", "path", n.path, "error", err)
		n.updateBackendError(err)
		return nil, 0, n.recordAndConvertError(err)
	}

	// Clear backend error on success
	n.updateBackendError(nil)

	// Normalise nil → empty slice so the FUSE Read handler can distinguish
	// "successfully loaded zero bytes" from "never loaded" (nil).
	// The backend returns nil for empty files (no stream chunks), but nil
	// in n.content means "not loaded" which triggers a reload loop → EIO.
	if content == nil {
		content = []byte{}
	}

	n.mu.Lock()
	n.content = content
	// Update size to match actual fetched content length.
	// Synthetic/generated files may have size=0 in metadata but real content
	// produced on-demand by the fetcher. Without this update the kernel
	// trusts the stale attr size and may not issue Read() at all.
	sizeChanged := uint64(len(content)) != n.size
	if sizeChanged {
		n.size = uint64(len(content))
	}
	n.mu.Unlock()

	n.logger.Debug("open complete", "path", n.path, "size", len(content))

	// Use DIRECT_IO when actual content size differs from the metadata size
	// reported via Getattr/Lookup. This forces the kernel to bypass its page
	// cache and issue Read() calls regardless of the previously reported size.
	// This is essential for synthetic/generated files whose metadata size is 0
	// but whose content is produced on-demand by the fetcher.
	if sizeChanged {
		return nil, fuse.FOPEN_DIRECT_IO, 0
	}

	// After a dependency push, force DIRECT_IO for dependency/ paths for a
	// short window. This prevents the kernel from serving stale page-cache
	// content that was cached from the pre-push overlay. Without this,
	// go mod verify may read stale content and compute the wrong hash.
	if isDependencyPath(n.path) && n.sessionMgr != nil && n.sessionMgr.DepsPushedRecently() {
		n.logger.Debug("open: forcing DIRECT_IO for dependency path after recent push", "path", n.path)
		return nil, fuse.FOPEN_DIRECT_IO, 0
	}

	return nil, fuse.FOPEN_KEEP_CACHE, 0
}
