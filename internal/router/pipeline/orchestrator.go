package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Orchestrator struct {
	mu      sync.RWMutex
	runs    map[string]*PipelineRun
	configs map[string]*PipelineConfig
	queue   *TaskQueue
	logger  *slog.Logger

	runHistory []*PipelineRun
	maxHistory int
}

func NewOrchestrator(queue *TaskQueue, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{
		runs:       make(map[string]*PipelineRun),
		configs:    make(map[string]*PipelineConfig),
		queue:      queue,
		logger:     logger,
		maxHistory: 100,
	}
}

func (o *Orchestrator) RegisterPipeline(cfg *PipelineConfig) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.configs[cfg.Name] = cfg
}

func (o *Orchestrator) UnregisterPipeline(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.configs, name)
}

func (o *Orchestrator) ListPipelines() []*PipelineConfig {
	o.mu.RLock()
	defer o.mu.RUnlock()
	result := make([]*PipelineConfig, 0, len(o.configs))
	for _, cfg := range o.configs {
		result = append(result, cfg)
	}
	return result
}

func (o *Orchestrator) StartRun(cfg *PipelineConfig, event WebhookEvent, affected []string) (*PipelineRun, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if cfg.Concurrency != nil && cfg.Concurrency.CancelInProgress {
		o.cancelExistingRuns(cfg.Name, cfg.Concurrency.Group)
	}

	run := &PipelineRun{
		RunID:        uuid.New().String(),
		PipelineName: cfg.Name,
		State:        RunPending,
		Trigger:      event.EventType,
		CommitSHA:    event.CommitSHA,
		Branch:       event.Branch,
		Tag:          event.Tag,
		PRNumber:     event.PRNumber,
		CreatedAt:    time.Now(),
		Jobs:         make(map[string]*JobStatus),
		Affected:     affected,
	}

	for jobName, jobConfig := range cfg.Jobs {
		run.Jobs[jobName] = &JobStatus{
			JobName:    jobName,
			State:      JobPending,
			MaxRetries: 2,
			Needs:      jobConfig.Needs,
		}
	}

	o.runs[run.RunID] = run
	o.addToHistory(run)

	o.logger.Info("pipeline run started",
		"pipeline", cfg.Name,
		"run_id", run.RunID,
		"trigger", event.EventType,
	)

	go o.executeRun(run, cfg, event, affected)

	return run, nil
}

func (o *Orchestrator) cancelExistingRuns(pipelineName, group string) {
	for _, run := range o.runs {
		if run.PipelineName == pipelineName && run.State == RunRunning {
			o.logger.Info("cancelling existing run for concurrency group",
				"run_id", run.RunID,
				"group", group,
			)
			o.cancelRunLocked(run)
		}
	}
}

func (o *Orchestrator) executeRun(run *PipelineRun, cfg *PipelineConfig, event WebhookEvent, affected []string) {
	o.setRunState(run, RunRunning)

	now := time.Now()
	run.StartedAt = &now

	entrypoints := cfg.EntrypointJobs()
	jobsStarted := 0
	for _, jobName := range entrypoints {
		if o.canRunJob(cfg, run, jobName, event, affected) {
			o.enqueueJobTasks(run, cfg, jobName, affected)
			o.setJobState(run, jobName, JobRunning)
			jobsStarted++
		} else {
			o.setJobState(run, jobName, JobSkipped)
		}
	}

	if jobsStarted == 0 {
		o.setRunState(run, RunSucceeded)
		now := time.Now()
		run.FinishedAt = &now
	}
}

func (o *Orchestrator) canRunJob(cfg *PipelineConfig, run *PipelineRun, jobName string, event WebhookEvent, affected []string) bool {
	job, ok := cfg.Jobs[jobName]
	if !ok {
		return false
	}

	if job.If != "" && !o.evaluateCondition(job.If, run, affected) {
		return false
	}

	return true
}

func (o *Orchestrator) evaluateCondition(condition string, run *PipelineRun, affected []string) bool {
	if condition == "always()" || condition == "" {
		return true
	}
	if condition == "failure()" {
		return run.State == RunFailed
	}
	if condition == "success()" {
		return run.State != RunFailed
	}
	if condition == "cancelled()" {
		return run.State == RunCancelled
	}
	if condition == "affected != ''" {
		return len(affected) > 0
	}
	return true
}

