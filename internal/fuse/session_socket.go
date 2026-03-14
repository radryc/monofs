// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/radryc/monofs/internal/cache"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SessionSocketHandler handles session management requests via Unix socket
type SessionSocketHandler struct {
	socketPath string
	sessionMgr *SessionManager
	commitMgr  *CommitManager
	ingester   BlobIngester    // optional, nil if not configured
	deleter    BlobDeleter     // optional, nil if not configured
	diffReader DiffReader      // optional, for reading original file content
	verifier   BackendVerifier // optional, for verifying backend has files before cleanup
	attrCache  *cache.Cache    // optional, for invalidation after push
	rootNode   *MonoNode       // optional, for kernel dentry cache invalidation
	listener   net.Listener
	logger     *slog.Logger
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
}

// BlobIngester ingests dependency files into the cluster backend so they
// are visible on all nodes (served the same way as github.com repos).
type BlobIngester interface {
	IngestBlobs(ctx context.Context, files []BlobIngestFile) (*BlobIngestResult, error)
}

// BlobDeleter deletes dependency files from the cluster backend.
// Paths are relative to the dependency root (e.g. "go/mod/cache/...").
type BlobDeleter interface {
	DeleteBlobs(ctx context.Context, paths []string) (*BlobDeleteResult, error)
}

// BlobDeleterFunc is a convenience adapter that turns a plain function into
// a BlobDeleter implementation.
type BlobDeleterFunc func(ctx context.Context, paths []string) (*BlobDeleteResult, error)

// DeleteBlobs implements BlobDeleter.
func (f BlobDeleterFunc) DeleteBlobs(ctx context.Context, paths []string) (*BlobDeleteResult, error) {
	return f(ctx, paths)
}

// BlobDeleteResult summarises the cluster deletion.
type BlobDeleteResult struct {
	FilesDeleted int
	FilesFailed  int
}

// BackendVerifier verifies that files are accessible in the backend before
// removing overlay entries. This ensures atomic cleanup by confirming the
// backend has the data before overlay cleanup proceeds.
type BackendVerifier interface {
	// VerifyBlobs checks if the specified paths are accessible in the backend.
	// Returns true if all paths are verified, false otherwise.
	VerifyBlobs(ctx context.Context, paths []string) (bool, error)
}

// BackendVerifierFunc is a convenience adapter that turns a plain function
// into a BackendVerifier implementation.
type BackendVerifierFunc func(ctx context.Context, paths []string) (bool, error)

// VerifyBlobs implements BackendVerifier.
func (f BackendVerifierFunc) VerifyBlobs(ctx context.Context, paths []string) (bool, error) {
	return f(ctx, paths)
}

// DiffReader reads original file content from the cluster, bypassing the
// overlay layer. Used by the diff command to compare overlay vs original.
type DiffReader interface {
	ReadOriginal(ctx context.Context, path string) ([]byte, error)
}

// DiffReaderFunc is a convenience adapter that turns a plain function into
// a DiffReader implementation.
type DiffReaderFunc func(ctx context.Context, path string) ([]byte, error)

// ReadOriginal implements DiffReader.
func (f DiffReaderFunc) ReadOriginal(ctx context.Context, path string) ([]byte, error) {
	return f(ctx, path)
}

// BlobIngesterFunc is a convenience adapter that turns a plain function into
// a BlobIngester implementation.
type BlobIngesterFunc func(ctx context.Context, files []BlobIngestFile) (*BlobIngestResult, error)

// IngestBlobs implements BlobIngester.
func (f BlobIngesterFunc) IngestBlobs(ctx context.Context, files []BlobIngestFile) (*BlobIngestResult, error) {
	return f(ctx, files)
}

// BlobFileType mirrors packager.FileType values for the ingestion pipeline.
type BlobFileType = uint8

const (
	BlobFileRegular BlobFileType = 0 // regular file
	BlobFileDir     BlobFileType = 1 // directory (zero-byte content)
	BlobFileSymlink BlobFileType = 2 // symlink (resolved to content)
)

// BlobIngestFile describes a single file to ingest into the cluster.
type BlobIngestFile struct {
	Path     string // relative to dependency root, e.g. "go/mod/cache/..."
	Content  []byte
	Mode     uint32
	FileType BlobFileType // 0=regular, 1=dir, 2=symlink
}

// BlobIngestResult summarises the cluster ingestion.
type BlobIngestResult struct {
	FilesIngested int
	FilesFailed   int
	FailedFiles   []BlobFailedFile // per-file failure details (may be nil if all succeeded)
}

