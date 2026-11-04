// Package spanengine is an internal, experimental architecture for span/trace
// storage and retrieval. It is not wired into MonoFS yet; the goal is to give
// trace queries a trace-centric read path instead of reusing the generic
// log/metric chunk pipeline.
//
// The package is intentionally behind internal/ and its API is expected to
// change while we benchmark layouts. See DESIGN.md for the target architecture.
package spanengine

import "context"

// Engine is the read/write surface the rest of MonoFS will eventually use for
// spans. Implementations must be safe for concurrent use.
type Engine interface {
	// Ingest appends spans to the engine. Implementations may buffer and
	// flush asynchronously, but a successful return means the spans are
	// durable enough to be found by a subsequent Search/GetTrace.
	Ingest(ctx context.Context, spans []Span) error

	// Search returns trace summaries matching the query, newest first.
	Search(ctx context.Context, query SearchQuery) (SearchPage, error)

	// GetTrace returns every span for a trace in start-time order.
	GetTrace(ctx context.Context, traceID string) ([]Span, error)

	// Stats returns a cheap operational snapshot.
	Stats(ctx context.Context) Stats

	// Close releases any resources held by the engine.
	Close() error
}
