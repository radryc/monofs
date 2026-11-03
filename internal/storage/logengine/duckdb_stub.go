//go:build !cgo || !duckdb

package logengine

// EnableDuckDB is a no-op error in builds without DuckDB support.
func (e *LogEngine) EnableDuckDB(cfg DuckDBConfig) error {
	return ErrDuckDBUnavailable
}
