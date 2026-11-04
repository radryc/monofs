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
	// These tests exercise DuckDB specifically; auto mode would route the
	// small queries in several cases to the native scanner.
	engine.queryMode = QueryModeDuckDB
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

func TestDuckDBLogRegexIsAnchored(t *testing.T) {
	ctx := context.Background()
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-duck-regex", []LogRecord{{
		Timestamp: base.Add(time.Minute), Level: "info", Service: "myapi-service", RawMessage: "ok",
	}}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	records, err := engine.QueryLogs(ctx, `{service=~"api"}`, "", base, base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("unanchored regex returned %d records, want 0", len(records))
	}
	records, err = engine.QueryLogs(ctx, `{service=~"myapi-.*"}`, "", base, base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("anchored regex returned %d records, want 1", len(records))
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

func TestDuckDBPingDoesNotWaitForQuerySlot(t *testing.T) {
	engine := newDuckTestEngine(t)
	duck, ok := engine.duck.(*duckQueryEngine)
	if !ok {
		t.Skip("DuckDB is not enabled")
	}
	if err := duck.acquire(context.Background()); err != nil {
		t.Fatalf("acquire() error = %v", err)
	}
	defer duck.release()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.CheckDuckDB(ctx); err != nil {
		t.Fatalf("CheckDuckDB() blocked on query slot: %v", err)
	}
}

func TestDuckDBSQLConsoleScopesPartitions(t *testing.T) {
	ctx := context.Background()
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-sql-scope", []LogRecord{
		{Timestamp: base.Add(time.Minute), Level: "error", Service: "guardian", RawMessage: "eu failure", Labels: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(2 * time.Minute), Level: "info", Service: "guardian", RawMessage: "us ok", Labels: map[string]string{"guardian.partition": "prod-us"}},
		{Timestamp: base.Add(3 * time.Minute), Level: "error", Service: "doctor", RawMessage: "eu failure", Labels: map[string]string{"guardian.partition": "prod-eu"}},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}

	all, err := engine.QueryDuckDBSQL(ctx, "logs", "SELECT partition, count(*) AS n FROM logs GROUP BY partition ORDER BY partition", base, base.Add(time.Hour), 100, QueryScope{})
	if err != nil {
		t.Fatalf("QueryDuckDBSQL() error = %v", err)
	}
	if all.RowCount != 2 {
		t.Fatalf("all rows = %#v, want two partitions", all.Rows)
	}

	scopedCtx := WithQueryScope(ctx, QueryScope{Restricted: true, Partitions: []string{"prod-us"}})
	scoped, err := engine.QueryDuckDBSQL(scopedCtx, "logs", "SELECT partition, count(*) AS n FROM logs GROUP BY partition", base, base.Add(time.Hour), 100, QueryScope{Restricted: true, Partitions: []string{"prod-us"}})
	if err != nil {
		t.Fatalf("scoped QueryDuckDBSQL() error = %v", err)
	}
	if scoped.RowCount != 1 || scoped.Rows[0][0] != "prod-us" {
		t.Fatalf("scoped rows = %#v, want only prod-us", scoped.Rows)
	}

	if _, err := engine.QueryDuckDBSQL(ctx, "logs", "DELETE FROM logs", base, base.Add(time.Hour), 100, QueryScope{}); err == nil {
		t.Fatal("write query unexpectedly succeeded")
	}
}

func TestDuckDBSQLEmptyRangeReturnsTypedColumns(t *testing.T) {
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	result, err := engine.QueryDuckDBSQL(context.Background(), "logs", "SELECT * FROM logs", base, base.Add(time.Hour), 10, QueryScope{})
	if err != nil {
		t.Fatalf("QueryDuckDBSQL() error = %v", err)
	}
	if result.RowCount != 0 || len(result.Columns) == 0 {
		t.Fatalf("result = %#v, want typed columns and no rows", result)
	}
}

