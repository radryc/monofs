package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
	"google.golang.org/grpc"
)

type sourcePushRepositoryPlan struct {
	repo                workspacebundle.SourceCommitRepository
	operations          []workspacebundle.Operation
	commitIDs           []string
	commitCount         int
	latestCommitMessage string
	latestAuthorName    string
	latestAuthorEmail   string
}

func (s *Service) StageWorkspaceCommitBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.StageWorkspaceBundleResponse]) error {
	var buf bytes.Buffer
	workspaceID := ""
	bundleID := ""
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "failed").Inc()
			s.syncStageFails.Add(1)
			return err
		}
		if chunk.GetWorkspaceId() != "" {
			if workspaceID != "" && workspaceID != chunk.GetWorkspaceId() {
				fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "failed").Inc()
				s.syncStageFails.Add(1)
				return fmt.Errorf("workspace_id changed within staged commit bundle stream")
			}
			workspaceID = chunk.GetWorkspaceId()
		}
		if chunk.GetBundleId() != "" {
			if bundleID != "" && bundleID != chunk.GetBundleId() {
				fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "failed").Inc()
				s.syncStageFails.Add(1)
				return fmt.Errorf("bundle_id changed within staged commit bundle stream")
			}
			bundleID = chunk.GetBundleId()
		}
		if len(chunk.GetData()) > 0 {
			if _, err := buf.Write(chunk.GetData()); err != nil {
				fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "failed").Inc()
				s.syncStageFails.Add(1)
				return fmt.Errorf("buffer staged commit bundle: %w", err)
			}
		}
	}
	if bundleID == "" {
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "failed").Inc()
		s.syncStageFails.Add(1)
		return fmt.Errorf("bundle_id is required")
	}
	if buf.Len() == 0 {
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "failed").Inc()
		s.syncStageFails.Add(1)
		return fmt.Errorf("source commit bundle is empty")
	}
	bundle, err := workspacebundle.ParseSourceCommitBundle(buf.Bytes())
	if err != nil {
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "failed").Inc()
		s.syncStageFails.Add(1)
		return err
	}
	if workspaceID == "" {
		workspaceID = bundle.WorkspaceID
	}
	if workspaceID != bundle.WorkspaceID {
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "failed").Inc()
		s.syncStageFails.Add(1)
		return fmt.Errorf("staged workspace_id %q does not match bundle workspace_id %q", workspaceID, bundle.WorkspaceID)
	}

	entry := &syncWorkerBundle{
		bundleID:     bundleID,
		workspaceID:  workspaceID,
		data:         append([]byte(nil), buf.Bytes()...),
		commitBundle: bundle,
		createdAt:    time.Now(),
		expiresAt:    time.Now().Add(stagedWorkspaceBundleTTL),
	}
	s.putStagedBundle(entry)
	fetcherGitSyncBundleBytesTotal.Add(float64(len(entry.data)))
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("stage_commit_bundle", "succeeded").Inc()

	return stream.SendAndClose(&pb.StageWorkspaceBundleResponse{
		BundleId:        bundleID,
		WorkspaceId:     workspaceID,
		BytesReceived:   int64(len(entry.data)),
		RepositoryCount: int32(len(bundle.RepositoryRefs())),
		ExpiresAtUnix:   entry.expiresAt.Unix(),
	})
}