// BlobFailedFile describes a single file that failed to ingest and why.
type BlobFailedFile struct {
	Path   string
	Reason string
}

// BlobFileInfo describes a single dependency file.
type BlobFileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// SessionRequest is received from CLI
type SessionRequest struct {
	Action    string `json:"action"`         // start, status, commit, discard, push, blobs-info, diff
	Path      string `json:"path,omitempty"` // optional file path filter (for diff)
	ShowBlobs bool   `json:"show_blobs,omitempty"`
}

// SessionResponse is sent to CLI
type SessionResponse struct {
	Success        bool           `json:"success"`
	SessionID      string         `json:"session_id,omitempty"`
	CreatedAt      string         `json:"created_at,omitempty"`
	Changes        int            `json:"changes,omitempty"`
	BlobChanges    int            `json:"blob_changes,omitempty"`
	Message        string         `json:"message,omitempty"`
	Error          string         `json:"error,omitempty"`
	ChangeList     []ChangeInfo   `json:"change_list,omitempty"`
	BlobChangeList []ChangeInfo   `json:"blob_change_list,omitempty"`
	DepsInfo       *BlobsInfoData `json:"blobs_info,omitempty"`
	DiffData       []FileDiff     `json:"diff_data,omitempty"`
	BlobDiffData   []FileDiff     `json:"blob_diff_data,omitempty"`
}

// FileDiff contains the unified diff for a single file.
type FileDiff struct {
	Path       string `json:"path"`
	ChangeType string `json:"change_type"` // create, modify, delete
	Diff       string `json:"diff"`        // unified diff text
}

// BlobsInfoData contains dependency file information for the current session.
type BlobsInfoData struct {
	TotalFiles int             `json:"total_files"`
	TotalBytes int64           `json:"total_bytes"`
	Tools      []BlobsToolInfo `json:"tools"`
}

// BlobsToolInfo contains per-tool dependency information.
type BlobsToolInfo struct {
	Tool     string         `json:"tool"`
	Files    int            `json:"files"`
	Bytes    int64          `json:"bytes"`
	FileList []BlobFileInfo `json:"file_list,omitempty"`
}

// ChangeInfo represents a single change for display
type ChangeInfo struct {
	Type      string `json:"type"`
	Path      string `json:"path"`
	Timestamp string `json:"timestamp"`
}

// SetIngester attaches a dependency ingester so push pushes
// files to the cluster backend.
func (h *SessionSocketHandler) SetIngester(i BlobIngester) {
	h.ingester = i
}

// SetDeleter attaches a dependency deleter so push propagates
// file deletions to the cluster backend.
func (h *SessionSocketHandler) SetDeleter(d BlobDeleter) {
	h.deleter = d
}

// SetDiffReader attaches a reader for fetching original file content
// from the cluster. Required for the diff command.
func (h *SessionSocketHandler) SetDiffReader(dr DiffReader) {
	h.diffReader = dr
}

// SetAttrCache attaches a metadata cache so push can invalidate stale
// dependency entries after ingestion.
func (h *SessionSocketHandler) SetAttrCache(c *cache.Cache) {
	h.attrCache = c
}

// SetRootNode attaches the FUSE root node so push can invalidate the
// kernel's dentry cache after removing blob changes.
func (h *SessionSocketHandler) SetRootNode(n *MonoNode) {
	h.rootNode = n
}

