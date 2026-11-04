package server

import (
	"context"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage/logengine"
)

type fakeTraceStore struct {
	records []logengine.SpanRecord
}

func (f *fakeTraceStore) IngestTraces(context.Context, string, []logengine.SpanRecord) error {
	return nil
}

func (f *fakeTraceStore) QueryTraces(context.Context, string, string, time.Time, time.Time, int) ([]logengine.SpanRecord, error) {
	return append([]logengine.SpanRecord(nil), f.records...), nil
}

func (f *fakeTraceStore) StreamTraces(_ context.Context, _ string, _ string, _ time.Time, _ time.Time, _ int, yield func(logengine.SpanRecord) error) error {
	for _, record := range f.records {
		if err := yield(record); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeTraceStore) DiscoverTraceServices(context.Context, time.Time, time.Time) ([]string, error) {
	return []string{"api"}, nil
}

func (f *fakeTraceStore) DiscoverTraceTags(context.Context, time.Time, time.Time) ([]string, error) {
	return []string{"service.name"}, nil
}

func (f *fakeTraceStore) DiscoverTraceTagValues(context.Context, time.Time, time.Time, string) ([]string, error) {
	return []string{"api"}, nil
}

func TestParseSpanEngine(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want SpanEngine
	}{
		{"", SpanEngineLogEngine},
		{"logengine", SpanEngineLogEngine},
		{"ClickHouse", SpanEngineClickHouse},
	} {
		got, err := ParseSpanEngine(tt.raw)
		if err != nil {
			t.Fatalf("ParseSpanEngine(%q) error = %v", tt.raw, err)
		}
		if got != tt.want {
			t.Fatalf("ParseSpanEngine(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
	if _, err := ParseSpanEngine("tempo"); err == nil {
		t.Fatal("ParseSpanEngine(tempo) error = nil, want error")
	}
}

func TestHybridBackendRoutesTracesAndLogs(t *testing.T) {
	base := &doctorStreamBackendStub{
		logs: []logengine.LogRecord{{Service: "logs"}},
	}
	traces := &fakeTraceStore{records: []logengine.SpanRecord{{TraceID: "trace"}}}
	backend := NewHybridBackend(base, traces)

	logs, err := backend.QueryLogs(context.Background(), "", "", time.Time{}, time.Time{}, 0)
	if err != nil || len(logs) != 1 || logs[0].Service != "logs" {
		t.Fatalf("QueryLogs() = %+v, %v", logs, err)
	}
	spans, err := backend.QueryTraces(context.Background(), "", "", time.Time{}, time.Time{}, 0)
	if err != nil || len(spans) != 1 || spans[0].TraceID != "trace" {
		t.Fatalf("QueryTraces() = %+v, %v", spans, err)
	}
	if _, err := backend.Aggregate(context.Background(), logengine.AggregateQuery{Signal: logengine.SignalTraces}); err == nil {
		t.Fatal("trace Aggregate() error = nil, want unsupported error")
	}
}
