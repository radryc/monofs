package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type mockKVS struct {
	mu   sync.RWMutex
	data map[string]mockKVSEntry
}

type mockKVSEntry struct {
	content   []byte
	versionID string
}

func newMockKVS() *mockKVS {
	return &mockKVS{data: make(map[string]mockKVSEntry)}
}

func (m *mockKVS) Write(logicalPath string, content []byte, expectedVersionID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.data[logicalPath]
	if ok {
		if expectedVersionID == "absent" {
			return "", fmt.Errorf("already exists")
		}
		if expectedVersionID != "" && existing.versionID != expectedVersionID {
			return "", fmt.Errorf("version mismatch")
		}
	}

	versionID := fmt.Sprintf("v%d", len(m.data)+1)
	if ok {
		versionID = fmt.Sprintf("%s-%d", existing.versionID, len(m.data))
	}

	m.data[logicalPath] = mockKVSEntry{content: content, versionID: versionID}
	return versionID, nil
}

func (m *mockKVS) Read(logicalPath string) ([]byte, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.data[logicalPath]
	if !ok {
		return nil, "", fmt.Errorf("not found: %s", logicalPath)
	}
	return entry.content, entry.versionID, nil
}

func (m *mockKVS) Delete(logicalPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, logicalPath)
	return nil
}

func (m *mockKVS) List(logicalDir string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	logicalDir = strings.TrimSuffix(logicalDir, "/")
	prefix := logicalDir + "/"

	var entries []string
	for path := range m.data {
		if strings.HasPrefix(path, prefix) {
			entries = append(entries, path)
		}
	}
	return entries, nil
}

func TestTaskQueueEnqueue(t *testing.T) {
	kvs := newMockKVS()
	queue := NewTaskQueue(kvs)
	runID := "run-test-001"

	task := &Task{
		JobName:    "build",
		RunnerType: RunnerBuilder,
		Steps: []StepConfig{
			{Run: "make build-server"},
		},
		TimeoutSec: 300,
		MaxRetries: 2,
	}

	err := queue.EnqueueTask(runID, task)
	if err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	if task.TaskID == "" {
		t.Fatal("TaskID should be set after enqueue")
	}

	entries, err := kvs.List(queue.tasksDir(runID))
	if err != nil {
		t.Fatalf("List tasks: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 task, got %d entries", len(entries))
	}

	content, _, err := kvs.Read(entries[0])
	if err != nil {
		t.Fatalf("Read task: %v", err)
	}

	var readTask Task
	if err := json.Unmarshal(content, &readTask); err != nil {
		t.Fatalf("Unmarshal task: %v", err)
	}
	if readTask.JobName != "build" {
		t.Errorf("JobName = %q, want build", readTask.JobName)
	}
	if readTask.RunnerType != RunnerBuilder {
		t.Errorf("RunnerType = %q, want builder", readTask.RunnerType)
	}
}

func TestTaskQueueClaimAndResult(t *testing.T) {
	kvs := newMockKVS()
	queue := NewTaskQueue(kvs)
	runID := "run-claim-001"

	task := &Task{
		JobName:    "test",
		RunnerType: RunnerBuilder,
		Steps: []StepConfig{
			{Run: "make test"},
		},
		TimeoutSec: 300,
	}
	if err := queue.EnqueueTask(runID, task); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	claimed, err := queue.ClaimTask(runID, "worker-1")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected to claim a task")
	}
	if claimed.TaskID != task.TaskID {
		t.Errorf("claimed TaskID = %s, want %s", claimed.TaskID, task.TaskID)
	}
	if claimed.JobName != "test" {
		t.Errorf("claimed JobName = %s, want test", claimed.JobName)
	}

	second, err := queue.ClaimTask(runID, "worker-2")
	if err != nil {
		t.Fatalf("ClaimTask (second worker): %v", err)
	}
	if second != nil {
		t.Fatal("second worker should not claim the already-claimed task")
	}

	result := &TaskResult{
		TaskID:   task.TaskID,
		RunID:    runID,
		JobName:  "test",
		State:    JobSucceeded,
		ExitCode: 0,
		WorkerID: "worker-1",
	}
	if err := queue.WriteResult(runID, result); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}

	readResult, err := queue.GetResult(runID, task.TaskID)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if readResult.State != JobSucceeded {
		t.Errorf("result State = %q, want succeeded", readResult.State)
	}

	third, err := queue.ClaimTask(runID, "worker-3")
	if err != nil {
		t.Fatalf("ClaimTask (after result): %v", err)
	}
	if third != nil {
		t.Fatal("should not claim a finished task")
	}
}

func TestTaskQueueMultipleTasks(t *testing.T) {
	kvs := newMockKVS()
	queue := NewTaskQueue(kvs)
	runID := "run-multi-001"

	for i := 0; i < 5; i++ {
		task := &Task{
			JobName:    fmt.Sprintf("job-%d", i),
			RunnerType: RunnerBuilder,
			Steps: []StepConfig{
				{Run: fmt.Sprintf("make build-%d", i)},
			},
			TimeoutSec: 300,
		}
		if err := queue.EnqueueTask(runID, task); err != nil {
			t.Fatalf("EnqueueTask %d: %v", i, err)
		}
	}

	claimed := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		task, err := queue.ClaimTask(runID, fmt.Sprintf("worker-%d", i))
		if err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}
		if task == nil {
			t.Fatalf("expected task %d to be available", i)
		}
		claimed = append(claimed, task.TaskID)
	}

	extra, err := queue.ClaimTask(runID, "worker-extra")
	if err != nil {
		t.Fatalf("ClaimTask extra: %v", err)
	}
	if extra != nil {
		t.Fatal("no more tasks should be available")
	}

	if len(claimed) != 5 {
		t.Errorf("claimed %d tasks, want 5", len(claimed))
	}
}

func TestTaskQueueCleanup(t *testing.T) {
	kvs := newMockKVS()
	queue := NewTaskQueue(kvs)
	runID := "run-cleanup-001"

	for i := 0; i < 3; i++ {
		task := &Task{
			JobName:    fmt.Sprintf("job-%d", i),
			RunnerType: RunnerBuilder,
			Steps:      []StepConfig{{Run: "echo hello"}},
			TimeoutSec: 60,
		}
		queue.EnqueueTask(runID, task)
	}

	if err := queue.CleanupRun(runID); err != nil {
		t.Fatalf("CleanupRun: %v", err)
	}

	entries, _ := kvs.List(queue.tasksDir(runID))
	if len(entries) != 0 {
		t.Errorf("expected 0 tasks after cleanup, got %d", len(entries))
	}
}

func TestTaskQueueListRuns(t *testing.T) {
	kvs := newMockKVS()
	queue := NewTaskQueue(kvs)
	runID := "run-list-001"

	for i := 0; i < 3; i++ {
		queue.EnqueueTask(runID, &Task{
			JobName:    fmt.Sprintf("job-%d", i),
			RunnerType: RunnerBuilder,
			Steps:      []StepConfig{{Run: "echo"}},
			TimeoutSec: 60,
		})
	}

	tasks, err := queue.ListRunTasks(runID)
	if err != nil {
		t.Fatalf("ListRunTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Errorf("ListRunTasks got %d, want 3", len(tasks))
	}
}
