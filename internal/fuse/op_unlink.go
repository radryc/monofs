package fuse

import (
	"context"
	"os"
	"syscall"
)

// Unlink implements fs.NodeUnlinker for deleting files
func (n *MonoNode) Unlink(ctx context.Context, name string) syscall.Errno {
	defer n.recoverPanic("Unlink")
	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		return syscall.EROFS
	}

	// Root-level deletion is not supported - must be within a repository
	if n.path == "" {
		return syscall.EROFS
	}

	// Ensure session exists (fast path - session usually already exists)
	if !n.sessionMgr.HasActiveSession() {
		if _, err := n.sessionMgr.StartSession(); err != nil {
			n.logger.Error("unlink: failed to start session", "error", err)
			return syscall.EIO
		}
	}

	targetPath := n.path + "/" + name

	// If there's a local copy, remove it
	if n.sessionMgr.HasLocalOverride(targetPath) {
		if localPath, err := n.sessionMgr.GetLocalPath(targetPath); err == nil {
			if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
				n.logger.Error("unlink: remove local failed", "error", err)
				return syscall.EIO
			}
		}
	}

	// Track the deletion
	if err := n.sessionMgr.TrackChange(ChangeDelete, targetPath, ""); err != nil {
		n.logger.Warn("unlink: failed to track change", "error", err)
	}

	n.logger.Debug("unlink: removed", "path", targetPath)
	return 0
}
