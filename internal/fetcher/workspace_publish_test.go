package fetcher

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestStageAndPublishWorkspaceBundle(t *testing.T) {
	remotePath, baseCommit := createPublishRemoteRepo(t)
	client, cleanup := startRepoSyncWorkerTestClient(t)
	defer cleanup()

	bundleBytes, err := json.Marshal(workspacebundle.Bundle{
		WorkspaceID: "workspace-a",
		Repositories: []workspacebundle.RepositoryBundle{{
			StorageID:   "repo-1",
			DisplayPath: "src/repo-1",
			RepoURL:     remotePath,
			Branch:      "main",
			BaseCommit:  baseCommit,
			Operations: []workspacebundle.Operation{{
				Kind:    workspacebundle.OperationUpsert,
				Path:    "README.md",
				Content: []byte("published from fetcher test\n"),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal workspace bundle: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stage, err := client.StageWorkspaceBundle(ctx)
	if err != nil {
		t.Fatalf("open stage stream: %v", err)
	}
	if err := stage.Send(&pb.WorkspaceBundleChunk{
		WorkspaceId: "workspace-a",
		BundleId:    "bundle-1",
		Data:        bundleBytes,
		IsLast:      true,
	}); err != nil {
		t.Fatalf("send stage chunk: %v", err)
	}
	stageResp, err := stage.CloseAndRecv()
	if err != nil {
		t.Fatalf("close stage stream: %v", err)
	}
	if stageResp.GetBytesReceived() != int64(len(bundleBytes)) {
		t.Fatalf("expected %d staged bytes, got %d", len(bundleBytes), stageResp.GetBytesReceived())
	}

	publish, err := client.StartWorkspacePublish(ctx, &pb.StartWorkspacePublishRequest{
		JobId:                   "job-1",
		WorkspaceId:             "workspace-a",
		BundleId:                "bundle-1",
		LogicalCommitMessage:    "publish from test",
		AuthorName:              "Test User",
		AuthorEmail:             "test@example.com",
		RequestedBranchStrategy: "direct",
	})
	if err != nil {
		t.Fatalf("start publish: %v", err)
	}

	var results []*pb.RepoSyncProgress
	for {
		progress, err := publish.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv publish progress: %v", err)
		}
		results = append(results, progress)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 publish result, got %d", len(results))
	}
	if results[0].GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED {
		t.Fatalf("expected published status, got %s", results[0].GetStatus().String())
	}
	if results[0].GetPushedCommit() == "" {
		t.Fatal("expected pushed commit hash")
	}

	content := readRemoteFile(t, remotePath, "main", "README.md")
	if string(content) != "published from fetcher test\n" {
		t.Fatalf("unexpected remote file content: %q", string(content))
	}

	statsResp, err := client.GetSyncWorkerStats(ctx, &pb.SyncWorkerStatsRequest{})
	if err != nil {
		t.Fatalf("get sync worker stats: %v", err)
	}
	if statsResp.GetStats().GetPublishJobs() != 1 {
		t.Fatalf("expected 1 publish job, got %d", statsResp.GetStats().GetPublishJobs())
	}
	if statsResp.GetStats().GetPublishedRepositories() != 1 {
		t.Fatalf("expected 1 published repository, got %d", statsResp.GetStats().GetPublishedRepositories())
	}

	if _, err := client.DiscardWorkspaceBundle(ctx, &pb.DiscardWorkspaceBundleRequest{BundleId: "bundle-1"}); err != nil {
		t.Fatalf("discard staged bundle: %v", err)
	}
}

func TestStageAndSourcePushWorkspaceCommitBundle(t *testing.T) {
	remotePath, baseCommit := createPublishRemoteRepo(t)
	client, cleanup := startRepoSyncWorkerTestClient(t)
	defer cleanup()

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
					RepoURL:     remotePath,
					Branch:      "main",
					BaseCommit:  baseCommit,
					Operations: []workspacebundle.Operation{{
						Kind:    workspacebundle.OperationUpsert,
						Path:    "README.md",
						Content: []byte("first local change\n"),
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
					RepoURL:     remotePath,
					Branch:      "main",
					BaseCommit:  baseCommit,
					Operations: []workspacebundle.Operation{{
						Kind:    workspacebundle.OperationUpsert,
						Path:    "README.md",
						Content: []byte("second local change\n"),
					}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal source commit bundle: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stage, err := client.StageWorkspaceCommitBundle(ctx)
	if err != nil {
		t.Fatalf("open source stage stream: %v", err)
	}
	if err := stage.Send(&pb.WorkspaceBundleChunk{
		WorkspaceId: "workspace-a",
		BundleId:    "commit-bundle-1",
		Data:        bundleBytes,
		IsLast:      true,
	}); err != nil {
		t.Fatalf("send source stage chunk: %v", err)
	}
	stageResp, err := stage.CloseAndRecv()
	if err != nil {
		t.Fatalf("close source stage stream: %v", err)
	}
	if stageResp.GetBytesReceived() != int64(len(bundleBytes)) {
		t.Fatalf("expected %d staged bytes, got %d", len(bundleBytes), stageResp.GetBytesReceived())
	}

	pushStream, err := client.StartWorkspaceCommitPush(ctx, &pb.StartWorkspaceCommitPushRequest{
		JobId:         "job-source-1",
		WorkspaceId:   "workspace-a",
		BundleId:      "commit-bundle-1",
		LogicalBranch: "feature/demo",
	})
	if err != nil {
		t.Fatalf("start source push: %v", err)
	}

	var results []*pb.RepoSyncProgress
	for {
		progress, err := pushStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv source push progress: %v", err)
		}
		results = append(results, progress)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 source push result, got %d", len(results))
	}
	if results[0].GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED {
		t.Fatalf("expected published status, got %s", results[0].GetStatus().String())
	}
	if results[0].GetTargetBranch() != "feature/demo" {
		t.Fatalf("target branch = %q, want feature/demo", results[0].GetTargetBranch())
	}
	if results[0].GetPushedCommit() == "" {
		t.Fatal("expected pushed commit hash")
	}

	content := readRemoteFile(t, remotePath, "feature/demo", "README.md")
	if string(content) != "second local change\n" {
		t.Fatalf("unexpected remote file content: %q", string(content))
	}
}

func startRepoSyncWorkerTestClient(t *testing.T) (pb.RepoSyncWorkerClient, func()) {
	t.Helper()

	cfg := DefaultServiceConfig()
	cfg.SyncRepoCacheDir = t.TempDir()
	service := NewService("test-fetcher", NewRegistry(), cfg, slog.Default())

	server := grpc.NewServer()
	service.RegisterService(server)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		service.Close()
		t.Fatalf("listen sync worker: %v", err)
	}
	go func() {
		_ = server.Serve(lis)
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		server.Stop()
		_ = lis.Close()
		_ = service.Close()
		t.Fatalf("dial sync worker: %v", err)
	}

	return pb.NewRepoSyncWorkerClient(conn), func() {
		_ = conn.Close()
		server.Stop()
		_ = lis.Close()
		_ = service.Close()
	}
}

func createPublishRemoteRepo(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	remotePath := filepath.Join(root, "remote.git")
	if _, err := gogit.PlainInit(remotePath, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}

	seedPath := filepath.Join(root, "seed")
	repo, err := gogit.PlainInit(seedPath, false)
	if err != nil {
		t.Fatalf("init seed repo: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set seed HEAD to main: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("seed repo worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedPath, "README.md"), []byte("initial\n"), 0644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("stage seed file: %v", err)
	}
	commitHash, err := wt.Commit("initial commit", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Seed User", Email: "seed@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit seed file: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remotePath}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{"refs/heads/main:refs/heads/main"}}); err != nil {
		t.Fatalf("push seed commit: %v", err)
	}

	return remotePath, commitHash.String()
}

func readRemoteFile(t *testing.T, remotePath, branch, filePath string) []byte {
	t.Helper()
	clonePath := filepath.Join(t.TempDir(), "clone")
	_, err := gogit.PlainClone(clonePath, &gogit.CloneOptions{
		URL:           remotePath,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
	})
	if err != nil {
		t.Fatalf("clone remote for verification: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(clonePath, filePath))
	if err != nil {
		t.Fatalf("read cloned file: %v", err)
	}
	return content
}
