package fuse

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Statfs returns filesystem statistics for df command
func (n *MonoNode) Statfs(ctx context.Context, out *fuse.StatfsOut) (errno syscall.Errno) {
	defer n.recoverPanic("Statfs")

	n.logger.Debug("statfs", "path", n.path)

	if n.client == nil {
		return syscall.EIO
	}

	snapshot, err := n.client.StatFS(ctx)
	if err != nil {
		return n.recordAndConvertError(err)
	}

	out.Blocks = snapshot.Blocks
	out.Bfree = snapshot.Bfree
	out.Bavail = snapshot.Bavail
	out.Files = snapshot.Files
	out.Ffree = snapshot.Ffree
	out.Bsize = snapshot.Bsize
	out.NameLen = snapshot.NameLen
	out.Frsize = snapshot.Frsize

	return fs.OK
}
