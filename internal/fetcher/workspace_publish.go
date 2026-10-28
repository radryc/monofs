package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	pb "github.com/radryc/monofs/api/proto"
	monogit "github.com/radryc/monofs/internal/git"
	"github.com/radryc/monofs/internal/workspacebundle"
	"google.golang.org/grpc"
)

const stagedWorkspaceBundleTTL = 30 * time.Minute

type syncWorkerBundle struct {
	bundleID     string
	workspaceID  string
	data         []byte
	bundle       *workspacebundle.Bundle
	commitBundle *workspacebundle.SourceCommitBundle
	createdAt    time.Time
	expiresAt    time.Time
}

func (s *Service) StageWorkspaceBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.StageWorkspaceBundleResponse]) error {
	var buf bytes.Buffer
	workspaceID := ""
	bundleID := ""
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "failed").Inc()
			s.syncStageFails.Add(1)
			return err
		}
		if chunk.GetWorkspaceId() != "" {
			if workspaceID != "" && workspaceID != chunk.GetWorkspaceId() {
				fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "failed").Inc()
				s.syncStageFails.Add(1)
				return fmt.Errorf("workspace_id changed within staged bundle stream")
			}
			workspaceID = chunk.GetWorkspaceId()
		}
		if chunk.GetBundleId() != "" {
			if bundleID != "" && bundleID != chunk.GetBundleId() {
				fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "failed").Inc()
				s.syncStageFails.Add(1)
				return fmt.Errorf("bundle_id changed within staged bundle stream")
			}
			bundleID = chunk.GetBundleId()
		}
		if len(chunk.GetData()) > 0 {
			if _, err := buf.Write(chunk.GetData()); err != nil {
				fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "failed").Inc()
				s.syncStageFails.Add(1)
				return fmt.Errorf("buffer staged bundle: %w", err)
			}
		}
	}
	if bundleID == "" {
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "failed").Inc()
		s.syncStageFails.Add(1)
		return fmt.Errorf("bundle_id is required")
	}
	if buf.Len() == 0 {
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "failed").Inc()
		s.syncStageFails.Add(1)
		return fmt.Errorf("workspace bundle is empty")
	}
	bundle, err := workspacebundle.Parse(buf.Bytes())
	if err != nil {
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "failed").Inc()
		s.syncStageFails.Add(1)
		return err
	}
	if workspaceID == "" {
		workspaceID = bundle.WorkspaceID
	}
	if workspaceID != bundle.WorkspaceID {
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "failed").Inc()
		s.syncStageFails.Add(1)
		return fmt.Errorf("staged workspace_id %q does not match bundle workspace_id %q", workspaceID, bundle.WorkspaceID)
	}

	entry := &syncWorkerBundle{
		bundleID:    bundleID,
		workspaceID: workspaceID,
		data:        append([]byte(nil), buf.Bytes()...),
		bundle:      bundle,
		createdAt:   time.Now(),
		expiresAt:   time.Now().Add(stagedWorkspaceBundleTTL),
	}
	s.putStagedBundle(entry)
	fetcherGitSyncBundleBytesTotal.Add(float64(len(entry.data)))
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_bundle", "succeeded").Inc()

	return stream.SendAndClose(&pb.StageWorkspaceBundleResponse{
		BundleId:        bundleID,
		WorkspaceId:     workspaceID,
		BytesReceived:   int64(len(entry.data)),
		RepositoryCount: int32(len(bundle.Repositories)),
		ExpiresAtUnix:   entry.expiresAt.Unix(),
	})
}

