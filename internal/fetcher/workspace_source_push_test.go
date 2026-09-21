package fetcher

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
)

// stageCommitBundle uploads a source commit bundle to the test service.
func stageCommitBundle(t *testing.T, ctx context.Context, client pb.RepoSyncWorkerClient, bundle *workspacebundle.SourceCommitBundle, bundleID string) {
	t.Helper()

	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal source commit bundle: %v", err)
	}

	stage, err := client.StageWorkspaceCommitBundle(ctx)
	if err != nil {
		t.Fatalf("open stage commit bundle stream: %v", err)
	}
	if err := stage.Send(&pb.WorkspaceBundleChunk{
		WorkspaceId: bundle.WorkspaceID,
		BundleId:    bundleID,
		Data:        bundleBytes,
		IsLast:      true,
	}); err != nil {
		t.Fatalf("send stage chunk: %v", err)
	}
	if _, err := stage.CloseAndRecv(); err != nil {
		t.Fatalf("close stage stream: %v", err)
	}
}

func TestSourcePushRollsBackAfterPartialFailure(t *testing.T) {
	remoteA, baseA := createPublishRemoteRepo(t)
	remoteB, _ := createPublishRemoteRepo(t)
	client, cleanup := startRepoSyncWorkerTestClient(t)
	defer cleanup()

	// One logical commit spanning two repositories. Repo B carries a
	// stale base commit so its push fails after repo A published.
	bundle := &workspacebundle.SourceCommitBundle{
		WorkspaceID: "workspace-rollback",
		Commits: []workspacebundle.SourceCommit{{
			ID:      "local-1",
			Message: "cross-repo change",
			Repositories: []workspacebundle.SourceCommitRepository{
				{
					StorageID:   "repo-a",
					DisplayPath: "src/aaa",
					RepoURL:     remoteA,
					Branch:      "main",
					BaseCommit:  baseA,
					Operations: []workspacebundle.Operation{{
						Kind:    workspacebundle.OperationUpsert,
						Path:    "README.md",
						Content: []byte("published then rolled back\n"),
					}},
				},
				{
					StorageID:   "repo-b",
					DisplayPath: "src/bbb",
					RepoURL:     remoteB,
					Branch:      "main",
					BaseCommit:  "0000000000000000000000000000000000000000",
					Operations: []workspacebundle.Operation{{
						Kind:    workspacebundle.OperationUpsert,
						Path:    "README.md",
						Content: []byte("never published\n"),
					}},
				},
			},
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stageCommitBundle(t, ctx, client, bundle, "bundle-rollback")

	stream, err := client.StartWorkspaceCommitPush(ctx, &pb.StartWorkspaceCommitPushRequest{
		JobId:          "job-rollback",
		WorkspaceId:    "workspace-rollback",
		BundleId:       "bundle-rollback",
		SourcePushMode: "squash",
	})
	if err != nil {
		t.Fatalf("start source push: %v", err)
	}

	byRepo := map[string][]*pb.RepoSyncProgress{}
	var order []string
	for {
		progress, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv push progress: %v", err)
		}
		repo := progress.GetRepository().GetStorageId()
		if _, seen := byRepo[repo]; !seen {
			order = append(order, repo)
		}
		byRepo[repo] = append(byRepo[repo], progress)
	}

	// Repo A (src/aaa) publishes first.
	aProgress := byRepo["repo-a"]
	if len(aProgress) == 0 || aProgress[0].GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED {
		t.Fatalf("repo-a progress = %+v, want published", aProgress)
	}
	// Repo B fails on the stale base commit.
	bProgress := byRepo["repo-b"]
	if len(bProgress) == 0 || bProgress[0].GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_CONFLICT {
		t.Fatalf("repo-b progress = %+v, want conflict", bProgress)
	}
	if bProgress[0].GetConflictReason() != "base_commit_mismatch" {
		t.Fatalf("repo-b conflict reason = %q, want base_commit_mismatch", bProgress[0].GetConflictReason())
	}
	// Repo A is then rolled back to its base commit.
	if len(aProgress) < 2 || aProgress[1].GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_ROLLED_BACK {
		t.Fatalf("repo-a rollback progress = %+v, want rolled_back", aProgress)
	}

	// The remote content of repo A is restored to the base state.
	if content := readRemoteFile(t, remoteA, "main", "README.md"); string(content) != "initial\n" {
		t.Fatalf("repo-a remote content after rollback = %q, want initial", string(content))
	}
	// Repo B never received the change.
	if content := readRemoteFile(t, remoteB, "main", "README.md"); string(content) != "initial\n" {
		t.Fatalf("repo-b remote content = %q, want initial", string(content))
	}
}

func TestSourcePushAllRepositoriesSucceedNoRollback(t *testing.T) {
	remoteA, baseA := createPublishRemoteRepo(t)
	remoteB, baseB := createPublishRemoteRepo(t)
	client, cleanup := startRepoSyncWorkerTestClient(t)
	defer cleanup()

	bundle := &workspacebundle.SourceCommitBundle{
		WorkspaceID: "workspace-ok",
		Commits: []workspacebundle.SourceCommit{{
			ID:      "local-1",
			Message: "cross-repo change",
			Repositories: []workspacebundle.SourceCommitRepository{
				{
					StorageID:   "repo-a",
					DisplayPath: "src/aaa",
					RepoURL:     remoteA,
					Branch:      "main",
					BaseCommit:  baseA,
					Operations: []workspacebundle.Operation{{
						Kind:    workspacebundle.OperationUpsert,
						Path:    "README.md",
						Content: []byte("from repo a\n"),
					}},
				},
				{
					StorageID:   "repo-b",
					DisplayPath: "src/bbb",
					RepoURL:     remoteB,
					Branch:      "main",
					BaseCommit:  baseB,
					Operations: []workspacebundle.Operation{{
						Kind:    workspacebundle.OperationUpsert,
						Path:    "README.md",
						Content: []byte("from repo b\n"),
					}},
				},
			},
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stageCommitBundle(t, ctx, client, bundle, "bundle-ok")

	stream, err := client.StartWorkspaceCommitPush(ctx, &pb.StartWorkspaceCommitPushRequest{
		JobId:          "job-ok",
		WorkspaceId:    "workspace-ok",
		BundleId:       "bundle-ok",
		SourcePushMode: "squash",
	})
	if err != nil {
		t.Fatalf("start source push: %v", err)
	}

	published := 0
	for {
		progress, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv push progress: %v", err)
		}
		switch progress.GetStatus() {
		case pb.RepoSyncStatus_REPO_SYNC_STATUS_PUBLISHED:
			published++
		case pb.RepoSyncStatus_REPO_SYNC_STATUS_ROLLED_BACK:
			t.Fatalf("unexpected rollback: %+v", progress)
		}
	}
	if published != 2 {
		t.Fatalf("published = %d, want 2", published)
	}

	if content := readRemoteFile(t, remoteA, "main", "README.md"); string(content) != "from repo a\n" {
		t.Fatalf("repo-a content = %q", string(content))
	}
	if content := readRemoteFile(t, remoteB, "main", "README.md"); string(content) != "from repo b\n" {
		t.Fatalf("repo-b content = %q", string(content))
	}
}
