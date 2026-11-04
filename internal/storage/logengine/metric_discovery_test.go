package logengine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverMetricNamesFiltersByStoredPartition(t *testing.T) {
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{
		LocalCacheDir: filepath.Join(dir, "cache"),
	})
	ctx := context.Background()
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	if err := engine.IngestMetrics(ctx, "chunk-discovery-partition", []MetricRecord{
		{Timestamp: base, Service: "doctor.query", MetricName: "doctor_metric", Value: 1, Labels: map[string]string{"guardian.partition": "doctor"}},
		{Timestamp: base.Add(time.Second), Service: "monofs.node", MetricName: "monofs_metric", Value: 1, Labels: map[string]string{"guardian.partition": "monofs"}},
	}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}

	// The partition matcher forces the exact name-only series scan. The chunk
	// contains both partitions, so a manifest-only result would incorrectly
	// include monofs_metric.
	records, err := engine.QueryMetrics(ctx, MetricQuery{
		LabelMatchers: []MetricLabelMatcher{
			{Name: "guardian.partition", Value: "doctor", Type: MetricMatchEqual},
			{Name: metricDiscoveryMatcherName, Value: metricDiscoveryModeNames, Type: MetricMatchEqual},
		},
	}, base.Add(-time.Minute), base.Add(time.Minute))
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}
	if len(records) != 1 || records[0].MetricName != "doctor_metric" {
		t.Fatalf("metric names = %+v, want only doctor_metric", records)
	}
}

func TestStreamMetricSeriesHonorsCardinalityLimit(t *testing.T) {
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{
		LocalCacheDir:   filepath.Join(dir, "cache"),
		MaxMetricSeries: 2,
	})
	ctx := context.Background()
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	if err := engine.IngestMetrics(ctx, "chunk-series-limit", []MetricRecord{
		{Timestamp: base, Service: "svc", MetricName: "metric_a", Value: 1},
		{Timestamp: base, Service: "svc", MetricName: "metric_b", Value: 1},
		{Timestamp: base, Service: "svc", MetricName: "metric_c", Value: 1},
	}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}

	err := engine.StreamMetricSeries(ctx, MetricQuery{}, base.Add(-time.Minute), base.Add(time.Minute), func(MetricSeriesMeta) error {
		return nil
	})
	if !errors.Is(err, ErrMetricSeriesLimit) {
		t.Fatalf("StreamMetricSeries() error = %v, want ErrMetricSeriesLimit", err)
	}
}

func TestMetricDiscoveryCacheExpiresEntries(t *testing.T) {
	cache := newMetricDiscoveryCache()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	cache.put("key", []MetricRecord{{MetricName: "metric_a"}})

	if records, ok := cache.get("key"); !ok || len(records) != 1 {
		t.Fatalf("cache.get() = %v, %v, want cached entry", records, ok)
	}

	now = now.Add(cache.ttl)
	if _, ok := cache.get("key"); ok {
		t.Fatal("cache.get() returned an expired entry")
	}
}
