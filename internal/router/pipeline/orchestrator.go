package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
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

	// onRunFinished, when set, is invoked (in its own goroutine) when a
	// run transitions into a terminal state.
	onRunFinished func(run *PipelineRun)
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

// SetOnRunFinished installs a callback invoked when a run finishes.
func (o *Orchestrator) SetOnRunFinished(fn func(run *PipelineRun)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.onRunFinished = fn
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
		RepoFullName: repoFullNameFromURL(event.RepoURL),
		RepoURL:      event.RepoURL,
		ChangedFiles: event.ChangedFiles,
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
	var notify bool

	o.mu.Lock()

	// Guard: only initialize a freshly-pending run. A run that has already
	// been driven to a terminal state (e.g. by a cancel or a direct task
	// result) must not be reset.
	if run.State != RunPending {
		o.mu.Unlock()
		return
	}

	o.setRunStateLocked(run, RunRunning)
	now := time.Now()
	run.StartedAt = &now

	entrypoints := cfg.EntrypointJobs()
	jobsStarted := 0
	for _, jobName := range entrypoints {
		if o.canRunJob(cfg, run, jobName, event, affected) {
			enqueued := o.enqueueJobTasks(run, cfg, jobName, affected)
			if enqueued > 0 {
				o.setJobStateLocked(run, jobName, JobRunning)
				jobsStarted++
			} else {
				o.setJobStateLocked(run, jobName, JobSkipped)
			}
		} else {
			o.setJobStateLocked(run, jobName, JobSkipped)
		}
	}

	if jobsStarted == 0 {
		o.setRunStateLocked(run, RunSucceeded)
		now := time.Now()
		run.FinishedAt = &now
		notify = true
	}

	o.mu.Unlock()

	if notify {
		o.notifyRunFinished(run)
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

// enqueueJobTasks enqueues the tasks for one job of a run, expanding any
// matrix strategy. It returns the number of tasks enqueued; zero means
// the job resolved to an empty matrix and should be skipped.
func (o *Orchestrator) enqueueJobTasks(run *PipelineRun, cfg *PipelineConfig, jobName string, affected []string) int {
	job, ok := cfg.Jobs[jobName]
	if !ok {
		return 0
	}

	if job.Strategy != nil && len(job.Strategy.Matrix) > 0 {
		matrixCombos, err := expandMatrix(job.Strategy.Matrix, func(expr string) ([]string, bool) {
			return resolveNeedsOutputExpr(expr, run)
		})
		if err != nil {
			o.logger.Error("matrix expansion failed", "job", jobName, "error", err)
			return 0
		}
		for _, combo := range matrixCombos {
			task := o.buildTask(run, jobName, job, combo)
			if err := o.queue.EnqueueTask(run.RunID, task); err != nil {
				o.logger.Error("enqueue task failed", "job", jobName, "error", err)
			}
		}
		return len(matrixCombos)
	}

	task := o.buildTask(run, jobName, job, nil)
	if err := o.queue.EnqueueTask(run.RunID, task); err != nil {
		o.logger.Error("enqueue task failed", "job", jobName, "error", err)
	}
	return 1
}

// resolveNeedsOutputExpr resolves a ${{ needs.<job>.outputs.<key> }}
// expression against the completed jobs of the run. It returns the
// comma-separated output split into values; false when the expression
// does not match the needs-outputs pattern.
func resolveNeedsOutputExpr(expr string, run *PipelineRun) ([]string, bool) {
	expr = strings.TrimSpace(expr)
	if !strings.HasPrefix(expr, "${{ needs.") || !strings.HasSuffix(expr, " }}") {
		return nil, false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(expr, "${{ needs."), " }}")
	parts := strings.Split(inner, ".outputs.")
	if len(parts) != 2 {
		return nil, false
	}
	neededJob, key := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if neededJob == "" || key == "" {
		return nil, false
	}
	if job, ok := run.Jobs[neededJob]; ok && job.Outputs != nil {
		if value, ok := job.Outputs[key]; ok {
			return splitOutputList(value), true
		}
	}
	return nil, true
}

// splitOutputList splits a comma-separated output value into trimmed,
// non-empty entries.
func splitOutputList(value string) []string {
	var result []string
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			result = append(result, entry)
		}
	}
	return result
}

func (o *Orchestrator) buildTask(run *PipelineRun, jobName string, job JobConfig, matrixVars map[string]string) *Task {
	steps := make([]StepConfig, len(job.Steps))
	copy(steps, job.Steps)

	needsOutputs := needsOutputResolver(run)
	for i := range steps {
		for k, v := range matrixVars {
			steps[i].Run = replaceVar(steps[i].Run, "matrix."+k, v)
		}
		steps[i].Run = substituteNeedsOutputs(steps[i].Run, needsOutputs)
	}

	timeout := job.TimeoutMinutes * 60
	if timeout == 0 {
		timeout = 600
	}

	return &Task{
		RunID:        run.RunID,
		JobName:      jobName,
		RunnerType:   job.RunsOn,
		Steps:        steps,
		TimeoutSec:   timeout,
		MaxRetries:   2,
		Affected:     run.Affected,
		ChangedFiles: run.ChangedFiles,
	}
}

// needsOutputResolver builds a "<job>.outputs.<key>" → output lookup.
func needsOutputResolver(run *PipelineRun) map[string]string {
	resolver := make(map[string]string)
	for jobName, job := range run.Jobs {
		for key, value := range job.Outputs {
			resolver[jobName+".outputs."+key] = value
		}
	}
	return resolver
}

