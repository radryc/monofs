package gittoken

import "testing"

func TestTokenForRepo(t *testing.T) {
	t.Setenv("MONOFS_GIT_TOKEN", "global-token")
	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "")

	if got := TokenForRepo("https://github.com/example/strata.git"); got != "global-token" {
		t.Fatalf("global token = %q", got)
	}

	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "repo-token")
	if got := TokenForRepo("https://github.com/example/strata.git"); got != "repo-token" {
		t.Fatalf("per-repo token = %q", got)
	}
	if got := TokenForRepo("git@github.com:example/strata.git"); got != "repo-token" {
		t.Fatalf("ssh url token = %q", got)
	}
	if got := TokenForRepo("https://github.com/example/other.git"); got != "global-token" {
		t.Fatalf("other repo token = %q", got)
	}

	t.Setenv("MONOFS_GIT_TOKEN", "")
	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "")
	if got := TokenForRepo("https://github.com/example/strata.git"); got != "" {
		t.Fatalf("empty token = %q", got)
	}
}

func TestForgeTokens(t *testing.T) {
	t.Setenv("MONOFS_GITHUB_TOKEN", "global-gh")
	t.Setenv("MONOFS_GITLAB_TOKEN", "global-gl")
	t.Setenv("MONOFS_GITHUB_TOKEN_EXAMPLE_STRATA", "repo-gh")
	t.Setenv("MONOFS_GITLAB_TOKEN_EXAMPLE_STRATA", "")

	github, gitlab := ForgeTokens("git@github.com:example/strata.git")
	if github != "repo-gh" || gitlab != "global-gl" {
		t.Fatalf("tokens = %q, %q", github, gitlab)
	}

	github, gitlab = ForgeTokens("https://gitlab.com/group/project.git")
	if github != "global-gh" || gitlab != "global-gl" {
		t.Fatalf("other tokens = %q, %q", github, gitlab)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"https://github.com/example/strata.git":    "EXAMPLE_STRATA",
		"git@github.com:example/strata.git":        "EXAMPLE_STRATA",
		"https://gitlab.com/group/sub/project.git": "GROUP_SUB_PROJECT",
		"https://github.com/my-org/my.repo.git":    "MY_ORG_MY_REPO",
		"":                                         "",
	}
	for input, want := range cases {
		if got := Slug(input); got != want {
			t.Errorf("Slug(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsHTTPS(t *testing.T) {
	if !IsHTTPS("https://github.com/example/strata.git") {
		t.Error("https must be true")
	}
	if !IsHTTPS("http://internal.example/repo") {
		t.Error("http must be true")
	}
	if IsHTTPS("git@github.com:example/strata.git") || IsHTTPS("/tmp/repo") {
		t.Error("ssh/local must be false")
	}
}
