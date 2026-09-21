package workspacepr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDetectGitHubProvider(t *testing.T) {
	prov, err := DetectProvider("https://github.com/org/repo.git", "", "", "")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if prov.ProviderName() != "github" {
		t.Fatalf("expected github, got %s", prov.ProviderName())
	}
}

func TestDetectGitLabProvider(t *testing.T) {
	prov, err := DetectProvider("https://gitlab.com/org/repo.git", "", "", "")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if prov.ProviderName() != "gitlab" {
		t.Fatalf("expected gitlab, got %s", prov.ProviderName())
	}
}

func TestDetectSelfHostedGitLab(t *testing.T) {
	prov, err := DetectProvider("https://git.internal.example.com/org/repo.git", "https://git.internal.example.com", "", "")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if prov.ProviderName() != "gitlab" {
		t.Fatalf("expected gitlab, got %s", prov.ProviderName())
	}
}

func TestDetectUnknownProvider(t *testing.T) {
	_, err := DetectProvider("https://bitbucket.org/org/repo.git", "", "", "")
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

func TestGitHubProviderCreatesPRViaAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", req.Method)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %q", got)
		}
		if !strings.HasSuffix(req.URL.Path, "/repos/org/repo/pulls") {
			t.Errorf("path = %q", req.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body["head"] != "monofs/ws/job" || body["base"] != "main" {
			t.Errorf("head/base = %q/%q", body["head"], body["base"])
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"html_url":"https://github.com/org/repo/pull/7","number":7}`)
	}))
	defer server.Close()

	prov := &GitHubProvider{Token: "test-token", APIBase: server.URL}
	result, err := prov.Create(context.Background(), CreatePRRequest{
		RepoCloneURL: "https://github.com/org/repo.git",
		SourceBranch: "monofs/ws/job",
		TargetBranch: "main",
		Title:        "test pr",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !result.Created || result.WebURL != "https://github.com/org/repo/pull/7" || result.ID != "7" {
		t.Fatalf("result = %+v", result)
	}
}

func TestGitHubProviderAlreadyExistsFallsBackToURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"message":"A pull request already exists"}`)
	}))
	defer server.Close()

	prov := &GitHubProvider{Token: "test-token", APIBase: server.URL}
	result, err := prov.Create(context.Background(), CreatePRRequest{
		RepoCloneURL: "https://github.com/org/repo.git",
		SourceBranch: "feature",
		TargetBranch: "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Created {
		t.Fatalf("result = %+v, want Created=false on 422", result)
	}
	if result.WebURL == "" {
		t.Fatal("expected fallback compare URL")
	}
}

func TestGitHubProviderWithoutTokenFabricatesURL(t *testing.T) {
	prov := &GitHubProvider{}
	result, err := prov.Create(context.Background(), CreatePRRequest{
		RepoCloneURL: "https://github.com/org/repo.git",
		SourceBranch: "feature",
		TargetBranch: "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Created || result.WebURL == "" {
		t.Fatalf("result = %+v, want fabricated URL", result)
	}
}

func TestGitLabProviderCreatesMRViaAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.EscapedPath(), "/api/v4/projects/org%2Frepo/merge_requests") {
			t.Errorf("path = %q", req.URL.EscapedPath())
		}
		if got := req.Header.Get("PRIVATE-TOKEN"); got != "test-token" {
			t.Errorf("private token = %q", got)
		}
		if err := req.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if req.Form.Get("source_branch") != "monofs/ws/job" || req.Form.Get("target_branch") != "main" {
			t.Errorf("branches = %q -> %q", req.Form.Get("source_branch"), req.Form.Get("target_branch"))
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"web_url":"https://gitlab.com/org/repo/-/merge_requests/3","iid":3}`)
	}))
	defer server.Close()

	prov := &GitLabProvider{BaseURL: server.URL, Token: "test-token"}
	result, err := prov.Create(context.Background(), CreatePRRequest{
		RepoCloneURL: "https://gitlab.com/org/repo.git",
		SourceBranch: "monofs/ws/job",
		TargetBranch: "main",
		Title:        "test mr",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !result.Created || result.WebURL != "https://gitlab.com/org/repo/-/merge_requests/3" || result.ID != "3" {
		t.Fatalf("result = %+v", result)
	}
}

func TestGitLabProviderAlreadyExistsFallsBackToURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"message":["Another open merge request for the same source branch exists"]}`)
	}))
	defer server.Close()

	prov := &GitLabProvider{BaseURL: server.URL, Token: "test-token"}
	result, err := prov.Create(context.Background(), CreatePRRequest{
		RepoCloneURL: "https://gitlab.com/org/repo.git",
		SourceBranch: "feature",
		TargetBranch: "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Created {
		t.Fatalf("result = %+v, want Created=false on 409", result)
	}
}
