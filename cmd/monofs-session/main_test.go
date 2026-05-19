package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocketTimeoutForAction(t *testing.T) {
	t.Parallel()

	tests := map[string]time.Duration{
		"status":     defaultSocketTimeout,
		"pull":       pushSocketTimeout,
		"push":       pushSocketTimeout,
		"push-blobs": pushSocketTimeout,
		"commit":     pushSocketTimeout,
		"":           defaultSocketTimeout,
	}

	for action, want := range tests {
		if got := socketTimeoutForAction(action); got != want {
			t.Fatalf("socketTimeoutForAction(%q) = %v, want %v", action, got, want)
		}
	}
}

func TestSetupBlobsKeepsShellExportsOnStdout(t *testing.T) {
	mountDir := t.TempDir()
	sc := &SessionCommand{}

	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.setupBlobs([]string{"--mount", mountDir})
	})
	if err != nil {
		t.Fatalf("setupBlobs returned error: %v", err)
	}

	if !strings.Contains(stdout, `export GOMODCACHE=`) {
		t.Fatalf("stdout missing shell exports: %q", stdout)
	}
	if strings.Contains(stdout, "monofs-session:") {
		t.Fatalf("stdout should not include status text: %q", stdout)
	}
	if !strings.Contains(stderr, "monofs-session:") {
		t.Fatalf("stderr missing status text: %q", stderr)
	}
}

func TestShowBranchesPrintsWorkspaceRefs(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "session.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen(unix) error = %v", err)
	}
	defer listener.Close()

	requests := make(chan SessionRequest, 1)
	serverErr := make(chan error, 1)
	go func() {
		defer close(serverErr)
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		var req SessionRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			serverErr <- err
			return
		}
		requests <- req

		resp := SessionResponse{
			Success: true,
			WorkspaceRefs: []WorkspaceRef{
				{DisplayPath: "github.com/acme/guardian", Ref: "main", CommitHash: "bbbbbbb2"},
				{DisplayPath: "github.com/acme/monofs", Ref: "release/next", CommitHash: "1234567890abcdef"},
			},
		}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.showBranches(nil)
	})
	if err != nil {
		t.Fatalf("showBranches() error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("showBranches() stderr = %q, want empty", stderr)
	}

	select {
	case req := <-requests:
		if req.Action != "branch" {
			t.Fatalf("request action = %q, want branch", req.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for branch request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("branch test server error = %v", err)
	}

	if !strings.Contains(stdout, "Workspace Refs") {
		t.Fatalf("stdout missing heading: %q", stdout)
	}
	if !strings.Contains(stdout, "main") || !strings.Contains(stdout, "github.com/acme/guardian") {
		t.Fatalf("stdout missing guardian ref row: %q", stdout)
	}
	if !strings.Contains(stdout, "1234567890ab") || !strings.Contains(stdout, "github.com/acme/monofs") {
		t.Fatalf("stdout missing shortened commit row: %q", stdout)
	}
}

func captureOutputs(t *testing.T, fn func() error) (string, string, error) {
	t.Helper()

	origStdout := os.Stdout
	origStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		t.Fatalf("create stderr pipe: %v", err)
	}

	os.Stdout = stdoutW
	os.Stderr = stderrW
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	runErr := fn()

	stdoutW.Close()
	stderrW.Close()

	stdout, readStdoutErr := io.ReadAll(stdoutR)
	stderr, readStderrErr := io.ReadAll(stderrR)
	stdoutR.Close()
	stderrR.Close()

	if readStdoutErr != nil {
		t.Fatalf("read stdout: %v", readStdoutErr)
	}
	if readStderrErr != nil {
		t.Fatalf("read stderr: %v", readStderrErr)
	}

	return string(stdout), string(stderr), runErr
}
