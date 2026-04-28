package logengine

import "time"

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
	Timestamp    time.Time // span start time
	EndTime      time.Time
	TraceID      string
	SpanID       string
	ParentSpanID string
	Service      string
	Name         string
	StatusCode   string
	Attributes   map[string]string
}

// ChunkManifest stores lightweight metadata for a single chunk.
type ChunkManifest struct {
	ChunkID    string    `json:"chunk_id"`
	Signal     Signal    `json:"signal"`
	MinTime    time.Time `json:"min_time"`
	MaxTime    time.Time `json:"max_time"`
	TraceBloom []byte    `json:"trace_bloom,omitempty"` // Serialized bloom filter for trace_id
}
