package git

import (
	"testing"

	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
)

func TestAuthForRepo(t *testing.T) {
	t.Setenv("MONOFS_GIT_TOKEN", "secret")
	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "")

	auth := AuthForRepo("https://github.com/example/strata.git")
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("auth = %T, want BasicAuth", auth)
	}
	if basic.Username != "x-access-token" || basic.Password != "secret" {
		t.Fatalf("auth = %+v", basic)
	}

	if AuthForRepo("git@github.com:example/strata.git") != nil {
		t.Error("ssh URLs must not use HTTP auth")
	}
	if AuthForRepo("/tmp/repo") != nil {
		t.Error("local paths must not use HTTP auth")
	}

	t.Setenv("MONOFS_GIT_TOKEN", "")
	if AuthForRepo("https://github.com/example/strata.git") != nil {
		t.Error("no token means no auth")
	}
}

func TestAuthForRepoPerRepoOverride(t *testing.T) {
	t.Setenv("MONOFS_GIT_TOKEN", "global")
	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "repo")
	auth := AuthForRepo("https://github.com/example/strata.git")
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok || basic.Password != "repo" {
		t.Fatalf("auth = %+v", auth)
	}
}
