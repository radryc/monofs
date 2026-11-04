package logengine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestCompactionRemovesOrphanOutputWithoutMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	remote := NewMockS3Store(filepath.Join(dir, "remote"))
	engine := New(remote, Config{LocalCacheDir: filepath.Join(dir, "cache")})
	hour := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	// Simulate a run that wrote an output but crashed before writing its
	// completion marker. The raw sources are still present.
	if _, err := engine.ingester.FlushChunkWithOptions(ctx, SignalLogs, "compact-100-200-orphan-0", []LogRecord{
		{Timestamp: hour.Add(time.Minute), Service: "svc", Level: "info", RawMessage: "duplicate"},
		{Timestamp: hour.Add(2 * time.Minute), Service: "svc", Level: "info", RawMessage: "duplicate"},
	}, nil, nil, ChunkWriteOptions{CompactionGroup: "orphan", CompactionLevel: 1}); err != nil {
		t.Fatal(err)
	}
	for idx := 0; idx < 2; idx++ {
		start := hour.Add(time.Duration(idx+3) * time.Minute)
		if err := engine.IngestLogs(ctx, fmt.Sprintf("chunk-orphan-%d", idx), []LogRecord{
			{Timestamp: start, Service: "svc", Level: "info", RawMessage: "a"},
			{Timestamp: start.Add(time.Second), Service: "svc", Level: "info", RawMessage: "b"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := engine.CompactRange(ctx, SignalLogs, hour, hour.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	records, err := engine.QueryLogs(ctx, `{service="svc"}`, "", hour, hour.Add(20*time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("QueryLogs() returned %d records, want 4 (orphan output must not duplicate)", len(records))
	}
}

func TestTextFilterMapsMatchesAcrossRowGroups(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	engine.ingester.rowGroupRows = 3

	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	logs := make([]LogRecord, 6)
	for idx := range logs {
		message := fmt.Sprintf("filler-%d", idx)
		if idx == 1 || idx == 4 {
			message = "needle"
		}
		logs[idx] = LogRecord{
			Timestamp:  base.Add(time.Duration(idx) * time.Second),
			Service:    "svc",
			Level:      "info",
			RawMessage: message,
		}
	}
	if err := engine.IngestLogs(ctx, "chunk-text-rowgroups", logs); err != nil {
		t.Fatal(err)
	}

	results, err := engine.QueryLogs(ctx, `{service="svc"} |= "needle"`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("QueryLogs() returned %d records, want 2 across row groups", len(results))
	}
	for _, record := range results {
		if record.RawMessage != "needle" {
			t.Fatalf("QueryLogs() returned %q, want needle", record.RawMessage)
		}
	}
	limited, err := engine.QueryLogs(ctx, `{service="svc"} |= "needle"`, "", time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("limited QueryLogs() returned %d records, want 2", len(limited))
	}
}

func TestTextFilterFallsBackWhenIndexMissing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-missing-index", []LogRecord{
		{Timestamp: base, Service: "svc", Level: "info", RawMessage: "needle one"},
		{Timestamp: base.Add(time.Second), Service: "svc", Level: "info", RawMessage: "needle two"},
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := engine.store.ListChunkPaths(ctx, SignalLogs, time.Time{}, time.Time{})
	if err != nil || len(paths) != 1 {
		t.Fatalf("ListChunkPaths() = %v, %v", paths, err)
	}
	if err := engine.store.Delete(ctx, filepath.Join("chunks", string(SignalLogs), paths[0], "text.index.tar.gz")); err != nil {
		t.Fatal(err)
	}
	records, err := engine.QueryLogs(ctx, `{service="svc"} |= "needle"`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("indexless text query returned %d records, want 2 via scan fallback", len(records))
	}
}

func TestTextFilterPunctuationFallsBackToScan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-punctuation", []LogRecord{{
		Timestamp: base, Service: "svc", Level: "info", RawMessage: "connection refused ...",
	}}); err != nil {
		t.Fatal(err)
	}
	records, err := engine.QueryLogs(ctx, `{service="svc"} |= "..."`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("punctuation line filter returned %d records, want 1", len(records))
	}
}

func TestLogRegexMatchersAreAnchored(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-regex-anchor", []LogRecord{{
		Timestamp: base, Service: "myapi-service", Level: "info", RawMessage: "ok",
	}}); err != nil {
		t.Fatal(err)
	}

	records, err := engine.QueryLogs(ctx, `{service=~"api"}`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("unanchored service regex returned %d records, want 0", len(records))
	}
	records, err = engine.QueryLogs(ctx, `{service=~"myapi-.*"}`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("anchored service regex returned %d records, want 1", len(records))
	}
}

