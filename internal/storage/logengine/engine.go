package logengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/monofs/internal/telemetrycrypto"
)

// QueryMode controls which telemetry query engine services read queries.
type QueryMode string

const (
	// QueryModeAuto sends small, recent, selective queries to the pure-Go
	// scanner and reserves DuckDB for aggregations and large scans.
	QueryModeAuto QueryMode = "auto"
	// QueryModeNative always uses the pure-Go scanner.
	QueryModeNative QueryMode = "native"
	// QueryModeDuckDB always uses DuckDB when it is enabled (the historical
	// behavior).
	QueryModeDuckDB QueryMode = "duckdb"
)

// ParseQueryMode normalizes a user-provided mode. Unknown values fall back to
// auto so a bad flag cannot disable telemetry reads.
func ParseQueryMode(raw string) QueryMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(QueryModeNative):
		return QueryModeNative
	case string(QueryModeDuckDB):
		return QueryModeDuckDB
	default:
		return QueryModeAuto
	}
}

const (
	defaultNativeMaxLimit     = 500
	defaultNativeMaxRange     = 6 * time.Hour
	defaultDuckQueueThreshold = 2
)

// LogEngine is the main interface for the high-compression, searchable log, metric & trace engine.
type LogEngine struct {
	store              *CachedStore
	ingester           *Ingester
	query              *QueryEngine
	compactor          *Compactor
	duck               duckQueryBackend
	duckFallback       bool
	duckFallbacks      atomic.Uint64
	queryMode          QueryMode
	nativeMaxLimit     int
	nativeMaxRange     time.Duration
	duckQueueThreshold int
}

// Config holds configuration for the LogEngine.
type Config struct {
	LocalCacheDir string
	ChunkDuration time.Duration
	// MaxMetricSeries bounds the distinct-series merge performed by
	// StreamMetricSeries. Zero uses the engine default.
	MaxMetricSeries int
	// QueryMode selects native vs DuckDB routing. Empty means auto.
	QueryMode QueryMode
	// NativeMaxLimit is the largest result limit routed to the native scanner
	// in auto mode. Zero uses 500.
	NativeMaxLimit int
	// NativeMaxRange is the largest time range routed to the native scanner in
	// auto mode. Zero uses 6h.
	NativeMaxRange time.Duration
	// DuckQueueThreshold routes small queries native when DuckDB already has
	// this many queries waiting. Zero uses 2.
	DuckQueueThreshold int
	// MaxQueryChunks bounds how many candidate chunks one telemetry query may
	// inspect. Zero uses the engine default.
	MaxQueryChunks int
	// MaxQueryRecords bounds how many records one streaming query may yield.
	// Zero uses the engine default. Compaction scans are not affected.
	MaxQueryRecords int64
	// Encryption enables transparent per-partition encryption of chunk objects.
	// It is optional and nil/disabled by default.
	Encryption *telemetrycrypto.Provider
}

