package worker

import (
	"testing"
)

func TestQueuePaths(t *testing.T) {
	runID := "run-abc123"
	taskID := "task-xyz789"

	taskP := taskPath(runID, taskID)
	if taskP != "/.queues/pipeline/run-abc123/tasks/task-xyz789.json" {
		t.Errorf("taskPath = %q", taskP)
	}

	claimP := claimPath(runID, taskID)
	if claimP != "/.queues/pipeline/run-abc123/.claims/task-xyz789.json" {
		t.Errorf("claimPath = %q", claimP)
	}

	resultP := resultPath(runID, taskID)
	if resultP != "/.queues/pipeline/run-abc123/.results/task-xyz789.json" {
		t.Errorf("resultPath = %q", resultP)
	}

	prefix := queuePrefix(runID)
	if prefix != "/.queues/pipeline/run-abc123" {
		t.Errorf("queuePrefix = %q", prefix)
	}
}

func TestIsContextError(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"context canceled", true},
		{"context deadline exceeded", true},
		{"rpc error: context canceled", true},
		{"connection refused", false},
		{"EOF", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			var err error
			if tt.input != "" {
				err = simpleError(tt.input)
			}
			if got := isContextError(err); got != tt.want {
				t.Errorf("isContextError(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

type simpleError string

func (e simpleError) Error() string { return string(e) }
