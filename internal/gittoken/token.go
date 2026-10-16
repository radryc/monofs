// Package gittoken resolves credentials for repository operations from the
// environment. Tokens are optional: public repositories work without any.
//
// Per-repo overrides win over the global token, using the env suffix derived
// from the repository owner and name (for example EXAMPLE_STRATA for
// github.com/example/strata):
//
//	MONOFS_GIT_TOKEN[_<OWNER>_<REPO>]     HTTPS clone/fetch/push token
//	MONOFS_GITHUB_TOKEN[_<OWNER>_<REPO>]  GitHub forge API token
//	MONOFS_GITLAB_TOKEN[_<OWNER>_<REPO>]  GitLab forge API token
//
// The git token is applied as HTTP BasicAuth (x-access-token for GitHub-style
// forges) to https:// clone, fetch and push operations.
package gittoken

import (
	"net/url"
	"os"
	"strings"
)

// TokenForRepo returns the HTTPS git token for a repository URL, or "".
func TokenForRepo(repoURL string) string {
	token := strings.TrimSpace(os.Getenv("MONOFS_GIT_TOKEN"))
	if slug := Slug(repoURL); slug != "" {
		if value := strings.TrimSpace(os.Getenv("MONOFS_GIT_TOKEN_" + slug)); value != "" {
			token = value
		}
	}
	return token
}

// ForgeTokens returns the GitHub and GitLab forge API tokens for a repository,
// honouring per-repo overrides.
func ForgeTokens(repoURL string) (githubToken, gitlabToken string) {
	githubToken = strings.TrimSpace(os.Getenv("MONOFS_GITHUB_TOKEN"))
	gitlabToken = strings.TrimSpace(os.Getenv("MONOFS_GITLAB_TOKEN"))
	slug := Slug(repoURL)
	if slug == "" {
		return githubToken, gitlabToken
	}
	if value := strings.TrimSpace(os.Getenv("MONOFS_GITHUB_TOKEN_" + slug)); value != "" {
		githubToken = value
	}
	if value := strings.TrimSpace(os.Getenv("MONOFS_GITLAB_TOKEN_" + slug)); value != "" {
		gitlabToken = value
	}
	return githubToken, gitlabToken
}

// Slug derives the env suffix for a repository clone URL:
// github.com/example/strata -> EXAMPLE_STRATA.
func Slug(repoURL string) string {
	owner, repo := ownerRepo(repoURL)
	if owner == "" && repo == "" {
		return ""
	}
	raw := owner + "_" + repo
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// IsHTTPS reports whether the repository URL uses the https/http transport.
func IsHTTPS(repoURL string) bool {
	return strings.HasPrefix(repoURL, "https://") || strings.HasPrefix(repoURL, "http://")
}

// ownerRepo extracts owner and repository name from a clone URL.
func ownerRepo(cloneURL string) (string, string) {
	value := strings.TrimSpace(cloneURL)
	if value == "" {
		return "", ""
	}
	var path string
	if strings.HasPrefix(value, "git@") {
		parts := strings.SplitN(value, ":", 2)
		if len(parts) == 2 {
			path = strings.TrimSuffix(parts[1], ".git")
		}
	} else {
		parsed, err := url.Parse(value)
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
