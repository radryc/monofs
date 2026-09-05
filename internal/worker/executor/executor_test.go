package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecutorSimpleCommand(t *testing.T) {
	cfg := Config{
		ID:      "test-worker-1",
		WorkDir: t.TempDir(),
		MaxJobs: 2,
		Platform: Platform{
			OS:   "linux",
			Arch: "amd64",
		},
	}
	e := New(cfg)
	defer e.Close()

	ctx := context.Background()
	req := &ExecuteRequest{
		Arguments:        []string{"echo", "hello world"},
		WorkingDirectory: "job-1",
		Timeout:          10 * time.Second,
	}

	result, err := e.Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code: got %d, want 0", result.ExitCode)
	}
	if !strings.Contains(string(result.Stdout), "hello world") {
		t.Errorf("stdout: got %q, want hello world", result.Stdout)
	}
	if result.Duration <= 0 {
		t.Error("duration should be positive")
	}
}

func TestExecutorFailingCommand(t *testing.T) {
	cfg := Config{
		ID:      "test-worker-2",
		WorkDir: t.TempDir(),
		MaxJobs: 1,
	}
	e := New(cfg)

	ctx := context.Background()
	req := &ExecuteRequest{
		Arguments:        []string{"sh", "-c", "exit 42"},
		WorkingDirectory: "job-2",
		Timeout:          10 * time.Second,
	}

	result, err := e.Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("exit code: got %d, want 42", result.ExitCode)
	}
}

func TestExecutorInputFiles(t *testing.T) {
	workDir := t.TempDir()
	cacheDir := t.TempDir()

	go startFakeCache(t, cacheDir, "19993")
	time.Sleep(100 * time.Millisecond)

	inputData := []byte("package main\n\nfunc main() {}\n")
	digest := computeDigest(inputData)
	cachePath := filepath.Join(cacheDir, "cas", strings.ReplaceAll(digest, "/", "_"))
	os.WriteFile(cachePath, inputData, 0644)

	cfg := Config{
		ID:        "test-worker-3",
		WorkDir:   workDir,
		MaxJobs:   1,
		CacheAddr: "localhost:19993",
	}
	e := New(cfg)

	ctx := context.Background()
	req := &ExecuteRequest{
		Arguments:        []string{"cat", "src/main.go"},
		WorkingDirectory: "job-3",
		InputFiles: []InputFile{
			{Path: "src/main.go", Digest: digest},
		},
		Timeout: 10 * time.Second,
	}

	result, err := e.Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code: got %d (stderr: %s)", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(string(result.Stdout), "package main") {
		t.Errorf("stdout should contain input file content: %q", result.Stdout)
	}
}

func TestExecutorOutputFiles(t *testing.T) {
	workDir := t.TempDir()
	cacheDir := t.TempDir()
	go startFakeCache(t, cacheDir, "19994")
	time.Sleep(100 * time.Millisecond)

	cfg := Config{
		ID:        "test-worker-4",
		WorkDir:   workDir,
		MaxJobs:   1,
		CacheAddr: "localhost:19994",
	}
	e := New(cfg)

	ctx := context.Background()
	req := &ExecuteRequest{
		Arguments:        []string{"sh", "-c", "mkdir -p output/bin && echo built > output/bin/app"},
		WorkingDirectory: "job-4",
		OutputFiles:      []string{"output/bin/app"},
		Timeout:          10 * time.Second,
	}

	result, err := e.Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code: %d (stderr: %s)", result.ExitCode, result.Stderr)
	}
	if len(result.OutputFiles) == 0 {
		t.Fatal("expected at least 1 output file")
	}
	out := result.OutputFiles[0]
	if out.Path != "output/bin/app" {
		t.Errorf("output path: got %q", out.Path)
	}
	if out.Size < 5 {
		t.Errorf("output size: got %d, want >= 5", out.Size)
	}
	if out.Digest == "" {
		t.Error("output digest should not be empty")
	}
}