func (s *Service) StartWorkspacePublish(req *pb.StartWorkspacePublishRequest, stream pb.RepoSyncWorker_StartWorkspacePublishServer) error {
	start := time.Now()
	resultLabel := "succeeded"
	s.syncTotalJobs.Add(1)
	s.syncActiveJobs.Add(1)
	s.syncPublishJobs.Add(1)
	fetcherGitSyncActiveJobs.Inc()
	defer s.syncActiveJobs.Add(-1)
	defer fetcherGitSyncActiveJobs.Dec()
	defer func() {
		fetcherGitSyncDurationSeconds.WithLabelValues("publish", resultLabel).Observe(time.Since(start).Seconds())
	}()

	bundleEntry := s.getStagedBundle(req.GetBundleId())
	if bundleEntry == nil {
		resultLabel = "failed"
		s.syncFailedJobs.Add(1)
		fetcherGitSyncJobsTotal.WithLabelValues("publish", "failed").Inc()
		return fmt.Errorf("staged workspace bundle not found: %s", req.GetBundleId())
	}
	if req.GetWorkspaceId() != "" && req.GetWorkspaceId() != bundleEntry.workspaceID {
		resultLabel = "failed"
		s.syncFailedJobs.Add(1)
		fetcherGitSyncJobsTotal.WithLabelValues("publish", "failed").Inc()
		return fmt.Errorf("publish workspace_id %q does not match staged workspace_id %q", req.GetWorkspaceId(), bundleEntry.workspaceID)
	}

	ctx := stream.Context()
	jobFailed := false
publishLoop:
	for _, repo := range bundleEntry.bundle.Repositories {
		select {
		case <-ctx.Done():
			resultLabel = "failed"
			jobFailed = true
			break publishLoop
		default:
		}

		progress := s.publishBundleRepository(ctx, req, repo)
		if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED ||
			progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR ||
			progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_AUTH_FAILED ||
			progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT ||
			progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED ||
			progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH {
			jobFailed = true
			resultLabel = "failed"
		}
		if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED {
			s.syncPublishedRepos.Add(1)
		}
		if progress.GetConflictReason() != "" {
			fetcherGitSyncConflictsTotal.WithLabelValues("publish", progress.GetConflictReason()).Inc()
		}
		if err := stream.Send(progress); err != nil {
			resultLabel = "failed"
			s.syncFailedJobs.Add(1)
			fetcherGitSyncJobsTotal.WithLabelValues("publish", "failed").Inc()
			return err
		}
	}

	s.syncDoneJobs.Add(1)
	if jobFailed {
		s.syncFailedJobs.Add(1)
		fetcherGitSyncJobsTotal.WithLabelValues("publish", "failed").Inc()
		return nil
	}
	fetcherGitSyncJobsTotal.WithLabelValues("publish", "succeeded").Inc()
	return nil
}

func (s *Service) DiscardWorkspaceBundle(ctx context.Context, req *pb.DiscardWorkspaceBundleRequest) (*pb.DiscardWorkspaceBundleResponse, error) {
	if req.GetBundleId() == "" {
		return &pb.DiscardWorkspaceBundleResponse{Success: false, Message: "bundle_id is required"}, nil
	}
	if !s.removeStagedBundle(req.GetBundleId()) {
		return &pb.DiscardWorkspaceBundleResponse{Success: false, Message: "bundle not found"}, nil
	}
	return &pb.DiscardWorkspaceBundleResponse{Success: true, Message: "bundle discarded"}, nil
}

