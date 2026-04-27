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

// Lookup looks up a child entry in a directory.
func (n *MonoNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	defer n.recoverPanic("Lookup")
	n.client.RecordOperation()
	n.logger.Debug("lookup", "path", n.path, "name", name)

	// If looking up FS_ERROR.txt in root and we have any error, create the error file
	if name == "FS_ERROR.txt" && n.path == "" {
		if rootNode := n.getRootNode(); rootNode != nil {
			rootNode.catErrorMu.RLock()
			errorMsg := rootNode.catastrophicError
			rootNode.catErrorMu.RUnlock()

			// Also check backend error
			if errorMsg == "" && n.hasBackendError() {
				errTime, err := n.getBackendError()
				errorMsg = fmt.Sprintf("MonoFS Backend Connection Error\n\nTime: %s\nError: %s\n\nThe backend servers are unavailable. This file will disappear when the connection is restored.\n",
					errTime.Format(time.RFC3339), err.Error())
			}

			if errorMsg != "" {
				child := n.newChild(name, false, 0444|uint32(syscall.S_IFREG), uint64(len(errorMsg)))
				child.content = []byte(errorMsg)
				out.Mode = 0444 | uint32(syscall.S_IFREG)
				out.Size = uint64(len(errorMsg))
				out.SetAttrTimeout(attrTimeout())
				out.SetEntryTimeout(attrTimeout())
				return n.NewInode(ctx, child, fs.StableAttr{
					Mode: fuse.S_IFREG,
					Ino:  0xFFFFFFFF,
				}), 0
			}
		}
	}

	// If looking for error file but no error, return ENOENT
	if name == "FS_ERROR.txt" && n.path == "" {
		return nil, syscall.ENOENT
	}

	childPath := name
	if n.path != "" {
		childPath = n.path + "/" + name
	}

	// Check overlay — single source of truth for local changes.
	// If a file is tracked in overlay or exists on disk under a user root dir,
	// we use it directly and never fall through to the backend.
	if n.sessionMgr != nil {
		// DOCTOR VIRTUAL FILE INTERCEPT
		parts := splitPath(childPath)
		if len(parts) == 5 && parts[0] == "doctor" && parts[1] == "v1" && parts[2] == "query" && parts[4] == "results.json" {
			child := n.newChild(name, false, 0444|uint32(syscall.S_IFREG), 0)
			out.Mode = 0444 | uint32(syscall.S_IFREG)
			out.Size = 0 // unknown until read
			out.Ino = hashPathForNode(childPath)
			out.SetAttrTimeout(attrTimeout())
			out.SetEntryTimeout(attrTimeout())
			return n.NewInode(ctx, child, fs.StableAttr{
				Mode: fuse.S_IFREG,
				Ino:  out.Ino,
			}), 0
		}

		state := n.sessionMgr.GetPathState(childPath)

		// User root directory at filesystem root
		if n.path == "" && state.IsUserRootDir {
			child := n.newChild(name, true, 0755|uint32(syscall.S_IFDIR), 0)
			stable := fs.StableAttr{
				Mode: fuse.S_IFDIR,
				Ino:  hashPathForNode(name),
			}
			inode := n.NewInode(ctx, child, stable)
			out.Mode = 0755 | fuse.S_IFDIR
			out.Ino = stable.Ino
			out.Nlink = 2
			out.Uid = 1000
			out.Gid = 1000
			out.SetEntryTimeout(attrTimeout())
			out.SetAttrTimeout(attrTimeout())
			n.logger.Debug("lookup: found user root directory", "name", name)
			return inode, 0
		}

		// Deleted in overlay → ENOENT, never query backend
		if state.IsDeleted {
			n.logger.Debug("lookup: file deleted in session", "path", childPath)
			return nil, syscall.ENOENT
		}

		// Symlink in overlay
		if state.IsSymlink {
			target := state.SymlinkTarget
			child := n.newChild(name, false, 0777|uint32(syscall.S_IFLNK), uint64(len(target)))
			child.symlinkTarget = target
			stable := fs.StableAttr{
				Mode: fuse.S_IFLNK,
				Ino:  hashPathForNode(childPath),
			}
			inode := n.NewInode(ctx, child, stable)
			out.Mode = 0777 | fuse.S_IFLNK
			out.Size = uint64(len(target))
			out.Ino = stable.Ino
			out.Nlink = 1
			out.Uid = 1000
			out.Gid = 1000
			out.SetEntryTimeout(attrTimeout())
			out.SetAttrTimeout(attrTimeout())
			n.logger.Debug("lookup: found symlink", "path", childPath, "target", target)
			return inode, 0
		}

		// File/dir tracked in overlay DB → use local copy, skip backend
		if state.HasOverride {
			return n.lookupFromOverlay(ctx, name, childPath, out)
		}

		// Paths under user root dirs: backend doesn't know about these.
		// Check disk directly — authoritative for user dirs, no DB needed.
		if len(parts) > 1 && n.sessionMgr.IsUserRootDir(parts[0]) {
			inode, errno := n.lookupFromOverlay(ctx, name, childPath, out)
			if errno == 0 {
				return inode, 0
			}
			return nil, syscall.ENOENT
		}
	}

	// Try cache first if available
	if n.cache != nil {
		if attr, err := n.cache.GetAttr(childPath); err == nil {
			n.logger.Debug("lookup cache hit", "path", childPath)
			mode := attr.Mode
			if n.sessionMgr != nil && !isDependencyPath(childPath) {
				mode = addWriteBits(mode)
			}
			isDir := mode&uint32(syscall.S_IFDIR) != 0
			child := n.newChild(name, isDir, mode, attr.Size)
			out.Mode = mode
			out.Size = attr.Size
			out.Ino = attr.Ino
			out.Mtime = uint64(attr.Mtime)
			out.Atime = uint64(attr.Atime)
			out.Ctime = uint64(attr.Ctime)
			out.Uid = attr.Uid
			out.Gid = attr.Gid
			out.Nlink = attr.Nlink
			if n.sessionMgr != nil {
				out.SetEntryTimeout(backendEntryTimeout())
				out.SetAttrTimeout(backendEntryTimeout())
			} else {
				out.SetEntryTimeout(attrTimeout())
				out.SetAttrTimeout(attrTimeout())
			}
			stable := fs.StableAttr{Mode: mode, Ino: attr.Ino}
			return n.NewInode(ctx, child, stable), 0
		}
	}

	// Query backend with retry for transient failures.
	// Lookup is the most frequently called FUSE operation; a single transient
	// network hiccup should not return EIO to userspace.
	resp, err := n.client.Lookup(ctx, childPath)
	for attempt := 1; err != nil && attempt <= maxMetadataRetries; attempt++ {
		n.logger.Debug("lookup retry", "path", childPath, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, syscall.EINTR
		case <-time.After(retryDelay(attempt - 1)):
		}
		resp, err = n.client.Lookup(ctx, childPath)
	}
	if err != nil {
		n.logger.Debug("lookup failed after retries", "path", childPath, "error", err)
		n.updateBackendError(err)
		return nil, n.recordAndConvertError(err)
	}

	// Clear backend error on success
	n.updateBackendError(nil)

	if !resp.Found {
		return nil, syscall.ENOENT
	}

	// Update cache if available
	if n.cache != nil {
		_ = n.cache.PutAttr(childPath, &cache.AttrEntry{
			Ino:   resp.Ino,
			Mode:  resp.Mode,
			Size:  resp.Size,
			Mtime: resp.Mtime,
			Atime: resp.Mtime, // Use Mtime as default for Atime/Ctime
			Ctime: resp.Mtime,
			Nlink: 1,
			Uid:   1000,
			Gid:   1000,
		})
	}

	mode := resp.Mode
	if n.sessionMgr != nil && !isDependencyPath(childPath) {
		mode = addWriteBits(mode)
	}
	isDir := mode&uint32(syscall.S_IFDIR) != 0
	child := n.newChild(name, isDir, mode, resp.Size)
	out.Mode = mode
	out.Size = resp.Size
	out.Ino = resp.Ino
	out.Mtime = uint64(resp.Mtime)
	out.Atime = uint64(resp.Mtime)
	out.Ctime = uint64(resp.Mtime)
	out.Uid = 1000
	out.Gid = 1000
	out.Nlink = 1
	if isDir {
		out.Nlink = 2
	}
	if n.sessionMgr != nil {
		out.SetEntryTimeout(backendEntryTimeout())
		out.SetAttrTimeout(backendEntryTimeout())
	} else {
		out.SetEntryTimeout(attrTimeout())
		out.SetAttrTimeout(attrTimeout())
	}

	stable := fs.StableAttr{Mode: mode, Ino: resp.Ino}
	return n.NewInode(ctx, child, stable), 0
}
