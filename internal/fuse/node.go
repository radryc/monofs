// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radryc/monofs/internal/cache"
	"github.com/radryc/monofs/internal/client"
)

// MonoNode represents a node in the MonoFS filesystem.
// It implements various fs.Node* interfaces for FUSE operations.
type MonoNode struct {
	fs.Inode

	// Path relative to repository root
	path string

	// Whether this node is a directory
	isDir bool

	// File mode
	mode uint32

	// File size (0 for directories)
	size uint64

	// Backend client for gRPC operations
	client client.MonoFSClient

	// Optional cache layer (can be nil)
	cache *cache.Cache

	// Session manager for write operations (can be nil for read-only)
	sessionMgr *SessionManager

	// Logger for structured logging
	logger *slog.Logger

	// Mutex for protecting concurrent access
	mu sync.RWMutex

	// File content buffer for opened files
	content []byte

	// Backend error tracking
	backendError   error
	lastErrorCheck time.Time

	// Catastrophic error tracking (shared across all nodes via root)
	catastrophicError string
	catErrorMu        sync.RWMutex

	// Write support fields
	isLocalWrite  bool     // True if file has local modifications
	localHandle   *os.File // Handle to local file when writing
	symlinkTarget string   // Target path for symlink nodes
}

// Ensure MonoNode implements required interfaces
var (
	_ fs.NodeLookuper  = (*MonoNode)(nil)
	_ fs.NodeGetattrer = (*MonoNode)(nil)
	_ fs.NodeReaddirer = (*MonoNode)(nil)
	_ fs.NodeOpener    = (*MonoNode)(nil)
	_ fs.NodeReader    = (*MonoNode)(nil)
	_ fs.NodeStatfser  = (*MonoNode)(nil)
	// Write interfaces
	_ fs.NodeSetattrer = (*MonoNode)(nil)
	_ fs.NodeCreater   = (*MonoNode)(nil)
	_ fs.NodeMkdirer   = (*MonoNode)(nil)
	_ fs.NodeUnlinker  = (*MonoNode)(nil)
	_ fs.NodeRmdirer   = (*MonoNode)(nil)
	_ fs.NodeRenamer   = (*MonoNode)(nil)
	_ fs.NodeWriter    = (*MonoNode)(nil)
	// Symlink interfaces
	_ fs.NodeSymlinker  = (*MonoNode)(nil)
	_ fs.NodeReadlinker = (*MonoNode)(nil)
)

// NewRoot creates the root node of the MonoFS filesystem.
func NewRoot(c client.MonoFSClient, cache *cache.Cache, logger *slog.Logger) *MonoNode {
	if logger == nil {
		logger = slog.Default()
	}
	return &MonoNode{
		path:   "",
		isDir:  true,
		mode:   0755 | uint32(syscall.S_IFDIR),
		client: c,
		cache:  cache,
		logger: logger.With("component", "fuse"),
	}
}

// NewRootWithSession creates the root node with session manager for write support.
func NewRootWithSession(c client.MonoFSClient, cache *cache.Cache, sessionMgr *SessionManager, logger *slog.Logger) *MonoNode {
	if logger == nil {
		logger = slog.Default()
	}
	return &MonoNode{
		path:       "",
		isDir:      true,
		mode:       0755 | uint32(syscall.S_IFDIR),
		client:     c,
		cache:      cache,
		sessionMgr: sessionMgr,
		logger:     logger.With("component", "fuse"),
	}
}

// newChild creates a child node with inherited client and cache.
func (n *MonoNode) newChild(name string, isDir bool, mode uint32, size uint64) *MonoNode {
	path := name
	if n.path != "" {
		path = n.path + "/" + name
	}
	return &MonoNode{
		path:       path,
		isDir:      isDir,
		mode:       mode,
		size:       size,
		client:     n.client,
		cache:      n.cache,
		sessionMgr: n.sessionMgr,
		logger:     n.logger,
	}
}

// toErrno converts any error to syscall.Errno.
// All backend errors are mapped to EIO for simplicity.
func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	// Map all errors to EIO as per requirements
	return syscall.EIO
}

