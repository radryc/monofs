package fuse

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// monofsFileHandle wraps an os.File for FUSE write operations
type monofsFileHandle struct {
	file       *os.File
	node       *MonoNode
	logger     *slog.Logger
	flushCount int // Counter to detect duplicate flushes
}

// Ensure monofsFileHandle implements required interfaces
var (
	_ fs.FileReader   = (*monofsFileHandle)(nil)
	_ fs.FileWriter   = (*monofsFileHandle)(nil)
	_ fs.FileFlusher  = (*monofsFileHandle)(nil)
	_ fs.FileFsyncer  = (*monofsFileHandle)(nil)
	_ fs.FileReleaser = (*monofsFileHandle)(nil)
)

// Read implements fs.FileReader
func (h *monofsFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.logger.Debug("handle read", "offset", off, "len", len(dest))

	n, err := h.file.ReadAt(dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		h.logger.Error("handle read failed", "error", err)
		return nil, syscall.EIO
	}
	h.node.client.RecordOperation()
	h.node.client.RecordBytesRead(int64(n))
	return fuse.ReadResultData(dest[:n]), 0
}

// Write implements fs.FileWriter
func (h *monofsFileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.logger.Debug("handle write", "offset", off, "len", len(data))

	n, err := h.file.WriteAt(data, off)
	if err != nil {
		h.logger.Error("handle write failed", "error", err)
		return 0, syscall.EIO
	}

	h.node.client.RecordOperation()

	// Update node size if we extended the file
	h.node.mu.Lock()
	// Mark node as having local writes - must be set for Flush to track changes
	h.node.isLocalWrite = true
	newSize := uint64(off) + uint64(n)
	if newSize > h.node.size {
		h.node.size = newSize
	}
	h.node.mu.Unlock()

	return uint32(n), 0
}

// Flush implements fs.FileFlusher.
// Called on every close() of a file descriptor (including duplicates).
// We do NOT fsync here — that is deferred to explicit Fsync calls.
// Flushing the overlay tracking is lightweight (single DB put when dirty).
func (h *monofsFileHandle) Flush(ctx context.Context) syscall.Errno {
	h.logger.Debug("handle flush")

	// Increment flush counter for duplicate flush detection
	h.flushCount++

	// Track changes in overlay DB only once per dirty file handle.
	// Use TrackChangeWithMeta to pass the known size and avoid an os.Lstat.
	if h.node.sessionMgr != nil && h.node.isLocalWrite {
		h.node.mu.Lock()
		sz := int64(h.node.size)
		// Reset the dirty flag BEFORE calling TrackChange to prevent
		// redundant tracking if Flush is called again (e.g., multiple closes
		// or concurrent operations). This fixes the "bad hash injection" issue
		// where the same file modification was tracked multiple times.
		h.node.isLocalWrite = false
		h.node.mu.Unlock()

		if err := h.node.sessionMgr.TrackChangeWithMeta(ChangeModify, h.node.path, "", sz); err != nil {
			h.logger.Warn("flush: failed to track change", "error", err)
			// If tracking failed, restore the dirty flag so the next Flush
			// will retry.
			h.node.mu.Lock()
			h.node.isLocalWrite = true
			h.node.mu.Unlock()
		}
	}
	return 0
}

// Fsync implements fs.FileFsyncer.
// Called only when the application explicitly requests fsync/fdatasync.
// This is the correct place for the expensive disk sync — not Flush.
func (h *monofsFileHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	h.logger.Debug("handle fsync")

	if err := h.file.Sync(); err != nil {
		h.logger.Error("handle fsync failed", "error", err)
		return syscall.EIO
	}
	return 0
}

// Release implements fs.FileReleaser.
// FUSE Release cannot return errors to the kernel, but failed closes can
// indicate lost writes. Log warnings so operators can detect data-loss
// scenarios in monitoring.
func (h *monofsFileHandle) Release(ctx context.Context) syscall.Errno {
	h.logger.Debug("handle release")

	if h.file != nil {
		if err := h.file.Close(); err != nil {
			h.logger.Warn("handle release: close failed (possible data loss)",
				"error", err)
		}
		h.file = nil
	}
	return 0
}
