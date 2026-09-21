package pipeline

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	QueuePrefix = "/.queues/pipeline"
)

type RunState string

const (
	RunPending   RunState = "pending"
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
)

type JobState string

const (
	JobPending   JobState = "pending"
	JobClaimed   JobState = "claimed"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobSkipped   JobState = "skipped"
	JobCancelled JobState = "cancelled"
)

type TriggerType string

const (
	TriggerPush        TriggerType = "push"
	TriggerPullRequest TriggerType = "pull_request"
	TriggerTag         TriggerType = "tag"
	TriggerManual      TriggerType = "manual"
)

type RunnerType string

const (
	RunnerBuilder RunnerType = "builder"
	RunnerDocker  RunnerType = "docker"
	RunnerDeploy  RunnerType = "deployer"
	RunnerLambda  RunnerType = "lambda"
	RunnerBazel   RunnerType = "bazel"
)

type PipelineConfig struct {
	Name        string               `yaml:"name" json:"name"`
	On          TriggerConfig        `yaml:"on" json:"on"`
	Concurrency *ConcurrencyConfig   `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	Jobs        map[string]JobConfig `yaml:"jobs" json:"jobs"`
	SourceDir   string               `yaml:"-" json:"source_dir,omitempty"`
}

type TriggerConfig struct {
	Push        *BranchFilter `yaml:"push,omitempty" json:"push,omitempty"`
	PullRequest *BranchFilter `yaml:"pull_request,omitempty" json:"pull_request,omitempty"`
	Tags        []string      `yaml:"tags,omitempty" json:"tags,omitempty"`
}

type BranchFilter struct {
	Branches    []string `yaml:"branches,omitempty" json:"branches,omitempty"`
	PathsIgnore []string `yaml:"paths-ignore,omitempty" json:"paths-ignore,omitempty"`
	Paths       []string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

type ConcurrencyConfig struct {
	Group            string `yaml:"group" json:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress" json:"cancel_in_progress"`
}

type JobConfig struct {
	Needs          []string        `yaml:"needs,omitempty" json:"needs,omitempty"`
	If             string          `yaml:"if,omitempty" json:"if,omitempty"`
	RunsOn         RunnerType      `yaml:"runs-on" json:"runs_on"`
	Strategy       *StrategyConfig `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	TimeoutMinutes int             `yaml:"timeout-minutes,omitempty" json:"timeout_minutes,omitempty"`
	Steps          []StepConfig    `yaml:"steps" json:"steps"`
}

type StrategyConfig struct {
	Matrix      MatrixConfig `yaml:"matrix,omitempty" json:"matrix,omitempty"`
	MaxParallel int          `yaml:"max-parallel,omitempty" json:"max_parallel,omitempty"`
}

// MatrixConfig maps matrix variable names to either a static value list or
// an expression (e.g. ${{ needs.detect.outputs.packages }}) resolved from
// upstream job outputs at enqueue time.
type MatrixConfig map[string]MatrixVar

// MatrixVar is a matrix value that is either a static list or a dynamic
// expression producing a comma-separated list.
type MatrixVar struct {
	Static []string
	Expr   string
}

// UnmarshalYAML accepts either a YAML sequence of values or a scalar
// expression string.
func (m *MatrixVar) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		m.Expr = value.Value
		return nil
	case yaml.SequenceNode:
		return value.Decode(&m.Static)
	default:
		return fmt.Errorf("matrix value must be a list or an expression string")
	}
}

// MarshalJSON encodes a matrix var as either its static list or expression.
func (m MatrixVar) MarshalJSON() ([]byte, error) {
	if m.Expr != "" {
		return json.Marshal(m.Expr)
	}
	return json.Marshal(m.Static)
}

type StepConfig struct {
	Name  string            `yaml:"name,omitempty" json:"name,omitempty"`
	Uses  string            `yaml:"uses,omitempty" json:"uses,omitempty"`
	Run   string            `yaml:"run,omitempty" json:"run,omitempty"`
	With  map[string]string `yaml:"with,omitempty" json:"with,omitempty"`
	ID    string            `yaml:"id,omitempty" json:"id,omitempty"`
	Shell string            `yaml:"shell,omitempty" json:"shell,omitempty"`
}

type PipelineRun struct {
	RunID        string                `json:"run_id"`
	PipelineName string                `json:"pipeline_name"`
	State        RunState              `json:"state"`
	Trigger      TriggerType           `json:"trigger"`
	CommitSHA    string                `json:"commit_sha"`
	Branch       string                `json:"branch"`
	Tag          string                `json:"tag,omitempty"`
	PRNumber     int                   `json:"pr_number,omitempty"`
	RepoFullName string                `json:"repo_full_name,omitempty"`
	RepoURL      string                `json:"repo_url,omitempty"`
	ChangedFiles []string              `json:"changed_files,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
	StartedAt    *time.Time            `json:"started_at,omitempty"`
	FinishedAt   *time.Time            `json:"finished_at,omitempty"`
	Jobs         map[string]*JobStatus `json:"jobs"`
	Affected     []string              `json:"affected,omitempty"`
}

