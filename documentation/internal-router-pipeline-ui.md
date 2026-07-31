# MonoFS — Internal Router Pipeline & UI Documentation

## Table of Contents
- [internal/router/pipeline/affected.go](#pipelineaffectedgo)
- [internal/router/pipeline/config.go](#pipelineconfiggo)
- [internal/router/pipeline/discover.go](#pipelinediscovergo)
- [internal/router/pipeline/orchestrator.go](#pipelineorchestratorgo)
- [internal/router/pipeline/reporter.go](#pipelinereportergo)
- [internal/router/pipeline/taskqueue.go](#pipelinetaskqueuego)
- [internal/router/pipeline/types.go](#pipelinetypesgo)
- [internal/router/pipeline/webhook.go](#pipelinewebhookgo)
- [internal/router/pipeline_kvs.go](#pipelinekvsgo)
- [internal/router/pipeline_router.go](#pipelineroutergo)
- [internal/router/pipeline_ui_handler.go](#pipelineuihandlergo)
- [internal/router/ui.go](#internalrouteruigo)
- [internal/router/ui_handler.go](#internalrouteruihandlergo)
- [internal/router/ui_types.go](#internalrouteruitypesgo)

---

## pipeline/affected.go

Package: `pipeline`

### `DetectAffectedPackages(meta *PackageMeta, changedFiles []string) []string`

**Signature:** `func DetectAffectedPackages(meta *PackageMeta, changedFiles []string) []string`

Determines which packages from a `PackageMeta` are affected by a list of changed files. For each changed file, it iterates over all packages and marks a package as affected if the file matches the package's own path or any of its dependency paths. Uses a `map[string]bool` for deduplication. Returns a de-duplicated slice of package names that were affected.

**Parameters:**
- `meta` — the parsed `PackageMeta` containing `Packages` map keyed by name.
- `changedFiles` — list of file paths that changed (from `git diff`).

**Callers:** `WebhookHandler.processEvent()` in `webhook.go:157`.

### `isPathAffected(file, prefix string) bool`

**Signature:** `func isPathAffected(file, prefix string) bool`

Checks whether a `file` path falls within or equals a `prefix` directory. Both are cleaned with `filepath.Clean`. Returns true if `file == prefix` or if `file` starts with `prefix + "/"`.

**Callers:** `DetectAffectedPackages()` internally; `PipelineConfig.MatchEvent()` in `config.go:155` for `SourceDir` filtering.

### `ComputeChangedFiles(baseRef, headRef string) ([]string, error)`

**Signature:** `func ComputeChangedFiles(baseRef, headRef string) ([]string, error)`

Runs `git diff --name-only baseRef..headRef` and returns the list of changed file paths. Returns nil, nil if the diff output is empty. Trims whitespace and filters out empty lines from the output.

**Parameters:**
- `baseRef` — the base git reference (e.g., `HEAD~1`).
- `headRef` — the head git reference (e.g., `HEAD`).

**Callers:** `WebhookHandler.parseGitHubPullRequest()` in `webhook.go:258`.

### `ComputeChangedFilesFromHead(n int) ([]string, error)`

**Signature:** `func ComputeChangedFilesFromHead(n int) ([]string, error)`

Runs `git diff --name-only HEAD~n` and returns the list of changed file paths. Uses the helper `itoa` to convert the integer to string. Returns nil, nil if the diff output is empty.

**Parameters:**
- `n` — number of commits back from HEAD.

**Callers:** `WebhookHandler.processEvent()` in `webhook.go:155` as a fallback when no changed files are available.

### `itoa(n int) string`

**Signature:** `func itoa(n int) string`

A simple integer-to-string conversion without importing `strconv`. Handles zero specially. Constructs the string by extracting decimal digits in reverse order.

**Callers:** Only `ComputeChangedFilesFromHead()`.

### `ResolveAffectedBuildTargets(meta *PackageMeta, affected []string) []string`

**Signature:** `func ResolveAffectedBuildTargets(meta *PackageMeta, affected []string) []string`

For each affected package name, looks up its `Build` target in `PackageMeta.Packages`. Returns a slice of build target strings (e.g., Bazel targets). Skips packages with empty build targets.

**Parameters:**
- `meta` — parsed package metadata.
- `affected` — list of affected package names.

**Callers:** Currently not directly called by any code in the scanned files (may be referenced elsewhere or prepared for future use).

---

## pipeline/config.go

Package: `pipeline`

### `LoadConfig(path string) (*PipelineConfig, error)`

**Signature:** `func LoadConfig(path string) (*PipelineConfig, error)`

Reads a YAML pipeline config from the given filesystem `path`, then parses and validates it via `ParseConfig`. Wraps OS read errors with context.

**Callers:** `DiscoverPipelines()` in `discover.go:47` (called for each `.yml` file found under `.monofs/pipelines/` during directory walk).

### `ParseConfig(data []byte) (*PipelineConfig, error)`

**Signature:** `func ParseConfig(data []byte) (*PipelineConfig, error)`

Unmarshals YAML bytes into a `PipelineConfig` and calls `Validate()` on the result. Returns a pointer to the validated config or an error.

**Callers:**
- `LoadConfig()` in `config.go:17`.
- `Router.loadPipelinesFromKVS()` in `pipeline_router.go:65` (parsing config stored in KVS).
- `Router.watchPipelinesFromKVS()` in `pipeline_router.go:107` (parsing config from change events).

### `(c *PipelineConfig) Validate() error`

**Signature:** `func (c *PipelineConfig) Validate() error`

Validates the pipeline configuration:
1. Requires `Name` to be non-empty.
2. Requires at least one job in `Jobs`.
3. Validates each `JobConfig` via `JobConfig.Validate()`.
4. Validates the DAG via `ValidateDAG()`.

**Callers:** `ParseConfig()` in `config.go:25`.

### `(j *JobConfig) Validate() error`

**Signature:** `func (j *JobConfig) Validate() error`

Validates a single job config. Currently checks that `Steps` is non-empty (at least one step required).

**Callers:** `PipelineConfig.Validate()` in `config.go:39`.

### `(j *JobConfig) HasRunner() bool`

**Signature:** `func (j *JobConfig) HasRunner() bool`

Returns true if the job's `RunsOn` field is non-empty (i.e., a runner type has been specified).

**Callers:** `PipelineConfig.Validate()` in `config.go:42` (called but result is discarded with `_ = name`).

### `(c *PipelineConfig) ValidateDAG() error`

**Signature:** `func (c *PipelineConfig) ValidateDAG() error`

Validates the job dependency graph:
1. Checks that every job referenced in `Needs` exists in the config.
2. Calls `detectCycle()` to check for circular dependencies.

**Callers:** `PipelineConfig.Validate()` in `config.go:46`.

### `(c *PipelineConfig) detectCycle() error`

**Signature:** `func (c *PipelineConfig) detectCycle() error`

Performs a DFS-based cycle detection using a white/gray/black three-color marking scheme. Iterates all jobs as DFS roots, tracking visited states. Returns an error with the cycle path if a cycle is detected (when a gray node is encountered during traversal).

**Callers:** `ValidateDAG()` in `config.go:71`.

### `(c *PipelineConfig) MatchEvent(event WebhookEvent) bool`

**Signature:** `func (c *PipelineConfig) MatchEvent(event WebhookEvent) bool`

Determines whether a pipeline should be triggered by a given webhook event. Logic:
- **Push events:** checks `c.On.Push` is non-nil and branch filter matches.
- **Pull Request events:** checks `c.On.PullRequest` is non-nil and branch filter matches.
- **Tag events:** iterates `c.On.Tags` and uses `matchTag()` for glob matching.
- **Manual events:** always returns true.
- **SourceDir filtering:** if `SourceDir` is set and not `"."`, checks that at least one changed file is affected by the source directory path.

**Callers:** `WebhookHandler.processEvent()` in `webhook.go:160` (iterates all registered configs to find matching pipelines).

### `(f *BranchFilter) matchesBranch(branch string) bool`

**Signature:** `func (f *BranchFilter) matchesBranch(branch string) bool`

Checks if a branch name matches the branch filter. If no `Branches` patterns are configured, returns true (match all). Otherwise iterates patterns and uses `matchGlob()`.

**Callers:** `PipelineConfig.MatchEvent()` in `config.go:121,129`.

### `matchGlob(pattern, value string) bool`

**Signature:** `func matchGlob(pattern, value string) bool`

A simple glob-style pattern matcher. Supports `*` as a wildcard (non-greedy). Splits the pattern on `*` and checks sequential prefix matching. If `pattern == "*"`, always returns true. If no `*` in pattern, does exact string comparison.

**Callers:**
- `BranchFilter.matchesBranch()` in `config.go:173`.
- `matchTag()` in `config.go:206`.

### `matchTag(pattern, tag string) bool`

**Signature:** `func matchTag(pattern, tag string) bool`

A thin wrapper around `matchGlob()` for tag pattern matching. Delegates directly to `matchGlob`.

**Callers:** `PipelineConfig.MatchEvent()` in `config.go:138`.

### `(c *PipelineConfig) EntrypointJobs() []string`

**Signature:** `func (c *PipelineConfig) EntrypointJobs() []string`

Returns the names of jobs that have no dependencies (`len(job.Needs) == 0`). These are the starting points in the pipeline DAG.

**Callers:** `Orchestrator.executeRun()` in `orchestrator.go:119`.

### `(c *PipelineConfig) DownstreamJobs(jobName string) []string`

**Signature:** `func (c *PipelineConfig) DownstreamJobs(jobName string) []string`

Returns the names of jobs that depend on `jobName` (i.e., jobs whose `Needs` list includes `jobName`).

**Callers:** Currently not directly called by any code in the scanned files (may be used elsewhere for pipeline visualization or manual advancement).

### `(c *PipelineConfig) AllNeedsSatisfied(jobName string, completed map[string]bool) bool`

**Signature:** `func (c *PipelineConfig) AllNeedsSatisfied(jobName string, completed map[string]bool) bool`

Checks whether all dependencies of a job have been completed. Returns false if the job doesn't exist or if any dependency is not yet completed.

**Callers:** `Orchestrator.advancePipeline()` in `orchestrator.go:284`.

### `LoadPackageMeta(ctx context.Context, path string) (*PackageMeta, error)`

**Signature:** `func LoadPackageMeta(ctx context.Context, path string) (*PackageMeta, error)`

Reads and unmarshals a YAML package metadata file (e.g., `monofs-packages.yaml`) from disk. The `ctx` parameter is accepted but not used.

**Callers:** `WebhookHandler.processEvent()` in `webhook.go:149`.

---

## pipeline/discover.go

Package: `pipeline`

### `DiscoverPipelines(rootPath string) ([]*PipelineConfig, error)`

**Signature:** `func DiscoverPipelines(rootPath string) ([]*PipelineConfig, error)`

Recursively walks `rootPath` to discover pipeline configurations. For each directory:
1. Skips directories named `.monofs` or `pipelines` (prevents infinite recursion into the pipelines dir itself).
2. Skips hidden directories (prefix `.`) except `.monofs`.
3. Looks for `<dir>/.monofs/pipelines/` subdirectory.
4. Loads each `.yml` file found there via `LoadConfig`.
5. Sets `SourceDir` on the config to the relative directory path.
6. If `SourceDir` is non-trivial, prefixes the pipeline name with the source directory path.

**Parameters:**
- `rootPath` — the root path to walk for pipeline discovery.

**Callers:** Expected to be called during router initialization to scan the working directory for local pipeline configs.

---

## pipeline/orchestrator.go

Package: `pipeline`

### `NewOrchestrator(queue *TaskQueue, logger *slog.Logger) *Orchestrator`

**Signature:** `func NewOrchestrator(queue *TaskQueue, logger *slog.Logger) *Orchestrator`

Constructs a new `Orchestrator` with empty run/configuration maps, the provided task queue, a derived logger, and `maxHistory` set to 100.

**Callers:** `Router.initPipeline()` in `pipeline_router.go:25`.

### `(o *Orchestrator) RegisterPipeline(cfg *PipelineConfig)`

**Signature:** `func (o *Orchestrator) RegisterPipeline(cfg *PipelineConfig)`

Registers a pipeline config under its `Name`. Uses write lock. Replaces any existing config with the same name.

**Callers:**
- `Router.loadPipelinesFromKVS()` in `pipeline_router.go:73`.
- `Router.watchPipelinesFromKVS()` in `pipeline_router.go:114`.

### `(o *Orchestrator) UnregisterPipeline(name string)`

**Signature:** `func (o *Orchestrator) UnregisterPipeline(name string)`

Removes a pipeline config by name from the orchestrator's config map.

**Callers:**
- `Router.watchPipelinesFromKVS()` in `pipeline_router.go:123`.
- `Router.handlePipelineUnregister()` in `pipeline_ui_handler.go:191`.

### `(o *Orchestrator) ListPipelines() []*PipelineConfig`

**Signature:** `func (o *Orchestrator) ListPipelines() []*PipelineConfig`

Returns a slice of all registered `PipelineConfig` pointers. Uses read lock.

**Callers:** Currently not directly called by any code in the scanned files.

### `(o *Orchestrator) StartRun(cfg *PipelineConfig, event WebhookEvent, affected []string) (*PipelineRun, error)`

**Signature:** `func (o *Orchestrator) StartRun(cfg *PipelineConfig, event WebhookEvent, affected []string) (*PipelineRun, error)`

Starts a new pipeline run:
1. If `Concurrency.CancelInProgress` is set, cancels any existing in-progress runs for the same pipeline/group.
2. Creates a `PipelineRun` with a new UUID, sets state to `RunPending`, records trigger metadata.
3. Creates `JobStatus` entries for every job in the config (each starts as `JobPending` with max 2 retries).
4. Adds to the runs map and history.
5. Launches `executeRun` in a goroutine.
6. Returns the run immediately (does not block on execution).

**Parameters:**
- `cfg` — the pipeline config to run.
- `event` — the triggering webhook event.
- `affected` — list of affected package names (used for conditional job execution).

**Callers:**
- `WebhookHandler.processEvent()` in `webhook.go:161`.
- `Router.handlePipelineRunTrigger()` in `pipeline_ui_handler.go:140`.

### `(o *Orchestrator) cancelExistingRuns(pipelineName, group string)`

**Signature:** `func (o *Orchestrator) cancelExistingRuns(pipelineName, group string)`

Cancels all existing runs for the same pipeline name that are in `RunRunning` state. Called within the write-locked region of `StartRun`.

**Callers:** `StartRun()` in `orchestrator.go:61`.

### `(o *Orchestrator) executeRun(run *PipelineRun, cfg *PipelineConfig, event WebhookEvent, affected []string)`

**Signature:** `func (o *Orchestrator) executeRun(run *PipelineRun, cfg *PipelineConfig, event WebhookEvent, affected []string)`

Executes a pipeline run:
1. Sets run state to `RunRunning` and records `StartedAt`.
2. Gets entrypoint jobs from the config.
3. For each entrypoint, checks `canRunJob` — if true, enqueues tasks and sets job to `JobRunning`; otherwise sets to `JobSkipped`.
4. If no jobs were started, immediately marks the run as `RunSucceeded`.

**Callers:** Launched as a goroutine from `StartRun()`.

### `(o *Orchestrator) canRunJob(cfg *PipelineConfig, run *PipelineRun, jobName string, event WebhookEvent, affected []string) bool`

**Signature:** `func (o *Orchestrator) canRunJob(cfg *PipelineConfig, run *PipelineRun, jobName string, event WebhookEvent, affected []string) bool`

Determines if a job should run by checking its `If` condition against the event and affected packages. Returns false if the job doesn't exist in the config.

**Callers:** `executeRun()` in `orchestrator.go:122`.

### `(o *Orchestrator) evaluateCondition(condition string, run *PipelineRun, affected []string) bool`

**Signature:** `func (o *Orchestrator) evaluateCondition(condition string, run *PipelineRun, affected []string) bool`

Evaluates a job's `If` condition string. Supports:
- `"always()"` / `""` → true
- `"failure()"` → true if run state is failed
- `"success()"` → true if run state is not failed
- `"cancelled()"` → true if run state is cancelled
- `"affected != ''"` → true if there are affected packages
- Default: true

**Callers:** `canRunJob()` in `orchestrator.go:144`.

### `(o *Orchestrator) enqueueJobTasks(run *PipelineRun, cfg *PipelineConfig, jobName string, affected []string)`

**Signature:** `func (o *Orchestrator) enqueueJobTasks(run *PipelineRun, cfg *PipelineConfig, jobName string, affected []string)`

Enqueues task(s) for a job. If the job has a `Strategy.Matrix`, expands the matrix combinations via `expandMatrix` and enqueues one task per combination; otherwise enqueues a single task. Logs errors from `EnqueueTask`.

**Callers:**
- `executeRun()` in `orchestrator.go:123`.
- `advancePipeline()` in `orchestrator.go:285`.

### `(o *Orchestrator) buildTask(runID, jobName string, job JobConfig, matrixVars map[string]string) *Task`

**Signature:** `func (o *Orchestrator) buildTask(runID, jobName string, job JobConfig, matrixVars map[string]string) *Task`

Constructs a `Task` object:
1. Copies job steps.
2. Substitutes matrix variables (`${{ matrix.KEY }}`) in each step's `Run` field.
3. Sets `TimeoutSec` from `job.TimeoutMinutes * 60`, defaulting to 600 seconds.
4. Sets `MaxRetries` to 2.
5. Assigns the `RunnerType` from the job config.

**Callers:** `enqueueJobTasks()` in `orchestrator.go:179,185`.

### `(o *Orchestrator) OnTaskResult(ctx context.Context, result *TaskResult)`

**Signature:** `func (o *Orchestrator) OnTaskResult(ctx context.Context, result *TaskResult)`

Handles a task result (called when a worker completes or fails a task):
- **Success:** Sets job to `JobSucceeded`, records `FinishedAt`, calls `advancePipeline`.
- **Failure:** Increments retry count. If under `MaxRetries`, re-enqueues the task with `RunnerBuilder`; if exhausted, marks job as failed, sets run to `RunFailed`, records `FinishedAt`.

The `ctx` parameter is accepted but not used.

**Callers:** Expected to be called by the task worker infrastructure when tasks complete.

### `(o *Orchestrator) advancePipeline(ctx context.Context, run *PipelineRun)`

**Signature:** `func (o *Orchestrator) advancePipeline(ctx context.Context, run *PipelineRun)`

Advances the pipeline after a job completes:
1. Finds the pipeline config by name.
2. Builds a `completed` map of all succeeded/skipped/failed jobs.
3. For each pending job, if all its dependencies are satisfied, enqueues its tasks and sets it to `JobRunning`.
4. If all jobs are done, sets the run final state (failed if any job failed, succeeded otherwise) and records `FinishedAt`.

The `ctx` parameter is accepted but not used.

**Callers:** `OnTaskResult()` in `orchestrator.go:235`.

### `(o *Orchestrator) CancelRun(runID string) error`

**Signature:** `func (o *Orchestrator) CancelRun(runID string) error`

Cancels a pipeline run by ID. Returns an error if the run is not found or has already finished. Otherwise calls `cancelRunLocked`.

**Callers:** `Router.handlePipelineRunCancel()` in `pipeline_ui_handler.go:111`.

### `(o *Orchestrator) cancelRunLocked(run *PipelineRun)`

**Signature:** `func (o *Orchestrator) cancelRunLocked(run *PipelineRun)`

Sets the run state to `RunCancelled`, records `FinishedAt`, and cancels all pending/claimed/running jobs by setting them to `JobCancelled`. Must be called while holding the write lock.

**Callers:**
- `CancelRun()` in `orchestrator.go:315`.
- `cancelExistingRuns()` in `orchestrator.go:108`.

### `(o *Orchestrator) GetRun(runID string) (*PipelineRun, error)`

**Signature:** `func (o *Orchestrator) GetRun(runID string) (*PipelineRun, error)`

Retrieves a run by ID. Returns a deep copy with cloned `Jobs` map and `JobStatus` values to prevent concurrent mutation. Returns an error if the run is not found.

**Callers:** `Router.handlePipelineRunDetail()` in `pipeline_ui_handler.go:98`.

### `(o *Orchestrator) ListRuns(pipelineName string, limit int) []*PipelineRun`

**Signature:** `func (o *Orchestrator) ListRuns(pipelineName string, limit int) []*PipelineRun`

Lists recent runs from history, newest first. If `pipelineName` is empty, returns runs from all pipelines. Limits results to `limit`. Returns an empty slice (not nil) if no results match.

**Callers:**
- `Router.handlePipelineList()` in `pipeline_ui_handler.go:76,81`.
- `Router.handlePipelineRunsList()` in `pipeline_ui_handler.go:89`.
- `Router.buildPipelineListView()` in `ui_handler.go:764,769`.

### `(o *Orchestrator) GetStats(pipelineName string) PipelineStats`

**Signature:** `func (o *Orchestrator) GetStats(pipelineName string) PipelineStats`

Computes pipeline statistics from history:
- Counts total, succeeded, and failed runs.
- Calculates `SuccessRate` as percentage.
- Collects durations for completed runs and computes `AvgDurationMs`, `P50DurationMs`, `P95DurationMs` using a simple percentile function.

**Callers:**
- `Router.handlePipelineStats()` in `pipeline_ui_handler.go:155`.
- `Router.buildPipelineStatsView()` in `ui_handler.go:787`.

### `(o *Orchestrator) PipelineConfigs() map[string]*PipelineConfig`

**Signature:** `func (o *Orchestrator) PipelineConfigs() map[string]*PipelineConfig`

Returns a copy of the registered pipeline configs map. Uses read lock.

**Callers:**
- `Router.handlePipelineList()` in `pipeline_ui_handler.go:68`.
- `Router.handlePipelineRunTrigger()` in `pipeline_ui_handler.go:120`.
- `Router.buildPipelineListView()` in `ui_handler.go:756`.

### `(o *Orchestrator) setRunState(run *PipelineRun, state RunState)`

**Signature:** `func (o *Orchestrator) setRunState(run *PipelineRun, state RunState)`

Thread-safe wrapper that acquires the write lock and delegates to `setRunStateLocked`.

**Callers:** `executeRun()` in `orchestrator.go:114,132`.

### `(o *Orchestrator) setRunStateLocked(run *PipelineRun, state RunState)`

**Signature:** `func (o *Orchestrator) setRunStateLocked(run *PipelineRun, state RunState)`

Sets the run state. Must be called while holding the write lock.

**Callers:**
- `setRunState()` in `orchestrator.go:421`.
- `OnTaskResult()` in `orchestrator.go:255`.
- `advancePipeline()` in `orchestrator.go:293,295`.

### `(o *Orchestrator) setJobState(run *PipelineRun, jobName string, state JobState)`

**Signature:** `func (o *Orchestrator) setJobState(run *PipelineRun, jobName string, state JobState)`

Thread-safe wrapper that acquires the write lock and delegates to `setJobStateLocked`.

**Callers:** `executeRun()` in `orchestrator.go:124,127`.

### `(o *Orchestrator) setJobStateLocked(run *PipelineRun, jobName string, state JobState)`

**Signature:** `func (o *Orchestrator) setJobStateLocked(run *PipelineRun, jobName string, state JobState)`

Sets the state of a specific job within a run. Must be called while holding the write lock.

**Callers:**
- `setJobState()` in `orchestrator.go:431`.
- `advancePipeline()` in `orchestrator.go:286`.

### `(o *Orchestrator) anyJobFailed(run *PipelineRun) bool`

**Signature:** `func (o *Orchestrator) anyJobFailed(run *PipelineRun) bool`

Returns true if any job in the run has `JobFailed` state.

**Callers:** `advancePipeline()` in `orchestrator.go:292`.

### `(o *Orchestrator) addToHistory(run *PipelineRun)`

**Signature:** `func (o *Orchestrator) addToHistory(run *PipelineRun)`

Appends a run to the history slice. If history exceeds `maxHistory` (100), evicts the oldest entry. Must be called while holding the write lock.

**Callers:** `StartRun()` in `orchestrator.go:88`.

### `expandMatrix(m map[string][]string) []map[string]string`

**Signature:** `func expandMatrix(m map[string][]string) []map[string]string`

Expands a matrix strategy into all combinations. Converts the map into parallel key/value slices and recursively builds all permutations. Returns nil if the matrix is empty.

**Callers:** `enqueueJobTasks()` in `orchestrator.go:177`.

### `expandMatrixRecursive(result *[]map[string]string, keys []string, values [][]string, current map[string]string, depth int)`

**Signature:** `func expandMatrixRecursive(result *[]map[string]string, keys []string, values [][]string, current map[string]string, depth int)`

Recursive helper for `expandMatrix`. At each depth level, iterates all values for the current key and recurses. When `depth == len(keys)`, appends a copy of the current combination to the result.

**Callers:** `expandMatrix()` in `orchestrator.go:470`.

### `replaceVar(s, varName, value string) string`

**Signature:** `func replaceVar(s, varName, value string) string`

Substitutes `${{ varName }}` placeholders in a string with the given value. Constructs the placeholder pattern and delegates to `stringsReplace`.

**Callers:** `buildTask()` in `orchestrator.go:198`.

### `stringsReplace(s, old, new string) string`

**Signature:** `func stringsReplace(s, old, new string) string`

A custom string replacement function using manual iteration and `indexOf`. Replaces all occurrences of `old` with `new` in `s`.

**Callers:** `replaceVar()` in `orchestrator.go:491`.

### `indexOf(s, substr string) int`

**Signature:** `func indexOf(s, substr string) int`

A custom substring search returning the index of the first occurrence, or -1 if not found. Uses a simple sliding window scan.

**Callers:** `stringsReplace()` in `orchestrator.go:497`.

### `percentile(durations []int64, p int) int64`

**Signature:** `func percentile(durations []int64, p int) int64`

Computes the p-th percentile of a slice of durations using a naive O(n^2) bubble sort and index calculation. Returns 0 for empty input. Clamps the index to the last element if the computed index exceeds the array bounds.

**Callers:** `GetStats()` in `orchestrator.go:402-403`.

---

## pipeline/reporter.go

Package: `pipeline`

### `NewStatusReporter(githubToken, gitlabToken string) *StatusReporter`

**Signature:** `func NewStatusReporter(githubToken, gitlabToken string) *StatusReporter`

Constructs a `StatusReporter` with GitHub and GitLab API tokens.

### `(r *StatusReporter) ReportGitHub(repoFullName, sha string, status CommitStatus) error`

**Signature:** `func (r *StatusReporter) ReportGitHub(repoFullName, sha string, status CommitStatus) error`

Posts a commit status to the GitHub API (`POST /repos/{owner}/{repo}/statuses/{sha}`). Returns nil silently if `githubToken` is empty. Sets `Authorization: token {token}`, `Accept: application/vnd.github.v3+json`. Returns an error if the API returns status >= 300.

### `(r *StatusReporter) ReportGitLab(projectID, sha string, status CommitStatus) error`

**Signature:** `func (r *StatusReporter) ReportGitLab(projectID, sha string, status CommitStatus) error`

Posts a commit status to the GitLab API (`POST /api/v4/projects/{id}/statuses/{sha}`). Returns nil silently if `gitlabToken` is empty. Uses `PRIVATE-TOKEN` header for authentication. Returns an error if the API returns status >= 300.

### `JobStateToCommitState(state JobState) string`

**Signature:** `func JobStateToCommitState(state JobState) string`

Converts a `JobState` to a commit status string:
- Pending/Claimed/Running → `"pending"`
- Succeeded → `"success"`
- Failed/Cancelled → `"failure"`
- Default → `"error"`

### `RunStateToCommitState(state RunState) string`

**Signature:** `func RunStateToCommitState(state RunState) string`

Converts a `RunState` to a commit status string:
- Pending/Running → `"pending"`
- Succeeded → `"success"`
- Failed/Cancelled → `"failure"`
- Default → `"error"`

---

## pipeline/taskqueue.go

Package: `pipeline`

### Function Type Aliases

```go
type KVSWriteFunc func(logicalPath string, content []byte, expectedVersionID string) (versionID string, _ error)
type KVSReadFunc func(logicalPath string) (content []byte, versionID string, _ error)
type KVSDeleteFunc func(logicalPath string) error
type KVSListFunc func(logicalDir string) ([]string, error)
```

These are function type aliases (not real functions) used as signatures for the KVS interface operations.

### `KVSClient` Interface

```go
type KVSClient interface {
    Write(logicalPath string, content []byte, expectedVersionID string) (versionID string, _ error)
    Read(logicalPath string) (content []byte, versionID string, _ error)
    Delete(logicalPath string) error
    List(logicalDir string) ([]string, error)
}
```

Defines the storage abstraction used by `TaskQueue` for persistent task/claim/result storage. Implemented by `routerKVSClient` in `pipeline_router.go`.

### `NewTaskQueue(kvs KVSClient) *TaskQueue`

**Signature:** `func NewTaskQueue(kvs KVSClient) *TaskQueue`

Constructs a task queue using the provided KVS client and the `runQueuePrefix` path function.

**Callers:** `Router.initPipeline()` in `pipeline_router.go:24`.

### `runQueuePrefix(runID string) string`

**Signature:** `func runQueuePrefix(runID string) string`

Returns the queue prefix path for a run: `/.queues/pipeline/{runID}`. Uses the constant `QueuePrefix` from `types.go`.

**Callers:** Used as the `runPrefix` field of `TaskQueue`.

### `(q *TaskQueue) tasksDir(runID string) string`

**Signature:** `func (q *TaskQueue) tasksDir(runID string) string`

Returns the tasks directory path: `{runPrefix}/tasks`.

### `(q *TaskQueue) claimsDir(runID string) string`

**Signature:** `func (q *TaskQueue) claimsDir(runID string) string`

Returns the claims directory path: `{runPrefix}/.claims`.

### `(q *TaskQueue) resultsDir(runID string) string`

**Signature:** `func (q *TaskQueue) resultsDir(runID string) string`

Returns the results directory path: `{runPrefix}/.results`.

### `(q *TaskQueue) taskPath(runID, taskID string) string`

**Signature:** `func (q *TaskQueue) taskPath(runID, taskID string) string`

Returns the logical path for a task JSON file: `{tasksDir}/{taskID}.json`.

### `(q *TaskQueue) claimPath(runID, taskID string) string`

**Signature:** `func (q *TaskQueue) claimPath(runID, taskID string) string`

Returns the logical path for a claim JSON file: `{claimsDir}/{taskID}.json`.

### `(q *TaskQueue) resultPath(runID, taskID string) string`

**Signature:** `func (q *TaskQueue) resultPath(runID, taskID string) string`

Returns the logical path for a result JSON file: `{resultsDir}/{taskID}.json`.

### `(q *TaskQueue) EnqueueTask(runID string, task *Task) error`

**Signature:** `func (q *TaskQueue) EnqueueTask(runID string, task *Task) error`

Enqueues a single task: assigns a UUID `TaskID`, sets `RunID`, `CreatedAt`, defaults `MaxRetries` to 2 and `TimeoutSec` to 600, marshals to JSON, and writes to KVS with empty expected version ID (create-semantics).

**Callers:**
- `Orchestrator.enqueueJobTasks()` in `orchestrator.go:180,186`.
- `Orchestrator.OnTaskResult()` in `orchestrator.go:242`.

### `(q *TaskQueue) EnqueueTasks(runID string, tasks []*Task) error`

**Signature:** `func (q *TaskQueue) EnqueueTasks(runID string, tasks []*Task) error`

Batch enqueues multiple tasks sequentially. Stops and returns the first error encountered.

### `(q *TaskQueue) ClaimTask(runID, workerID string) (*Task, error)`

**Signature:** `func (q *TaskQueue) ClaimTask(runID, workerID string) (*Task, error)`

Implements a distributed task claim pattern:
1. Lists all tasks in the run's tasks directory.
2. For each task, skips if already claimed or finished.
3. Attempts to write a claim file (with empty expected version ID) — if the write fails due to "already exists" or "FailedPrecondition", assumes another worker claimed it first and continues.
4. If claim succeeds, reads and returns the task data.
5. Returns nil, nil if no unclaimed tasks are available.

The claim file acts as a distributed mutex using KVS's optimistic concurrency.

### `(q *TaskQueue) isTaskClaimed(runID, taskID string) (bool, error)`

**Signature:** `func (q *TaskQueue) isTaskClaimed(runID, taskID string) (bool, error)`

Checks if a claim file exists for a task by attempting a KVS read. Returns true if the file exists (read succeeds), false if not found.

**Callers:** `ClaimTask()` in `taskqueue.go:105`.

### `(q *TaskQueue) isTaskFinished(runID, taskID string) (bool, error)`

**Signature:** `func (q *TaskQueue) isTaskFinished(runID, taskID string) (bool, error)`

Checks if a result file exists for a task by attempting a KVS read. Returns true if the file exists, false if not found.

**Callers:** `ClaimTask()` in `taskqueue.go:108`.

### `(q *TaskQueue) WriteResult(runID string, result *TaskResult) error`

**Signature:** `func (q *TaskQueue) WriteResult(runID string, result *TaskResult) error`

Marshals a `TaskResult` to JSON and writes it to the results directory. Uses empty expected version ID (create-semantics).

### `(q *TaskQueue) GetTask(runID, taskID string) (*Task, error)`

**Signature:** `func (q *TaskQueue) GetTask(runID, taskID string) (*Task, error)`

Reads and unmarshals a task from KVS by run ID and task ID.

**Callers:** `ListRunTasks()` in `taskqueue.go:212`.

### `(q *TaskQueue) GetResult(runID, taskID string) (*TaskResult, error)`

**Signature:** `func (q *TaskQueue) GetResult(runID, taskID string) (*TaskResult, error)`

Reads and unmarshals a task result from KVS by run ID and task ID.

**Callers:** `ListRunResults()` in `taskqueue.go:232`.

### `(q *TaskQueue) ListRunTasks(runID string) ([]*Task, error)`

**Signature:** `func (q *TaskQueue) ListRunTasks(runID string) ([]*Task, error)`

Lists all tasks for a run. Reads the tasks directory, filters hidden entries (prefix `.`), then reads and unmarshals each task file. Skips tasks that fail to read with a continue.

### `(q *TaskQueue) ListRunResults(runID string) ([]*TaskResult, error)`

**Signature:** `func (q *TaskQueue) ListRunResults(runID string) ([]*TaskResult, error)`

Lists all results for a run. Same pattern as `ListRunTasks` but for the results directory.

### `(q *TaskQueue) CleanupRun(runID string) error`

**Signature:** `func (q *TaskQueue) CleanupRun(runID string) error`

Deletes all tasks, claims, and results for a run. Iterates each directory and deletes each entry. Errors during deletion or listing are silently ignored (continue).

---

## pipeline/types.go

Package: `pipeline`

This file contains only type definitions and constants — no functions.

Key types: `PipelineConfig`, `TriggerConfig`, `BranchFilter`, `ConcurrencyConfig`, `JobConfig`, `StrategyConfig`, `StepConfig`, `PipelineRun`, `JobStatus`, `Task`, `TaskResult`, `TaskClaim`, `PackageMeta`, `PackageInfo`, `WebhookEvent`, `PipelineStats`.

Key constants:
- `QueuePrefix = "/.queues/pipeline"`
- Run states: `RunPending`, `RunRunning`, `RunSucceeded`, `RunFailed`, `RunCancelled`
- Job states: `JobPending`, `JobClaimed`, `JobRunning`, `JobSucceeded`, `JobFailed`, `JobSkipped`, `JobCancelled`
- Trigger types: `TriggerPush`, `TriggerPullRequest`, `TriggerTag`, `TriggerManual`
- Runner types: `RunnerBuilder`, `RunnerDocker`, `RunnerDeploy`, `RunnerLambda`

---

## pipeline/webhook.go

Package: `pipeline`

### `NewWebhookHandler(orchestrator *Orchestrator, cfg WebhookConfig, metaPath string) *WebhookHandler`

**Signature:** `func NewWebhookHandler(orchestrator *Orchestrator, cfg WebhookConfig, metaPath string) *WebhookHandler`

Constructs a `WebhookHandler` with the given orchestrator, webhook configuration (GitHub/GitLab secrets), and path to the package metadata file (`monofs-packages.yaml`). Initializes an empty configs map.

**Callers:** `Router.initPipeline()` in `pipeline_router.go:28`.

### `(h *WebhookHandler) RegisterPipeline(cfg *PipelineConfig)`

**Signature:** `func (h *WebhookHandler) RegisterPipeline(cfg *PipelineConfig)`

Registers a pipeline config with the webhook handler so it can be matched against incoming webhook events.

**Callers:**
- `Router.loadPipelinesFromKVS()` in `pipeline_router.go:75.
- `Router.watchPipelinesFromKVS()` in `pipeline_router.go:116.

### `(h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)`

**Signature:** `func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)`

Implements `http.Handler`. Routes requests based on headers:
- `X-GitHub-Event` → `handleGitHub()`
- `X-Gitlab-Event` → `handleGitLab()`
- Neither → 400 "unknown webhook source"

**Callers:** `Router.handleGitHubWebhook()` and `Router.handleGitLabWebhook()` in `pipeline_ui_handler.go:173,182`.

### `(h *WebhookHandler) handleGitHub(w http.ResponseWriter, r *http.Request, eventType string)`

**Signature:** `func (h *WebhookHandler) handleGitHub(w http.ResponseWriter, r *http.Request, eventType string)`

Handles GitHub webhook events:
1. If GitHub secret is configured, verifies the `X-Hub-Signature-256` HMAC header.
2. Reads the request body.
3. Dispatches based on event type: "push" → `parseGitHubPush()`, "pull_request" → `parseGitHubPullRequest()`, "create" → `parseGitHubCreate()`.
4. If `CommitSHA` is empty, responds with 200 and message.
5. Launches `processEvent()` in a goroutine and returns "accepted".

### `(h *WebhookHandler) handleGitLab(w http.ResponseWriter, r *http.Request, eventType string)`

**Signature:** `func (h *WebhookHandler) handleGitLab(w http.ResponseWriter, r *http.Request, eventType string)`

Handles GitLab webhook events:
1. If GitLab secret is configured, verifies `X-Gitlab-Token` header.
2. Reads the request body.
3. Dispatches based on event type: "Push Hook" → `parseGitLabPush()`, "Merge Request Hook" → `parseGitLabMergeRequest()`, "Tag Push Hook" → `parseGitLabTagPush()`.
4. Same early-return and goroutine pattern as GitHub.

### `(h *WebhookHandler) processEvent(event WebhookEvent)`

**Signature:** `func (h *WebhookHandler) processEvent(event WebhookEvent)`

Core event processing logic – called from goroutines in `handleGitHub`/`handleGitLab`:
1. Loads package metadata from `monofs-packages.yaml`.
2. If no changed files in the event, attempts to compute them from `git diff HEAD~1`.
3. Calls `DetectAffectedPackages()` to find affected packages.
4. Iterates all registered pipeline configs.
5. For each config that matches the event (`MatchEvent`), calls `Orchestrator.StartRun()`.

### `(h *WebhookHandler) verifyGitHubSignature(sigHeader string, r *http.Request) bool`

**Signature:** `func (h *WebhookHandler) verifyGitHubSignature(sigHeader string, r *http.Request) bool`

Verifies the GitHub HMAC-SHA256 signature. Expects `sha256=<hex>` format. Decodes the hex signature, recomputes HMAC over the request body, and uses `hmac.Equal` for constant-time comparison. Note: reading the body here consumes it — callers must re-read or buffer.

**Callers:** `handleGitHub()` in `webhook.go:62`.

### `(h *WebhookHandler) verifyGitLabSignature(token string) bool`

**Signature:** `func (h *WebhookHandler) verifyGitLabSignature(token string) bool`

Simple string comparison of the GitLab token header against the configured secret.

### `(h *WebhookHandler) parseGitHubPush(body []byte) WebhookEvent`

**Signature:** `func (h *WebhookHandler) parseGitHubPush(body []byte) WebhookEvent`

Parses a GitHub push event JSON payload. Extracts `ref` (strips `refs/heads/` prefix), `after` (commit SHA), sender login, repository URL. Collects all added/removed/modified files from all commits and deduplicates them. Returns a `WebhookEvent` (type will be set by caller).

### `(h *WebhookHandler) parseGitHubPullRequest(body []byte) WebhookEvent`

**Signature:** `func (h *WebhookHandler) parseGitHubPullRequest(body []byte) WebhookEvent`

Parses a GitHub pull_request event. Only processes if action is "opened", "synchronize", or "reopened". Extracts PR number, head SHA, head branch. Calls `ComputeChangedFiles("HEAD~1", "HEAD")` to detect changes.

### `(h *WebhookHandler) parseGitHubCreate(body []byte) WebhookEvent`

**Signature:** `func (h *WebhookHandler) parseGitHubCreate(body []byte) WebhookEvent`

Parses a GitHub create event. Only processes if `ref_type == "tag"`. Extracts the tag name, sender, and repository URL. Returns empty `CommitSHA`.

### `(h *WebhookHandler) parseGitLabPush(body []byte) WebhookEvent`

**Signature:** `func (h *WebhookHandler) parseGitLabPush(body []byte) WebhookEvent`

Parses a GitLab push event JSON payload. Similar to GitHub push: extracts branch from `ref`, `checkout_sha`, user, project URL. Collects and deduplicates changed files from all commits.

### `(h *WebhookHandler) parseGitLabMergeRequest(body []byte) WebhookEvent`

**Signature:** `func (h *WebhookHandler) parseGitLabMergeRequest(body []byte) WebhookEvent`

Parses a GitLab merge request event. Only processes if action is "open", "update", or "reopen". Extracts MR IID, last commit SHA, source branch.

### `(h *WebhookHandler) parseGitLabTagPush(body []byte) WebhookEvent`

**Signature:** `func (h *WebhookHandler) parseGitLabTagPush(body []byte) WebhookEvent`

Parses a GitLab tag push event. Extracts tag name by stripping `refs/tags/` prefix, user, and project URL.

### `dedupeStrings(s []string) []string`

**Signature:** `func dedupeStrings(s []string) []string`

Removes duplicate strings from a slice while preserving insertion order. Uses a `map[string]bool` for seen tracking.

**Callers:** `parseGitHubPush()` in `webhook.go:223`, `parseGitLabPush()` in `webhook.go:334`.

---

## pipeline_kvs.go

Package: `router` (not `pipeline` — this file lives in the `router` package)

### `(r *Router) writePipelinePath(logicalPath string, content []byte, expectedVersionID string) (string, error)`

**Signature:** `func (r *Router) writePipelinePath(logicalPath string, content []byte, expectedVersionID string) (string, error)`

Writes pipeline data to the KVS via the Guardian protocol. Builds an `UpsertGuardianPathsRequest` with the pipeline internal token, a single write with the given logical path, content, and expected version ID. Calls `processGuardianUpsert` to execute. Returns the version ID from the response. Uses a 15-second context timeout.

**Callers:** `routerKVSClient.Write()` in `pipeline_router.go:138` (which implements `KVSClient.Write`).

### `(r *Router) readPipelinePath(logicalPath string) ([]byte, string, error)`

**Signature:** `func (r *Router) readPipelinePath(logicalPath string) ([]byte, string, error)`

Reads pipeline data from KVS:
1. Maps the logical path via `mapGuardianLogicalPath`.
2. Checks the guardian versions cache — if content is cached in memory, returns that directly.
3. If tombstoned or not found, returns an error.
4. Otherwise connects to a guardian node client and reads the file content from the storage node. Uses a 15-second context timeout.

Returns `(content, versionID, error)`.

**Callers:** `routerKVSClient.Read()` in `pipeline_router.go:142`.

### `(r *Router) deletePipelinePath(logicalPath string) error`

**Signature:** `func (r *Router) deletePipelinePath(logicalPath string) error`

Deletes a pipeline path from the Guardian KVS. Builds a `DeleteGuardianPathsRequest` with the pipeline token, an empty expected version ID, and the logical path. Calls `processGuardianDelete`. Uses a 15-second context timeout.

**Callers:** `routerKVSClient.Delete()` in `pipeline_router.go:146`.

### `(r *Router) listPipelinePath(logicalDir string) ([]string, error)`

**Signature:** `func (r *Router) listPipelinePath(logicalDir string) ([]string, error)`

Lists entries in a pipeline KVS directory. Strips trailing slashes, ensures leading slash, then queries `guardianVersions.list` with a 1000-entry limit. Filters out tombstoned and nil version entries. Returns relative names (trimming the prefix).

**Callers:** `routerKVSClient.List()` in `pipeline_router.go:149`.

### `(r *Router) guardianNodeClientForPath(mapped guardianPhysicalPath) (pb.MonoFSClient, func(), error)`

**Signature:** `func (r *Router) guardianNodeClientForPath(mapped guardianPhysicalPath) (pb.MonoFSClient, func(), error)`

Gets a gRPC client for a guardian storage node. Collects healthy guardian nodes and returns a client for the first one. Returns the client, a close function, and an error if no healthy nodes are available.

**Callers:** `readPipelinePath()` in `pipeline_kvs.go:51`.

### `(r *Router) processGuardianDelete(ctx context.Context, req *pb.DeleteGuardianPathsRequest) (*pb.DeleteGuardianPathsResponse, error)`

**Signature:** `func (r *Router) processGuardianDelete(ctx context.Context, req *pb.DeleteGuardianPathsRequest) (*pb.DeleteGuardianPathsResponse, error)`

Processes a guardian path deletion:
1. Authenticates using the guardian token.
2. Validates at least one delete is requested.
3. Collects healthy guardian nodes.
4. For each delete, maps the logical path, checks if the current version exists and isn't tombstoned, connects to a node client and calls `DeleteFile`, records a tombstone version commit, publishes change events.
5. Returns a response with success status and count.

**Callers:** `deletePipelinePath()` in `pipeline_kvs.go:81`.

---

## pipeline_router.go

Package: `router`

### `(r *Router) initPipeline(logger *slog.Logger)`

**Signature:** `func (r *Router) initPipeline(logger *slog.Logger)`

Initializes the pipeline orchestrator subsystem:
1. Registers a guardian principal with ID `"monofs-pipeline"` for internal authentication.
2. Creates a `routerKVSClient` wrapping the router for pipeline task queue storage in the Guardian KVS.
3. Creates a `TaskQueue` using the KVS client.
4. Creates an `Orchestrator` with a derived logger.
5. Creates a `WebhookHandler` with empty `WebhookConfig` and default meta path `"monofs-packages.yaml"`.
6. Calls `loadPipelinesFromKVS()` to load existing configs.
7. Starts a `watchPipelinesFromKVS()` goroutine for change events.

**Callers:** Expected to be called during router startup/setup.

### `(r *Router) loadPipelinesFromKVS(logger *slog.Logger) error`

**Signature:** `func (r *Router) loadPipelinesFromKVS(logger *slog.Logger) error`

Loads pipeline configs from the Guardian KVS under the `/.pipelines` prefix. Lists all versions, filters out tombstoned entries, reads each config content from the versions cache, parses it via `pipeline.ParseConfig`, and registers it with both the orchestrator and webhook handler. Logs and skips invalid configs.

### `(r *Router) watchPipelinesFromKVS(logger *slog.Logger)`

**Signature:** `func (r *Router) watchPipelinesFromKVS(logger *slog.Logger)`

A long-running goroutine that subscribes to guardian logical change events on the `/.pipelines` prefix with inline content. Handles:
- **ADDED/MODIFIED:** Parses the inline content as a pipeline config and registers it.
- **DELETED:** Checks if the current version is tombstoned, then unregisters the pipeline from the orchestrator.

Exits when the `stopUI` channel is closed.

### `(c *routerKVSClient) Write(logicalPath string, content []byte, expectedVersionID string) (string, error)`

**Signature:** `func (c *routerKVSClient) Write(logicalPath string, content []byte, expectedVersionID string) (string, error)`

Implements `KVSClient.Write`. Delegates to `Router.writePipelinePath()`.

### `(c *routerKVSClient) Read(logicalPath string) ([]byte, string, error)`

**Signature:** `func (c *routerKVSClient) Read(logicalPath string) ([]byte, string, error)`

Implements `KVSClient.Read`. Delegates to `Router.readPipelinePath()`.

### `(c *routerKVSClient) Delete(logicalPath string) error`

**Signature:** `func (c *routerKVSClient) Delete(logicalPath string) error`

Implements `KVSClient.Delete`. Delegates to `Router.deletePipelinePath()`.

### `(c *routerKVSClient) List(logicalDir string) ([]string, error)`

**Signature:** `func (c *routerKVSClient) List(logicalDir string) ([]string, error)`

Implements `KVSClient.List`. Delegates to `Router.listPipelinePath()`.

### `(r *Router) subscribeGuardianLogicalChanges(prefixes []string, includeInline bool) (<-chan *pb.GuardianChangeEvent, uint64)`

**Signature:** `func (r *Router) subscribeGuardianLogicalChanges(prefixes []string, includeInline bool) (<-chan *pb.GuardianChangeEvent, uint64)`

Subscribes to guardian logical change events. Creates a buffered channel (cap 128) and tracks it in `guardianLogicalChangeSubs`. Returns the channel and a subscription ID for later unsubscribe. Uses an atomic sequence for unique IDs.

**Callers:** `watchPipelinesFromKVS()` in `pipeline_router.go:86`.

### `(r *Router) unsubscribeGuardianLogicalChanges(id uint64)`

**Signature:** `func (r *Router) unsubscribeGuardianLogicalChanges(id uint64)`

Removes a subscription by ID, closing the event channel and deleting from the subscribers map.

**Callers:** `watchPipelinesFromKVS()` in `pipeline_router.go:87` (deferred).

---

## pipeline_ui_handler.go

Package: `router`

### `(r *Router) handlePipelinesAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handlePipelinesAPI(w http.ResponseWriter, req *http.Request)`

Central pipeline API routing handler registered at `/api/pipelines` and `/api/pipelines/`. Parses the URL path after `/api/pipelines/` and routes to:
- `""` (GET) → `handlePipelineList` (list all pipelines)
- `{name}/runs` (GET) → `handlePipelineRunsList`
- `{name}/runs/{runID}` (GET) → `handlePipelineRunDetail`
- `{name}/runs/{runID}/cancel` (POST) → `handlePipelineRunCancel`
- `{name}/run` (POST) → `handlePipelineRunTrigger`
- `{name}` (DELETE) → `handlePipelineUnregister`
- `stats` (GET) → `handlePipelineStats`

Returns 404 for unrecognized paths. If the orchestrator is nil, returns empty pipeline list.

**Callers:** Registered in `ServeHTTP()` at `ui.go:181-182`.

### `(r *Router) handlePipelineList(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handlePipelineList(w http.ResponseWriter, req *http.Request)`

Returns a JSON list of all registered pipelines (`PipelineListData`). For each pipeline, fetches the last run state and total run count. Each entry is a `PipelineSummary`.

### `(r *Router) handlePipelineRunsList(w http.ResponseWriter, req *http.Request, pipelineName string)`

**Signature:** `func (r *Router) handlePipelineRunsList(w http.ResponseWriter, req *http.Request, pipelineName string)`

Returns the last 20 runs for a specific pipeline as `PipelineRunsData`. Each run is converted via `pipelineRunToData()`.

### `(r *Router) handlePipelineRunDetail(w http.ResponseWriter, req *http.Request, runID string)`

**Signature:** `func (r *Router) handlePipelineRunDetail(w http.ResponseWriter, req *http.Request, runID string)`

Returns details for a specific run by `runID`. Calls `Orchestrator.GetRun()` and returns 404 if not found. Returns `PipelineRunData` via `pipelineRunToData()`.

### `(r *Router) handlePipelineRunCancel(w http.ResponseWriter, req *http.Request, runID string)`

**Signature:** `func (r *Router) handlePipelineRunCancel(w http.ResponseWriter, req *http.Request, runID string)`

Cancels a running pipeline. Only accepts POST. Calls `Orchestrator.CancelRun()`. Returns 400 if the run can't be cancelled (e.g., already finished). Returns `{"message": "cancelled"}` on success.

### `(r *Router) handlePipelineRunTrigger(w http.ResponseWriter, req *http.Request, pipelineName string)`

**Signature:** `func (r *Router) handlePipelineRunTrigger(w http.ResponseWriter, req *http.Request, pipelineName string)`

Manually triggers a pipeline run via POST. Parses optional `branch` (defaults to "main") and `commit` (defaults to "HEAD") from the JSON body. Looks up the pipeline config, creates a `TriggerManual` event, and calls `Orchestrator.StartRun()`. Returns 201 with the run data, or 404 if the pipeline is not found.

### `(r *Router) handlePipelineStats(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handlePipelineStats(w http.ResponseWriter, req *http.Request)`

Returns aggregate pipeline statistics across all pipelines via `Orchestrator.GetStats("")`. Returns `PipelineStatsData` with total/succeeded/failed runs, success rate, and duration percentiles.

### `(r *Router) handleGitHubWebhook(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleGitHubWebhook(w http.ResponseWriter, req *http.Request)`

Delegates to `pipelineWebhookHandler.ServeHTTP()` if the handler is non-nil. Otherwise returns a JSON message indicating webhooks are not configured.

**Callers:** Registered at `/api/webhooks/github` in `ui.go:184`.

### `(r *Router) handleGitLabWebhook(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleGitLabWebhook(w http.ResponseWriter, req *http.Request)`

Same pattern as `handleGitHubWebhook` but for GitLab events.

**Callers:** Registered at `/api/webhooks/gitlab` in `ui.go:185`.

### `(r *Router) handlePipelineUnregister(w http.ResponseWriter, req *http.Request, name string)`

**Signature:** `func (r *Router) handlePipelineUnregister(w http.ResponseWriter, req *http.Request, name string)`

Unregisters a pipeline by name via DELETE. Only accepts DELETE method. Calls `Orchestrator.UnregisterPipeline()`. Returns a JSON confirmation message.

### `pipelineRunToData(run *pipeline.PipelineRun) PipelineRunData`

**Signature:** `func pipelineRunToData(run *pipeline.PipelineRun) PipelineRunData`

Converts a `pipeline.PipelineRun` to the UI-facing `PipelineRunData` struct. Formats timestamps as RFC3339-like strings (`2006-01-02T15:04:05Z`). For each job, converts to `JobStatusData` and computes `DurationMs` if both start and finish times are set.

**Callers:**
- `handlePipelineRunsList()` in `pipeline_ui_handler.go:92.
- `handlePipelineRunDetail()` in `pipeline_ui_handler.go:103.
- `handlePipelineRunTrigger()` in `pipeline_ui_handler.go:151.

---

## internal/router/ui.go

Package: `router`

### `(s *mockIngestStream) Send(_ *pb.IngestProgress) error`

Implements `MonoFSRouter_IngestRepositoryServer.Send()`. Always returns nil (fire-and-forget progress).

### `(s *mockIngestStream) Context() context.Context`

Returns the mock stream's context.

### `(s *mockIngestStream) SendMsg(m interface{}) error`

No-op implementation.

### `(s *mockIngestStream) RecvMsg(m interface{}) error`

No-op implementation.

### `(s *mockIngestStream) SetHeader(metadata.MD) error`

No-op implementation.

### `(s *mockIngestStream) SendHeader(metadata.MD) error`

No-op implementation.

### `(s *mockIngestStream) SetTrailer(metadata.MD)`

No-op implementation.

### `(r *Router) injectGuardianPartitionFromSource(ctx context.Context, source, ref, partitionName, token string) error`

**Signature:** `func (r *Router) injectGuardianPartitionFromSource(ctx context.Context, source, ref, partitionName, token string) error`

Ingests a guardian partition from a git source. Creates a git ingestion backend, validates and initializes it, walks all files, collects them as `InjectGuardianFile` entries, and calls `InjectGuardianPartition` gRPC. Returns an error if source/partitionName is empty, if no files are produced, or if any step fails.

**Callers:** `handleGuardianInject()` in `ui.go:1721`.

### `(r *Router) ServeHTTP() http.Handler`

**Signature:** `func (r *Router) ServeHTTP() http.Handler`

Constructs and returns the main HTTP mux for the router. Registers all API routes (ingest, status, repositories, pipelines, search, guardian, registry, webhooks, health, metrics, pprof), static file serving from `static/*` and `ui/dist` (Vue SPA), and SPA fallback to `index.html` for non-asset paths.

### `(r *Router) handleSPA(dist fs.FS) http.HandlerFunc`

**Signature:** `func (r *Router) handleSPA(dist fs.FS) http.HandlerFunc`

Returns an HTTP handler that serves the Vue SPA. Tries to serve the requested file first; if not found, falls back to `index.html` with `ServeContent` for proper caching headers. Returns 503 if `index.html` is not available.

### `(r *Router) handleIngest(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleIngest(w http.ResponseWriter, req *http.Request)`

Handles POST `/api/ingest`. Enforces ingestion whitelist if enabled. Parses form values (`source`, `ref`, `source_id`, `ingestion_type`, `fetch_type`, `replicate_data`). Rejects guardian ingestion type (must use guardian API). Defaults ingestion type to `git`, fetch type to `git`. Starts async ingestion via a goroutine using `mockIngestStream`. Returns immediately with "in_progress" status.

### `(r *Router) handleIngestFileUpload(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleIngestFileUpload(w http.ResponseWriter, req *http.Request)`

Handles POST `/api/ingest/file`. Parses multipart form (max 500MB). Creates a temp directory, writes uploaded files preserving directory structure, counts files, and starts async ingestion of the temp directory. Cleans up temp dir after completion. Returns file count and "in_progress" status.

### `parseIngestionTypeString(s string) pb.IngestionType`

**Signature:** `func parseIngestionTypeString(s string) pb.IngestionType`

Converts a string to `IngestionType` enum: "git" → `INGESTION_GIT`, "s3" → `INGESTION_S3`, "file" → `INGESTION_FILE`, "guardian" → `INGESTION_GUARDIAN`. Defaults to `INGESTION_GIT`.

**Callers:** `handleIngest()` in `ui.go:325`.

### `parseFetchTypeString(s string) pb.SourceType`

**Signature:** `func parseFetchTypeString(s string) pb.SourceType`

Converts a string to `SourceType` enum: "git" → `SOURCE_TYPE_GIT`, "blob" → `SOURCE_TYPE_BLOB`. Defaults to `SOURCE_TYPE_BLOB`.

**Callers:** `handleIngest()` in `ui.go:326`.

### `(r *Router) handleStatus(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleStatus(w http.ResponseWriter, req *http.Request)`

Returns cluster status over the UI channel. Uses a 2-second cache to avoid repeated channel requests. Falls back to the channel-based request pattern with 5-second timeout. Returns 503 on timeout or channel error.

### `(r *Router) handleRepositoriesList(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleRepositoriesList(w http.ResponseWriter, req *http.Request)`

Returns the repository list. Uses a 3-second cache. Otherwise sends a `UIRequestRepositories` over the channel with 5-second timeout. Returns 503 on error.

### `(r *Router) handleRouters(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleRouters(w http.ResponseWriter, req *http.Request)`

Returns aggregated router status. Uses a 3-second cache. Otherwise sends `UIRequestRouters` over the channel with 8-second timeout. Returns 503 on error.

### `(r *Router) handleHealth(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request)`

Simple health check returning `{"status": "healthy", "service": "monofs-router"}` with 200.

### `(r *Router) handleRebalance(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleRebalance(w http.ResponseWriter, req *http.Request)`

Handles POST `/api/rebalance`. Requires `storage_id` form value. Checks that the repository exists and is not already rebalancing. Starts rebalancing asynchronously via `rebalanceRepository()`. Returns "rebalancing started" on success.

### `(r *Router) handleSearchAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleSearchAPI(w http.ResponseWriter, req *http.Request)`

Handles POST search requests. Parses JSON body with `query`, `storage_id`, `case_sensitive`, `regex`, `max_results`, `file_patterns`. Defaults max results to 100. Forwards to `searchClient.Search()` gRPC with 30-second timeout.

### `(r *Router) handleSearchIndexes(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleSearchIndexes(w http.ResponseWriter, req *http.Request)`

Returns all search indexes via `searchClient.ListIndexes()`. Returns empty index list if search service is unavailable.

### `(r *Router) handleSearchRebuild(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleSearchRebuild(w http.ResponseWriter, req *http.Request)`

Triggers index rebuild via POST. Accepts `storage_id`, `all`, `force` in JSON body. If `all` is true, calls `RebuildAllIndexes()` (120s timeout); otherwise calls `RebuildIndex()` for a specific storage ID (60s timeout).

### `(r *Router) handleSearchStats(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleSearchStats(w http.ResponseWriter, req *http.Request)`

Returns search service statistics via `searchClient.GetStats()`.

### `(r *Router) handleGuardianClientsAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleGuardianClientsAPI(w http.ResponseWriter, req *http.Request)`

Returns the list of connected guardian clients (local + peered). Iterates `guardianClients` map, marks clients as "stale" if heartbeat is older than 60 seconds. Also fetches guardian clients from peer routers and deduplicates them.

### `(r *Router) handleClientsAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleClientsAPI(w http.ResponseWriter, req *http.Request)`

Returns connected FUSE clients. Calls `ListClients()` gRPC, then fetches clients from peer routers and deduplicates by `client_id`.

### `(r *Router) handleLocalClientsAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleLocalClientsAPI(w http.ResponseWriter, req *http.Request)`

Returns only this router's FUSE clients (called by peer routers for aggregation).

### `(r *Router) handleGuardianLocalClientsAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleGuardianLocalClientsAPI(w http.ResponseWriter, req *http.Request)`

Returns only this router's guardian clients (called by peer routers for aggregation). Same staleness logic as `handleGuardianClientsAPI`.

### `(r *Router) handleFileContent(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleFileContent(w http.ResponseWriter, req *http.Request)`

Handles POST `/api/file/content`. Reads `storage_id` and `file_path` from JSON body. Locates the file node via `GetNodeForFile()` gRPC, then streams the file content using the node's `Read()` gRPC call. Limits content to 10MB. Returns `{"content": "..."}` JSON.

### `(r *Router) handleFetchersAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleFetchersAPI(w http.ResponseWriter, req *http.Request)`

Returns fetcher cluster statistics via `GetFetcherClusterStats()`. Unless `?detailed=true` is set, strips per-fetcher source stats to keep response small.

### `(r *Router) handleFetcherStorageObjects(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleFetcherStorageObjects(w http.ResponseWriter, req *http.Request)`

Aggregates storage object listings from all fetcher diagnostics endpoints. Uses configured `FetcherDiagnostics` addresses or discovers fetchers from the fetcher client with address offset +1. Returns a JSON response with per-fetcher entries and total object count.

### `(r *Router) queryFetcherStorageObjects(ctx context.Context, client *http.Client, baseURL, address string) fetcherStorageObjectEntry`

**Signature:** `func (r *Router) queryFetcherStorageObjects(ctx context.Context, client *http.Client, baseURL, address string) fetcherStorageObjectEntry`

Makes an HTTP GET to `{baseURL}/storage-objects` on a fetcher diagnostics endpoint and returns the parsed `fetcherStorageObjectEntry`.

### `(r *Router) handleFetcherKeyStatus(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleFetcherKeyStatus(w http.ResponseWriter, req *http.Request)`

Aggregates encryption key guard status from all fetcher diagnostics endpoints. Returns per-fetcher state (e.g., "pending", "ok", "no_key"), fingerprint info, and whether key confirmation is possible. Computes aggregate counts and `all_good` flag.

### `(r *Router) queryFetcherKeyStatus(ctx context.Context, client *http.Client, baseURL, address string) fetcherKeyStatusEntry`

**Signature:** `func (r *Router) queryFetcherKeyStatus(ctx context.Context, client *http.Client, baseURL, address string) fetcherKeyStatusEntry`

Makes an HTTP GET to `{baseURL}/key-status` on a fetcher diagnostics endpoint. Returns parsed key status entry. Marks as healthy if state is "ok", "auto_accepted", or "no_key".

### `(r *Router) handleConfirmFetcherKey(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleConfirmFetcherKey(w http.ResponseWriter, req *http.Request)`

Handles POST to confirm fetcher encryption keys. Forwards confirmation requests to all fetcher diagnostics endpoints (`/confirm-key`). Returns aggregated results with 207 Multi-Status if not all confirmed, 200 if all OK.

### `(r *Router) handleRegistryStats(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleRegistryStats(w http.ResponseWriter, req *http.Request)`

Proxies to monofs-registry `/api/v1/stats`. Returns zero-valued stats if registry address is not configured.

### `(r *Router) handleRegistryRepos(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleRegistryRepos(w http.ResponseWriter, req *http.Request)`

Proxies to monofs-registry `/api/v1/repos`. Returns empty repo list if registry address is not configured.

### `(r *Router) handleRegistryRepoDetail(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleRegistryRepoDetail(w http.ResponseWriter, req *http.Request)`

Proxies to monofs-registry `/api/v1/repos/{name}`. Returns empty tag list if registry is not configured.

### `(r *Router) handleLogEngineAPI(w http.ResponseWriter, _ *http.Request)`

**Signature:** `func (r *Router) handleLogEngineAPI(w http.ResponseWriter, _ *http.Request)`

Returns per-node doctor telemetry log engine statistics. Snapshots all nodes, collects `LogChunks`, `MetricChunks`, `TraceChunks` from each node's log engine state. Sorts results by node ID. Returns totals and enabled status.

### `(r *Router) handleDependenciesAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleDependenciesAPI(w http.ResponseWriter, req *http.Request)`

Returns dependency information via the UI request channel (`UIRequestDependencies`) with a 10-second timeout. Returns 503 on error.

### `(r *Router) handlePredictorAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handlePredictorAPI(w http.ResponseWriter, req *http.Request)`

Returns predictive prefetch stats from all healthy storage nodes. Queries each node's `GetPredictorStats()` gRPC in parallel goroutines. Aggregates markov chains, directory maps, predictions, prefetches, hit rates. Sorts results by node ID. Computes cluster-level totals and hit rate.

### `(r *Router) handleGuardianInject(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleGuardianInject(w http.ResponseWriter, req *http.Request)`

Handles POST `/api/guardian/inject`. Validates the `X-Guardian-Token` header, requires `source` and `partition_name` form values. Rejects partition names containing `/`. Starts async guardian partition ingestion via `injectGuardianPartitionFromSource()` in a goroutine. Returns "started" message immediately.

### `(r *Router) handleGuardianPartitions(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleGuardianPartitions(w http.ResponseWriter, req *http.Request)`

Handles GET `/api/guardian/partitions`. Lists all guardian partitions by building repository data and filtering for `is_guardian == true` entries.

### `(r *Router) handleGuardianPartition(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleGuardianPartition(w http.ResponseWriter, req *http.Request)`

Handles DELETE `/api/guardian/partitions/{name}[/files][/dirs]`. Requires guardian token authentication. Routes based on sub-path:
- `files` → deletes a specific file from all nodes via `deleteGuardianFileFromAllNodes()`.
- `dirs` → deletes a directory from all nodes via `deleteGuardianDirFromAllNodes()`.
- (empty) → deletes entire partition via `deleteRepositoryInternal()`.

### `(r *Router) handlePprofCollectAPI(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handlePprofCollectAPI(w http.ResponseWriter, req *http.Request)`

Handles POST `/api/pprof/collect`. Collects pprof profiles from all discovered targets (routers, servers, search, registry, fetchers). Accepts JSON body with `profiles` (default: `["cpu","heap","goroutine"]`) and `cpu_duration_seconds` (default: 30, max: 120). Creates a zip file with per-target per-profile `.pprof` files and a `manifest.json`. Returns the zip as a download with custom headers tracking success/failure counts.

### `(r *Router) collectPprofTargets(req *http.Request) []pprofTarget`

**Signature:** `func (r *Router) collectPprofTargets(req *http.Request) []pprofTarget`

Discovers all pprof collection targets:
1. Local router and peered routers.
2. Storage servers (explicit diagnostics config or gRPC address +100 offset).
3. Search service (explicit diagnostics config or gRPC address +1 offset).
4. Registry (explicit diagnostics config only).
5. Fetchers (explicit diagnostics config or gRPC address +1 offset).

Deduplicates by `serviceType|name|baseURL` key.

### `collectPprofArtifacts(parent context.Context, targets []pprofTarget, profiles []string, cpuSeconds int) (pprofCollectManifest, map[string][]byte)`

**Signature:** `func collectPprofArtifacts(parent context.Context, targets []pprofTarget, profiles []string, cpuSeconds int) (pprofCollectManifest, map[string][]byte)`

Collects pprof profile data from all targets in parallel goroutines. For each target and each profile, calls `fetchPprofProfile()`. Builds a manifest with per-target results and a file map keyed by path like `{serviceType}/{name}/{profile}.pprof`. Sorts results by service type then name.

### `fetchPprofProfile(parent context.Context, baseURL, profile string, cpuSeconds int) ([]byte, error)`

**Signature:** `func fetchPprofProfile(parent context.Context, baseURL, profile string, cpuSeconds int) ([]byte, error)`

Fetches a single pprof profile from a target's `/debug/pprof/{profile}` endpoint. For "cpu" profile, adds `?seconds=N` query parameter. Uses `profileTimeout()` for appropriate timeout per profile type. Returns error for non-200 status or empty response.

### `profileTimeout(profile string, cpuSeconds int) time.Duration`

**Signature:** `func profileTimeout(profile string, cpuSeconds int) time.Duration`

Returns context timeout for pprof profile fetching: CPU: `cpuSeconds+15` seconds, trace: 45 seconds, everything else: 20 seconds.

### `normalizePprofProfiles(input []string) []string`

**Signature:** `func normalizePprofProfiles(input []string) []string`

Validates and normalizes profile names. Only allows: cpu, heap, goroutine, allocs, mutex, block, threadcreate, trace. Lowercases, trims, deduplicates. Returns valid profiles in original order.

### `sanitizePathSegment(value string) string`

**Signature:** `func sanitizePathSegment(value string) string`

Sanitizes a string for use as a filesystem path segment: replaces `:`, `/`, `\`, `..` with `_`, cleans with `filepath.Clean`, returns "unknown" if empty or root.

### `pprofTargetServiceTypes(targets []pprofTarget) []string`

**Signature:** `func pprofTargetServiceTypes(targets []pprofTarget) []string`

Returns a sorted, de-duplicated list of service types from a slice of pprof targets.

### `routerBaseURLFromRequest(req *http.Request) string`

**Signature:** `func routerBaseURLFromRequest(req *http.Request) string`

Extracts the base URL from an HTTP request. Uses `X-Forwarded-Host` (taking first value if comma-separated) or `req.Host`. Uses `X-Forwarded-Proto` or `req.TLS` to determine scheme. Returns `{scheme}://{host}`.

### `addressWithOffset(address string, portOffset int) (string, error)`

**Signature:** `func addressWithOffset(address string, portOffset int) (string, error)`

Parses a `host:port` address, adds `portOffset` to the port number, and returns the new `host:port` string. Used to derive diagnostics port from gRPC port.

### `diagnosticsEndpoint(raw string) (baseURL string, address string)`

**Signature:** `func diagnosticsEndpoint(raw string) (baseURL string, address string)`

Parses a diagnostics address string. If it contains `://`, parses as URL and returns the string (stripped of trailing `/`) and the host. Otherwise prepends `http://` and returns both the URL and the raw trimmed string as address.

### `fetchPeerClients(peerURL string) []*pb.ClientInfo`

**Signature:** `func fetchPeerClients(peerURL string) []*pb.ClientInfo`

Fetches FUSE clients from a peer router's `/api/local-clients` endpoint with a 2-second timeout. Returns nil on any error (best-effort aggregation).

### `dedupeGuardianClients(clients []guardianClientJSON) []guardianClientJSON`

**Signature:** `func dedupeGuardianClients(clients []guardianClientJSON) []guardianClientJSON`

Deduplicates guardian clients by `ClientID`. Keeps the entry with the most recent heartbeat. Prefers "connected" state over "stale" when heartbeats are equal. Preserves discovery order.

### `fetchPeerGuardianClients(peerURL, peerName string) []guardianClientJSON`

**Signature:** `func fetchPeerGuardianClients(peerURL, peerName string) []guardianClientJSON`

Fetches guardian clients from a peer router's `/api/guardian/local-clients` endpoint. Tags each client with the peer's router name. Returns nil on any error.

### `(r *Router) handleFSList(w http.ResponseWriter, req *http.Request)`

**Signature:** `func (r *Router) handleFSList(w http.ResponseWriter, req *http.Request)`

Handles GET `/api/fs/ls?path=...`. Lists the local filesystem directory contents using `os.ReadDir`. Returns JSON with `path`, `entries` (name + is_dir), and optional error. Sorts entries with directories first, then alphabetically.

---

## internal/router/ui_handler.go

Package: `router`

### `(r *Router) handleUIRequests()`

**Signature:** `func (r *Router) handleUIRequests()`

Long-running goroutine that processes UI requests from the `uiRequests` channel. For each request, calls `processUIRequest()`. Exits when `stopUI` channel is closed.

**Callers:** Expected to be started as a goroutine during router initialization.

### `(r *Router) processUIRequest(req UIRequest)`

**Signature:** `func (r *Router) processUIRequest(req UIRequest)`

Dispatches a UI request by type:
- `UIRequestRepositories` → `buildRepositoriesData()`
- `UIRequestStatus` → `buildStatusData()`
- `UIRequestRouters` → `buildRoutersData()`
- `UIRequestDependencies` → `buildDependenciesData()`
- `UIRequestPipelines` → `buildPipelineListView()`
- `UIRequestPipelineRuns` → `buildPipelineRunsView()`
- `UIRequestPipelineRun` → `buildPipelineRunView()`
- `UIRequestPipelineStats` → `buildPipelineStatsView()`

Sends the result back on `req.Response`.

### `(r *Router) buildRepositoriesData() *RepositoriesData`

**Signature:** `func (r *Router) buildRepositoriesData() *RepositoriesData`

Builds the repository list UI payload. Snapshots nodes and in-progress ingestions under lock. For each healthy storage node, queries `ListRepositories()` and `GetRepositoryInfo()` gRPC in parallel (concurrency limited to 6). Also includes in-progress ingestions with progress info. Computes product links (guardian/doctor URLs). Returns `RepositoriesData` with topology version.

### `(r *Router) buildStatusData() *StatusData`

**Signature:** `func (r *Router) buildStatusData() *StatusData`

Builds the cluster status UI payload. Snapshots all nodes, collects health/KVS/file/disk metrics per node. Includes failover mappings, drain status, build version info, and feature flags (storage node WAL ledger, workspace job WAL, HRW routing, policy gate, auto push). Updates Prometheus gauge metrics. Returns `StatusData`.

### `(r *Router) buildRoutersData() *RoutersData`

**Signature:** `func (r *Router) buildRoutersData() *RoutersData`

Builds aggregated router status for the UI. Always includes the local router snapshot. For each peer router, fetches status and repositories in parallel (concurrency limited to 4, 1.5s timeout). Returns `RoutersData` with per-router snapshots and errors.

### `normalizeRouterURL(raw string) (string, error)`

**Signature:** `func normalizeRouterURL(raw string) (string, error)`

Normalizes a router URL: trims whitespace, adds `http://` if no scheme present, parses and validates host is non-empty, strips trailing `/`.

### `fetchRouterStatus(client *http.Client, baseURL string) (*StatusData, error)`

**Signature:** `func fetchRouterStatus(client *http.Client, baseURL string) (*StatusData, error)`

Fetches `{baseURL}/api/status` from a peer router with 1-second timeout. Returns parsed `StatusData` or error.

### `fetchRouterRepositories(client *http.Client, baseURL string) (*RepositoriesData, error)`

**Signature:** `func fetchRouterRepositories(client *http.Client, baseURL string) (*RepositoriesData, error)`

Fetches `{baseURL}/api/repositories` from a peer router with 1-second timeout. Returns parsed `RepositoriesData` or error.

### `(r *Router) sendUIRequest(reqType UIRequestType, timeout time.Duration) (interface{}, error)`

**Signature:** `func (r *Router) sendUIRequest(reqType UIRequestType, timeout time.Duration) (interface{}, error)`

Sends a request to the UI handler channel and waits for a response. Creates a response channel, sends the request to `uiRequests`, then waits for the response on the channel. Both send and receive operations are guarded by the same timeout. Returns `ErrUITimeout` on timeout.

**Callers:** `handleStatus()`, `handleRepositoriesList()`, `handleRouters()`, `handleDependenciesAPI()`.

### `(r *Router) buildDependenciesData() *DependenciesData`

**Signature:** `func (r *Router) buildDependenciesData() *DependenciesData`

Aggregates dependency information from the cluster. Looks up the "dependency" repository by deterministic storage ID (SHA256 of "dependency"). Queries each healthy node for files in that repo via `streamRepositoryFiles()`. Deduplicates files, groups by tool prefix (first path segment), computes per-node file counts, and sorts tools by file count descending.

### `(r *Router) buildPipelineListView() *PipelineListData`

**Signature:** `func (r *Router) buildPipelineListView() *PipelineListData`

Builds the pipeline list view for the UI from the orchestrator. Returns empty list if orchestrator is nil. For each registered pipeline, includes name, source dir, last run state and ID, and total run count.

**Callers:** `processUIRequest()` for `UIRequestPipelines`.

### `(r *Router) buildPipelineRunsView() *PipelineRunsData`

**Signature:** `func (r *Router) buildPipelineRunsView() *PipelineRunsData`

Returns an empty `PipelineRunsData` (stub for future UI support).

### `(r *Router) buildPipelineRunView() *PipelineRunData`

**Signature:** `func (r *Router) buildPipelineRunView() *PipelineRunData`

Returns an empty `PipelineRunData` (stub for future UI support via channel).

### `(r *Router) buildPipelineStatsView() *PipelineStatsData`

**Signature:** `func (r *Router) buildPipelineStatsView() *PipelineStatsData`

Builds pipeline stats view from the orchestrator. Returns empty data if orchestrator is nil. Calls `Orchestrator.GetStats("")` and maps to `PipelineStatsData`.

---

## internal/router/ui_types.go

Package: `router`

This file contains only type definitions and constants — no executable functions.

### Constants

`UIRequestType` enum:
- `UIRequestRepositories`, `UIRequestStatus`, `UIRequestRouters`, `UIRequestDependencies`, `UIRequestPipelines`, `UIRequestPipelineRuns`, `UIRequestPipelineRun`, `UIRequestPipelineStats`

### Types

- `UIRequest` — channel-based UI request with `Type` and `Response` channel
- `UIResponse` — response with `Data` and `Error`
- `RepositoriesData` — repository list with topology version
- `StatusData` — cluster status with nodes, failovers, drain mode, version, features, metrics
- `FeatureInfo` — runtime capability descriptor for the UI
- `RouterSnapshot` — per-router UI data (local or peer)
- `RoutersData` — aggregated router snapshots
- `DependenciesData` — aggregated dependency info with tools and nodes
- `DepsToolSummary` — per-tool dependency aggregation
- `DepsNodeInfo` — per-node dependency distribution
- `PipelineListData`, `PipelineSummary` — pipeline listing types
- `PipelineRunData`, `JobStatusData` — pipeline run detail types
- `PipelineRunsData`, `PipelineStatsData` — pipeline data collection types
