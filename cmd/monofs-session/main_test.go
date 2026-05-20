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

func TestDefaultSessionSocketPathPrefersLegacyGitfsOverlayDir(t *testing.T) {
	t.Setenv("MONOFS_OVERLAY_DIR", "")
	overlayDir := filepath.Join(t.TempDir(), "overlay")
	t.Setenv("GITFS_OVERLAY_DIR", overlayDir)

	if got, want := defaultSessionSocketPath(), filepath.Join(overlayDir, "session.sock"); got != want {
		t.Fatalf("defaultSessionSocketPath() = %q, want %q", got, want)
	}
}

func TestDefaultSessionSocketPathPrefersMonofsOverlayDir(t *testing.T) {
	monofsOverlayDir := filepath.Join(t.TempDir(), "monofs-overlay")
	gitfsOverlayDir := filepath.Join(t.TempDir(), "gitfs-overlay")
	t.Setenv("MONOFS_OVERLAY_DIR", monofsOverlayDir)
	t.Setenv("GITFS_OVERLAY_DIR", gitfsOverlayDir)

	if got, want := defaultSessionSocketPath(), filepath.Join(monofsOverlayDir, "session.sock"); got != want {
		t.Fatalf("defaultSessionSocketPath() = %q, want %q", got, want)
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
	for _, relPath := range []string{"dependency/go/mod", "dependency/go/path"} {
		if _, err := os.Stat(filepath.Join(mountDir, relPath)); err != nil {
			t.Fatalf("Stat(%q) error = %v", relPath, err)
		}
	}
}

func TestShowRefsPrintsWorkspaceRefs(t *testing.T) {
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
		return sc.showRefs(nil)
	})
	if err != nil {
		t.Fatalf("showRefs() error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("showRefs() stderr = %q, want empty", stderr)
	}

	select {
	case req := <-requests:
		if req.Action != "refs" {
			t.Fatalf("request action = %q, want refs", req.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for refs request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("refs test server error = %v", err)
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

func TestManageBranchesCreateSendsBranchRequest(t *testing.T) {
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

		resp := SessionResponse{Success: true, Message: "created and switched to logical branch feature/demo; working tree and index stay unchanged"}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.manageBranches([]string{"create", "feature/demo"})
	})
	if err != nil {
		t.Fatalf("manageBranches(create) error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("manageBranches(create) stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "created and switched to logical branch feature/demo") {
		t.Fatalf("manageBranches(create) stdout = %q, want create summary", stdout)
	}

	select {
	case req := <-requests:
		if req.Action != "branch" || req.BranchOp != "create" || req.BranchName != "feature/demo" {
			t.Fatalf("request = %+v, want branch create feature/demo", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for branch create request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("branch create test server error = %v", err)
	}
}

func TestPushSourceSendsPushRequest(t *testing.T) {
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

		resp := SessionResponse{Success: true, Message: "pushed 2 local commit(s) across 1 repositor(y/ies) on logical branch feature/demo"}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.pushSource(nil)
	})
	if err != nil {
		t.Fatalf("pushSource() error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("pushSource() stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "Warning: pending local commits are currently squashed into one upstream Git commit per affected repository.") || !strings.Contains(stdout, "Local commits pushed") || !strings.Contains(stdout, "logical branch feature/demo") {
		t.Fatalf("pushSource() stdout = %q, want push summary", stdout)
	}

	select {
	case req := <-requests:
		if req.Action != "push" {
			t.Fatalf("request action = %q, want push", req.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for push request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("push test server error = %v", err)
	}
}

func TestExecutePushDepsAliasSendsPushBlobsRequest(t *testing.T) {
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

		resp := SessionResponse{Success: true, Message: "cluster: 3 files ingested, 0 failed", Changes: 3}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.Execute([]string{"push-deps"})
	})
	if err != nil {
		t.Fatalf("Execute(push-deps) error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("Execute(push-deps) stderr = %q, want empty", stderr)
	}
	if strings.Contains(stdout, "squashed into one upstream Git commit") {
		t.Fatalf("Execute(push-deps) stdout = %q, do not want source push warning", stdout)
	}
	if !strings.Contains(stdout, "Dependencies uploaded") {
		t.Fatalf("Execute(push-deps) stdout = %q, want dependency upload summary", stdout)
	}

	select {
	case req := <-requests:
		if req.Action != "push-blobs" {
			t.Fatalf("request action = %q, want push-blobs", req.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for push-blobs request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("push-deps test server error = %v", err)
	}
}

func TestUploadBlobsSendsPushBlobsRequest(t *testing.T) {
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

		resp := SessionResponse{Success: true, Message: "cluster: 3 files ingested, 0 failed", Changes: 3}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.uploadBlobs(nil)
	})
	if err != nil {
		t.Fatalf("uploadBlobs() error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("uploadBlobs() stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "Dependencies uploaded") {
		t.Fatalf("uploadBlobs() stdout = %q, want upload summary", stdout)
	}

	select {
	case req := <-requests:
		if req.Action != "push-blobs" {
			t.Fatalf("request action = %q, want push-blobs", req.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for push-blobs request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("push-blobs test server error = %v", err)
	}
}

func TestShowLogicalBranchesPrintsCurrentBranchAndMappings(t *testing.T) {
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
			Success:       true,
			SessionID:     "session-1",
			CreatedAt:     "2026-05-20 11:00:00",
			CurrentBranch: "feature/demo",
			BranchList: []BranchInfo{
				{Name: "feature/demo", Current: true, PendingCommits: 2, HasMappings: true},
				{Name: "release/demo", Current: false, HasMappings: true},
			},
			BranchMappings: []BranchMappingInfo{{
				DisplayPath:      "github.com/acme/monofs",
				OriginalBranch:   "main",
				ActualBranch:     "feature/demo-20260520",
				LastPushedCommit: "1234567890abcdef",
			}},
		}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.showLogicalBranches()
	})
	if err != nil {
		t.Fatalf("showLogicalBranches() error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("showLogicalBranches() stderr = %q, want empty", stderr)
	}

	select {
	case req := <-requests:
		if req.Action != "branch" || req.BranchOp != "show" {
			t.Fatalf("request = %+v, want branch show", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for branch show request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("branch show test server error = %v", err)
	}

	for _, want := range []string{"Logical Branches", "Current:    feature/demo", "* feature/demo  (2 pending local commit(s))  [repo mappings tracked]", "Current Branch Repo Mappings:", "feature/demo-20260520", "github.com/acme/monofs"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("showLogicalBranches() stdout missing %q: %q", want, stdout)
		}
	}
}

func TestAddToIndexSendsRequestedPaths(t *testing.T) {
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

		resp := SessionResponse{Success: true, StagedChanges: 2, Message: "staged 2 source change(s)"}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.addToIndex([]string{"github.com/acme/monofs", "github.com/acme/guardian/README.md"})
	})
	if err != nil {
		t.Fatalf("addToIndex() error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("addToIndex() stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "staged 2 source change(s)") {
		t.Fatalf("addToIndex() stdout = %q, want staging message", stdout)
	}

	select {
	case req := <-requests:
		if req.Action != "add" {
			t.Fatalf("request action = %q, want add", req.Action)
		}
		if len(req.Paths) != 2 || req.Paths[0] != "github.com/acme/monofs" || req.Paths[1] != "github.com/acme/guardian/README.md" {
			t.Fatalf("request paths = %v", req.Paths)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for add request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("add test server error = %v", err)
	}
}

func TestRemoveFromIndexSendsRequestedPaths(t *testing.T) {
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

		resp := SessionResponse{Success: true, Changes: 2, StagedChanges: 1, Message: "removed 2 path(s); staged 1 source change(s)"}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.removeFromIndex([]string{"github.com/acme/monofs/main.go", "github.com/acme/guardian/docs"})
	})
	if err != nil {
		t.Fatalf("removeFromIndex() error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("removeFromIndex() stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "removed 2 path(s); staged 1 source change(s)") {
		t.Fatalf("removeFromIndex() stdout = %q, want remove message", stdout)
	}

	select {
	case req := <-requests:
		if req.Action != "rm" {
			t.Fatalf("request action = %q, want rm", req.Action)
		}
		if len(req.Paths) != 2 || req.Paths[0] != "github.com/acme/monofs/main.go" || req.Paths[1] != "github.com/acme/guardian/docs" {
			t.Fatalf("request paths = %v", req.Paths)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rm request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("rm test server error = %v", err)
	}
}

func TestShowLogPrintsLocalCommitMetadata(t *testing.T) {
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
			Success:   true,
			SessionID: "session-1",
			CreatedAt: "2026-05-20 10:00:00",
			LocalCommitList: []LocalCommitInfo{{
				ID:              "local-2",
				ParentID:        "local-1",
				Message:         "second checkpoint",
				LogicalBranch:   "feature/demo",
				AuthorName:      "Test User",
				AuthorEmail:     "test@example.com",
				PrincipalID:     "principal-a",
				CreatedAt:       "2026-05-20 10:01:00",
				RepositoryCount: 2,
				OperationCount:  3,
			}},
		}
		serverErr <- json.NewEncoder(conn).Encode(resp)
	}()

	sc := &SessionCommand{socketPath: socketPath}
	stdout, stderr, err := captureOutputs(t, func() error {
		return sc.showLog(nil)
	})
	if err != nil {
		t.Fatalf("showLog() error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("showLog() stderr = %q, want empty", stderr)
	}

	select {
	case req := <-requests:
		if req.Action != "log" {
			t.Fatalf("request action = %q, want log", req.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for log request")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("log test server error = %v", err)
	}

	for _, want := range []string{"Local Commit Log", "commit local-2", "Principal:  principal-a", "Author:     Test User <test@example.com>", "Changes:    3 operation(s) across 2 repo(s)", "second checkpoint"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("showLog() stdout missing %q: %q", want, stdout)
		}
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