// getRootNode safely returns the root MonoNode, or self if not embedded in FUSE tree.
// This handles the case of unit tests where nodes aren't mounted.
func (n *MonoNode) getRootNode() *MonoNode {
	// Check if properly embedded in FUSE tree
	inode := n.EmbeddedInode()
	if inode == nil {
		// Not embedded - if we're root (path==""), return self
		if n.path == "" {
			return n
		}
		return nil
	}
	// Try to get parent to check if we're in a proper tree
	_, parentInode := inode.Parent()
	if parentInode == nil && n.path == "" {
		// We are root but not mounted yet
		return n
	}
	root := n.Root()
	if rootNode, ok := root.Operations().(*MonoNode); ok {
		return rootNode
	}
	return nil
}

// updateBackendError updates the backend error state
func (n *MonoNode) updateBackendError(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.backendError = err
	n.lastErrorCheck = time.Now()
}

// getBackendError returns the current backend error state
func (n *MonoNode) getBackendError() (error, time.Time) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.backendError, n.lastErrorCheck
}

// hasBackendError checks if there's an active backend error
func (n *MonoNode) hasBackendError() bool {
	err, _ := n.getBackendError()
	return err != nil
}

// attrTimeout returns the cache timeout duration for FUSE.
func attrTimeout() time.Duration {
	return cache.DefaultAttrTTL
}

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
				err, errTime := n.getBackendError()
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

	// Fast path: Get all session state in a single lock acquisition
	// This replaces 4+ separate lock/unlock cycles with 1
	if n.sessionMgr != nil {
		// For root level, check user root dirs
		if n.path == "" {
			state := n.sessionMgr.GetPathState(name)
			if state.HasSession && state.IsUserRootDir {
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
		}

		// Get consolidated session state for child path (single lock)
		state := n.sessionMgr.GetPathState(childPath)
		if state.HasSession {
			// Check symlink first
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

			// Check if deleted
			if state.IsDeleted {
				n.logger.Debug("lookup: file deleted in session", "path", childPath)
				return nil, syscall.ENOENT
			}

			// Check local override - use cached state, but verify with disk if needed
			if state.HasOverride {
				overlay := NewOverlayManager(n.sessionMgr)
				attr, err := overlay.GetLocalAttr(childPath)
				if err == nil {
					n.logger.Debug("lookup: using local override", "path", childPath)
					child := n.newChild(name, attr.IsDir, attr.Mode, attr.Size)
					out.Mode = attr.Mode
					out.Size = attr.Size
					out.Ino = hashPathForNode(childPath)
					out.Mtime = uint64(attr.Mtime)
					out.Atime = uint64(attr.Mtime)
					out.Ctime = uint64(attr.Mtime)
					out.Uid = 1000
					out.Gid = 1000
					out.Nlink = 1
					if attr.IsDir {
						out.Nlink = 2
					}
					out.SetEntryTimeout(attrTimeout())
					out.SetAttrTimeout(attrTimeout())

					stable := fs.StableAttr{Mode: attr.Mode, Ino: hashPathForNode(childPath)}
					return n.NewInode(ctx, child, stable), 0
				}
			} else if !state.HasOverride {
				// Not in cache - do disk check and update cache
				if n.sessionMgr.HasLocalOverride(childPath) {
					overlay := NewOverlayManager(n.sessionMgr)
					attr, err := overlay.GetLocalAttr(childPath)
					if err == nil {
						n.logger.Debug("lookup: using local override", "path", childPath)
						child := n.newChild(name, attr.IsDir, attr.Mode, attr.Size)
						out.Mode = attr.Mode
						out.Size = attr.Size
						out.Ino = hashPathForNode(childPath)
						out.Mtime = uint64(attr.Mtime)
						out.Atime = uint64(attr.Mtime)
						out.Ctime = uint64(attr.Mtime)
						out.Uid = 1000
						out.Gid = 1000
						out.Nlink = 1
						if attr.IsDir {
							out.Nlink = 2
						}
						out.SetEntryTimeout(attrTimeout())
						out.SetAttrTimeout(attrTimeout())

						stable := fs.StableAttr{Mode: attr.Mode, Ino: hashPathForNode(childPath)}
						return n.NewInode(ctx, child, stable), 0
					}
				}
			}
		}
	}

	// Try cache first if available
	if n.cache != nil {
		if attr, err := n.cache.GetAttr(childPath); err == nil {
			n.logger.Debug("lookup cache hit", "path", childPath)
			isDir := attr.Mode&uint32(syscall.S_IFDIR) != 0
			child := n.newChild(name, isDir, attr.Mode, attr.Size)
			out.Mode = attr.Mode
			out.Size = attr.Size
			out.Ino = attr.Ino
			out.Mtime = uint64(attr.Mtime)
			out.Atime = uint64(attr.Atime)
			out.Ctime = uint64(attr.Ctime)
			out.Uid = attr.Uid
			out.Gid = attr.Gid
			out.Nlink = attr.Nlink
			out.SetEntryTimeout(attrTimeout())
			out.SetAttrTimeout(attrTimeout())
			stable := fs.StableAttr{Mode: attr.Mode, Ino: attr.Ino}
			return n.NewInode(ctx, child, stable), 0
		}
	}

	// Query backend
	resp, err := n.client.Lookup(ctx, childPath)
	if err != nil {
		n.logger.Debug("lookup failed", "path", childPath, "error", err)
		n.updateBackendError(err)
		return nil, toErrno(err)
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

	isDir := resp.Mode&uint32(syscall.S_IFDIR) != 0
	child := n.newChild(name, isDir, resp.Mode, resp.Size)
	out.Mode = resp.Mode
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
	out.SetEntryTimeout(attrTimeout())
	out.SetAttrTimeout(attrTimeout())

	stable := fs.StableAttr{Mode: resp.Mode, Ino: resp.Ino}
	return n.NewInode(ctx, child, stable), 0
}

// Getattr returns file attributes.
func (n *MonoNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	defer n.recoverPanic("Getattr")
	n.logger.Debug("getattr", "path", n.path)

	// Special handling for FS_ERROR.txt
	if n.path == "FS_ERROR.txt" && n.hasBackendError() {
		err, errTime := n.getBackendError()
		errorMsg := fmt.Sprintf("MonoFS Backend Connection Error\n\nTime: %s\nError: %s\n\nThe backend servers are unavailable. This file will disappear when the connection is restored.\n",
			errTime.Format(time.RFC3339), err.Error())
		out.Mode = 0444 | uint32(syscall.S_IFREG)
		out.Size = uint64(len(errorMsg))
		out.Ino = 0xFFFFFFFF
		out.Nlink = 1
		out.SetTimeout(1 * time.Second)
		return 0
	}

	// For root, return directory attrs
	if n.path == "" {
		now := uint64(time.Now().Unix())
		out.Mode = 0755 | uint32(syscall.S_IFDIR)
		out.Size = 0
		out.Nlink = 2
		out.Mtime = now
		out.Atime = now
		out.Ctime = now
		out.Uid = 1000
		out.Gid = 1000
		out.SetTimeout(attrTimeout())
		return 0
	}

	// Check if this is a user-created root directory or has session state
	// Use consolidated GetPathState for single lock acquisition
	if n.sessionMgr != nil {
		// Check for user root directories (first path component)
		parts := splitPath(n.path)
		if len(parts) >= 1 {
			rootState := n.sessionMgr.GetPathState(parts[0])
			if rootState.HasSession && rootState.IsUserRootDir {
				now := uint64(time.Now().Unix())
				out.Mode = 0755 | uint32(syscall.S_IFDIR)
				out.Size = 0
				out.Nlink = 2
				out.Mtime = now
				out.Atime = now
				out.Ctime = now
				out.Uid = 1000
				out.Gid = 1000
				out.Ino = hashPathForNode(n.path)
				out.SetTimeout(attrTimeout())
				n.logger.Debug("getattr: user root directory", "path", n.path)
				return 0
			}
		}

		// Get consolidated session state (single lock)
		state := n.sessionMgr.GetPathState(n.path)
		if state.HasSession {
			// Check for symlink
			if state.IsSymlink {
				now := uint64(time.Now().Unix())
				out.Mode = 0777 | uint32(syscall.S_IFLNK)
				out.Size = uint64(len(state.SymlinkTarget))
				out.Nlink = 1
				out.Mtime = now
				out.Atime = now
				out.Ctime = now
				out.Uid = 1000
				out.Gid = 1000
				out.Ino = hashPathForNode(n.path)
				out.SetTimeout(attrTimeout())
				n.logger.Debug("getattr: symlink", "path", n.path, "target", state.SymlinkTarget)
				return 0
			}

			// Check for local override
			hasOverride := state.HasOverride
			if !hasOverride {
				// Not cached - check disk
				hasOverride = n.sessionMgr.HasLocalOverride(n.path)
			}
			if hasOverride {
				overlay := NewOverlayManager(n.sessionMgr)
				attr, err := overlay.GetLocalAttr(n.path)
				if err == nil {
					n.logger.Debug("getattr: using local override", "path", n.path)
					out.Mode = attr.Mode
					out.Size = attr.Size
					out.Ino = hashPathForNode(n.path)
					out.Mtime = uint64(attr.Mtime)
					out.Atime = uint64(attr.Mtime)
					out.Ctime = uint64(attr.Mtime)
					out.Uid = 1000
					out.Gid = 1000
					out.Nlink = 1
					if attr.IsDir {
						out.Nlink = 2
					}
					out.SetTimeout(attrTimeout())
					return 0
				}
			}

			// Check if deleted
			if state.IsDeleted {
				n.logger.Debug("getattr: file deleted in session", "path", n.path)
				return syscall.ENOENT
			}
		}
	}

	// Try cache first if available
	if n.cache != nil {
		if attr, err := n.cache.GetAttr(n.path); err == nil {
			n.logger.Debug("getattr cache hit", "path", n.path)
			out.Mode = attr.Mode
			out.Size = attr.Size
			out.Ino = attr.Ino
			out.Mtime = uint64(attr.Mtime)
			out.Atime = uint64(attr.Atime)
			out.Ctime = uint64(attr.Ctime)
			out.Nlink = attr.Nlink
			out.Uid = attr.Uid
			out.Gid = attr.Gid
			out.SetTimeout(attrTimeout())
			return 0
		}
	}

	// Query backend
	resp, err := n.client.GetAttr(ctx, n.path)
	if err != nil {
		n.logger.Debug("getattr failed", "path", n.path, "error", err)
		n.updateBackendError(err)
		return toErrno(err)
	}

	// Clear backend error on success
	n.updateBackendError(nil)

	if !resp.Found {
		return syscall.ENOENT
	}

	// Update cache if available
	if n.cache != nil {
		_ = n.cache.PutAttr(n.path, &cache.AttrEntry{
			Ino:   resp.Ino,
			Mode:  resp.Mode,
			Size:  resp.Size,
			Mtime: resp.Mtime,
			Atime: resp.Atime,
			Ctime: resp.Ctime,
			Nlink: resp.Nlink,
			Uid:   resp.Uid,
			Gid:   resp.Gid,
		})
	}

	out.Mode = resp.Mode
	out.Size = resp.Size
	out.Ino = resp.Ino
	out.Mtime = uint64(resp.Mtime)
	out.Atime = uint64(resp.Atime)
	out.Ctime = uint64(resp.Ctime)
	out.Nlink = resp.Nlink
	out.Uid = resp.Uid
	out.Gid = resp.Gid
	out.SetTimeout(attrTimeout())
	return 0
}

// recoverPanic catches panics and stores them as catastrophic errors
func (n *MonoNode) recoverPanic(operation string) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		errMsg := fmt.Sprintf("PANIC in %s: %v\n\nStack trace:\n%s", operation, r, string(stack))
		n.logger.Error("catastrophic error", "operation", operation, "panic", r)

		// Store error in root node (only if properly embedded in FUSE tree)
		inode := n.EmbeddedInode()
		_, parentInode := inode.Parent() // Parent() returns (name, *Inode)
		if inode != nil && parentInode != nil {
			root := n.Root()
			if rootNode, ok := root.Operations().(*MonoNode); ok {
				rootNode.catErrorMu.Lock()
				rootNode.catastrophicError = errMsg
				rootNode.catErrorMu.Unlock()
			}
		} else {
			// Not properly embedded - store on self if we're root
			n.catErrorMu.Lock()
			n.catastrophicError = errMsg
			n.catErrorMu.Unlock()
		}
	}
}

