package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type workspaceSyncTestFetcherServer struct {
	pb.UnimplementedBlobFetcherServer
	pb.UnimplementedRepoSyncWorkerServer

	responses []*pb.RepoSyncProgress
}

func (s *workspaceSyncTestFetcherServer) ProbeWorkspaceRefresh(req *pb.ProbeWorkspaceRefreshRequest, stream grpc.ServerStreamingServer[pb.RepoSyncProgress]) error {
	for _, resp := range s.responses {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func startWorkspaceSyncTestFetcher(t *testing.T, responses []*pb.RepoSyncProgress) (string, func()) {
	t.Helper()

	server := grpc.NewServer()
	pb.RegisterBlobFetcherServer(server, &workspaceSyncTestFetcherServer{responses: responses})
	pb.RegisterRepoSyncWorkerServer(server, &workspaceSyncTestFetcherServer{responses: responses})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fetcher: %v", err)
	}
	go func() {
		_ = server.Serve(lis)
	}()

	return lis.Addr().String(), func() {
		server.Stop()
		_ = lis.Close()
	}
}

func startWorkspaceSyncTestRouter(t *testing.T, r *Router) (pb.MonoFSRouterClient, func()) {
	t.Helper()

	server := grpc.NewServer()
	pb.RegisterMonoFSRouterServer(server, r)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen router: %v", err)
	}
	go func() {
		_ = server.Serve(lis)
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		server.Stop()
		_ = lis.Close()
		t.Fatalf("dial router: %v", err)
	}

	return pb.NewMonoFSRouterClient(conn), func() {
		_ = conn.Close()
		server.Stop()
		_ = lis.Close()
	}
}

func TestRefreshWorkspaceCompletesAndStoresJob(t *testing.T) {
	responses := []*pb.RepoSyncProgress{{
		Repository: &pb.WorkspaceRepositoryRef{
			StorageId:   "repo-1",
			DisplayPath: "src/repo-1",
			RepoUrl:     "https://example.com/repo-1.git",
			Branch:      "main",
			BaseCommit:  "abc123",
		},
		Status:       pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED,
		RemoteCommit: "abc123",
		Message:      "already up to date",
	}}
	fetcherAddr, cleanupFetcher := startWorkspaceSyncTestFetcher(t, responses)
	defer cleanupFetcher()

	router := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() { _ = router.Close() }()
	if err := router.SetFetcherClient([]string{fetcherAddr}); err != nil {
		t.Fatalf("set fetcher client: %v", err)
	}

	client, cleanupRouter := startWorkspaceSyncTestRouter(t, router)
	defer cleanupRouter()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.RefreshWorkspace(ctx, &pb.RefreshWorkspaceRequest{
		WorkspaceId: "workspace-a",
		Repositories: []*pb.WorkspaceRepositoryRef{{
			StorageId:   "repo-1",
			DisplayPath: "src/repo-1",
			RepoUrl:     "https://example.com/repo-1.git",
			Branch:      "main",
			BaseCommit:  "abc123",
		}},
	})
	if err != nil {
		t.Fatalf("refresh workspace: %v", err)
	}

	var events []*pb.WorkspaceSyncEvent
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv event: %v", err)
		}
		events = append(events, event)
	}
	if len(events) < 3 {
		t.Fatalf("expected multiple events, got %d", len(events))
	}
	if got := events[len(events)-1].GetEventType(); got != pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_COMPLETED {
		t.Fatalf("expected final completion event, got %s", got.String())
	}

	jobs, err := router.ListWorkspaceSyncJobs(ctx, &pb.ListWorkspaceSyncJobsRequest{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.GetJobs()) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs.GetJobs()))
	}
	job := jobs.GetJobs()[0]
	if job.GetState() != pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED {
		t.Fatalf("expected succeeded job state, got %s", job.GetState().String())
	}
	if job.GetSummary().GetRepositoriesSucceeded() != 1 {
		t.Fatalf("expected 1 succeeded repository, got %d", job.GetSummary().GetRepositoriesSucceeded())
	}
}

func TestWorkspaceSyncJobsAPIListsStoredJobs(t *testing.T) {
	router := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() { _ = router.Close() }()

	entry := &workspaceSyncJobEntry{job: &pb.WorkspaceSyncJob{
		JobId:         "job-1",
		WorkspaceId:   "workspace-a",
		Action:        pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_REFRESH,
		State:         pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED,
		CreatedAtUnix: time.Now().Unix(),
		Summary: &pb.WorkspaceSyncSummary{
			RepositoriesTotal:     1,
			RepositoriesSucceeded: 1,
		},
	}}
	router.storeWorkspaceSyncJob(entry)

	req := httptest.NewRequest(http.MethodGet, "/api/workspace-sync/jobs", nil)
	resp := httptest.NewRecorder()
	router.handleWorkspaceSyncJobsAPI(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Code)
	}

	var body pb.ListWorkspaceSyncJobsResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.GetJobs()) != 1 {
		t.Fatalf("expected 1 job, got %d", len(body.GetJobs()))
	}
	if body.GetJobs()[0].GetJobId() != "job-1" {
		t.Fatalf("unexpected job id %q", body.GetJobs()[0].GetJobId())
	}
}