func (s *Service) publishBundleRepository(ctx context.Context, req *pb.StartWorkspacePublishRequest, repo workspacebundle.RepositoryBundle) *pb.RepoSyncProgress {
	progress := &pb.RepoSyncProgress{
		JobId: req.GetJobId(),
		Repository: &pb.WorkspaceRepositoryRef{
			StorageId:   repo.StorageID,
			DisplayPath: repo.DisplayPath,
			RepoUrl:     repo.RepoURL,
			Branch:      repo.Branch,
			BaseCommit:  repo.BaseCommit,
		},
		TargetBranch: choosePublishTargetBranch(req.GetRequestedBranchStrategy(), req.GetWorkspaceId(), req.GetJobId(), repo),
	}
	if len(repo.Operations) == 0 {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED
		progress.Message = "no operations to publish"
		return progress
	}
	operations, skippedSensitive := filterSensitiveOperations(repo.Operations)
	if len(operations) == 0 {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED
		progress.Message = fmt.Sprintf("%d sensitive path(s) skipped; nothing to publish", skippedSensitive)
		return progress
	}

	worktreeRoot, resolvedBranch, err := s.clonePublishWorktree(ctx, repo)
	if err != nil {
		progress.Status, progress.ConflictReason = mapPublishError(err)
		progress.Message = err.Error()
		return progress
	}
	defer os.RemoveAll(worktreeRoot)
	if resolvedBranch != "" {
		progress.Repository.Branch = resolvedBranch
	}

	repoHandle, err := gogit.PlainOpen(worktreeRoot)
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("open publish worktree: %v", err)
		return progress
	}
	headRef, err := repoHandle.Head()
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("read publish head: %v", err)
		return progress
	}
	progress.RemoteCommit = headRef.Hash().String()
	// An empty base commit means "publish on top of the current remote head";
	// conflicts are still caught by the push itself (non-fast-forward).
	if repo.BaseCommit != "" && progress.RemoteCommit != repo.BaseCommit {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT
		progress.ConflictReason = "base_commit_mismatch"
		progress.Message = fmt.Sprintf("remote head %s does not match base commit %s", progress.RemoteCommit, repo.BaseCommit)
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("clone_publish", "failed").Inc()
		return progress
	}
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("clone_publish", "succeeded").Inc()

	wt, err := repoHandle.Worktree()
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("open git worktree: %v", err)
		return progress
	}
	if err := checkoutPublishBranch(wt, progress.GetTargetBranch()); err != nil {
		progress.Status, progress.ConflictReason = mapPublishError(err)
		progress.Message = fmt.Sprintf("checkout publish branch: %v", err)
		return progress
	}
	if err := applyRepositoryOperations(worktreeRoot, operations); err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("apply repository operations: %v", err)
		return progress
	}

	worktreeBytes, _ := directorySize(worktreeRoot)
	s.syncWorktreeBytes.Add(worktreeBytes)
	fetcherGitSyncWorktreeBytes.Add(float64(worktreeBytes))
	defer s.syncWorktreeBytes.Add(-worktreeBytes)
	defer fetcherGitSyncWorktreeBytes.Add(-float64(worktreeBytes))

	hasChanges, err := stageWorktreeChanges(wt)
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("stage repository changes: %v", err)
		return progress
	}
	if !hasChanges {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED
		progress.Message = "no publishable changes after applying bundle"
		return progress
	}

	authorName := req.GetAuthorName()
	if authorName == "" {
		authorName = "MonoFS"
	}
	authorEmail := req.GetAuthorEmail()
	if authorEmail == "" {
		authorEmail = "monofs@local"
	}
	commitMessage := req.GetLogicalCommitMessage()
	if commitMessage == "" {
		commitMessage = fmt.Sprintf("MonoFS publish %s", repo.DisplayPath)
	}
	commitHash, err := wt.Commit(commitMessage, &gogit.CommitOptions{
		Author: &object.Signature{Name: authorName, Email: authorEmail, When: time.Now()},
		Signer: unsignedCommit{},
	})
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("commit repository changes: %v", err)
		return progress
	}
	progress.PushedCommit = commitHash.String()

	pushRef := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", progress.GetTargetBranch(), progress.GetTargetBranch()))
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_publish", "started").Inc()
	if err := repoHandle.PushContext(ctx, &gogit.PushOptions{RefSpecs: []config.RefSpec{pushRef}, ClientOptions: monogit.AuthForRepo(repo.RepoURL)}); err != nil {
		progress.Status, progress.ConflictReason = mapPublishError(err)
		progress.Message = fmt.Sprintf("push repository changes: %v", err)
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_publish", "failed").Inc()
		return progress
	}
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_publish", "succeeded").Inc()
	progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED
	progress.Message = "repository published"
	if skippedSensitive > 0 {
		progress.Message = fmt.Sprintf("repository published (%d sensitive path(s) skipped)", skippedSensitive)
		s.logger.Warn("skipped sensitive paths during publish",
			"display_path", repo.DisplayPath,
			"skipped", skippedSensitive)
	}
	return progress
}

func (s *Service) clonePublishWorktree(ctx context.Context, repo workspacebundle.RepositoryBundle) (string, string, error) {
	baseDir := ""
	if s.config.SyncRepoCacheDir != "" {
		baseDir = s.config.SyncRepoCacheDir
	}
	worktreeRoot, err := os.MkdirTemp(baseDir, "publish-*")
	if err != nil {
		return "", "", fmt.Errorf("create publish worktree: %w", err)
	}
	cloneOptions := &gogit.CloneOptions{
		URL:           repo.RepoURL,
		ClientOptions: monogit.AuthForRepo(repo.RepoURL),
		SingleBranch:  true,
	}
	// An empty branch clones the remote default branch (HEAD), resolved from
	// the remote refs so repositories whose HEAD is unset or points at a
	// missing branch (common on freshly created bare remotes) still work.
	if branch := strings.TrimSpace(repo.Branch); branch != "" {
		cloneOptions.ReferenceName = plumbing.NewBranchReferenceName(branch)
	} else {
		resolved, err := resolveRemoteDefaultBranch(ctx, repo.RepoURL)
		if err != nil {
			_ = os.RemoveAll(worktreeRoot)
			return "", "", err
		}
		cloneOptions.ReferenceName = plumbing.NewBranchReferenceName(resolved)
	}
	_, err = gogit.PlainCloneContext(ctx, worktreeRoot, cloneOptions)
	if err != nil {
		_ = os.RemoveAll(worktreeRoot)
		return "", "", fmt.Errorf("clone repository for publish: %w", err)
	}
	resolvedBranch := strings.TrimSpace(repo.Branch)
	if resolvedBranch == "" {
		if handle, openErr := gogit.PlainOpen(worktreeRoot); openErr == nil {
			if head, headErr := handle.Head(); headErr == nil && head != nil {
				resolvedBranch = head.Name().Short()
			}
		}
	}
	return worktreeRoot, resolvedBranch, nil
}