// NewSessionSocketHandler creates a new socket handler
func NewSessionSocketHandler(overlayDir string, sessionMgr *SessionManager, commitMgr *CommitManager, logger *slog.Logger) (*SessionSocketHandler, error) {
	if logger == nil {
		logger = slog.Default()
	}

	socketPath := filepath.Join(overlayDir, "session.sock")

	// Remove existing socket
	os.Remove(socketPath)

	// Create socket
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	// Set permissions
	if err := os.Chmod(socketPath, 0600); err != nil {
		listener.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	handler := &SessionSocketHandler{
		socketPath: socketPath,
		sessionMgr: sessionMgr,
		commitMgr:  commitMgr,
		listener:   listener,
		logger:     logger.With("component", "session-socket"),
		ctx:        ctx,
		cancel:     cancel,
	}

	return handler, nil
}

// Start begins accepting connections
func (h *SessionSocketHandler) Start() {
	h.wg.Add(1)
	go h.acceptLoop()
	h.logger.Info("session socket started", "path", h.socketPath)
}

// Stop closes the socket and stops accepting connections
func (h *SessionSocketHandler) Stop() {
	h.cancel()
	h.listener.Close()
	h.wg.Wait()
	os.Remove(h.socketPath)
	h.logger.Info("session socket stopped")
}

func (h *SessionSocketHandler) acceptLoop() {
	defer h.wg.Done()

	for {
		conn, err := h.listener.Accept()
		if err != nil {
			select {
			case <-h.ctx.Done():
				return
			default:
				h.logger.Warn("accept error", "error", err)
				continue
			}
		}

		h.wg.Add(1)
		go h.handleConnection(conn)
	}
}

func (h *SessionSocketHandler) handleConnection(conn net.Conn) {
	defer h.wg.Done()
	defer conn.Close()

	// Read request
	var req SessionRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		h.logger.Warn("failed to decode request", "error", err)
		h.sendError(conn, "invalid request")
		return
	}

	h.logger.Debug("session request", "action", req.Action)

	// Handle action
	var resp SessionResponse
	switch req.Action {
	case "start":
		resp = h.handleStart()
	case "status":
		resp = h.handleStatus(req.ShowBlobs)
	case "commit":
		resp = h.handleCommit()
	case "discard":
		resp = h.handleDiscard()
	case "push", "push-blobs":
		resp = h.handleUploadDeps()
	case "blobs-info":
		resp = h.handleBlobsInfo()
	case "diff":
		resp = h.handleDiff(req.Path, req.ShowBlobs)
	default:
		resp = SessionResponse{
			Success: false,
			Error:   "unknown action: " + req.Action,
		}
	}

	// Send response
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		h.logger.Warn("failed to encode response", "error", err)
	}
}

func (h *SessionSocketHandler) handleStart() SessionResponse {
	session, err := h.sessionMgr.StartSession()
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}

	_, _, changeCount, _ := h.sessionMgr.GetSessionInfo()

	return SessionResponse{
		Success:   true,
		SessionID: session.ID,
		CreatedAt: session.CreatedAt.Format("2006-01-02 15:04:05"),
		Changes:   changeCount,
	}
}

func (h *SessionSocketHandler) handleStatus(showBlobs bool) SessionResponse {
	id, createdAt, changeCount, ok := h.sessionMgr.GetSessionInfo()
	if !ok {
		return SessionResponse{
			Success: false,
			Error:   "no active session",
		}
	}

	// Get change list and separate deps from other files
	changes := h.sessionMgr.GetChanges()
	var changeList []ChangeInfo
	var depChangeList []ChangeInfo
	depCount := 0
	const depPrefix = "dependency/"

	for _, c := range changes {
		isDep := len(c.Path) >= len(depPrefix) && c.Path[:len(depPrefix)] == depPrefix
		if isDep {
			depCount++
			if showBlobs {
				depChangeList = append(depChangeList, ChangeInfo{
					Type:      string(c.Type),
					Path:      c.Path,
					Timestamp: c.Timestamp.Format("15:04:05"),
				})
			}
			continue // don't list individual dep files in default status list
		}
		// also consider the 'dependency' directory itself as a dependency
		if c.Path == "dependency" {
			depCount++
			if showBlobs {
				depChangeList = append(depChangeList, ChangeInfo{
					Type:      string(c.Type),
					Path:      c.Path,
					Timestamp: c.Timestamp.Format("15:04:05"),
				})
			}
			continue
		}

		changeList = append(changeList, ChangeInfo{
			Type:      string(c.Type),
			Path:      c.Path,
			Timestamp: c.Timestamp.Format("15:04:05"),
		})
	}

	return SessionResponse{
		Success:        true,
		SessionID:      id,
		CreatedAt:      createdAt.Format("2006-01-02 15:04:05"),
		Changes:        changeCount - depCount,
		BlobChanges:    depCount,
		ChangeList:     changeList,
		BlobChangeList: depChangeList,
	}
}

func (h *SessionSocketHandler) handleCommit() SessionResponse {
	if h.commitMgr == nil {
		return SessionResponse{
			Success: false,
			Error:   "commit manager not available",
		}
	}

	result, err := h.commitMgr.CommitChanges(h.ctx)
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}

	message := "No changes to commit"
	if result.FilesProcessed > 0 {
		message = formatCommitMessage(result)
	}

	return SessionResponse{
		Success:   result.Success,
		SessionID: result.SessionID,
		Message:   message,
	}
}

