package fuse

import (
	"context"
	"os"
	"syscall"
)

// Rmdir implements fs.NodeRmdirer for removing directories
func (n *MonoNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	defer n.recoverPanic("Rmdir")
	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		return syscall.EROFS
	}

	// Ensure session exists (fast path - session usually already exists)
	if !n.sessionMgr.HasActiveSession() {
		if _, err := n.sessionMgr.StartSession(); err != nil {
			n.logger.Error("rmdir: failed to start session", "error", err)
			return syscall.EIO
		}
	}

	// Handle root-level directory removal
	if n.path == "" {
		// Only allow removal of user-created directories, not repositories
		if !n.sessionMgr.IsUserRootDir(name) {
			return syscall.EROFS
		}

		if err := n.sessionMgr.RemoveUserRootDir(name); err != nil {
			n.logger.Error("rmdir: failed to remove user root dir", "error", err)
			return syscall.EIO
		}

		return 0
	}

	targetPath := n.path + "/" + name

	// If there's a local copy, remove it
	if n.sessionMgr.HasLocalOverride(targetPath) {
		if localPath, err := n.sessionMgr.GetLocalPath(targetPath); err == nil {
			if err := os.RemoveAll(localPath); err != nil && !os.IsNotExist(err) {
				n.logger.Error("rmdir: remove local failed", "error", err)
				return syscall.EIO
			}
		}
	}

	// Track the deletion
	if err := n.sessionMgr.TrackChange(ChangeRmdir, targetPath, ""); err != nil {
		n.logger.Warn("rmdir: failed to track change", "error", err)
	}

	n.logger.Debug("rmdir: removed", "path", targetPath)
	return 0
}
