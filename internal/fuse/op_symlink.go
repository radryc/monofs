package fuse

import (
	"context"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Symlink implements fs.NodeSymlinker for creating symbolic links
func (n *MonoNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	defer n.recoverPanic("Symlink")
	n.logger.Debug("symlink", "parent", n.path, "name", name, "target", target)

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("symlink: no session manager, read-only mode")
		return nil, syscall.EROFS
	}

	// Root-level symlinks are not supported
	if n.path == "" {
		n.logger.Warn("symlink: cannot create symlinks at filesystem root")
		return nil, syscall.EROFS
	}

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("symlink: failed to start session", "error", err)
		return nil, syscall.EIO
	}

	newPath := n.path + "/" + name

	// Create symlink in session
	if err := n.sessionMgr.CreateSymlink(newPath, target); err != nil {
		n.logger.Error("symlink: failed to create symlink", "error", err)
		return nil, syscall.EIO
	}

	// Create child node for symlink
	child := n.newChild(name, false, 0777|uint32(syscall.S_IFLNK), uint64(len(target)))
	child.symlinkTarget = target
	child.stampNode()

	stable := fs.StableAttr{
		Mode: fuse.S_IFLNK | 0777,
		Ino:  hashPathForNode(newPath),
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

	n.logger.Info("created symlink", "path", newPath, "target", target)
	return inode, 0
}

// Readlink implements fs.NodeReadlinker for reading symbolic link targets
func (n *MonoNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	defer n.recoverPanic("Readlink")
	n.client.RecordOperation()
	n.logger.Debug("readlink", "path", n.path)

	// Check if we have the target cached in the node
	if n.symlinkTarget != "" {
		return []byte(n.symlinkTarget), 0
	}

	// Check session manager for symlink target
	if n.sessionMgr != nil {
		if target, ok := n.sessionMgr.GetSymlinkTarget(n.path); ok {
			return []byte(target), 0
		}
	}

	// Check local file system in overlay
	if n.sessionMgr != nil {
		localPath, err := n.sessionMgr.GetLocalPath(n.path)
		if err == nil {
			target, err := os.Readlink(localPath)
			if err == nil {
				return []byte(target), 0
			}
		}
	}

	n.logger.Warn("readlink: not a symlink or target not found", "path", n.path)
	return nil, syscall.EINVAL
}