func (o *Orchestrator) enqueueJobTasks(run *PipelineRun, cfg *PipelineConfig, jobName string, affected []string) {
	job, ok := cfg.Jobs[jobName]
	if !ok {
		return
	}

	if job.Strategy != nil && len(job.Strategy.Matrix) > 0 {
		matrixCombos := expandMatrix(job.Strategy.Matrix)
		for _, combo := range matrixCombos {
			task := o.buildTask(run.RunID, jobName, job, combo)
			if err := o.queue.EnqueueTask(run.RunID, task); err != nil {
				o.logger.Error("enqueue task failed", "job", jobName, "error", err)
			}
		}
	} else {
		task := o.buildTask(run.RunID, jobName, job, nil)
		if err := o.queue.EnqueueTask(run.RunID, task); err != nil {
			o.logger.Error("enqueue task failed", "job", jobName, "error", err)
		}
	}
}

func (o *Orchestrator) buildTask(runID, jobName string, job JobConfig, matrixVars map[string]string) *Task {
	steps := make([]StepConfig, len(job.Steps))
	copy(steps, job.Steps)

	for i := range steps {
		for k, v := range matrixVars {
			steps[i].Run = replaceVar(steps[i].Run, "matrix."+k, v)
		}
	}

	timeout := job.TimeoutMinutes * 60
	if timeout == 0 {
		timeout = 600
	}

	return &Task{
		RunID:      runID,
		JobName:    jobName,
		RunnerType: job.RunsOn,
		Steps:      steps,
		TimeoutSec: timeout,
		MaxRetries: 2,
	}
}

func (o *Orchestrator) OnTaskResult(ctx context.Context, result *TaskResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	run, ok := o.runs[result.RunID]
	if !ok {
		return
	}

	job, ok := run.Jobs[result.JobName]
	if !ok {
		return
	}

	switch result.State {
	case JobSucceeded:
		job.State = JobSucceeded
		job.FinishedAt = &result.EndedAt
		o.advancePipeline(ctx, run)

	case JobFailed:
		job.Retries++
		if job.Retries < job.MaxRetries {
			job.State = JobRunning
			o.logger.Info("retrying job", "job", result.JobName, "retry", job.Retries)
			if err := o.queue.EnqueueTask(result.RunID, &Task{
				RunID:      result.RunID,
				JobName:    result.JobName,
				RunnerType: RunnerBuilder,
				TimeoutSec: 600,
				MaxRetries: job.MaxRetries - job.Retries,
			}); err != nil {
				o.logger.Error("retry enqueue failed", "job", result.JobName, "error", err)
				job.State = JobFailed
			}
		} else {
			job.State = JobFailed
			job.Error = result.Error
			o.setRunStateLocked(run, RunFailed)
			now := time.Now()
			run.FinishedAt = &now
		}
	}
}

func (o *Orchestrator) advancePipeline(ctx context.Context, run *PipelineRun) {
	cfg, ok := o.configs[run.PipelineName]
	if !ok {
		return
	}

	completed := make(map[string]bool)
	allDone := true
	for name, job := range run.Jobs {
		if job.State == JobSucceeded || job.State == JobSkipped {
			completed[name] = true
		} else if job.State != JobFailed {
			allDone = false
		} else {
			completed[name] = true
		}
	}

	for jobName, job := range run.Jobs {
		if job.State != JobPending {
			continue
		}
		if cfg.AllNeedsSatisfied(jobName, completed) {
			o.enqueueJobTasks(run, cfg, jobName, run.Affected)
			o.setJobStateLocked(run, jobName, JobRunning)
			allDone = false
		}
	}

	if allDone {
		if o.anyJobFailed(run) {
			o.setRunStateLocked(run, RunFailed)
		} else {
			o.setRunStateLocked(run, RunSucceeded)
		}
		now := time.Now()
		run.FinishedAt = &now
	}
}

func (o *Orchestrator) CancelRun(runID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	run, ok := o.runs[runID]
	if !ok {
		return fmt.Errorf("run not found: %s", runID)
	}

	if run.State == RunSucceeded || run.State == RunFailed || run.State == RunCancelled {
		return fmt.Errorf("run already finished: %s", run.State)
	}

	o.cancelRunLocked(run)
	return nil
}

func (o *Orchestrator) cancelRunLocked(run *PipelineRun) {
	run.State = RunCancelled
	now := time.Now()
	run.FinishedAt = &now
	for _, job := range run.Jobs {
		if job.State == JobPending || job.State == JobClaimed || job.State == JobRunning {
			job.State = JobCancelled
		}
	}
}

func (o *Orchestrator) GetRun(runID string) (*PipelineRun, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	run, ok := o.runs[runID]
	if !ok {
		return nil, fmt.Errorf("run not found: %s", runID)
	}
	cloned := *run
	cloned.Jobs = make(map[string]*JobStatus, len(run.Jobs))
	for k, v := range run.Jobs {
		vc := *v
		cloned.Jobs[k] = &vc
	}
	return &cloned, nil
}

