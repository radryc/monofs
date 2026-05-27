package router

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/protobuf/proto"
)

type workspaceSyncJobEntry struct {
	mu     sync.RWMutex
	job    *pb.WorkspaceSyncJob
	cancel context.CancelFunc
}

func (r *Router) RefreshWorkspace(req *pb.RefreshWorkspaceRequest, stream pb.MonoFSRouter_RefreshWorkspaceServer) error {
	start := time.Now()
	actionLabel := workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_REFRESH)
	job := r.newWorkspaceSyncJob(req, extractClientID(stream.Context()))
	entry := &workspaceSyncJobEntry{job: job}
	r.storeWorkspaceSyncJob(entry)
	routerWorkspaceSyncJobsTotal.WithLabelValues(actionLabel, "started").Inc()
	routerWorkspaceSyncActiveJobs.WithLabelValues(actionLabel).Inc()
	defer routerWorkspaceSyncActiveJobs.WithLabelValues(actionLabel).Dec()
	defer func() {
		resultLabel := workspaceSyncResultLabel(entry.snapshot().GetState())
		routerWorkspaceSyncDurationSeconds.WithLabelValues(actionLabel, resultLabel).Observe(time.Since(start).Seconds())
	}()

	if err := stream.Send(&pb.WorkspaceSyncEvent{
		EventType: pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_ACCEPTED,
		Job:       entry.snapshot(),
		Message:   "workspace refresh job accepted",
	}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(stream.Context())
	entry.mu.Lock()
	entry.cancel = cancel
	entry.mu.Unlock()
	defer cancel()

	if err := r.runWorkspaceRefreshJob(ctx, entry, req, func(event *pb.WorkspaceSyncEvent) error {
		return stream.Send(event)
	}); err != nil {
		return err
	}
	return nil
}

func (r *Router) GetWorkspaceSyncJob(ctx context.Context, req *pb.GetWorkspaceSyncJobRequest) (*pb.WorkspaceSyncJob, error) {
	entry := r.getWorkspaceSyncJob(req.GetJobId())
	if entry == nil {
		return nil, fmt.Errorf("workspace sync job not found: %s", req.GetJobId())
	}
	return entry.snapshot(), nil
}

func (r *Router) ListWorkspaceSyncJobs(ctx context.Context, req *pb.ListWorkspaceSyncJobsRequest) (*pb.ListWorkspaceSyncJobsResponse, error) {
	jobs := r.listWorkspaceSyncJobs(req)
	return &pb.ListWorkspaceSyncJobsResponse{Jobs: jobs}, nil
}

func (r *Router) CancelWorkspaceSyncJob(ctx context.Context, req *pb.CancelWorkspaceSyncJobRequest) (*pb.CancelWorkspaceSyncJobResponse, error) {
	entry := r.getWorkspaceSyncJob(req.GetJobId())
	if entry == nil {
		return &pb.CancelWorkspaceSyncJobResponse{Success: false, Message: "job not found"}, nil
	}
	entry.mu.Lock()
	cancel := entry.cancel
	if entry.job.GetState() == pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED || entry.job.GetState() == pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_FAILED || entry.job.GetState() == pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_CANCELLED {
		entry.mu.Unlock()
		return &pb.CancelWorkspaceSyncJobResponse{Success: false, Message: "job already finished"}, nil
	}
	entry.job.State = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_CANCELLED
	entry.job.FinishedAtUnix = time.Now().Unix()
	entry.job.ErrorMessage = "cancelled"
	entry.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return &pb.CancelWorkspaceSyncJobResponse{Success: true, Message: "job cancelled"}, nil
}

