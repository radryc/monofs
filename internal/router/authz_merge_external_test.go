package router

import (
	"context"
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/router/workspacepr"
	"github.com/radryc/monofs/pkg/authz"
)

type fakePRProvider struct {
	last workspacepr.CreatePRRequest
}

func (f *fakePRProvider) Create(_ context.Context, req workspacepr.CreatePRRequest) (*workspacepr.CreatePRResult, error) {
	f.last = req
	return &workspacepr.CreatePRResult{WebURL: "https://example/pr/1", ID: "1", Created: true}, nil
}

func (f *fakePRProvider) ProviderName() string { return "fake" }

func TestBuildPRRequestBodyAndReviewers(t *testing.T) {
	mr := externalMergeRequest{
		RepoCloneURL: "https://github.com/org/repo",
		SourceBranch: "ws/contributor",
		TargetBranch: "main",
		Title:        "Update docs",
		UnownedPaths: []string{"a/b/x.txt"},
		Reviewers:    reviewersFromOwnerRefs([]authz.OwnerRef{{Team: "platform-eng"}, {Subject: "alice@example.com"}}),
	}
	req := buildPRRequest(mr)
	if req.Title != "Update docs" || req.TargetBranch != "main" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if !strings.Contains(req.Body, "a/b/x.txt") {
		t.Fatalf("body missing unowned path: %q", req.Body)
	}
	if !strings.Contains(req.Body, "@platform-eng") || !strings.Contains(req.Body, "alice@example.com") {
		t.Fatalf("body missing reviewers: %q", req.Body)
	}
}

func TestBuildPRRequestDefaultTitle(t *testing.T) {
	req := buildPRRequest(externalMergeRequest{RepoCloneURL: "x"})
	if strings.TrimSpace(req.Title) == "" {
		t.Fatal("expected a default title")
	}
}

func TestOpenExternalMergeRequest(t *testing.T) {
	provider := &fakePRProvider{}
	res, err := openExternalMergeRequest(context.Background(), provider, externalMergeRequest{
		RepoCloneURL: "https://github.com/org/repo",
		SourceBranch: "ws/x",
		TargetBranch: "main",
		Title:        "t",
		UnownedPaths: []string{"p"},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !res.Created || provider.last.SourceBranch != "ws/x" {
		t.Fatalf("unexpected result/provider state: res=%+v last=%+v", res, provider.last)
	}
}

func TestOpenExternalMergeRequestNilProvider(t *testing.T) {
	if _, err := openExternalMergeRequest(context.Background(), nil, externalMergeRequest{RepoCloneURL: "x"}); err == nil {
		t.Fatal("expected error for nil provider")
	}
}
