package logengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

func TestMockS3Store_GhostChunk(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "s3store_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	store := NewMockS3Store(tmpDir)

	_, err = store.Read(ctx, "nonexistent/file.txt")
	if err != ErrGhostChunk {
		t.Fatalf("expected ErrGhostChunk, got %v", err)
	}
}

func TestLogEngine_IngestAndQuery(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	cfg := Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	}

	engine := New(backend, cfg)

	// Make sure it implements storage.StorageBackend
	var _ storage.StorageBackend = engine

	// Test IngestLogs
	logs := []LogRecord{
		{
			Timestamp:  time.Now(),
			Level:      "error",
			Service:    "payment",
			TraceID:    "trc-123",
			RawMessage: "connection timeout to database",
		},
		{
			Timestamp:  time.Now(),
			Level:      "info",
			Service:    "payment",
			TraceID:    "trc-124",
			RawMessage: "payment processed successfully",
		},
	}

	err = engine.IngestLogs(ctx, "chunk-1", logs)
	if err != nil {
		t.Fatalf("failed to ingest logs: %v", err)
	}

	// Test QueryLogs
	// Our mock query engine simply returns a mocked record for any query that includes "|=",
	// but we can at least ensure the pipeline executes without errors.
	results, err := engine.QueryLogs(ctx, `{service="payment"} |= "connection timeout"`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("failed to query logs: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("expected at least 1 result, got 0")
	}

	if results[0].Service != "payment" {
		t.Fatalf("expected service payment, got %s", results[0].Service)
	}
}

func TestLogEngine_QueryLogsRespectsTimeRange(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_range_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	older := []LogRecord{{
		Timestamp:  base.Add(-2 * time.Hour),
		Level:      "info",
		Service:    "payment",
		TraceID:    "trace-old",
		RawMessage: "older event",
	}}
	newer := []LogRecord{{
		Timestamp:  base.Add(-10 * time.Minute),
		Level:      "info",
		Service:    "payment",
		TraceID:    "trace-new",
		RawMessage: "newer event",
	}}

	if err := engine.IngestLogs(ctx, "chunk-old", older); err != nil {
		t.Fatalf("IngestLogs(chunk-old) error = %v", err)
	}
	if err := engine.IngestLogs(ctx, "chunk-new", newer); err != nil {
		t.Fatalf("IngestLogs(chunk-new) error = %v", err)
	}

	results, err := engine.QueryLogs(ctx, `{service="payment"}`, "", base.Add(-30*time.Minute), base, 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryLogs() returned %d records, want 1", len(results))
	}
	if got := results[0].RawMessage; got != "newer event" {
		t.Fatalf("QueryLogs() returned %q, want newer event", got)
	}
}

func TestLogEngine_QueryLogsSupportsSelectorOperators(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_matchers_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	logs := []LogRecord{
		{
			Timestamp:  time.Now(),
			Level:      "error",
			Service:    "payment",
			TraceID:    "trc-123",
			RawMessage: "connection timeout to database",
		},
		{
			Timestamp:  time.Now(),
			Level:      "info",
			Service:    "payment",
			TraceID:    "trc-124",
			RawMessage: "connection timeout but recovered",
		},
		{
			Timestamp:  time.Now(),
			Level:      "error",
			Service:    "billing",
			TraceID:    "trc-125",
			RawMessage: "connection timeout to external service",
		},
	}

	if err := engine.IngestLogs(ctx, "chunk-matchers", logs); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}

	results, err := engine.QueryLogs(ctx, `{service="payment",level!="info",trace_id=~"trc-12[34]"}`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryLogs() returned %d records, want 1", len(results))
	}
	if got := results[0].TraceID; got != "trc-123" {
		t.Fatalf("QueryLogs() returned trace_id %q, want trc-123", got)
	}
}

func TestLogEngine_QueryLogsSupportsRegexAndNegativeLineFilters(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_linefilters_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	logs := []LogRecord{
		{
			Timestamp:  time.Now(),
			Level:      "error",
			Service:    "payment",
			TraceID:    "trc-123",
			RawMessage: "connection timeout to database",
		},
		{
			Timestamp:  time.Now(),
			Level:      "error",
			Service:    "payment",
			TraceID:    "trc-124",
			RawMessage: "connection timeout but ignored by retry loop",
		},
		{
			Timestamp:  time.Now(),
			Level:      "info",
			Service:    "payment",
			TraceID:    "trc-125",
			RawMessage: "payment processed successfully",
		},
	}

	if err := engine.IngestLogs(ctx, "chunk-linefilters", logs); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}

	results, err := engine.QueryLogs(ctx, `{service="payment"} != "ignored" !~ "successfully"`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryLogs() returned %d records, want 1", len(results))
	}
	if got := results[0].RawMessage; got != "connection timeout to database" {
		t.Fatalf("QueryLogs() returned %q, want database timeout record", got)
	}

	results, err = engine.QueryLogs(ctx, `{service="payment"} |~ "processed.*successfully"`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("QueryLogs() regex error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryLogs() regex returned %d records, want 1", len(results))
	}
	if got := results[0].TraceID; got != "trc-125" {
		t.Fatalf("QueryLogs() regex returned trace_id %q, want trc-125", got)
	}
}