func (h *SessionSocketHandler) handleDiscard() SessionResponse {
	err := h.sessionMgr.DiscardSession()
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}

	return SessionResponse{
		Success: true,
		Message: "Session discarded successfully",
	}
}

func (h *SessionSocketHandler) handleUploadDeps() SessionResponse {
	if h.ingester == nil {
		return SessionResponse{
			Success: false,
			Error:   "dependency ingestion not configured",
		}
	}

	session := h.sessionMgr.GetCurrentSession()
	if session == nil {
		return SessionResponse{
			Success: false,
			Error:   "no active session",
		}
	}

	h.logger.Info("DEBUG_RACE: handleUploadDeps started", "session", session.ID)

	// Collect files to ingest (creates/modifies) into the cluster backend.
	ingestFiles, readErr := h.collectBlobFiles()
	if readErr != nil {
		h.logger.Error("failed to collect dep files for ingestion", "error", readErr)
		return SessionResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to collect dep files: %v", readErr),
		}
	}
	h.logger.Info("DEBUG_RACE: collected files for ingestion", "count", len(ingestFiles))

	// Collect deleted dependency paths to propagate to the backend.
	deletedPaths := h.sessionMgr.GetDeletedBlobPaths()
	if len(deletedPaths) > 0 {
		h.logger.Info("DEBUG_RACE: collected deleted paths", "count", len(deletedPaths))
	}

	if len(ingestFiles) == 0 && len(deletedPaths) == 0 {
		return SessionResponse{
			Success:   true,
			SessionID: session.ID,
			Message:   "no dependency files to ingest",
		}
	}

	var messages []string

	// Ingest new/modified files.
	if len(ingestFiles) > 0 {
		result, err := h.ingester.IngestBlobs(h.ctx, ingestFiles)
		if err != nil {
			h.logger.Error("dep ingestion failed", "error", err)
			return SessionResponse{
				Success: false,
				Error:   fmt.Sprintf("dep ingestion failed: %v", err),
			}
		}

		msg := fmt.Sprintf("cluster: %d files ingested, %d failed", result.FilesIngested, result.FilesFailed)
		// Include per-file failure reasons (cap at 20 to avoid huge responses)
		for i, ff := range result.FailedFiles {
			if i >= 20 {
				msg += fmt.Sprintf("\n  ... and %d more failures", len(result.FailedFiles)-20)
				break
			}
			msg += fmt.Sprintf("\n  FAILED %s: %s", ff.Path, ff.Reason)
		}
		messages = append(messages, msg)
	}

	// Delete removed files from the backend.
	if len(deletedPaths) > 0 && h.deleter != nil {
		delResult, err := h.deleter.DeleteBlobs(h.ctx, deletedPaths)
		if err != nil {
			h.logger.Warn("dep deletion partially failed", "error", err)
			messages = append(messages, fmt.Sprintf("cluster: deletion error: %v", err))
		} else {
			messages = append(messages, fmt.Sprintf("cluster: %d files deleted, %d failed",
				delResult.FilesDeleted, delResult.FilesFailed))
		}
	} else if len(deletedPaths) > 0 {
		h.logger.Warn("dependency deletions not propagated: no deleter configured",
			"count", len(deletedPaths))
		messages = append(messages, fmt.Sprintf("warning: %d deletions not propagated (no deleter configured)", len(deletedPaths)))
	}

	h.logger.Info("DEBUG_RACE: about to call handleRemoveBlobChanges", "time_since_start_ms", time.Since(time.Now()).Milliseconds())

	// Remove dep entries from the overlay so the session is clean
	// and only retains non-dependency changes.
	h.handleRemoveBlobChanges()

	h.logger.Info("DEBUG_RACE: handleUploadDeps completed")

	return SessionResponse{
		Success:   true,
		SessionID: session.ID,
		Changes:   len(ingestFiles) + len(deletedPaths),
		Message:   strings.Join(messages, "; "),
	}
}

