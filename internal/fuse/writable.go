// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radryc/monofs/internal/client"
)

// LocalFileHandle wraps an os.File for FUSE write operations
type LocalFileHandle struct {
	file   *os.File
	node   *WritableNode
	logger *slog.Logger
}

// Ensure LocalFileHandle implements required interfaces
var (
	_ fs.FileReader    = (*LocalFileHandle)(nil)
	_ fs.FileWriter    = (*LocalFileHandle)(nil)
	_ fs.FileFlusher   = (*LocalFileHandle)(nil)
	_ fs.FileReleaser  = (*LocalFileHandle)(nil)
	_ fs.FileGetattrer = (*LocalFileHandle)(nil)
)

// Read implements fs.FileReader
func (h *LocalFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.logger.Debug("local read", "offset", off, "len", len(dest))

	n, err := h.file.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		h.logger.Error("local read failed", "error", err)
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

// Write implements fs.FileWriter
func (h *LocalFileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.logger.Debug("local write", "offset", off, "len", len(data))

	n, err := h.file.WriteAt(data, off)
	if err != nil {
		h.logger.Error("local write failed", "error", err)
		return 0, syscall.EIO
	}

	// Update node size if we extended the file
	h.node.mu.Lock()
	newSize := uint64(off) + uint64(n)
	if newSize > h.node.size {
		h.node.size = newSize
	}
	h.node.mu.Unlock()

	return uint32(n), 0
}

// Flush implements fs.FileFlusher
func (h *LocalFileHandle) Flush(ctx context.Context) syscall.Errno {
	h.logger.Debug("local flush")

	if err := h.file.Sync(); err != nil {
		h.logger.Error("local flush failed", "error", err)
		return syscall.EIO
	}
	return 0
}

// Release implements fs.FileReleaser
func (h *LocalFileHandle) Release(ctx context.Context) syscall.Errno {
	h.logger.Debug("local release")

	if h.file != nil {
		h.file.Close()
		h.file = nil
	}
	return 0
}

// Getattr implements fs.FileGetattrer
func (h *LocalFileHandle) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	h.logger.Debug("local getattr via handle")

	info, err := h.file.Stat()
	if err != nil {
		return syscall.EIO
	}

	out.Size = uint64(info.Size())
	out.Mode = uint32(info.Mode())
	out.Mtime = uint64(info.ModTime().Unix())
	out.Atime = out.Mtime
	out.Ctime = out.Mtime
	out.Nlink = 1
	out.Uid = 1000
	out.Gid = 1000

	return 0
}

// WritableNode extends MonoNode with write capabilities
type WritableNode struct {
	fs.Inode

	// Path relative to repository root
	path string

	// Whether this node is a directory
	isDir bool

	// File mode
	mode uint32

	// File size
	size uint64

	// Backend client for read operations
	client client.MonoFSClient

	// Session manager for write operations
	sessionMgr *SessionManager

	// Logger
	logger *slog.Logger

	// Mutex for protecting concurrent access
	mu syncRWMutex

	// Indicates if this node has local modifications
	isLocalWrite bool

	// Original blob hash before modification
	origBlobHash string
}

// syncRWMutex is a sync.RWMutex with some helper methods
type syncRWMutex struct {
	mu sync.RWMutex
}

func (m *syncRWMutex) Lock()    { m.mu.Lock() }
func (m *syncRWMutex) Unlock()  { m.mu.Unlock() }
func (m *syncRWMutex) RLock()   { m.mu.RLock() }
func (m *syncRWMutex) RUnlock() { m.mu.RUnlock() }

// Ensure WritableNode implements required interfaces
var (
	_ fs.NodeSetattrer = (*WritableNode)(nil)
	_ fs.NodeCreater   = (*WritableNode)(nil)
	_ fs.NodeMkdirer   = (*WritableNode)(nil)
	_ fs.NodeUnlinker  = (*WritableNode)(nil)
	_ fs.NodeRmdirer   = (*WritableNode)(nil)
	_ fs.NodeRenamer   = (*WritableNode)(nil)
)

// Setattr implements fs.NodeSetattrer for truncate/chmod operations
func (n *WritableNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.logger.Debug("setattr", "path", n.path)

	// Handle file handle setattr first
	if fh != nil {
		if lh, ok := fh.(*LocalFileHandle); ok {
			return n.setattrLocal(ctx, lh, in, out)
		}
	}

	// Handle truncate
	if in.Valid&fuse.FATTR_SIZE != 0 {
		if err := n.ensureLocalCopy(ctx); err != nil {
			n.logger.Error("setattr: failed to create local copy", "error", err)
			return syscall.EIO
		}

		localPath, _ := n.sessionMgr.GetLocalPath(n.path)
		if err := os.Truncate(localPath, int64(in.Size)); err != nil {
			n.logger.Error("setattr: truncate failed", "error", err)
			return syscall.EIO
		}

		n.mu.Lock()
		n.size = in.Size
		n.isLocalWrite = true
		n.mu.Unlock()
	}

	// Handle mode change
	if in.Valid&fuse.FATTR_MODE != 0 {
		if n.sessionMgr.HasLocalOverride(n.path) {
			localPath, _ := n.sessionMgr.GetLocalPath(n.path)
			if err := os.Chmod(localPath, os.FileMode(in.Mode)); err != nil {
				n.logger.Error("setattr: chmod failed", "error", err)
				return syscall.EIO
			}
		}
		n.mu.Lock()
		n.mode = in.Mode
		n.mu.Unlock()
	}

	return n.fillAttrOut(out)
}

