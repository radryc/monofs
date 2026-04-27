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
	results, err := engine.QueryLogs(ctx, `{service="payment"} |= "connection timeout"`)
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
