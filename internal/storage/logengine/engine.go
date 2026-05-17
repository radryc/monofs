package logengine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

// LogEngine is the main interface for the high-compression, searchable log, metric & trace engine.
type LogEngine struct {
	store    *CachedStore
	ingester *Ingester
	query    *QueryEngine
}

// Config holds configuration for the LogEngine.
type Config struct {
	LocalCacheDir string
	ChunkDuration time.Duration
}

// New creates a new LogEngine instance with the given storage backend and configuration.
func New(backend ObjectStoreBackend, cfg Config) *LogEngine {
	cachedStore := NewCachedStore(backend, cfg.LocalCacheDir)
	return &LogEngine{
		store:    cachedStore,
		ingester: NewIngester(cachedStore, cfg.ChunkDuration),
		query:    NewQueryEngine(cachedStore),
	}
}

// Type returns the storage type identifier.
func (e *LogEngine) Type() string {
	return "logengine"
}

// Initialize prepares the backend.
func (e *LogEngine) Initialize(ctx context.Context, config storage.BackendConfig) error {
	// Not fully implemented yet
	return nil
}

// IngestLogs writes a batch of log records to the engine.
func (e *LogEngine) IngestLogs(ctx context.Context, chunkID string, logs []LogRecord) error {
	return e.ingester.FlushChunk(ctx, SignalLogs, chunkID, logs, nil, nil)
}

// IngestMetrics writes a batch of metric records to the engine.
func (e *LogEngine) IngestMetrics(ctx context.Context, chunkID string, metrics []MetricRecord) error {
	return e.ingester.FlushChunk(ctx, SignalMetrics, chunkID, nil, metrics, nil)
}

// IngestTraces writes a batch of trace spans to the engine.
func (e *LogEngine) IngestTraces(ctx context.Context, chunkID string, spans []SpanRecord) error {
	return e.ingester.FlushChunk(ctx, SignalTraces, chunkID, nil, nil, spans)
}

// Ingest implements storage.StorageBackend interface (no-op passthrough).
func (e *LogEngine) Ingest(ctx context.Context, id string, data []byte) error {
	return nil
}

// QueryLogs executes a MonoFS log query and returns the matching log records.
func (e *LogEngine) QueryLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int) ([]LogRecord, error) {
	return e.query.QueryLogs(ctx, queryStr, service, from, to, limit)
}

// StreamLogs executes a MonoFS log query and yields matching log records.
func (e *LogEngine) StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(LogRecord) error) error {
	return e.query.StreamLogs(ctx, queryStr, service, from, to, limit, yield)
}

// QueryMetrics returns metric data points matching the given query and time range.
func (e *LogEngine) QueryMetrics(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error) {
	return e.query.QueryMetrics(ctx, query, from, to)
}

// StreamMetrics yields metric data points matching the given query and time range.
func (e *LogEngine) StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error {
	return e.query.StreamMetrics(ctx, query, from, to, yield)
}

// QueryTraces returns trace spans matching traceID and/or service in the given time range.
func (e *LogEngine) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]SpanRecord, error) {
	return e.query.QueryTraces(ctx, traceID, service, from, to, limit)
}

// StreamTraces yields trace spans matching traceID and/or service in the given time range.
func (e *LogEngine) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error {
	return e.query.StreamTraces(ctx, traceID, service, from, to, limit, yield)
}

// Query executes a MonoFS log query and returns the result as raw JSON bytes (for gRPC compat).
func (e *LogEngine) Query(ctx context.Context, queryStr string) ([]byte, error) {
	logs, err := e.query.QueryLogs(ctx, queryStr, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(logs)
}

// Close cleans up resources.
func (e *LogEngine) Close() error {
	return nil
}

// LogEngineStats is a snapshot of per-signal chunk counts.
type LogEngineStats struct {
	LogChunks    int64
	MetricChunks int64
	TraceChunks  int64
}

// Stats returns per-signal chunk counts for this engine's backing store.
func (e *LogEngine) Stats(ctx context.Context) (LogEngineStats, error) {
	logChunks, _ := e.store.ListChunks(ctx, filepath.Join("chunks", string(SignalLogs)))
	metricChunks, _ := e.store.ListChunks(ctx, filepath.Join("chunks", string(SignalMetrics)))
	traceChunks, _ := e.store.ListChunks(ctx, filepath.Join("chunks", string(SignalTraces)))
	return LogEngineStats{
		LogChunks:    int64(len(logChunks)),
		MetricChunks: int64(len(metricChunks)),
		TraceChunks:  int64(len(traceChunks)),
	}, nil
}

// --- Mock S3 Storage Implementation for demonstration ---

// MockS3Store implements StorageBackend over the local filesystem
// but simulates S3 lifecycle expirations (Ghost Chunks).
type MockS3Store struct {
	baseDir string
}

// NewMockS3Store creates a new MockS3Store.
func NewMockS3Store(baseDir string) *MockS3Store {
	return &MockS3Store{baseDir: baseDir}
}

func (s *MockS3Store) Write(ctx context.Context, path string, reader io.Reader) error {
	fullPath := filepath.Join(s.baseDir, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}
	f, err := os.Create(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, reader)
	return err
}

func (s *MockS3Store) Read(ctx context.Context, path string) (io.ReadSeekCloser, error) {
	fullPath := filepath.Join(s.baseDir, path)
	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Map NotExist to ErrGhostChunk to simulate S3 NoSuchKey
			return nil, ErrGhostChunk
		}
		return nil, err
	}
	return f, nil
}

func (s *MockS3Store) ListChunks(ctx context.Context, prefix string) ([]string, error) {
	// Simple mock listing
	fullPrefix := filepath.Join(s.baseDir, prefix)
	entries, err := os.ReadDir(fullPrefix)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var chunks []string
	for _, e := range entries {
		if e.IsDir() {
			chunks = append(chunks, e.Name())
		}
	}
	return chunks, nil
}

// dummyReadSeekCloser is a utility for testing
type dummyReadSeekCloser struct {
	*bytes.Reader
}

func (d *dummyReadSeekCloser) Close() error {
	return nil
}

type tempFileReadSeekCloser struct {
	*os.File
}

func (t *tempFileReadSeekCloser) Close() error {
	name := t.Name()
	err := t.File.Close()
	removeErr := os.Remove(name)
	if err != nil {
		return err
	}
	return removeErr
}
