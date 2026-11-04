package logengine

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

type routingDuckBackend struct {
	queueDepth  int64
	logCalls    int
	metricCalls int
	traceCalls  int
}

func (f *routingDuckBackend) StreamLogs(context.Context, string, string, time.Time, time.Time, int, func(LogRecord) error) error {
	f.logCalls++
	return nil
}

func (f *routingDuckBackend) StreamMetrics(context.Context, MetricQuery, time.Time, time.Time, func(MetricRecord) error) error {
	f.metricCalls++
	return nil
}

func (f *routingDuckBackend) StreamTraces(context.Context, string, string, time.Time, time.Time, int, func(SpanRecord) error) error {
	f.traceCalls++
	return nil
}

func (f *routingDuckBackend) Ping(context.Context) error { return nil }

func (f *routingDuckBackend) QueryDuckDB(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, nil
}

func (f *routingDuckBackend) Stats() DuckDBStats { return DuckDBStats{} }
func (f *routingDuckBackend) Close() error       { return nil }
func (f *routingDuckBackend) QueueDepth() int64  { return f.queueDepth }

func newRoutingTestEngine(t *testing.T, mode QueryMode, maxLimit int, maxRange time.Duration, queueThreshold int, duck *routingDuckBackend) *LogEngine {
	t.Helper()
	dir := t.TempDir()
	engine := New(NewMockS3Store(dir), Config{
		LocalCacheDir:      dir,
		ChunkDuration:      5 * time.Minute,
		QueryMode:          mode,
		NativeMaxLimit:     maxLimit,
		NativeMaxRange:     maxRange,
		DuckQueueThreshold: queueThreshold,
	})
	engine.duck = duck
	engine.duckFallback = false
	return engine
}

func TestAutoRoutesSmallLogQueriesNative(t *testing.T) {
	duck := &routingDuckBackend{}
	engine := newRoutingTestEngine(t, QueryModeAuto, 500, 6*time.Hour, 2, duck)
	to := time.Now().UTC()
	if err := engine.StreamLogs(context.Background(), `{service="x"}`, "", to.Add(-time.Hour), to, 50, func(LogRecord) error { return nil }); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	if duck.logCalls != 0 {
		t.Fatalf("DuckDB log calls = %d, want 0 for a small recent query", duck.logCalls)
	}
}

