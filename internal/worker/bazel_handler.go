package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/router/pipeline"
)

// BazelHandler executes pipeline steps that use runs-on: bazel.
// It shells out to `bazel build` / `bazel test` and parses
// the Build Event Protocol output for structured results.
type BazelHandler struct {
	mountPath    string
	cacheAddr    string
	executorAddr string
	logger       *slog.Logger
}

// NewBazelHandler creates a handler for bazel pipeline jobs.
func NewBazelHandler(mountPath, cacheAddr, executorAddr string, logger *slog.Logger) *BazelHandler {
	return &BazelHandler{
		mountPath:    mountPath,
		cacheAddr:    cacheAddr,
		executorAddr: executorAddr,
		logger:       logger,
	}
}

// Execute implements Handler. It runs each step's Run command through
// a shell and parses BEP output if bazel was invoked.
func (h *BazelHandler) Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error) {
	for i, step := range task.Steps {
		code, err := h.executeStep(ctx, step, logWriter)
		if err != nil {
			fmt.Fprintf(logWriter, "step %d failed: %v\n", i, err)
			return code, err
		}
	}
	return 0, nil
}

func (h *BazelHandler) executeStep(ctx context.Context, step StepData, logWriter io.Writer) (int, error) {
	if step.Run == "" {
		return 0, fmt.Errorf("bazel step has no run directive")
	}

	// Build environment with cache/executor config.
	env := os.Environ()
	if h.cacheAddr != "" {
		env = append(env, "MONOFS_CACHE_ADDR="+h.cacheAddr)
	}
	if h.executorAddr != "" {
		env = append(env, "MONOFS_EXECUTOR_ADDR="+h.executorAddr)
	}

	shell := "/bin/sh"
	if s := os.Getenv("SHELL"); s != "" {
		shell = s
	}

	cmd := exec.CommandContext(ctx, shell, "-c", step.Run)
	cmd.Dir = h.mountPath
	cmd.Env = env
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	start := time.Now()
	fmt.Fprintf(logWriter, "=== bazel step: %s ===\n", step.Run)
	err := cmd.Run()
	dur := time.Since(start)

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			// Try to parse BEP for structured failure info.
			bepPath := findBEPFile(h.mountPath, step.Run)
			if bepPath != "" {
				if result, parseErr := pipeline.ParseBEP(bepPath); parseErr == nil {
					data := pipeline.MarshalStepResult(code, "", result)
					fmt.Fprintf(logWriter, "\n--- BEP result ---\n%s\n", data)
				}
			}
			fmt.Fprintf(logWriter, "step failed: exit=%d duration=%s\n", code, dur)
			return code, nil
		}
		fmt.Fprintf(logWriter, "step error: %v\n", err)
		return -1, err
	}

	// Success: parse BEP for structured output.
	bepPath := findBEPFile(h.mountPath, step.Run)
	if bepPath != "" {
		if result, parseErr := pipeline.ParseBEP(bepPath); parseErr == nil {
			data := pipeline.MarshalStepResult(0, "", result)
			fmt.Fprintf(logWriter, "\n--- BEP result ---\n%s\n", data)
		}
	}

	fmt.Fprintf(logWriter, "step complete: duration=%s\n", dur)
	return 0, nil
}

// findBEPFile looks for a BEP JSON file that bazel may have written
// during the step. Bazel writes to --build_event_json_file=<path>.
func findBEPFile(workDir, runCmd string) string {
	// Heuristic: extract the action from the command.
	action := "build"
	if strings.Contains(runCmd, "bazel test") {
		action = "test"
	}

	// Check typical BEP paths.
	candidates := []string{
		filepath.Join(os.TempDir(), fmt.Sprintf("bazel-bep-%s.json", action)),
		filepath.Join(workDir, "bazel-out", "bep.json"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
