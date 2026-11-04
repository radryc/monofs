package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage/logengine"
)

// SpanEngine selects the telemetry storage/query implementation. The name is
// kept for the existing --spanengine flag; the selected engine owns logs,
// metrics, and traces.
type SpanEngine string

const (
	// SpanEngineLogEngine keeps telemetry in the local MonoFS Parquet chunks.
	SpanEngineLogEngine SpanEngine = "logengine"
	// SpanEngineClickHouse stores telemetry in ClickHouse tables.
	SpanEngineClickHouse SpanEngine = "clickhouse"
)

// ParseSpanEngine normalizes a user-provided trace engine. An empty value keeps
// the historical logengine behavior.
func ParseSpanEngine(raw string) (SpanEngine, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(SpanEngineLogEngine):
		return SpanEngineLogEngine, nil
	case string(SpanEngineClickHouse):
		return SpanEngineClickHouse, nil
	default:
		return "", fmt.Errorf("unsupported span engine %q (want %q or %q)", raw, SpanEngineLogEngine, SpanEngineClickHouse)
	}
}

// TraceStore is the trace half of a pluggable telemetry backend.
type TraceStore interface {
	IngestTraces(ctx context.Context, chunkID string, spans []logengine.SpanRecord) error
	QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]logengine.SpanRecord, error)
	StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(logengine.SpanRecord) error) error
	DiscoverTraceServices(ctx context.Context, from, to time.Time) ([]string, error)
	DiscoverTraceTags(ctx context.Context, from, to time.Time) ([]string, error)
	DiscoverTraceTagValues(ctx context.Context, from, to time.Time, tag string) ([]string, error)
}

// HybridBackend routes logs and metrics to the local logengine and traces to a
// separate TraceStore (currently ClickHouse). Keeping the switch here means the
// server, router, and Doctor APIs do not need signal-specific implementations.
type HybridBackend struct {
	base   DoctorBackend
	traces TraceStore
}

var _ DoctorBackend = (*HybridBackend)(nil)

// NewHybridBackend creates a hybrid telemetry backend. base may be nil for a
// traces-only deployment; log and metric operations then fail explicitly.
func NewHybridBackend(base DoctorBackend, traces TraceStore) *HybridBackend {
	return &HybridBackend{base: base, traces: traces}
}

func (b *HybridBackend) baseOrErr() (DoctorBackend, error) {
	if b.base == nil {
		return nil, fmt.Errorf("logs/metrics backend is not configured")
	}
	return b.base, nil
}

func (b *HybridBackend) IngestLogs(ctx context.Context, chunkID string, logs []logengine.LogRecord) error {
	base, err := b.baseOrErr()
	if err != nil {
		return err
	}
	return base.IngestLogs(ctx, chunkID, logs)
}

func (b *HybridBackend) IngestMetrics(ctx context.Context, chunkID string, metrics []logengine.MetricRecord) error {
	base, err := b.baseOrErr()
	if err != nil {
		return err
	}
	return base.IngestMetrics(ctx, chunkID, metrics)
}

func (b *HybridBackend) IngestTraces(ctx context.Context, chunkID string, spans []logengine.SpanRecord) error {
	return b.traces.IngestTraces(ctx, chunkID, spans)
}

func (b *HybridBackend) StreamLogs(ctx context.Context, query, service string, from, to time.Time, limit int, yield func(logengine.LogRecord) error) error {
	base, err := b.baseOrErr()
	if err != nil {
		return err
	}
	return base.StreamLogs(ctx, query, service, from, to, limit, yield)
}

func (b *HybridBackend) StreamMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricRecord) error) error {
	base, err := b.baseOrErr()
	if err != nil {
		return err
	}
	return base.StreamMetrics(ctx, query, from, to, yield)
}

func (b *HybridBackend) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(logengine.SpanRecord) error) error {
	return b.traces.StreamTraces(ctx, traceID, service, from, to, limit, yield)
}

func (b *HybridBackend) StreamMetricSeries(ctx context.Context, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricSeriesMeta) error) error {
	base, err := b.baseOrErr()
	if err != nil {
		return err
	}
	return base.StreamMetricSeries(ctx, query, from, to, yield)
}

func (b *HybridBackend) QueryLogs(ctx context.Context, query, service string, from, to time.Time, limit int) ([]logengine.LogRecord, error) {
	base, err := b.baseOrErr()
	if err != nil {
		return nil, err
	}
	return base.QueryLogs(ctx, query, service, from, to, limit)
}

func (b *HybridBackend) QueryMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time) ([]logengine.MetricRecord, error) {
	base, err := b.baseOrErr()
	if err != nil {
		return nil, err
	}
	return base.QueryMetrics(ctx, query, from, to)
}

func (b *HybridBackend) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]logengine.SpanRecord, error) {
	return b.traces.QueryTraces(ctx, traceID, service, from, to, limit)
}

func (b *HybridBackend) Aggregate(ctx context.Context, query logengine.AggregateQuery) ([]logengine.AggregateRow, error) {
	if query.Signal == logengine.SignalTraces {
		return nil, fmt.Errorf("trace aggregation is not supported by the ClickHouse backend yet")
	}
	base, err := b.baseOrErr()
	if err != nil {
		return nil, err
	}
	return base.Aggregate(ctx, query)
}

func (b *HybridBackend) QueryDuckDBSQL(ctx context.Context, dataset, query string, from, to time.Time, limit int, scope logengine.QueryScope) (logengine.DuckDBSQLResult, error) {
	if strings.EqualFold(strings.TrimSpace(dataset), "traces") {
		return logengine.DuckDBSQLResult{}, fmt.Errorf("trace SQL is not supported by the ClickHouse backend yet")
	}
	base, err := b.baseOrErr()
	if err != nil {
		return logengine.DuckDBSQLResult{}, err
	}
	return base.QueryDuckDBSQL(ctx, dataset, query, from, to, limit, scope)
}

func (b *HybridBackend) Stats(ctx context.Context) (logengine.LogEngineStats, error) {
	if b.base == nil {
		return logengine.LogEngineStats{}, nil
	}
	return b.base.Stats(ctx)
}

// DuckDBStats forwards the optional DuckDB sidecar status when the local
// logengine exposes it.
func (b *HybridBackend) DuckDBStats() logengine.DuckDBStats {
	if provider, ok := b.base.(interface{ DuckDBStats() logengine.DuckDBStats }); ok {
		return provider.DuckDBStats()
	}
	return logengine.DuckDBStats{}
}

// CompactionStats forwards the optional local compactor status.
func (b *HybridBackend) CompactionStats() logengine.CompactionStats {
	if provider, ok := b.base.(interface {
		CompactionStats() logengine.CompactionStats
	}); ok {
		return provider.CompactionStats()
	}
	return logengine.CompactionStats{}
}

func (b *HybridBackend) DiscoverTraceServices(ctx context.Context, from, to time.Time) ([]string, error) {
	return b.traces.DiscoverTraceServices(ctx, from, to)
}

func (b *HybridBackend) DiscoverTraceTags(ctx context.Context, from, to time.Time) ([]string, error) {
	return b.traces.DiscoverTraceTags(ctx, from, to)
}

func (b *HybridBackend) DiscoverTraceTagValues(ctx context.Context, from, to time.Time, tag string) ([]string, error) {
	return b.traces.DiscoverTraceTagValues(ctx, from, to, tag)
}