// handleRemoveBlobChanges cleans up dependency entries from the overlay after push.
// It also invalidates cached attrs/dirs for dependency paths so that subsequent
// reads fetch fresh metadata from the backend (with correct permissions).
//
// The order is critical for correctness:
//
//  1. Optional verification — verify backend has files before cleanup (atomic cleanup)
//  2. DB cleanup — overlay entries removed so Lookup/Readdir stop resolving
//     to the overlay. New FUSE requests now fall through to the backend.
//  3. Attr cache invalidation — cached attributes cleared so Getattr
//     re-fetches from the backend with correct (read-only) permissions.
//  4. Kernel dentry invalidation — NotifyEntry for the entire dependency
//     subtree forces the kernel to forget its dentry cache. After this,
//     all in-flight FUSE ops for dependency/ paths have completed and no
//     new ones will reference overlay files.
//  5. Disk cleanup — bulk-remove the dependency/ directory tree from the
//     overlay session. Safe because no kernel references remain.
//  6. Mark push timestamp — enables DIRECT_IO bypass for stale page cache.
func (h *SessionSocketHandler) handleRemoveBlobChanges() {
	// Phase 0: Optional verification - ensure backend has the files before cleanup.
	// This provides atomic cleanup semantics - if verification fails, we don't
	// remove overlay entries, preventing the "file not found" race condition.
	if h.verifier != nil && h.sessionMgr != nil {
		// Get a sample of dependency files to verify
		depFiles := h.sessionMgr.GetDependencyFilePaths(10) // Sample up to 10 files
		if len(depFiles) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			verified, verifyErr := h.verifier.VerifyBlobs(ctx, depFiles)
			cancel()
			if verifyErr != nil {
				h.logger.Warn("backend verification failed, skipping cleanup",
					"error", verifyErr, "sample_size", len(depFiles))
				return
			}
			if !verified {
				h.logger.Warn("backend verification returned false, skipping cleanup",
					"sample_size", len(depFiles))
				return
			}
			h.logger.Info("backend verification passed, proceeding with cleanup",
				"sample_size", len(depFiles))
		}
	}

	// Phase 1: Remove overlay DB entries (no disk I/O on the dep files).
	removed, err := h.sessionMgr.RemoveBlobChanges()
	if err != nil {
		h.logger.Warn("failed to remove dep changes after push", "error", err)
	} else if removed > 0 {
		h.logger.Info("removed dep DB entries after push", "removed", removed)
	}

	// Phase 2: Invalidate cached attrs/dirs for dependency paths so the
	// FUSE layer re-fetches from backend with the correct permissions.
	if h.attrCache != nil {
		n := h.attrCache.InvalidatePrefix("dependency")
		if n > 0 {
			h.logger.Info("invalidated dependency cache entries after push", "count", n)
		}
	}

	// Phase 3: Invalidate the kernel's dentry cache for the entire
	// dependency subtree. This walks the go-fuse inode tree depth-first,
	// calling RmChild + NotifyEntry on every cached child. After this
	// returns, the kernel will re-issue Lookup on the next access.
	if h.rootNode != nil {
		h.invalidateDependencyTree()
		h.logger.Info("invalidated kernel dentry cache for dependency tree after push")
	}

	// Phase 4: Now that no kernel dentries reference overlay files, it is
	// safe to bulk-remove the dependency/ subtree from disk.
	if h.sessionMgr != nil {
		h.sessionMgr.RemoveBlobDisk()
	}

	// Phase 5: Record the push timestamp so Open() can force
	// FOPEN_DIRECT_IO for dependency/ paths, bypassing any stale kernel
	// page cache content that was cached from the pre-push overlay.
	if h.sessionMgr != nil {
		h.sessionMgr.MarkDepsPushed()
	}
}

// invalidateDependencyTree walks the FUSE inode tree under the root node
// and removes all cached children whose path starts with "dependency".
// This forces the kernel to re-issue Lookup on the next access.
func (h *SessionSocketHandler) invalidateDependencyTree() {
	if h.rootNode == nil {
		return
	}

	// First invalidate the top-level entry.
	h.rootNode.invalidateEntry("dependency")

	// Then walk the inode tree and evict all children recursively.
	// go-fuse's Inode exposes Children() to iterate cached child inodes.
	var walkAndInvalidate func(parent *fs.Inode)
	walkAndInvalidate = func(parent *fs.Inode) {
		if parent == nil {
			return
		}
		// Snapshot children to avoid mutating during iteration.
		type child struct {
			name  string
			inode *fs.Inode
		}
		var children []child

		// ForgetPersistent + RmChild for each child.
		// Inode.Children() is the documented way to iterate.
		for name, ch := range parent.Children() {
			children = append(children, child{name, ch})
		}

		for _, c := range children {
			// Recurse first (depth-first) so leaves are evicted before parents.
			walkAndInvalidate(c.inode)
			parent.RmChild(c.name)
			// NotifyEntry tells the kernel to forget its dentry cache for this name.
			parent.NotifyEntry(c.name)
		}
	}

	// Find the "dependency" child inode under root.
	defer func() {
		if r := recover(); r != nil {
			// Inode not mounted (e.g. unit tests) — silently ignore.
			h.logger.Debug("invalidateDependencyTree: recovered panic", "panic", r)
		}
	}()

	rootInode := h.rootNode.EmbeddedInode()
	if rootInode == nil {
		return
	}
	depInode := rootInode.GetChild("dependency")
	if depInode == nil {
		return
	}
	walkAndInvalidate(depInode)
	rootInode.RmChild("dependency")
	rootInode.NotifyEntry("dependency")
}

