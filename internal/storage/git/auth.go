package git

import (
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/radryc/monofs/internal/gittoken"
)

// authForRepo returns the HTTP auth method for a repository URL (go-git v5
// transport), or nil for ssh/local URLs and repositories without a token.
func authForRepo(repoURL string) transport.AuthMethod {
	if !gittoken.IsHTTPS(repoURL) {
		return nil
	}
	token := gittoken.TokenForRepo(repoURL)
	if token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: token}
}
