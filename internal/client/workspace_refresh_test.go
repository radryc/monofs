package client

import (
	"testing"
)

func TestRefreshRequestForRepository(t *testing.T) {
	repo := WorkspaceRepository{
		DisplayPath: "github.com/acme/monofs",
		Source:      "git@example.com/acme/monofs.git",
		Ref:         "main",
		CommitHash:  "abc123",
	}

	req, err := refreshRequestForRepository(repo)
	if err != nil {
		t.Fatalf("refreshRequestForRepository() error = %v", err)
	}
	if req.GetRepoUrl() != repo.Source {
		t.Fatalf("request repo_url = %q, want %q", req.GetRepoUrl(), repo.Source)
	}
	if req.GetBranch() != repo.Ref {
		t.Fatalf("request branch = %q, want %q", req.GetBranch(), repo.Ref)
	}
	if req.GetDisplayPath() != repo.DisplayPath {
		t.Fatalf("request display_path = %q, want %q", req.GetDisplayPath(), repo.DisplayPath)
	}
	if req.GetBaseCommit() != repo.CommitHash {
		t.Fatalf("request base_commit = %q, want %q", req.GetBaseCommit(), repo.CommitHash)
	}
}

func TestRefreshRequestForRepositoryRequiresSource(t *testing.T) {
	_, err := refreshRequestForRepository(WorkspaceRepository{DisplayPath: "github.com/acme/monofs"})
	if err == nil {
		t.Fatal("refreshRequestForRepository() error = nil, want source validation")
	}
}
