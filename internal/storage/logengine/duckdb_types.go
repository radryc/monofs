package logengine

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrDuckDBUnavailable is returned when the binary was built without DuckDB.
var ErrDuckDBUnavailable = errors.New("logengine built without DuckDB support: rebuild with CGO_ENABLED=1 and -tags duckdb")

// DuckDBConfig configures the optional embedded DuckDB query engine.
type DuckDBConfig struct {
	// MemoryLimit uses DuckDB syntax, for example "2GB" or "50%".
	MemoryLimit string
	// Threads caps DuckDB worker threads per database.
	Threads int
	// TempDir is the spill directory. It must be writable.
	TempDir string
	// MaxTempDirectorySize bounds DuckDB spill files, for example "50GB".
	MaxTempDirectorySize string
	// MaxOpenConns bounds simultaneous DuckDB connections.
	MaxOpenConns int
	// MaxConcurrentQueries bounds simultaneous queries; <= 0 means MaxOpenConns.
	MaxConcurrentQueries int
	// FallbackOnError lets the log engine use the pure-Go scanner when DuckDB
	// cannot execute a query. Fallback never broadens the query scope.
	FallbackOnError bool
}

// DuckDBStats is a small operational snapshot for /metrics and the UI.
type DuckDBStats struct {
	Enabled   bool
	Memory    string
	Threads   int
	TempDir   string
	Queries   uint64
	Errors    uint64
	Fallbacks uint64
}

type duckQueryBackend interface {
	StreamLogs(ctx context.Context, query string, service string, from, to time.Time, limit int, yield func(LogRecord) error) error
	StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error
	StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error
	Ping(ctx context.Context) error
	QueryDuckDB(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	Stats() DuckDBStats
	Close() error
}
