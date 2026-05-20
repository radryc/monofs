package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
	"google.golang.org/grpc"
)

func (r *Router) UploadWorkspaceCommitBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.UploadWorkspaceBundleResponse]) error {
	var buf bytes.Buffer
	workspaceID := ""
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if chunk.GetWorkspaceId() != "" {
			if workspaceID != "" && workspaceID != chunk.GetWorkspaceId() {
				return fmt.Errorf("workspace_id changed within commit bundle upload")
			}
			workspaceID = chunk.GetWorkspaceId()
		}
		if len(chunk.GetData()) > 0 {
			if _, err := buf.Write(chunk.GetData()); err != nil {
				return fmt.Errorf("buffer source commit bundle: %w", err)
			}
		}
	}
	if buf.Len() == 0 {
		return fmt.Errorf("source commit bundle is empty")
	}
	bundle, err := workspacebundle.ParseSourceCommitBundle(buf.Bytes())
	if err != nil {
		return err
	}
	if workspaceID == "" {
		workspaceID = bundle.WorkspaceID
	}
	if workspaceID != bundle.WorkspaceID {
		return fmt.Errorf("uploaded workspace_id %q does not match bundle workspace_id %q", workspaceID, bundle.WorkspaceID)
	}

	bundleID := fmt.Sprintf("wcommit-%d", time.Now().UnixNano())
	entry := &stagedWorkspaceBundle{
		bundleID:     bundleID,
		workspaceID:  workspaceID,
		data:         append([]byte(nil), buf.Bytes()...),
		commitBundle: bundle,
		createdAt:    time.Now(),
		expiresAt:    time.Now().Add(workspaceBundleTTL),
	}
	r.storeWorkspaceBundle(entry)
	routerWorkspaceSyncBundleBytesTotal.Add(float64(len(entry.data)))

	return stream.SendAndClose(&pb.UploadWorkspaceBundleResponse{
		BundleId:        bundleID,
		WorkspaceId:     workspaceID,
		BytesReceived:   int64(len(entry.data)),
		RepositoryCount: int32(len(bundle.RepositoryRefs())),
		ExpiresAtUnix:   entry.expiresAt.Unix(),
	})
}

func (r *Router) PushWorkspaceCommits(req *pb.PushWorkspaceCommitsRequest, stream pb.MonoFSRouter_PushWorkspaceCommitsServer) error {
	start := time.Now()
	actionLabel := workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH)
	bundleEntry := r.getWorkspaceBundle(req.GetBundleId())
	if bundleEntry == nil || bundleEntry.commitBundle == nil {
		return fmt.Errorf("source commit bundle not found: %s", req.GetBundleId())
	}
	if req.GetWorkspaceId() != "" && req.GetWorkspaceId() != bundleEntry.workspaceID {
		return fmt.Errorf("source push workspace_id %q does not match bundle workspace_id %q", req.GetWorkspaceId(), bundleEntry.workspaceID)
	}
	logicalBranch, err := resolveSourcePushLogicalBranch(req.GetLogicalBranch(), bundleEntry.commitBundle)
	if err != nil {
		return err
	}

	job := r.newWorkspaceCommitPushJob(req, bundleEntry.commitBundle, logicalBranch, extractClientID(stream.Context()))
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
		Message:   "workspace source push job accepted",
	}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(stream.Context())
	entry.mu.Lock()
	entry.cancel = cancel
	entry.mu.Unlock()
	defer cancel()

	if err := r.runWorkspaceCommitPushJob(ctx, entry, req, logicalBranch, bundleEntry, func(event *pb.WorkspaceSyncEvent) error {
		return stream.Send(event)
	}); err != nil {
		return err
	}
	return nil
}

