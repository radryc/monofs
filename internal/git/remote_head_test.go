package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestRemoteHead(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("add: %v", err)
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	commit, err := wt.Commit("init", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("main"), commit)); err != nil {
		t.Fatalf("set main: %v", err)
	}

	rm, err := NewRepoManager(t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	head, err := rm.RemoteHead(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("RemoteHead: %v", err)
	}
	if head != commit.String() {
		t.Fatalf("head = %s, want %s", head, commit)
	}
}
