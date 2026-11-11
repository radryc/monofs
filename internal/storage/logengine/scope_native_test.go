package logengine

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newNativeScopeTestEngine(t *testing.T) *LogEngine {
	t.Helper()
	dir := t.TempDir()
	engine := New(NewMockS3Store(filepath.Join(dir, "remote")), Config{
		LocalCacheDir: filepath.Join(dir, "cache"),
		ChunkDuration: time.Minute,
	})
	engine.queryMode = QueryModeNative
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

// A restricted caller must not have its top-N batch consumed by spans it
// cannot see. Before the fix the native scanner filled the limit with newer
// noise and only applied the scope at emit time, returning nothing.
func TestNativeTraceScopeFiltersBeforeTopN(t *testing.T) {
	ctx := context.Background()
	engine := newNativeScopeTestEngine(t)
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	spans := make([]SpanRecord, 0, 11)
	for i := 0; i < 10; i++ {
		spans = append(spans, SpanRecord{
			Timestamp:  base.Add(time.Duration(i) * time.Second),
			EndTime:    base.Add(time.Duration(i)*time.Second + time.Millisecond),
			TraceID:    "noise",
			SpanID:     "noise-span",
			Service:    "noise",
			Name:       "noise",
			Attributes: map[string]string{"guardian.partition": "noise"},
		})
	}
	spans = append(spans, SpanRecord{
		Timestamp:  base.Add(-time.Minute),
		EndTime:    base.Add(-time.Minute + time.Millisecond),
		TraceID:    "match",
		SpanID:     "match-span",
		Service:    "match",
		Name:       "match",
		Attributes: map[string]string{"guardian.partition": "match"},
	})
	if err := engine.IngestTraces(ctx, "chunk-scope-traces", spans); err != nil {
		t.Fatalf("IngestTraces() error = %v", err)
	}

	scoped := WithQueryScope(ctx, QueryScope{Restricted: true, Partitions: []string{"match"}})
	got, err := engine.QueryTraces(scoped, "", "", base.Add(-time.Hour), base.Add(time.Hour), 1)
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	if len(got) != 1 || got[0].TraceID != "match" {
		t.Fatalf("QueryTraces() = %+v, want the match span despite newer noise", got)
	}
}

func TestNativeLogScopeFiltersBeforeTopN(t *testing.T) {
	ctx := context.Background()
	engine := newNativeScopeTestEngine(t)
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	logs := make([]LogRecord, 0, 11)
	for i := 0; i < 10; i++ {
		logs = append(logs, LogRecord{
			Timestamp:  base.Add(time.Duration(i) * time.Second),
			Service:    "noise",
			RawMessage: "noise",
			Labels:     map[string]string{"guardian.partition": "noise"},
		})
	}
	logs = append(logs, LogRecord{
		Timestamp:  base.Add(-time.Minute),
		Service:    "match",
		RawMessage: "match",
		Labels:     map[string]string{"guardian.partition": "match"},
	})
	if err := engine.IngestLogs(ctx, "chunk-scope-logs", logs); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}

	scoped := WithQueryScope(ctx, QueryScope{Restricted: true, Partitions: []string{"match"}})
	got, err := engine.QueryLogs(scoped, `{}`, "", base.Add(-time.Hour), base.Add(time.Hour), 1)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(got) != 1 || got[0].Service != "match" {
		t.Fatalf("QueryLogs() = %+v, want the match log despite newer noise", got)
	}
}