// Readdir reads directory entries.
func (n *MonoNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	defer n.recoverPanic("Readdir")
	n.client.RecordOperation()
	n.logger.Debug("readdir", "path", n.path)

	// Check if this is a user-created root directory (or subdirectory of one)
	if n.sessionMgr != nil && n.path != "" {
		parts := splitPath(n.path)
		if len(parts) >= 1 && n.sessionMgr.IsUserRootDir(parts[0]) {
			// This is a user directory - only return local overlay entries
			n.logger.Debug("readdir: user directory", "path", n.path)
			overlay := NewOverlayManager(n.sessionMgr)
			dirEntries := overlay.MergeReadDir(nil, n.path)
			return fs.NewListDirStream(dirEntries), 0
		}
	}

	// Try cache first if available
	if n.cache != nil {
		if entries, err := n.cache.GetDir(n.path); err == nil {
			n.logger.Debug("readdir cache hit", "path", n.path, "count", len(entries))
			// Convert cache entries to fuse entries
			dirEntries := make([]fuse.DirEntry, len(entries))
			for i, e := range entries {
				dirEntries[i] = fuse.DirEntry{
					Name: e.Name,
					Mode: e.Mode,
					Ino:  e.Ino,
				}
			}
			// Add FS_ERROR.txt if this is root and there's a catastrophic error
			if n.path == "" {
				if rootNode := n.getRootNode(); rootNode != nil {
					rootNode.catErrorMu.RLock()
					hasCatError := rootNode.catastrophicError != ""
					rootNode.catErrorMu.RUnlock()
					if hasCatError {
						dirEntries = append(dirEntries, fuse.DirEntry{
							Name: "FS_ERROR.txt",
							Mode: 0444 | uint32(syscall.S_IFREG),
							Ino:  0xFFFFFFFF,
						})
					}
				}
			}
			// Still need to merge overlay even for cached results
			if n.sessionMgr != nil && n.sessionMgr.HasActiveSession() {
				overlay := NewOverlayManager(n.sessionMgr)
				dirEntries = overlay.MergeReadDir(dirEntries, n.path)
			}
			return fs.NewListDirStream(dirEntries), 0
		}
		n.logger.Debug("readdir cache miss", "path", n.path)
	}

	// Check for catastrophic error file in root directory
	if n.path == "" {
		if rootNode := n.getRootNode(); rootNode != nil {
			rootNode.catErrorMu.RLock()
			hasCatastrophicError := rootNode.catastrophicError != ""
			rootNode.catErrorMu.RUnlock()

			if hasCatastrophicError {
				n.logger.Debug("catastrophic error exists, including FS_ERROR.txt")
			}
		}
	}

	// Query backend
	n.logger.Debug("readdir calling backend rpc", "path", n.path)
	backendEntries, err := n.client.ReadDir(ctx, n.path)
	if err != nil {
		n.logger.Debug("readdir failed", "path", n.path, "error", err)
		n.updateBackendError(err)
		// For root directory, return FS_ERROR.txt instead of failing
		if n.path == "" {
			n.logger.Debug("readdir backend error, returning error file", "error", err)

			// Store error in root node
			if rootNode := n.getRootNode(); rootNode != nil {
				rootNode.catErrorMu.Lock()
				rootNode.catastrophicError = fmt.Sprintf("Backend error in Readdir: %v", err)
				rootNode.catErrorMu.Unlock()
			}

			errorEntry := []fuse.DirEntry{
				{
					Name: "FS_ERROR.txt",
					Mode: 0444 | uint32(syscall.S_IFREG),
					Ino:  0xFFFFFFFF,
				},
			}
			return fs.NewListDirStream(errorEntry), 0
		}
		return nil, toErrno(err)
	}
	// Clear backend error on success
	n.updateBackendError(nil)
	// Clear catastrophic error on success (backend has recovered)
	if n.path == "" {
		if rootNode := n.getRootNode(); rootNode != nil {
			rootNode.catErrorMu.Lock()
			if rootNode.catastrophicError != "" {
				n.logger.Info("backend recovered, clearing catastrophic error")
				rootNode.catastrophicError = ""
			}
			rootNode.catErrorMu.Unlock()
		}
	}
	n.logger.Debug("readdir backend returned", "path", n.path, "entries", len(backendEntries))

	// Convert to FUSE entries and cache entries
	dirEntries := make([]fuse.DirEntry, len(backendEntries))
	cacheEntries := make([]cache.DirEntry, len(backendEntries))

	for i, e := range backendEntries {
		n.logger.Debug("readdir entry",
			"path", n.path,
			"name", e.Name,
			"mode", fmt.Sprintf("0%o", e.Mode),
			"ino", e.Ino)
		dirEntries[i] = fuse.DirEntry{
			Name: e.Name,
			Mode: e.Mode,
			Ino:  e.Ino,
		}
		cacheEntries[i] = cache.DirEntry{
			Name: e.Name,
			Mode: e.Mode,
			Ino:  e.Ino,
		}
	}

	// Add FS_ERROR.txt if this is root and there's a catastrophic error or backend error
	if n.path == "" {
		showErrorFile := false
		if rootNode := n.getRootNode(); rootNode != nil {
			rootNode.catErrorMu.RLock()
			showErrorFile = rootNode.catastrophicError != ""
			rootNode.catErrorMu.RUnlock()
		}
		if !showErrorFile {
			showErrorFile = n.hasBackendError()
		}
		if showErrorFile {
			dirEntries = append(dirEntries, fuse.DirEntry{
				Name: "FS_ERROR.txt",
				Mode: 0444 | uint32(syscall.S_IFREG),
				Ino:  0xFFFFFFFF,
			})
		}
	}

	// Update cache if available
	if n.cache != nil {
		_ = n.cache.PutDir(n.path, cacheEntries)
	}

	// Merge local overlay changes if session manager is available
	if n.sessionMgr != nil && n.sessionMgr.HasActiveSession() {
		overlay := NewOverlayManager(n.sessionMgr)
		dirEntries = overlay.MergeReadDir(dirEntries, n.path)
	}

	n.logger.Debug("readdir complete", "path", n.path, "count", len(dirEntries))
	return fs.NewListDirStream(dirEntries), 0
}

