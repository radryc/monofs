package search

import (
	"encoding/base64"
	"strings"
	"testing"
)

func envValues(env []string, key string) []string {
	var values []string
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == key {
			values = append(values, value)
		}
	}
	return values
}

func TestGitCloneEnvDisablesPrompts(t *testing.T) {
	t.Setenv("MONOFS_GIT_TOKEN", "")
	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")

	env := gitCloneEnv("https://github.com/example/strata.git")

	prompts := envValues(env, "GIT_TERMINAL_PROMPT")
	if len(prompts) != 1 || prompts[0] != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT = %v, want exactly [0]", prompts)
	}
	if values := envValues(env, "GIT_CONFIG_COUNT"); len(values) != 0 {
		t.Fatalf("GIT_CONFIG_COUNT = %v, want none without a token", values)
	}
}

func TestGitCloneEnvAppliesRepoToken(t *testing.T) {
	t.Setenv("MONOFS_GIT_TOKEN", "global-token")
	t.Setenv("MONOFS_GIT_TOKEN_EXAMPLE_STRATA", "repo-token")

	env := gitCloneEnv("https://github.com/example/strata.git")

	want := "Authorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte("x-access-token:repo-token"))
	if values := envValues(env, "GIT_CONFIG_VALUE_0"); len(values) != 1 || values[0] != want {
		t.Fatalf("GIT_CONFIG_VALUE_0 = %v, want [%q]", values, want)
	}
	if values := envValues(env, "GIT_CONFIG_KEY_0"); len(values) != 1 || values[0] != "http.extraHeader" {
		t.Fatalf("GIT_CONFIG_KEY_0 = %v, want [http.extraHeader]", values)
	}
}

func TestGitCloneEnvSkipsTokenForSSH(t *testing.T) {
	t.Setenv("MONOFS_GIT_TOKEN", "secret")

	env := gitCloneEnv("git@github.com:example/strata.git")

	if values := envValues(env, "GIT_CONFIG_COUNT"); len(values) != 0 {
		t.Fatalf("GIT_CONFIG_COUNT = %v, want none for ssh URLs", values)
	}
}
