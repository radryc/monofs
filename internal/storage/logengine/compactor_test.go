package logengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompactorMergesLogChunksAndRemovesSources(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_compact_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	engine := New(NewMockS3Store(filepath.Join(tmpDir, "remote")), Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	hour := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	first := []LogRecord{
		{Timestamp: hour.Add(1 * time.Minute), Level: "error", Service: "payment", RawMessage: "first"},
		{Timestamp: hour.Add(2 * time.Minute), Level: "info", Service: "payment", RawMessage: "second"},
	}
	second := []LogRecord{
		{Timestamp: hour.Add(10 * time.Minute), Level: "warn", Service: "payment", RawMessage: "third"},
		{Timestamp: hour.Add(11 * time.Minute), Level: "error", Service: "payment", RawMessage: "fourth"},
	}
	for idx, records := range [][]LogRecord{first, second} {
		chunkID := fmt.Sprintf("chunk-%d-%d", records[0].Timestamp.UnixNano(), records[len(records)-1].Timestamp.UnixNano())
		if err := engine.IngestLogs(ctx, chunkID, records); err != nil {
			t.Fatalf("IngestLogs(%d) error = %v", idx, err)
		}
	}

	result, err := engine.CompactRange(ctx, SignalLogs, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("CompactRange() error = %v", err)
	}
	if result.InputChunks != 2 || result.OutputChunks != 1 || result.DeletedChunks != 2 {
		t.Fatalf("compaction result = %+v, want 2 inputs, 1 output, 2 deletes", result)
	}

	paths, err := engine.store.ListChunkPaths(ctx, SignalLogs, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListChunkPaths() error = %v", err)
	}
	if len(paths) != 1 || !strings.HasPrefix(filepath.Base(paths[0]), "compact-") {
		t.Fatalf("remaining chunks = %v, want one compacted chunk", paths)
	}

	records, err := engine.QueryLogs(ctx, `{service="payment"}`, "", hour, hour.Add(20*time.Minute), 0)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("QueryLogs() returned %d records, want 4", len(records))
	}

	again, err := engine.CompactRange(ctx, SignalLogs, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("second CompactRange() error = %v", err)
	}
	if again.InputChunks != 0 || again.OutputChunks != 0 {
		t.Fatalf("second compaction result = %+v, want no-op", again)
	}
}

func TestCompactorMergesTraceChunks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_compact_traces_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	engine := New(NewMockS3Store(filepath.Join(tmpDir, "remote")), Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	hour := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	first := []SpanRecord{
		{Timestamp: hour.Add(time.Minute), EndTime: hour.Add(time.Minute + time.Second), TraceID: "trace-a", SpanID: "span-a", Service: "doctor", Name: "a"},
	}
	second := []SpanRecord{
		{Timestamp: hour.Add(10 * time.Minute), EndTime: hour.Add(10*time.Minute + time.Second), TraceID: "trace-b", SpanID: "span-b", Service: "doctor", Name: "b"},
	}
	for idx, records := range [][]SpanRecord{first, second} {
		chunkID := fmt.Sprintf("chunk-%d-%d", records[0].Timestamp.UnixNano(), records[len(records)-1].EndTime.UnixNano())
		if err := engine.IngestTraces(ctx, chunkID, records); err != nil {
			t.Fatalf("IngestTraces(%d) error = %v", idx, err)
		}
	}

	result, err := engine.CompactRange(ctx, SignalTraces, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("CompactRange() error = %v", err)
	}
	if result.InputChunks != 2 || result.OutputChunks != 1 || result.DeletedChunks != 2 {
		t.Fatalf("compaction result = %+v, want 2 inputs, 1 output, 2 deletes", result)
	}
	records, err := engine.QueryTraces(ctx, "", "doctor", hour, hour.Add(20*time.Minute), 0)
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("QueryTraces() returned %d records, want 2", len(records))
	}
}

func TestCompactorMergesMetricChunks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_compact_metrics_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	engine := New(NewMockS3Store(filepath.Join(tmpDir, "remote")), Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	hour := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	first := []MetricRecord{
		{Timestamp: hour.Add(time.Minute), Service: "guardian", MetricName: "requests_total", Value: 1, Labels: map[string]string{"env": "prod"}},
	}
	second := []MetricRecord{
		{Timestamp: hour.Add(10 * time.Minute), Service: "guardian", MetricName: "requests_total", Value: 2, Labels: map[string]string{"env": "prod"}},
	}
	for idx, records := range [][]MetricRecord{first, second} {
		chunkID := fmt.Sprintf("chunk-%d-%d", records[0].Timestamp.UnixNano(), records[len(records)-1].Timestamp.UnixNano())
		if err := engine.IngestMetrics(ctx, chunkID, records); err != nil {
			t.Fatalf("IngestMetrics(%d) error = %v", idx, err)
		}
	}

	result, err := engine.CompactRange(ctx, SignalMetrics, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("CompactRange() error = %v", err)
	}
	if result.InputChunks != 2 || result.OutputChunks != 1 || result.DeletedChunks != 2 {
		t.Fatalf("compaction result = %+v, want 2 inputs, 1 output, 2 deletes", result)
	}
	records, err := engine.QueryMetrics(ctx, MetricQuery{}, hour, hour.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("QueryMetrics() returned %d records, want 2", len(records))
	}
}

func TestCompactorGroupsLogsByService(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_compact_services_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	engine := New(NewMockS3Store(filepath.Join(tmpDir, "remote")), Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})
	hour := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	records := [][]LogRecord{
		{
			{Timestamp: hour.Add(time.Minute), Service: "alpha", Level: "info", RawMessage: "a"},
			{Timestamp: hour.Add(2 * time.Minute), Service: "beta", Level: "error", RawMessage: "b"},
		},
		{
			{Timestamp: hour.Add(3 * time.Minute), Service: "alpha", Level: "info", RawMessage: "c"},
			{Timestamp: hour.Add(4 * time.Minute), Service: "beta", Level: "error", RawMessage: "d"},
		},
	}
	for idx, chunkRecords := range records {
		chunkID := fmt.Sprintf("chunk-%d-%d", chunkRecords[0].Timestamp.UnixNano(), chunkRecords[len(chunkRecords)-1].Timestamp.UnixNano())
		if err := engine.IngestLogs(ctx, chunkID, chunkRecords); err != nil {
			t.Fatalf("IngestLogs(%d) error = %v", idx, err)
		}
	}
	if _, err := engine.CompactRange(ctx, SignalLogs, hour, hour.Add(time.Hour)); err != nil {
		t.Fatalf("CompactRange() error = %v", err)
	}
	paths, err := engine.store.ListChunkPaths(ctx, SignalLogs, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListChunkPaths() error = %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("compacted paths = %v, want one chunk per service", paths)
	}
	for _, path := range paths {
		manifest, err := engine.store.ReadManifest(ctx, SignalLogs, path)
		if err != nil {
			t.Fatalf("ReadManifest() error = %v", err)
		}
		if len(manifest.Services) != 1 {
			t.Fatalf("manifest services = %v, want one service", manifest.Services)
		}
	}
}
