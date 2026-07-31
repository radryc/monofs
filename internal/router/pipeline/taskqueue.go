package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type KVSWriteFunc func(logicalPath string, content []byte, expectedVersionID string) (versionID string, _ error)
type KVSReadFunc func(logicalPath string) (content []byte, versionID string, _ error)
type KVSDeleteFunc func(logicalPath string) error
type KVSListFunc func(logicalDir string) ([]string, error)

type KVSClient interface {
	Write(logicalPath string, content []byte, expectedVersionID string) (versionID string, _ error)
	Read(logicalPath string) (content []byte, versionID string, _ error)
	Delete(logicalPath string) error
	List(logicalDir string) ([]string, error)
}

type TaskQueue struct {
	kvs       KVSClient
	runPrefix func(runID string) string
}

func NewTaskQueue(kvs KVSClient) *TaskQueue {
	return &TaskQueue{kvs: kvs, runPrefix: runQueuePrefix}
}

func runQueuePrefix(runID string) string {
	return fmt.Sprintf("%s/%s", QueuePrefix, runID)
}

func (q *TaskQueue) tasksDir(runID string) string {
	return q.runPrefix(runID) + "/tasks"
}

func (q *TaskQueue) claimsDir(runID string) string {
	return q.runPrefix(runID) + "/.claims"
}

func (q *TaskQueue) resultsDir(runID string) string {
	return q.runPrefix(runID) + "/.results"
}

func (q *TaskQueue) taskPath(runID, taskID string) string {
	return q.tasksDir(runID) + "/" + taskID + ".json"
}

func (q *TaskQueue) claimPath(runID, taskID string) string {
	return q.claimsDir(runID) + "/" + taskID + ".json"
}

func (q *TaskQueue) resultPath(runID, taskID string) string {
	return q.resultsDir(runID) + "/" + taskID + ".json"
}

func (q *TaskQueue) EnqueueTask(runID string, task *Task) error {
	task.TaskID = uuid.New().String()
	task.RunID = runID
	task.CreatedAt = time.Now()
	if task.MaxRetries == 0 {
		task.MaxRetries = 2
	}
	if task.TimeoutSec == 0 {
		task.TimeoutSec = 600
	}

	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}

	_, err = q.kvs.Write(q.taskPath(runID, task.TaskID), data, "")
	if err != nil {
		return fmt.Errorf("enqueue task: %w", err)
	}
	return nil
}

func (q *TaskQueue) EnqueueTasks(runID string, tasks []*Task) error {
	for _, task := range tasks {
		if err := q.EnqueueTask(runID, task); err != nil {
			return fmt.Errorf("enqueue task %s: %w", task.TaskID, err)
		}
	}
	return nil
}

func (q *TaskQueue) ClaimTask(runID, workerID string) (*Task, error) {
	entries, err := q.kvs.List(q.tasksDir(runID))
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	for _, entry := range entries {
		taskID := strings.TrimSuffix(strings.TrimPrefix(entry, q.tasksDir(runID)+"/"), ".json")
		if taskID == "" || strings.HasPrefix(taskID, ".") {
			continue
		}

		if claimed, _ := q.isTaskClaimed(runID, taskID); claimed {
			continue
		}
		if finished, _ := q.isTaskFinished(runID, taskID); finished {
			continue
		}

		claim := TaskClaim{
			TaskID:    taskID,
			RunID:     runID,
			JobName:   "",
			WorkerID:  workerID,
			ClaimedAt: time.Now(),
		}
		claimData, err := json.Marshal(claim)
		if err != nil {
			continue
		}

		_, err = q.kvs.Write(q.claimPath(runID, taskID), claimData, "")
		if err != nil {
			if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "FailedPrecondition") {
				continue
			}
			return nil, fmt.Errorf("write claim: %w", err)
		}

		data, _, err := q.kvs.Read(q.taskPath(runID, taskID))
		if err != nil {
			q.kvs.Delete(q.claimPath(runID, taskID))
			return nil, fmt.Errorf("read task: %w", err)
		}

		var task Task
		if err := json.Unmarshal(data, &task); err != nil {
			q.kvs.Delete(q.claimPath(runID, taskID))
			return nil, fmt.Errorf("unmarshal task: %w", err)
		}
		return &task, nil
	}

	return nil, nil
}

func (q *TaskQueue) isTaskClaimed(runID, taskID string) (bool, error) {
	_, _, err := q.kvs.Read(q.claimPath(runID, taskID))
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (q *TaskQueue) isTaskFinished(runID, taskID string) (bool, error) {
	_, _, err := q.kvs.Read(q.resultPath(runID, taskID))
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (q *TaskQueue) WriteResult(runID string, result *TaskResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	_, err = q.kvs.Write(q.resultPath(runID, result.TaskID), data, "")
	if err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func (q *TaskQueue) GetTask(runID, taskID string) (*Task, error) {
	data, _, err := q.kvs.Read(q.taskPath(runID, taskID))
	if err != nil {
		return nil, fmt.Errorf("read task: %w", err)
	}
	var task Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("unmarshal task: %w", err)
	}
	return &task, nil
}

func (q *TaskQueue) GetResult(runID, taskID string) (*TaskResult, error) {
	data, _, err := q.kvs.Read(q.resultPath(runID, taskID))
	if err != nil {
		return nil, fmt.Errorf("read result: %w", err)
	}
	var result TaskResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal result: %w", err)
	}
	return &result, nil
}

func (q *TaskQueue) ListRunTasks(runID string) ([]*Task, error) {
	entries, err := q.kvs.List(q.tasksDir(runID))
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var tasks []*Task
	for _, entry := range entries {
		taskID := strings.TrimSuffix(strings.TrimPrefix(entry, q.tasksDir(runID)+"/"), ".json")
		if taskID == "" || strings.HasPrefix(taskID, ".") {
			continue
		}
		task, err := q.GetTask(runID, taskID)
		if err != nil {
			continue
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (q *TaskQueue) ListRunResults(runID string) ([]*TaskResult, error) {
	entries, err := q.kvs.List(q.resultsDir(runID))
	if err != nil {
		return nil, fmt.Errorf("list results: %w", err)
	}
	var results []*TaskResult
	for _, entry := range entries {
		taskID := strings.TrimSuffix(strings.TrimPrefix(entry, q.resultsDir(runID)+"/"), ".json")
		if taskID == "" || strings.HasPrefix(taskID, ".") {
			continue
		}
		result, err := q.GetResult(runID, taskID)
		if err != nil {
			continue
		}
		results = append(results, result)
	}
	return results, nil
}

func (q *TaskQueue) CleanupRun(runID string) error {
	dirs := []string{q.tasksDir(runID), q.claimsDir(runID), q.resultsDir(runID)}
	for _, dir := range dirs {
		entries, err := q.kvs.List(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			target := entry
			if !strings.HasPrefix(entry, dir) {
				target = dir + "/" + entry
			}
			q.kvs.Delete(target)
		}
	}
	return nil
}
