// Package router provides UI request/response types for channel-based communication.
package router

// UIRequestType identifies the type of UI request.
type UIRequestType int

const (
	UIRequestRepositories UIRequestType = iota
	UIRequestStatus
	UIRequestRouters
	UIRequestDependencies
)

// UIRequest represents a request from the UI handler.
type UIRequest struct {
	Type     UIRequestType
	Response chan UIResponse
}

// UIResponse contains the data returned to the UI handler.
type UIResponse struct {
	Data  interface{}
	Error error
}

// RepositoriesData contains repository list response.
type RepositoriesData struct {
	Repositories           []map[string]interface{} `json:"repositories"`
	CurrentTopologyVersion int64                    `json:"current_topology_version"`
}

// StatusData contains cluster status response.
type StatusData struct {
	Nodes     []map[string]interface{} `json:"nodes"`
	Failovers map[string]string        `json:"failovers"`
	DrainMode map[string]interface{}   `json:"drain_mode"`
	Version   map[string]string        `json:"version"`
	Features  []FeatureInfo            `json:"features"`
	Metrics   map[string]float64       `json:"metrics"`
}

// FeatureInfo describes a runtime capability exposed in the UI.
type FeatureInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Status      string `json:"status"`
	EnableHint  string `json:"enable_hint,omitempty"`
	DisableHint string `json:"disable_hint,omitempty"`
	HelpHint    string `json:"help_hint,omitempty"`
}

// RouterSnapshot holds UI data for a single router.
type RouterSnapshot struct {
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	Local        bool              `json:"local"`
	Status       *StatusData       `json:"status,omitempty"`
	Repositories *RepositoriesData `json:"repositories,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// RoutersData aggregates status from multiple routers.
type RoutersData struct {
	Routers     []RouterSnapshot `json:"routers"`
	GeneratedAt int64            `json:"generated_at"`
}

// DependenciesData contains aggregated dependency information queried from the cluster.
type DependenciesData struct {
	TotalFiles    int               `json:"total_files"`
	Ecosystems    int               `json:"ecosystems"`
	NodesWithData int               `json:"nodes_with_data"`
	IngestedAt    int64             `json:"ingested_at"`
	Tools         []DepsToolSummary `json:"tools"`
	Nodes         []DepsNodeInfo    `json:"nodes"`
}

// DepsToolSummary aggregates per-tool dependency information.
type DepsToolSummary struct {
	Tool  string `json:"tool"`
	Files int    `json:"files"`
}

// DepsNodeInfo describes dependency file distribution on a single node.
type DepsNodeInfo struct {
	NodeID string `json:"node_id"`
	Files  int    `json:"files"`
}

// PipelineSummary is a brief pipeline overview for the UI list.
type PipelineSummary struct {
	Name         string `json:"name"`
	SourceDir    string `json:"source_dir,omitempty"`
	LastRunState string `json:"last_run_state"`
	LastRunID    string `json:"last_run_id,omitempty"`
	RunCount     int    `json:"run_count"`
}

// PipelineListData wraps a list of pipeline summaries for JSON serialization.
type PipelineListData struct {
	Pipelines []PipelineSummary `json:"pipelines"`
}

// PipelineRunData serializes a pipeline run for the UI.
type PipelineRunData struct {
	RunID        string          `json:"run_id"`
	PipelineName string          `json:"pipeline_name"`
	State        string          `json:"state"`
	Trigger      string          `json:"trigger"`
	CommitSHA    string          `json:"commit_sha"`
	Branch       string          `json:"branch"`
	Tag          string          `json:"tag,omitempty"`
	PRNumber     int             `json:"pr_number,omitempty"`
	CreatedAt    string          `json:"created_at"`
	StartedAt    string          `json:"started_at,omitempty"`
	FinishedAt   string          `json:"finished_at,omitempty"`
	Jobs         []JobStatusData `json:"jobs"`
	Affected     []string        `json:"affected,omitempty"`
}

// PipelineRunsData wraps a list of pipeline runs for JSON serialization.
type PipelineRunsData struct {
	Runs []PipelineRunData `json:"runs"`
}

// JobStatusData serializes a single job status within a pipeline run.
type JobStatusData struct {
	JobName    string   `json:"job_name"`
	State      string   `json:"state"`
	Needs      []string `json:"needs,omitempty"`
	WorkerID   string   `json:"worker_id,omitempty"`
	Retries    int      `json:"retries"`
	MaxRetries int      `json:"max_retries"`
	Error      string   `json:"error,omitempty"`
	ExitCode   int      `json:"exit_code,omitempty"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	DurationMs int64    `json:"duration_ms,omitempty"`
}

// PipelineStatsData serializes aggregate pipeline statistics for the UI.
type PipelineStatsData struct {
	TotalRuns     int     `json:"total_runs"`
	SucceededRuns int     `json:"succeeded_runs"`
	FailedRuns    int     `json:"failed_runs"`
	SuccessRate   float64 `json:"success_rate"`
	AvgDurationMs int64   `json:"avg_duration_ms"`
	P50DurationMs int64   `json:"p50_duration_ms"`
	P95DurationMs int64   `json:"p95_duration_ms"`
}