func (n *WritableNode) setattrLocal(_ context.Context, lh *LocalFileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if in.Valid&fuse.FATTR_SIZE != 0 {
		if err := lh.file.Truncate(int64(in.Size)); err != nil {
			n.logger.Error("setattr: truncate via handle failed", "error", err)
			return syscall.EIO
		}
		n.mu.Lock()
		n.size = in.Size
		n.mu.Unlock()
	}
	return n.fillAttrOut(out)
}

func (n *WritableNode) fillAttrOut(out *fuse.AttrOut) syscall.Errno {
	n.mu.RLock()
	defer n.mu.RUnlock()

	out.Size = n.size
	out.Mode = n.mode
	out.Nlink = 1
	if n.isDir {
		out.Nlink = 2
		out.Mode |= uint32(syscall.S_IFDIR)
	} else {
		out.Mode |= uint32(syscall.S_IFREG)
	}
	out.Uid = 1000
	out.Gid = 1000

	return 0
}

// Create implements fs.NodeCreater for creating new files
func (n *WritableNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	n.logger.Debug("create", "parent", n.path, "name", name, "mode", mode)

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("create: failed to start session", "error", err)
		return nil, nil, 0, syscall.EIO
	}

	newPath := name
	if n.path != "" {
		newPath = n.path + "/" + name
	}

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
	child := &WritableNode{
		path:         newPath,
		isDir:        false,
		mode:         mode,
		size:         0,
		client:       n.client,
		sessionMgr:   n.sessionMgr,
		logger:       n.logger,
		isLocalWrite: true,
	}

	stable := fs.StableAttr{
		Mode: fuse.S_IFREG | mode,
		Ino:  hashPath(newPath),
	}

	inode := n.NewInode(ctx, child, stable)

	out.Mode = mode | fuse.S_IFREG
	out.Size = 0
	out.Ino = stable.Ino
	out.Nlink = 1
	out.Uid = 1000
	out.Gid = 1000

	fh := &LocalFileHandle{
		file:   f,
		node:   child,
		logger: n.logger,
	}

	n.logger.Info("created file", "path", newPath)

	return inode, fh, fuse.FOPEN_DIRECT_IO, 0
}

// Mkdir implements fs.NodeMkdirer for creating directories
func (n *WritableNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.logger.Debug("mkdir", "parent", n.path, "name", name, "mode", mode)

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("mkdir: failed to start session", "error", err)
		return nil, syscall.EIO
	}

	newPath := name
	if n.path != "" {
		newPath = n.path + "/" + name
	}

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
	child := &WritableNode{
		path:       newPath,
		isDir:      true,
		mode:       mode | uint32(syscall.S_IFDIR),
		client:     n.client,
		sessionMgr: n.sessionMgr,
		logger:     n.logger,
	}

	stable := fs.StableAttr{
		Mode: fuse.S_IFDIR | mode,
		Ino:  hashPath(newPath),
	}

	inode := n.NewInode(ctx, child, stable)

	out.Mode = mode | fuse.S_IFDIR
	out.Ino = stable.Ino
	out.Nlink = 2
	out.Uid = 1000
	out.Gid = 1000

	n.logger.Info("created directory", "path", newPath)

	return inode, 0
}

// Unlink implements fs.NodeUnlinker for deleting files
func (n *WritableNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.logger.Debug("unlink", "parent", n.path, "name", name)

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("unlink: failed to start session", "error", err)
		return syscall.EIO
	}

	targetPath := name
	if n.path != "" {
		targetPath = n.path + "/" + name
	}

	// If there's a local copy, remove it
	if n.sessionMgr.HasLocalOverride(targetPath) {
		localPath, _ := n.sessionMgr.GetLocalPath(targetPath)
		if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
			n.logger.Error("unlink: remove local failed", "error", err)
			return syscall.EIO
		}
	}

	// Track the deletion
	if err := n.sessionMgr.TrackChange(ChangeDelete, targetPath, ""); err != nil {
		n.logger.Warn("unlink: failed to track change", "error", err)
	}

	n.logger.Info("deleted file", "path", targetPath)

	return 0
}