func (s *Service) StartWorkspaceCommitPush(req *pb.StartWorkspaceCommitPushRequest, stream pb.RepoSyncWorker_StartWorkspaceCommitPushServer) error {
	start := time.Now()
	resultLabel := "succeeded"
	s.syncTotalJobs.Add(1)
	s.syncActiveJobs.Add(1)
	s.syncPublishJobs.Add(1)
	fetcherGitSyncActiveJobs.Inc()
	defer s.syncActiveJobs.Add(-1)
	defer fetcherGitSyncActiveJobs.Dec()
	defer func() {
		fetcherGitSyncDurationSeconds.WithLabelValues("source_push", resultLabel).Observe(time.Since(start).Seconds())
	}()

	bundleEntry := s.getStagedBundle(req.GetBundleId())
	if bundleEntry == nil || bundleEntry.commitBundle == nil {
		resultLabel = "failed"
		s.syncFailedJobs.Add(1)
		fetcherGitSyncJobsTotal.WithLabelValues("source_push", "failed").Inc()
		return fmt.Errorf("staged source commit bundle not found: %s", req.GetBundleId())
	}
	if req.GetWorkspaceId() != "" && req.GetWorkspaceId() != bundleEntry.workspaceID {
		resultLabel = "failed"
		s.syncFailedJobs.Add(1)
		fetcherGitSyncJobsTotal.WithLabelValues("source_push", "failed").Inc()
		return fmt.Errorf("source push workspace_id %q does not match staged workspace_id %q", req.GetWorkspaceId(), bundleEntry.workspaceID)
	}

	ctx := stream.Context()
	jobFailed := false
	pushMode := strings.ToLower(strings.TrimSpace(req.GetSourcePushMode()))

	if pushMode == sourcePushModePreserve {
		if err := s.pushSourceCommitsPreserve(ctx, req, bundleEntry, stream); err != nil {
			jobFailed = true
			resultLabel = "failed"
		}
	} else {
		plans := sourcePushRepositoryPlans(bundleEntry.commitBundle)
		published := make([]sourcePushRollback, 0, len(plans))
		hardFailed := false
		for _, plan := range plans {
			select {
			case <-ctx.Done():
				resultLabel = "failed"
				jobFailed = true
				hardFailed = true
			default:
			}

			if hardFailed {
				// Once one repository failed hard, do not publish the
				// remaining repositories; report them as skipped so the
				// job is an all-or-nothing unit.
				progress := &pb.RepoSyncProgress{
					JobId: req.GetJobId(),
					Repository: &pb.WorkspaceRepositoryRef{
						StorageId:   plan.repo.StorageID,
						DisplayPath: plan.repo.DisplayPath,
						RepoUrl:     plan.repo.RepoURL,
						Branch:      plan.repo.Branch,
						BaseCommit:  plan.repo.BaseCommit,
					},
					Status:  pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED,
					Message: "skipped: earlier repository in this push failed",
				}
				if err := stream.Send(progress); err != nil {
					resultLabel = "failed"
					s.syncFailedJobs.Add(1)
					fetcherGitSyncJobsTotal.WithLabelValues("source_push", "failed").Inc()
					return err
				}
				continue
			}

			progress := s.pushSourceCommitRepository(ctx, req, plan)
			if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED ||
				progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR ||
				progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_AUTH_FAILED ||
				progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT ||
				progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED ||
				progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH {
				jobFailed = true
				resultLabel = "failed"
				hardFailed = true
			}
			if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED {
				s.syncPublishedRepos.Add(1)
				published = append(published, sourcePushRollback{
					storageID:    plan.repo.StorageID,
					displayPath:  plan.repo.DisplayPath,
					repoURL:      plan.repo.RepoURL,
					branch:       plan.repo.Branch,
					baseCommit:   plan.repo.BaseCommit,
					targetBranch: progress.GetTargetBranch(),
				})
			}
			if progress.GetConflictReason() != "" {
				fetcherGitSyncConflictsTotal.WithLabelValues("source_push", progress.GetConflictReason()).Inc()
			}
			if err := stream.Send(progress); err != nil {
				resultLabel = "failed"
				s.syncFailedJobs.Add(1)
				fetcherGitSyncJobsTotal.WithLabelValues("source_push", "failed").Inc()
				return err
			}
		}

		// Best-effort rollback: a multi-repo push that failed part-way
		// reverts every repository already published back to its base
		// commit so the logical changeset does not linger half-applied.
		if hardFailed && len(published) > 0 {
			for _, record := range published {
				rollbackProgress := s.rollbackSourcePushRepository(ctx, req, record, nil, nil)
				if rollbackProgress.GetConflictReason() != "" {
					fetcherGitSyncConflictsTotal.WithLabelValues("source_push_rollback", rollbackProgress.GetConflictReason()).Inc()
				}
				if err := stream.Send(rollbackProgress); err != nil {
					resultLabel = "failed"
					return err
				}
			}
		}
	}

	s.syncDoneJobs.Add(1)
	if jobFailed {
		s.syncFailedJobs.Add(1)
		fetcherGitSyncJobsTotal.WithLabelValues("source_push", "failed").Inc()
		return nil
	}
	fetcherGitSyncJobsTotal.WithLabelValues("source_push", "succeeded").Inc()
	return nil
}

