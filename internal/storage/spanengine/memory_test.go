package spanengine

import (
	"context"
	"testing"
	"time"
)

func testSpans(base time.Time) []Span {
	return []Span{
		{
			TraceID:    "trace-a",
			SpanID:     "span-a-root",
			Service:    "api",
			Name:       "GET /a",
			Start:      base,
			End:        base.Add(100 * time.Millisecond),
			StatusCode: "OK",
			Attributes: map[string]string{"partition": "p1"},
		},
		{
			TraceID:      "trace-a",
			SpanID:       "span-a-db",
			ParentSpanID: "span-a-root",
			Service:      "db",
			Name:         "SELECT",
			Start:        base.Add(10 * time.Millisecond),
			End:          base.Add(60 * time.Millisecond),
			StatusCode:   "OK",
			Attributes:   map[string]string{"partition": "p1"},
		},
		{
			TraceID:    "trace-b",
			SpanID:     "span-b-root",
			Service:    "api",
			Name:       "GET /b",
			Start:      base.Add(time.Minute),
			End:        base.Add(time.Minute + 200*time.Millisecond),
			StatusCode: "ERROR",
			Attributes: map[string]string{"partition": "p2"},
		},
		{
			TraceID:    "trace-c",
			SpanID:     "span-c-root",
			Service:    "worker",
			Name:       "job",
			Start:      base.Add(2 * time.Minute),
			End:        base.Add(2*time.Minute + 500*time.Millisecond),
			StatusCode: "OK",
			Attributes: map[string]string{"partition": "p2"},
		},
	}
}

func TestMemorySearchAndGetTrace(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	engine := NewMemory()
	defer engine.Close()
	if err := engine.Ingest(ctx, testSpans(base)); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	page, err := engine.Search(ctx, SearchQuery{
		From:    base.Add(-time.Minute),
		To:      base.Add(10 * time.Minute),
		Service: "api",
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(page.Traces) != 2 {
		t.Fatalf("Search() returned %d traces, want 2", len(page.Traces))
	}
	if page.Traces[0].TraceID != "trace-b" || page.Traces[1].TraceID != "trace-a" {
		t.Fatalf("Search() order = %v, want newest first", []string{page.Traces[0].TraceID, page.Traces[1].TraceID})
	}

	spans, err := engine.GetTrace(ctx, "trace-a")
	if err != nil {
		t.Fatalf("GetTrace() error = %v", err)
	}
	if len(spans) != 2 || spans[0].SpanID != "span-a-root" || spans[1].SpanID != "span-a-db" {
		t.Fatalf("GetTrace() = %#v, want root then child", spans)
	}

	stats := engine.Stats(ctx)
	if stats.Spans != 4 || stats.Traces != 3 {
		t.Fatalf("Stats() = %+v, want 4 spans across 3 traces", stats)
	}
}

func TestMemorySearchFiltersAndPagination(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	engine := NewMemory()
	defer engine.Close()
	if err := engine.Ingest(ctx, testSpans(base)); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	errorPage, err := engine.Search(ctx, SearchQuery{StatusCode: "ERROR"})
	if err != nil {
		t.Fatalf("Search(status) error = %v", err)
	}
	if len(errorPage.Traces) != 1 || errorPage.Traces[0].TraceID != "trace-b" {
		t.Fatalf("Search(status) = %#v, want trace-b", errorPage.Traces)
	}

	slowPage, err := engine.Search(ctx, SearchQuery{MinDuration: 150 * time.Millisecond})
	if err != nil {
		t.Fatalf("Search(duration) error = %v", err)
	}
	if len(slowPage.Traces) != 2 {
		t.Fatalf("Search(duration) returned %d traces, want 2", len(slowPage.Traces))
	}

	attrPage, err := engine.Search(ctx, SearchQuery{Attributes: map[string]string{"partition": "p1"}})
	if err != nil {
		t.Fatalf("Search(attributes) error = %v", err)
	}
	if len(attrPage.Traces) != 1 || attrPage.Traces[0].TraceID != "trace-a" {
		t.Fatalf("Search(attributes) = %#v, want trace-a", attrPage.Traces)
	}

	first, err := engine.Search(ctx, SearchQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Search(page 1) error = %v", err)
	}
	if len(first.Traces) != 1 || first.NextCursor == "" {
		t.Fatalf("Search(page 1) = %#v, want one trace and a cursor", first)
	}
	second, err := engine.Search(ctx, SearchQuery{Limit: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("Search(page 2) error = %v", err)
	}
	if len(second.Traces) != 1 || second.Traces[0].TraceID == first.Traces[0].TraceID {
		t.Fatalf("Search(page 2) = %#v, want a different trace", second)
	}
}

func TestMemoryIngestClonesAttributes(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UTC()
	attributes := map[string]string{"partition": "p1"}
	span := Span{
		TraceID:    "trace-a",
		SpanID:     "span-a",
		Service:    "api",
		Name:       "GET /a",
		Start:      base,
		End:        base.Add(time.Millisecond),
		Attributes: attributes,
	}
	engine := NewMemory()
	defer engine.Close()
	if err := engine.Ingest(ctx, []Span{span}); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	attributes["partition"] = "mutated"
	page, err := engine.Search(ctx, SearchQuery{Attributes: map[string]string{"partition": "p1"}})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(page.Traces) != 1 {
		t.Fatalf("Search() returned %d traces, want 1 after caller mutation", len(page.Traces))
	}
}
