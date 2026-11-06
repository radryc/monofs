//go:build cgo && duckdb

package logengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	"github.com/radryc/monofs/internal/storage/logquery"
)

type duckQueryEngine struct {
	db          *sql.DB
	connector   *duckdb.Connector
	store       *CachedStore
	config      DuckDBConfig
	querySlots  chan struct{}
	active      atomic.Int64
	waiting     atomic.Int64
	queries     atomic.Uint64
	queryErrors atomic.Uint64
	closed      atomic.Bool
}

func (e *LogEngine) EnableDuckDB(cfg DuckDBConfig) error {
	engine, err := newDuckQueryEngine(e.store, cfg)
	if err != nil {
		return err
	}
	e.duck = engine
	e.duckFallback = cfg.FallbackOnError
	return nil
}

func newDuckQueryEngine(store *CachedStore, cfg DuckDBConfig) (*duckQueryEngine, error) {
	if cfg.MemoryLimit == "" {
		cfg.MemoryLimit = "2GB"
	}
	if cfg.Threads <= 0 {
		cfg.Threads = 2
	}
	if cfg.MaxConcurrentQueries <= 0 {
		// A single heavy query can already use the full DuckDB memory budget.
		// Queue additional queries instead of multiplying peak memory.
		cfg.MaxConcurrentQueries = 1
	}
	if cfg.MaxOpenConns <= 0 {
		cfg.MaxOpenConns = 2
	}
	// Keep one connection outside the query-slot budget so health checks and
	// lightweight probes do not queue behind a long-running scan.
	if cfg.MaxOpenConns <= cfg.MaxConcurrentQueries {
		cfg.MaxOpenConns = cfg.MaxConcurrentQueries + 1
	}
	if cfg.MaxTempDirectorySize == "" {
		cfg.MaxTempDirectorySize = "20GB"
	}
	if cfg.TempDir == "" {
		if store.localDir == "" {
			return nil, fmt.Errorf("duckdb temp directory is required when no local cache directory is configured")
		}
		cfg.TempDir = filepath.Join(store.localDir, "duckdb-tmp")
	}
	if err := os.MkdirAll(cfg.TempDir, 0755); err != nil {
		return nil, fmt.Errorf("create duckdb temp directory: %w", err)
	}

	connector, err := duckdb.NewConnector("", nil)
	if err != nil {
		return nil, fmt.Errorf("open duckdb connector: %w", err)
	}

	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxOpenConns)
	// Most DuckDB settings are database-global and cannot be changed once
	// another connection is active. Apply them once on a dedicated connection
	// instead of in the per-connection init callback, which would fail on the
	// second pooled connection.
	conn, err := db.Conn(context.Background())
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open duckdb setup connection: %w", err)
	}
	for _, statement := range duckDBPragmas(cfg) {
		if _, err := conn.ExecContext(context.Background(), statement, nil); err != nil {
			_ = conn.Close()
			_ = db.Close()
			return nil, fmt.Errorf("duckdb pragma %q: %w", statement, err)
		}
	}
	if err := conn.Close(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &duckQueryEngine{
		db:         db,
		connector:  connector,
		store:      store,
		config:     cfg,
		querySlots: make(chan struct{}, cfg.MaxConcurrentQueries),
	}, nil
}

func duckDBPragmas(cfg DuckDBConfig) []string {
	return []string{
		"SET memory_limit = '" + sqlEscape(cfg.MemoryLimit) + "'",
		fmt.Sprintf("SET threads = %d", cfg.Threads),
		"SET temp_directory = '" + sqlEscape(cfg.TempDir) + "'",
		"SET max_temp_directory_size = '" + sqlEscape(cfg.MaxTempDirectorySize) + "'",
		"SET preserve_insertion_order = false",
		// read_parquet needs local filesystem access. Extension autoloading is
		// still disabled, and queries never accept user-supplied paths.
		"SET autoinstall_known_extensions = false",
		"SET autoload_known_extensions = false",
	}
}

func sqlEscape(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func (d *duckQueryEngine) acquire(ctx context.Context) error {
	d.waiting.Add(1)
	select {
	case d.querySlots <- struct{}{}:
		d.waiting.Add(-1)
		d.active.Add(1)
		return nil
	case <-ctx.Done():
		d.waiting.Add(-1)
		return ctx.Err()
	}
}

func (d *duckQueryEngine) release() {
	d.active.Add(-1)
	<-d.querySlots
}

// QueueDepth reports how many callers are waiting for a DuckDB query slot.
func (d *duckQueryEngine) QueueDepth() int64 { return d.waiting.Load() }

// ActiveQueries reports how many DuckDB queries currently hold a slot.
func (d *duckQueryEngine) ActiveQueries() int64 { return d.active.Load() }

func (d *duckQueryEngine) QueryDuckDB(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	d.queries.Add(1)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		d.queryErrors.Add(1)
	}
	return rows, err
}