func sourcePushRepositoryPlans(bundle *workspacebundle.SourceCommitBundle) []sourcePushRepositoryPlan {
	if bundle == nil {
		return nil
	}
	plans := make(map[string]*sourcePushRepositoryPlan)
	for _, commit := range bundle.Commits {
		for _, repo := range commit.Repositories {
			key := repo.StorageID
			if strings.TrimSpace(key) == "" {
				key = repo.DisplayPath
			}
			plan := plans[key]
			if plan == nil {
				repoCopy := repo
				plan = &sourcePushRepositoryPlan{repo: repoCopy}
				plans[key] = plan
			}
			plan.operations = append(plan.operations, repo.Operations...)
			plan.commitCount++
			plan.commitIDs = append(plan.commitIDs, commit.ID)
			plan.latestCommitMessage = strings.TrimSpace(commit.Message)
			if strings.TrimSpace(commit.AuthorName) != "" {
				plan.latestAuthorName = strings.TrimSpace(commit.AuthorName)
			}
			if strings.TrimSpace(commit.AuthorEmail) != "" {
				plan.latestAuthorEmail = strings.TrimSpace(commit.AuthorEmail)
			}
		}
	}
	out := make([]sourcePushRepositoryPlan, 0, len(plans))
	for _, plan := range plans {
		out = append(out, *plan)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].repo.DisplayPath < out[j].repo.DisplayPath
	})
	return out
}

func (s *Service) pushSourceCommitRepository(ctx context.Context, req *pb.StartWorkspaceCommitPushRequest, plan sourcePushRepositoryPlan) *pb.RepoSyncProgress {
	repo := plan.repo
	targetBranch := chooseSourcePushTargetBranch(req.GetLogicalBranch(), repo)
	progress := &pb.RepoSyncProgress{
		JobId: req.GetJobId(),
		Repository: &pb.WorkspaceRepositoryRef{
			StorageId:   repo.StorageID,
			DisplayPath: repo.DisplayPath,
			RepoUrl:     repo.RepoURL,
			Branch:      repo.Branch,
			BaseCommit:  repo.BaseCommit,
		},
		TargetBranch: targetBranch,
	}
	if len(plan.operations) == 0 {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED
		progress.Message = "no operations to push"
		return progress
	}

	worktreeRoot, err := cloneSourcePushWorktree(ctx, repo)
	if err != nil {
		progress.Status, progress.ConflictReason = mapPublishError(err)
		progress.Message = err.Error()
		return progress
	}
	defer os.RemoveAll(worktreeRoot)

	repoHandle, err := gogit.PlainOpen(worktreeRoot)
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("open source push worktree: %v", err)
		return progress
	}
	headRef, err := repoHandle.Head()
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("read source push head: %v", err)
		return progress
	}
	progress.RemoteCommit = headRef.Hash().String()
	if progress.RemoteCommit != repo.BaseCommit {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT
		progress.ConflictReason = "base_commit_mismatch"
		progress.Message = fmt.Sprintf("remote head %s does not match base commit %s", progress.RemoteCommit, repo.BaseCommit)
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("clone_source_push", "failed").Inc()
		return progress
	}
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("clone_source_push", "succeeded").Inc()

	wt, err := repoHandle.Worktree()
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("open git worktree: %v", err)
		return progress
	}
	if err := checkoutPublishBranch(wt, targetBranch); err != nil {
		progress.Status, progress.ConflictReason = mapPublishError(err)
		progress.Message = fmt.Sprintf("checkout source push branch: %v", err)
		return progress
	}
	if err := applyRepositoryOperations(worktreeRoot, plan.operations); err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("apply source commit operations: %v", err)
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
		progress.Message = fmt.Sprintf("stage source push changes: %v", err)
		return progress
	}
	if !hasChanges {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED
		progress.Message = "no source-push changes after applying commit bundle"
		return progress
	}

	authorName := plan.latestAuthorName
	if authorName == "" {
		authorName = "MonoFS"
	}
	authorEmail := plan.latestAuthorEmail
	if authorEmail == "" {
		authorEmail = "monofs@local"
	}
	commitHash, err := wt.Commit(sourcePushCommitMessage(plan), &gogit.CommitOptions{
		Author: &object.Signature{Name: authorName, Email: authorEmail, When: time.Now()},
	})
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("commit source push changes: %v", err)
		return progress
	}
	progress.PushedCommit = commitHash.String()

	pushRef := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", targetBranch, targetBranch))
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_source_push", "started").Inc()
	if err := repoHandle.PushContext(ctx, &gogit.PushOptions{RefSpecs: []config.RefSpec{pushRef}}); err != nil {
		progress.Status, progress.ConflictReason = mapPublishError(err)
		progress.Message = fmt.Sprintf("push source changes: %v", err)
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_source_push", "failed").Inc()
		return progress
	}
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_source_push", "succeeded").Inc()
	progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED
	if plan.commitCount == 1 {
		progress.Message = "repository pushed from 1 local commit"
	} else {
		progress.Message = fmt.Sprintf("repository pushed from %d local commits", plan.commitCount)
	}
	return progress
}