// Rmdir implements fs.NodeRmdirer for removing directories
func (n *WritableNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.logger.Debug("rmdir", "parent", n.path, "name", name)

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("rmdir: failed to start session", "error", err)
		return syscall.EIO
	}

	targetPath := name
	if n.path != "" {
		targetPath = n.path + "/" + name
	}

	// If there's a local copy, remove it
	if n.sessionMgr.HasLocalOverride(targetPath) {
		localPath, _ := n.sessionMgr.GetLocalPath(targetPath)
		if err := os.RemoveAll(localPath); err != nil && !os.IsNotExist(err) {
			n.logger.Error("rmdir: remove local failed", "error", err)
			return syscall.EIO
		}
	}

	// Track the deletion
	if err := n.sessionMgr.TrackChange(ChangeRmdir, targetPath, ""); err != nil {
		n.logger.Warn("rmdir: failed to track change", "error", err)
	}

	n.logger.Info("removed directory", "path", targetPath)

	return 0
}

// Rename implements fs.NodeRenamer for moving/renaming files
func (n *WritableNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	n.logger.Debug("rename", "oldParent", n.path, "oldName", name, "newName", newName)

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("rename: failed to start session", "error", err)
		return syscall.EIO
	}

	oldPath := name
	if n.path != "" {
		oldPath = n.path + "/" + name
	}

	// Get new parent path
	var newParentPath string
	if wn, ok := newParent.(*WritableNode); ok {
		newParentPath = wn.path
	} else if gn, ok := newParent.(*MonoNode); ok {
		newParentPath = gn.path
	}

	newPath := newName
	if newParentPath != "" {
		newPath = newParentPath + "/" + newName
	}

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

	// Track as delete + create
	n.sessionMgr.TrackChange(ChangeDelete, oldPath, "")
	n.sessionMgr.TrackChange(ChangeCreate, newPath, "")

	n.logger.Info("renamed", "from", oldPath, "to", newPath)

	return 0
}

// ensureLocalCopy copies the file from backend to local overlay
func (n *WritableNode) ensureLocalCopy(ctx context.Context) error {
	return n.ensureLocalCopyFor(ctx, n.path)
}

func (n *WritableNode) ensureLocalCopyFor(ctx context.Context, monofsPath string) error {
	localPath, err := n.sessionMgr.GetLocalPath(monofsPath)
	if err != nil {
		return err
	}

	// Check if already exists locally
	if _, err := os.Stat(localPath); err == nil {
		return nil // Already have local copy
	}

	// Create parent directories
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("create parent dirs: %w", err)
	}

	// Fetch from backend
	content, err := n.client.Read(ctx, monofsPath, 0, 0)
	if err != nil {
		// File might be new, create empty
		n.logger.Debug("backend read failed, creating empty file", "path", monofsPath, "error", err)
		if err := os.WriteFile(localPath, []byte{}, 0644); err != nil {
			return fmt.Errorf("create empty file: %w", err)
		}
	} else {
		if err := os.WriteFile(localPath, content, 0644); err != nil {
			return fmt.Errorf("write local copy: %w", err)
		}
		n.mu.Lock()
		n.origBlobHash = "" // TODO: get actual hash
		n.mu.Unlock()
	}

	return nil
}

// OpenForWrite opens a file for writing
func (n *WritableNode) OpenForWrite(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.logger.Debug("open for write", "path", n.path, "flags", flags)

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		n.logger.Error("open: failed to start session", "error", err)
		return nil, 0, syscall.EIO
	}

	// Create local copy for writing
	if err := n.ensureLocalCopy(ctx); err != nil {
		n.logger.Error("open: failed to create local copy", "error", err)
		return nil, 0, syscall.EIO
	}

	localPath, _ := n.sessionMgr.GetLocalPath(n.path)

	// Determine open flags
	openFlags := os.O_RDWR
	if flags&syscall.O_APPEND != 0 {
		openFlags |= os.O_APPEND
	}
	if flags&syscall.O_TRUNC != 0 {
		openFlags |= os.O_TRUNC
	}

	f, err := os.OpenFile(localPath, openFlags, 0644)
	if err != nil {
		n.logger.Error("open: failed to open local file", "error", err)
		return nil, 0, syscall.EIO
	}

	// Track the modification
	changeType := ChangeModify
	if n.origBlobHash == "" {
		changeType = ChangeCreate
	}
	n.sessionMgr.TrackChange(changeType, n.path, n.origBlobHash)

	n.mu.Lock()
	n.isLocalWrite = true
	n.mu.Unlock()

	fh := &LocalFileHandle{
		file:   f,
		node:   n,
		logger: n.logger,
	}

	return fh, fuse.FOPEN_DIRECT_IO, 0
}

// hashPath creates a stable inode number from a path
func hashPath(path string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(path))
	return h.Sum64()
}

// Ensure fmt is used
var _ = fmt.Sprintf