func (r *Router) runWorkspaceRefreshJob(ctx context.Context, entry *workspaceSyncJobEntry, req *pb.RefreshWorkspaceRequest, send func(*pb.WorkspaceSyncEvent) error) error {
	r.updateWorkspaceSyncJob(entry, func(job *pb.WorkspaceSyncJob) {
		job.State = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_RUNNING
		job.StartedAtUnix = time.Now().Unix()
	})
	if err := send(&pb.WorkspaceSyncEvent{
		EventType: pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_STARTED,
		Job:       entry.snapshot(),
		Message:   "workspace refresh job started",
	}); err != nil {
		return err
	}

	fetcherClient := r.getFetcherClient()
	if fetcherClient == nil {
		r.failWorkspaceSyncJob(entry, "refresh", "fetcher cluster not configured")
		return nil
	}

	probeReq := &pb.ProbeWorkspaceRefreshRequest{
		JobId:        entry.snapshot().GetJobId(),
		WorkspaceId:  req.GetWorkspaceId(),
		Repositories: req.GetRepositories(),
	}
	probeResults, err := fetcherClient.ProbeWorkspaceRefresh(ctx, probeReq)
	if err != nil {
		r.failWorkspaceSyncJob(entry, "refresh", err.Error())
		return nil
	}

	for _, result := range probeResults {
		select {
		case <-ctx.Done():
			r.cancelWorkspaceSyncJob(entry)
			return nil
		default:
		}

		repoResult := workspaceRepositoryResultFromProbe(result)
		r.updateWorkspaceSyncRepository(entry, repoResult)
		if err := send(&pb.WorkspaceSyncEvent{
			EventType:  workspaceEventTypeForRepository(repoResult),
			Job:        entry.snapshot(),
			Repository: cloneWorkspaceSyncRepositoryResult(repoResult),
			Message:    repoResult.GetMessage(),
		}); err != nil {
			return err
		}

		if repoResult.GetStatus() != pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESH_REQUIRED {
			continue
		}

		if err := send(&pb.WorkspaceSyncEvent{
			EventType:  pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_REINGEST_STARTED,
			Job:        entry.snapshot(),
			Repository: cloneWorkspaceSyncRepositoryResult(repoResult),
			Message:    "starting repository re-ingestion",
		}); err != nil {
			return err
		}

		ingestErr := r.IngestRepository(&pb.IngestRequest{
			Source:        repoResult.GetRepoUrl(),
			Ref:           repoResult.GetBranch(),
			SourceId:      repoResult.GetDisplayPath(),
			IngestionType: pb.IngestionType_INGESTION_GIT,
			FetchType:     pb.SourceType_SOURCE_TYPE_BLOB,
		}, &mockIngestStream{ctx: ctx})
		if ingestErr != nil {
			repoCopy := cloneWorkspaceSyncRepositoryResult(repoResult)
			repoCopy.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED
			repoCopy.Message = ingestErr.Error()
			r.updateWorkspaceSyncRepository(entry, repoCopy)
			routerWorkspaceSyncReingestTotal.WithLabelValues("failed").Inc()
			if err := send(&pb.WorkspaceSyncEvent{
				EventType:  pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_REPOSITORY_FAILED,
				Job:        entry.snapshot(),
				Repository: cloneWorkspaceSyncRepositoryResult(repoCopy),
				Message:    repoCopy.GetMessage(),
			}); err != nil {
				return err
			}
			continue
		}

		repoCopy := cloneWorkspaceSyncRepositoryResult(repoResult)
		repoCopy.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESHED
		repoCopy.Message = "repository refreshed"
		r.updateWorkspaceSyncRepository(entry, repoCopy)
		routerWorkspaceSyncReingestTotal.WithLabelValues("succeeded").Inc()
		if err := send(&pb.WorkspaceSyncEvent{
			EventType:  pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_REINGEST_COMPLETED,
			Job:        entry.snapshot(),
			Repository: cloneWorkspaceSyncRepositoryResult(repoCopy),
			Message:    repoCopy.GetMessage(),
		}); err != nil {
			return err
		}
	}

	r.finalizeWorkspaceRefreshJob(entry)
	if err := send(&pb.WorkspaceSyncEvent{
		EventType: pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_COMPLETED,
		Job:       entry.snapshot(),
		Message:   "workspace refresh job completed",
	}); err != nil {
		return err
	}
	return nil
}

func (r *Router) newWorkspaceSyncJob(req *pb.RefreshWorkspaceRequest, clientID string) *pb.WorkspaceSyncJob {
	jobID := fmt.Sprintf("wsync-%d", time.Now().UnixNano())
	return &pb.WorkspaceSyncJob{
		JobId:                jobID,
		WorkspaceId:          req.GetWorkspaceId(),
		Action:               pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_REFRESH,
		State:                pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED,
		RequestedByClientId:  clientID,
		CreatedAtUnix:        time.Now().Unix(),
		Summary:              &pb.WorkspaceSyncSummary{RepositoriesTotal: int32(len(req.GetRepositories()))},
		RejectIfLocalChanges: req.GetRejectIfLocalChanges(),
		AllowFastForwardOnly: req.GetAllowFastForwardOnly(),
	}
}

func (r *Router) storeWorkspaceSyncJob(entry *workspaceSyncJobEntry) {
	r.workspaceSyncMu.Lock()
	r.workspaceSyncJobs[entry.job.GetJobId()] = entry
	r.workspaceSyncMu.Unlock()
}

func (r *Router) getWorkspaceSyncJob(jobID string) *workspaceSyncJobEntry {
	r.workspaceSyncMu.RLock()
	defer r.workspaceSyncMu.RUnlock()
	return r.workspaceSyncJobs[jobID]
}

