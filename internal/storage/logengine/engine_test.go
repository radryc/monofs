package logengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

func TestMockS3Store_GhostChunk(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "s3store_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	store := NewMockS3Store(tmpDir)

	_, err = store.Read(ctx, "nonexistent/file.txt")
	if err != ErrGhostChunk {
		t.Fatalf("expected ErrGhostChunk, got %v", err)
	}
}

func TestLogEngine_IngestAndQuery(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	cfg := Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	}

	engine := New(backend, cfg)

	// Make sure it implements storage.StorageBackend
	var _ storage.StorageBackend = engine

	// Test IngestLogs
	logs := []LogRecord{
		{
			Timestamp:  time.Now(),
			Level:      "error",
			Service:    "payment",
			TraceID:    "trc-123",
			RawMessage: "connection timeout to database",
		},
		{
			Timestamp:  time.Now(),
			Level:      "info",
			Service:    "payment",
			TraceID:    "trc-124",
			RawMessage: "payment processed successfully",
		},
	}

	err = engine.IngestLogs(ctx, "chunk-1", logs)
	if err != nil {
		t.Fatalf("failed to ingest logs: %v", err)
	}

	// Test QueryLogs
	// Our mock query engine simply returns a mocked record for any query that includes "|=",
	// but we can at least ensure the pipeline executes without errors.
	results, err := engine.QueryLogs(ctx, `{service="payment"} |= "connection timeout"`, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("failed to query logs: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("expected at least 1 result, got 0")
	}

	if results[0].Service != "payment" {
		t.Fatalf("expected service payment, got %s", results[0].Service)
	}
}

func TestLogEngine_QueryLogsRespectsTimeRange(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logengine_range_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	backend := NewMockS3Store(filepath.Join(tmpDir, "remote"))
	engine := New(backend, Config{
		LocalCacheDir: filepath.Join(tmpDir, "cache"),
		ChunkDuration: 5 * time.Minute,
	})

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	older := []LogRecord{{
		Timestamp:  base.Add(-2 * time.Hour),
		Level:      "info",
		Service:    "payment",
		TraceID:    "trace-old",
		RawMessage: "older event",
	}}
	newer := []LogRecord{{
		Timestamp:  base.Add(-10 * time.Minute),
		Level:      "info",
		Service:    "payment",
		TraceID:    "trace-new",
		RawMessage: "newer event",
	}}

	if err := engine.IngestLogs(ctx, "chunk-old", older); err != nil {
		t.Fatalf("IngestLogs(chunk-old) error = %v", err)
	}
	if err := engine.IngestLogs(ctx, "chunk-new", newer); err != nil {
		t.Fatalf("IngestLogs(chunk-new) error = %v", err)
	}

	results, err := engine.QueryLogs(ctx, `{service="payment"}`, "", base.Add(-30*time.Minute), base, 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("QueryLogs() returned %d records, want 1", len(results))
	}
	if got := results[0].RawMessage; got != "newer event" {
		t.Fatalf("QueryLogs() returned %q, want newer event", got)
	}
}