func (d *duckQueryEngine) Ping(ctx context.Context) error {
	// Deliberately do not acquire a query slot. A health check must be able to
	// report whether DuckDB is alive even while a heavy query occupies the
	// single query slot; otherwise a busy-but-healthy engine is marked
	// unhealthy and every query queues behind the probe.
	var one int
	if err := d.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		d.queryErrors.Add(1)
		return err
	}
	if one != 1 {
		return fmt.Errorf("unexpected duckdb ping result %d", one)
	}
	return nil
}

func (d *duckQueryEngine) Stats() DuckDBStats {
	return DuckDBStats{
		Enabled:       true,
		Memory:        d.config.MemoryLimit,
		Threads:       d.config.Threads,
		TempDir:       d.config.TempDir,
		Queries:       d.queries.Load(),
		Errors:        d.queryErrors.Load(),
		ActiveQueries: d.active.Load(),
		QueuedQueries: d.waiting.Load(),
	}
}

func (d *duckQueryEngine) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	return d.db.Close()
}

func (d *duckQueryEngine) planLogFiles(ctx context.Context, parsed logquery.Query, service string, from, to time.Time, scope QueryScope) ([]string, error) {
	query := &QueryEngine{store: d.store}
	chunkIDs, err := d.store.ListChunkPaths(ctx, SignalLogs, from, to)
	if err != nil {
		return nil, fmt.Errorf("list log chunks: %w", err)
	}
	compiled, err := compileLogQuery(parsed)
	if err != nil {
		return nil, err
	}
	observer := beginQueryPathObservation(SignalLogs)
	defer observer.finish()
	candidates, err := query.logCandidates(ctx, chunkIDs, service, compiled, from, to, observer)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !manifestMatchesScope(candidate.manifest, scope) {
			continue
		}
		local, err := d.store.EnsureLocalParquet(ctx, chunkDataPath(SignalLogs, candidate.chunkID))
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		files = append(files, local)
	}
	return files, nil
}

func (d *duckQueryEngine) planMetricFiles(ctx context.Context, query MetricQuery, from, to time.Time) ([]string, error) {
	engine := &QueryEngine{store: d.store}
	chunkIDs, err := d.store.ListChunkPaths(ctx, SignalMetrics, from, to)
	if err != nil {
		return nil, fmt.Errorf("list metric chunks: %w", err)
	}
	compiled, err := compileMetricMatchers(query.LabelMatchers)
	if err != nil {
		return nil, err
	}
	observer := beginQueryPathObservation(SignalMetrics)
	defer observer.finish()
	candidates, err := engine.metricCandidates(ctx, chunkIDs, query, compiled, from, to, observer)
	if err != nil {
		return nil, err
	}
	return d.localFiles(ctx, SignalMetrics, candidates)
}

func (d *duckQueryEngine) planTraceFiles(ctx context.Context, traceID, service string, from, to time.Time, scope QueryScope) ([]string, error) {
	engine := &QueryEngine{store: d.store}
	chunkIDs, err := d.store.ListChunkPaths(ctx, SignalTraces, from, to)
	if err != nil {
		return nil, fmt.Errorf("list trace chunks: %w", err)
	}
	observer := beginQueryPathObservation(SignalTraces)
	defer observer.finish()
	candidates, err := engine.traceCandidates(ctx, chunkIDs, traceID, service, from, to, observer)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !manifestMatchesScope(candidate.manifest, scope) {
			continue
		}
		local, err := d.store.EnsureLocalParquet(ctx, chunkDataPath(SignalTraces, candidate.chunkID))
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		files = append(files, local)
	}
	return files, nil
}

func (d *duckQueryEngine) localFiles(ctx context.Context, signal Signal, candidates []candidateChunk) ([]string, error) {
	files := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		local, err := d.store.EnsureLocalParquet(ctx, chunkDataPath(signal, candidate.chunkID))
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		files = append(files, local)
	}
	return files, nil
}