func TestDuckDBSQLMetricAndTraceViews(t *testing.T) {
	ctx := context.Background()
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestMetrics(ctx, "chunk-sql-metrics", []MetricRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", MetricName: "requests_total", Value: 3, Labels: map[string]string{"guardian.partition": "prod-eu"}},
	}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}
	metrics, err := engine.QueryDuckDBSQL(ctx, "metrics", "SELECT service, metric_name, sum(value) AS total FROM metrics GROUP BY 1, 2", base, base.Add(time.Hour), 10, QueryScope{})
	if err != nil {
		t.Fatalf("metrics SQL error = %v", err)
	}
	if metrics.RowCount != 1 {
		t.Fatalf("metrics rows = %#v", metrics.Rows)
	}

	if err := engine.IngestTraces(ctx, "chunk-sql-traces", []SpanRecord{
		{Timestamp: base.Add(time.Minute), EndTime: base.Add(time.Minute + time.Second), TraceID: "trace-a", SpanID: "span-a", Service: "doctor", Name: "a", StatusCode: "STATUS_CODE_OK", Attributes: map[string]string{"guardian.partition": "prod-us"}},
	}); err != nil {
		t.Fatalf("IngestTraces() error = %v", err)
	}
	traces, err := engine.QueryDuckDBSQL(ctx, "traces", "SELECT service, status_code, count(*) AS spans FROM traces GROUP BY 1, 2", base, base.Add(time.Hour), 10, QueryScope{})
	if err != nil {
		t.Fatalf("traces SQL error = %v", err)
	}
	if traces.RowCount != 1 {
		t.Fatalf("trace rows = %#v", traces.Rows)
	}
}

func TestDuckDBSQLASTRestrictsTablesToSelectedDataset(t *testing.T) {
	ctx := context.Background()
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-sql-ast", []LogRecord{
		{Timestamp: base.Add(time.Minute), Level: "info", Service: "guardian", RawMessage: "ok", Labels: map[string]string{"guardian.partition": "prod-eu"}},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	if _, err := engine.QueryDuckDBSQL(ctx, "logs", "SELECT * FROM metrics", base, base.Add(time.Hour), 10, QueryScope{}); err == nil {
		t.Fatal("cross-dataset query unexpectedly succeeded")
	}
	result, err := engine.QueryDuckDBSQL(ctx, "logs", "WITH recent AS (SELECT * FROM logs) SELECT count(*) AS n FROM recent", base, base.Add(time.Hour), 10, QueryScope{})
	if err != nil {
		t.Fatalf("CTE query error = %v", err)
	}
	if result.RowCount != 1 {
		t.Fatalf("CTE result = %#v", result)
	}
}

func TestDuckDBMetricDiscoveryUsesNativeMetadata(t *testing.T) {
	ctx := context.Background()
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestMetrics(ctx, "chunk-duck-discovery", []MetricRecord{
		{Timestamp: base.Add(time.Minute), Service: "goalert.k8s.goalert", MetricName: "guardian.probe.up", Value: 1, Labels: map[string]string{"guardian.partition": "goalert"}},
		{Timestamp: base.Add(2 * time.Minute), Service: "doctor.query", MetricName: "doctor.requests", Value: 1, Labels: map[string]string{"guardian.partition": "doctor"}},
	}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}

	records, err := engine.QueryMetrics(ctx, MetricQuery{
		LabelMatchers: []MetricLabelMatcher{
			{Name: metricDiscoveryMatcherName, Value: metricDiscoveryModeNames, Type: MetricMatchEqual},
			{Name: "guardian.partition", Value: "goalert", Type: MetricMatchEqual},
		},
	}, base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}
	if len(records) != 1 || records[0].MetricName != "guardian.probe.up" {
		t.Fatalf("discovery records = %+v, want only goalert metric", records)
	}
}

func TestDuckDBSQLASTRejectsExternalReaders(t *testing.T) {
	engine := newDuckTestEngine(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	cases := []string{
		"SELECT * FROM read_parquet('/etc/passwd')",
		"SELECT * FROM read_csv_auto('/etc/passwd')",
		"SELECT * FROM read_text('/etc/passwd')",
		"SELECT * FROM range(10)",
		"SELECT * FROM information_schema.tables",
		"SELECT * FROM logs, 'x.parquet'",
		"SELECT * FROM logs WHERE service IN (SELECT service FROM metrics)",
	}
	for _, query := range cases {
		if _, err := engine.QueryDuckDBSQL(context.Background(), "logs", query, base, base.Add(time.Hour), 10, QueryScope{}); err == nil {
			t.Fatalf("QueryDuckDBSQL(%q) unexpectedly succeeded", query)
		}
	}
}