func cloneSourcePushWorktree(ctx context.Context, repo workspacebundle.SourceCommitRepository) (string, error) {
	worktreeRoot, err := os.MkdirTemp("", "source-push-*")
	if err != nil {
		return "", fmt.Errorf("create source push worktree: %w", err)
	}
	_, err = gogit.PlainCloneContext(ctx, worktreeRoot, &gogit.CloneOptions{
		URL:           repo.RepoURL,
		ReferenceName: plumbing.NewBranchReferenceName(repo.Branch),
		SingleBranch:  true,
	})
	if err != nil {
		_ = os.RemoveAll(worktreeRoot)
		return "", fmt.Errorf("clone repository for source push: %w", err)
	}
	return worktreeRoot, nil
}

func chooseSourcePushTargetBranch(logicalBranch string, repo workspacebundle.SourceCommitRepository) string {
	if branch := strings.TrimSpace(logicalBranch); branch != "" {
		if sanitized := sanitizeBranchName(branch); sanitized != "" {
			return sanitized
		}
	}
	return repo.Branch
}

func sourcePushCommitMessage(plan sourcePushRepositoryPlan) string {
	message := strings.TrimSpace(plan.latestCommitMessage)
	if plan.commitCount <= 1 {
		if message != "" {
			return message
		}
		return fmt.Sprintf("MonoFS source push %s", plan.repo.DisplayPath)
	}
	if message != "" {
		return fmt.Sprintf("%s\n\nMonoFS source push squashed %d local commits for %s", message, plan.commitCount, plan.repo.DisplayPath)
	}
	return fmt.Sprintf("MonoFS source push %s (%d local commits)", plan.repo.DisplayPath, plan.commitCount)
}

const sourcePushModePreserve = "preserve"

// sourcePushRollback records everything needed to revert one published
// repository back to its base commit after a partial multi-repo push
// failure.
type sourcePushRollback struct {
	storageID    string
	displayPath  string
	repoURL      string
	branch       string
	baseCommit   string
	targetBranch string
}

