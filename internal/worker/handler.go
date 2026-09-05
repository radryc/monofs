package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type BuilderHandler struct {
	mountPath string
	logger    *slog.Logger
}

func NewBuilderHandler(mountPath string, logger *slog.Logger) *BuilderHandler {
	return &BuilderHandler{mountPath: mountPath, logger: logger}
}

func (h *BuilderHandler) Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error) {
	for _, step := range task.Steps {
		code, err := h.executeStep(ctx, step, logWriter)
		if err != nil {
			return code, err
		}
	}
	return 0, nil
}

func (h *BuilderHandler) executeStep(ctx context.Context, step StepData, logWriter io.Writer) (int, error) {
	if step.Uses != "" {
		return h.executeBuiltin(ctx, step, logWriter)
	}
	if step.Run != "" {
		return h.executeShell(ctx, step.Run, logWriter)
	}
	return 0, fmt.Errorf("step has no run or uses directive")
}

func (h *BuilderHandler) executeShell(ctx context.Context, cmdStr string, logWriter io.Writer) (int, error) {
	shell := []string{"/bin/sh", "-c"}
	if os.Getenv("SHELL") != "" {
		shell[0] = os.Getenv("SHELL")
	}

	cmd := exec.CommandContext(ctx, shell[0], append(shell[1:], cmdStr)...)
	cmd.Dir = h.mountPath
	cmd.Env = os.Environ()
	cmd.Stdout = io.MultiWriter(logWriter, &stepLogBuffer{})
	cmd.Stderr = io.MultiWriter(logWriter, &stepLogBuffer{})

	h.logger.Debug("executing step", "cmd", cmdStr)

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), fmt.Errorf("step failed with exit code %d: %s", exitErr.ExitCode(), cmdStr)
		}
		return -1, fmt.Errorf("step failed: %s: %w", cmdStr, err)
	}
	return 0, nil
}

func (h *BuilderHandler) executeBuiltin(ctx context.Context, step StepData, logWriter io.Writer) (int, error) {
	name := strings.TrimPrefix(step.Uses, "monofs/")

	switch {
	case name == "affected@v1" || name == "affected":
		return h.builtinAffected(ctx, step, logWriter)
	case strings.HasPrefix(name, "checkout"):
		return h.builtinCheckout(ctx, step, logWriter)
	default:
		return -1, fmt.Errorf("unknown builtin action: %s", step.Uses)
	}
}

func (h *BuilderHandler) builtinAffected(ctx context.Context, step StepData, logWriter io.Writer) (int, error) {
	packagesPath := step.With["packages"]
	if packagesPath == "" {
		packagesPath = filepath.Join(h.mountPath, "monofs-packages.yaml")
	} else if !filepath.IsAbs(packagesPath) {
		packagesPath = filepath.Join(h.mountPath, packagesPath)
	}

	data, err := os.ReadFile(packagesPath)
	if err != nil {
		fmt.Fprintf(logWriter, "::error::cannot read packages file: %v\n", err)
		return -1, err
	}
	fmt.Fprintf(logWriter, "::group::Affected packages\n%s\n::endgroup::\n", string(data))
	return 0, nil
}

func (h *BuilderHandler) builtinCheckout(ctx context.Context, step StepData, logWriter io.Writer) (int, error) {
	fmt.Fprintf(logWriter, "::notice::source is at %s\n", h.mountPath)
	return 0, nil
}

type stepLogBuffer struct {
	buf bytes.Buffer
}

func (b *stepLogBuffer) Write(p []byte) (int, error) {
	b.buf.Write(p)
	return len(p), nil
}

func (b *stepLogBuffer) String() string {
	return b.buf.String()
}

type DockerHandler struct {
	logger *slog.Logger
}

func NewDockerHandler(logger *slog.Logger) *DockerHandler {
	return &DockerHandler{logger: logger}
}

func (h *DockerHandler) Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error) {
	for _, step := range task.Steps {
		if step.Run == "" {
			continue
		}
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", step.Run)
		cmd.Env = os.Environ()
		cmd.Stdout = logWriter
		cmd.Stderr = logWriter

		h.logger.Debug("executing docker step", "cmd", step.Run)

		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode(), fmt.Errorf("docker step failed: %s", step.Run)
			}
			return -1, fmt.Errorf("docker step failed: %s: %w", step.Run, err)
		}
	}
	return 0, nil
}

type DeployerHandler struct {
	mountPath string
	logger    *slog.Logger
}

func NewDeployerHandler(mountPath string, logger *slog.Logger) *DeployerHandler {
	return &DeployerHandler{mountPath: mountPath, logger: logger}
}

func (h *DeployerHandler) Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error) {
	for _, step := range task.Steps {
		if step.Run == "" {
			continue
		}
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", step.Run)
		cmd.Dir = h.mountPath
		cmd.Env = os.Environ()
		cmd.Stdout = logWriter
		cmd.Stderr = logWriter

		h.logger.Debug("executing deploy step", "cmd", step.Run)

		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode(), fmt.Errorf("deploy step failed: %s", step.Run)
			}
			return -1, fmt.Errorf("deploy step failed: %s: %w", step.Run, err)
		}
	}
	return 0, nil
}