type JobStatus struct {
	JobName    string            `json:"job_name"`
	State      JobState          `json:"state"`
	Needs      []string          `json:"needs,omitempty"`
	WorkerID   string            `json:"worker_id,omitempty"`
	ClaimedAt  *time.Time        `json:"claimed_at,omitempty"`
	StartedAt  *time.Time        `json:"started_at,omitempty"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	Retries    int               `json:"retries"`
	MaxRetries int               `json:"max_retries"`
	Error      string            `json:"error,omitempty"`
	ExitCode   int               `json:"exit_code,omitempty"`
	Outputs    map[string]string `json:"outputs,omitempty"`
}

type Task struct {
	TaskID       string            `json:"task_id"`
	RunID        string            `json:"run_id"`
	JobName      string            `json:"job_name"`
	RunnerType   RunnerType        `json:"runner_type"`
	Steps        []StepConfig      `json:"steps"`
	Env          map[string]string `json:"env,omitempty"`
	TimeoutSec   int               `json:"timeout_sec"`
	MaxRetries   int               `json:"max_retries"`
	CreatedAt    time.Time         `json:"created_at"`
	Affected     []string          `json:"affected,omitempty"`
	ChangedFiles []string          `json:"changed_files,omitempty"`
}

type TaskResult struct {
	TaskID    string            `json:"task_id"`
	RunID     string            `json:"run_id"`
	JobName   string            `json:"job_name"`
	State     JobState          `json:"state"`
	ExitCode  int               `json:"exit_code"`
	Error     string            `json:"error,omitempty"`
	StartedAt time.Time         `json:"started_at"`
	EndedAt   time.Time         `json:"ended_at"`
	WorkerID  string            `json:"worker_id"`
	Outputs   map[string]string `json:"outputs,omitempty"`
}

type TaskClaim struct {
	TaskID    string    `json:"task_id"`
	RunID     string    `json:"run_id"`
	JobName   string    `json:"job_name"`
	WorkerID  string    `json:"worker_id"`
	ClaimedAt time.Time `json:"claimed_at"`
}

type PackageMeta struct {
	Packages map[string]PackageInfo `yaml:"packages" json:"packages"`
}

type PackageInfo struct {
	Path  string   `yaml:"path" json:"path"`
	Deps  []string `yaml:"deps" json:"deps"`
	Build string   `yaml:"build" json:"build"`
	Test  string   `yaml:"test" json:"test"`
}

type WebhookEvent struct {
	EventType    TriggerType
	CommitSHA    string
	Branch       string
	Tag          string
	PRNumber     int
	RepoURL      string
	Sender       string
	ChangedFiles []string
}

type PipelineStats struct {
	TotalRuns     int     `json:"total_runs"`
	SucceededRuns int     `json:"succeeded_runs"`
	FailedRuns    int     `json:"failed_runs"`
	SuccessRate   float64 `json:"success_rate"`
	AvgDurationMs int64   `json:"avg_duration_ms"`
	P50DurationMs int64   `json:"p50_duration_ms"`
	P95DurationMs int64   `json:"p95_duration_ms"`
}
