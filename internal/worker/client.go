package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

type Client struct {
	router   pb.MonoFSRouterClient
	token    string
	workerID string
	logger   *slog.Logger
}

func NewClient(conn grpc.ClientConnInterface, token, workerID string, logger *slog.Logger) (*Client, error) {
	if workerID == "" {
		workerID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	return &Client{
		router:   pb.NewMonoFSRouterClient(conn),
		token:    token,
		workerID: workerID,
		logger:   logger,
	}, nil
}

func (c *Client) SubscribeChanges(ctx context.Context, prefixes []string) (<-chan *pb.GuardianChangeEvent, error) {
	stream, err := c.router.SubscribeGuardianChanges(ctx, &pb.SubscribeGuardianChangesRequest{
		GuardianToken:        c.token,
		LogicalPrefixes:      prefixes,
		IncludeInlineContent: true,
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	ch := make(chan *pb.GuardianChangeEvent, 256)
	go func() {
		defer close(ch)
		for {
			event, err := stream.Recv()
			if err != nil {
				if err != io.EOF && !isContextError(err) {
					c.logger.Error("subscribe stream error", "error", err)
				}
				return
			}
			select {
			case ch <- event:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

func (c *Client) WritePath(ctx context.Context, logicalPath string, content []byte, expectedVersion string) error {
	_, err := c.router.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
		GuardianToken: c.token,
		Writes: []*pb.GuardianPathWrite{
			{
				LogicalPath:       logicalPath,
				Content:           content,
				ExpectedVersionId: expectedVersion,
			},
		},
	})
	return err
}

func (c *Client) WorkerID() string {
	return c.workerID
}

type TaskData struct {
	TaskID     string     `json:"task_id"`
	RunID      string     `json:"run_id"`
	JobName    string     `json:"job_name"`
	RunnerType string     `json:"runner_type"`
	Steps      []StepData `json:"steps"`
	TimeoutSec int        `json:"timeout_sec"`
	MaxRetries int        `json:"max_retries"`
}

type StepData struct {
	Name string            `json:"name"`
	Run  string            `json:"run"`
	Uses string            `json:"uses"`
	With map[string]string `json:"with"`
	ID   string            `json:"id"`
}

type ResultData struct {
	TaskID    string `json:"task_id"`
	RunID     string `json:"run_id"`
	JobName   string `json:"job_name"`
	State     string `json:"state"`
	ExitCode  int    `json:"exit_code"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	WorkerID  string `json:"worker_id"`
}

type ClaimData struct {
	TaskID    string `json:"task_id"`
	RunID     string `json:"run_id"`
	JobName   string `json:"job_name"`
	WorkerID  string `json:"worker_id"`
	ClaimedAt string `json:"claimed_at"`
}

func queuePrefix(runID string) string {
	return "/.queues/pipeline/" + runID
}

func taskPath(runID, taskID string) string {
	return queuePrefix(runID) + "/tasks/" + taskID + ".json"
}

func claimPath(runID, taskID string) string {
	return queuePrefix(runID) + "/.claims/" + taskID + ".json"
}

func resultPath(runID, taskID string) string {
	return queuePrefix(runID) + "/.results/" + taskID + ".json"
}

func isContextError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "context canceled") ||
		strings.Contains(err.Error(), "context deadline exceeded")
}

type Handler interface {
	Execute(ctx context.Context, task *TaskData, logWriter io.Writer) (int, error)
}

type Worker struct {
	client      *Client
	handler     Handler
	concurrency int
	logger      *slog.Logger
	sem         chan struct{}
}

func New(client *Client, handler Handler, concurrency int, logger *slog.Logger) *Worker {
	return &Worker{
		client:      client,
		handler:     handler,
		concurrency: concurrency,
		logger:      logger,
		sem:         make(chan struct{}, concurrency),
	}
}

const (
	initialBackoff = time.Second
	maxBackoff     = time.Minute
)

func (w *Worker) Run(ctx context.Context) error {
	backoff := initialBackoff
	for {
		err := w.runOnce(ctx)
		if err == nil {
			return nil
		}
		if isContextError(err) {
			return err
		}
		w.logger.Warn("worker event stream ended, reconnecting",
			"backoff", backoff,
			"error", err,
		)
		select {
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (w *Worker) runOnce(ctx context.Context) error {
	events, err := w.client.SubscribeChanges(ctx, []string{"/.queues/pipeline"})
	if err != nil {
		return fmt.Errorf("subscribe to pipeline changes: %w", err)
	}

	w.logger.Info("worker ready, listening for pipeline tasks")

	for {
		select {
		case event, ok := <-events:
			if !ok {
				return fmt.Errorf("event stream closed")
			}
			if event.GetType() != pb.ChangeType_ADDED {
				continue
			}
			if !strings.Contains(event.GetLogicalPath(), "/tasks/") {
				continue
			}
			w.processTaskEvent(ctx, event)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (w *Worker) processTaskEvent(ctx context.Context, event *pb.GuardianChangeEvent) {
	logicalPath := event.GetLogicalPath()

	parts := strings.Split(strings.TrimPrefix(logicalPath, "/.queues/pipeline/"), "/")
	if len(parts) < 3 {
		return
	}
	runID := parts[0]
	taskID := strings.TrimSuffix(parts[len(parts)-1], ".json")

	content := event.GetInlineContent()
	if len(content) == 0 {
		w.logger.Warn("task event has no inline content, skipping", "task_id", taskID)
		return
	}

	var task TaskData
	if err := json.Unmarshal(content, &task); err != nil {
		w.logger.Warn("unmarshal task content failed", "task_id", taskID, "error", err)
		return
	}

	w.sem <- struct{}{}
	go func() {
		defer func() { <-w.sem }()
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("task execution panicked", "task_id", taskID, "run_id", runID, "panic", r)
				w.writeResult(ctx, resultPath(runID, taskID), ResultData{
					TaskID:    taskID,
					RunID:     runID,
					JobName:   task.JobName,
					State:     "failed",
					ExitCode:  -1,
					Error:     fmt.Sprintf("panic: %v", r),
					StartedAt: time.Now().UTC().Format(time.RFC3339),
					EndedAt:   time.Now().UTC().Format(time.RFC3339),
					WorkerID:  w.client.WorkerID(),
				})
			}
		}()
		w.executeTask(ctx, runID, taskID, &task)
	}()
}

func (w *Worker) executeTask(ctx context.Context, runID, taskID string, task *TaskData) {
	cPath := claimPath(runID, taskID)
	rPath := resultPath(runID, taskID)

	claim := ClaimData{
		TaskID:    taskID,
		RunID:     runID,
		JobName:   task.JobName,
		WorkerID:  w.client.WorkerID(),
		ClaimedAt: time.Now().UTC().Format(time.RFC3339),
	}
	claimData, _ := json.Marshal(claim)
	if err := w.client.WritePath(ctx, cPath, claimData, ""); err != nil {
		w.logger.Debug("claim failed (already claimed)", "task_id", taskID)
		return
	}

	taskCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TimeoutSec)*time.Second)
	defer cancel()

	w.logger.Info("executing task", "task_id", taskID, "job", task.JobName, "steps", len(task.Steps))

	startedAt := time.Now().UTC()
	exitCode, execErr := w.handler.Execute(taskCtx, task, discardWriter{})
	endedAt := time.Now().UTC()

	state := "succeeded"
	errMsg := ""
	if execErr != nil {
		state = "failed"
		errMsg = execErr.Error()
	}

	w.writeResult(ctx, rPath, ResultData{
		TaskID:    taskID,
		RunID:     runID,
		JobName:   task.JobName,
		State:     state,
		ExitCode:  exitCode,
		Error:     errMsg,
		StartedAt: startedAt.Format(time.RFC3339),
		EndedAt:   endedAt.Format(time.RFC3339),
		WorkerID:  w.client.WorkerID(),
	})

	w.logger.Info("task completed", "task_id", taskID, "state", state,
		"duration", endedAt.Sub(startedAt).String())
}

func (w *Worker) writeResult(ctx context.Context, path string, result ResultData) {
	data, _ := json.Marshal(result)
	if err := w.client.WritePath(ctx, path, data, ""); err != nil {
		w.logger.Error("write result failed", "path", path, "error", err)
	}
}

type discardWriter struct{}

func (d discardWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

var _ io.Writer = discardWriter{}