// Open opens a file.
func (n *MonoNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.logger.Debug("open", "path", n.path, "flags", flags)

	// Special handling for FS_ERROR.txt - content is already set
	if n.path == "FS_ERROR.txt" && len(n.content) > 0 {
		return nil, fuse.FOPEN_KEEP_CACHE, 0
	}

	// Check if this is a write operation
	isWrite := flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_TRUNC) != 0

	if isWrite {
		// Handle write mode
		if n.sessionMgr == nil {
			n.logger.Warn("open: write requested but no session manager")
			return nil, 0, syscall.EROFS
		}

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

		n.mu.Lock()
		n.isLocalWrite = true
		n.localHandle = f
		n.mu.Unlock()

		fh := &monofsFileHandle{
			file:   f,
			node:   n,
			logger: n.logger,
		}

		return fh, fuse.FOPEN_DIRECT_IO, 0
	}

	// Read mode: check for local override first
	if n.sessionMgr != nil && n.sessionMgr.HasLocalOverride(n.path) {
		localPath, _ := n.sessionMgr.GetLocalPath(n.path)
		content, err := os.ReadFile(localPath)
		if err == nil {
			n.mu.Lock()
			n.content = content
			n.mu.Unlock()
			n.logger.Debug("open: using local override", "path", n.path, "size", len(content))
			return nil, fuse.FOPEN_KEEP_CACHE, 0
		}
	}

	// Read file content from backend with retry logic
	const maxRetries = 3
	var content []byte
	var err error

	for attempt := 0; attempt < maxRetries; attempt++ {
		n.logger.Debug("open calling client.Read", "path", n.path, "size", n.size, "attempt", attempt+1)
		content, err = n.client.Read(ctx, n.path, 0, 0) // 0, 0 means read entire file
		n.logger.Debug("open client.Read returned", "path", n.path, "content_len", len(content), "error", err)

		if err == nil {
			break
		}

		n.logger.Debug("open read retry", "path", n.path, "attempt", attempt+1, "error", err)

		// Wait before retry with exponential backoff
		if attempt < maxRetries-1 {
			delay := time.Duration(100*(1<<uint(attempt))) * time.Millisecond
			select {
			case <-ctx.Done():
				n.logger.Debug("open cancelled during retry", "path", n.path)
				n.updateBackendError(ctx.Err())
				return nil, 0, syscall.EINTR
			case <-time.After(delay):
			}
		}
	}

	if err != nil {
		n.logger.Debug("open failed after retries", "path", n.path, "error", err)
		n.updateBackendError(err)
		return nil, 0, toErrno(err)
	}

	// Clear backend error on success
	n.updateBackendError(nil)

	n.mu.Lock()
	n.content = content
	n.mu.Unlock()

	n.logger.Debug("open complete", "path", n.path, "size", len(content))
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