// rollbackSourcePushRepository reverts a published repository to its
// base commit by creating and pushing a revert commit on the target
// branch. When handle/wt are nil a fresh clone is made. It never
// force-pushes.
func (s *Service) rollbackSourcePushRepository(ctx context.Context, req *pb.StartWorkspaceCommitPushRequest, record sourcePushRollback, handle *gogit.Repository, wt *gogit.Worktree) *pb.RepoSyncProgress {
	progress := &pb.RepoSyncProgress{
		JobId: req.GetJobId(),
		Repository: &pb.WorkspaceRepositoryRef{
			StorageId:   record.storageID,
			DisplayPath: record.displayPath,
			RepoUrl:     record.repoURL,
			Branch:      record.branch,
			BaseCommit:  record.baseCommit,
		},
		TargetBranch: record.targetBranch,
	}

	if handle == nil || wt == nil {
		worktreeRoot, err := cloneBranchWorktree(ctx, record.repoURL, record.targetBranch)
		if err != nil {
			progress.Status, progress.ConflictReason = mapPublishError(err)
			progress.Message = fmt.Sprintf("rollback clone failed: %v", err)
			return progress
		}
		defer os.RemoveAll(worktreeRoot)
		handle, err = gogit.PlainOpen(worktreeRoot)
		if err != nil {
			progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
			progress.Message = fmt.Sprintf("rollback open failed: %v", err)
			return progress
		}
		wt, err = handle.Worktree()
		if err != nil {
			progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
			progress.Message = fmt.Sprintf("rollback worktree failed: %v", err)
			return progress
		}
	}

	baseHash := plumbing.NewHash(record.baseCommit)
	if record.baseCommit == "" {
		if head, err := handle.Head(); err == nil {
			// No base recorded: roll back to the pushed commit's parent.
			parentIter, err := handle.Log(&gogit.LogOptions{From: head.Hash()})
			if err == nil {
				count := 0
				_ = parentIter.ForEach(func(c *object.Commit) error {
					if count == 1 {
						baseHash = c.Hash
					}
					count++
					return nil
				})
			}
		}
	}
	if baseHash.IsZero() {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = "rollback aborted: base commit unavailable"
		return progress
	}
	baseCommit, err := handle.CommitObject(baseHash)
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("rollback aborted: base commit %s not found: %v", record.baseCommit, err)
		return progress
	}

	// Restore the base tree content in the worktree while leaving HEAD
	// at the pushed commit, so the revert is a normal fast-forward push
	// (never a force push).
	if err := restoreBaseTree(wt.Filesystem.Root(), baseCommit); err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("rollback restore failed: %v", err)
		return progress
	}

	hasChanges, err := stageWorktreeChanges(wt)
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("rollback stage failed: %v", err)
		return progress
	}
	if !hasChanges {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = "rollback aborted: repository already matches base"
		return progress
	}

	rollbackCommit, err := wt.Commit(fmt.Sprintf(
		"MonoFS rollback: revert source push of %s after partial failure\n\nMonoFS-Job: %s",
		record.displayPath, req.GetJobId()), &gogit.CommitOptions{
		Author: &object.Signature{Name: "MonoFS", Email: "monofs@local", When: time.Now()},
	})
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("rollback commit failed: %v", err)
		return progress
	}

	pushRef := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", record.targetBranch, record.targetBranch))
	if err := handle.PushContext(ctx, &gogit.PushOptions{RefSpecs: []config.RefSpec{pushRef}}); err != nil {
		progress.Status, progress.ConflictReason = mapPublishError(err)
		progress.Message = fmt.Sprintf("rollback push failed: %v", err)
		return progress
	}

	progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_ROLLED_BACK
	progress.PushedCommit = rollbackCommit.String()
	progress.Message = fmt.Sprintf("rolled back to base commit %s after partial push failure", record.baseCommit)
	return progress
}

// cloneBranchWorktree clones a repository at an explicit branch for
// rollback operations.
func cloneBranchWorktree(ctx context.Context, repoURL, branch string) (string, error) {
	worktreeRoot, err := os.MkdirTemp("", "source-push-rollback-*")
	if err != nil {
		return "", fmt.Errorf("create rollback worktree: %w", err)
	}
	_, err = gogit.PlainCloneContext(ctx, worktreeRoot, &gogit.CloneOptions{
		URL:           repoURL,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
	})
	if err != nil {
		_ = os.RemoveAll(worktreeRoot)
		return "", fmt.Errorf("clone repository for rollback: %w", err)
	}
	return worktreeRoot, nil
}

