package logengine

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAggregateLogsByServiceAndPartition(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(ctx, "chunk-aggregate-logs", []LogRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", Level: "error", RawMessage: "a", Labels: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(2 * time.Minute), Service: "guardian", Level: "error", RawMessage: "b", Labels: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(3 * time.Minute), Service: "guardian", Level: "info", RawMessage: "c", Labels: map[string]string{"guardian.partition": "prod-us"}},
		{Timestamp: base.Add(4 * time.Minute), Service: "doctor", Level: "info", RawMessage: "d", Labels: map[string]string{"guardian.partition": "prod-eu"}},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	rows, err := engine.Aggregate(ctx, AggregateQuery{
		Signal:  SignalLogs,
		From:    base,
		To:      base.Add(time.Hour),
		GroupBy: []string{"partition", "service"},
		Aggregates: []AggregateSpec{
			{Op: AggregateCount, Alias: "logs"},
			{Op: AggregateMin, Field: "timestamp", Alias: "first"},
			{Op: AggregateMax, Field: "timestamp", Alias: "last"},
		},
	})
	if err != nil {
		t.Fatalf("Aggregate() error = %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3 groups", rows)
	}
	got := map[string]int64{}
	for _, row := range rows {
		got[row.Group["partition"]+"/"+row.Group["service"]] = row.Values["logs"].(int64)
	}
	if got["prod-eu/guardian"] != 2 || got["prod-us/guardian"] != 1 || got["prod-eu/doctor"] != 1 {
		t.Fatalf("group counts = %v", got)
	}
}

func TestAggregateMetricsSumMinMax(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestMetrics(ctx, "chunk-aggregate-metrics", []MetricRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", MetricName: "requests_total", Value: 2, Labels: map[string]string{"env": "prod"}},
		{Timestamp: base.Add(2 * time.Minute), Service: "guardian", MetricName: "requests_total", Value: 5, Labels: map[string]string{"env": "prod"}},
		{Timestamp: base.Add(3 * time.Minute), Service: "guardian", MetricName: "requests_total", Value: 7, Labels: map[string]string{"env": "dev"}},
	}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}
	rows, err := engine.Aggregate(ctx, AggregateQuery{
		Signal:      SignalMetrics,
		MetricQuery: MetricQuery{MetricName: "requests_total"},
		From:        base,
		To:          base.Add(time.Hour),
		GroupBy:     []string{"labels.env"},
		Aggregates: []AggregateSpec{
			{Op: AggregateCount, Alias: "points"},
			{Op: AggregateSum, Field: "value", Alias: "total"},
			{Op: AggregateMin, Field: "value", Alias: "min"},
			{Op: AggregateMax, Field: "value", Alias: "max"},
		},
	})
	if err != nil {
		t.Fatalf("Aggregate() error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 groups", rows)
	}
	for _, row := range rows {
		switch row.Group["labels.env"] {
		case "prod":
			if row.Values["total"].(float64) != 7 || row.Values["min"].(float64) != 2 || row.Values["max"].(float64) != 5 {
				t.Fatalf("prod values = %+v", row.Values)
			}
		case "dev":
			if row.Values["total"].(float64) != 7 {
				t.Fatalf("dev values = %+v", row.Values)
			}
		}
	}
}

func TestAggregateTracesAndLimit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{LocalCacheDir: filepath.Join(dir, "cache")})
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestTraces(ctx, "chunk-aggregate-traces", []SpanRecord{
		{Timestamp: base.Add(time.Minute), EndTime: base.Add(time.Minute + time.Second), TraceID: "a", SpanID: "a1", Service: "doctor", Name: "a"},
		{Timestamp: base.Add(2 * time.Minute), EndTime: base.Add(2*time.Minute + time.Second), TraceID: "b", SpanID: "b1", Service: "doctor", Name: "b"},
		{Timestamp: base.Add(3 * time.Minute), EndTime: base.Add(3*time.Minute + time.Second), TraceID: "c", SpanID: "c1", Service: "guardian", Name: "c"},
	}); err != nil {
		t.Fatalf("IngestTraces() error = %v", err)
	}
	rows, err := engine.Aggregate(ctx, AggregateQuery{
		Signal:  SignalTraces,
		From:    base,
		To:      base.Add(time.Hour),
		GroupBy: []string{"service"},
		Aggregates: []AggregateSpec{
			{Op: AggregateCount, Alias: "spans"},
			{Op: AggregateMax, Field: "end_time", Alias: "last_end"},
		},
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("Aggregate() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want limit 1", rows)
	}
}