// handleDiff computes unified diffs for changed files in the current session.
// If filterPath is non-empty, only the matching file is diffed.
func (h *SessionSocketHandler) handleDiff(filterPath string, showBlobs bool) SessionResponse {
	if h.diffReader == nil {
		return SessionResponse{
			Success: false,
			Error:   "diff not available (no cluster reader configured)",
		}
	}

	session := h.sessionMgr.GetCurrentSession()
	if session == nil {
		return SessionResponse{
			Success: false,
			Error:   "no active session",
		}
	}

	changes := h.sessionMgr.GetChanges()
	if len(changes) == 0 {
		return SessionResponse{
			Success:   true,
			SessionID: session.ID,
			Message:   "no changes to diff",
		}
	}

	var diffs []FileDiff
	ctx := h.ctx

	for _, c := range changes {
		// Skip non-file changes (mkdir, rmdir, symlink, etc.)
		switch c.Type {
		case ChangeCreate, ChangeModify, ChangeDelete:
			// These are diffable
		default:
			continue
		}

		// Apply path filter if specified
		if filterPath != "" && c.Path != filterPath {
			continue
		}

		fd := FileDiff{
			Path:       c.Path,
			ChangeType: string(c.Type),
		}

		switch c.Type {
		case ChangeCreate:
			// New file — diff against empty
			newContent, err := os.ReadFile(c.LocalPath)
			if err != nil {
				h.logger.Warn("diff: cannot read new file", "path", c.Path, "error", err)
				fd.Diff = fmt.Sprintf("(cannot read new file: %v)", err)
			} else {
				var header string
				header = fmt.Sprintf("diff --git a/%s b/%s\nnew file mode 100644\n", c.Path, c.Path)
				diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
					A:        difflib.SplitLines(""),
					B:        difflib.SplitLines(string(newContent)),
					FromFile: "/dev/null",
					ToFile:   "b/" + c.Path,
					Context:  3,
				})
				fd.Diff = header + diff
			}

		case ChangeModify:
			// Modified file — diff original (cluster) vs overlay (local)
			origContent, err := h.diffReader.ReadOriginal(ctx, c.Path)
			if err != nil {
				if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
					// Original not on cluster — treat as a new file (create-style diff)
					newContent, readErr := os.ReadFile(c.LocalPath)
					if readErr != nil {
						fd.Diff = fmt.Sprintf("(cannot read file: %v)", readErr)
					} else {
						header := fmt.Sprintf("diff --git a/%s b/%s\nnew file mode 100644\n", c.Path, c.Path)
						diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
							A:        difflib.SplitLines(""),
							B:        difflib.SplitLines(string(newContent)),
							FromFile: "/dev/null",
							ToFile:   "b/" + c.Path,
							Context:  3,
						})
						fd.Diff = header + diff
						fd.ChangeType = string(ChangeCreate)
					}
				} else {
					h.logger.Warn("diff: cannot read original", "path", c.Path, "error", err)
					fd.Diff = fmt.Sprintf("(cannot read original from cluster: %v)", err)
				}
				diffs = append(diffs, fd)
				continue
			}

			newContent, err := os.ReadFile(c.LocalPath)
			if err != nil {
				h.logger.Warn("diff: cannot read overlay file", "path", c.Path, "error", err)
				fd.Diff = fmt.Sprintf("(cannot read overlay file: %v)", err)
				diffs = append(diffs, fd)
				continue
			}

			diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
				A:        difflib.SplitLines(string(origContent)),
				B:        difflib.SplitLines(string(newContent)),
				FromFile: "a/" + c.Path,
				ToFile:   "b/" + c.Path,
				Context:  3,
			})
			if diff == "" {
				// Content identical — not a real change (e.g. Go toolchain
				// extracting the same module the fetcher already serves).
				// Drop it from the diff output silently.
				continue
			}
			header := fmt.Sprintf("diff --git a/%s b/%s\n", c.Path, c.Path)
			fd.Diff = header + diff

		case ChangeDelete:
			// Deleted file — diff original against empty
			origContent, err := h.diffReader.ReadOriginal(ctx, c.Path)
			if err != nil {
				if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
					// File was never on the cluster (e.g. transient .tmp file)
					fd.Diff = "(deleted — original not available on cluster)"
				} else {
					h.logger.Warn("diff: cannot read deleted file original", "path", c.Path, "error", err)
					fd.Diff = fmt.Sprintf("(cannot read original from cluster: %v)", err)
				}
			} else {
				header := fmt.Sprintf("diff --git a/%s b/%s\ndeleted file mode 100644\n", c.Path, c.Path)
				diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
					A:        difflib.SplitLines(string(origContent)),
					B:        difflib.SplitLines(""),
					FromFile: "a/" + c.Path,
					ToFile:   "/dev/null",
					Context:  3,
				})
				fd.Diff = header + diff
			}
		}

		diffs = append(diffs, fd)
	}

	// Separate dependency diffs from non-dependency diffs
	var nonDepDiffs, depDiffs []FileDiff
	const depPfx = "dependency/"
	for _, fd := range diffs {
		isDep := (len(fd.Path) >= len(depPfx) && fd.Path[:len(depPfx)] == depPfx) || fd.Path == "dependency"
		if isDep {
			if showBlobs {
				depDiffs = append(depDiffs, fd)
			}
		} else {
			nonDepDiffs = append(nonDepDiffs, fd)
		}
	}

	if filterPath != "" && len(nonDepDiffs) == 0 && len(depDiffs) == 0 {
		return SessionResponse{
			Success: false,
			Error:   fmt.Sprintf("file not found in changes: %s", filterPath),
		}
	}

	return SessionResponse{
		Success:      true,
		SessionID:    session.ID,
		Changes:      len(nonDepDiffs),
		BlobChanges:  len(depDiffs),
		DiffData:     nonDepDiffs,
		BlobDiffData: depDiffs,
	}
}

