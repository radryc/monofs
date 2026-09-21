package fetcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	pb "github.com/radryc/monofs/api/proto"
)

// createUpstreamFixture builds a local bare remote with two commits, a tag, and
// a multi-line file for blame tests. Returns the remote path and both commit
// hashes.
func createUpstreamFixture(t *testing.T) (remotePath string, firstSHA, secondSHA string) {
	t.Helper()
	root := t.TempDir()
	remotePath = filepath.Join(root, "remote.git")
	if _, err := gogit.PlainInit(remotePath, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	seed := filepath.Join(root, "seed")
	repo, err := gogit.PlainInit(seed, false)
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	sig := &object.Signature{Name: "Test User", Email: "test@example.com", When: time.Now()}

	commit := func(file, content, msg string) plumbing.Hash {
		if err := os.WriteFile(filepath.Join(seed, file), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		if _, err := wt.Add(file); err != nil {
			t.Fatalf("add %s: %v", file, err)
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: sig})
		if err != nil {
			t.Fatalf("commit %s: %v", file, err)
		}
		return h
	}

	firstSHA = commit("README.md", "first\n", "first commit").String()
	secondSHA = commit("main.go", "line one\nline two\nline three\n", "second commit").String()

	if _, err := repo.CreateTag("v1.0.0", plumbing.NewHash(firstSHA), &gogit.CreateTagOptions{Message: "release", Tagger: sig}); err != nil {
		t.Fatalf("create tag: %v", err)
	}

	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remotePath}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	if err := repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"refs/heads/main:refs/heads/main", "refs/tags/*:refs/tags/*"},
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	return remotePath, firstSHA, secondSHA
}

func newUpstreamTestService(t *testing.T) *Service {
	t.Helper()
	cfg := DefaultServiceConfig()
	cfg.SyncRepoCacheDir = filepath.Join(t.TempDir(), "sync-cache")
	cfg.PrefetchWorkers = 0
	cfg.PrefetchQueueSize = 1
	return NewService("test", nil, cfg, slog.New(slog.DiscardHandler))
}

func TestGetUpstreamLog(t *testing.T) {
	remote, first, second := createUpstreamFixture(t)
	s := newUpstreamTestService(t)

	resp, err := s.GetUpstreamLog(context.Background(), &pb.UpstreamLogRequest{RepoUrl: remote, Branch: "main", Limit: 10})
	if err != nil {
		t.Fatalf("GetUpstreamLog: %v", err)
	}
	if len(resp.GetEntries()) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(resp.GetEntries()))
	}
	// Newest first.
	if resp.GetEntries()[0].GetHash() != second || resp.GetEntries()[1].GetHash() != first {
		t.Fatalf("unexpected order: %s, %s", resp.GetEntries()[0].GetHash(), resp.GetEntries()[1].GetHash())
	}
	if resp.GetEntries()[0].GetAuthorName() != "Test User" {
		t.Fatalf("unexpected author: %s", resp.GetEntries()[0].GetAuthorName())
	}
}

func TestGetUpstreamLogLimit(t *testing.T) {
	remote, _, _ := createUpstreamFixture(t)
	s := newUpstreamTestService(t)

	resp, err := s.GetUpstreamLog(context.Background(), &pb.UpstreamLogRequest{RepoUrl: remote, Branch: "main", Limit: 1})
	if err != nil {
		t.Fatalf("GetUpstreamLog: %v", err)
	}
	if len(resp.GetEntries()) != 1 {
		t.Fatalf("expected 1 entry (limit), got %d", len(resp.GetEntries()))
	}
}

func TestGetUpstreamTags(t *testing.T) {
	remote, first, _ := createUpstreamFixture(t)
	s := newUpstreamTestService(t)

	resp, err := s.GetUpstreamTags(context.Background(), &pb.UpstreamTagsRequest{RepoUrl: remote})
	if err != nil {
		t.Fatalf("GetUpstreamTags: %v", err)
	}
	if len(resp.GetTags()) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(resp.GetTags()))
	}
	if resp.GetTags()[0].GetName() != "v1.0.0" || resp.GetTags()[0].GetCommitHash() != first {
		t.Fatalf("unexpected tag: %+v", resp.GetTags()[0])
	}
}

func TestGetUpstreamBlame(t *testing.T) {
	remote, first, second := createUpstreamFixture(t)
	s := newUpstreamTestService(t)

	resp, err := s.GetUpstreamBlame(context.Background(), &pb.UpstreamBlameRequest{RepoUrl: remote, Branch: "main", Path: "main.go"})
	if err != nil {
		t.Fatalf("GetUpstreamBlame: %v", err)
	}
	if len(resp.GetLines()) != 3 {
		t.Fatalf("expected 3 blame lines, got %d", len(resp.GetLines()))
	}
	// All three lines were introduced in the second commit.
	for i, line := range resp.GetLines() {
		if line.GetHash() != second {
			t.Fatalf("line %d hash = %s, want %s", i+1, line.GetHash(), second)
		}
		if line.GetContent() == "" {
			t.Fatalf("line %d has empty content", i+1)
		}
	}
	_ = first
}
