package logengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

// LogEngine is the main interface for the high-compression, searchable log, metric & trace engine.
type LogEngine struct {
	store        *CachedStore
	ingester     *Ingester
	query        *QueryEngine
	compactor    *Compactor
	duck         duckQueryBackend
	duckFallback bool
}

// Config holds configuration for the LogEngine.
type Config struct {
	LocalCacheDir string
	ChunkDuration time.Duration
}

// New creates a new LogEngine instance with the given storage backend and configuration.
func New(backend ObjectStoreBackend, cfg Config) *LogEngine {
	cachedStore := NewCachedStore(backend, cfg.LocalCacheDir)
	ingester := NewIngester(cachedStore, cfg.ChunkDuration)
	query := NewQueryEngine(cachedStore)
	return &LogEngine{
		store:     cachedStore,
		ingester:  ingester,
		query:     query,
		compactor: newCompactor(cachedStore, ingester, query),
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
	records := make([]LogRecord, 0)
	if err := e.StreamLogs(ctx, queryStr, service, from, to, limit, func(record LogRecord) error {
		records = append(records, record)
		return nil
	}); err != nil {
		return nil, err
	}
	return records, nil
}

// StreamLogs executes a MonoFS log query and yields matching log records.
func (e *LogEngine) StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(LogRecord) error) error {
	if e.duck != nil {
		err := e.duck.StreamLogs(ctx, queryStr, service, from, to, limit, yield)
		if err == nil {
			return nil
		}
		if !e.duckFallback || ctx.Err() != nil {
			return err
		}
		log.Printf("logengine duckdb log query failed, falling back to native scanner: %v", err)
	}
	return e.query.StreamLogs(ctx, queryStr, service, from, to, limit, yield)
}

// QueryMetrics returns metric data points matching the given query and time range.
func (e *LogEngine) QueryMetrics(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error) {
	records := make([]MetricRecord, 0)
	if err := e.StreamMetrics(ctx, query, from, to, func(record MetricRecord) error {
		records = append(records, record)
		return nil
	}); err != nil {
		return nil, err
	}
	return records, nil
}

// StreamMetrics yields metric data points matching the given query and time range.
func (e *LogEngine) StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error {
	if e.duck != nil {
		err := e.duck.StreamMetrics(ctx, query, from, to, yield)
		if err == nil {
			return nil
		}
		if !e.duckFallback || ctx.Err() != nil {
			return err
		}
		log.Printf("logengine duckdb metric query failed, falling back to native scanner: %v", err)
	}
	return e.query.StreamMetrics(ctx, query, from, to, yield)
}

// StreamMetricSeries yields per-series metadata from the chunk series index.
func (e *LogEngine) StreamMetricSeries(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricSeriesMeta) error) error {
	return e.query.StreamMetricSeries(ctx, query, from, to, yield)
}

// QueryTraces returns trace spans matching traceID and/or service in the given time range.
func (e *LogEngine) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]SpanRecord, error) {
	records := make([]SpanRecord, 0)
	if err := e.StreamTraces(ctx, traceID, service, from, to, limit, func(record SpanRecord) error {
		records = append(records, record)
		return nil
	}); err != nil {
		return nil, err
	}
	return records, nil
}

// StreamTraces yields trace spans matching traceID and/or service in the given time range.
func (e *LogEngine) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error {
	if e.duck != nil {
		err := e.duck.StreamTraces(ctx, traceID, service, from, to, limit, yield)
		if err == nil {
			return nil
		}
		if !e.duckFallback || ctx.Err() != nil {
			return err
		}
		log.Printf("logengine duckdb trace query failed, falling back to native scanner: %v", err)
	}
	return e.query.StreamTraces(ctx, traceID, service, from, to, limit, yield)
}

// DiscoverTraceServices returns trace services from chunk manifests.
func (e *LogEngine) DiscoverTraceServices(ctx context.Context, from, to time.Time) ([]string, error) {
	return e.query.DiscoverTraceServices(ctx, from, to)
}

// DiscoverTraceTags returns discoverable trace tag names from chunk manifests.
func (e *LogEngine) DiscoverTraceTags(ctx context.Context, from, to time.Time) ([]string, error) {
	return e.query.DiscoverTraceTags(ctx, from, to)
}

// DiscoverTraceTagValues returns values for one trace tag from chunk manifests.
func (e *LogEngine) DiscoverTraceTagValues(ctx context.Context, from, to time.Time, tag string) ([]string, error) {
	return e.query.DiscoverTraceTagValues(ctx, from, to, tag)
}

// Query executes a MonoFS log query and returns the result as raw JSON bytes (for gRPC compat).
func (e *LogEngine) Query(ctx context.Context, queryStr string) ([]byte, error) {
	logs, err := e.QueryLogs(ctx, queryStr, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(logs)
}

// Close cleans up resources.
func (e *LogEngine) Close() error {
	if e.duck != nil {
		return e.duck.Close()
	}
	return nil
}

// DuckDBStats returns the optional DuckDB backend status.
func (e *LogEngine) DuckDBStats() DuckDBStats {
	if e.duck == nil {
		return DuckDBStats{}
	}
	return e.duck.Stats()
}

// CheckDuckDB verifies that the optional DuckDB backend can execute a query.
func (e *LogEngine) CheckDuckDB(ctx context.Context) error {
	if e.duck == nil {
		return ErrDuckDBUnavailable
	}
	return e.duck.Ping(ctx)
}

// QueryDuckDB executes an ad-hoc SQL query against the local Parquet files.
// This is intended for the sidecar CLI and local diagnostics, not for
// untrusted remote callers.
func (e *LogEngine) QueryDuckDB(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if e.duck == nil {
		return nil, ErrDuckDBUnavailable
	}
	return e.duck.QueryDuckDB(ctx, query, args...)
}

// LogEngineStats is a snapshot of per-signal chunk counts.
type LogEngineStats struct {
	LogChunks    int64
	MetricChunks int64
	TraceChunks  int64
}

// Stats returns per-signal chunk counts for this engine's backing store.
func (e *LogEngine) Stats(ctx context.Context) (LogEngineStats, error) {
	logChunks, _ := e.store.ListChunkPaths(ctx, SignalLogs, time.Time{}, time.Time{})
	metricChunks, _ := e.store.ListChunkPaths(ctx, SignalMetrics, time.Time{}, time.Time{})
	traceChunks, _ := e.store.ListChunkPaths(ctx, SignalTraces, time.Time{}, time.Time{})
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

// LocalPath exposes a local object path for engines that can read Parquet
// directly, such as DuckDB.
func (s *MockS3Store) LocalPath(path string) (string, bool) {
	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	if cleaned == "." || cleaned == string(os.PathSeparator) {
		return "", false
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) || filepath.IsAbs(cleaned) {
		return "", false
	}
	full := filepath.Join(s.baseDir, cleaned)
	rel, err := filepath.Rel(s.baseDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
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

// Delete removes a path (file or directory) from the local store.
func (s *MockS3Store) Delete(ctx context.Context, path string) error {
	return os.RemoveAll(filepath.Join(s.baseDir, path))
}
