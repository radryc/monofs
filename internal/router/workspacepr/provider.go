package workspacepr

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

type PullRequestProvider interface {
	Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error)
	ProviderName() string
}

type CreatePRRequest struct {
	RepoCloneURL string
	SourceBranch string
	TargetBranch string
	Title        string
	Body         string
}

type CreatePRResult struct {
	WebURL  string
	ID      string
	Created bool
}

func DetectProvider(repoCloneURL string, gitLabBaseURL string) (PullRequestProvider, error) {
	host := parseHost(repoCloneURL)
	if host == "" {
		return nil, fmt.Errorf("cannot parse host from repo URL: %s", repoCloneURL)
	}

	if host == "github.com" {
		return &GitHubProvider{}, nil
	}

	if host == "gitlab.com" || (gitLabBaseURL != "" && host == parseHost(gitLabBaseURL)) {
		return &GitLabProvider{BaseURL: gitLabBaseURL}, nil
	}

	return nil, fmt.Errorf("unknown provider for host: %s", host)
}

func CompareURL(repoCloneURL, sourceBranch, targetBranch string) string {
	host := parseHost(repoCloneURL)
	owner, repo := parseOwnerRepo(repoCloneURL)

	switch {
	case host == "github.com":
		return fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s", owner, repo, url.PathEscape(targetBranch), url.PathEscape(sourceBranch))
	case host == "gitlab.com" || strings.Contains(host, "gitlab"):
		return fmt.Sprintf("https://%s/%s/%s/-/merge_requests/new?merge_request[source_branch]=%s&merge_request[target_branch]=%s", host, owner, repo, url.QueryEscape(sourceBranch), url.QueryEscape(targetBranch))
	default:
		return fmt.Sprintf("Create PR: %s → %s on %s", sourceBranch, targetBranch, repoCloneURL)
	}
}

func parseHost(cloneURL string) string {
	u := cloneURL
	if strings.HasPrefix(u, "git@") {
		parts := strings.SplitN(u, ":", 2)
		if len(parts) == 2 {
			return parts[0][4:]
		}
		return ""
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func parseOwnerRepo(cloneURL string) (string, string) {
	var path string
	if strings.HasPrefix(cloneURL, "git@") {
		parts := strings.SplitN(cloneURL, ":", 2)
		if len(parts) == 2 {
			path = strings.TrimSuffix(parts[1], ".git")
		}
	} else {
		parsed, err := url.Parse(cloneURL)
		if err == nil {
			path = strings.TrimSuffix(strings.TrimPrefix(parsed.Path, "/"), ".git")
		}
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", path
}

type GitHubProvider struct{}

func (p *GitHubProvider) Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error) {
	owner, repo := parseOwnerRepo(req.RepoCloneURL)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("cannot parse owner/repo from %s", req.RepoCloneURL)
	}
	webURL := fmt.Sprintf("https://github.com/%s/%s/pull/new/%s...%s", owner, repo, url.PathEscape(req.TargetBranch), url.PathEscape(req.SourceBranch))
	return &CreatePRResult{WebURL: webURL, Created: false}, nil
}

func (p *GitHubProvider) ProviderName() string { return "github" }

type GitLabProvider struct {
	BaseURL string
}

func (p *GitLabProvider) Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error) {
	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	owner, repo := parseOwnerRepo(req.RepoCloneURL)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("cannot parse owner/repo from %s", req.RepoCloneURL)
	}
	webURL := fmt.Sprintf("%s/%s/%s/-/merge_requests/new?merge_request[source_branch]=%s&merge_request[target_branch]=%s",
		baseURL, owner, repo, url.QueryEscape(req.SourceBranch), url.QueryEscape(req.TargetBranch))
	return &CreatePRResult{WebURL: webURL, Created: false}, nil
}

func (p *GitLabProvider) ProviderName() string { return "gitlab" }
