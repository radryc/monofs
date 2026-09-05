package pipeline

import (
	"log/slog"
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
	matrix := map[string][]string{}
	result := expandMatrix(matrix)
	if result != nil {
		t.Fatal("expected nil for empty matrix")
	}

	single := map[string][]string{"x": {"a"}}
	result = expandMatrix(single)
	if len(result) != 1 {
		t.Fatalf("expected 1 combination, got %d", len(result))
	}
	if result[0]["x"] != "a" {
		t.Errorf("expected x=a, got x=%s", result[0]["x"])
	}
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
