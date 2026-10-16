package router

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/workspacebundle"
)

func TestOpenPullRequestFabricatesURLWithoutToken(t *testing.T) {
	t.Setenv("MONOFS_GITHUB_TOKEN", "")
	t.Setenv("MONOFS_GITHUB_TOKEN_EXAMPLE_STRATA", "")
	t.Setenv("MONOFS_GITLAB_TOKEN", "")

	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	repoResult := &pb.WorkspaceSyncRepositoryResult{
		StorageId:    "repo-1",
		DisplayPath:  "github.com/example/strata",
		RepoUrl:      "https://github.com/example/strata.git",
		Branch:       "main",
		TargetBranch: "monofs/guardian-adopt-demo/repo-1",
		Status:       pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED,
		Message:      "repository published",
	}
	repo := workspacebundle.RepositoryBundle{
		StorageID:   "repo-1",
		DisplayPath: "github.com/example/strata",
		RepoURL:     "https://github.com/example/strata.git",
		Operations: []workspacebundle.Operation{{
			Kind: workspacebundle.OperationUpsert,
			Path: "guardian/partitions/demo/config.yaml",
		}},
	}

	r.attachPublishPullRequest(context.Background(), repoResult, repo, "Guardian adoption: demo", "job-1")

	if repoResult.GetPullRequestUrl() == "" {
		t.Fatal("expected a pull request URL to be fabricated")
	}
	if !strings.Contains(repoResult.GetPullRequestUrl(), "github.com/example/strata") {
		t.Fatalf("pull request url = %q", repoResult.GetPullRequestUrl())
	}
	if repoResult.GetPullRequestCreated() {
		t.Fatal("without a token the PR must not be reported as created")
	}
	if !strings.Contains(repoResult.GetMessage(), "open pull request") {
		t.Fatalf("message = %q", repoResult.GetMessage())
	}
}

func TestOpenPullRequestSkipsNonReviewBranches(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	repoResult := &pb.WorkspaceSyncRepositoryResult{
		RepoUrl:      "https://github.com/example/strata.git",
		Branch:       "main",
		TargetBranch: "main",
		Status:       pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED,
	}
	r.openPullRequest(context.Background(), repoResult, "t", "b", nil)
	if repoResult.GetPullRequestUrl() != "" {
		t.Fatal("direct pushes must not get a pull request")
	}

	failed := &pb.WorkspaceSyncRepositoryResult{
		RepoUrl:      "https://github.com/example/strata.git",
		Branch:       "main",
		TargetBranch: "monofs/review",
		Status:       pb.WorkspaceSyncRepositoryStatus_WORKSPACE_SYNC_REPOSITORY_STATUS_FAILED,
	}
	r.openPullRequest(context.Background(), failed, "t", "b", nil)
	if failed.GetPullRequestUrl() != "" {
		t.Fatal("failed pushes must not get a pull request")
	}
}

func TestChangedPathsFromRepositoryBundle(t *testing.T) {
	repo := workspacebundle.RepositoryBundle{
		DisplayPath: "github.com/example/strata",
		Operations: []workspacebundle.Operation{
			{Kind: workspacebundle.OperationUpsert, Path: "guardian/partitions/demo/config.yaml"},
			{Kind: workspacebundle.OperationUpsert, Path: "guardian/partitions/demo/intents/api.yaml"},
			{Kind: workspacebundle.OperationUpsert, Path: "guardian/partitions/demo/config.yaml"},
			{Kind: workspacebundle.OperationDelete, Path: "old/file.txt"},
		},
	}
	paths := changedPathsFromRepositoryBundle(repo)
	if len(paths) != 3 {
		t.Fatalf("paths = %v, want 3 distinct entries", paths)
	}
	for _, want := range []string{
		"github.com/example/strata/guardian/partitions/demo/config.yaml",
		"github.com/example/strata/guardian/partitions/demo/intents/api.yaml",
		"github.com/example/strata/old/file.txt",
	} {
		found := false
		for _, path := range paths {
			if path == want {
				found = true
			}
		}
		if !found {
			t.Errorf("paths missing %q: %v", want, paths)
		}
	}
}

func TestRepositoryBundleByStorageID(t *testing.T) {
	bundle := &workspacebundle.Bundle{
		WorkspaceID: "ws",
		Repositories: []workspacebundle.RepositoryBundle{
			{StorageID: "a", DisplayPath: "x"},
			{StorageID: "b", DisplayPath: "y"},
		},
	}
	repo, ok := repositoryBundleByStorageID(bundle, "b")
	if !ok || repo.DisplayPath != "y" {
		t.Fatalf("lookup = %+v, %v", repo, ok)
	}
	if _, ok := repositoryBundleByStorageID(bundle, "missing"); ok {
		t.Fatal("expected missing repository")
	}
}

func TestPublishPullRequestBodyListsFiles(t *testing.T) {
	body := publishPullRequestBody(workspacebundle.RepositoryBundle{
		StorageID:   "repo-1",
		DisplayPath: "github.com/example/strata",
		Operations: []workspacebundle.Operation{
			{Kind: workspacebundle.OperationUpsert, Path: "guardian/partitions/demo/config.yaml"},
		},
	}, "job-9")
	for _, want := range []string{"github.com/example/strata", "upsert guardian/partitions/demo/config.yaml", "MonoFS-Job: job-9"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}
