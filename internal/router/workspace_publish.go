package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
	"google.golang.org/grpc"
)

const workspaceBundleTTL = 30 * time.Minute

type stagedWorkspaceBundle struct {
	bundleID     string
	workspaceID  string
	data         []byte
	bundle       *workspacebundle.Bundle
	commitBundle *workspacebundle.SourceCommitBundle
	createdAt    time.Time
	expiresAt    time.Time
}

func (r *Router) UploadWorkspaceBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.UploadWorkspaceBundleResponse]) error {
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
				return fmt.Errorf("workspace_id changed within bundle upload")
			}
			workspaceID = chunk.GetWorkspaceId()
		}
		if len(chunk.GetData()) > 0 {
			if _, err := buf.Write(chunk.GetData()); err != nil {
				return fmt.Errorf("buffer workspace bundle: %w", err)
			}
		}
	}
	if buf.Len() == 0 {
		return fmt.Errorf("workspace bundle is empty")
	}
	bundle, err := workspacebundle.Parse(buf.Bytes())
	if err != nil {
		return err
	}
	if workspaceID == "" {
		workspaceID = bundle.WorkspaceID
	}
	if workspaceID != bundle.WorkspaceID {
		return fmt.Errorf("uploaded workspace_id %q does not match bundle workspace_id %q", workspaceID, bundle.WorkspaceID)
	}

	bundleID := fmt.Sprintf("wbundle-%d", time.Now().UnixNano())
	entry := &stagedWorkspaceBundle{
		bundleID:    bundleID,
		workspaceID: workspaceID,
		data:        append([]byte(nil), buf.Bytes()...),
		bundle:      bundle,
		createdAt:   time.Now(),
		expiresAt:   time.Now().Add(workspaceBundleTTL),
	}
	r.storeWorkspaceBundle(entry)
	routerWorkspaceSyncBundleBytesTotal.Add(float64(len(entry.data)))

	return stream.SendAndClose(&pb.UploadWorkspaceBundleResponse{
		BundleId:        bundleID,
		WorkspaceId:     workspaceID,
		BytesReceived:   int64(len(entry.data)),
		RepositoryCount: int32(len(bundle.Repositories)),
		ExpiresAtUnix:   entry.expiresAt.Unix(),
	})
}

func (r *Router) PublishWorkspace(req *pb.PublishWorkspaceRequest, stream pb.MonoFSRouter_PublishWorkspaceServer) error {
	start := time.Now()
	actionLabel := workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH)
	bundleEntry := r.getWorkspaceBundle(req.GetBundleId())
	if bundleEntry == nil {
		return fmt.Errorf("workspace bundle not found: %s", req.GetBundleId())
	}
	if req.GetWorkspaceId() != "" && req.GetWorkspaceId() != bundleEntry.workspaceID {
		return fmt.Errorf("publish workspace_id %q does not match bundle workspace_id %q", req.GetWorkspaceId(), bundleEntry.workspaceID)
	}

	job := r.newWorkspacePublishJob(req, bundleEntry.bundle, extractClientID(stream.Context()))
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
		Message:   "workspace publish job accepted",
	}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(stream.Context())
	entry.mu.Lock()
	entry.cancel = cancel
	entry.mu.Unlock()
	defer cancel()

	if err := r.runWorkspacePublishJob(ctx, entry, req, bundleEntry, func(event *pb.WorkspaceSyncEvent) error {
		return stream.Send(event)
	}); err != nil {
		return err
	}
	return nil
}

