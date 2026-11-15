package router

import (
	"context"
	"log/slog"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
)

type fakeAutoPushPusher struct {
	staged   int
	pushed   int
	stageErr error
	pushErr  error
	status   pb.RepoSyncStatus
}

func (f *fakeAutoPushPusher) StageWorkspaceCommitBundle(context.Context, string, string, []byte) (*pb.StageWorkspaceBundleResponse, error) {
	f.staged++
	return &pb.StageWorkspaceBundleResponse{}, f.stageErr
}

func (f *fakeAutoPushPusher) StartWorkspaceCommitPush(context.Context, *pb.StartWorkspaceCommitPushRequest) ([]*pb.RepoSyncProgress, error) {
	f.pushed++
	return []*pb.RepoSyncProgress{{Status: f.status}}, f.pushErr
}

func newAutoPushTestRouter() *Router {
	return &Router{
		config:            RouterConfig{},
		logger:            slog.New(slog.DiscardHandler),
		workspaceSyncJobs: map[string]*workspaceSyncJobEntry{},
		workspaceBundles:  map[string]*stagedWorkspaceBundle{},
		nodes:             map[string]*nodeState{},
	}
}

func addSourcePushJob(r *Router, id, workspaceID string, state pb.WorkspaceSyncState) {
	r.workspaceSyncJobs[id] = &workspaceSyncJobEntry{job: &pb.WorkspaceSyncJob{
		WorkspaceId:   workspaceID,
		BundleId:      "bundle-" + workspaceID,
		LogicalBranch: "main",
		Action:        pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH,
		State:         state,
	}}
}

func TestAutoPushGetCandidatesFilters(t *testing.T) {
	r := newAutoPushTestRouter()
	addSourcePushJob(r, "queued", "ws-queued", pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED)
	addSourcePushJob(r, "running", "ws-running", pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_RUNNING)
	addSourcePushJob(r, "active", "ws-active", pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED)
	addSourcePushJob(r, "backoff", "ws-backoff", pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED)
	addSourcePushJob(r, "dead", "ws-dead", pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED)

	w := newAutoPushWorker(r, time.Minute, 1, r.logger)
	w.active["ws-active"] = true
	w.backoffUntil["ws-backoff"] = time.Now().Add(time.Hour)
	w.failureCount["ws-dead"] = deadLetterAfterFailures

	candidates := w.getCandidates()
	if len(candidates) != 1 || candidates[0] != "ws-queued" {
		t.Fatalf("candidates = %v, want [ws-queued]", candidates)
	}
}

func TestAutoPushTryPushSuccessResetsBackoff(t *testing.T) {
	r := newAutoPushTestRouter()
	addSourcePushJob(r, "job-1", "ws-1", pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED)
	r.workspaceBundles["bundle-ws-1"] = &stagedWorkspaceBundle{
		bundleID:    "bundle-ws-1",
		workspaceID: "ws-1",
		data:        []byte("bundle"),
		expiresAt:   time.Now().Add(time.Hour),
		commitBundle: &workspacebundle.SourceCommitBundle{
			WorkspaceID: "ws-1",
			Commits: []workspacebundle.SourceCommit{{
				Repositories: []workspacebundle.SourceCommitRepository{{StorageID: "repo-1"}},
			}},
		},
	}

	pusher := &fakeAutoPushPusher{status: pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED}
	w := newAutoPushWorker(r, time.Minute, 1, r.logger)
	w.pusher = pusher
	w.failureCount["ws-1"] = 3
	w.backoffUntil["ws-1"] = time.Now().Add(time.Hour)

	w.tryPush("ws-1")

	if pusher.staged != 1 || pusher.pushed != 1 {
		t.Fatalf("pusher staged=%d pushed=%d", pusher.staged, pusher.pushed)
	}
	if _, ok := w.failureCount["ws-1"]; ok {
		t.Fatal("failure count was not reset after a successful push")
	}
	if _, ok := w.backoffUntil["ws-1"]; ok {
		t.Fatal("backoff was not reset after a successful push")
	}
}

func TestAutoPushTryPushFailuresBackOff(t *testing.T) {
	r := newAutoPushTestRouter()
	addSourcePushJob(r, "job-1", "ws-1", pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_QUEUED)

	w := newAutoPushWorker(r, time.Minute, 1, r.logger)
	w.pusher = &fakeAutoPushPusher{}
	w.tryPush("ws-1") // bundle missing
	if w.failureCount["ws-1"] != 1 {
		t.Fatalf("failure count = %d, want 1", w.failureCount["ws-1"])
	}
	if until := w.backoffUntil["ws-1"]; until.Before(time.Now()) {
		t.Fatalf("backoff until %v is not in the future", until)
	}

	// Stage errors also count as failures.
	r.workspaceBundles["bundle-ws-1"] = &stagedWorkspaceBundle{
		commitBundle: &workspacebundle.SourceCommitBundle{
			Commits: []workspacebundle.SourceCommit{{
				Repositories: []workspacebundle.SourceCommitRepository{{StorageID: "repo-1"}},
			}},
		},
		data:      []byte("bundle"),
		expiresAt: time.Now().Add(time.Hour),
	}
	w.pusher = &fakeAutoPushPusher{stageErr: context.Canceled}
	w.tryPush("ws-1")
	if w.failureCount["ws-1"] != 2 {
		t.Fatalf("failure count = %d, want 2", w.failureCount["ws-1"])
	}
}

func TestAutoPushWorkerStopIsIdempotent(t *testing.T) {
	r := newAutoPushTestRouter()
	w := newAutoPushWorker(r, time.Minute, 1, r.logger)
	w.Start()
	w.Start() // start is idempotent
	w.Stop()
	w.Stop() // must not panic or block
}