// Read reads file content.
// If content is nil (file was cleaned up or not opened), attempts to reload.
func (n *MonoNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	defer n.recoverPanic("Read")
	n.logger.Debug("read", "path", n.path, "offset", off, "len", len(dest))

	n.mu.RLock()
	content := n.content
	n.mu.RUnlock()

	// If content is nil, try to reload it (handles race conditions and cleanup)
	if content == nil {
		n.logger.Debug("read: content nil, attempting reload", "path", n.path)

		// Try to reload content with retry
		const maxRetries = 3
		var err error

		for attempt := 0; attempt < maxRetries; attempt++ {
			content, err = n.client.Read(ctx, n.path, 0, 0)
			if err == nil && content != nil {
				// Cache the content for future reads
				n.mu.Lock()
				n.content = content
				n.mu.Unlock()
				n.logger.Debug("read: content reloaded successfully", "path", n.path, "size", len(content))
				break
			}

			n.logger.Debug("read: reload retry", "path", n.path, "attempt", attempt+1, "error", err)

			if attempt < maxRetries-1 {
				delay := time.Duration(100*(1<<uint(attempt))) * time.Millisecond
				select {
				case <-ctx.Done():
					return nil, syscall.EINTR
				case <-time.After(delay):
				}
			}
		}

		if content == nil {
			n.logger.Debug("read: reload failed after retries", "path", n.path, "error", err)
			n.updateBackendError(err)
			return nil, syscall.EIO
		}

		// Clear backend error on success
		n.updateBackendError(nil)
	}

	end := int(off) + len(dest)
	if end > len(content) {
		end = len(content)
	}
	if int(off) >= len(content) {
		n.client.RecordOperation()
		return fuse.ReadResultData(nil), 0
	}

	bytesRead := int64(end - int(off))
	n.client.RecordOperation()
	n.client.RecordBytesRead(bytesRead)
	return fuse.ReadResultData(content[off:end]), 0
}

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

