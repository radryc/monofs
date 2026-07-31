package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuilderHandlerShellStep(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	handler := NewBuilderHandler("/tmp", logger)

	var buf bytes.Buffer
	task := &TaskData{
		TaskID:     "test-task",
		JobName:    "test",
		RunnerType: "builder",
		Steps: []StepData{
			{Run: "echo hello"},
		},
		TimeoutSec: 5,
	}

	code, err := handler.Execute(context.Background(), task, &buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	output := buf.String()
	if output == "" {
		t.Fatal("expected output, got empty")
	}
}

func TestBuilderHandlerFailingStep(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	handler := NewBuilderHandler("/tmp", logger)

	var buf bytes.Buffer
	task := &TaskData{
		TaskID:     "test-fail",
		JobName:    "test",
		RunnerType: "builder",
		Steps: []StepData{
			{Run: "exit 1"},
		},
		TimeoutSec: 5,
	}

	code, err := handler.Execute(context.Background(), task, &buf)
	if err == nil {
		t.Fatal("expected error for failing step")
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestBuilderHandlerMultipleSteps(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	handler := NewBuilderHandler("/tmp", logger)

	var buf bytes.Buffer
	task := &TaskData{
		TaskID:     "test-multi",
		JobName:    "test",
		RunnerType: "builder",
		Steps: []StepData{
			{Run: "echo step1"},
			{Run: "echo step2"},
			{Run: "echo step3"},
		},
		TimeoutSec: 5,
	}

	code, err := handler.Execute(context.Background(), task, &buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	output := buf.String()
	if !bytes.Contains([]byte(output), []byte("step2")) {
		t.Error("output should contain step2 output")
	}
}

func TestBuilderHandlerTimeout(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	handler := NewBuilderHandler("/tmp", logger)

	var buf bytes.Buffer
	task := &TaskData{
		TaskID:     "test-timeout",
		JobName:    "test",
		RunnerType: "builder",
		Steps: []StepData{
			{Run: "sleep 10"},
		},
		TimeoutSec: 1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := handler.Execute(ctx, task, &buf)
	if err == nil {
		t.Fatal("expected error for timeout")
	}
}

func TestBuilderHandlerBuiltinCheckout(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	handler := NewBuilderHandler("/tmp", logger)

	var buf bytes.Buffer
	task := &TaskData{
		TaskID:     "test-checkout",
		JobName:    "checkout",
		RunnerType: "builder",
		Steps: []StepData{
			{Uses: "monofs/checkout@v1"},
		},
		TimeoutSec: 5,
	}

	code, err := handler.Execute(context.Background(), task, &buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestDockerHandler(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skipping docker tests in CI")
	}

	logger := slog.New(slog.DiscardHandler)
	handler := NewDockerHandler(logger)

	var buf bytes.Buffer
	task := &TaskData{
		TaskID:     "docker-test",
		JobName:    "containerize",
		RunnerType: "docker",
		Steps: []StepData{
			{Run: "echo docker build dummy-image"},
		},
		TimeoutSec: 5,
	}

	code, err := handler.Execute(context.Background(), task, &buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestDeployerHandler(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	handler := NewDeployerHandler("/tmp", logger)

	var buf bytes.Buffer
	task := &TaskData{
		TaskID:     "deploy-test",
		JobName:    "deploy",
		RunnerType: "deployer",
		Steps: []StepData{
			{Run: "echo guardianctl deploy partition"},
		},
		TimeoutSec: 5,
	}

	code, err := handler.Execute(context.Background(), task, &buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestBuilderHandlerBuiltinAffected(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	dir := t.TempDir()
	packagesPath := filepath.Join(dir, "monofs-packages.yaml")
	os.WriteFile(packagesPath, []byte(`packages:
  server:
    path: cmd/monofs-server
    deps: [internal/server]
    build: make build-server
    test: make test-unit
`), 0644)

	handler := NewBuilderHandler(dir, logger)

	var buf bytes.Buffer
	task := &TaskData{
		TaskID:     "test-affected",
		JobName:    "detect",
		RunnerType: "builder",
		Steps: []StepData{
			{Uses: "monofs/affected@v1", With: map[string]string{"packages": packagesPath}},
		},
		TimeoutSec: 5,
	}

	code, err := handler.Execute(context.Background(), task, &buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !bytes.Contains(buf.Bytes(), []byte("cmd/monofs-server")) {
		t.Error("output should contain package path from the packages file")
	}
}

func TestClaimResultJSON(t *testing.T) {
	claim := ClaimData{
		TaskID:    "task-001",
		RunID:     "run-001",
		JobName:   "build",
		WorkerID:  "worker-1",
		ClaimedAt: time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}

	var parsed ClaimData
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal claim: %v", err)
	}
	if parsed.TaskID != "task-001" {
		t.Errorf("TaskID = %q, want task-001", parsed.TaskID)
	}
}

func TestResultJSON(t *testing.T) {
	result := ResultData{
		TaskID:    "task-001",
		RunID:     "run-001",
		JobName:   "build",
		State:     "succeeded",
		ExitCode:  0,
		StartedAt: "2026-07-09T00:00:00Z",
		EndedAt:   "2026-07-09T00:01:00Z",
		WorkerID:  "worker-1",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	var parsed ResultData
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed.State != "succeeded" {
		t.Errorf("State = %q, want succeeded", parsed.State)
	}
	if parsed.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", parsed.ExitCode)
	}
}

func TestTaskDataJSON(t *testing.T) {
	task := TaskData{
		TaskID:     "task-001",
		RunID:      "run-001",
		JobName:    "build",
		RunnerType: "builder",
		Steps: []StepData{
			{Run: "make build"},
			{Uses: "monofs/affected@v1", With: map[string]string{"packages": "monofs-packages.yaml"}},
		},
		TimeoutSec: 300,
		MaxRetries: 2,
	}

	data, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}

	var parsed TaskData
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if parsed.JobName != "build" {
		t.Errorf("JobName = %q, want build", parsed.JobName)
	}
	if len(parsed.Steps) != 2 {
		t.Errorf("Steps = %d, want 2", len(parsed.Steps))
	}
	if parsed.TimeoutSec != 300 {
		t.Errorf("TimeoutSec = %d, want 300", parsed.TimeoutSec)
	}
}
