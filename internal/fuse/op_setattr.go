package fuse

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Setattr implements fs.NodeSetattrer for truncate/chmod operations
func (n *MonoNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	defer n.recoverPanic("Setattr")
	n.logger.Debug("setattr", "path", n.path, "valid", in.Valid)

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("setattr: no session manager, read-only mode")
		return syscall.EROFS
	}

	// Ensure session exists for any setattr mutation
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("setattr: failed to start session", "error", err)
		return syscall.EIO
	}

	changed := false
	needLocalCopy := (in.Valid&fuse.FATTR_SIZE != 0) || (in.Valid&fuse.FATTR_MODE != 0)

	// Ensure local copy once for all mutations in this Setattr call.
	// Special case: truncate-to-zero doesn't need the backend content —
	// just create an empty file. This avoids a full backend fetch followed
	// by immediate truncation (common with O_TRUNC opens).
	if needLocalCopy {
		truncToZero := (in.Valid&fuse.FATTR_SIZE != 0) && in.Size == 0

		if truncToZero {
			// Fast path: create an empty local file without fetching backend
			localPath, err := n.sessionMgr.GetLocalPath(n.path)
			if err != nil {
				n.logger.Error("setattr: failed to get local path", "error", err)
				return syscall.EIO
			}
			if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
				n.logger.Error("setattr: mkdir failed", "error", err)
				return syscall.EIO
			}
			if err := os.WriteFile(localPath, []byte{}, 0644); err != nil {
				n.logger.Error("setattr: create empty file failed", "error", err)
				return syscall.EIO
			}
		} else {
			if err := n.ensureLocalCopy(ctx); err != nil {
				n.logger.Error("setattr: failed to create local copy", "error", err)
				return syscall.EIO
			}
		}
	}

	// Handle truncate
	if in.Valid&fuse.FATTR_SIZE != 0 {
		if in.Size != 0 {
			// Non-zero truncate: file was already ensured above
			localPath, _ := n.sessionMgr.GetLocalPath(n.path)
			if err := os.Truncate(localPath, int64(in.Size)); err != nil {
				n.logger.Error("setattr: truncate failed", "error", err)
				return syscall.EIO
			}
		}

		n.mu.Lock()
		n.size = in.Size
		n.isLocalWrite = true
		n.mu.Unlock()
		changed = true
	}

	// Handle mode change
	if in.Valid&fuse.FATTR_MODE != 0 {
		localPath, _ := n.sessionMgr.GetLocalPath(n.path)
		if err := os.Chmod(localPath, os.FileMode(in.Mode)); err != nil {
			n.logger.Error("setattr: chmod failed", "error", err)
			return syscall.EIO
		}
		n.mu.Lock()
		n.mode = in.Mode
		n.mu.Unlock()
		changed = true
	}

	// Track change in DB if anything was modified — use known size to
	// skip the os.Lstat syscall.
	if changed {
		n.stampNode()
		n.mu.RLock()
		sz := int64(n.size)
		n.mu.RUnlock()
		if err := n.sessionMgr.TrackChangeWithMeta(ChangeModify, n.path, "", sz); err != nil {
			n.logger.Warn("setattr: failed to track change", "error", err)
		}
	}

	// Fill out attributes from local state — avoids a backend round-trip
	// that Getattr would trigger.
	n.mu.RLock()
	mode := n.mode
	size := n.size
	n.mu.RUnlock()

	out.Mode = mode
	out.Size = size
	out.Ino = hashPathForNode(n.path)
	now := uint64(time.Now().Unix())
	out.Mtime = now
	out.Atime = now
	out.Ctime = now
	out.Uid = 1000
	out.Gid = 1000
	out.Nlink = 1
	out.SetTimeout(overlayEntryTimeout())
	return 0
}