// =============================================================================
// Write Operations
// =============================================================================

// Setattr implements fs.NodeSetattrer for truncate/chmod operations
func (n *MonoNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.logger.Debug("setattr", "path", n.path, "valid", in.Valid)

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("setattr: no session manager, read-only mode")
		return syscall.EROFS
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

	// Fill out attributes
	return n.Getattr(ctx, fh, out)
}

// Create implements fs.NodeCreater for creating new files
func (n *MonoNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
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
	out.SetEntryTimeout(attrTimeout())
	out.SetAttrTimeout(attrTimeout())

	fh := &monofsFileHandle{
		file:   f,
		node:   child,
		logger: n.logger,
	}

	n.logger.Info("created file", "path", newPath)

	return inode, fh, fuse.FOPEN_DIRECT_IO, 0
}

// Mkdir implements fs.NodeMkdirer for creating directories
func (n *MonoNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
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

// Unlink implements fs.NodeUnlinker for deleting files
func (n *MonoNode) Unlink(ctx context.Context, name string) syscall.Errno {
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

	return 0
}

// Rmdir implements fs.NodeRmdirer for removing directories
func (n *MonoNode) Rmdir(ctx context.Context, name string) syscall.Errno {
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

	return 0
}

// Rename implements fs.NodeRenamer for moving/renaming files
func (n *MonoNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
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

	// Track as delete + create
	n.sessionMgr.TrackChange(ChangeDelete, oldPath, "")
	n.sessionMgr.TrackChange(ChangeCreate, newPath, "")

	n.logger.Info("renamed", "from", oldPath, "to", newPath)

	return 0
}

// Write implements fs.NodeWriter for writing to files
func (n *MonoNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	n.logger.Debug("write", "path", n.path, "offset", off, "len", len(data))

	// Use file handle if available
	if gfh, ok := fh.(*monofsFileHandle); ok {
		return gfh.Write(ctx, data, off)
	}

	// Check if we have session manager for writes
	if n.sessionMgr == nil {
		n.logger.Warn("write: no session manager, read-only mode")
		return 0, syscall.EROFS
	}

	// Ensure local copy exists
	if err := n.ensureLocalCopy(ctx); err != nil {
		n.logger.Error("write: failed to create local copy", "error", err)
		return 0, syscall.EIO
	}

	localPath, _ := n.sessionMgr.GetLocalPath(n.path)

	// Open for writing
	f, err := os.OpenFile(localPath, os.O_RDWR, 0644)
	if err != nil {
		n.logger.Error("write: failed to open local file", "error", err)
		return 0, syscall.EIO
	}
	defer f.Close()

	// Write at offset
	written, err := f.WriteAt(data, off)
	if err != nil {
		n.logger.Error("write: write failed", "error", err)
		return 0, syscall.EIO
	}

	// Update size if needed
	n.mu.Lock()
	newSize := uint64(off) + uint64(written)
	if newSize > n.size {
		n.size = newSize
	}
	n.isLocalWrite = true
	n.mu.Unlock()

	return uint32(written), 0
}

// ensureLocalCopy copies the file from backend to local overlay
func (n *MonoNode) ensureLocalCopy(ctx context.Context) error {
	return n.ensureLocalCopyFor(ctx, n.path)
}

func (n *MonoNode) ensureLocalCopyFor(ctx context.Context, monofsPath string) error {
	if n.sessionMgr == nil {
		return fmt.Errorf("no session manager")
	}

	// Ensure session exists
	if _, err := n.sessionMgr.StartSession(); err != nil {
		return err
	}

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
	}

	// Track modification
	n.sessionMgr.TrackChange(ChangeModify, monofsPath, "")

	return nil
}

