package workspacepr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type PullRequestProvider interface {
	Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error)
	RequestReviewers(ctx context.Context, req ReviewersRequest) error
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

// ReviewersRequest requests reviewers for an already-created pull/merge
// request. Reviewers are candidate logins (GitHub) or usernames (GitLab); a
// leading "@" is tolerated and stripped. Team references may not resolve on
// every forge and failure is logged rather than fatal.
type ReviewersRequest struct {
	RepoCloneURL string
	PRNumber     string // GitHub pull number or GitLab MR iid
	Reviewers    []string
}

// DetectProvider returns the PR provider for a repository clone URL.
// Tokens are optional: without a token the provider falls back to
// fabricating a PR-creation web URL instead of calling the API.
func DetectProvider(repoCloneURL, gitLabBaseURL, githubToken, gitlabToken string) (PullRequestProvider, error) {
	host := parseHost(repoCloneURL)
	if host == "" {
		return nil, fmt.Errorf("cannot parse host from repo URL: %s", repoCloneURL)
	}

	if host == "github.com" {
		return &GitHubProvider{Token: githubToken}, nil
	}

	if host == "gitlab.com" || (gitLabBaseURL != "" && host == parseHost(gitLabBaseURL)) {
		return &GitLabProvider{BaseURL: gitLabBaseURL, Token: gitlabToken}, nil
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

var prHTTPClient = &http.Client{Timeout: 30 * time.Second}

// GitHubProvider creates pull requests through the GitHub REST API.
// Without a token it fabricates a compare URL instead.
type GitHubProvider struct {
	Token string
	// APIBase overrides https://api.github.com (for GitHub Enterprise).
	APIBase string
}

func (p *GitHubProvider) Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error) {
	owner, repo := parseOwnerRepo(req.RepoCloneURL)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("cannot parse owner/repo from %s", req.RepoCloneURL)
	}
	if p.Token == "" {
		return &CreatePRResult{WebURL: p.compareURL(owner, repo, req)}, nil
	}

	apiBase := p.APIBase
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}

	payload, err := json.Marshal(map[string]string{
		"title": req.Title,
		"body":  req.Body,
		"head":  req.SourceBranch,
		"base":  req.TargetBranch,
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/repos/%s/%s/pulls", apiBase, owner, repo), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.Token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := prHTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnprocessableEntity {
		// GitHub returns 422 when a pull request already exists for
		// these branches; surface the compare URL instead of failing.
		return &CreatePRResult{WebURL: p.compareURL(owner, repo, req)}, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github pull request API returned %d", resp.StatusCode)
	}

	var body struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &CreatePRResult{WebURL: body.HTMLURL, ID: fmt.Sprintf("%d", body.Number), Created: true}, nil
}

func (p *GitHubProvider) compareURL(owner, repo string, req CreatePRRequest) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/new/%s...%s",
		owner, repo, url.PathEscape(req.TargetBranch), url.PathEscape(req.SourceBranch))
}

func (p *GitHubProvider) ProviderName() string { return "github" }

// RequestReviewers requests reviewers on an existing pull request via the
// GitHub REST API. Without a token or a pull number it is a no-op.
func (p *GitHubProvider) RequestReviewers(ctx context.Context, req ReviewersRequest) error {
	if p.Token == "" || req.PRNumber == "" || len(req.Reviewers) == 0 {
		return nil
	}
	owner, repo := parseOwnerRepo(req.RepoCloneURL)
	if owner == "" || repo == "" {
		return fmt.Errorf("cannot parse owner/repo from %s", req.RepoCloneURL)
	}
	logins := make([]string, 0, len(req.Reviewers))
	for _, r := range req.Reviewers {
		if l := strings.TrimPrefix(strings.TrimSpace(r), "@"); l != "" {
			logins = append(logins, l)
		}
	}
	if len(logins) == 0 {
		return nil
	}

	apiBase := p.APIBase
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	payload, err := json.Marshal(map[string][]string{"reviewers": logins})
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/repos/%s/%s/pulls/%s/requested_reviewers", apiBase, owner, repo, req.PRNumber),
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.Token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := prHTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github requested_reviewers API returned %d", resp.StatusCode)
	}
	return nil
}

