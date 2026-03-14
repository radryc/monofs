package fuse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
)

// Rename implements fs.NodeRenamer for moving/renaming files
func (n *MonoNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	defer n.recoverPanic("Rename")
	n.logger.Debug("rename", "oldParent", n.path, "oldName", name, "newName", newName)

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("rename: no session manager, read-only mode")
		return syscall.EROFS
	}

	// Root-level rename is not supported - must be within a repository
	if n.path == "" {
		n.logger.Warn("rename: cannot rename at filesystem root")
		return syscall.EROFS
	}

	// Get new parent path and check it's not root
	var newParentPath string
	if gn, ok := newParent.(*MonoNode); ok {
		newParentPath = gn.path
		if newParentPath == "" {
			n.logger.Warn("rename: cannot move to filesystem root")
			return syscall.EROFS
		}
	}

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("rename: failed to start session", "error", err)
		return syscall.EIO
	}

	oldPath := n.path + "/" + name

	newPath := newParentPath + "/" + newName

	// Ensure local copy exists for the source
	if err := n.ensureLocalCopyFor(ctx, oldPath); err != nil {
		n.logger.Error("rename: failed to copy source", "error", err)
		return syscall.EIO
	}

	oldLocalPath, _ := n.sessionMgr.GetLocalPath(oldPath)
	newLocalPath, _ := n.sessionMgr.GetLocalPath(newPath)

	// Create parent directories for destination
	if err := os.MkdirAll(filepath.Dir(newLocalPath), 0755); err != nil {
		n.logger.Error("rename: mkdir for dest failed", "error", err)
		return syscall.EIO
	}

	// Rename locally
	if err := os.Rename(oldLocalPath, newLocalPath); err != nil {
		n.logger.Error("rename: local rename failed", "error", err)
		return syscall.EIO
	}

	// Re-key all overlay DB entries under the old path so they sit under
	// the new path. This keeps the overlay in sync with the physical file
	// layout on disk (which was moved by os.Rename above). Without this,
	// operations like collectBlobFiles would find stale local paths that
	// no longer exist, causing files to be skipped during ingestion.
	if n.sessionMgr != nil {
		n.sessionMgr.RenameChildren(oldPath, newPath)
	}

	// Update the child node's path and all descendants BEFORE tracking
	// changes. After Rename returns, the kernel reuses existing inodes at
	// the new location. If MonoNode.path is stale, subsequent Open/Getattr
	// /Read calls use the wrong path and fail.
	if childInode := n.GetChild(name); childInode != nil {
		if childNode, ok := childInode.Operations().(*MonoNode); ok {
			childNode.mu.Lock()
			childNode.path = newPath
			childNode.mu.Unlock()
			childNode.stampNode()
		}
		// Recursively update all descendant MonoNode paths.
		updateDescendantPaths(childInode, oldPath, newPath)
	}

	// Track as delete + create
	n.sessionMgr.TrackChange(ChangeDelete, oldPath, "")
	n.sessionMgr.TrackChange(ChangeCreate, newPath, "")

	// Do NOT call invalidateEntry / NotifyEntry here.
	//
	// The kernel sends RENAME while holding a dentry lock on the destination
	// name (newName) until it receives our reply.  Calling NotifyEntry for
	// newName from inside the handler sends NOTIFY_INVAL_ENTRY back to the
	// kernel, which tries to lock that same dentry → deadlock / hang.
	//
	// The kernel's RENAME opcode already atomically updates its own dentry
	// cache for both the source and destination names, so explicit
	// invalidation from inside the handler is unnecessary.
	//
	// Calling RmChild(name) here also interferes with go-fuse's own
	// post-Rename moveNode housekeeping that runs after we return, causing
	// the renamed inode to be lost from the inode tree.

	n.logger.Info("renamed", "from", oldPath, "to", newPath)

	return 0
}

// updateDescendantPaths recursively walks the go-fuse inode tree below
// `inode` and rewrites every MonoNode.path from oldPrefix to newPrefix.
// This is called during Rename so that child inodes (which go-fuse moves
// automatically in the inode tree) have a consistent MonoNode.path.
func updateDescendantPaths(inode *fs.Inode, oldPrefix, newPrefix string) {
	for childName, child := range inode.Children() {
		_ = childName // name is already correct in the inode tree
		if mn, ok := child.Operations().(*MonoNode); ok {
			mn.mu.Lock()
			if mn.path == oldPrefix || strings.HasPrefix(mn.path, oldPrefix+"/") {
				mn.path = newPrefix + mn.path[len(oldPrefix):]
			}
			mn.mu.Unlock()
		}
		// Recurse into child directories.
		updateDescendantPaths(child, oldPrefix, newPrefix)
	}
}