func (d *duckQueryEngine) StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(LogRecord) error) error {
	parsed, err := logquery.Parse(queryStr)
	if err != nil {
		return fmt.Errorf("failed to parse log query: %w", err)
	}
	scope, _ := QueryScopeFromContext(ctx)
	if err := d.acquire(ctx); err != nil {
		return err
	}
	defer d.release()

	files, err := d.planLogFiles(ctx, parsed, service, from, to, scope)
	if err != nil {
		d.queryErrors.Add(1)
		return err
	}
	if len(files) == 0 {
		return nil
	}
	statement, args, err := buildLogDuckSQL(files, parsed, service, from, to, limit, scope)
	if err != nil {
		d.queryErrors.Add(1)
		return err
	}
	d.queries.Add(1)
	rows, err := d.db.QueryContext(ctx, statement, args...)
	if err != nil {
		d.queryErrors.Add(1)
		return fmt.Errorf("duckdb log query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			timestamp int64
			level     sql.NullString
			service   sql.NullString
			traceID   sql.NullString
			spanID    sql.NullString
			message   sql.NullString
			labels    sql.NullString
			partition sql.NullString
		)
		if err := rows.Scan(&timestamp, &level, &service, &traceID, &spanID, &message, &labels, &partition); err != nil {
			d.queryErrors.Add(1)
			return fmt.Errorf("scan duckdb log row: %w", err)
		}
		record := LogRecord{
			Timestamp:  time.Unix(0, timestamp).UTC(),
			Level:      level.String,
			Service:    service.String,
			TraceID:    traceID.String,
			SpanID:     spanID.String,
			RawMessage: message.String,
		}
		if labels.Valid && labels.String != "" {
			_ = json.Unmarshal([]byte(labels.String), &record.Labels)
		}
		if !scope.allowsLog(record) {
			continue
		}
		if err := yield(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		d.queryErrors.Add(1)
		return fmt.Errorf("duckdb log rows: %w", err)
	}
	return nil
}

func (d *duckQueryEngine) StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error {
	if err := d.acquire(ctx); err != nil {
		return err
	}
	defer d.release()

	files, err := d.planMetricFiles(ctx, query, from, to)
	if err != nil {
		d.queryErrors.Add(1)
		return err
	}
	if len(files) == 0 {
		return nil
	}
	statement, args, err := buildMetricDuckSQL(files, query, from, to)
	if err != nil {
		d.queryErrors.Add(1)
		return err
	}
	d.queries.Add(1)
	rows, err := d.db.QueryContext(ctx, statement, args...)
	if err != nil {
		d.queryErrors.Add(1)
		return fmt.Errorf("duckdb metric query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			timestamp   int64
			startTime   sql.NullInt64
			service     sql.NullString
			metricName  sql.NullString
			value       sql.NullFloat64
			metricType  sql.NullString
			temporality sql.NullString
			labels      sql.NullString
			partition   sql.NullString
		)
		if err := rows.Scan(&timestamp, &startTime, &service, &metricName, &value, &metricType, &temporality, &labels, &partition); err != nil {
			d.queryErrors.Add(1)
			return fmt.Errorf("scan duckdb metric row: %w", err)
		}
		record := MetricRecord{
			Timestamp:   time.Unix(0, timestamp).UTC(),
			Service:     service.String,
			MetricName:  metricName.String,
			Value:       value.Float64,
			MetricType:  metricType.String,
			Temporality: temporality.String,
		}
		if startTime.Valid && startTime.Int64 != 0 {
			record.StartTime = time.Unix(0, startTime.Int64).UTC()
		}
		if labels.Valid && labels.String != "" {
			_ = json.Unmarshal([]byte(labels.String), &record.Labels)
		}
		if err := yield(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		d.queryErrors.Add(1)
		return fmt.Errorf("duckdb metric rows: %w", err)
	}
	return nil
}

func (d *duckQueryEngine) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error {
	scope, _ := QueryScopeFromContext(ctx)
	if err := d.acquire(ctx); err != nil {
		return err
	}
	defer d.release()

	files, err := d.planTraceFiles(ctx, traceID, service, from, to, scope)
	if err != nil {
		d.queryErrors.Add(1)
		return err
	}
	if len(files) == 0 {
		return nil
	}
	statement, args, err := buildTraceDuckSQL(files, traceID, service, from, to, limit, scope)
	if err != nil {
		d.queryErrors.Add(1)
		return err
	}
	d.queries.Add(1)
	rows, err := d.db.QueryContext(ctx, statement, args...)
	if err != nil {
		d.queryErrors.Add(1)
		return fmt.Errorf("duckdb trace query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			timestamp    int64
			endTime      int64
			traceIDValue sql.NullString
			spanID       sql.NullString
			parentSpanID sql.NullString
			serviceValue sql.NullString
			name         sql.NullString
			statusCode   sql.NullString
			attributes   sql.NullString
			partition    sql.NullString
		)
		if err := rows.Scan(&timestamp, &endTime, &traceIDValue, &spanID, &parentSpanID, &serviceValue, &name, &statusCode, &attributes, &partition); err != nil {
			d.queryErrors.Add(1)
			return fmt.Errorf("scan duckdb trace row: %w", err)
		}
		record := SpanRecord{
			Timestamp:    time.Unix(0, timestamp).UTC(),
			EndTime:      time.Unix(0, endTime).UTC(),
			TraceID:      traceIDValue.String,
			SpanID:       spanID.String,
			ParentSpanID: parentSpanID.String,
			Service:      serviceValue.String,
			Name:         name.String,
			StatusCode:   statusCode.String,
		}
		if attributes.Valid && attributes.String != "" {
			_ = json.Unmarshal([]byte(attributes.String), &record.Attributes)
		}
		if !scope.allowsSpan(record) {
			continue
		}
		if err := yield(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		d.queryErrors.Add(1)
		return fmt.Errorf("duckdb trace rows: %w", err)
	}
	return nil
}
