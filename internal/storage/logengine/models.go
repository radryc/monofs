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
	SpanID     string    `json:"span_id,omitempty"`
	RawMessage string    `json:"body"`
	// Labels is a flat map of normalized resource/point attributes for
	// cross-signal correlation (e.g. {"guardian.partition": "doctor"}).
	Labels map[string]string `json:"attributes,omitempty"`
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
	Levels            []string            `json:"levels,omitempty"`
	Partitions        []string            `json:"partitions,omitempty"`
	LabelValues       map[string][]string `json:"label_values,omitempty"`
	MetricNames       []string            `json:"metric_names,omitempty"`
	MetricLabelValues map[string][]string `json:"metric_label_values,omitempty"`
	// MetricLabelValuesTruncated means the per-chunk label dictionary was too
	// large to include. Discovery must scan the series index for this chunk
	// instead of treating the manifest as authoritative.
	MetricLabelValuesTruncated bool     `json:"metric_label_values_truncated,omitempty"`
	ParquetBytes               int64    `json:"parquet_bytes,omitempty"`
	RecordCount                int64    `json:"record_count,omitempty"`
	RowGroups                  int      `json:"row_groups,omitempty"`
	CompactionLevel            int      `json:"compaction_level,omitempty"`
	CompactionGroup            string   `json:"compaction_group,omitempty"`
	CompactionSources          []string `json:"compaction_sources,omitempty"`
	SchemaVersion              int      `json:"schema_version,omitempty"`
	// TraceBloom is a serialized bloom filter for trace IDs. Logs also use it
	// so trace-id lookups can prune chunks without opening Parquet.
	TraceBloom []byte `json:"trace_bloom,omitempty"`
	// TraceTagKeys and TraceTagValues form the trace discovery index. They
	// contain bounded, deduplicated tag metadata so service/tag pickers do not
	// need to scan span payloads. The truncated markers force discovery to fall
	// back to a scan when the manifest dictionary is incomplete.
	TraceTagKeys            []string            `json:"trace_tag_keys,omitempty"`
	TraceTagValues          map[string][]string `json:"trace_tag_values,omitempty"`
	TraceTagKeysTruncated   bool                `json:"trace_tag_keys_truncated,omitempty"`
	TraceTagValuesTruncated []string            `json:"trace_tag_values_truncated,omitempty"`
}

// MetricSeriesMeta describes one distinct metric series inside a chunk: the
// series identity plus the first and last sample times. Chunks written by the
// ingester carry one of these per series in series.index.json so aggregations
// like `count by (__name__)` and series discovery never scan sample data.
type MetricSeriesMeta struct {
	MetricName string            `json:"metric_name"`
	Service    string            `json:"service,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	MinTime    time.Time         `json:"min_time"`
	MaxTime    time.Time         `json:"max_time"`
}
