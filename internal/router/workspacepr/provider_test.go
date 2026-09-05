package workspacepr

import (
	"testing"
)

func TestDetectGitHubProvider(t *testing.T) {
	prov, err := DetectProvider("https://github.com/org/repo.git", "")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if prov.ProviderName() != "github" {
		t.Fatalf("expected github, got %s", prov.ProviderName())
	}
}

func TestDetectGitLabProvider(t *testing.T) {
	prov, err := DetectProvider("https://gitlab.com/org/repo.git", "")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if prov.ProviderName() != "gitlab" {
		t.Fatalf("expected gitlab, got %s", prov.ProviderName())
	}
}

func TestDetectSelfHostedGitLab(t *testing.T) {
	prov, err := DetectProvider("https://git.internal.example.com/org/repo.git", "https://git.internal.example.com")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if prov.ProviderName() != "gitlab" {
		t.Fatalf("expected gitlab, got %s", prov.ProviderName())
	}
}

func TestDetectUnknownProvider(t *testing.T) {
	_, err := DetectProvider("https://bitbucket.org/org/repo.git", "")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestGitHubCompareURL(t *testing.T) {
	url := CompareURL("https://github.com/org/repo.git", "feature/x", "main")
	expected := "https://github.com/org/repo/compare/main...feature%2Fx"
	if url != expected {
		t.Fatalf("expected %s, got %s", expected, url)
	}
}

func TestGitLabCompareURL(t *testing.T) {
	url := CompareURL("https://gitlab.com/org/repo.git", "feature/y", "main")
	expected := "https://gitlab.com/org/repo/-/merge_requests/new?merge_request[source_branch]=feature%2Fy&merge_request[target_branch]=main"
	if url != expected {
		t.Fatalf("expected %s, got %s", expected, url)
	}
}

func TestParseSSHCloneURL(t *testing.T) {
	owner, repo := parseOwnerRepo("git@github.com:org/repo.git")
	if owner != "org" || repo != "repo" {
		t.Fatalf("expected org/repo, got %s/%s", owner, repo)
	}
}

func TestCompareURLUnknownProvider(t *testing.T) {
	url := CompareURL("https://bitbucket.org/org/repo.git", "feature/z", "main")
	if url == "" {
		t.Fatal("expected non-empty fallback URL")
	}
}