// collectBlobFiles reads all overlay dependency entries and returns them as
// BlobIngestFile structs ready for cluster ingestion.
//
// Regular files   → ingested with their content.
// Symlinks        → resolved to the target file content and ingested as
//
//	regular files so the backend serves them transparently.
//
// Directories     → ingested as zero-byte entries with directory permission
//
//	bits so the backend recognises them as directories.
//
// Go module download cache ".info" files (e.g. v1.8.0.info under @v/) are
// skipped because they are ephemeral metadata that the Go toolchain
// recreates on demand. Their presence on the backend can cause go mod verify
// to attempt verification of partially-extracted modules.
func (h *SessionSocketHandler) collectBlobFiles() ([]BlobIngestFile, error) {
	blobFiles := h.sessionMgr.GetAllBlobFiles()
	if len(blobFiles) == 0 {
		return nil, nil
	}

	var files []BlobIngestFile
	for monofsPath, entry := range blobFiles {
		// monofsPath is "dependency/go/mod/cache/..." — strip the leading
		// "dependency/" prefix since IngestBlobs rebuilds it.
		relPath := monofsPath
		if len(relPath) > len("dependency/") {
			relPath = relPath[len("dependency/"):]
		}

		// Skip .info files in the Go module download cache. These are
		// ephemeral version-info JSON files created by `go mod download`
		// that the toolchain regenerates as needed. Ingesting them can
		// confuse `go mod verify` when the corresponding module directory
		// isn't fully extracted yet.
		if strings.HasSuffix(relPath, ".info") && strings.Contains(relPath, "/@v/") {
			h.logger.Debug("skipping .info file from ingestion", "path", relPath)
			continue
		}

		switch entry.Type {
		case FileEntryDir:
			// Empty directory marker — zero-byte content with dir permission.
			mode := entry.Mode
			if mode == 0 {
				mode = 0555 // default read-only for dependency dirs
			}
			files = append(files, BlobIngestFile{
				Path:     relPath,
				Content:  nil,
				Mode:     mode,
				FileType: BlobFileDir,
			})

		case FileEntrySymlink:
			// Resolve symlink to its target content. go mod verify
			// follows symlinks, so serving the resolved content keeps
			// the hash consistent.
			if entry.LocalPath == "" {
				continue
			}
			// os.ReadFile follows symlinks automatically
			content, err := os.ReadFile(entry.LocalPath)
			if err != nil {
				h.logger.Warn("skipping unreadable dep symlink",
					"path", monofsPath, "local", entry.LocalPath,
					"target", entry.SymlinkTarget, "error", err)
				continue
			}
			// Preserve the resolved target's permissions (typically 0444
			// for Go module cache files) so that the backend stores the
			// original read-only mode.
			var symlinkMode uint32 = 0644
			if info, err := os.Stat(entry.LocalPath); err == nil {
				symlinkMode = uint32(info.Mode().Perm())
			}
			files = append(files, BlobIngestFile{
				Path:     relPath,
				Content:  content,
				Mode:     symlinkMode,
				FileType: BlobFileSymlink,
			})

		default: // FileEntryRegular
			if entry.LocalPath == "" {
				continue
			}
			content, err := os.ReadFile(entry.LocalPath)
			if err != nil {
				h.logger.Warn("skipping unreadable dep file",
					"path", monofsPath, "local", entry.LocalPath, "error", err)
				continue
			}

			var mode uint32 = 0644
			if info, err := os.Stat(entry.LocalPath); err == nil {
				mode = uint32(info.Mode().Perm())
			}

			files = append(files, BlobIngestFile{
				Path:    relPath,
				Content: content,
				Mode:    mode,
			})
		}
	}

	return files, nil
}