func (r *Router) runWorkspacePublishJob(ctx context.Context, entry *workspaceSyncJobEntry, req *pb.PublishWorkspaceRequest, bundleEntry *stagedWorkspaceBundle, send func(*pb.WorkspaceSyncEvent) error) error {
	actionLabel := workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH)
	r.updateWorkspaceSyncJob(entry, func(job *pb.WorkspaceSyncJob) {
		job.State = pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_RUNNING
		job.StartedAtUnix = time.Now().Unix()
	})
	if err := send(&pb.WorkspaceSyncEvent{
		EventType: pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_STARTED,
		Job:       entry.snapshot(),
		Message:   "workspace publish job started",
	}); err != nil {
		return err
	}

	fetcherClient := r.getFetcherClient()
	if fetcherClient == nil {
		r.failWorkspaceSyncJob(entry, workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH), "fetcher cluster not configured")
		return sendWorkspaceSyncTerminalEvent(send, entry, "workspace publish job failed")
	}
	defer func() {
		_ = fetcherClient.DiscardWorkspaceBundle(context.Background(), bundleEntry.workspaceID, req.GetBundleId())
	}()

	if _, err := fetcherClient.StageWorkspaceBundle(ctx, req.GetBundleId(), bundleEntry.workspaceID, bundleEntry.data); err != nil {
		r.failWorkspaceSyncJob(entry, workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH), err.Error())
		return sendWorkspaceSyncTerminalEvent(send, entry, "workspace publish job failed")
	}

	publishResults, err := fetcherClient.StartWorkspacePublish(ctx, &pb.StartWorkspacePublishRequest{
		JobId:                   entry.snapshot().GetJobId(),
		WorkspaceId:             bundleEntry.workspaceID,
		BundleId:                req.GetBundleId(),
		LogicalCommitMessage:    req.GetLogicalCommitMessage(),
		AuthorName:              req.GetAuthorName(),
		AuthorEmail:             req.GetAuthorEmail(),
		RequestedBranchStrategy: req.GetRequestedBranchStrategy(),
	})
	if err != nil {
		r.failWorkspaceSyncJob(entry, workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH), err.Error())
		return sendWorkspaceSyncTerminalEvent(send, entry, "workspace publish job failed")
	}

	for _, progress := range publishResults {
		select {
		case <-ctx.Done():
			r.cancelWorkspaceSyncJob(entry)
			return sendWorkspaceSyncTerminalEvent(send, entry, "workspace publish job cancelled")
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
	routerWorkspaceSyncJobsTotal.WithLabelValues(workspaceSyncActionMetricLabel(pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH), workspaceSyncResultLabel(entry.snapshot().GetState())).Inc()
	return sendWorkspaceSyncTerminalEvent(send, entry, "workspace publish job completed")
}

func (r *Router) newWorkspacePublishJob(req *pb.PublishWorkspaceRequest, bundle *workspacebundle.Bundle, clientID string) *pb.WorkspaceSyncJob {
	jobID := fmt.Sprintf("wsync-%d", time.Now().UnixNano())
	repositoriesTotal := 0
	workspaceID := req.GetWorkspaceId()
	if bundle != nil {
		repositoriesTotal = len(bundle.Repositories)
		if workspaceID == "" {
			workspaceID = bundle.WorkspaceID
		}
	}
	return &pb.WorkspaceSyncJob{
		JobId:                jobID,
		WorkspaceId:          workspaceID,
		Action:               pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH,
		State:                pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED,
		RequestedByClientId:  clientID,
		CreatedAtUnix:        time.Now().Unix(),
		Summary:              &pb.WorkspaceSyncSummary{RepositoriesTotal: int32(repositoriesTotal)},
		BundleId:             req.GetBundleId(),
		LogicalCommitMessage: req.GetLogicalCommitMessage(),
	}
}

func (r *Router) storeWorkspaceBundle(entry *stagedWorkspaceBundle) {
	r.workspaceBundleMu.Lock()
	r.workspaceBundles[entry.bundleID] = entry
	r.workspaceBundleMu.Unlock()
}

func (r *Router) getWorkspaceBundle(bundleID string) *stagedWorkspaceBundle {
	r.workspaceBundleMu.RLock()
	entry := r.workspaceBundles[bundleID]
	r.workspaceBundleMu.RUnlock()
	if entry == nil {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		r.workspaceBundleMu.Lock()
		delete(r.workspaceBundles, bundleID)
		r.workspaceBundleMu.Unlock()
		return nil
	}
	return entry
}

func sendWorkspaceSyncTerminalEvent(send func(*pb.WorkspaceSyncEvent) error, entry *workspaceSyncJobEntry, message string) error {
	return send(&pb.WorkspaceSyncEvent{
		EventType: pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_COMPLETED,
		Job:       entry.snapshot(),
		Message:   message,
	})
}
