package fuse

import (
	"context"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Create implements fs.NodeCreater for creating new files
func (n *MonoNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	defer n.recoverPanic("Create")
	n.logger.Debug("create", "parent", n.path, "name", name, "mode", mode)

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("create: no session manager, read-only mode")
		return nil, nil, 0, syscall.EROFS
	}

	// Root-level file creation is not allowed - only directories can be created at root
	if n.path == "" {
		n.logger.Warn("create: cannot create files at filesystem root, use mkdir first")
		return nil, nil, 0, syscall.EROFS
	}

	// Check if we're inside a user-created root directory or a repository
	parts := splitPath(n.path)
	isUserDir := len(parts) >= 1 && n.sessionMgr.IsUserRootDir(parts[0])

	// If not in user directory, we're in a repository - files must be within repository structure
	if !isUserDir {
		n.logger.Debug("create: inside repository", "path", n.path)
	}

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("create: failed to start session", "error", err)
		return nil, nil, 0, syscall.EIO
	}

	newPath := n.path + "/" + name

	localPath, err := n.sessionMgr.GetLocalPath(newPath)
	if err != nil {
		n.logger.Error("create: failed to get local path", "error", err)
		return nil, nil, 0, syscall.EIO
	}

	// Create parent directories
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		n.logger.Error("create: mkdir failed", "error", err)
		return nil, nil, 0, syscall.EIO
	}

	// Create the file
	f, err := os.OpenFile(localPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(mode))
	if err != nil {
		n.logger.Error("create: open failed", "error", err)
		return nil, nil, 0, syscall.EIO
	}

	// Track the creation
	if err := n.sessionMgr.TrackChange(ChangeCreate, newPath, ""); err != nil {
		n.logger.Warn("create: failed to track change", "error", err)
	}

	// Create child node
	child := n.newChild(name, false, mode, 0)
	child.isLocalWrite = true
	child.localHandle = f
	child.stampNode()

	stable := fs.StableAttr{
		Mode: fuse.S_IFREG | mode,
		Ino:  hashPathForNode(newPath),
	}

	inode := n.NewInode(ctx, child, stable)

	out.Mode = mode | fuse.S_IFREG
	out.Size = 0
	out.Ino = stable.Ino
	out.Nlink = 1
	out.Uid = 1000
	out.Gid = 1000
	out.SetEntryTimeout(overlayEntryTimeout())
	out.SetAttrTimeout(overlayEntryTimeout())

	fh := &monofsFileHandle{
		file:   f,
		node:   child,
		logger: n.logger,
	}

	n.logger.Info("created file", "path", newPath)

	return inode, fh, fuse.FOPEN_DIRECT_IO, 0
}
