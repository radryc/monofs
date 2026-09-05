// Package fuse implements the FUSE filesystem layer for MonoFS.
package fuse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/radryc/monofs/internal/cache"
	monoclient "github.com/radryc/monofs/internal/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SessionSocketHandler handles session management requests via Unix socket
type SessionSocketHandler struct {
	socketPath  string
	sessionMgr  *SessionManager
	commitMgr   *CommitManager
	principalID string
	ingester    BlobIngester // optional, nil if not configured
	deleter     BlobDeleter  // optional, nil if not configured
	refresher   WorkspaceRefresher
	diffReader  DiffReader      // optional, for reading original file content
	verifier    BackendVerifier // optional, for verifying backend has files before cleanup
	attrCache   *cache.Cache    // optional, for invalidation after push
	rootNode    *MonoNode       // optional, for kernel dentry cache invalidation
	listener    net.Listener
	logger      *slog.Logger
	wg          sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
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

type WorkspaceRefresher interface {
	RefreshWorkspaceRepositories(ctx context.Context, repos []monoclient.WorkspaceRepository) (*monoclient.WorkspaceRefreshResult, error)
}

type WorkspaceRefresherFunc func(ctx context.Context, repos []monoclient.WorkspaceRepository) (*monoclient.WorkspaceRefreshResult, error)

func (f WorkspaceRefresherFunc) RefreshWorkspaceRepositories(ctx context.Context, repos []monoclient.WorkspaceRepository) (*monoclient.WorkspaceRefreshResult, error) {
	return f(ctx, repos)
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
	Action                  string   `json:"action"`         // start, add, rm, status, branch, log, commit, pull, discard, push, push-blobs, blobs-info, diff
	Path                    string   `json:"path,omitempty"` // optional file path filter (for diff)
	Paths                   []string `json:"paths,omitempty"`
	BranchOp                string   `json:"branch_op,omitempty"`
	BranchName              string   `json:"branch_name,omitempty"`
	ShowBlobs               bool     `json:"show_blobs,omitempty"`
	LogicalCommitMessage    string   `json:"logical_commit_message,omitempty"`
	AuthorName              string   `json:"author_name,omitempty"`
	AuthorEmail             string   `json:"author_email,omitempty"`
	RequestedBranchStrategy string   `json:"requested_branch_strategy,omitempty"`
}

// SessionResponse is sent to CLI
type SessionResponse struct {
	Success           bool                `json:"success"`
	SessionID         string              `json:"session_id,omitempty"`
	CreatedAt         string              `json:"created_at,omitempty"`
	Changes           int                 `json:"changes,omitempty"`
	UnstagedChanges   int                 `json:"unstaged_changes,omitempty"`
	StagedChanges     int                 `json:"staged_changes,omitempty"`
	PendingCommits    int                 `json:"pending_commits,omitempty"`
	BlobChanges       int                 `json:"blob_changes,omitempty"`
	ExcludedChanges   int                 `json:"excluded_changes,omitempty"`
	Message           string              `json:"message,omitempty"`
	Error             string              `json:"error,omitempty"`
	ChangeList        []ChangeInfo        `json:"change_list,omitempty"`
	StagedChangeList  []ChangeInfo        `json:"staged_change_list,omitempty"`
	LocalCommitList   []LocalCommitInfo   `json:"local_commit_list,omitempty"`
	PendingCommitList []LocalCommitInfo   `json:"pending_commit_list,omitempty"`
	CurrentBranch     string              `json:"current_branch,omitempty"`
	BranchList        []BranchInfo        `json:"branch_list,omitempty"`
	BranchMappings    []BranchMappingInfo `json:"branch_mappings,omitempty"`
	BlobChangeList    []ChangeInfo        `json:"blob_change_list,omitempty"`
	WorkspaceRefs     []WorkspaceRef      `json:"workspace_refs,omitempty"`
	DepsInfo          *BlobsInfoData      `json:"blobs_info,omitempty"`
	DiffData          []FileDiff          `json:"diff_data,omitempty"`
	BlobDiffData      []FileDiff          `json:"blob_diff_data,omitempty"`
}

// FileDiff contains the unified diff for a single file.
type FileDiff struct {
	Path       string `json:"path"`
	ChangeType string `json:"change_type"` // create, modify, delete
	Repository string `json:"repository,omitempty"`
	StorageID  string `json:"storage_id,omitempty"`
	Diff       string `json:"diff"` // unified diff text
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
	Type       string `json:"type"`
	Path       string `json:"path"`
	Repository string `json:"repository,omitempty"`
	StorageID  string `json:"storage_id,omitempty"`
	Timestamp  string `json:"timestamp"`
}

// LocalCommitInfo is a lightweight status summary for a local virtual commit.
type LocalCommitInfo struct {
	ID              string `json:"id"`
	ParentID        string `json:"parent_id,omitempty"`
	Message         string `json:"message"`
	LogicalBranch   string `json:"logical_branch,omitempty"`
	AuthorName      string `json:"author_name,omitempty"`
	AuthorEmail     string `json:"author_email,omitempty"`
	PrincipalID     string `json:"principal_id,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	RepositoryCount int    `json:"repository_count,omitempty"`
	OperationCount  int    `json:"operation_count,omitempty"`
	Pushed          bool   `json:"pushed,omitempty"`
}

// BranchInfo summarizes one logical branch known to the current session.
type BranchInfo struct {
	Name           string `json:"name"`
	Current        bool   `json:"current,omitempty"`
	PendingCommits int    `json:"pending_commits,omitempty"`
	HasMappings    bool   `json:"has_mappings,omitempty"`
}

// BranchMappingInfo describes the current branch's per-repo remote mapping.
type BranchMappingInfo struct {
	DisplayPath      string `json:"display_path"`
	OriginalBranch   string `json:"original_branch,omitempty"`
	ActualBranch     string `json:"actual_branch,omitempty"`
	LastPushedCommit string `json:"last_pushed_commit,omitempty"`
}

// WorkspaceRef describes the authoritative tracked ref for one mounted repository.
type WorkspaceRef struct {
	DisplayPath string `json:"display_path"`
	Ref         string `json:"ref,omitempty"`
	CommitHash  string `json:"commit_hash,omitempty"`
}

type sessionChangeScope int

const (
	sessionChangeWorkspace sessionChangeScope = iota
	sessionChangeBlob
	sessionChangeExcluded
)

type classifiedSessionChange struct {
	Change
	Scope      sessionChangeScope
	Repository string
	StorageID  string
}

type removeTarget struct {
	Path              string
	LocalPath         string
	IsDir             bool
	ShouldTrackDelete bool
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

// SetWorkspaceRefresher attaches a workspace refresher so pull can re-ingest
// the visible source repositories through the router.
func (h *SessionSocketHandler) SetWorkspaceRefresher(r WorkspaceRefresher) {
	h.refresher = r
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

// SetPrincipalID records the mounted client identity used for branch scoping.
func (h *SessionSocketHandler) SetPrincipalID(principalID string) {
	h.principalID = strings.TrimSpace(principalID)
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
	case "add":
		resp = h.handleAdd(req.Paths)
	case "rm":
		resp = h.handleRemove(req.Paths)
	case "status":
		resp = h.handleStatus(req.ShowBlobs)
	case "branch":
		resp = h.handleBranch(req)
	case "refs":
		resp = h.handleRefs()
	case "log":
		resp = h.handleLog()
	case "commit":
		resp = h.handleCommit(req)
	case "pull", "refresh":
		resp = h.handlePull()
	case "discard":
		resp = h.handleDiscard()
	case "push":
		resp = h.handlePushSource()
	case "push-blobs":
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

func (h *SessionSocketHandler) handleAdd(paths []string) SessionResponse {
	if _, _, _, ok := h.sessionMgr.GetSessionInfo(); !ok {
		return SessionResponse{Success: false, Error: "no active session"}
	}
	normalizedPaths, err := normalizeRequestedSessionPaths(paths)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	if len(normalizedPaths) == 0 {
		return SessionResponse{Success: false, Error: "at least one path is required"}
	}

	changes := h.sessionMgr.GetChanges()
	classified := make([]classifiedSessionChange, 0, len(changes))
	for _, change := range changes {
		classified = append(classified, h.classifySessionChange(h.ctx, change))
	}

	selected, err := h.selectWorkspaceChangesForStaging(normalizedPaths, classified)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	if len(selected) == 0 {
		return SessionResponse{Success: false, Error: "no pending source changes to stage"}
	}

	stagedInfos := make([]ChangeInfo, 0, len(selected))
	for _, change := range selected {
		entry, err := h.snapshotStagedEntry(change)
		if err != nil {
			return SessionResponse{Success: false, Error: err.Error()}
		}
		if err := h.sessionMgr.PutStagedEntry(entry); err != nil {
			return SessionResponse{Success: false, Error: err.Error()}
		}
		stagedInfos = append(stagedInfos, changeInfoFromStagedEntry(entry))
	}
	sortChangeInfos(stagedInfos)

	return SessionResponse{
		Success:          true,
		Changes:          len(selected),
		StagedChanges:    len(selected),
		StagedChangeList: stagedInfos,
		Message:          fmt.Sprintf("staged %d source change(s)", len(selected)),
	}
}

func (h *SessionSocketHandler) handleRemove(paths []string) SessionResponse {
	normalizedPaths, err := normalizeRequestedSessionPaths(paths)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	if len(normalizedPaths) == 0 {
		return SessionResponse{Success: false, Error: "at least one path is required"}
	}

	if !h.sessionMgr.HasActiveSession() {
		if _, err := h.sessionMgr.StartSession(); err != nil {
			return SessionResponse{Success: false, Error: err.Error()}
		}
	}

	for _, path := range normalizedPaths {
		target, err := h.resolveRemoveTarget(path)
		if err != nil {
			return SessionResponse{Success: false, Error: err.Error()}
		}
		if target.IsDir {
			if err := h.removeDirectoryTarget(target); err != nil {
				return SessionResponse{Success: false, Error: err.Error()}
			}
		} else {
			if err := h.removeFileTarget(target); err != nil {
				return SessionResponse{Success: false, Error: err.Error()}
			}
		}
		if err := h.clearStagedEntriesForPath(path); err != nil {
			return SessionResponse{Success: false, Error: err.Error()}
		}
	}

	changes := h.sessionMgr.GetChanges()
	classified := make([]classifiedSessionChange, 0, len(changes))
	for _, change := range changes {
		classified = append(classified, h.classifySessionChange(h.ctx, change))
	}
	selected := collectWorkspaceChangesForPaths(normalizedPaths, classified)

	stagedInfos := make([]ChangeInfo, 0, len(selected))
	for _, change := range selected {
		entry, err := h.snapshotStagedEntry(change)
		if err != nil {
			return SessionResponse{Success: false, Error: err.Error()}
		}
		if err := h.sessionMgr.PutStagedEntry(entry); err != nil {
			return SessionResponse{Success: false, Error: err.Error()}
		}
		stagedInfos = append(stagedInfos, changeInfoFromStagedEntry(entry))
	}
	sortChangeInfos(stagedInfos)

	message := fmt.Sprintf("removed %d path(s); staged %d source change(s)", len(normalizedPaths), len(stagedInfos))
	if len(stagedInfos) == 0 {
		message = fmt.Sprintf("removed %d path(s); no source changes remain", len(normalizedPaths))
	}

	return SessionResponse{
		Success:          true,
		Changes:          len(normalizedPaths),
		StagedChanges:    len(stagedInfos),
		StagedChangeList: stagedInfos,
		Message:          message,
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
	id, createdAt, _, ok := h.sessionMgr.GetSessionInfo()
	if !ok {
		return SessionResponse{
			Success: false,
			Error:   "no active session",
		}
	}

	// Get change list and separate workspace-visible, dependency, and excluded files.
	changes := h.sessionMgr.GetChanges()
	classified := make([]classifiedSessionChange, 0, len(changes))
	for _, c := range changes {
		classified = append(classified, h.classifySessionChange(h.ctx, c))
	}
	sortClassifiedSessionChanges(classified)

	stagedEntries, err := h.sessionMgr.ListStagedEntries()
	if err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("list staged entries: %v", err)}
	}
	stagedByPath := make(map[string]StagedIndexEntry, len(stagedEntries))
	for _, entry := range stagedEntries {
		stagedByPath[entry.Path] = entry
	}

	localCommits, err := h.sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("list local commits: %v", err)}
	}

	var changeList []ChangeInfo
	stagedChangeList := make([]ChangeInfo, 0, len(stagedEntries))
	pendingCommitList := make([]LocalCommitInfo, 0)
	var depChangeList []ChangeInfo
	excludedCount := 0
	depCount := 0
	workspaceCount := 0

	for _, c := range classified {
		switch c.Scope {
		case sessionChangeExcluded:
			excludedCount++
			continue
		case sessionChangeBlob:
			depCount++
			if showBlobs {
				depChangeList = append(depChangeList, ChangeInfo{
					Type:       string(c.Type),
					Path:       c.Path,
					Repository: c.Repository,
					StorageID:  c.StorageID,
					Timestamp:  c.Timestamp.Format("15:04:05"),
				})
			}
			continue
		}

		workspaceCount++
		if staged, ok := stagedByPath[c.Path]; ok {
			currentSnapshot, snapshotErr := h.snapshotStagedEntry(c)
			if snapshotErr == nil && stagedEntriesEqual(staged, currentSnapshot) {
				continue
			}
		}

		changeList = append(changeList, ChangeInfo{
			Type:       string(c.Type),
			Path:       c.Path,
			Repository: c.Repository,
			StorageID:  c.StorageID,
			Timestamp:  c.Timestamp.Format("15:04:05"),
		})
	}

	for _, staged := range stagedEntries {
		stagedChangeList = append(stagedChangeList, changeInfoFromStagedEntry(staged))
	}
	sortChangeInfos(stagedChangeList)

	for _, commit := range localCommits {
		if commit.Pushed {
			continue
		}
		pendingCommitList = append(pendingCommitList, localCommitInfoFromCommit(commit))
	}
	sortLocalCommitInfos(pendingCommitList)

	return SessionResponse{
		Success:           true,
		SessionID:         id,
		CreatedAt:         createdAt.Format("2006-01-02 15:04:05"),
		Changes:           workspaceCount,
		UnstagedChanges:   len(changeList),
		StagedChanges:     len(stagedChangeList),
		PendingCommits:    len(pendingCommitList),
		BlobChanges:       depCount,
		ExcludedChanges:   excludedCount,
		ChangeList:        changeList,
		StagedChangeList:  stagedChangeList,
		PendingCommitList: pendingCommitList,
		BlobChangeList:    depChangeList,
	}
}

func (h *SessionSocketHandler) handleRefs() SessionResponse {
	if h.rootNode == nil || h.rootNode.WorkspaceManifest() == nil {
		return SessionResponse{
			Success: false,
			Error:   "workspace ref info requires a virtual-monorepo mount",
		}
	}

	entries, err := h.rootNode.WorkspaceManifest().List(h.ctx)
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   fmt.Sprintf("list workspace repositories: %v", err),
		}
	}

	refs := make([]WorkspaceRef, 0, len(entries))
	for _, entry := range entries {
		if !entry.Included {
			continue
		}
		refs = append(refs, WorkspaceRef{
			DisplayPath: entry.Repository.DisplayPath,
			Ref:         entry.Repository.Ref,
			CommitHash:  entry.Repository.CommitHash,
		})
	}

	return SessionResponse{
		Success:       true,
		WorkspaceRefs: refs,
	}
}

func (h *SessionSocketHandler) handleBranch(req SessionRequest) SessionResponse {
	branchOp := strings.TrimSpace(req.BranchOp)
	if branchOp == "" {
		branchOp = "show"
	}

	switch branchOp {
	case "show":
		return h.handleBranchShow()
	case "create":
		return h.handleBranchCreate(req.BranchName)
	case "switch":
		return h.handleBranchSwitch(req.BranchName)
	default:
		return SessionResponse{Success: false, Error: fmt.Sprintf("unknown branch operation: %s", branchOp)}
	}
}

func (h *SessionSocketHandler) handleBranchShow() SessionResponse {
	id, createdAt, _, ok := h.sessionMgr.GetSessionInfo()
	if !ok {
		return SessionResponse{Success: false, Error: "no active session"}
	}

	currentBranch, foundCurrentBranch, err := h.sessionMgr.GetCurrentLogicalBranch()
	if err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("read current logical branch: %v", err)}
	}
	if !foundCurrentBranch {
		currentBranch = ""
	}

	branchList, mappings, err := h.collectLogicalBranchState(currentBranch)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}

	return SessionResponse{
		Success:        true,
		SessionID:      id,
		CreatedAt:      createdAt.Format("2006-01-02 15:04:05"),
		CurrentBranch:  currentBranch,
		BranchList:     branchList,
		BranchMappings: mappings,
	}
}

func (h *SessionSocketHandler) handleBranchCreate(rawBranchName string) SessionResponse {
	branchName, err := normalizeLogicalBranchName(rawBranchName)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}

	currentBranch, foundCurrentBranch, err := h.sessionMgr.GetCurrentLogicalBranch()
	if err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("read current logical branch: %v", err)}
	}
	if !foundCurrentBranch {
		currentBranch = ""
	}

	knownBranches, _, err := h.collectLogicalBranchState(currentBranch)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	for _, branch := range knownBranches {
		if branch.Name == branchName {
			return SessionResponse{Success: false, Error: fmt.Sprintf("logical branch %q already exists", branchName)}
		}
	}

	if err := h.seedLogicalBranchMappings(branchName); err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	if err := h.sessionMgr.SetCurrentLogicalBranch(branchName); err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("set current logical branch: %v", err)}
	}

	return SessionResponse{
		Success:       true,
		CurrentBranch: branchName,
		Message:       fmt.Sprintf("created and switched to logical branch %s; working tree and index stay unchanged", branchName),
	}
}

func (h *SessionSocketHandler) handleBranchSwitch(rawBranchName string) SessionResponse {
	branchName, err := normalizeLogicalBranchName(rawBranchName)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}

	currentBranch, foundCurrentBranch, err := h.sessionMgr.GetCurrentLogicalBranch()
	if err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("read current logical branch: %v", err)}
	}
	if !foundCurrentBranch {
		currentBranch = ""
	}

	knownBranches, _, err := h.collectLogicalBranchState(currentBranch)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}
	exists := false
	for _, branch := range knownBranches {
		if branch.Name == branchName {
			exists = true
			break
		}
	}
	if !exists {
		return SessionResponse{Success: false, Error: fmt.Sprintf("logical branch %q does not exist; use 'monofs-session branch create %s' first", branchName, branchName)}
	}
	if currentBranch == branchName {
		return SessionResponse{Success: true, CurrentBranch: branchName, Message: fmt.Sprintf("already on logical branch %s", branchName)}
	}

	if err := h.sessionMgr.SetCurrentLogicalBranch(branchName); err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("set current logical branch: %v", err)}
	}

	return SessionResponse{
		Success:       true,
		CurrentBranch: branchName,
		Message:       fmt.Sprintf("switched to logical branch %s; working tree and index stay unchanged", branchName),
	}
}

func (h *SessionSocketHandler) handleCommit(req SessionRequest) SessionResponse {
	if h.commitMgr == nil {
		return SessionResponse{
			Success: false,
			Error:   "commit manager not available",
		}
	}

	result, err := h.commitMgr.CommitChanges(h.ctx, CommitOptions{
		LogicalCommitMessage:    req.LogicalCommitMessage,
		AuthorName:              req.AuthorName,
		AuthorEmail:             req.AuthorEmail,
		RequestedBranchStrategy: req.RequestedBranchStrategy,
	})
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}

	message := strings.TrimSpace(result.Message)
	if message == "" {
		message = fmt.Sprintf("created local commit %s with %d staged change(s)", result.LocalCommitID, result.FilesProcessed)
		if result.Repositories > 0 {
			message += fmt.Sprintf(" across %d repositories", result.Repositories)
		}
	}

	return SessionResponse{
		Success:   result.Success,
		SessionID: result.SessionID,
		Message:   message,
	}
}

func (h *SessionSocketHandler) handlePushSource() SessionResponse {
	if h.commitMgr == nil {
		return SessionResponse{Success: false, Error: "commit manager not available"}
	}

	result, err := h.commitMgr.PushPendingLocalCommits(h.ctx)
	if err != nil {
		return SessionResponse{Success: false, Error: err.Error()}
	}

	return SessionResponse{
		Success:       result.Success,
		SessionID:     result.SessionID,
		CurrentBranch: result.LogicalBranch,
		Message:       result.Message,
	}
}

func (h *SessionSocketHandler) handleLog() SessionResponse {
	id, createdAt, _, ok := h.sessionMgr.GetSessionInfo()
	if !ok {
		return SessionResponse{
			Success: false,
			Error:   "no active session",
		}
	}

	localCommits, err := h.sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("list local commits: %v", err)}
	}

	commitList := make([]LocalCommitInfo, 0, len(localCommits))
	for _, commit := range localCommits {
		commitList = append(commitList, localCommitInfoFromCommit(commit))
	}
	sortLocalCommitInfosNewestFirst(commitList)

	return SessionResponse{
		Success:         true,
		SessionID:       id,
		CreatedAt:       createdAt.Format("2006-01-02 15:04:05"),
		LocalCommitList: commitList,
	}
}

func (h *SessionSocketHandler) handlePull() SessionResponse {
	if h.refresher == nil {
		return SessionResponse{
			Success: false,
			Error:   "workspace refresh is not available",
		}
	}
	if h.rootNode == nil || h.rootNode.WorkspaceManifest() == nil {
		return SessionResponse{
			Success: false,
			Error:   "workspace refresh requires virtual monorepo mode",
		}
	}
	if len(h.sessionMgr.GetChanges()) > 0 {
		return SessionResponse{
			Success: false,
			Error:   "local changes pending; commit, discard, or push dependency changes before pull",
		}
	}
	localCommits, err := h.sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		return SessionResponse{Success: false, Error: fmt.Sprintf("list local commits: %v", err)}
	}
	for _, commit := range localCommits {
		if !commit.Pushed {
			return SessionResponse{
				Success: false,
				Error:   "local commits pending; push source commits before pull",
			}
		}
	}

	repos, err := h.collectPullRepositories(h.ctx)
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}
	if len(repos) == 0 {
		return SessionResponse{
			Success: true,
			Message: "No workspace repositories to refresh",
		}
	}

	result, err := h.refresher.RefreshWorkspaceRepositories(h.ctx, repos)
	if result != nil && result.Refreshed > 0 {
		h.invalidateWorkspaceAfterPull(repos)
	}
	if err != nil {
		return SessionResponse{
			Success: false,
			Error:   err.Error(),
		}
	}

	return SessionResponse{
		Success: true,
		Changes: result.Refreshed,
		Message: h.appendWorkspaceGitSyncWarning(formatWorkspacePullMessage(result), result != nil && result.Refreshed > 0),
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
	excludedCount := 0
	ctx := h.ctx

	for _, c := range changes {
		classified := h.classifySessionChange(ctx, c)

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
		if classified.Scope == sessionChangeExcluded {
			excludedCount++
			continue
		}

		fd := FileDiff{
			Path:       c.Path,
			ChangeType: string(c.Type),
			Repository: classified.Repository,
			StorageID:  classified.StorageID,
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
	for _, fd := range diffs {
		if isDependencyPath(fd.Path) {
			if showBlobs {
				depDiffs = append(depDiffs, fd)
			}
		} else {
			nonDepDiffs = append(nonDepDiffs, fd)
		}
	}
	sortFileDiffs(nonDepDiffs)
	sortFileDiffs(depDiffs)

	if filterPath != "" && len(nonDepDiffs) == 0 && len(depDiffs) == 0 {
		return SessionResponse{
			Success: false,
			Error:   fmt.Sprintf("file not found in changes: %s", filterPath),
		}
	}

	return SessionResponse{
		Success:         true,
		SessionID:       session.ID,
		Changes:         len(nonDepDiffs),
		BlobChanges:     len(depDiffs),
		ExcludedChanges: excludedCount,
		DiffData:        nonDepDiffs,
		BlobDiffData:    depDiffs,
	}
}

func (h *SessionSocketHandler) classifySessionChange(ctx context.Context, change Change) classifiedSessionChange {
	classified := classifiedSessionChange{Change: change}
	if isDependencyPath(change.Path) {
		classified.Scope = sessionChangeBlob
	}

	if h.rootNode == nil || h.rootNode.workspace == nil {
		if classified.Scope != sessionChangeBlob {
			classified.Scope = sessionChangeWorkspace
		}
		return classified
	}

	resolution, err := h.rootNode.workspace.ResolvePath(ctx, change.Path)
	if err != nil {
		h.logger.Warn("workspace resolve failed for session change", "path", change.Path, "error", err)
		if classified.Scope == sessionChangeBlob {
			return classified
		}
		if h.rootNode.workspace.ShouldHidePath(change.Path) {
			classified.Scope = sessionChangeExcluded
			return classified
		}
		classified.Scope = sessionChangeWorkspace
		return classified
	}

	if resolution.Repository != nil {
		classified.Repository = resolution.Repository.DisplayPath
		classified.StorageID = resolution.Repository.StorageID
	}
	if !resolution.Included {
		if classified.Scope == sessionChangeBlob {
			return classified
		}
		classified.Scope = sessionChangeExcluded
		return classified
	}

	classified.Scope = sessionChangeWorkspace
	return classified
}

func sortClassifiedSessionChanges(changes []classifiedSessionChange) {
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Scope != changes[j].Scope {
			return changes[i].Scope < changes[j].Scope
		}
		if changes[i].Repository != changes[j].Repository {
			return changes[i].Repository < changes[j].Repository
		}
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Type < changes[j].Type
	})
}

func sortChangeInfos(changes []ChangeInfo) {
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Repository != changes[j].Repository {
			return changes[i].Repository < changes[j].Repository
		}
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Type < changes[j].Type
	})
}

func sortLocalCommitInfos(commits []LocalCommitInfo) {
	sort.Slice(commits, func(i, j int) bool {
		if commits[i].CreatedAt != commits[j].CreatedAt {
			return commits[i].CreatedAt < commits[j].CreatedAt
		}
		return commits[i].ID < commits[j].ID
	})
}

func sortLocalCommitInfosNewestFirst(commits []LocalCommitInfo) {
	sort.Slice(commits, func(i, j int) bool {
		if commits[i].CreatedAt != commits[j].CreatedAt {
			return commits[i].CreatedAt > commits[j].CreatedAt
		}
		return commits[i].ID > commits[j].ID
	})
}

func sortBranchInfos(branches []BranchInfo) {
	sort.Slice(branches, func(i, j int) bool {
		if branches[i].Current != branches[j].Current {
			return branches[i].Current
		}
		return branches[i].Name < branches[j].Name
	})
}

func sortBranchMappingInfos(mappings []BranchMappingInfo) {
	sort.Slice(mappings, func(i, j int) bool {
		return mappings[i].DisplayPath < mappings[j].DisplayPath
	})
}

func changeInfoFromStagedEntry(entry StagedIndexEntry) ChangeInfo {
	return ChangeInfo{
		Type:       string(entry.ChangeType),
		Path:       entry.Path,
		Repository: entry.RepositoryPath,
		StorageID:  entry.RepositoryStorageID,
		Timestamp:  entry.StagedAt.Format("15:04:05"),
	}
}

func localCommitInfoFromCommit(commit LocalVirtualCommit) LocalCommitInfo {
	return LocalCommitInfo{
		ID:              commit.ID,
		ParentID:        commit.ParentID,
		Message:         commit.Message,
		LogicalBranch:   commit.LogicalBranch,
		AuthorName:      commit.AuthorName,
		AuthorEmail:     commit.AuthorEmail,
		PrincipalID:     commit.PrincipalID,
		CreatedAt:       commit.CreatedAt.Format("2006-01-02 15:04:05"),
		RepositoryCount: len(commit.Repositories),
		OperationCount:  localCommitOperationCount(commit),
		Pushed:          commit.Pushed,
	}
}

func localCommitOperationCount(commit LocalVirtualCommit) int {
	total := 0
	for _, repo := range commit.Repositories {
		total += len(repo.Operations)
	}
	return total
}

func normalizeLogicalBranchName(rawBranchName string) (string, error) {
	branchName := strings.TrimSpace(rawBranchName)
	if branchName == "" {
		return "", fmt.Errorf("logical branch name is required")
	}
	if branchName == "." || branchName == ".." || branchName == "HEAD" {
		return "", fmt.Errorf("logical branch name %q is not allowed", branchName)
	}
	if strings.HasPrefix(branchName, "/") || strings.HasSuffix(branchName, "/") || strings.Contains(branchName, "//") {
		return "", fmt.Errorf("logical branch name %q is not allowed", branchName)
	}
	if strings.HasSuffix(branchName, ".") || strings.HasSuffix(branchName, ".lock") || strings.Contains(branchName, "..") || strings.Contains(branchName, "@{") {
		return "", fmt.Errorf("logical branch name %q is not allowed", branchName)
	}
	if strings.ContainsAny(branchName, " ~^:?*[\\") {
		return "", fmt.Errorf("logical branch name %q is not allowed", branchName)
	}
	return branchName, nil
}

func (h *SessionSocketHandler) collectLogicalBranchState(currentBranch string) ([]BranchInfo, []BranchMappingInfo, error) {
	localCommits, err := h.sessionMgr.ListLocalVirtualCommits()
	if err != nil {
		return nil, nil, fmt.Errorf("list local commits: %v", err)
	}
	branchMappings, err := h.sessionMgr.ListBranchMappings()
	if err != nil {
		return nil, nil, fmt.Errorf("list branch mappings: %v", err)
	}

	branchMap := make(map[string]*BranchInfo)
	addBranch := func(name string) *BranchInfo {
		branch := branchMap[name]
		if branch == nil {
			branch = &BranchInfo{Name: name}
			branchMap[name] = branch
		}
		return branch
	}

	trimmedCurrent := strings.TrimSpace(currentBranch)
	if trimmedCurrent != "" {
		addBranch(trimmedCurrent).Current = true
	}

	for _, commit := range localCommits {
		branchName := strings.TrimSpace(commit.LogicalBranch)
		if branchName == "" {
			continue
		}
		branch := addBranch(branchName)
		if !commit.Pushed {
			branch.PendingCommits++
		}
	}

	currentMappings := make([]BranchMappingInfo, 0)
	for _, mapping := range branchMappings {
		if h.principalID != "" && mapping.PrincipalID != h.principalID {
			continue
		}
		branchName := strings.TrimSpace(mapping.LogicalBranch)
		if branchName == "" {
			continue
		}
		branch := addBranch(branchName)
		branch.HasMappings = true
		if branchName == trimmedCurrent && strings.TrimSpace(mapping.ActualBranch) != "" {
			currentMappings = append(currentMappings, BranchMappingInfo{
				DisplayPath:      mapping.DisplayPath,
				OriginalBranch:   mapping.OriginalBranch,
				ActualBranch:     mapping.ActualBranch,
				LastPushedCommit: mapping.LastPushedCommit,
			})
		}
	}

	branches := make([]BranchInfo, 0, len(branchMap))
	for _, branch := range branchMap {
		branches = append(branches, *branch)
	}
	sortBranchInfos(branches)
	sortBranchMappingInfos(currentMappings)
	return branches, currentMappings, nil
}

func (h *SessionSocketHandler) seedLogicalBranchMappings(branchName string) error {
	if strings.TrimSpace(h.principalID) == "" {
		return nil
	}
	if h.rootNode == nil || h.rootNode.WorkspaceManifest() == nil {
		return nil
	}

	entries, err := h.rootNode.WorkspaceManifest().List(h.ctx)
	if err != nil {
		return fmt.Errorf("list workspace repositories: %v", err)
	}
	for _, entry := range entries {
		if !entry.Included {
			continue
		}
		mapping := SessionBranchMapping{
			PrincipalID:    h.principalID,
			LogicalBranch:  branchName,
			StorageID:      entry.Repository.StorageID,
			DisplayPath:    entry.Repository.DisplayPath,
			OriginalBranch: entry.Repository.Ref,
			ActualBranch:   "",
		}
		if err := h.sessionMgr.PutBranchMapping(mapping); err != nil {
			return fmt.Errorf("persist branch mapping for %s: %v", entry.Repository.DisplayPath, err)
		}
	}
	return nil
}

func stagedEntriesEqual(staged, current StagedIndexEntry) bool {
	return staged.Path == current.Path &&
		staged.RepositoryStorageID == current.RepositoryStorageID &&
		staged.RepositoryPath == current.RepositoryPath &&
		staged.ChangeType == current.ChangeType &&
		staged.Mode == current.Mode &&
		staged.SymlinkTarget == current.SymlinkTarget &&
		bytes.Equal(staged.Content, current.Content)
}

func normalizeRequestedSessionPaths(paths []string) ([]string, error) {
	cleaned := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, rawPath := range paths {
		trimmed := strings.Trim(strings.TrimSpace(rawPath), "/")
		if trimmed == "" || trimmed == "." {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		cleaned = append(cleaned, trimmed)
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("at least one path is required")
	}

	sort.Slice(cleaned, func(i, j int) bool {
		if len(cleaned[i]) != len(cleaned[j]) {
			return len(cleaned[i]) < len(cleaned[j])
		}
		return cleaned[i] < cleaned[j]
	})

	normalized := make([]string, 0, len(cleaned))
	for _, path := range cleaned {
		covered := false
		for _, existing := range normalized {
			if requestedPathMatchesChange(existing, path) {
				covered = true
				break
			}
		}
		if !covered {
			normalized = append(normalized, path)
		}
	}

	return normalized, nil
}

func (h *SessionSocketHandler) snapshotStagedEntry(change classifiedSessionChange) (StagedIndexEntry, error) {
	entry := StagedIndexEntry{
		Path:                change.Path,
		RepositoryStorageID: change.StorageID,
		RepositoryPath:      change.Repository,
		ChangeType:          change.Type,
		StagedAt:            time.Now().UTC(),
		LocalPath:           change.LocalPath,
	}

	switch change.Type {
	case ChangeCreate, ChangeModify:
		content, mode, err := loadCommitLocalFile(change.LocalPath)
		if err != nil {
			return StagedIndexEntry{}, err
		}
		entry.Content = content
		entry.Mode = mode
	case ChangeDelete, ChangeRmdir, ChangeRemoveUserRootDir:
		// No local content required for a delete snapshot.
	case ChangeMkdir, ChangeUserRootDir:
		entry.Mode = commitLocalMode(change.LocalPath, 0755)
	case ChangeSymlink:
		entry.Mode = commitLocalMode(change.LocalPath, 0777)
		target := strings.TrimSpace(change.SymlinkTarget)
		if target == "" {
			if sessionTarget, ok := h.sessionMgr.GetSymlinkTarget(change.Path); ok {
				target = sessionTarget
			} else if strings.TrimSpace(change.LocalPath) != "" {
				var err error
				target, err = os.Readlink(change.LocalPath)
				if err != nil {
					return StagedIndexEntry{}, fmt.Errorf("read symlink target for %q: %w", change.Path, err)
				}
			}
		}
		if target == "" {
			return StagedIndexEntry{}, fmt.Errorf("symlink target missing for %q", change.Path)
		}
		entry.SymlinkTarget = target
	default:
		return StagedIndexEntry{}, fmt.Errorf("staging %q changes is not supported", change.Type)
	}

	return entry, nil
}

func (h *SessionSocketHandler) resolveRemoveTarget(path string) (removeTarget, error) {
	trimmedPath := strings.Trim(strings.TrimSpace(path), "/")
	if trimmedPath == "" {
		return removeTarget{}, fmt.Errorf("path is required")
	}
	if isDependencyPath(trimmedPath) {
		return removeTarget{}, fmt.Errorf("path %q is a dependency path; use push-blobs for dependency changes", path)
	}
	if h.rootNode != nil && h.rootNode.WorkspaceManifest() != nil {
		resolution, err := h.rootNode.WorkspaceManifest().ResolvePath(h.ctx, trimmedPath)
		if err != nil {
			return removeTarget{}, fmt.Errorf("resolve workspace path %q: %w", trimmedPath, err)
		}
		if !resolution.Included {
			return removeTarget{}, fmt.Errorf("path %q is outside the virtual monorepo view", path)
		}
		if resolution.Repository != nil && strings.Trim(resolution.Repository.DisplayPath, "/") == trimmedPath {
			return removeTarget{}, fmt.Errorf("removing repository root %q is not supported", path)
		}
	}

	localPath, err := h.sessionMgr.GetLocalPath(trimmedPath)
	if err != nil {
		return removeTarget{}, err
	}
	target := removeTarget{Path: trimmedPath, LocalPath: localPath}

	if h.sessionMgr.IsDeleted(trimmedPath) {
		target.ShouldTrackDelete = true
	}

	if entry, found, err := h.sessionMgr.GetOverlayDB().GetFile(trimmedPath); err == nil && found {
		target.ShouldTrackDelete = true
		target.IsDir = entry.Type == FileEntryDir
		if strings.TrimSpace(entry.LocalPath) != "" {
			target.LocalPath = entry.LocalPath
		}
		return target, nil
	} else if err != nil {
		return removeTarget{}, fmt.Errorf("read overlay entry for %q: %w", trimmedPath, err)
	}

	if info, statErr := os.Lstat(localPath); statErr == nil {
		if info.IsDir() {
			target.IsDir = true
		} else {
			target.ShouldTrackDelete = true
			return target, nil
		}
	}

	if stagedEntry, found, err := h.sessionMgr.GetStagedEntry(trimmedPath); err == nil && found {
		target.ShouldTrackDelete = true
		target.IsDir = stagedEntry.ChangeType == ChangeRmdir || stagedEntry.ChangeType == ChangeMkdir
		if strings.TrimSpace(stagedEntry.LocalPath) != "" {
			target.LocalPath = stagedEntry.LocalPath
		}
		return target, nil
	} else if err != nil {
		return removeTarget{}, fmt.Errorf("read staged entry for %q: %w", trimmedPath, err)
	}

	backendFound, backendIsDir, err := h.backendPathKind(trimmedPath)
	if err != nil {
		return removeTarget{}, err
	}
	if backendFound {
		target.IsDir = backendIsDir
		target.ShouldTrackDelete = true
		return target, nil
	}

	if target.IsDir {
		return target, nil
	}

	return removeTarget{}, fmt.Errorf("path %q does not exist in the current session or workspace", path)
}

func (h *SessionSocketHandler) removeFileTarget(target removeTarget) error {
	if strings.TrimSpace(target.LocalPath) != "" {
		if err := forceRemoveFile(target.LocalPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %q: %w", target.Path, err)
		}
	}
	if !target.ShouldTrackDelete {
		return nil
	}
	if err := h.sessionMgr.TrackChange(ChangeDelete, target.Path, ""); err != nil {
		return fmt.Errorf("track delete for %q: %w", target.Path, err)
	}
	return nil
}

func (h *SessionSocketHandler) removeDirectoryTarget(target removeTarget) error {
	if strings.TrimSpace(target.LocalPath) != "" {
		if err := forceRemoveAll(target.LocalPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove directory %q: %w", target.Path, err)
		}
	}
	if target.ShouldTrackDelete {
		if err := h.sessionMgr.TrackChange(ChangeRmdir, target.Path, ""); err != nil {
			return fmt.Errorf("track directory delete for %q: %w", target.Path, err)
		}
	}
	if _, err := h.sessionMgr.GetOverlayDB().DeleteFilesUnderPrefix(target.Path); err != nil {
		return fmt.Errorf("clear descendant overlay files for %q: %w", target.Path, err)
	}
	if _, err := h.sessionMgr.GetOverlayDB().DeleteDeletedUnderPrefix(target.Path); err != nil {
		return fmt.Errorf("clear descendant delete markers for %q: %w", target.Path, err)
	}
	return nil
}

func (h *SessionSocketHandler) clearStagedEntriesForPath(path string) error {
	entries, err := h.sessionMgr.ListStagedEntries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !requestedPathMatchesChange(path, entry.Path) {
			continue
		}
		if err := h.sessionMgr.DeleteStagedEntry(entry.Path); err != nil {
			return err
		}
	}
	return nil
}

func collectWorkspaceChangesForPaths(paths []string, classified []classifiedSessionChange) []classifiedSessionChange {
	selected := make([]classifiedSessionChange, 0)
	selectedByPath := make(map[string]struct{})
	for _, requested := range paths {
		for _, change := range classified {
			if change.Scope != sessionChangeWorkspace || !requestedPathMatchesChange(requested, change.Path) {
				continue
			}
			if _, exists := selectedByPath[change.Path]; exists {
				continue
			}
			selectedByPath[change.Path] = struct{}{}
			selected = append(selected, change)
		}
	}
	sortClassifiedSessionChanges(selected)
	return selected
}

func (h *SessionSocketHandler) selectWorkspaceChangesForStaging(paths []string, classified []classifiedSessionChange) ([]classifiedSessionChange, error) {
	selected := make([]classifiedSessionChange, 0)
	selectedByPath := make(map[string]struct{})

	for _, rawPath := range paths {
		requested := strings.Trim(strings.TrimSpace(rawPath), "/")
		stageAll := requested == "" || requested == "."

		workspaceMatches := 0
		blobMatches := 0
		excludedMatches := 0
		anyMatches := 0

		for _, change := range classified {
			if !stageAll && !requestedPathMatchesChange(requested, change.Path) {
				continue
			}
			anyMatches++
			switch change.Scope {
			case sessionChangeWorkspace:
				workspaceMatches++
				if _, exists := selectedByPath[change.Path]; exists {
					continue
				}
				selectedByPath[change.Path] = struct{}{}
				selected = append(selected, change)
			case sessionChangeBlob:
				blobMatches++
			case sessionChangeExcluded:
				excludedMatches++
			}
		}

		if workspaceMatches > 0 {
			continue
		}
		if blobMatches > 0 {
			return nil, fmt.Errorf("path %q only has dependency changes; use push-blobs instead", rawPath)
		}
		if excludedMatches > 0 {
			return nil, fmt.Errorf("path %q is outside the virtual monorepo view", rawPath)
		}
		if anyMatches == 0 {
			return nil, fmt.Errorf("no pending source changes for %q", rawPath)
		}
	}

	sortClassifiedSessionChanges(selected)
	return selected, nil
}

func (h *SessionSocketHandler) backendPathKind(path string) (bool, bool, error) {
	if h.rootNode == nil || h.rootNode.client == nil {
		return false, false, nil
	}
	backendPath := path
	if mapped, ok := backendPathForSystemView(path); ok {
		backendPath = mapped
	}
	resp, err := h.rootNode.client.GetAttr(h.ctx, backendPath)
	if err != nil {
		return false, false, fmt.Errorf("lookup backend attrs for %q: %w", path, err)
	}
	if resp == nil || !resp.Found {
		return false, false, nil
	}
	return true, resp.Mode&uint32(syscall.S_IFDIR) != 0, nil
}

func requestedPathMatchesChange(requested, changePath string) bool {
	requested = strings.Trim(strings.TrimSpace(requested), "/")
	changePath = strings.Trim(strings.TrimSpace(changePath), "/")
	if requested == "" {
		return true
	}
	return changePath == requested || strings.HasPrefix(changePath, requested+"/")
}

func sortFileDiffs(diffs []FileDiff) {
	sort.Slice(diffs, func(i, j int) bool {
		if diffs[i].Repository != diffs[j].Repository {
			return diffs[i].Repository < diffs[j].Repository
		}
		if diffs[i].Path != diffs[j].Path {
			return diffs[i].Path < diffs[j].Path
		}
		return diffs[i].ChangeType < diffs[j].ChangeType
	})
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
	repoSummary := ""
	if result.Repositories > 0 {
		repoSummary = fmt.Sprintf(" across %d repositories", result.Repositories)
	}
	if result.FilesFailed > 0 {
		return fmt.Sprintf("Processed %d files%s: %d uploaded, %d failed",
			result.FilesProcessed, repoSummary, result.FilesUploaded, result.FilesFailed)
	}
	return fmt.Sprintf("Successfully processed %d files%s", result.FilesProcessed, repoSummary)
}

func formatWorkspacePullMessage(result *monoclient.WorkspaceRefreshResult) string {
	if result == nil || result.Requested == 0 {
		return "No workspace repositories to refresh"
	}
	if result.Failed > 0 {
		return fmt.Sprintf("Refreshed %d of %d repositories; %d failed", result.Refreshed, result.Requested, result.Failed)
	}
	return fmt.Sprintf("Refreshed %d workspace repositories", result.Refreshed)
}

func (h *SessionSocketHandler) collectPullRepositories(ctx context.Context) ([]monoclient.WorkspaceRepository, error) {
	manifest := h.rootNode.WorkspaceManifest()
	entries, err := manifest.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workspace repositories: %w", err)
	}
	repos := make([]monoclient.WorkspaceRepository, 0, len(entries))
	for _, entry := range entries {
		if !entry.Included {
			continue
		}
		repos = append(repos, entry.Repository)
	}
	return repos, nil
}

func (h *SessionSocketHandler) invalidateWorkspaceAfterPull(repos []monoclient.WorkspaceRepository) {
	if h.rootNode != nil && h.rootNode.WorkspaceManifest() != nil {
		h.rootNode.WorkspaceManifest().Invalidate()
	}

	if h.attrCache != nil {
		h.attrCache.Invalidate("")
		for _, repo := range repos {
			h.attrCache.InvalidatePrefix(repo.DisplayPath)
		}
	}

	if h.rootNode == nil {
		return
	}
	namespaces := make(map[string]struct{})
	for _, repo := range repos {
		parts := splitN(repo.DisplayPath, "/", 2)
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			namespaces[parts[0]] = struct{}{}
		}
	}
	for namespace := range namespaces {
		h.rootNode.invalidateEntry(namespace)
	}
	h.rootNode.invalidateEntry(syntheticWorkspaceControlDirName)
}

func (h *SessionSocketHandler) appendWorkspaceGitSyncWarning(message string, shouldSync bool) string {
	if !shouldSync || h.rootNode == nil {
		return message
	}
	if err := h.rootNode.SyncWorkspaceGitProjection(h.ctx); err != nil {
		warning := fmt.Sprintf("workspace git metadata sync failed: %v", err)
		if strings.TrimSpace(message) == "" {
			return warning
		}
		return message + "; warning: " + warning
	}
	return message
}
