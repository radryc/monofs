package main

import (
	"io"
	"os"
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