// GitLabProvider creates merge requests through the GitLab REST API.
// Without a token it fabricates a merge-request URL instead.
type GitLabProvider struct {
	BaseURL string
	Token   string
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
	if p.Token == "" {
		return &CreatePRResult{WebURL: p.newMRURL(baseURL, owner, repo, req)}, nil
	}

	projectPath := url.PathEscape(strings.TrimSuffix(strings.TrimPrefix(owner+"/"+repo, "/"), ".git"))
	form := url.Values{}
	form.Set("source_branch", req.SourceBranch)
	form.Set("target_branch", req.TargetBranch)
	form.Set("title", req.Title)
	form.Set("description", req.Body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v4/projects/%s/merge_requests", baseURL, projectPath),
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("PRIVATE-TOKEN", p.Token)
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := prHTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// GitLab returns 409 when a merge request already exists for
		// these branches.
		return &CreatePRResult{WebURL: p.newMRURL(baseURL, owner, repo, req)}, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab merge request API returned %d", resp.StatusCode)
	}

	var body struct {
		WebURL string `json:"web_url"`
		IID    int    `json:"iid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &CreatePRResult{WebURL: body.WebURL, ID: fmt.Sprintf("%d", body.IID), Created: true}, nil
}

func (p *GitLabProvider) newMRURL(baseURL, owner, repo string, req CreatePRRequest) string {
	return fmt.Sprintf("%s/%s/%s/-/merge_requests/new?merge_request[source_branch]=%s&merge_request[target_branch]=%s",
		baseURL, owner, repo, url.QueryEscape(req.SourceBranch), url.QueryEscape(req.TargetBranch))
}

func (p *GitLabProvider) ProviderName() string { return "gitlab" }

// RequestReviewers requests reviewers on an existing merge request via the
// GitLab REST API, resolving usernames to user IDs. Without a token or an MR
// iid it is a no-op; unresolvable usernames are skipped.
func (p *GitLabProvider) RequestReviewers(ctx context.Context, req ReviewersRequest) error {
	if p.Token == "" || req.PRNumber == "" || len(req.Reviewers) == 0 {
		return nil
	}
	owner, repo := parseOwnerRepo(req.RepoCloneURL)
	if owner == "" || repo == "" {
		return fmt.Errorf("cannot parse owner/repo from %s", req.RepoCloneURL)
	}
	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}

	ids := make([]int, 0, len(req.Reviewers))
	for _, r := range req.Reviewers {
		username := strings.TrimPrefix(strings.TrimSpace(r), "@")
		if username == "" {
			continue
		}
		id, err := p.resolveUserID(ctx, baseURL, username)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}

	projectPath := url.PathEscape(strings.TrimSuffix(strings.TrimPrefix(owner+"/"+repo, "/"), ".git"))
	form := url.Values{}
	for _, id := range ids {
		form.Add("reviewer_ids[]", strconv.Itoa(id))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%s", baseURL, projectPath, req.PRNumber),
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	httpReq.Header.Set("PRIVATE-TOKEN", p.Token)
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := prHTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gitlab merge request update API returned %d", resp.StatusCode)
	}
	return nil
}

// resolveUserID maps a GitLab username to its user ID (best-effort).
func (p *GitLabProvider) resolveUserID(ctx context.Context, baseURL, username string) (int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v4/users?username=%s", baseURL, url.QueryEscape(username)), nil)
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("PRIVATE-TOKEN", p.Token)

	resp, err := prHTTPClient.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("gitlab users API returned %d", resp.StatusCode)
	}
	var users []struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return 0, err
	}
	if len(users) == 0 {
		return 0, fmt.Errorf("gitlab user %q not found", username)
	}
	return users[0].ID, nil
}