func (r *Router) listWorkspaceSyncJobs(req *pb.ListWorkspaceSyncJobsRequest) []*pb.WorkspaceSyncJob {
	r.workspaceSyncMu.RLock()
	jobs := make([]*pb.WorkspaceSyncJob, 0, len(r.workspaceSyncJobs))
	for _, entry := range r.workspaceSyncJobs {
		job := entry.snapshot()
		if req.GetAction() != pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_UNSPECIFIED && job.GetAction() != req.GetAction() {
			continue
		}
		if req.GetState() != pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_UNSPECIFIED && job.GetState() != req.GetState() {
			continue
		}
		jobs = append(jobs, job)
	}
	r.workspaceSyncMu.RUnlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].GetCreatedAtUnix() > jobs[j].GetCreatedAtUnix() })
	limit := int(req.GetLimit())
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs
}

func (e *workspaceSyncJobEntry) snapshot() *pb.WorkspaceSyncJob {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return proto.Clone(e.job).(*pb.WorkspaceSyncJob)
}

func (r *Router) updateWorkspaceSyncJob(entry *workspaceSyncJobEntry, update func(*pb.WorkspaceSyncJob)) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	update(entry.job)
}

func (r *Router) updateWorkspaceSyncRepository(entry *workspaceSyncJobEntry, repo *pb.WorkspaceSyncRepositoryResult) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	actionLabel := workspaceSyncActionMetricLabel(entry.job.GetAction())
	updated := false
	for i := range entry.job.Repositories {
		if entry.job.Repositories[i].GetStorageId() == repo.GetStorageId() {
			entry.job.Repositories[i] = cloneWorkspaceSyncRepositoryResult(repo)
			updated = true
			break
		}
	}
	if !updated {
		entry.job.Repositories = append(entry.job.Repositories, cloneWorkspaceSyncRepositoryResult(repo))
	}
	routerWorkspaceSyncRepositoriesTotal.WithLabelValues(actionLabel, workspaceSyncRepositoryMetricLabel(repo.GetStatus())).Inc()
	updateWorkspaceSyncSummary(entry.job)
}

func (r *Router) failWorkspaceSyncJob(entry *workspaceSyncJobEntry, actionLabel, message string) {
	r.updateWorkspaceSyncJob(entry, func(job *pb.WorkspaceSyncJob) {
		job.State = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_FAILED
		job.FinishedAtUnix = time.Now().Unix()
		job.ErrorMessage = message
	})
	routerWorkspaceSyncJobsTotal.WithLabelValues(actionLabel, "failed").Inc()
}

func (r *Router) cancelWorkspaceSyncJob(entry *workspaceSyncJobEntry) {
	actionLabel := workspaceSyncActionMetricLabel(entry.snapshot().GetAction())
	r.updateWorkspaceSyncJob(entry, func(job *pb.WorkspaceSyncJob) {
		job.State = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_CANCELLED
		job.FinishedAtUnix = time.Now().Unix()
		job.ErrorMessage = "cancelled"
	})
	routerWorkspaceSyncJobsTotal.WithLabelValues(actionLabel, "cancelled").Inc()
}

func (r *Router) finalizeWorkspaceRefreshJob(entry *workspaceSyncJobEntry) {
	r.finalizeWorkspaceSyncJob(entry)
	result := workspaceSyncResultLabel(entry.snapshot().GetState())
	routerWorkspaceSyncJobsTotal.WithLabelValues(workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_REFRESH), result).Inc()
}

func (r *Router) finalizeWorkspaceSyncJob(entry *workspaceSyncJobEntry) {
	r.updateWorkspaceSyncJob(entry, func(job *pb.WorkspaceSyncJob) {
		job.FinishedAtUnix = time.Now().Unix()
		updateWorkspaceSyncSummary(job)
		if job.GetSummary().GetRepositoriesFailed() > 0 || job.GetSummary().GetRepositoriesConflicted() > 0 {
			job.State = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_FAILED
			return
		}
		job.State = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED
	})
}

func updateWorkspaceSyncSummary(job *pb.WorkspaceSyncJob) {
	if job.Summary == nil {
		job.Summary = &pb.WorkspaceSyncSummary{}
	}
	summary := &pb.WorkspaceSyncSummary{RepositoriesTotal: int32(len(job.Repositories))}
	for _, repo := range job.Repositories {
		switch repo.GetStatus() {
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_UNCHANGED:
			summary.RepositoriesSucceeded++
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESH_REQUIRED:
			// not terminal
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESHED:
			summary.RepositoriesSucceeded++
			summary.RepositoriesRefreshed++
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED:
			summary.RepositoriesSucceeded++
			summary.RepositoriesPublished++
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT:
			summary.RepositoriesConflicted++
		case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED:
			summary.RepositoriesFailed++
		}
	}
	job.Summary = summary
}

