package git

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// gitHTTPBackendHandler serves GIT_PROJECT_ROOT over smart HTTP and requires
// basic auth, mirroring a private GitHub repository.
func gitHTTPBackendHandler(t *testing.T, projectRoot, username, password string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		cmd := exec.Command("git", "http-backend")
		env := append(os.Environ(),
			"GIT_PROJECT_ROOT="+projectRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"REQUEST_METHOD="+r.Method,
			"PATH_INFO="+r.URL.Path,
			"QUERY_STRING="+r.URL.RawQuery,
			"CONTENT_TYPE="+r.Header.Get("Content-Type"),
			"REMOTE_USER="+user,
		)
		if protocol := r.Header.Get("Git-Protocol"); protocol != "" {
			env = append(env, "GIT_PROTOCOL="+protocol)
		}
		cmd.Env = env
		cmd.Stdin = r.Body

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Logf("git http-backend: %v: %s", err, stderr.String())
			http.Error(w, "git http-backend failed", http.StatusInternalServerError)
			return
		}

		// CGI response: headers, blank line, body. Honour an optional Status.
		header, body, _ := strings.Cut(stdout.String(), "\r\n\r\n")
		if header == "" {
			header, body, _ = strings.Cut(stdout.String(), "\n\n")
		}
		status := http.StatusOK
		for _, line := range strings.Split(header, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			name, value, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(name), "Status") {
				code, _, _ := strings.Cut(strings.TrimSpace(value), " ")
				if parsed, err := strconv.Atoi(code); err == nil {
					status = parsed
				}
				continue
			}
			w.Header().Add(strings.TrimSpace(name), strings.TrimSpace(value))
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func TestGetDefaultBranchUsesAuthForPrivateRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	projectRoot := t.TempDir()
	source := filepath.Join(projectRoot, "source")
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(source, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(source, "add", ".")
	runGit(source, "commit", "-q", "-m", "init")
	runGit(projectRoot, "clone", "-q", "--bare", source, filepath.Join(projectRoot, "private.git"))

	server := httptest.NewServer(gitHTTPBackendHandler(t, projectRoot, "x-access-token", "secret"))
	defer server.Close()
	repoURL := server.URL + "/private.git"

	manager, err := NewRepoManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("MONOFS_GIT_TOKEN", "")
	if _, err := manager.GetDefaultBranch(context.Background(), repoURL); err == nil {
		t.Fatal("GetDefaultBranch without a token must fail for a private repository")
	}

	t.Setenv("MONOFS_GIT_TOKEN", "secret")
	branch, err := manager.GetDefaultBranch(context.Background(), repoURL)
	if err != nil {
		t.Fatalf("GetDefaultBranch with token: %v", err)
	}
	if branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
}