func TestNativePathReturnsSmallLogQuery(t *testing.T) {
	dir := t.TempDir()
	engine := New(NewMockS3Store(dir), Config{
		LocalCacheDir: dir,
		ChunkDuration: 5 * time.Minute,
		QueryMode:     QueryModeNative,
	})
	base := time.Now().UTC().Add(-time.Minute)
	if err := engine.IngestLogs(context.Background(), "chunk-native-parity", []LogRecord{{
		Timestamp:  base,
		Level:      "error",
		Service:    "guardian",
		RawMessage: "connection timeout to database",
		Labels:     map[string]string{"guardian.partition": "prod-eu"},
	}}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	results, err := engine.QueryLogs(context.Background(), `{service="guardian"} |= "timeout"`, "", base.Add(-time.Minute), base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(results) != 1 || results[0].RawMessage == "" {
		t.Fatalf("QueryLogs() = %#v, want one matching record", results)
	}
}

func TestAutoRoutesLargeLogQueriesDuckDB(t *testing.T) {
	duck := &routingDuckBackend{}
	engine := newRoutingTestEngine(t, QueryModeAuto, 500, 6*time.Hour, 2, duck)
	to := time.Now().UTC()
	if err := engine.StreamLogs(context.Background(), `{service="x"}`, "", to.Add(-time.Hour), to, 5000, func(LogRecord) error { return nil }); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	if duck.logCalls != 1 {
		t.Fatalf("DuckDB log calls = %d, want 1 for a large query", duck.logCalls)
	}
}

func TestAutoRoutesTraceLookupNative(t *testing.T) {
	duck := &routingDuckBackend{}
	engine := newRoutingTestEngine(t, QueryModeAuto, 500, 6*time.Hour, 2, duck)
	to := time.Now().UTC()
	if err := engine.StreamTraces(context.Background(), "trace-123", "", to.Add(-time.Hour), to, 20000, func(SpanRecord) error { return nil }); err != nil {
		t.Fatalf("StreamTraces() error = %v", err)
	}
	if duck.traceCalls != 0 {
		t.Fatalf("DuckDB trace calls = %d, want 0 for an exact trace lookup", duck.traceCalls)
	}
}

func TestAutoRoutesLargeTraceSearchDuckDB(t *testing.T) {
	duck := &routingDuckBackend{}
	engine := newRoutingTestEngine(t, QueryModeAuto, 500, 6*time.Hour, 2, duck)
	to := time.Now().UTC()
	if err := engine.StreamTraces(context.Background(), "", "svc", to.Add(-time.Hour), to, 5000, func(SpanRecord) error { return nil }); err != nil {
		t.Fatalf("StreamTraces() error = %v", err)
	}
	if duck.traceCalls != 1 {
		t.Fatalf("DuckDB trace calls = %d, want 1 for a broad trace search", duck.traceCalls)
	}
}

func TestNativeModeNeverUsesDuckDB(t *testing.T) {
	duck := &routingDuckBackend{}
	engine := newRoutingTestEngine(t, QueryModeNative, 500, 6*time.Hour, 2, duck)
	to := time.Now().UTC()
	if err := engine.StreamLogs(context.Background(), `{service="x"}`, "", to.Add(-time.Hour), to, 5000, func(LogRecord) error { return nil }); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	if duck.logCalls != 0 {
		t.Fatalf("DuckDB log calls = %d, want 0 in native mode", duck.logCalls)
	}
}

func TestDuckDBModeAlwaysUsesDuckDB(t *testing.T) {
	duck := &routingDuckBackend{}
	engine := newRoutingTestEngine(t, QueryModeDuckDB, 500, 6*time.Hour, 2, duck)
	to := time.Now().UTC()
	if err := engine.StreamLogs(context.Background(), `{service="x"}`, "", to.Add(-time.Hour), to, 50, func(LogRecord) error { return nil }); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	if duck.logCalls != 1 {
		t.Fatalf("DuckDB log calls = %d, want 1 in duckdb mode", duck.logCalls)
	}
}

func TestDuckDBQueuePressureRoutesSmallQueriesNative(t *testing.T) {
	duck := &routingDuckBackend{queueDepth: 3}
	engine := newRoutingTestEngine(t, QueryModeAuto, 100, 2*time.Hour, 2, duck)
	to := time.Now().UTC()
	if err := engine.StreamLogs(context.Background(), `{service="x"}`, "", to.Add(-time.Hour), to, 150, func(LogRecord) error { return nil }); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	if duck.logCalls != 0 {
		t.Fatalf("DuckDB log calls = %d, want 0 while DuckDB is backlogged", duck.logCalls)
	}
}

func TestDuckDBQueuePressureRoutesLogDiscoveryNative(t *testing.T) {
	duck := &routingDuckBackend{queueDepth: 3}
	engine := newRoutingTestEngine(t, QueryModeAuto, 500, 6*time.Hour, 2, duck)
	to := time.Now().UTC()
	if err := engine.StreamLogs(context.Background(), "", "", to.Add(-time.Hour), to, 2000, func(LogRecord) error { return nil }); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	if duck.logCalls != 0 {
		t.Fatalf("DuckDB log calls = %d, want 0 for log discovery while DuckDB is backlogged", duck.logCalls)
	}
}

func TestAutoMetricRouting(t *testing.T) {
	to := time.Now().UTC()
	from := to.Add(-time.Hour)

	t.Run("selective metric goes native", func(t *testing.T) {
		duck := &routingDuckBackend{}
		engine := newRoutingTestEngine(t, QueryModeAuto, 500, 6*time.Hour, 2, duck)
		query := MetricQuery{MetricName: "http_requests_total", Service: "doctor.query.query"}
		if err := engine.StreamMetrics(context.Background(), query, from, to, func(MetricRecord) error { return nil }); err != nil {
			t.Fatalf("StreamMetrics() error = %v", err)
		}
		if duck.metricCalls != 0 {
			t.Fatalf("DuckDB metric calls = %d, want 0 for a selective metric query", duck.metricCalls)
		}
	})

	t.Run("broad metric stays on duckdb", func(t *testing.T) {
		duck := &routingDuckBackend{}
		engine := newRoutingTestEngine(t, QueryModeAuto, 500, 6*time.Hour, 2, duck)
		query := MetricQuery{MetricName: "http_requests_total"}
		if err := engine.StreamMetrics(context.Background(), query, from, to, func(MetricRecord) error { return nil }); err != nil {
			t.Fatalf("StreamMetrics() error = %v", err)
		}
		if duck.metricCalls != 1 {
			t.Fatalf("DuckDB metric calls = %d, want 1 for a broad metric query", duck.metricCalls)
		}
	})

	t.Run("queue pressure moves a bounded metric native", func(t *testing.T) {
		duck := &routingDuckBackend{queueDepth: 3}
		engine := newRoutingTestEngine(t, QueryModeAuto, 500, 6*time.Hour, 2, duck)
		query := MetricQuery{MetricName: "http_requests_total"}
		if err := engine.StreamMetrics(context.Background(), query, from, to, func(MetricRecord) error { return nil }); err != nil {
			t.Fatalf("StreamMetrics() error = %v", err)
		}
		if duck.metricCalls != 0 {
			t.Fatalf("DuckDB metric calls = %d, want 0 while DuckDB is backlogged", duck.metricCalls)
		}
	})
}
