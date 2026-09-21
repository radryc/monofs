package pipeline

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testOrchestrator() *Orchestrator {
	kvs := newMockKVS()
	queue := NewTaskQueue(kvs)
	logger := slog.New(slog.DiscardHandler)
	orch := NewOrchestrator(queue, logger)

	cfg, _ := ParseConfig([]byte(`
name: test-pipeline
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: builder
    steps:
      - run: make build
  deploy:
    needs: [build]
    runs-on: deployer
    steps:
      - run: guardianctl deploy
`))
	orch.RegisterPipeline(cfg)
	return orch
}

func TestStartRun(t *testing.T) {
	orch := testOrchestrator()

	cfg := orch.configs["test-pipeline"]
	run, err := orch.StartRun(cfg, WebhookEvent{
		EventType: TriggerPush,
		CommitSHA: "abc123",
		Branch:    "main",
	}, nil)

	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for i := 0; i < 50; i++ {
		run, err = orch.GetRun(run.RunID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.State == RunRunning || run.State == RunSucceeded || run.State == RunFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if run.RunID == "" {
		t.Fatal("run ID should be set")
	}
	if run.State != RunRunning && run.State != RunSucceeded {
		t.Errorf("run state = %q, want running or succeeded", run.State)
	}
	if run.Trigger != TriggerPush {
		t.Errorf("trigger = %q, want push", run.Trigger)
	}
	if run.CommitSHA != "abc123" {
		t.Errorf("commit = %q, want abc123", run.CommitSHA)
	}
	if len(run.Jobs) != 2 {
		t.Errorf("expected 2 jobs, got %d", len(run.Jobs))
	}
}

func TestListRuns(t *testing.T) {
	orch := testOrchestrator()

	for i := 0; i < 3; i++ {
		cfg := orch.configs["test-pipeline"]
		orch.StartRun(cfg, WebhookEvent{
			EventType: TriggerPush,
			CommitSHA: "sha",
			Branch:    "main",
		}, nil)
	}

	runs := orch.ListRuns("test-pipeline", 10)
	if len(runs) != 3 {
		t.Errorf("ListRuns: got %d runs, want 3", len(runs))
	}

	time.Sleep(10 * time.Millisecond)

	runs = orch.ListRuns("", 2)
	if len(runs) != 2 {
		t.Errorf("ListRuns(all, limit=2): got %d runs, want 2", len(runs))
	}
}

func TestCancelRun(t *testing.T) {
	orch := testOrchestrator()

	cfg := orch.configs["test-pipeline"]
	run, _ := orch.StartRun(cfg, WebhookEvent{
		EventType: TriggerPush,
		CommitSHA: "def456",
		Branch:    "main",
	}, nil)

	for i := 0; i < 50; i++ {
		current, _ := orch.GetRun(run.RunID)
		if current.State != RunPending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	orch.CancelRun(run.RunID)

	cancelled, err := orch.GetRun(run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if cancelled.State != RunCancelled {
		t.Errorf("run state = %q, want cancelled", cancelled.State)
	}
}

func TestCancelFinishedRun(t *testing.T) {
	orch := testOrchestrator()

	cfg := orch.configs["test-pipeline"]
	run, _ := orch.StartRun(cfg, WebhookEvent{
		EventType: TriggerPush,
		CommitSHA: "ghi789",
		Branch:    "main",
	}, nil)

	orch.CancelRun(run.RunID)

	err := orch.CancelRun(run.RunID)
	if err == nil {
		t.Fatal("expected error when cancelling already-finished run")
	}
}

func TestGetRunNotFound(t *testing.T) {
	orch := testOrchestrator()
	_, err := orch.GetRun("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent run")
	}
}

func TestListPipelines(t *testing.T) {
	orch := testOrchestrator()
	pipelines := orch.ListPipelines()
	if len(pipelines) != 1 {
		t.Fatalf("expected 1 pipeline, got %d", len(pipelines))
	}
	if pipelines[0].Name != "test-pipeline" {
		t.Errorf("pipeline name = %q, want test-pipeline", pipelines[0].Name)
	}
}

func TestRegisterUnregisterPipeline(t *testing.T) {
	orch := NewOrchestrator(NewTaskQueue(newMockKVS()), slog.New(slog.DiscardHandler))

	cfg1, _ := ParseConfig([]byte(`
name: pipeline-1
on:
  push:
    branches: [main]
jobs:
  a:
    runs-on: builder
    steps:
      - run: echo a
`))
	cfg2, _ := ParseConfig([]byte(`
name: pipeline-2
on:
  pull_request:
    branches: [main]
jobs:
  b:
    runs-on: builder
    steps:
      - run: echo b
`))

	orch.RegisterPipeline(cfg1)
	orch.RegisterPipeline(cfg2)

	if len(orch.ListPipelines()) != 2 {
		t.Fatal("expected 2 pipelines")
	}

	orch.UnregisterPipeline("pipeline-1")
	if len(orch.ListPipelines()) != 1 {
		t.Fatal("expected 1 pipeline after unregister")
	}
}

func TestEvaluateCondition(t *testing.T) {
	orch := testOrchestrator()

	tests := []struct {
		condition string
		state     RunState
		affected  []string
		want      bool
	}{
		{"always()", RunRunning, nil, true},
		{"", RunRunning, nil, true},
		{"success()", RunSucceeded, nil, true},
		{"success()", RunFailed, nil, false},
		{"failure()", RunFailed, nil, true},
		{"failure()", RunRunning, nil, false},
		{"cancelled()", RunCancelled, nil, true},
		{"cancelled()", RunRunning, nil, false},
		{"affected != ''", RunRunning, []string{"server"}, true},
		{"affected != ''", RunRunning, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			run := &PipelineRun{State: tt.state}
			if got := orch.evaluateCondition(tt.condition, run, tt.affected); got != tt.want {
				t.Errorf("evaluateCondition(%q, %s, %v) = %v, want %v",
					tt.condition, tt.state, tt.affected, got, tt.want)
			}
		})
	}
}

func TestGetStats(t *testing.T) {
	orch := testOrchestrator()

	for i := 0; i < 5; i++ {
		cfg := orch.configs["test-pipeline"]
		run, _ := orch.StartRun(cfg, WebhookEvent{
			EventType: TriggerPush,
			CommitSHA: "sha",
			Branch:    "main",
		}, nil)

		if i < 3 {
			orch.OnTaskResult(nil, &TaskResult{
				RunID:   run.RunID,
				JobName: "build",
				State:   JobSucceeded,
			})
			orch.OnTaskResult(nil, &TaskResult{
				RunID:   run.RunID,
				JobName: "deploy",
				State:   JobSucceeded,
			})
		}
	}

	stats := orch.GetStats("test-pipeline")
	if stats.TotalRuns != 5 {
		t.Errorf("TotalRuns = %d, want 5", stats.TotalRuns)
	}
	if stats.SucceededRuns < 3 {
		t.Errorf("SucceededRuns = %d, want at least 3", stats.SucceededRuns)
	}
}

func TestMatrixExpansionEdgeCases(t *testing.T) {
	noResolve := func(string) ([]string, bool) { return nil, false }

	matrix := MatrixConfig{}
	result, err := expandMatrix(matrix, noResolve)
	if err != nil {
		t.Fatalf("expandMatrix: %v", err)
	}
	if result != nil {
		t.Fatal("expected nil for empty matrix")
	}

	single := MatrixConfig{"x": {Static: []string{"a"}}}
	result, err = expandMatrix(single, noResolve)
	if err != nil {
		t.Fatalf("expandMatrix: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 combination, got %d", len(result))
	}
	if result[0]["x"] != "a" {
		t.Errorf("expected x=a, got x=%s", result[0]["x"])
	}
}

func TestOnTaskResultAdvancesDAGAndNotifies(t *testing.T) {
	orch := testOrchestrator()

	notified := make(chan *PipelineRun, 4)
	orch.SetOnRunFinished(func(run *PipelineRun) { notified <- run })

	cfg := orch.configs["test-pipeline"]
	run, err := orch.StartRun(cfg, WebhookEvent{
		EventType: TriggerPush,
		CommitSHA: "abc123",
		Branch:    "main",
		RepoURL:   "https://github.com/org/repo",
	}, nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Wait until the entrypoint job is running (executeRun is async).
	waitFor(t, func() bool {
		current, err := orch.GetRun(run.RunID)
		return err == nil && current.Jobs["build"].State == JobRunning
	})

	// Build succeeds -> downstream deploy job should be enqueued.
	orch.OnTaskResult(nil, &TaskResult{
		RunID:    run.RunID,
		JobName:  "build",
		State:    JobSucceeded,
		WorkerID: "worker-1",
		EndedAt:  time.Now(),
	})

	current, err := orch.GetRun(run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if current.Jobs["build"].State != JobSucceeded {
		t.Errorf("build state = %q, want succeeded", current.Jobs["build"].State)
	}
	if current.Jobs["deploy"].State != JobRunning {
		t.Errorf("deploy state = %q, want running (DAG should advance)", current.Jobs["deploy"].State)
	}
	if current.State == RunSucceeded {
		t.Error("run should not be finished before deploy reports")
	}

	// Deploy succeeds -> run finishes and the callback fires.
	orch.OnTaskResult(nil, &TaskResult{
		RunID:    run.RunID,
		JobName:  "deploy",
		State:    JobSucceeded,
		WorkerID: "worker-1",
		EndedAt:  time.Now(),
	})

	current, err = orch.GetRun(run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if current.State != RunSucceeded {
		t.Errorf("run state = %q, want succeeded", current.State)
	}

	select {
	case finished := <-notified:
		if finished.RunID != run.RunID {
			t.Errorf("notified run ID = %q, want %q", finished.RunID, run.RunID)
		}
		if finished.State != RunSucceeded {
			t.Errorf("notified state = %q, want succeeded", finished.State)
		}
		if finished.RepoFullName != "org/repo" {
			t.Errorf("notified repo = %q, want org/repo", finished.RepoFullName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run-finished callback was not invoked")
	}
}

func TestOnTaskResultFailureNotifies(t *testing.T) {
	orch := testOrchestrator()

	notified := make(chan *PipelineRun, 4)
	orch.SetOnRunFinished(func(run *PipelineRun) { notified <- run })

	cfg := orch.configs["test-pipeline"]
	run, _ := orch.StartRun(cfg, WebhookEvent{
		EventType: TriggerPush,
		CommitSHA: "abc123",
		Branch:    "main",
		RepoURL:   "https://gitlab.com/group/subgroup/repo.git",
	}, nil)

	waitFor(t, func() bool {
		current, err := orch.GetRun(run.RunID)
		return err == nil && current.Jobs["build"].State == JobRunning
	})

	// Exhaust retries: two failures exceed MaxRetries (2).
	for i := 0; i < 3; i++ {
		orch.OnTaskResult(nil, &TaskResult{
			RunID:   run.RunID,
			JobName: "build",
			State:   JobFailed,
			Error:   "boom",
		})
	}

	current, err := orch.GetRun(run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if current.State != RunFailed {
		t.Errorf("run state = %q, want failed", current.State)
	}

	select {
	case finished := <-notified:
		if finished.State != RunFailed {
			t.Errorf("notified state = %q, want failed", finished.State)
		}
		if finished.RepoFullName != "subgroup/repo" {
			t.Errorf("notified repo = %q, want subgroup/repo", finished.RepoFullName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run-finished callback was not invoked on failure")
	}
}

func TestOnTaskResultUnknownRun(t *testing.T) {
	orch := testOrchestrator()

	notified := make(chan *PipelineRun, 1)
	orch.SetOnRunFinished(func(run *PipelineRun) { notified <- run })

	orch.OnTaskResult(nil, &TaskResult{
		RunID:   "does-not-exist",
		JobName: "build",
		State:   JobSucceeded,
	})

	select {
	case <-notified:
		t.Fatal("callback should not fire for unknown runs")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCancelRunNotifies(t *testing.T) {
	orch := testOrchestrator()

	notified := make(chan *PipelineRun, 1)
	orch.SetOnRunFinished(func(run *PipelineRun) { notified <- run })

	cfg := orch.configs["test-pipeline"]
	run, _ := orch.StartRun(cfg, WebhookEvent{
		EventType: TriggerPush,
		CommitSHA: "def456",
		Branch:    "main",
	}, nil)

	if err := orch.CancelRun(run.RunID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	select {
	case finished := <-notified:
		if finished.State != RunCancelled {
			t.Errorf("notified state = %q, want cancelled", finished.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run-finished callback was not invoked on cancel")
	}
}

func TestRepoFullNameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/org/repo", "org/repo"},
		{"https://github.com/org/repo.git", "org/repo"},
		{"https://gitlab.com/group/repo", "group/repo"},
		{"https://gitlab.com/group/subgroup/deep/repo.git", "deep/repo"},
		{"http://ghe.internal.example.com/team/project", "team/project"},
		{"", ""},
		{"not-a-url", ""},
		{"https://github.com/", ""},
	}

	for _, tt := range tests {
		if got := repoFullNameFromURL(tt.url); got != tt.want {
			t.Errorf("repoFullNameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestTaskResultUnmarshalWorkerPayload(t *testing.T) {
	// This is the exact JSON shape workers write to
	// /.queues/pipeline/<run>/.results/<task>.json (internal/worker
	// ResultData). The router result watcher must decode it directly.
	payload := `{
		"task_id": "task-1",
		"run_id": "run-1",
		"job_name": "build",
		"state": "succeeded",
		"exit_code": 0,
		"started_at": "2026-09-05T10:00:00Z",
		"ended_at": "2026-09-05T10:01:30Z",
		"worker_id": "worker-9"
	}`

	var result TaskResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("unmarshal worker result payload: %v", err)
	}
	if result.RunID != "run-1" || result.JobName != "build" {
		t.Errorf("run/job = %q/%q, want run-1/build", result.RunID, result.JobName)
	}
	if result.State != JobSucceeded {
		t.Errorf("state = %q, want succeeded", result.State)
	}
	if result.WorkerID != "worker-9" {
		t.Errorf("worker = %q, want worker-9", result.WorkerID)
	}
	if result.EndedAt.Sub(result.StartedAt) != 90*time.Second {
		t.Errorf("duration = %v, want 90s", result.EndedAt.Sub(result.StartedAt))
	}
}

func TestTaskResultUnmarshalFailedState(t *testing.T) {
	payload := `{
		"task_id": "task-2",
		"run_id": "run-1",
		"job_name": "deploy",
		"state": "failed",
		"exit_code": 1,
		"error": "guardianctl: rollout timeout",
		"worker_id": "worker-9"
	}`

	var result TaskResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.State != JobFailed {
		t.Errorf("state = %q, want failed", result.State)
	}
	if result.Error != "guardianctl: rollout timeout" {
		t.Errorf("error = %q", result.Error)
	}
}

func TestNeedsOutputsDriveMatrixExpansion(t *testing.T) {
	kvs := newMockKVS()
	queue := NewTaskQueue(kvs)
	orch := NewOrchestrator(queue, slog.New(slog.DiscardHandler))

	cfg, err := ParseConfig([]byte(`
name: matrix-pipeline
on:
  push:
    branches: [main]
jobs:
  detect:
    runs-on: builder
    steps:
      - uses: monofs/affected@v1
        id: affected
  build:
    needs: [detect]
    if: "affected != ''"
    strategy:
      matrix:
        package: ${{ needs.detect.outputs.packages }}
    runs-on: builder
    steps:
      - run: make build-${{ matrix.package }}
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	orch.RegisterPipeline(cfg)

	run, err := orch.StartRun(cfg, WebhookEvent{
		EventType:    TriggerPush,
		CommitSHA:    "sha-1",
		Branch:       "main",
		ChangedFiles: []string{"internal/server/x.go"},
	}, []string{"server"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	waitFor(t, func() bool {
		current, err := orch.GetRun(run.RunID)
		return err == nil && current.Jobs["detect"].State == JobRunning
	})

	// Detect succeeds and publishes the affected packages as outputs.
	orch.OnTaskResult(nil, &TaskResult{
		RunID:   run.RunID,
		JobName: "detect",
		State:   JobSucceeded,
		Outputs: map[string]string{"packages": "server,router"},
	})

	// The build job should expand to one task per affected package.
	waitFor(t, func() bool {
		current, err := orch.GetRun(run.RunID)
		return err == nil && current.Jobs["build"].State == JobRunning
	})

	tasks, err := queue.ListRunTasks(run.RunID)
	if err != nil {
		t.Fatalf("ListRunTasks: %v", err)
	}
	buildTasks := 0
	for _, task := range tasks {
		if task.JobName != "build" {
			continue
		}
		buildTasks++
		for _, step := range task.Steps {
			if !strings.HasPrefix(step.Run, "make build-") {
				t.Errorf("build step run not substituted: %q", step.Run)
			}
		}
	}
	if buildTasks != 2 {
		t.Errorf("build tasks = %d, want 2 (one per affected package)", buildTasks)
	}

	// Both tasks finish -> run succeeds.
	for i := 0; i < 2; i++ {
		orch.OnTaskResult(nil, &TaskResult{
			RunID:   run.RunID,
			JobName: "build",
			State:   JobSucceeded,
		})
	}
	current, _ := orch.GetRun(run.RunID)
	if current.State != RunSucceeded {
		t.Errorf("run state = %q, want succeeded", current.State)
	}
}

func TestEmptyOutputsSkipMatrixJob(t *testing.T) {
	kvs := newMockKVS()
	queue := NewTaskQueue(kvs)
	orch := NewOrchestrator(queue, slog.New(slog.DiscardHandler))

	cfg, err := ParseConfig([]byte(`
name: empty-matrix-pipeline
on:
  push:
    branches: [main]
jobs:
  detect:
    runs-on: builder
    steps:
      - uses: monofs/affected@v1
  build:
    needs: [detect]
    strategy:
      matrix:
        package: ${{ needs.detect.outputs.packages }}
    runs-on: builder
    steps:
      - run: make build-${{ matrix.package }}
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	orch.RegisterPipeline(cfg)

	run, _ := orch.StartRun(cfg, WebhookEvent{
		EventType: TriggerPush,
		CommitSHA: "sha-2",
		Branch:    "main",
	}, nil)

	waitFor(t, func() bool {
		current, err := orch.GetRun(run.RunID)
		return err == nil && current.Jobs["detect"].State == JobRunning
	})

	// Detect succeeds but reports no affected packages.
	orch.OnTaskResult(nil, &TaskResult{
		RunID:   run.RunID,
		JobName: "detect",
		State:   JobSucceeded,
		Outputs: map[string]string{"packages": ""},
	})

	// The build job resolves to an empty matrix and is skipped; the run
	// finishes successfully without it.
	waitFor(t, func() bool {
		current, err := orch.GetRun(run.RunID)
		return err == nil && isTerminalRunState(current.State)
	})

	current, _ := orch.GetRun(run.RunID)
	if current.Jobs["build"].State != JobSkipped {
		t.Errorf("build state = %q, want skipped", current.Jobs["build"].State)
	}
	if current.State != RunSucceeded {
		t.Errorf("run state = %q, want succeeded", current.State)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func BenchmarkStartRun(b *testing.B) {
	orch := testOrchestrator()
	cfg := orch.configs["test-pipeline"]
	event := WebhookEvent{EventType: TriggerPush, CommitSHA: "sha", Branch: "main"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		orch.StartRun(cfg, event, nil)
	}
}

func BenchmarkGetRun(b *testing.B) {
	orch := testOrchestrator()
	cfg := orch.configs["test-pipeline"]
	run, _ := orch.StartRun(cfg, WebhookEvent{EventType: TriggerPush, CommitSHA: "x", Branch: "m"}, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		orch.GetRun(run.RunID)
	}
}
