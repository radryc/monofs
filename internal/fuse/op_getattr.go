package fuse

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radryc/monofs/internal/cache"
)

// Getattr returns file attributes.
func (n *MonoNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	defer n.recoverPanic("Getattr")
	n.logger.Debug("getattr", "path", n.path)

	// Special handling for FS_ERROR.txt
	if n.path == "FS_ERROR.txt" && n.hasBackendError() {
		errTime, err := n.getBackendError()
		errorMsg := fmt.Sprintf("MonoFS Backend Connection Error\n\nTime: %s\nError: %s\n\nThe backend servers are unavailable. This file will disappear when the connection is restored.\n",
			errTime.Format(time.RFC3339), err.Error())
		out.Mode = 0444 | uint32(syscall.S_IFREG)
		out.Size = uint64(len(errorMsg))
		out.Ino = 0xFFFFFFFF
		out.Nlink = 1
		out.SetTimeout(1 * time.Second)
		return 0
	}

	// For root, return directory attrs
	if n.path == "" {
		now := uint64(time.Now().Unix())
		out.Mode = 0755 | uint32(syscall.S_IFDIR)
		out.Size = 0
		out.Nlink = 2
		out.Mtime = now
		out.Atime = now
		out.Ctime = now
		out.Uid = 1000
		out.Gid = 1000
		out.SetTimeout(attrTimeout())
		return 0
	}

	// Check overlay — single source of truth for local changes.
	// If tracked in overlay, use it directly; never fall through to backend.
	if n.sessionMgr != nil {
		state := n.sessionMgr.GetPathState(n.path)

		// User root directory at filesystem root
		parts := splitPath(n.path)
		if len(parts) == 1 && state.IsUserRootDir {
			now := uint64(time.Now().Unix())
			out.Mode = 0755 | uint32(syscall.S_IFDIR)
			out.Size = 0
			out.Nlink = 2
			out.Mtime = now
			out.Atime = now
			out.Ctime = now
			out.Uid = 1000
			out.Gid = 1000
			out.Ino = hashPathForNode(n.path)
			out.SetTimeout(attrTimeout())
			n.logger.Debug("getattr: user root directory", "path", n.path)
			return 0
		}

		// Deleted in overlay → ENOENT, never query backend
		if state.IsDeleted {
			n.logger.Debug("getattr: file deleted in session", "path", n.path)
			return syscall.ENOENT
		}

		// Symlink in overlay
		if state.IsSymlink {
			now := uint64(time.Now().Unix())
			out.Mode = 0777 | uint32(syscall.S_IFLNK)
			out.Size = uint64(len(state.SymlinkTarget))
			out.Nlink = 1
			out.Mtime = now
			out.Atime = now
			out.Ctime = now
			out.Uid = 1000
			out.Gid = 1000
			out.Ino = hashPathForNode(n.path)
			out.SetTimeout(attrTimeout())
			n.logger.Debug("getattr: symlink", "path", n.path, "target", state.SymlinkTarget)
			return 0
		}

		// File/dir tracked in overlay DB → use local attrs, skip backend
		if state.HasOverride {
			return n.getattrFromOverlay(out)
		}

		// Paths under user root dirs: only overlay matters
		if len(parts) > 1 && n.sessionMgr.IsUserRootDir(parts[0]) {
			errno := n.getattrFromOverlay(out)
			if errno == 0 {
				return 0
			}
			n.logger.Debug("getattr: file not found under user root dir", "path", n.path)
			return syscall.ENOENT
		}
	}

	// Try cache first if available
	if n.cache != nil {
		if attr, err := n.cache.GetAttr(n.path); err == nil {
			n.logger.Debug("getattr cache hit", "path", n.path)
			mode := attr.Mode
			if n.sessionMgr != nil && !isDependencyPath(n.path) {
				mode = addWriteBits(mode)
			}
			out.Mode = mode
			out.Size = attr.Size
			out.Ino = attr.Ino
			out.Mtime = uint64(attr.Mtime)
			out.Atime = uint64(attr.Atime)
			out.Ctime = uint64(attr.Ctime)
			out.Nlink = attr.Nlink
			out.Uid = attr.Uid
			out.Gid = attr.Gid
			if n.sessionMgr != nil {
				out.SetTimeout(backendEntryTimeout())
			} else {
				out.SetTimeout(attrTimeout())
			}
			return 0
		}
	}

	// Query backend with retry for transient failures.
	// Getattr is called frequently by the kernel to revalidate cached attrs;
	// a single transient failure should not surface as EIO.
	resp, err := n.client.GetAttr(ctx, n.path)
	for attempt := 1; err != nil && attempt <= maxMetadataRetries; attempt++ {
		n.logger.Debug("getattr retry", "path", n.path, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return syscall.EINTR
		case <-time.After(retryDelay(attempt - 1)):
		}
		resp, err = n.client.GetAttr(ctx, n.path)
	}
	if err != nil {
		n.logger.Debug("getattr failed after retries", "path", n.path, "error", err)
		n.updateBackendError(err)
		return n.recordAndConvertError(err)
	}

	// Clear backend error on success
	n.updateBackendError(nil)

	if !resp.Found {
		return syscall.ENOENT
	}

	// Update cache if available
	if n.cache != nil {
		_ = n.cache.PutAttr(n.path, &cache.AttrEntry{
			Ino:   resp.Ino,
			Mode:  resp.Mode,
			Size:  resp.Size,
			Mtime: resp.Mtime,
			Atime: resp.Atime,
			Ctime: resp.Ctime,
			Nlink: resp.Nlink,
			Uid:   resp.Uid,
			Gid:   resp.Gid,
		})
	}

	mode := resp.Mode
	if n.sessionMgr != nil && !isDependencyPath(n.path) {
		mode = addWriteBits(mode)
	}
	out.Mode = mode
	out.Size = resp.Size
	out.Ino = resp.Ino
	out.Mtime = uint64(resp.Mtime)
	out.Atime = uint64(resp.Atime)
	out.Ctime = uint64(resp.Ctime)
	out.Nlink = resp.Nlink
	out.Uid = resp.Uid
	out.Gid = resp.Gid
	if n.sessionMgr != nil {
		out.SetTimeout(backendEntryTimeout())
	} else {
		out.SetTimeout(attrTimeout())
	}
	return 0
}
