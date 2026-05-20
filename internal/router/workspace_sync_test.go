package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type workspaceSyncTestFetcherServer struct {
	pb.UnimplementedBlobFetcherServer
	pb.UnimplementedRepoSyncWorkerServer

	responses               []*pb.RepoSyncProgress
	publishResponses        []*pb.RepoSyncProgress
	commitPushResponses     []*pb.RepoSyncProgress
	stagedBundleBytes       int
	stagedCommitBundleBytes int
}

func (s *workspaceSyncTestFetcherServer) ProbeWorkspaceRefresh(req *pb.ProbeWorkspaceRefreshRequest, stream grpc.ServerStreamingServer[pb.RepoSyncProgress]) error {
	for _, resp := range s.responses {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (s *workspaceSyncTestFetcherServer) StageWorkspaceBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.StageWorkspaceBundleResponse]) error {
	bundleID := ""
	workspaceID := ""
	bytesReceived := 0
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		bundleID = chunk.GetBundleId()
		workspaceID = chunk.GetWorkspaceId()
		bytesReceived += len(chunk.GetData())
	}
	s.stagedBundleBytes += bytesReceived
	return stream.SendAndClose(&pb.StageWorkspaceBundleResponse{
		BundleId:      bundleID,
		WorkspaceId:   workspaceID,
		BytesReceived: int64(bytesReceived),
	})
}

func (s *workspaceSyncTestFetcherServer) StageWorkspaceCommitBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.StageWorkspaceBundleResponse]) error {
	bundleID := ""
	workspaceID := ""
	bytesReceived := 0
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		bundleID = chunk.GetBundleId()
		workspaceID = chunk.GetWorkspaceId()
		bytesReceived += len(chunk.GetData())
	}
	s.stagedCommitBundleBytes += bytesReceived
	return stream.SendAndClose(&pb.StageWorkspaceBundleResponse{
		BundleId:      bundleID,
		WorkspaceId:   workspaceID,
		BytesReceived: int64(bytesReceived),
	})
}