// hashPathForNode creates a stable inode number from a path
func hashPathForNode(path string) uint64 {
	h := uint64(14695981039346656037) // FNV offset basis
	for _, c := range []byte(path) {
		h ^= uint64(c)
		h *= 1099511628211 // FNV prime
	}
	return h
}

// splitPath splits a path into its components
func splitPath(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// monofsFileHandle wraps an os.File for FUSE write operations
type monofsFileHandle struct {
	file   *os.File
	node   *MonoNode
	logger *slog.Logger
}

// Ensure monofsFileHandle implements required interfaces
var (
	_ fs.FileReader   = (*monofsFileHandle)(nil)
	_ fs.FileWriter   = (*monofsFileHandle)(nil)
	_ fs.FileFlusher  = (*monofsFileHandle)(nil)
	_ fs.FileReleaser = (*monofsFileHandle)(nil)
)

// Read implements fs.FileReader
func (h *monofsFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.logger.Debug("handle read", "offset", off, "len", len(dest))

	n, err := h.file.ReadAt(dest, off)
	if err != nil && err.Error() != "EOF" {
		h.logger.Error("handle read failed", "error", err)
		return nil, syscall.EIO
	}
	h.node.client.RecordOperation()
	h.node.client.RecordBytesRead(int64(n))
	return fuse.ReadResultData(dest[:n]), 0
}

// Write implements fs.FileWriter
func (h *monofsFileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.logger.Debug("handle write", "offset", off, "len", len(data))

	n, err := h.file.WriteAt(data, off)
	if err != nil {
		h.logger.Error("handle write failed", "error", err)
		return 0, syscall.EIO
	}

	h.node.client.RecordOperation()

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
func (h *monofsFileHandle) Flush(ctx context.Context) syscall.Errno {
	h.logger.Debug("handle flush")

	if err := h.file.Sync(); err != nil {
		h.logger.Error("handle flush failed", "error", err)
		return syscall.EIO
	}
	return 0
}

// Release implements fs.FileReleaser
func (h *monofsFileHandle) Release(ctx context.Context) syscall.Errno {
	h.logger.Debug("handle release")

	if h.file != nil {
		h.file.Close()
		h.file = nil
	}
	return 0
}

// Symlink implements fs.NodeSymlinker for creating symbolic links
func (n *MonoNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
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
