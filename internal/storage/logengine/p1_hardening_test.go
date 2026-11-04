package logengine

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestMetricLabelDiscoveryFallsBackWhenManifestDictionaryTruncated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	metrics := make([]MetricRecord, 0, maxMetricManifestLabelValues+1)
	for i := 0; i < maxMetricManifestLabelValues+1; i++ {
		metrics = append(metrics, MetricRecord{
			Timestamp:  base.Add(time.Duration(i) * time.Millisecond),
			Service:    "svc",
			MetricName: "gauge",
			Value:      float64(i),
			Labels:     map[string]string{"instance": "instance-" + string(rune('a'+i%26)) + "-" + strconv.Itoa(i)},
		})
	}
	if err := engine.IngestMetrics(ctx, "chunk-metric-dict-truncated", metrics); err != nil {
		t.Fatal(err)
	}
	paths, err := engine.store.ListChunkPaths(ctx, SignalMetrics, time.Time{}, time.Time{})
	if err != nil || len(paths) != 1 {
		t.Fatalf("ListChunkPaths() = %v, %v", paths, err)
	}
	manifest, err := engine.store.ReadManifest(ctx, SignalMetrics, paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.MetricLabelValuesTruncated {
		t.Fatal("manifest did not mark truncated metric label dictionary")
	}
	records, err := engine.QueryMetrics(ctx, MetricQuery{
		MetricName: "gauge",
		LabelMatchers: []MetricLabelMatcher{{
			Name:  metricDiscoveryMatcherName,
			Value: metricDiscoveryModeValues + "instance",
			Type:  MetricMatchEqual,
		}},
	}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != maxMetricManifestLabelValues+1 {
		t.Fatalf("discovered %d values, want %d via series-index fallback", len(records), maxMetricManifestLabelValues+1)
	}
}

func TestQueryChunkBudget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{
		LocalCacheDir:   filepath.Join(dir, "cache"),
		MaxQueryChunks:  1,
		MaxQueryRecords: 100,
	})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := engine.IngestLogs(ctx, "chunk-budget-"+string(rune('a'+i)), []LogRecord{{
			Timestamp: base.Add(time.Duration(i) * time.Minute), Service: "svc", RawMessage: "a",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := engine.QueryLogs(ctx, `{service="svc"}`, "", time.Time{}, time.Time{}, 0)
	if !errors.Is(err, ErrQueryTooBroad) {
		t.Fatalf("QueryLogs() error = %v, want ErrQueryTooBroad", err)
	}
}

func TestQueryRecordBudget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{
		LocalCacheDir:   filepath.Join(dir, "cache"),
		MaxQueryRecords: 1,
	})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-record-budget", []LogRecord{
		{Timestamp: base, Service: "svc", RawMessage: "a"},
		{Timestamp: base.Add(time.Second), Service: "svc", RawMessage: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := engine.QueryLogs(ctx, `{service="svc"}`, "", time.Time{}, time.Time{}, 0)
	if !errors.Is(err, ErrQueryTooBroad) {
		t.Fatalf("QueryLogs() error = %v, want ErrQueryTooBroad", err)
	}
}

func TestDeleteBeforeRetentionKeepsOverlappingChunks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-old-retention", []LogRecord{
		{Timestamp: base.Add(-48 * time.Hour), Service: "svc", RawMessage: "old"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.IngestLogs(ctx, "chunk-new-retention", []LogRecord{
		{Timestamp: base, Service: "svc", RawMessage: "new"},
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := engine.DeleteBefore(ctx, SignalLogs, base.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteBefore() deleted %d chunks, want 1", deleted)
	}
	records, err := engine.QueryLogs(ctx, `{service="svc"}`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RawMessage != "new" {
		t.Fatalf("QueryLogs() = %+v, want only the new record", records)
	}
}
