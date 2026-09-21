package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/radryc/monofs/internal/storage"
)

// createPinnedRemoteRepo builds a local bare remote with two commits and a tag
// on the tip, returning the remote path, the tip commit SHA, and the tag name.
func createPinnedRemoteRepo(t *testing.T) (remotePath, tipSHA, tag string) {
	t.Helper()
	root := t.TempDir()
	remotePath = filepath.Join(root, "remote.git")
	if _, err := gogit.PlainInit(remotePath, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}

	seedPath := filepath.Join(root, "seed")
	repo, err := gogit.PlainInit(seedPath, false)
	if err != nil {
		t.Fatalf("init seed repo: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set seed HEAD: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	sig := &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}

	mustCommit := func(file, content, msg string) plumbing.Hash {
		if err := os.WriteFile(filepath.Join(seedPath, file), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		if _, err := wt.Add(file); err != nil {
			t.Fatalf("stage %s: %v", file, err)
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: sig})
		if err != nil {
			t.Fatalf("commit %s: %v", file, err)
		}
		return h
	}

	mustCommit("README.md", "first\n", "first commit")
	tipSHA = mustCommit("file2.txt", "second\n", "second commit").String()

	tag = "v1.0.0"
	if _, err := repo.CreateTag(tag, plumbing.NewHash(tipSHA), &gogit.CreateTagOptions{
		Message: "release",
		Tagger:  sig,
	}); err != nil {
		t.Fatalf("create tag: %v", err)
	}

	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remotePath}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	refSpecs := []config.RefSpec{
		"refs/heads/main:refs/heads/main",
		"refs/tags/v1.0.0:refs/tags/v1.0.0",
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin", RefSpecs: refSpecs}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Set the bare remote's HEAD to main so a default-branch (full) clone can
	// resolve HEAD, as a real remote would.
	bare, err := gogit.PlainOpen(remotePath)
	if err != nil {
		t.Fatalf("open bare remote: %v", err)
	}
	if err := bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set bare remote HEAD: %v", err)
	}

	return remotePath, tipSHA, tag
}

func ingestAtRef(t *testing.T, remotePath, ref string) string {
	t.Helper()
	backend := NewGitIngestionBackend()
	ctx := context.Background()
	repoID := fmt.Sprintf("pinned-%d", time.Now().UnixNano())
	config := map[string]string{
		"branch":       ref,
		"display_path": repoID,
	}
	if err := backend.Initialize(ctx, remotePath, config); err != nil {
		t.Fatalf("initialize at ref %q: %v", ref, err)
	}
	defer backend.Cleanup()
	return config["commit_hash"]
}

func TestIngestAtBranch(t *testing.T) {
	remote, tipSHA, _ := createPinnedRemoteRepo(t)
	if got := ingestAtRef(t, remote, "main"); got != tipSHA {
		t.Fatalf("branch ingest commit_hash = %q, want %q", got, tipSHA)
	}
}

func TestIngestAtTag(t *testing.T) {
	remote, tipSHA, tag := createPinnedRemoteRepo(t)
	if got := ingestAtRef(t, remote, tag); got != tipSHA {
		t.Fatalf("tag ingest commit_hash = %q, want %q", got, tipSHA)
	}
}

func TestIngestAtSHA(t *testing.T) {
	remote, tipSHA, _ := createPinnedRemoteRepo(t)
	if got := ingestAtRef(t, remote, tipSHA); got != tipSHA {
		t.Fatalf("SHA ingest commit_hash = %q, want %q", got, tipSHA)
	}
}

func TestIngestProgrammaticBackendType(t *testing.T) {
	backend := NewGitIngestionBackend()
	if backend.Type() != storage.IngestionTypeGit {
		t.Fatalf("backend type = %v, want git", backend.Type())
	}
}