// New creates a new LogEngine instance with the given storage backend and configuration.
func New(backend ObjectStoreBackend, cfg Config) *LogEngine {
	backend = NewEncryptedStore(backend, cfg.Encryption)
	cachedStore := NewCachedStore(backend, cfg.LocalCacheDir)
	ingester := NewIngester(cachedStore, cfg.ChunkDuration)
	query := NewQueryEngine(cachedStore)
	if cfg.MaxMetricSeries > 0 {
		query.maxMetricSeries = cfg.MaxMetricSeries
	}
	if cfg.MaxQueryChunks > 0 {
		query.maxQueryChunks = cfg.MaxQueryChunks
	}
	if cfg.MaxQueryRecords > 0 {
		query.maxQueryRecords = cfg.MaxQueryRecords
	}
	if cfg.QueryMode == "" {
		cfg.QueryMode = QueryModeAuto
	}
	if cfg.NativeMaxLimit <= 0 {
		cfg.NativeMaxLimit = defaultNativeMaxLimit
	}
	if cfg.NativeMaxRange <= 0 {
		cfg.NativeMaxRange = defaultNativeMaxRange
	}
	if cfg.DuckQueueThreshold <= 0 {
		cfg.DuckQueueThreshold = defaultDuckQueueThreshold
	}
	return &LogEngine{
		store:              cachedStore,
		ingester:           ingester,
		query:              query,
		compactor:          newCompactor(cachedStore, ingester, query),
		queryMode:          cfg.QueryMode,
		nativeMaxLimit:     cfg.NativeMaxLimit,
		nativeMaxRange:     cfg.NativeMaxRange,
		duckQueueThreshold: cfg.DuckQueueThreshold,
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
	// Both query engines require a selector. The compatibility layer already
	// normalizes an empty LogQL expression to "all services"; mirror that here
	// so internal callers cannot fail simply because they had no filter.
	if strings.TrimSpace(queryStr) == "" {
		queryStr = `{service=~".+"}`
	}
	if e.useNativeLogs(limit, from, to) {
		sent := false
		err := e.query.StreamLogs(ctx, queryStr, service, from, to, limit, func(record LogRecord) error {
			sent = true
			return yield(record)
		})
		if err == nil {
			return nil
		}
		if e.queryMode == QueryModeNative || !e.duckFallback || e.duck == nil || ctx.Err() != nil || sent {
			return err
		}
		e.duckFallbacks.Add(1)
		log.Printf("logengine native log query failed, falling back to duckdb: %v", err)
		return e.duck.StreamLogs(ctx, queryStr, service, from, to, limit, yield)
	}
	if e.duck != nil {
		sent := false
		err := e.duck.StreamLogs(ctx, queryStr, service, from, to, limit, func(record LogRecord) error {
			sent = true
			return yield(record)
		})
		if err == nil {
			return nil
		}
		if !e.duckFallback || ctx.Err() != nil || sent {
			return err
		}
		e.duckFallbacks.Add(1)
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
	// Discovery queries use internal manifest/series-index metadata and carry a
	// sentinel label matcher that DuckDB would treat as a real (and therefore
	// unmatchable) label. Always serve discovery from the native engine.
	if _, mode := stripMetricDiscoveryMatchers(query); mode != "" {
		return e.query.StreamMetrics(ctx, query, from, to, yield)
	}
	if e.useNativeMetrics(query, from, to) {
		sent := false
		err := e.query.StreamMetrics(ctx, query, from, to, func(record MetricRecord) error {
			sent = true
			return yield(record)
		})
		if err == nil {
			return nil
		}
		if e.queryMode == QueryModeNative || !e.duckFallback || e.duck == nil || ctx.Err() != nil || sent {
			return err
		}
		e.duckFallbacks.Add(1)
		log.Printf("logengine native metric query failed, falling back to duckdb: %v", err)
		return e.duck.StreamMetrics(ctx, query, from, to, yield)
	}
	if e.duck != nil {
		sent := false
		err := e.duck.StreamMetrics(ctx, query, from, to, func(record MetricRecord) error {
			sent = true
			return yield(record)
		})
		if err == nil {
			return nil
		}
		if !e.duckFallback || ctx.Err() != nil || sent {
			return err
		}
		e.duckFallbacks.Add(1)
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
	if e.useNativeTraces(traceID, service, from, to, limit) {
		sent := false
		err := e.query.StreamTraces(ctx, traceID, service, from, to, limit, func(record SpanRecord) error {
			sent = true
			return yield(record)
		})
		if err == nil {
			return nil
		}
		if e.queryMode == QueryModeNative || !e.duckFallback || e.duck == nil || ctx.Err() != nil || sent {
			return err
		}
		e.duckFallbacks.Add(1)
		log.Printf("logengine native trace query failed, falling back to duckdb: %v", err)
		return e.duck.StreamTraces(ctx, traceID, service, from, to, limit, yield)
	}
	if e.duck != nil {
		sent := false
		err := e.duck.StreamTraces(ctx, traceID, service, from, to, limit, func(record SpanRecord) error {
			sent = true
			return yield(record)
		})
		if err == nil {
			return nil
		}
		if !e.duckFallback || ctx.Err() != nil || sent {
			return err
		}
		e.duckFallbacks.Add(1)
		log.Printf("logengine duckdb trace query failed, falling back to native scanner: %v", err)
	}
	return e.query.StreamTraces(ctx, traceID, service, from, to, limit, yield)
}

// useNativeLogs decides which engine should serve a log query in auto mode.
// Small, recent queries use the native scanner because it can stop after the
// newest chunks and does not contend for DuckDB's single query slot.
func (e *LogEngine) useNativeLogs(limit int, from, to time.Time) bool {
	switch e.queryMode {
	case QueryModeNative:
		return true
	case QueryModeDuckDB:
		return false
	}
	if e.duck == nil {
		return true
	}
	if e.duckQueueDepth() > int64(e.duckQueueThreshold) &&
		limit > 0 && limit <= e.nativeMaxLimit*4 &&
		e.withinNativeRange(from, to, e.nativeMaxRange*2) {
		return true
	}
	if limit <= 0 || limit > e.nativeMaxLimit {
		return false
	}
	return e.withinNativeRange(from, to, e.nativeMaxRange)
}

// useNativeTraces keeps exact trace lookups on the native path and routes
// recent, bounded searches there as well. Broad trace searches stay on DuckDB.
func (e *LogEngine) useNativeTraces(traceID, service string, from, to time.Time, limit int) bool {
	switch e.queryMode {
	case QueryModeNative:
		return true
	case QueryModeDuckDB:
		return false
	}
	if e.duck == nil {
		return true
	}
	if traceID != "" {
		return true
	}
	if e.duckQueueDepth() > int64(e.duckQueueThreshold) &&
		limit > 0 && limit <= e.nativeMaxLimit*2 &&
		e.withinNativeRange(from, to, e.nativeMaxRange*2) {
		return true
	}
	if limit <= 0 || limit > e.nativeMaxLimit {
		return false
	}
	return e.withinNativeRange(from, to, e.nativeMaxRange)
}

// useNativeMetrics prefers the native scanner for narrow metric reads and when
// DuckDB already has a queue. Broad metric scans remain on DuckDB.
func (e *LogEngine) useNativeMetrics(query MetricQuery, from, to time.Time) bool {
	switch e.queryMode {
	case QueryModeNative:
		return true
	case QueryModeDuckDB:
		return false
	}
	if e.duck == nil {
		return true
	}
	if strings.TrimSpace(query.MetricName) == "" {
		return false
	}
	if e.duckQueueDepth() > int64(e.duckQueueThreshold) && e.withinNativeRange(from, to, e.nativeMaxRange) {
		return true
	}
	if !metricQueryIsSelective(query) {
		return false
	}
	return e.withinNativeRange(from, to, e.nativeMaxRange)
}

func (e *LogEngine) withinNativeRange(from, to time.Time, max time.Duration) bool {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return true
	}
	return to.Sub(from) <= max
}

func metricQueryIsSelective(query MetricQuery) bool {
	if strings.TrimSpace(query.MetricName) == "" {
		return false
	}
	if strings.TrimSpace(query.Service) != "" {
		return true
	}
	for _, matcher := range query.LabelMatchers {
		name := strings.TrimSpace(matcher.Name)
		if name == "" || name == metricDiscoveryMatcherName {
			continue
		}
		value := strings.TrimSpace(matcher.Value)
		switch matcher.Type {
		case "", MetricMatchEqual, MetricMatchNotEqual:
			if value != "" {
				return true
			}
		case MetricMatchRegexp, MetricMatchNotRegexp:
			if value != "" && value != ".*" && value != ".+" {
				return true
			}
		}
	}
	return false
}

func (e *LogEngine) duckQueueDepth() int64 {
	if e == nil || e.duck == nil {
		return 0
	}
	if stats, ok := e.duck.(interface{ QueueDepth() int64 }); ok {
		return stats.QueueDepth()
	}
	return 0
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
	stats := e.duck.Stats()
	stats.Fallbacks = e.duckFallbacks.Load()
	return stats
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

	// Buckets are distinct hourly chunk buckets. CompactedBuckets counts the
	// buckets that contain at least one compact-* output chunk.
	LogBuckets             int64
	MetricBuckets          int64
	TraceBuckets           int64
	LogCompactedBuckets    int64
	MetricCompactedBuckets int64
	TraceCompactedBuckets  int64
}

// DeleteBefore removes chunk manifests and data for one signal whose entire
// time range ends before cutoff. It returns the number of chunk directories
// deleted. Chunks that straddle the cutoff are kept.
func (e *LogEngine) DeleteBefore(ctx context.Context, signal Signal, cutoff time.Time) (int, error) {
	if cutoff.IsZero() {
		return 0, nil
	}
	paths, err := e.store.ListChunkPaths(ctx, signal, time.Time{}, cutoff)
	if err != nil {
		return 0, fmt.Errorf("list %s chunks for retention: %w", signal, err)
	}
	deleted := 0
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		manifest, err := e.store.ReadManifest(ctx, signal, path)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return deleted, err
		}
		if manifest.MaxTime.After(cutoff) {
			continue
		}
		if err := e.store.Delete(ctx, chunkStoragePath(signal, path)); err != nil {
			return deleted, fmt.Errorf("delete expired %s chunk %s: %w", signal, path, err)
		}
		deleted++
	}
	return deleted, nil
}

