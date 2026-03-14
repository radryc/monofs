package fuse

import (
	"context"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Mkdir implements fs.NodeMkdirer for creating directories
func (n *MonoNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	defer n.recoverPanic("Mkdir")
	n.logger.Debug("mkdir", "parent", n.path, "name", name, "mode", mode)

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("mkdir: no session manager, read-only mode")
		return nil, syscall.EROFS
	}

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("mkdir: failed to start session", "error", err)
		return nil, syscall.EIO
	}

	// Handle root-level user directory creation
	if n.path == "" {
		if err := n.sessionMgr.CreateUserRootDir(name); err != nil {
			n.logger.Error("mkdir: failed to create user root dir", "error", err)
			return nil, syscall.EIO
		}

		child := n.newChild(name, true, mode|uint32(syscall.S_IFDIR), 0)
		child.stampNode()
		stable := fs.StableAttr{
			Mode: fuse.S_IFDIR | mode,
			Ino:  hashPathForNode(name),
		}
		inode := n.NewInode(ctx, child, stable)

		out.Mode = mode | fuse.S_IFDIR
		out.Ino = stable.Ino
		out.Nlink = 2
		out.Uid = 1000
		out.Gid = 1000
		out.SetEntryTimeout(attrTimeout())
		out.SetAttrTimeout(attrTimeout())

		n.logger.Info("created user root directory", "name", name)
		return inode, 0
	}

	newPath := n.path + "/" + name

	localPath, err := n.sessionMgr.GetLocalPath(newPath)
	if err != nil {
		n.logger.Error("mkdir: failed to get local path", "error", err)
		return nil, syscall.EIO
	}

	// Create the directory
	if err := os.MkdirAll(localPath, os.FileMode(mode)); err != nil {
		n.logger.Error("mkdir: failed", "error", err)
		return nil, syscall.EIO
	}

	// Track the creation
	if err := n.sessionMgr.TrackChange(ChangeMkdir, newPath, ""); err != nil {
		n.logger.Warn("mkdir: failed to track change", "error", err)
	}

	// Create child node
	child := n.newChild(name, true, mode|uint32(syscall.S_IFDIR), 0)
	child.stampNode()

	stable := fs.StableAttr{
		Mode: fuse.S_IFDIR | mode,
		Ino:  hashPathForNode(newPath),
	}

	inode := n.NewInode(ctx, child, stable)

	out.Mode = mode | fuse.S_IFDIR
	out.Ino = stable.Ino
	out.Nlink = 2
	out.Uid = 1000
	out.Gid = 1000
	out.SetEntryTimeout(attrTimeout())
	out.SetAttrTimeout(attrTimeout())

	n.logger.Info("created directory", "path", newPath)

	return inode, 0
}
