package fetcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	pb "github.com/radryc/monofs/api/proto"
	monogit "github.com/radryc/monofs/internal/git"
)

func TestProbeWorkspaceRepositoryMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	writeCommit := func(name, content string) plumbing.Hash {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add: %v", err)
		}
		sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
		hash, err := wt.Commit(content, &gogit.CommitOptions{Author: sig, Committer: sig})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), hash)); err != nil {
			t.Fatalf("set main: %v", err)
		}
		return hash
	}

	first := writeCommit("README.md", "first")
	mgr, err := monogit.NewRepoManager(t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	svc := &Service{repoMgr: mgr}
	ref := &pb.WorkspaceRepositoryRef{
		StorageId:    "repo-1",
		DisplayPath:  "src/repo",
		RepoUrl:      dir,
		Branch:       "main",
		BaseCommit:   first.String(),
		MetadataOnly: true,
	}

	progress := svc.probeWorkspaceRepository(context.Background(), ref)
	if progress.GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED {
		t.Fatalf("unchanged status = %v, message = %q", progress.GetStatus(), progress.GetMessage())
	}

	second := writeCommit("README.md", "second")
	progress = svc.probeWorkspaceRepository(context.Background(), ref)
	if progress.GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED {
		t.Fatalf("advanced status = %v, message = %q", progress.GetStatus(), progress.GetMessage())
	}
	if progress.GetRemoteCommit() != second.String() {
		t.Fatalf("remote commit = %s, want %s", progress.GetRemoteCommit(), second)
	}

	ref.BaseCommit = ""
	progress = svc.probeWorkspaceRepository(context.Background(), ref)
	if progress.GetStatus() != pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR {
		t.Fatalf("missing base status = %v, message = %q", progress.GetStatus(), progress.GetMessage())
	}
}