// substituteNeedsOutputs replaces all ${{ needs.<job>.outputs.<key> }}
// occurrences in a step command with the corresponding upstream output.
func substituteNeedsOutputs(s string, resolver map[string]string) string {
	if !strings.Contains(s, "${{ needs.") {
		return s
	}
	for key, value := range resolver {
		s = replaceVar(s, "needs."+key, value)
	}
	return s
}

func (o *Orchestrator) OnTaskResult(ctx context.Context, result *TaskResult) {
	o.mu.Lock()

	run, ok := o.runs[result.RunID]
	if !ok {
		o.mu.Unlock()
		return
	}

	prevState := run.State

	job, ok := run.Jobs[result.JobName]
	if !ok {
		o.mu.Unlock()
		return
	}

	switch result.State {
	case JobSucceeded:
		job.State = JobSucceeded
		job.WorkerID = result.WorkerID
		job.ExitCode = result.ExitCode
		job.Outputs = result.Outputs
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
			job.WorkerID = result.WorkerID
			job.ExitCode = result.ExitCode
			o.setRunStateLocked(run, RunFailed)
			now := time.Now()
			run.FinishedAt = &now
		}
	}

	o.mu.Unlock()

	if !isTerminalRunState(prevState) && isTerminalRunState(run.State) {
		o.notifyRunFinished(run)
	}
}

// notifyRunFinished invokes the run-finished callback with a snapshot of
// the run. The snapshot is taken under the lock so it never races with the
// run's state transitions.
func (o *Orchestrator) notifyRunFinished(run *PipelineRun) {
	o.mu.RLock()
	fn := o.onRunFinished
	if fn == nil {
		o.mu.RUnlock()
		return
	}
	cloned := cloneRun(run)
	o.mu.RUnlock()

	go fn(&cloned)
}

func isTerminalRunState(state RunState) bool {
	return state == RunSucceeded || state == RunFailed || state == RunCancelled
}

// repoFullNameFromURL extracts the "owner/repo" identity from a repository
// URL such as https://github.com/org/repo or https://gitlab.com/group/repo.git.
func repoFullNameFromURL(repoURL string) string {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return ""
	}
	if u, err := url.Parse(repoURL); err == nil && u.Path != "" {
		repoURL = u.Path
	}
	repoURL = strings.TrimPrefix(repoURL, "/")
	repoURL = strings.TrimSuffix(repoURL, ".git")
	parts := strings.Split(repoURL, "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

func (o *Orchestrator) advancePipeline(ctx context.Context, run *PipelineRun) {
	cfg, ok := o.configs[run.PipelineName]
	if !ok {
		return
	}

	completed := make(map[string]bool)
	for name, job := range run.Jobs {
		if job.State == JobSucceeded || job.State == JobSkipped || job.State == JobFailed || job.State == JobCancelled {
			completed[name] = true
		}
	}

	// Advance to a fixpoint: a job that resolves to an empty matrix is
	// skipped immediately and may unblock its own downstream jobs in the
	// same pass.
	maxPasses := len(run.Jobs) + 1
	for pass := 0; pass < maxPasses; pass++ {
		progressed := false
		for jobName, job := range run.Jobs {
			if job.State != JobPending {
				continue
			}
			if !cfg.AllNeedsSatisfied(jobName, completed) {
				continue
			}
			enqueued := o.enqueueJobTasks(run, cfg, jobName, run.Affected)
			if enqueued > 0 {
				o.setJobStateLocked(run, jobName, JobRunning)
			} else {
				o.setJobStateLocked(run, jobName, JobSkipped)
				completed[jobName] = true
			}
			progressed = true
		}
		if !progressed {
			break
		}
	}

	// The run is finished once every job reached a terminal state.
	allDone := true
	for _, job := range run.Jobs {
		if !completed[job.JobName] && job.State != JobSucceeded && job.State != JobSkipped && job.State != JobFailed && job.State != JobCancelled {
			allDone = false
			break
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

	run, ok := o.runs[runID]
	if !ok {
		o.mu.Unlock()
		return fmt.Errorf("run not found: %s", runID)
	}

	if isTerminalRunState(run.State) {
		o.mu.Unlock()
		return fmt.Errorf("run already finished: %s", run.State)
	}

	o.cancelRunLocked(run)
	o.mu.Unlock()

	o.notifyRunFinished(run)
	return nil
}

// cloneRun returns a shallow copy of run with a copied Jobs map.
func cloneRun(run *PipelineRun) PipelineRun {
	cloned := *run
	cloned.Jobs = make(map[string]*JobStatus, len(run.Jobs))
	for k, v := range run.Jobs {
		vc := *v
		cloned.Jobs[k] = &vc
	}
	return cloned
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

func (o *Orchestrator) setRunStateLocked(run *PipelineRun, state RunState) {
	run.State = state
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

// expandMatrix computes the matrix combinations. Static values are used
// as-is; expression values are resolved through the supplied resolver
// (typically needs-outputs) and split on commas. An error is returned
// when an expression cannot be resolved against any known output.
func expandMatrix(m MatrixConfig, resolve func(expr string) ([]string, bool)) ([]map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}

	var keys []string
	var values [][]string
	for k, v := range m {
		keys = append(keys, k)
		if v.Expr != "" {
			resolved, ok := resolve(v.Expr)
			if !ok {
				return nil, fmt.Errorf("unresolved matrix expression %q", v.Expr)
			}
			values = append(values, resolved)
			continue
		}
		if len(v.Static) == 0 {
			return nil, nil
		}
		values = append(values, v.Static)
	}

	var result []map[string]string
	expandMatrixRecursive(&result, keys, values, make(map[string]string), 0)
	return result, nil
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