func (h *SessionSocketHandler) handleBlobsInfo() SessionResponse {
	session := h.sessionMgr.GetCurrentSession()
	if session == nil {
		return SessionResponse{
			Success: false,
			Error:   "no active session",
		}
	}

	// Gather current dependency files from the overlay
	blobFiles := h.sessionMgr.GetAllBlobFiles()
	if len(blobFiles) == 0 {
		return SessionResponse{
			Success:   true,
			SessionID: session.ID,
			Message:   "No dependency files in current session",
			DepsInfo:  &BlobsInfoData{},
		}
	}

	// Group by tool: dependency/<tool>/rest
	type toolEntry struct {
		files []BlobFileInfo
		bytes int64
	}
	tools := make(map[string]*toolEntry)

	for monofsPath, entry := range blobFiles {
		parts := splitN(monofsPath, "/", 3) // ["dependency", "<tool>", "rest"]
		if len(parts) < 3 {
			continue
		}
		tool := parts[1]
		te, ok := tools[tool]
		if !ok {
			te = &toolEntry{}
			tools[tool] = te
		}

		var size int64
		if entry.Type != FileEntryDir {
			if info, err := os.Stat(entry.LocalPath); err == nil {
				size = info.Size()
			}
		}
		archiveName := monofsPath[len("dependency/"+tool+"/"):]
		te.files = append(te.files, BlobFileInfo{Path: archiveName, Size: size})
		te.bytes += size
	}

	info := &BlobsInfoData{}
	for tool, te := range tools {
		info.Tools = append(info.Tools, BlobsToolInfo{
			Tool:     tool,
			Files:    len(te.files),
			Bytes:    te.bytes,
			FileList: te.files,
		})
		info.TotalFiles += len(te.files)
		info.TotalBytes += te.bytes
	}

	return SessionResponse{
		Success:   true,
		SessionID: session.ID,
		Changes:   info.TotalFiles,
		DepsInfo:  info,
	}
}

// splitN is a simple helper that avoids importing strings just for SplitN.
func splitN(s, sep string, n int) []string {
	result := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		idx := -1
		for j := 0; j < len(s); j++ {
			if s[j] == sep[0] {
				idx = j
				break
			}
		}
		if idx < 0 {
			break
		}
		result = append(result, s[:idx])
		s = s[idx+1:]
	}
	result = append(result, s)
	return result
}

func (h *SessionSocketHandler) sendError(conn net.Conn, msg string) {
	resp := SessionResponse{
		Success: false,
		Error:   msg,
	}
	json.NewEncoder(conn).Encode(resp)
}

func formatCommitMessage(result *CommitResult) string {
	if result.FilesFailed > 0 {
		return fmt.Sprintf("Processed %d files: %d uploaded, %d failed",
			result.FilesProcessed, result.FilesUploaded, result.FilesFailed)
	}
	return fmt.Sprintf("Successfully processed %d files", result.FilesProcessed)
}