func (o *Orchestrator) ListRuns(pipelineName string, limit int) []*PipelineRun {
	o.mu.RLock()
	defer o.mu.RUnlock()

	var result []*PipelineRun
	for i := len(o.runHistory) - 1; i >= 0; i-- {
		run := o.runHistory[i]
		if pipelineName == "" || run.PipelineName == pipelineName {
			result = append(result, run)
			if len(result) >= limit {
				break
			}
		}
	}
	if result == nil {
		result = []*PipelineRun{}
	}
	return result
}

func (o *Orchestrator) GetStats(pipelineName string) PipelineStats {
	o.mu.RLock()
	defer o.mu.RUnlock()

	stats := PipelineStats{}
	var durations []int64

	for _, run := range o.runHistory {
		if pipelineName != "" && run.PipelineName != pipelineName {
			continue
		}
		stats.TotalRuns++
		switch run.State {
		case RunSucceeded:
			stats.SucceededRuns++
		case RunFailed:
			stats.FailedRuns++
		}

		if run.StartedAt != nil && run.FinishedAt != nil {
			d := run.FinishedAt.Sub(*run.StartedAt).Milliseconds()
			durations = append(durations, d)
		}
	}

	if stats.TotalRuns > 0 {
		stats.SuccessRate = float64(stats.SucceededRuns) / float64(stats.TotalRuns) * 100
	}

	if len(durations) > 0 {
		var total int64
		for _, d := range durations {
			total += d
		}
		stats.AvgDurationMs = total / int64(len(durations))
		stats.P50DurationMs = percentile(durations, 50)
		stats.P95DurationMs = percentile(durations, 95)
	}

	return stats
}

func (o *Orchestrator) PipelineConfigs() map[string]*PipelineConfig {
	o.mu.RLock()
	defer o.mu.RUnlock()
	result := make(map[string]*PipelineConfig, len(o.configs))
	for k, v := range o.configs {
		result[k] = v
	}
	return result
}

func (o *Orchestrator) setRunState(run *PipelineRun, state RunState) {
	o.mu.Lock()
	o.setRunStateLocked(run, state)
	o.mu.Unlock()
}

func (o *Orchestrator) setRunStateLocked(run *PipelineRun, state RunState) {
	run.State = state
}

func (o *Orchestrator) setJobState(run *PipelineRun, jobName string, state JobState) {
	o.mu.Lock()
	o.setJobStateLocked(run, jobName, state)
	o.mu.Unlock()
}

func (o *Orchestrator) setJobStateLocked(run *PipelineRun, jobName string, state JobState) {
	if job, ok := run.Jobs[jobName]; ok {
		job.State = state
	}
}

func (o *Orchestrator) anyJobFailed(run *PipelineRun) bool {
	for _, job := range run.Jobs {
		if job.State == JobFailed {
			return true
		}
	}
	return false
}

func (o *Orchestrator) addToHistory(run *PipelineRun) {
	o.runHistory = append(o.runHistory, run)
	if len(o.runHistory) > o.maxHistory {
		o.runHistory = o.runHistory[1:]
	}
}

func expandMatrix(m map[string][]string) []map[string]string {
	if len(m) == 0 {
		return nil
	}

	var keys []string
	var values [][]string
	for k, v := range m {
		keys = append(keys, k)
		values = append(values, v)
	}

	var result []map[string]string
	expandMatrixRecursive(&result, keys, values, make(map[string]string), 0)
	return result
}

func expandMatrixRecursive(result *[]map[string]string, keys []string, values [][]string, current map[string]string, depth int) {
	if depth == len(keys) {
		combo := make(map[string]string, len(current))
		for k, v := range current {
			combo[k] = v
		}
		*result = append(*result, combo)
		return
	}
	for _, val := range values[depth] {
		current[keys[depth]] = val
		expandMatrixRecursive(result, keys, values, current, depth+1)
	}
}

func replaceVar(s, varName, value string) string {
	placeholder := "${{ " + varName + " }}"
	return stringsReplace(s, placeholder, value)
}

func stringsReplace(s, old, new string) string {
	result := ""
	for {
		idx := indexOf(s, old)
		if idx < 0 {
			result += s
			break
		}
		result += s[:idx] + new
		s = s[idx+len(old):]
	}
	return result
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func percentile(durations []int64, p int) int64 {
	if len(durations) == 0 {
		return 0
	}
	sorted := make([]int64, len(durations))
	copy(sorted, durations)

	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	idx := len(sorted) * p / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