// resolveRemoteDefaultBranch finds the default branch of a remote by listing
// its refs: the symbolic HEAD target when it exists, then main, then master.
func resolveRemoteDefaultBranch(ctx context.Context, repoURL string) (string, error) {
	remote := gogit.NewRemote(nil, &config.RemoteConfig{Name: "origin", URLs: []string{repoURL}})
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{ClientOptions: monogit.AuthForRepo(repoURL)})
	if err != nil {
		return "", fmt.Errorf("list remote refs for %s: %w", repoURL, err)
	}
	exists := func(name plumbing.ReferenceName) bool {
		for _, ref := range refs {
			if ref.Name() == name {
				return true
			}
		}
		return false
	}
	for _, ref := range refs {
		if ref.Name() != plumbing.HEAD || ref.Target() == "" {
			continue
		}
		target := ref.Target().String()
		if strings.HasPrefix(target, "refs/heads/") && exists(plumbing.ReferenceName(target)) {
			return strings.TrimPrefix(target, "refs/heads/"), nil
		}
	}
	for _, branch := range []string{"main", "master"} {
		if exists(plumbing.NewBranchReferenceName(branch)) {
			return branch, nil
		}
	}
	return "", fmt.Errorf("could not determine the default branch of %s", repoURL)
}

func checkoutPublishBranch(wt *gogit.Worktree, targetBranch string) error {
	if targetBranch == "" {
		return nil
	}
	branchRef := plumbing.NewBranchReferenceName(targetBranch)
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: branchRef}); err == nil {
		return nil
	}
	return wt.Checkout(&gogit.CheckoutOptions{Branch: branchRef, Create: true})
}