func (r *Router) runWorkspaceCommitPushJob(ctx context.Context, entry *workspaceSyncJobEntry, req *pb.PushWorkspaceCommitsRequest, logicalBranch string, bundleEntry *stagedWorkspaceBundle, send func(*pb.WorkspaceSyncEvent) error) error {
	actionLabel := workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH)
	r.updateWorkspaceSyncJob(entry, func(job *pb.WorkspaceSyncJob) {
		job.State = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_RUNNING
		job.StartedAtUnix = time.Now().Unix()
	})
	if err := send(&pb.WorkspaceSyncEvent{
		EventType: pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_STARTED,
		Job:       entry.snapshot(),
		Message:   "workspace source push job started",
	}); err != nil {
		return err
	}

	fetcherClient := r.getFetcherClient()
	if fetcherClient == nil {
		r.failWorkspaceSyncJob(entry, workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH), "fetcher cluster not configured")
		return sendWorkspaceSyncTerminalEvent(send, entry, "workspace source push job failed")
	}
	defer func() {
		_ = fetcherClient.DiscardWorkspaceBundle(context.Background(), bundleEntry.workspaceID, req.GetBundleId())
	}()

	if _, err := fetcherClient.StageWorkspaceCommitBundle(ctx, req.GetBundleId(), bundleEntry.workspaceID, bundleEntry.data); err != nil {
		r.failWorkspaceSyncJob(entry, workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH), err.Error())
		return sendWorkspaceSyncTerminalEvent(send, entry, "workspace source push job failed")
	}

	pushResults, err := fetcherClient.StartWorkspaceCommitPush(ctx, &pb.StartWorkspaceCommitPushRequest{
		JobId:         entry.snapshot().GetJobId(),
		WorkspaceId:   bundleEntry.workspaceID,
		BundleId:      req.GetBundleId(),
		LogicalBranch: logicalBranch,
	})
	if err != nil {
		r.failWorkspaceSyncJob(entry, workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH), err.Error())
		return sendWorkspaceSyncTerminalEvent(send, entry, "workspace source push job failed")
	}

	for _, progress := range pushResults {
		select {
		case <-ctx.Done():
			r.cancelWorkspaceSyncJob(entry)
			return sendWorkspaceSyncTerminalEvent(send, entry, "workspace source push job cancelled")
		default:
		}

		repoResult := workspaceRepositoryResultFromPublish(progress, actionLabel)
		r.updateWorkspaceSyncRepository(entry, repoResult)
		if err := send(&pb.WorkspaceSyncEvent{
			EventType:  workspaceEventTypeForRepository(repoResult),
			Job:        entry.snapshot(),
			Repository: cloneWorkspaceSyncRepositoryResult(repoResult),
			Message:    repoResult.GetMessage(),
		}); err != nil {
			return err
		}
	}

	r.finalizeWorkspaceSyncJob(entry)
	routerWorkspaceSyncJobsTotal.WithLabelValues(workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH), workspaceSyncResultLabel(entry.snapshot().GetState())).Inc()
	return sendWorkspaceSyncTerminalEvent(send, entry, "workspace source push job completed")
}

func (r *Router) newWorkspaceCommitPushJob(req *pb.PushWorkspaceCommitsRequest, bundle *workspacebundle.SourceCommitBundle, logicalBranch, clientID string) *pb.WorkspaceSyncJob {
	jobID := fmt.Sprintf("wsync-%d", time.Now().UnixNano())
	repositoriesTotal := 0
	workspaceID := req.GetWorkspaceId()
	localCommitIDs := []string(nil)
	if bundle != nil {
		repositoriesTotal = len(bundle.RepositoryRefs())
		localCommitIDs = bundle.LocalCommitIDs()
		if workspaceID == "" {
			workspaceID = bundle.WorkspaceID
		}
	}
	return &pb.WorkspaceSyncJob{
		JobId:               jobID,
		WorkspaceId:         workspaceID,
		Action:              pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH,
		State:               pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED,
		RequestedByClientId: clientID,
		CreatedAtUnix:       time.Now().Unix(),
		Summary:             &pb.WorkspaceSyncSummary{RepositoriesTotal: int32(repositoriesTotal)},
		BundleId:            req.GetBundleId(),
		LogicalBranch:       logicalBranch,
		LocalCommitIds:      localCommitIDs,
	}
}

func resolveSourcePushLogicalBranch(requested string, bundle *workspacebundle.SourceCommitBundle) (string, error) {
	bundleBranch := ""
	if bundle != nil {
		bundleBranch = strings.TrimSpace(bundle.LogicalBranch)
	}
	requested = strings.TrimSpace(requested)
	if requested != "" && bundleBranch != "" && requested != bundleBranch {
		return "", fmt.Errorf("source push logical_branch %q does not match bundle logical_branch %q", requested, bundleBranch)
	}
	if requested != "" {
		return requested, nil
	}
	return bundleBranch, nil
}
