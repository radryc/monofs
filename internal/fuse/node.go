// Package fuse implements the FUSE filesystem layer for MonoFS.
//
// Each FUSE syscall lives in its own file (op_*.go). This file contains the
// MonoNode type definition, constructors, and shared helpers used across
// multiple operations.
package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
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

	// Tracking fields — populated on every mutation for auditing/debugging
	modTime   time.Time // When this node was last created or modified
	sessionID string    // Session ID active at the time of last mutation
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

// =============================================================================
// Shared helpers used by multiple op_*.go files
// =============================================================================

// stampNode records the current time and active session ID on the node.
// Called on every mutation (create, write, setattr, rename, symlink).
func (n *MonoNode) stampNode() {
	now := time.Now()
	var sid string
	if n.sessionMgr != nil {
		if s := n.sessionMgr.GetCurrentSession(); s != nil {
			sid = s.ID
		}
	}
	n.mu.Lock()
	n.modTime = now
	n.sessionID = sid
	n.mu.Unlock()
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

// recordAndConvertError records an I/O error metric on the client and converts to errno.
// Context cancellations (FUSE kernel aborting the request) are NOT counted as errors.
func (n *MonoNode) recordAndConvertError(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	// Context cancelled = kernel/process abandoned the request, not a real error
	if err == context.Canceled || err == context.DeadlineExceeded {
		return syscall.EINTR
	}
	if n.client != nil {
		n.client.RecordError()
	}
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
func (n *MonoNode) getBackendError() (time.Time, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.lastErrorCheck, n.backendError
}

// hasBackendError checks if there's an active backend error within the TTL window.
// After backendErrorTTL elapses since the last error, this returns false even
// if no successful request has explicitly cleared it. This prevents stale
// FS_ERROR.txt files persisting after backend recovery.
func (n *MonoNode) hasBackendError() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.backendError == nil {
		return false
	}
	return time.Since(n.lastErrorCheck) < backendErrorTTL
}

// Retry and reliability constants for FUSE operations.
const (
	// maxMetadataRetries is the number of retries for Lookup/Getattr backend calls.
	// These are the most frequently called FUSE operations; a single transient
	// failure should not return EIO to userspace.
	maxMetadataRetries = 2

	// backendErrorTTL is how long a backend error is considered active.
	// After this duration, hasBackendError() returns false even if no
	// successful request has cleared the error — this prevents stale
	// FS_ERROR.txt visibility after backend recovery.
	backendErrorTTL = 30 * time.Second

	// readdirTimeout is the timeout for a single Readdir backend RPC, used
	// when the FUSE request context has already been cancelled (FUSE_INTERRUPT)
	// but the caller (e.g. go mod verify) still needs a complete directory
	// listing to compute a correct hash. A fresh context with this timeout is
	// used for the retry so that one kernel interrupt does not silently
	// truncate a large dependency directory.
	readdirTimeout = 30 * time.Second
)

// retryDelay returns an exponential backoff duration with ±25% jitter
// to avoid thundering-herd retries when many FUSE operations fail
// simultaneously against the same backend.
func retryDelay(attempt int) time.Duration {
	base := time.Duration(50*(1<<uint(attempt))) * time.Millisecond // 50ms, 100ms, 200ms, ...
	jitter := time.Duration(float64(base) * (0.75 + rand.Float64()*0.5))
	return jitter
}

// attrTimeout returns the cache timeout duration for FUSE.
func attrTimeout() time.Duration {
	return cache.DefaultAttrTTL
}

// overlayEntryTimeout returns a short timeout for user-dir overlay content.
// Files under user root dirs (e.g. .deps/) change rapidly during downloads,
// so we use a much shorter cache duration than backend content.
func overlayEntryTimeout() time.Duration {
	return 1 * time.Second
}

// backendEntryTimeout returns a shorter timeout for backend entries when the
// filesystem is in writable mode. This ensures that after overlay mutations
// (create, delete, rename) the kernel re-validates cached backend entries
// quickly, while still providing reasonable caching for performance.
func backendEntryTimeout() time.Duration {
	return 5 * time.Second
}

// addWriteBits upgrades backend-reported file/directory permissions so the
// kernel allows write operations (Create, Unlink, Open(O_WRONLY), etc.) to
// reach the FUSE handlers. Without this, the kernel may reject mutations
// with EACCES before FUSE ever sees the request.
func addWriteBits(mode uint32) uint32 {
	if mode&uint32(syscall.S_IFDIR) != 0 {
		// Directories: ensure owner rwx (need write for create/unlink,
		// execute for traversal)
		return mode | 0700
	}
	// Regular files: add owner write
	return mode | 0200
}

// isDependencyPath returns true if the path is under the "dependency/" tree.
// Files in this tree are package-manager caches (e.g. Go module cache) that
// must preserve their original permissions (0444 for files, 0555 for dirs)
// so that tools like `go mod verify` compute the correct directory hash.
func isDependencyPath(path string) bool {
	const prefix = "dependency/"
	return path == "dependency" ||
		(len(path) > len(prefix) && path[:len(prefix)] == prefix)
}

// invalidateEntry invalidates the kernel dentry cache for a child name.
// This tells the kernel to forget any cached positive/negative lookup for
// this name, forcing it to re-issue a LOOKUP on the next access.
// Safe to call even when the node is not mounted in a FUSE tree (e.g. unit
// tests where the embedded Inode is zero-valued).
func (n *MonoNode) invalidateEntry(name string) {
	defer func() {
		if r := recover(); r != nil {
			// Inode not initialized (e.g. unit tests) — silently ignore
		}
	}()
	if inode := n.GetChild(name); inode != nil {
		n.RmChild(name)
	}
	n.NotifyEntry(name)
}

// lookupFromOverlay creates a child node using local overlay attributes.
// Returns (nil, ENOENT) if the local file doesn't exist on disk.
func (n *MonoNode) lookupFromOverlay(ctx context.Context, name, childPath string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	overlay := NewOverlayManager(n.sessionMgr)
	attr, err := overlay.GetLocalAttr(childPath)
	if err != nil {
		return nil, syscall.ENOENT
	}

	n.logger.Debug("lookup: using overlay", "path", childPath)
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
	out.SetEntryTimeout(overlayEntryTimeout())
	out.SetAttrTimeout(overlayEntryTimeout())

	stable := fs.StableAttr{Mode: attr.Mode, Ino: hashPathForNode(childPath)}
	return n.NewInode(ctx, child, stable), 0
}

// getattrFromOverlay populates attributes from local overlay file.
func (n *MonoNode) getattrFromOverlay(out *fuse.AttrOut) syscall.Errno {
	overlay := NewOverlayManager(n.sessionMgr)
	attr, err := overlay.GetLocalAttr(n.path)
	if err != nil {
		return syscall.ENOENT
	}

	n.logger.Debug("getattr: using overlay", "path", n.path)
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
	out.SetTimeout(overlayEntryTimeout())
	return 0
}

// recoverPanic catches panics and stores them as catastrophic errors.
// This must be safe to call even when the node is not mounted in a FUSE
// tree (e.g. during unit tests or early initialisation).
func (n *MonoNode) recoverPanic(operation string) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		errMsg := fmt.Sprintf("PANIC in %s: %v\n\nStack trace:\n%s", operation, r, string(stack))
		n.logger.Error("catastrophic error", "operation", operation, "panic", r)

		// Store error in root node (only if properly embedded in FUSE tree).
		// Check inode != nil BEFORE calling Parent() to avoid a nil-deref
		// inside the panic handler itself.
		inode := n.EmbeddedInode()
		if inode == nil {
			// Not embedded in FUSE tree — store on self
			n.catErrorMu.Lock()
			n.catastrophicError = errMsg
			n.catErrorMu.Unlock()
			return
		}

		_, parentInode := inode.Parent() // Parent() returns (name, *Inode)
		if parentInode != nil {
			root := n.Root()
			if rootNode, ok := root.Operations().(*MonoNode); ok {
				rootNode.catErrorMu.Lock()
				rootNode.catastrophicError = errMsg
				rootNode.catErrorMu.Unlock()
				return
			}
		}

		// Fallback: not in a tree or root cast failed — store on self
		// (covers root node before mount and edge cases)
		n.catErrorMu.Lock()
		n.catastrophicError = errMsg
		n.catErrorMu.Unlock()
	}
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

	// For paths under user root directories (e.g. .deps/), the backend
	// doesn't know about these files. Skip the backend fetch and create
	// an empty local file instead.
	parts := splitPath(monofsPath)
	isUnderUserDir := len(parts) > 1 && n.sessionMgr.IsUserRootDir(parts[0])

	if isUnderUserDir {
		n.logger.Debug("ensureLocalCopy: user root dir path, creating empty file", "path", monofsPath)
		if err := os.WriteFile(localPath, []byte{}, 0644); err != nil {
			return fmt.Errorf("create empty file: %w", err)
		}
	} else {
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
	}

	// NOTE: We intentionally do NOT call TrackChange here. The caller
	// (Open, Write, Setattr) is responsible for tracking the change at the
	// appropriate time. Calling it here caused a redundant NutsDB
	// transaction on the very first write to every file (double DB write).

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
