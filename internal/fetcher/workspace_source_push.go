package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
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
	plans := sourcePushRepositoryPlans(bundleEntry.commitBundle)
	for _, plan := range plans {
		select {
		case <-ctx.Done():
			resultLabel = "failed"
			jobFailed = true
			break
		default:
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
		}
		if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED {
			s.syncPublishedRepos.Add(1)
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
