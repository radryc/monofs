package logengine

import "time"

// MetricMatchType identifies how a metric label matcher should be evaluated.
type MetricMatchType string

const (
	MetricMatchEqual     MetricMatchType = "equal"
	MetricMatchNotEqual  MetricMatchType = "not_equal"
	MetricMatchRegexp    MetricMatchType = "regexp"
	MetricMatchNotRegexp MetricMatchType = "not_regexp"
)

// MetricLabelMatcher filters metrics by label name/value.
type MetricLabelMatcher struct {
	Name  string          `json:"name"`
	Value string          `json:"value"`
	Type  MetricMatchType `json:"type"`
}

// MetricQuery defines the filters that can be pushed into metric chunk scans.
type MetricQuery struct {
	MetricName    string               `json:"metric_name,omitempty"`
	Service       string               `json:"service,omitempty"`
	LabelMatchers []MetricLabelMatcher `json:"label_matchers,omitempty"`
}

// Signal identifies the telemetry signal type stored in a chunk.
type Signal string

const (
	SignalLogs    Signal = "logs"
	SignalMetrics Signal = "metrics"
	SignalTraces  Signal = "traces"
)

// LogRecord represents a single structured log entry.
type LogRecord struct {
	Timestamp  time.Time `json:"timestamp"`
	Level      string    `json:"severity_text,omitempty"`
	Service    string    `json:"service"`
	TraceID    string    `json:"trace_id,omitempty"`
	RawMessage string    `json:"body"`
}

// MetricRecord represents a single metric data point.
type MetricRecord struct {
	Timestamp  time.Time
	Service    string
	MetricName string
	Value      float64
	// Labels is a flat map of label key→value pairs (e.g. {"env": "prod"}).
	Labels map[string]string
}

// SpanRecord represents a single trace span.
type SpanRecord struct {
	Timestamp    time.Time         `json:"start_time"`
	EndTime      time.Time         `json:"end_time"`
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	Service      string            `json:"service"`
	Name         string            `json:"name"`
	StatusCode   string            `json:"status_code,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// ChunkManifest stores lightweight metadata for a single chunk.
type ChunkManifest struct {
	ChunkID           string              `json:"chunk_id"`
	Signal            Signal              `json:"signal"`
	MinTime           time.Time           `json:"min_time"`
	MaxTime           time.Time           `json:"max_time"`
	Services          []string            `json:"services,omitempty"`
	MetricNames       []string            `json:"metric_names,omitempty"`
	MetricLabelValues map[string][]string `json:"metric_label_values,omitempty"`
	TraceBloom        []byte              `json:"trace_bloom,omitempty"` // Serialized bloom filter for trace_id
}
