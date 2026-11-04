package spanengine

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 1000
)

// Memory is a reference implementation of Engine. It stores every span in
// memory and scans linearly; it exists to pin down the API, semantics, and
// tests before the columnar/chunked implementation lands. It is not intended
// for production data volumes.
type Memory struct {
	mu      sync.RWMutex
	spans   []Span
	byTrace map[string][]Span
	minTime time.Time
	maxTime time.Time
	closed  bool
}

var _ Engine = (*Memory)(nil)

// NewMemory creates an empty in-memory span engine.
func NewMemory() *Memory {
	return &Memory{byTrace: make(map[string][]Span)}
}

// Ingest implements Engine.
func (m *Memory) Ingest(_ context.Context, spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("spanengine: closed")
	}
	for _, span := range spans {
		span = cloneSpan(span)
		m.spans = append(m.spans, span)
		if span.TraceID != "" {
			m.byTrace[span.TraceID] = append(m.byTrace[span.TraceID], span)
		}
		if m.minTime.IsZero() || span.Start.Before(m.minTime) {
			m.minTime = span.Start
		}
		if m.maxTime.IsZero() || span.End.After(m.maxTime) {
			m.maxTime = span.End
		}
	}
	return nil
}

// Search implements Engine.
func (m *Memory) Search(_ context.Context, query SearchQuery) (SearchPage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	offset := 0
	if query.Cursor != "" {
		parsed, err := strconv.Atoi(query.Cursor)
		if err != nil || parsed < 0 {
			return SearchPage{}, fmt.Errorf("spanengine: invalid cursor %q", query.Cursor)
		}
		offset = parsed
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return SearchPage{}, fmt.Errorf("spanengine: closed")
	}

	matchedTraces := make(map[string]struct{})
	for _, span := range m.spans {
		if !traceOverlaps(span, query.From, query.To) {
			continue
		}
		if query.Service != "" && span.Service != query.Service {
			continue
		}
		if query.Operation != "" && span.Name != query.Operation {
			continue
		}
		if query.StatusCode != "" && span.StatusCode != query.StatusCode {
			continue
		}
		if !durationMatches(span.Duration(), query.MinDuration, query.MaxDuration) {
			continue
		}
		if !attributesMatch(span.Attributes, query.Attributes) {
			continue
		}
		if span.TraceID != "" {
			matchedTraces[span.TraceID] = struct{}{}
		}
	}

	summaries := make([]TraceSummary, 0, len(matchedTraces))
	for traceID := range matchedTraces {
		spans := m.byTrace[traceID]
		if len(spans) == 0 {
			continue
		}
		summaries = append(summaries, summarizeTrace(traceID, spans))
	}
	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].Start.Equal(summaries[j].Start) {
			return summaries[i].Start.After(summaries[j].Start)
		}
		return summaries[i].TraceID < summaries[j].TraceID
	})

	if offset > len(summaries) {
		offset = len(summaries)
	}
	end := offset + limit
	if end > len(summaries) {
		end = len(summaries)
	}
	page := SearchPage{Traces: append([]TraceSummary(nil), summaries[offset:end]...)}
	if end < len(summaries) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

// GetTrace implements Engine.
func (m *Memory) GetTrace(_ context.Context, traceID string) ([]Span, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("spanengine: closed")
	}
	spans := m.byTrace[traceID]
	out := make([]Span, len(spans))
	for i, span := range spans {
		out[i] = cloneSpan(span)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].SpanID < out[j].SpanID
	})
	return out, nil
}

// Stats implements Engine.
func (m *Memory) Stats(context.Context) Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Stats{
		Spans:   int64(len(m.spans)),
		Traces:  int64(len(m.byTrace)),
		MinTime: m.minTime,
		MaxTime: m.maxTime,
	}
}

// Close implements Engine.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.spans = nil
	m.byTrace = nil
	return nil
}

func traceOverlaps(span Span, from, to time.Time) bool {
	if !from.IsZero() && span.End.Before(from) {
		return false
	}
	if !to.IsZero() && span.Start.After(to) {
		return false
	}
	return true
}

func durationMatches(duration, min, max time.Duration) bool {
	if min > 0 && duration < min {
		return false
	}
	if max > 0 && duration > max {
		return false
	}
	return true
}

func attributesMatch(attributes, filters map[string]string) bool {
	for key, value := range filters {
		if attributes[key] != value {
			return false
		}
	}
	return true
}

func summarizeTrace(traceID string, spans []Span) TraceSummary {
	var (
		root       = spans[0]
		start      = spans[0].Start
		end        = spans[0].End
		errorSpans int
		status     = spans[0].StatusCode
	)
	for _, span := range spans {
		if span.Start.Before(start) {
			start = span.Start
		}
		if span.End.After(end) {
			end = span.End
		}
		if span.ParentSpanID == "" && (root.ParentSpanID != "" || span.Start.Before(root.Start)) {
			root = span
		}
		if span.StatusCode == "ERROR" {
			errorSpans++
			status = "ERROR"
		}
	}
	duration := time.Duration(0)
	if !end.Before(start) {
		duration = end.Sub(start)
	}
	return TraceSummary{
		TraceID:     traceID,
		RootService: root.Service,
		RootName:    root.Name,
		Start:       start,
		Duration:    duration,
		SpanCount:   len(spans),
		ErrorSpans:  errorSpans,
		StatusCode:  status,
	}
}

func cloneSpan(span Span) Span {
	if span.Attributes != nil {
		attributes := make(map[string]string, len(span.Attributes))
		for key, value := range span.Attributes {
			attributes[key] = value
		}
		span.Attributes = attributes
	}
	return span
}