func TestMetricLabelNormalizationMatchesBothSpellings(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestMetrics(ctx, "chunk-metric-label-normalization", []MetricRecord{
		{Timestamp: base, Service: "svc", MetricName: "requests_total", Value: 1, Labels: map[string]string{"http.method": "GET"}},
		{Timestamp: base.Add(time.Second), Service: "svc", MetricName: "requests_total", Value: 2, Labels: map[string]string{"http_method": "POST"}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name  string
		value string
		want  float64
	}{
		{name: "http_method", value: "GET", want: 1},
		{name: "http.method", value: "POST", want: 2},
	} {
		records, err := engine.QueryMetrics(ctx, MetricQuery{
			MetricName:    "requests_total",
			LabelMatchers: []MetricLabelMatcher{{Name: tt.name, Value: tt.value, Type: MetricMatchEqual}},
		}, time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 || records[0].Value != tt.want {
			t.Fatalf("matcher %s=%s returned %+v, want one value %v", tt.name, tt.value, records, tt.want)
		}
	}
}

func TestTraceTagDiscoveryFallsBackWhenValuesTruncated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	spans := make([]SpanRecord, 100)
	for idx := range spans {
		spans[idx] = SpanRecord{
			Timestamp:  base.Add(time.Duration(idx) * time.Second),
			EndTime:    base.Add(time.Duration(idx)*time.Second + time.Millisecond),
			TraceID:    fmt.Sprintf("trace-%d", idx),
			SpanID:     fmt.Sprintf("span-%d", idx),
			Service:    "svc",
			Name:       "op",
			Attributes: map[string]string{"tenant": fmt.Sprintf("tenant-%03d", idx)},
		}
	}
	if err := engine.IngestTraces(ctx, "chunk-trace-tag-truncation", spans); err != nil {
		t.Fatal(err)
	}

	values, err := engine.DiscoverTraceTagValues(ctx, time.Time{}, time.Time{}, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 100 {
		t.Fatalf("DiscoverTraceTagValues() returned %d values, want 100 after scan fallback", len(values))
	}
}

type deleteFailStore struct {
	*MockS3Store
	failPath string
}

func (s *deleteFailStore) Delete(ctx context.Context, path string) error {
	if s.failPath != "" && path == s.failPath {
		return errors.New("injected delete failure")
	}
	return s.MockS3Store.Delete(ctx, path)
}

func TestCompactionMarkerRecoversAfterPartialSourceDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	remote := NewMockS3Store(filepath.Join(dir, "remote"))
	hour := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for idx := 0; idx < 2; idx++ {
		start := hour.Add(time.Duration(idx) * time.Minute)
		if err := New(remote, Config{LocalCacheDir: filepath.Join(dir, "cache")}).IngestLogs(ctx, fmt.Sprintf("chunk-marker-%d", idx), []LogRecord{
			{Timestamp: start, Service: "svc", Level: "info", RawMessage: "a"},
			{Timestamp: start.Add(time.Second), Service: "svc", Level: "info", RawMessage: "b"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := New(remote, Config{LocalCacheDir: filepath.Join(dir, "cache")}).store.ListChunkPaths(ctx, SignalLogs, hour, hour.Add(time.Hour))
	if err != nil || len(paths) != 2 {
		t.Fatalf("ListChunkPaths() = %v, %v", paths, err)
	}

	failing := &deleteFailStore{MockS3Store: remote, failPath: chunkStoragePath(SignalLogs, paths[0])}
	engine := New(failing, Config{LocalCacheDir: filepath.Join(dir, "cache-failing")})
	if _, err := engine.CompactRange(ctx, SignalLogs, hour, hour.Add(time.Hour)); err == nil {
		t.Fatal("CompactRange() error = nil, want injected delete failure")
	}

	recovered := New(remote, Config{LocalCacheDir: filepath.Join(dir, "cache-recovered")})
	result, err := recovered.CompactRange(ctx, SignalLogs, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("recovery CompactRange() error = %v", err)
	}
	if result.InputChunks != 0 || result.OutputChunks != 0 {
		t.Fatalf("recovery compaction result = %+v, want marker cleanup only", result)
	}
	records, err := recovered.QueryLogs(ctx, `{service="svc"}`, "", hour, hour.Add(20*time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("QueryLogs() returned %d records, want 4 without duplicates", len(records))
	}
}