func TestLogEngine_QueryLogsUnlimitedNewestFirstAcrossOverlappingChunks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_log_merge_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-a", []LogRecord{
		{
			Timestamp:  base.Add(-15 * time.Minute),
			Level:      "info",
			Service:    "payment",
			TraceID:    "trace-a1",
			RawMessage: "a-older",
		},
		{
			Timestamp:  base.Add(-5 * time.Minute),
			Level:      "info",
			Service:    "payment",
			TraceID:    "trace-a2",
			RawMessage: "a-newer",
		},
	}); err != nil {
		t.Fatalf("IngestLogs(chunk-a) error = %v", err)
	}
	if err := engine.IngestLogs(ctx, "chunk-b", []LogRecord{
		{
			Timestamp:  base.Add(-10 * time.Minute),
			Level:      "info",
			Service:    "payment",
			TraceID:    "trace-b1",
			RawMessage: "b-older",
		},
		{
			Timestamp:  base.Add(-1 * time.Minute),
			Level:      "info",
			Service:    "payment",
			TraceID:    "trace-b2",
			RawMessage: "b-newest",
		},
	}); err != nil {
		t.Fatalf("IngestLogs(chunk-b) error = %v", err)
	}

	results, err := engine.QueryLogs(ctx, `{service="payment"}`, "", base.Add(-30*time.Minute), base, 0)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("QueryLogs() returned %d records, want 4", len(results))
	}
	got := []string{results[0].RawMessage, results[1].RawMessage, results[2].RawMessage, results[3].RawMessage}
	want := []string{"b-newest", "a-newer", "b-older", "a-older"}
	for idx := range want {
		if got[idx] != want[idx] {
			t.Fatalf("QueryLogs() order = %v, want %v", got, want)
		}
	}
}

func TestLogEngine_QueryMetricsMetricNameDiscoveryUsesManifestMetadata(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_metric_discovery_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestMetrics(ctx, "chunk-old", []MetricRecord{{
		Timestamp:  base.Add(-2 * time.Hour),
		Service:    "api",
		MetricName: "old_metric_total",
		Value:      1,
		Labels:     map[string]string{"env": "prod"},
	}}); err != nil {
		t.Fatalf("IngestMetrics(chunk-old) error = %v", err)
	}
	if err := engine.IngestMetrics(ctx, "chunk-new-a", []MetricRecord{{
		Timestamp:  base.Add(-20 * time.Minute),
		Service:    "api",
		MetricName: "requests_total",
		Value:      1,
		Labels:     map[string]string{"env": "prod"},
	}}); err != nil {
		t.Fatalf("IngestMetrics(chunk-new-a) error = %v", err)
	}
	if err := engine.IngestMetrics(ctx, "chunk-new-b", []MetricRecord{
		{
			Timestamp:  base.Add(-5 * time.Minute),
			Service:    "api",
			MetricName: "requests_total",
			Value:      2,
			Labels:     map[string]string{"env": "prod"},
		},
		{
			Timestamp:  base.Add(-5 * time.Minute),
			Service:    "api",
			MetricName: "latency_seconds",
			Value:      0.2,
			Labels:     map[string]string{"env": "prod"},
		},
	}); err != nil {
		t.Fatalf("IngestMetrics(chunk-new-b) error = %v", err)
	}

	results, err := engine.QueryMetrics(ctx, MetricQuery{
		LabelMatchers: []MetricLabelMatcher{{
			Name:  metricDiscoveryMatcherName,
			Value: metricDiscoveryModeNames,
			Type:  MetricMatchEqual,
		}},
	}, base.Add(-30*time.Minute), base)
	if err != nil {
		t.Fatalf("QueryMetrics(discovery) error = %v", err)
	}

	got := make([]string, 0, len(results))
	for _, result := range results {
		got = append(got, result.MetricName)
	}
	want := []string{"latency_seconds", "requests_total"}
	if len(got) != len(want) {
		t.Fatalf("QueryMetrics(discovery) returned %v, want %v", got, want)
	}
	for idx := range want {
		if got[idx] != want[idx] {
			t.Fatalf("QueryMetrics(discovery) returned %v, want %v", got, want)
		}
	}
}

