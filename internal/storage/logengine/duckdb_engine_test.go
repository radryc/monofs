//go:build cgo && duckdb

package logengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newDuckTestEngine(t *testing.T) *LogEngine {
	t.Helper()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{
		LocalCacheDir: filepath.Join(dir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})
	if err := engine.EnableDuckDB(DuckDBConfig{
		MemoryLimit:     "256MB",
		Threads:         2,
		TempDir:         filepath.Join(dir, "duckdb-tmp"),
		MaxOpenConns:    2,
		FallbackOnError: false,
	}); err != nil {
		t.Fatalf("EnableDuckDB() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

func TestDuckDBLogQueryMatchesPartitionAndLineFilters(t *testing.T) {
	ctx := context.Background()
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	logs := []LogRecord{
		{Timestamp: base.Add(time.Minute), Level: "error", Service: "guardian", TraceID: "trace-a", SpanID: "span-a", RawMessage: "connection timeout to database", Labels: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(2 * time.Minute), Level: "info", Service: "guardian", TraceID: "trace-b", SpanID: "span-b", RawMessage: "request ok", Labels: map[string]string{"guardian.partition": "prod-us"}},
		{Timestamp: base.Add(3 * time.Minute), Level: "warn", Service: "doctor", TraceID: "trace-c", SpanID: "span-c", RawMessage: "slow query", Labels: map[string]string{"guardian.partition": "prod-eu"}},
	}
	if err := engine.IngestLogs(ctx, "chunk-duck-logs", logs); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	results, err := engine.QueryLogs(ctx, `{service="guardian"} |= "timeout"`, "", base, base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(results) != 1 || results[0].TraceID != "trace-a" {
		t.Fatalf("QueryLogs() = %+v, want trace-a", results)
	}

	scoped := WithQueryScope(ctx, QueryScope{Restricted: true, Partitions: []string{"prod-us"}})
	results, err = engine.QueryLogs(scoped, `{}`, "", base, base.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("scoped QueryLogs() error = %v", err)
	}
	if len(results) != 1 || results[0].TraceID != "trace-b" {
		t.Fatalf("scoped QueryLogs() = %+v, want trace-b", results)
	}
}

func TestDuckDBMetricAndTraceQueries(t *testing.T) {
	ctx := context.Background()
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestMetrics(ctx, "chunk-duck-metrics", []MetricRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", MetricName: "requests_total", Value: 3, Labels: map[string]string{"env": "prod", "guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(2 * time.Minute), Service: "guardian", MetricName: "requests_total", Value: 5, Labels: map[string]string{"env": "dev", "guardian.partition": "prod-eu"}},
	}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}
	metrics, err := engine.QueryMetrics(ctx, MetricQuery{MetricName: "requests_total", LabelMatchers: []MetricLabelMatcher{{Name: "env", Value: "prod", Type: MetricMatchEqual}}}, base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}
	if len(metrics) != 1 || metrics[0].Value != 3 {
		t.Fatalf("QueryMetrics() = %+v, want one prod point", metrics)
	}

	if err := engine.IngestTraces(ctx, "chunk-duck-traces", []SpanRecord{
		{Timestamp: base.Add(time.Minute), EndTime: base.Add(time.Minute + time.Second), TraceID: "trace-a", SpanID: "span-a", Service: "doctor", Name: "a", Attributes: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(2 * time.Minute), EndTime: base.Add(2*time.Minute + time.Second), TraceID: "trace-b", SpanID: "span-b", Service: "doctor", Name: "b", Attributes: map[string]string{"guardian.partition": "prod-us"}},
	}); err != nil {
		t.Fatalf("IngestTraces() error = %v", err)
	}
	traces, err := engine.QueryTraces(ctx, "", "doctor", base, base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	if len(traces) != 2 || traces[0].TraceID != "trace-b" {
		t.Fatalf("QueryTraces() = %+v, want newest first", traces)
	}
}

func TestDuckDBUnavailableWithoutBuildTag(t *testing.T) {
	// Kept in the duckdb test file as a reminder that the default build must
	// still compile and return ErrDuckDBUnavailable. The tagged build enables it.
	if _, err := os.Stat(os.TempDir()); err != nil {
		t.Skip(err)
	}
}

func TestDuckDBPing(t *testing.T) {
	engine := newDuckTestEngine(t)
	if err := engine.CheckDuckDB(context.Background()); err != nil {
		t.Fatalf("CheckDuckDB() error = %v", err)
	}
}
