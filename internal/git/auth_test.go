package git

import (
	"testing"
)

func TestAuthForRepo(t *testing.T) {
	t.Setenv("MONOFS_GIT_TOKEN", "secret")
	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "")

	basic := basicAuthForRepo("https://github.com/example/strata.git")
	if basic == nil {
		t.Fatal("auth = nil, want BasicAuth")
	}
	if basic.Username != "x-access-token" || basic.Password != "secret" {
		t.Fatalf("auth = %+v", basic)
	}
	if opts := AuthForRepo("https://github.com/example/strata.git"); len(opts) != 1 {
		t.Fatalf("AuthForRepo options = %d, want 1", len(opts))
	}

	if basicAuthForRepo("git@github.com:example/strata.git") != nil {
		t.Error("ssh URLs must not use HTTP auth")
	}
	if basicAuthForRepo("/tmp/repo") != nil {
		t.Error("local paths must not use HTTP auth")
	}

	t.Setenv("MONOFS_GIT_TOKEN", "")
	if basicAuthForRepo("https://github.com/example/strata.git") != nil {
		t.Error("no token means no auth")
	}
	if opts := AuthForRepo("https://github.com/example/strata.git"); opts != nil {
		t.Errorf("AuthForRepo options = %v, want nil", opts)
	}
}

func TestAuthForRepoPerRepoOverride(t *testing.T) {
	t.Setenv("MONOFS_GIT_TOKEN", "global")
	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "repo")
	basic := basicAuthForRepo("https://github.com/example/strata.git")
	if basic == nil || basic.Password != "repo" {
		t.Fatalf("auth = %+v", basic)
	}
}
