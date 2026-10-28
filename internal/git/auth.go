package git

import (
	"github.com/go-git/go-git/v6/plumbing/client"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"

	"github.com/radryc/monofs/internal/gittoken"
)

// AuthForRepo returns HTTP client options for a repository URL. It returns nil
// for ssh/local URLs and for repositories without a configured token, so
// public repositories keep working.
func AuthForRepo(repoURL string) []client.Option {
	basic := basicAuthForRepo(repoURL)
	if basic == nil {
		return nil
	}
	return []client.Option{client.WithHTTPAuth(basic)}
}

func basicAuthForRepo(repoURL string) *githttp.BasicAuth {
	if !gittoken.IsHTTPS(repoURL) {
		return nil
	}
	token := gittoken.TokenForRepo(repoURL)
	if token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: token}
}