func applyRepositoryOperations(root string, operations []workspacebundle.Operation) error {
	for _, op := range operations {
		path := filepath.Join(root, filepath.Clean(op.Path))
		switch op.Kind {
		case workspacebundle.OperationUpsert:
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(path, op.Content, fs.FileMode(defaultFileMode(op.Mode))); err != nil {
				return err
			}
		case workspacebundle.OperationDelete:
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		case workspacebundle.OperationMkdir:
			if err := os.MkdirAll(path, fs.FileMode(defaultDirMode(op.Mode))); err != nil {
				return err
			}
		case workspacebundle.OperationRmdir:
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		case workspacebundle.OperationSymlink:
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			if err := os.Symlink(op.Target, path); err != nil {
				return err
			}
		case workspacebundle.OperationRename:
			target := filepath.Join(root, filepath.Clean(op.Target))
			if rel, relErr := filepath.Rel(root, target); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("rename target %q escapes repository root", op.Target)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.Rename(path, target); err != nil {
				return err
			}
		case workspacebundle.OperationChmod:
			mode := fs.FileMode(op.Mode)
			if mode == 0 {
				mode = 0644
			}
			if err := os.Chmod(path, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func stageWorktreeChanges(wt *gogit.Worktree) (bool, error) {
	status, err := wt.Status()
	if err != nil {
		return false, err
	}
	hasChanges := false
	for path, fileStatus := range status {
		if fileStatus.Worktree == gogit.Unmodified && fileStatus.Staging == gogit.Unmodified {
			continue
		}
		hasChanges = true
		if fileStatus.Worktree == gogit.Deleted {
			if _, err := wt.Remove(path); err != nil && !strings.Contains(err.Error(), "entry not found") {
				return false, err
			}
			continue
		}
		if _, err := wt.Add(path); err != nil {
			return false, err
		}
	}
	return hasChanges, nil
}

func choosePublishTargetBranch(strategy, workspaceID, jobID string, repo workspacebundle.RepositoryBundle) string {
	switch strategy {
	case "", "direct":
		return repo.Branch
	case "workspace_branch":
		return sanitizeBranchName(fmt.Sprintf("monofs/%s/%s", workspaceID, shortJobID(jobID)))
	case "per_repo_branch":
		return sanitizeBranchName(fmt.Sprintf("monofs/%s/%s", workspaceID, repo.StorageID))
	default:
		return repo.Branch
	}
}

func sanitizeBranchName(name string) string {
	replacer := strings.NewReplacer(" ", "-", "..", "-", "~", "-", "^", "-", ":", "-", "?", "-", "*", "-", "[", "-", "\\", "-")
	clean := replacer.Replace(name)
	clean = strings.Trim(clean, "/.-")
	if clean == "" {
		return "monofs/publish"
	}
	return clean
}

func shortJobID(jobID string) string {
	if len(jobID) <= 12 {
		return jobID
	}
	return jobID[len(jobID)-12:]
}

func defaultFileMode(mode int64) int64 {
	if mode == 0 {
		return 0644
	}
	return mode
}

func defaultDirMode(mode int64) int64 {
	if mode == 0 {
		return 0755
	}
	return mode
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (s *Service) putStagedBundle(entry *syncWorkerBundle) {
	s.stagedBundlesMu.Lock()
	if existing, ok := s.stagedBundles[entry.bundleID]; ok {
		s.syncStagedBundleBytes.Add(-int64(len(existing.data)))
	} else {
		s.syncStagedBundles.Add(1)
	}
	s.stagedBundles[entry.bundleID] = entry
	s.syncStagedBundleBytes.Add(int64(len(entry.data)))
	s.stagedBundlesMu.Unlock()
}

func (s *Service) getStagedBundle(bundleID string) *syncWorkerBundle {
	s.stagedBundlesMu.RLock()
	entry := s.stagedBundles[bundleID]
	s.stagedBundlesMu.RUnlock()
	if entry == nil {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		s.removeStagedBundle(bundleID)
		return nil
	}
	return entry
}

func (s *Service) removeStagedBundle(bundleID string) bool {
	s.stagedBundlesMu.Lock()
	defer s.stagedBundlesMu.Unlock()
	entry, ok := s.stagedBundles[bundleID]
	if !ok {
		return false
	}
	delete(s.stagedBundles, bundleID)
	s.syncStagedBundles.Add(-1)
	s.syncStagedBundleBytes.Add(-int64(len(entry.data)))
	return true
}

func mapPublishError(err error) (pb.RepoSyncStatus, string) {
	if err == nil {
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED, ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "authentication") || strings.Contains(msg, "authorization") || strings.Contains(msg, "access denied"):
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_AUTH_FAILED, "auth_failed"
	case strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "already exists") || strings.Contains(msg, "rejected") || strings.Contains(msg, "base commit"):
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT, "non_fast_forward"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "connection") || strings.Contains(msg, "transport"):
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR, "transport_error"
	default:
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED, ""
	}
}

// sensitiveSegments are directory names that must never be published upstream.
var sensitiveSegments = map[string]bool{
	".registry": true,
	"secrets":   true,
}

// sensitiveNames and sensitiveSuffixes catch secret-bearing files. They are
// deliberately narrow so ordinary source files are never blocked.
var sensitiveNames = map[string]bool{
	".env":             true,
	".netrc":           true,
	".git-credentials": true,
	"id_rsa":           true,
	"id_ed25519":       true,
}

var sensitiveSuffixes = []string{
	".env", ".secret.yaml", ".secret.yml", ".secret.json",
	".pem", ".key", ".p12", ".pfx",
}

// sensitivePublishPath reports whether a repository-relative path is on the
// publish deny-list.
func sensitivePublishPath(p string) bool {
	clean := strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
	if clean == "" {
		return false
	}
	segments := strings.Split(clean, "/")
	for _, segment := range segments {
		if sensitiveSegments[strings.ToLower(segment)] {
			return true
		}
	}
	base := strings.ToLower(segments[len(segments)-1])
	if sensitiveNames[base] || strings.HasPrefix(base, ".env.") {
		return true
	}
	for _, suffix := range sensitiveSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// filterSensitiveOperations drops operations that touch deny-listed paths so
// secrets and local overlays never reach the upstream repository.
func filterSensitiveOperations(operations []workspacebundle.Operation) ([]workspacebundle.Operation, int) {
	filtered := make([]workspacebundle.Operation, 0, len(operations))
	skipped := 0
	for _, op := range operations {
		if sensitivePublishPath(op.Path) || sensitivePublishPath(op.Target) {
			skipped++
			continue
		}
		filtered = append(filtered, op)
	}
	return filtered, skipped
}
