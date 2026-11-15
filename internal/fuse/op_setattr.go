package fuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Setattr implements fs.NodeSetattrer for truncate/chmod operations
func (n *MonoNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) (resultErrno syscall.Errno) {
	defer n.recoverPanic("Setattr", &resultErrno)
	n.logger.Debug("setattr", "path", n.path, "valid", in.Valid)

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("setattr: no session manager, read-only mode")
		return syscall.EROFS
	}
	if n.isWorkspaceReadOnlyPath() {
		n.logger.Warn("setattr: read-only workspace path", "path", n.path)
		return syscall.EROFS
	}

	// Ensure session exists for any setattr mutation
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("setattr: failed to start session", "error", err)
		return syscall.EIO
	}

	changed := false
	sizeChanged := in.Valid&fuse.FATTR_SIZE != 0
	modeChanged := in.Valid&fuse.FATTR_MODE != 0
	timeChanged := in.Valid&(fuse.FATTR_ATIME|fuse.FATTR_MTIME) != 0
	ownerChanged := in.Valid&(fuse.FATTR_UID|fuse.FATTR_GID) != 0

	if n.isDir {
		// A directory has no backend content to fetch. Never create a regular
		// file shadow for it: materialise a directory in the overlay instead.
		if sizeChanged {
			return syscall.EINVAL
		}
		if modeChanged || timeChanged || ownerChanged {
			localPath, err := n.sessionMgr.GetLocalPath(n.path)
			if err != nil {
				n.logger.Error("setattr: failed to get local path", "error", err)
				return syscall.EIO
			}
			perm := os.FileMode(in.Mode & 0777)
			if perm == 0 {
				perm = 0755
			}
			if err := os.MkdirAll(localPath, perm); err != nil {
				n.logger.Error("setattr: mkdir failed", "error", err)
				return syscall.EIO
			}
			if modeChanged {
				if err := os.Chmod(localPath, perm); err != nil {
					n.logger.Error("setattr: chmod failed", "error", err)
					return syscall.EIO
				}
				n.mu.Lock()
				n.mode = (n.mode &^ 0777) | (in.Mode & 0777)
				n.mu.Unlock()
				changed = true
			}
		}
	} else {
		needLocalCopy := sizeChanged || modeChanged || timeChanged || ownerChanged

		// Ensure local copy once for all mutations in this Setattr call.
		// Special case: truncate-to-zero doesn't need the backend content —
		// just create an empty file. This avoids a full backend fetch followed
		// by immediate truncation (common with O_TRUNC opens).
		if needLocalCopy {
			truncToZero := sizeChanged && in.Size == 0

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
				perm := os.FileMode(n.mode & 0777)
				if perm == 0 {
					perm = 0644
				}
				if err := os.WriteFile(localPath, []byte{}, perm); err != nil {
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
		if sizeChanged {
			if in.Size != 0 {
				// Non-zero truncate: file was already ensured above
				localPath, err := n.sessionMgr.GetLocalPath(n.path)
				if err != nil {
					n.logger.Error("setattr: failed to get local path", "error", err)
					return syscall.EIO
				}
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

		// Handle mode change. Preserve the file type bits: in.Mode carries only
		// the permission bits, but out.Mode must describe a regular file.
		if modeChanged {
			localPath, err := n.sessionMgr.GetLocalPath(n.path)
			if err != nil {
				n.logger.Error("setattr: failed to get local path", "error", err)
				return syscall.EIO
			}
			if err := os.Chmod(localPath, os.FileMode(in.Mode&0777)); err != nil {
				n.logger.Error("setattr: chmod failed", "error", err)
				return syscall.EIO
			}
			n.mu.Lock()
			n.mode = (n.mode &^ 0777) | (in.Mode & 0777)
			n.mu.Unlock()
			changed = true
		}
	}

	// Apply timestamps and ownership after the content/mode changes, once a
	// local inode exists.
	now := time.Now()
	atime, hasATime := in.GetATime()
	mtime, hasMTime := in.GetMTime()
	if !hasATime {
		atime = now
	}
	if !hasMTime {
		mtime = now
	}
	if timeChanged || ownerChanged {
		localPath, err := n.sessionMgr.GetLocalPath(n.path)
		if err != nil {
			n.logger.Error("setattr: failed to get local path", "error", err)
			return syscall.EIO
		}
		if timeChanged {
			if err := os.Chtimes(localPath, atime, mtime); err != nil {
				n.logger.Error("setattr: utimes failed", "error", err)
				return syscall.EIO
			}
			changed = true
		}
		if ownerChanged {
			uid, gid := -1, -1
			if value, ok := in.GetUID(); ok {
				uid = int(value)
			}
			if value, ok := in.GetGID(); ok {
				gid = int(value)
			}
			if err := os.Chown(localPath, uid, gid); err != nil {
				n.logger.Error("setattr: chown failed", "error", err)
				if errors.Is(err, os.ErrPermission) {
					return syscall.EPERM
				}
				return syscall.EIO
			}
			changed = true
		}
	}

	// Track change in DB if anything was modified — use known size to
	// skip the os.Lstat syscall.
	if changed {
		n.stampNode()
		n.mu.RLock()
		sz := int64(n.size)
		n.mu.RUnlock()
		var trackErr error
		for attempt := 0; attempt < 2; attempt++ {
			if trackErr = n.sessionMgr.TrackChangeWithMeta(ChangeModify, n.path, "", sz); trackErr == nil {
				break
			}
		}
		if trackErr != nil {
			// The mutation was applied but cannot be committed; report failure
			// so the caller retries instead of silently losing the change.
			n.logger.Error("setattr: failed to track change", "error", trackErr)
			return syscall.EIO
		}
	}

	// Fill out attributes from local state — avoids a backend round-trip
	// that Getattr would trigger.
	n.mu.RLock()
	mode := n.mode
	size := n.size
	n.mu.RUnlock()

	if mode&uint32(syscall.S_IFMT) == 0 {
		if n.isDir {
			mode |= uint32(syscall.S_IFDIR)
		} else {
			mode |= uint32(syscall.S_IFREG)
		}
	}

	out.Mode = mode
	out.Size = size
	out.Ino = hashPathForNode(n.path)
	out.Mtime = uint64(mtime.Unix())
	out.Atime = uint64(atime.Unix())
	out.Ctime = uint64(now.Unix())
	n.setAttrOwner(out)
	out.Nlink = 1
	out.SetTimeout(overlayEntryTimeout())
	return 0
}