func TestLogEngine_QueryMetricsFiltersByMetricNameServiceAndLabels(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_metric_query_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestMetrics(ctx, "chunk-metrics", []MetricRecord{
		{
			Timestamp:  base.Add(-10 * time.Minute),
			Service:    "api",
			MetricName: "requests_total",
			Value:      2,
			Labels:     map[string]string{"env": "prod", "pod": "a"},
		},
		{
			Timestamp:  base.Add(-8 * time.Minute),
			Service:    "api",
			MetricName: "requests_total",
			Value:      3,
			Labels:     map[string]string{"env": "stage", "pod": "b"},
		},
		{
			Timestamp:  base.Add(-7 * time.Minute),
			Service:    "worker",
			MetricName: "requests_total",
			Value:      4,
			Labels:     map[string]string{"env": "prod", "pod": "c"},
		},
		{
			Timestamp:  base.Add(-6 * time.Minute),
			Service:    "api",
			MetricName: "latency_seconds",
			Value:      0.5,
			Labels:     map[string]string{"env": "prod", "pod": "a"},
		},
	}); err != nil {
		t.Fatalf("IngestMetrics(chunk-metrics) error = %v", err)
	}

	results, err := engine.QueryMetrics(ctx, MetricQuery{
		MetricName: "requests_total",
		Service:    "api",
		LabelMatchers: []MetricLabelMatcher{{
			Name:  "env",
			Value: "prod",
			Type:  MetricMatchEqual,
		}},
	}, base.Add(-30*time.Minute), base)
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryMetrics() returned %d records, want 1", len(results))
	}
	if got := results[0].Value; got != 2 {
		t.Fatalf("QueryMetrics() value = %v, want 2", got)
	}
	if got := results[0].Labels["pod"]; got != "a" {
		t.Fatalf("QueryMetrics() pod label = %q, want a", got)
	}
}

func TestLogEngine_QueryTracesRespectsLimitAndNewestOrder(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_trace_query_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestTraces(ctx, "chunk-old", []SpanRecord{{
		Timestamp:  base.Add(-2 * time.Hour),
		EndTime:    base.Add(-2*time.Hour + time.Second),
		TraceID:    "trace-old",
		SpanID:     "span-old",
		Service:    "doctor",
		Name:       "older",
		Attributes: map[string]string{"env": "prod"},
	}}); err != nil {
		t.Fatalf("IngestTraces(chunk-old) error = %v", err)
	}
	if err := engine.IngestTraces(ctx, "chunk-new", []SpanRecord{
		{
			Timestamp:  base.Add(-10 * time.Minute),
			EndTime:    base.Add(-10*time.Minute + time.Second),
			TraceID:    "trace-newer",
			SpanID:     "span-newer",
			Service:    "doctor",
			Name:       "newer",
			Attributes: map[string]string{"env": "prod"},
		},
		{
			Timestamp:  base.Add(-5 * time.Minute),
			EndTime:    base.Add(-5*time.Minute + time.Second),
			TraceID:    "trace-newest",
			SpanID:     "span-newest",
			Service:    "doctor",
			Name:       "newest",
			Attributes: map[string]string{"env": "prod"},
		},
	}); err != nil {
		t.Fatalf("IngestTraces(chunk-new) error = %v", err)
	}

	results, err := engine.QueryTraces(ctx, "", "doctor", base.Add(-3*time.Hour), base, 2)
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("QueryTraces() returned %d records, want 2", len(results))
	}
	if results[0].SpanID != "span-newest" || results[1].SpanID != "span-newer" {
		t.Fatalf("QueryTraces() order = [%s %s], want [span-newest span-newer]", results[0].SpanID, results[1].SpanID)
	}
}

func TestLogEngine_QueryTracesUnlimitedNewestFirstAcrossOverlappingChunks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_trace_merge_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestTraces(ctx, "chunk-a", []SpanRecord{
		{
			Timestamp:  base.Add(-20 * time.Minute),
			EndTime:    base.Add(-20*time.Minute + time.Second),
			TraceID:    "trace-a1",
			SpanID:     "span-a1",
			Service:    "doctor",
			Name:       "a-older",
			Attributes: map[string]string{"env": "prod"},
		},
		{
			Timestamp:  base.Add(-6 * time.Minute),
			EndTime:    base.Add(-6*time.Minute + time.Second),
			TraceID:    "trace-a2",
			SpanID:     "span-a2",
			Service:    "doctor",
			Name:       "a-newer",
			Attributes: map[string]string{"env": "prod"},
		},
	}); err != nil {
		t.Fatalf("IngestTraces(chunk-a) error = %v", err)
	}
	if err := engine.IngestTraces(ctx, "chunk-b", []SpanRecord{
		{
			Timestamp:  base.Add(-12 * time.Minute),
			EndTime:    base.Add(-12*time.Minute + time.Second),
			TraceID:    "trace-b1",
			SpanID:     "span-b1",
			Service:    "doctor",
			Name:       "b-older",
			Attributes: map[string]string{"env": "prod"},
		},
		{
			Timestamp:  base.Add(-2 * time.Minute),
			EndTime:    base.Add(-2*time.Minute + time.Second),
			TraceID:    "trace-b2",
			SpanID:     "span-b2",
			Service:    "doctor",
			Name:       "b-newest",
			Attributes: map[string]string{"env": "prod"},
		},
	}); err != nil {
		t.Fatalf("IngestTraces(chunk-b) error = %v", err)
	}

	results, err := engine.QueryTraces(ctx, "", "doctor", base.Add(-30*time.Minute), base, 0)
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("QueryTraces() returned %d records, want 4", len(results))
	}
	got := []string{results[0].SpanID, results[1].SpanID, results[2].SpanID, results[3].SpanID}
	want := []string{"span-b2", "span-a2", "span-b1", "span-a1"}
	for idx := range want {
		if got[idx] != want[idx] {
			t.Fatalf("QueryTraces() order = %v, want %v", got, want)
		}
	}
}
