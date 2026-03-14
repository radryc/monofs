package fuse

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// Statfs returns filesystem statistics for df command
func (n *MonoNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	n.logger.Debug("statfs", "path", n.path)

	// Return filesystem statistics
	const blockSize = 4096
	const totalBlocks = 1024 * 1024 * 1024 // 4TB total
	const freeBlocks = 512 * 1024 * 1024   // 2TB free

	out.Blocks = totalBlocks
	out.Bfree = freeBlocks
	out.Bavail = freeBlocks
	out.Files = 1000000 // Max files
	out.Ffree = 500000  // Free inodes
	out.Bsize = blockSize
	out.NameLen = 255
	out.Frsize = blockSize

	return 0
}