func (s *workspaceSyncTestFetcherServer) StartWorkspacePublish(req *pb.StartWorkspacePublishRequest, stream grpc.ServerStreamingServer[pb.RepoSyncProgress]) error {
	for _, resp := range s.publishResponses {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (s *workspaceSyncTestFetcherServer) StartWorkspaceCommitPush(req *pb.StartWorkspaceCommitPushRequest, stream grpc.ServerStreamingServer[pb.RepoSyncProgress]) error {
	for _, resp := range s.commitPushResponses {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (s *workspaceSyncTestFetcherServer) DiscardWorkspaceBundle(ctx context.Context, req *pb.DiscardWorkspaceBundleRequest) (*pb.DiscardWorkspaceBundleResponse, error) {
	return &pb.DiscardWorkspaceBundleResponse{Success: true, Message: "discarded"}, nil
}

func startWorkspaceSyncTestFetcher(t *testing.T, responses []*pb.RepoSyncProgress, publishResponses []*pb.RepoSyncProgress, commitPushResponses []*pb.RepoSyncProgress) (string, *workspaceSyncTestFetcherServer, func()) {
	t.Helper()

	serverImpl := &workspaceSyncTestFetcherServer{responses: responses, publishResponses: publishResponses, commitPushResponses: commitPushResponses}
	server := grpc.NewServer()
	pb.RegisterBlobFetcherServer(server, serverImpl)
	pb.RegisterRepoSyncWorkerServer(server, serverImpl)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fetcher: %v", err)
	}
	go func() {
		_ = server.Serve(lis)
	}()

	return lis.Addr().String(), serverImpl, func() {
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
	fetcherAddr, _, cleanupFetcher := startWorkspaceSyncTestFetcher(t, responses, nil, nil)
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

func TestPublishWorkspaceUploadsBundleAndStoresPublishJob(t *testing.T) {
	publishResponses := []*pb.RepoSyncProgress{{
		Repository: &pb.WorkspaceRepositoryRef{
			StorageId:   "repo-1",
			DisplayPath: "src/repo-1",
			RepoUrl:     "https://example.com/repo-1.git",
			Branch:      "main",
			BaseCommit:  "abc123",
		},
		Status:       pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED,
		RemoteCommit: "abc123",
		PushedCommit: "def456",
		TargetBranch: "main",
		Message:      "repository published",
	}}
	fetcherAddr, fetcherServer, cleanupFetcher := startWorkspaceSyncTestFetcher(t, nil, publishResponses, nil)
	defer cleanupFetcher()

	router := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() { _ = router.Close() }()
	if err := router.SetFetcherClient([]string{fetcherAddr}); err != nil {
		t.Fatalf("set fetcher client: %v", err)
	}

	client, cleanupRouter := startWorkspaceSyncTestRouter(t, router)
	defer cleanupRouter()

	bundleBytes, err := json.Marshal(workspacebundle.Bundle{
		WorkspaceID: "workspace-a",
		Repositories: []workspacebundle.RepositoryBundle{{
			StorageID:   "repo-1",
			DisplayPath: "src/repo-1",
			RepoURL:     "https://example.com/repo-1.git",
			Branch:      "main",
			BaseCommit:  "abc123",
			Operations: []workspacebundle.Operation{{
				Kind:    workspacebundle.OperationUpsert,
				Path:    "README.md",
				Content: []byte("published from test\n"),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal workspace bundle: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	upload, err := client.UploadWorkspaceBundle(ctx)
	if err != nil {
		t.Fatalf("open upload stream: %v", err)
	}
	if err := upload.Send(&pb.WorkspaceBundleChunk{WorkspaceId: "workspace-a", Data: bundleBytes, IsLast: true}); err != nil {
		t.Fatalf("send workspace bundle: %v", err)
	}
	uploadResp, err := upload.CloseAndRecv()
	if err != nil {
		t.Fatalf("close upload stream: %v", err)
	}
	if uploadResp.GetBundleId() == "" {
		t.Fatal("expected bundle id from upload response")
	}

	stream, err := client.PublishWorkspace(ctx, &pb.PublishWorkspaceRequest{
		WorkspaceId:             "workspace-a",
		BundleId:                uploadResp.GetBundleId(),
		LogicalCommitMessage:    "test publish",
		AuthorName:              "Test User",
		AuthorEmail:             "test@example.com",
		RequestedBranchStrategy: "direct",
	})
	if err != nil {
		t.Fatalf("publish workspace: %v", err)
	}

	var events []*pb.WorkspaceSyncEvent
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv publish event: %v", err)
		}
		events = append(events, event)
	}
	if len(events) < 3 {
		t.Fatalf("expected multiple publish events, got %d", len(events))
	}
	if got := events[len(events)-1].GetEventType(); got != pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_COMPLETED {
		t.Fatalf("expected final completion event, got %s", got.String())
	}

	jobs, err := router.ListWorkspaceSyncJobs(ctx, &pb.ListWorkspaceSyncJobsRequest{Action: pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_PUBLISH})
	if err != nil {
		t.Fatalf("list publish jobs: %v", err)
	}
	if len(jobs.GetJobs()) != 1 {
		t.Fatalf("expected 1 publish job, got %d", len(jobs.GetJobs()))
	}
	job := jobs.GetJobs()[0]
	if job.GetState() != pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED {
		t.Fatalf("expected succeeded publish job state, got %s", job.GetState().String())
	}
	if job.GetBundleId() != uploadResp.GetBundleId() {
		t.Fatalf("expected bundle id %q, got %q", uploadResp.GetBundleId(), job.GetBundleId())
	}
	if job.GetSummary().GetRepositoriesPublished() != 1 {
		t.Fatalf("expected 1 published repository, got %d", job.GetSummary().GetRepositoriesPublished())
	}
	if fetcherServer.stagedBundleBytes != len(bundleBytes) {
		t.Fatalf("expected staged bundle bytes %d, got %d", len(bundleBytes), fetcherServer.stagedBundleBytes)
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

func TestPushWorkspaceCommitsUploadsBundleAndStoresSourcePushJob(t *testing.T) {
	commitPushResponses := []*pb.RepoSyncProgress{{
		Repository: &pb.WorkspaceRepositoryRef{
			StorageId:   "repo-1",
			DisplayPath: "src/repo-1",
			RepoUrl:     "https://example.com/repo-1.git",
			Branch:      "main",
			BaseCommit:  "abc123",
		},
		Status:       pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED,
		RemoteCommit: "abc123",
		PushedCommit: "def456",
		TargetBranch: "feature/demo",
		Message:      "repository pushed from 2 local commits",
	}}
	fetcherAddr, fetcherServer, cleanupFetcher := startWorkspaceSyncTestFetcher(t, nil, nil, commitPushResponses)
	defer cleanupFetcher()

	router := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() { _ = router.Close() }()
	if err := router.SetFetcherClient([]string{fetcherAddr}); err != nil {
		t.Fatalf("set fetcher client: %v", err)
	}

	client, cleanupRouter := startWorkspaceSyncTestRouter(t, router)
	defer cleanupRouter()

	bundleBytes, err := json.Marshal(workspacebundle.SourceCommitBundle{
		WorkspaceID:   "workspace-a",
		LogicalBranch: "feature/demo",
		Commits: []workspacebundle.SourceCommit{
			{
				ID:      "local-1",
				Message: "first local commit",
				Repositories: []workspacebundle.SourceCommitRepository{{
					StorageID:   "repo-1",
					DisplayPath: "src/repo-1",
					RepoURL:     "https://example.com/repo-1.git",
					Branch:      "main",
					BaseCommit:  "abc123",
					Operations: []workspacebundle.Operation{{
						Kind:    workspacebundle.OperationUpsert,
						Path:    "README.md",
						Content: []byte("first\n"),
					}},
				}},
			},
			{
				ID:       "local-2",
				ParentID: "local-1",
				Message:  "second local commit",
				Repositories: []workspacebundle.SourceCommitRepository{{
					StorageID:   "repo-1",
					DisplayPath: "src/repo-1",
					RepoURL:     "https://example.com/repo-1.git",
					Branch:      "main",
					BaseCommit:  "abc123",
					Operations: []workspacebundle.Operation{{
						Kind:    workspacebundle.OperationUpsert,
						Path:    "README.md",
						Content: []byte("second\n"),
					}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal source commit bundle: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	upload, err := client.UploadWorkspaceCommitBundle(ctx)
	if err != nil {
		t.Fatalf("open source bundle upload stream: %v", err)
	}
	if err := upload.Send(&pb.WorkspaceBundleChunk{WorkspaceId: "workspace-a", Data: bundleBytes, IsLast: true}); err != nil {
		t.Fatalf("send source bundle: %v", err)
	}
	uploadResp, err := upload.CloseAndRecv()
	if err != nil {
		t.Fatalf("close source bundle upload: %v", err)
	}
	if uploadResp.GetBundleId() == "" {
		t.Fatal("expected bundle id from source upload response")
	}

	stream, err := client.PushWorkspaceCommits(ctx, &pb.PushWorkspaceCommitsRequest{
		WorkspaceId:   "workspace-a",
		BundleId:      uploadResp.GetBundleId(),
		LogicalBranch: "feature/demo",
	})
	if err != nil {
		t.Fatalf("push workspace commits: %v", err)
	}

	var events []*pb.WorkspaceSyncEvent
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv source push event: %v", err)
		}
		events = append(events, event)
	}
	if len(events) < 3 {
		t.Fatalf("expected multiple source push events, got %d", len(events))
	}
	if got := events[len(events)-1].GetEventType(); got != pb.WorkspaceSyncEventType_WORKSPACE_SYNC_EVENT_JOB_COMPLETED {
		t.Fatalf("expected final completion event, got %s", got.String())
	}

	jobs, err := router.ListWorkspaceSyncJobs(ctx, &pb.ListWorkspaceSyncJobsRequest{Action: pb.WorkspaceSyncAction_WORKSPACE_SYNC_ACTION_SOURCE_PUSH})
	if err != nil {
		t.Fatalf("list source push jobs: %v", err)
	}
	if len(jobs.GetJobs()) != 1 {
		t.Fatalf("expected 1 source push job, got %d", len(jobs.GetJobs()))
	}
	job := jobs.GetJobs()[0]
	if job.GetState() != pb.WorkspaceSyncState_WORKSPACE_SYNC_STATE_SUCCEEDED {
		t.Fatalf("expected succeeded source push job state, got %s", job.GetState().String())
	}
	if job.GetLogicalBranch() != "feature/demo" {
		t.Fatalf("logical branch = %q, want feature/demo", job.GetLogicalBranch())
	}
	if strings.Join(job.GetLocalCommitIds(), ",") != "local-1,local-2" {
		t.Fatalf("local commit ids = %v, want [local-1 local-2]", job.GetLocalCommitIds())
	}
	if job.GetSummary().GetRepositoriesPublished() != 1 {
		t.Fatalf("expected 1 published repository, got %d", job.GetSummary().GetRepositoriesPublished())
	}
	if fetcherServer.stagedCommitBundleBytes != len(bundleBytes) {
		t.Fatalf("expected staged commit bundle bytes %d, got %d", len(bundleBytes), fetcherServer.stagedCommitBundleBytes)
	}
}
