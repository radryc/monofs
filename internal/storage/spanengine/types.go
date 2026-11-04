package spanengine

import "time"

// Span is the storage-facing representation of one trace span. It is
// intentionally independent of the generic log/metric record types so the span
// engine can evolve its layout and indexes without changing ingest callers.
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Service      string
	Name         string
	Start        time.Time
	End          time.Time
	StatusCode   string // OK, ERROR, UNSET (OTLP status code)
	Attributes   map[string]string
}

// Duration returns the span's wall-clock duration. Malformed spans with an end
// before their start are treated as zero-duration rather than negative.
func (s Span) Duration() time.Duration {
	if s.End.Before(s.Start) {
		return 0
	}
	return s.End.Sub(s.Start)
}

// TraceSummary is the lightweight shape returned by trace search. It avoids
// materializing every span for list/recent views.
type TraceSummary struct {
	TraceID     string
	RootService string
	RootName    string
	Start       time.Time
	Duration    time.Duration
	SpanCount   int
	ErrorSpans  int
	StatusCode  string
}

// SearchQuery describes a trace search. Attributes are exact-match AND
// filters; richer expression support can be added without changing the
// storage API.
type SearchQuery struct {
	From        time.Time
	To          time.Time
	Service     string
	Operation   string
	StatusCode  string
	MinDuration time.Duration
	MaxDuration time.Duration
	Attributes  map[string]string
	Limit       int
	Cursor      string
}

// SearchPage is a page of trace summaries plus the cursor for the next page.
type SearchPage struct {
	Traces     []TraceSummary
	NextCursor string
}

// Stats is a small operational snapshot for health endpoints and benchmarks.
type Stats struct {
	Spans   int64
	Traces  int64
	MinTime time.Time
	MaxTime time.Time
}