func workspaceRepositoryResultFromProbe(progress *pb.RepoSyncProgress) *pb.WorkspaceSyncRepositoryResult {
	result := &pb.WorkspaceSyncRepositoryResult{}
	if progress == nil || progress.GetRepository() == nil {
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED
		result.Message = "invalid probe result"
		return result
	}
	repo := progress.GetRepository()
	result.StorageId = repo.GetStorageId()
	result.DisplayPath = repo.GetDisplayPath()
	result.RepoUrl = repo.GetRepoUrl()
	result.Branch = repo.GetBranch()
	result.BaseCommit = repo.GetBaseCommit()
	result.RemoteCommit = progress.GetRemoteCommit()
	result.Message = progress.GetMessage()
	switch progress.GetStatus() {
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_UNCHANGED
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESH_REQUIRED
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED, pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT
		result.ConflictReason = strings.ToLower(strings.TrimPrefix(progress.GetStatus().String(), "REPO_SYNC_STATUS_"))
		routerWorkspaceSyncConflictsTotal.WithLabelValues("refresh", result.ConflictReason).Inc()
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_AUTH_FAILED, pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR, pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED
	default:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED
	}
	return result
}

func workspaceRepositoryResultFromPublish(progress *pb.RepoSyncProgress, actionLabel string) *pb.WorkspaceSyncRepositoryResult {
	result := &pb.WorkspaceSyncRepositoryResult{}
	if progress == nil || progress.GetRepository() == nil {
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED
		result.Message = "invalid publish result"
		return result
	}
	repo := progress.GetRepository()
	result.StorageId = repo.GetStorageId()
	result.DisplayPath = repo.GetDisplayPath()
	result.RepoUrl = repo.GetRepoUrl()
	result.Branch = repo.GetBranch()
	result.BaseCommit = repo.GetBaseCommit()
	result.RemoteCommit = progress.GetRemoteCommit()
	result.TargetBranch = progress.GetTargetBranch()
	result.PushedCommit = progress.GetPushedCommit()
	result.ConflictReason = progress.GetConflictReason()
	result.Message = progress.GetMessage()
	switch progress.GetStatus() {
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_UNCHANGED
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT, pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED, pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT
		if result.ConflictReason == "" {
			result.ConflictReason = strings.ToLower(strings.TrimPrefix(progress.GetStatus().String(), "REPO_SYNC_STATUS_"))
		}
		routerWorkspaceSyncConflictsTotal.WithLabelValues(actionLabel, result.ConflictReason).Inc()
	case pb.RepoSyncStatus_REPO_SYNC_STATUS_AUTH_FAILED, pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR, pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED
	default:
		result.Status = pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED
	}
	if result.TargetBranch == "" {
		result.TargetBranch = repo.GetBranch()
	}
	return result
}

func workspaceEventTypeForRepository(repo *pb.WorkspaceSyncRepositoryResult) pb.WorkspaceSyncEventType {
	switch repo.GetStatus() {
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_UNCHANGED:
		return pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_REPOSITORY_COMPLETED
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESH_REQUIRED,
		pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESHED,
		pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED:
		return pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_REPOSITORY_COMPLETED
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT:
		return pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_REPOSITORY_CONFLICTED
	default:
		return pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_REPOSITORY_FAILED
	}
}

func cloneWorkspaceSyncRepositoryResult(repo *pb.WorkspaceSyncRepositoryResult) *pb.WorkspaceSyncRepositoryResult {
	if repo == nil {
		return nil
	}
	return proto.Clone(repo).(*pb.WorkspaceSyncRepositoryResult)
}

func workspaceSyncResultLabel(state pb.WorkspaceSyncState) string {
	switch state {
	case pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED:
		return "succeeded"
	case pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_CANCELLED:
		return "cancelled"
	default:
		return "failed"
	}
}

func workspaceSyncActionMetricLabel(action pb.WorkspaceSyncAction) string {
	switch action {
	case pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH:
		return "publish"
	case pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_REFRESH:
		return "refresh"
	case pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH:
		return "source_push"
	default:
		return "unknown"
	}
}

func workspaceSyncRepositoryMetricLabel(status pb.WorkspaceSyncRepositoryStatus) string {
	switch status {
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_UNCHANGED:
		return "unchanged"
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESH_REQUIRED:
		return "refresh_required"
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_REFRESHED:
		return "refreshed"
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED:
		return "published"
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CONFLICT:
		return "conflict"
	case pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_CANCELLED:
		return "cancelled"
	default:
		return "failed"
	}
}