// RunRetention deletes chunks older than maxAge on every interval. It is
// disabled unless maxAge and interval are positive.
func (e *LogEngine) RunRetention(ctx context.Context, interval, maxAge time.Duration) {
	if interval <= 0 || maxAge <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			e.runRetentionOnce(ctx, maxAge)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (e *LogEngine) runRetentionOnce(ctx context.Context, maxAge time.Duration) {
	cutoff := time.Now().UTC().Add(-maxAge)
	for _, signal := range []Signal{SignalLogs, SignalMetrics, SignalTraces} {
		deleted, err := e.DeleteBefore(ctx, signal, cutoff)
		if err != nil {
			log.Printf("logengine retention failed signal=%s cutoff=%s: %v", signal, cutoff.Format(time.RFC3339), err)
			continue
		}
		if deleted > 0 {
			log.Printf("logengine retention deleted signal=%s chunks=%d cutoff=%s", signal, deleted, cutoff.Format(time.RFC3339))
		}
	}
}

// Stats returns per-signal chunk and bucket counts for this engine's backing
// store. A bucket is an hourly directory (new partition/date/hour layout or
// the legacy year/month/day/hour layout); flat chunks count as their own
// bucket so older data is not hidden.
func (e *LogEngine) Stats(ctx context.Context) (LogEngineStats, error) {
	logChunks, _ := e.store.ListChunkPaths(ctx, SignalLogs, time.Time{}, time.Time{})
	metricChunks, _ := e.store.ListChunkPaths(ctx, SignalMetrics, time.Time{}, time.Time{})
	traceChunks, _ := e.store.ListChunkPaths(ctx, SignalTraces, time.Time{}, time.Time{})

	logBuckets, logCompacted := countChunkBuckets(logChunks)
	metricBuckets, metricCompacted := countChunkBuckets(metricChunks)
	traceBuckets, traceCompacted := countChunkBuckets(traceChunks)

	return LogEngineStats{
		LogChunks:              int64(len(logChunks)),
		MetricChunks:           int64(len(metricChunks)),
		TraceChunks:            int64(len(traceChunks)),
		LogBuckets:             logBuckets,
		MetricBuckets:          metricBuckets,
		TraceBuckets:           traceBuckets,
		LogCompactedBuckets:    logCompacted,
		MetricCompactedBuckets: metricCompacted,
		TraceCompactedBuckets:  traceCompacted,
	}, nil
}

// countChunkBuckets returns the number of stored and compacted hourly buckets
// represented by chunk paths relative to chunks/<signal>/.
func countChunkBuckets(paths []string) (stored, compacted int64) {
	storedSet := make(map[string]struct{}, len(paths))
	compactedSet := make(map[string]struct{})
	for _, path := range paths {
		key := chunkBucketKey(path)
		storedSet[key] = struct{}{}
		if isCompactedChunkPath(path) {
			compactedSet[key] = struct{}{}
		}
	}
	return int64(len(storedSet)), int64(len(compactedSet))
}

func chunkBucketKey(path string) string {
	clean := strings.Trim(strings.ReplaceAll(path, "\\", "/"), "/")
	parts := strings.Split(clean, "/")
	if len(parts) == 0 || clean == "" {
		return clean
	}

	// Current layout: partition=<p>/date=YYYY-MM-DD/hour=HH/<chunkID>.
	var partition, date, hour string
	for _, part := range parts {
		switch {
		case strings.HasPrefix(part, "partition="):
			partition = part
		case strings.HasPrefix(part, "date="):
			date = part
		case strings.HasPrefix(part, "hour="):
			hour = part
		}
	}
	if date != "" && hour != "" {
		return strings.Join([]string{partition, date, hour}, "/")
	}

	// Legacy layout: YYYY/MM/DD/HH/<chunkID>.
	if len(parts) >= 5 && len(parts[0]) == 4 && len(parts[1]) == 2 && len(parts[2]) == 2 && len(parts[3]) == 2 {
		return strings.Join(parts[:4], "/")
	}

	// Flat chunk: keep it distinct rather than collapsing unrelated data.
	return clean
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