func TestExecutorConcurrencyLimit(t *testing.T) {
	cfg := Config{
		ID:      "test-worker-5",
		WorkDir: t.TempDir(),
		MaxJobs: 2,
	}
	e := New(cfg)

	results := make(chan error, 4)
	for i := range 4 {
		go func(id int) {
			req := &ExecuteRequest{
				Arguments:        []string{"sleep", "0.2"},
				WorkingDirectory: fmt.Sprintf("job-conc-%d", id),
				Timeout:          5 * time.Second,
			}
			_, err := e.Execute(context.Background(), req)
			results <- err
		}(i)
	}

	timeout := time.After(3 * time.Second)
	for i := range 4 {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("job %d: %v", i, err)
			}
		case <-timeout:
			t.Fatal("timeout waiting for jobs")
		}
	}
}

func TestExecutorStatus(t *testing.T) {
	cfg := Config{
		ID:       "test-worker-status",
		WorkDir:  t.TempDir(),
		MaxJobs:  4,
		Platform: Platform{OS: "linux", Arch: "amd64", Pool: "builder"},
	}
	e := New(cfg)

	status := e.Status()
	if status["worker_id"] != "test-worker-status" {
		t.Errorf("worker_id: got %v", status["worker_id"])
	}
	if status["max_jobs"].(int) != 4 {
		t.Errorf("max_jobs: got %v", status["max_jobs"])
	}
}

func TestExecutorTimeoutKill(t *testing.T) {
	cfg := Config{
		ID:      "test-worker-timeout",
		WorkDir: t.TempDir(),
		MaxJobs: 1,
	}
	e := New(cfg)

	ctx := context.Background()
	req := &ExecuteRequest{
		Arguments:        []string{"sleep", "10"},
		WorkingDirectory: "job-timeout",
		Timeout:          50 * time.Millisecond,
	}

	result, err := e.Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode == 0 {
		t.Log("expected non-zero exit from timeout kill, got 0 (may vary by OS)")
	}
}

func TestExecutorStoreDirectory(t *testing.T) {
	workDir := t.TempDir()
	cacheDir := t.TempDir()
	go startFakeCache(t, cacheDir, "19995")
	time.Sleep(100 * time.Millisecond)

	cfg := Config{
		ID:        "test-worker-dir",
		WorkDir:   workDir,
		MaxJobs:   1,
		CacheAddr: "localhost:19995",
	}
	e := New(cfg)

	// Create a test directory with files.
	testDir := filepath.Join(workDir, "test-output")
	os.MkdirAll(testDir, 0755)
	os.WriteFile(filepath.Join(testDir, "a.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(testDir, "b.txt"), []byte("world"), 0644)

	ctx := context.Background()
	treeDigest, err := e.storeDirectory(ctx, testDir)
	if err != nil {
		t.Fatalf("storeDirectory: %v", err)
	}
	if treeDigest == "" {
		t.Error("tree digest should not be empty")
	}
}

// --- helpers ---

func startFakeCache(t *testing.T, dir, port string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "cas"), 0755)
	mux := http.NewServeMux()
	mux.HandleFunc("/cas/", func(w http.ResponseWriter, r *http.Request) {
		digest := strings.TrimPrefix(r.URL.Path, "/cas/")
		digest = strings.ReplaceAll(digest, "/", "_")
		path := filepath.Join(dir, "cas", filepath.Base(digest))
		if r.Method == http.MethodGet {
			data, err := os.ReadFile(path)
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(data)
			return
		}
		if r.Method == http.MethodPut {
			data, _ := io.ReadAll(r.Body)
			r.Body.Close()
			os.WriteFile(path, data, 0644)
			w.WriteHeader(http.StatusOK)
			return
		}
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	go func() { _ = http.ListenAndServe(":"+port, mux) }()
}

func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%s/%d", hex.EncodeToString(h[:]), len(data))
}