// restoreBaseTree writes the content of a base commit's tree into the
// worktree without moving HEAD: files present in the base are restored,
// files absent from the base are removed (except .git).
func restoreBaseTree(worktreeRoot string, baseCommit *object.Commit) error {
	if worktreeRoot == "" {
		return fmt.Errorf("worktree root is required")
	}
	baseFiles := make(map[string]bool)

	files, err := baseCommit.Files()
	if err != nil {
		return fmt.Errorf("read base tree: %w", err)
	}
	if err := files.ForEach(func(f *object.File) error {
		baseFiles[f.Name] = true
		target := filepath.Join(worktreeRoot, filepath.Clean(f.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		content, err := f.Contents()
		if err != nil {
			return err
		}
		return os.WriteFile(target, []byte(content), fs.FileMode(fileModeOf(f)))
	}); err != nil {
		return fmt.Errorf("restore base files: %w", err)
	}

	return filepath.WalkDir(worktreeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(worktreeRoot, path)
		if relErr != nil {
			return nil
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			return nil
		}
		if !baseFiles[filepath.ToSlash(rel)] {
			return os.Remove(path)
		}
		return nil
	})
}

func fileModeOf(f *object.File) os.FileMode {
	if osMode, err := f.Mode.ToOSFileMode(); err == nil {
		return osMode
	}
	return 0o644
}

func (s *Service) pushSourceCommitsPreserve(ctx context.Context, req *pb.StartWorkspaceCommitPushRequest, bundleEntry *syncWorkerBundle, stream pb.RepoSyncWorker_StartWorkspaceCommitPushServer) error {
	bundle := bundleEntry.commitBundle
	sort.Slice(bundle.Commits, func(i, j int) bool {
		if bundle.Commits[i].CreatedAtUnix == bundle.Commits[j].CreatedAtUnix {
			return bundle.Commits[i].ID < bundle.Commits[j].ID
		}
		return bundle.Commits[i].CreatedAtUnix < bundle.Commits[j].CreatedAtUnix
	})

	repoWorktrees := make(map[string]*repoWorktree)
	defer func() {
		for _, rw := range repoWorktrees {
			if rw.root != "" {
				os.RemoveAll(rw.root)
			}
		}
	}()

	// published tracks repositories that received at least one pushed
	// commit so they can be rolled back on a later failure.
	published := make(map[string]sourcePushRollback)
	rollbackAndReport := func() error {
		for _, record := range published {
			rw := repoWorktrees[record.storageID]
			var handle *gogit.Repository
			var wt *gogit.Worktree
			if rw != nil {
				handle, wt = rw.repoHandle, rw.wt
			}
			progress := s.rollbackSourcePushRepository(ctx, req, record, handle, wt)
			if progress.GetConflictReason() != "" {
				fetcherGitSyncConflictsTotal.WithLabelValues("source_push_rollback", progress.GetConflictReason()).Inc()
			}
			if err := stream.Send(progress); err != nil {
				return err
			}
		}
		return nil
	}

	for commitIdx, commit := range bundle.Commits {
		select {
		case <-ctx.Done():
			return rollbackAndReport()
		default:
		}

		for _, repo := range commit.Repositories {
			rw, ok := repoWorktrees[repo.StorageID]
			if !ok {
				rw = &repoWorktree{}
				var err error
				rw.root, err = cloneSourcePushWorktree(ctx, repo)
				if err != nil {
					progress := &pb.RepoSyncProgress{
						JobId:            req.GetJobId(),
						Repository:       repoRefFromSourceCommitRepo(repo),
						Status:           pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED,
						Message:          err.Error(),
						LocalCommitId:    commit.ID,
						LocalCommitIndex: int32(commitIdx),
					}
					progress.Status, progress.ConflictReason = mapPublishError(err)
					if err := stream.Send(progress); err != nil {
						return err
					}
					return rollbackAndReport()
				}
				repoWorktrees[repo.StorageID] = rw
			}

			progress := s.pushSinglePreserveCommit(ctx, req, rw, commitIdx, commit, repo, bundleEntry.workspaceID, len(bundle.Commits))
			if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED {
				s.syncPublishedRepos.Add(1)
				if _, exists := published[repo.StorageID]; !exists {
					published[repo.StorageID] = sourcePushRollback{
						storageID:    repo.StorageID,
						displayPath:  repo.DisplayPath,
						repoURL:      repo.RepoURL,
						branch:       repo.Branch,
						baseCommit:   repo.BaseCommit,
						targetBranch: progress.GetTargetBranch(),
					}
				}
			}
			if progress.GetConflictReason() != "" {
				fetcherGitSyncConflictsTotal.WithLabelValues("source_push", progress.GetConflictReason()).Inc()
			}
			if err := stream.Send(progress); err != nil {
				return err
			}
			if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT ||
				progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED {
				return rollbackAndReport()
			}
			if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED {
				return rollbackAndReport()
			}
		}
	}
	return nil
}

type repoWorktree struct {
	root       string
	repoHandle *gogit.Repository
	wt         *gogit.Worktree
}

func (s *Service) pushSinglePreserveCommit(ctx context.Context, req *pb.StartWorkspaceCommitPushRequest, rw *repoWorktree, commitIdx int, commit workspacebundle.SourceCommit, repo workspacebundle.SourceCommitRepository, workspaceID string, totalCommits int) *pb.RepoSyncProgress {
	targetBranch := chooseSourcePushTargetBranch(req.GetLogicalBranch(), repo)
	progress := &pb.RepoSyncProgress{
		JobId:            req.GetJobId(),
		Repository:       repoRefFromSourceCommitRepo(repo),
		TargetBranch:     targetBranch,
		LocalCommitId:    commit.ID,
		LocalCommitIndex: int32(commitIdx),
	}
	if len(repo.Operations) == 0 {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED
		progress.Message = fmt.Sprintf("commit %s had no operations for %s", commit.ID, repo.DisplayPath)
		return progress
	}

	if rw.repoHandle == nil {
		rh, err := gogit.PlainOpen(rw.root)
		if err != nil {
			progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
			progress.Message = fmt.Sprintf("open source push worktree: %v", err)
			return progress
		}
		rw.repoHandle = rh

		headRef, headErr := rh.Head()
		if headErr != nil {
			progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
			progress.Message = fmt.Sprintf("read source push head: %v", headErr)
			return progress
		}
		progress.RemoteCommit = headRef.Hash().String()
		if progress.RemoteCommit != repo.BaseCommit {
			progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT
			progress.ConflictReason = "base_commit_mismatch"
			progress.Message = fmt.Sprintf("remote head %s does not match base commit %s", progress.RemoteCommit, repo.BaseCommit)
			return progress
		}

		wt, wtErr := rh.Worktree()
		if wtErr != nil {
			progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
			progress.Message = fmt.Sprintf("open git worktree: %v", wtErr)
			return progress
		}
		if err := checkoutPublishBranch(wt, targetBranch); err != nil {
			progress.Status, progress.ConflictReason = mapPublishError(err)
			progress.Message = fmt.Sprintf("checkout source push branch: %v", err)
			return progress
		}
		rw.wt = wt
	}

	if err := applyRepositoryOperations(rw.root, repo.Operations); err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("apply commit %s operations: %v", commit.ID, err)
		return progress
	}

	hasChanges, err := stageWorktreeChanges(rw.wt)
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("stage commit %s changes: %v", commit.ID, err)
		return progress
	}
	if !hasChanges {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED
		progress.Message = fmt.Sprintf("commit %s produced no changes", commit.ID)
		return progress
	}

	authorName := strings.TrimSpace(commit.AuthorName)
	if authorName == "" {
		authorName = "MonoFS"
	}
	authorEmail := strings.TrimSpace(commit.AuthorEmail)
	if authorEmail == "" {
		authorEmail = "monofs@local"
	}

	commitMsg := buildPreserveCommitMessage(commit, req.GetJobId(), workspaceID)
	commitHash, err := rw.wt.Commit(commitMsg, &gogit.CommitOptions{
		Author: &object.Signature{Name: authorName, Email: authorEmail, When: time.Now()},
	})
	if err != nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = fmt.Sprintf("commit %s changes: %v", commit.ID, err)
		return progress
	}
	progress.PushedCommit = commitHash.String()

	pushRef := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", targetBranch, targetBranch))
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_source_push", "started").Inc()
	if err := rw.repoHandle.PushContext(ctx, &gogit.PushOptions{RefSpecs: []config.RefSpec{pushRef}}); err != nil {
		progress.Status, progress.ConflictReason = mapPublishError(err)
		progress.Message = fmt.Sprintf("push commit %s: %v", commit.ID, err)
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_source_push", "failed").Inc()
		return progress
	}
	fetcherGitSyncRemoteOpsTotal.WithLabelValues("push_source_push", "succeeded").Inc()
	progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED
	progress.Message = fmt.Sprintf("pushed local commit %s (%d/%d)", commit.ID, commitIdx+1, totalCommits)
	return progress
}

func buildPreserveCommitMessage(commit workspacebundle.SourceCommit, jobID, workspaceID string) string {
	msg := strings.TrimSpace(commit.Message)
	if msg == "" {
		msg = fmt.Sprintf("MonoFS local commit %s", commit.ID)
	}
	msg += fmt.Sprintf("\n\nMonoFS-Local-Commit: %s\nMonoFS-Workspace: %s\nMonoFS-Job: %s", commit.ID, workspaceID, jobID)
	return msg
}

func repoRefFromSourceCommitRepo(repo workspacebundle.SourceCommitRepository) *pb.WorkspaceRepositoryRef {
	return &pb.WorkspaceRepositoryRef{
		StorageId:   repo.StorageID,
		DisplayPath: repo.DisplayPath,
		RepoUrl:     repo.RepoURL,
		Branch:      repo.Branch,
		BaseCommit:  repo.BaseCommit,
	}
}
