package logengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	defaultDuckDBSQLRows  = 1_000
	maxDuckDBSQLRows      = 5_000
	maxDuckDBSQLRange     = 7 * 24 * time.Hour
	maxDuckDBSQLFiles     = 10_000
	maxDuckDBSQLValueSize = 16 * 1024
	maxDuckDBSQLBytes     = 8 << 20
)

// DuckDBSQLResult is the row-oriented result of an ad-hoc DuckDB query.
type DuckDBSQLResult struct {
	Dataset    string   `json:"dataset"`
	Columns    []string `json:"columns"`
	Rows       [][]any  `json:"rows"`
	RowCount   int      `json:"row_count"`
	Truncated  bool     `json:"truncated"`
	DurationMs int64    `json:"duration_ms"`
}

var (
	reDuckDBSQLStart = regexp.MustCompile(`(?is)^\s*(select|with)\b`)
)

// QueryDuckDBSQL runs a read-only SELECT against a partition-scoped view of
// the local Parquet chunks. The view is exposed as `logs`, `metrics`, or
// `traces`; the caller's QueryScope is applied inside the view before the
// user's SQL is evaluated.
func (e *LogEngine) QueryDuckDBSQL(ctx context.Context, dataset, query string, from, to time.Time, limit int, scope QueryScope) (DuckDBSQLResult, error) {
	started := time.Now()
	result := DuckDBSQLResult{Dataset: strings.ToLower(strings.TrimSpace(dataset)), Rows: [][]any{}}
	if e == nil || e.duck == nil {
		return result, ErrDuckDBUnavailable
	}
	var err error
	result.Dataset, query, err = normalizeDuckDBSQL(result.Dataset, query)
	if err != nil {
		return result, err
	}
	if err := e.validateDuckDBSQLAST(ctx, query, result.Dataset); err != nil {
		return result, err
	}
	if limit <= 0 {
		limit = defaultDuckDBSQLRows
	}
	if limit > maxDuckDBSQLRows {
		limit = maxDuckDBSQLRows
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	if !to.After(from) {
		return result, fmt.Errorf("query time range must be non-empty")
	}
	if to.Sub(from) > maxDuckDBSQLRange {
		return result, fmt.Errorf("query time range is limited to %s", maxDuckDBSQLRange)
	}

	files, err := e.planDuckDBSQLFiles(ctx, result.Dataset, from, to)
	if err != nil {
		return result, err
	}
	view, args := buildSQLDataset(files, result.Dataset, from, to, scope)
	statement := "WITH " + result.Dataset + " AS (" + view + ") SELECT * FROM (" + query + ") AS doctor_sql LIMIT ?"
	args = append(args, limit+1)

	rows, err := e.duck.QueryDuckDB(ctx, statement, args...)
	if err != nil {
		return result, fmt.Errorf("duckdb sql query: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return result, fmt.Errorf("duckdb sql columns: %w", err)
	}
	result.Columns = columns
	encodedBytes := 0
	for rows.Next() {
		if len(result.Rows) >= limit {
			result.Truncated = true
			break
		}
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return result, fmt.Errorf("scan duckdb sql row: %w", err)
		}
		rowBytes := 8
		for i, value := range values {
			values[i] = normalizeDuckDBSQLValue(value)
			rowBytes += len(fmt.Sprint(values[i])) + 8
		}
		if encodedBytes+rowBytes > maxDuckDBSQLBytes {
			result.Truncated = true
			break
		}
		encodedBytes += rowBytes
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("duckdb sql rows: %w", err)
	}
	result.RowCount = len(result.Rows)
	result.DurationMs = time.Since(started).Milliseconds()
	return result, nil
}

func normalizeDuckDBSQL(dataset, query string) (string, string, error) {
	dataset = strings.ToLower(strings.TrimSpace(dataset))
	switch dataset {
	case "logs", "metrics", "traces":
	default:
		return dataset, query, fmt.Errorf("unsupported dataset %q (want logs, metrics, or traces)", dataset)
	}

	q := strings.TrimSpace(query)
	if q == "" {
		return dataset, q, fmt.Errorf("query is empty")
	}
	if len(q) > 32*1024 {
		return dataset, q, fmt.Errorf("query exceeds the 32 KiB limit")
	}
	if strings.HasSuffix(q, ";") {
		q = strings.TrimSpace(strings.TrimSuffix(q, ";"))
	}
	if strings.Contains(q, ";") {
		return dataset, q, fmt.Errorf("only a single statement is allowed")
	}
	if !reDuckDBSQLStart.MatchString(q) {
		return dataset, q, fmt.Errorf("only SELECT and WITH queries are allowed")
	}
	if strings.Contains(q, "?") {
		return dataset, q, fmt.Errorf("query placeholders are not supported")
	}
	reDatasetCTE := regexp.MustCompile(`(?is)\b(?:with|,)\s*` + regexp.QuoteMeta(dataset) + `\s+as\s*\(`)
	if reDatasetCTE.MatchString(q) {
		return dataset, q, fmt.Errorf("query may not redefine the %q dataset", dataset)
	}
	return dataset, q, nil
}

// validateDuckDBSQLAST asks DuckDB to parse the statement and rejects any
// table that is not the selected dataset or a CTE alias, plus every table
// function. This is the authoritative allow-list; the regex checks in
// normalizeDuckDBSQL are a cheap pre-filter and provide clearer errors.
func (e *LogEngine) validateDuckDBSQLAST(ctx context.Context, query, dataset string) error {
	rows, err := e.duck.QueryDuckDB(ctx, "SELECT json_serialize_sql(CAST(? AS VARCHAR)) AS ast", query)
	if err != nil {
		return fmt.Errorf("parse SQL: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return fmt.Errorf("parse SQL: empty statement")
	}
	var raw any
	if err := rows.Scan(&raw); err != nil {
		return fmt.Errorf("parse SQL result: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("parse SQL rows: %w", err)
	}

	var ast map[string]any
	switch value := raw.(type) {
	case map[string]any:
		ast = value
	case string:
		if err := json.Unmarshal([]byte(value), &ast); err != nil {
			return fmt.Errorf("parse SQL AST: %w", err)
		}
	case []byte:
		if err := json.Unmarshal(value, &ast); err != nil {
			return fmt.Errorf("parse SQL AST: %w", err)
		}
	default:
		return fmt.Errorf("parse SQL AST: unexpected %T", raw)
	}

	if failed, _ := ast["error"].(bool); failed {
		message, _ := ast["error_message"].(string)
		if strings.TrimSpace(message) == "" {
			message = "query is not a valid SELECT statement"
		}
		return fmt.Errorf("%s", message)
	}
	statements, _ := ast["statements"].([]any)
	if len(statements) != 1 {
		return fmt.Errorf("only a single statement is allowed")
	}

	ctes := make(map[string]struct{})
	walkDuckDBSQLAST(ast, func(node map[string]any) {
		cteMap, ok := node["cte_map"].(map[string]any)
		if !ok {
			return
		}
		entries, _ := cteMap["map"].([]any)
		for _, entry := range entries {
			record, _ := entry.(map[string]any)
			key, _ := record["key"].(string)
			if key != "" {
				ctes[strings.ToLower(key)] = struct{}{}
			}
		}
	})
	if _, ok := ctes[dataset]; ok {
		return fmt.Errorf("query may not redefine the %q dataset", dataset)
	}

	var validationErr error
	walkDuckDBSQLAST(ast, func(node map[string]any) {
		if validationErr != nil {
			return
		}
		switch node["type"] {
		case "TABLE_FUNCTION":
			function, _ := node["function"].(map[string]any)
			name, _ := function["function_name"].(string)
			if name == "" {
				name = "table function"
			}
			validationErr = fmt.Errorf("table function %q is not allowed", name)
		case "BASE_TABLE":
			catalog, _ := node["catalog_name"].(string)
			schema, _ := node["schema_name"].(string)
			table, _ := node["table_name"].(string)
			if catalog != "" || schema != "" {
				validationErr = fmt.Errorf("catalog or schema table %q is not allowed", table)
				return
			}
			if table == "" {
				validationErr = fmt.Errorf("unnamed table is not allowed")
				return
			}
			table = strings.ToLower(table)
			if table != dataset {
				if _, ok := ctes[table]; !ok {
					validationErr = fmt.Errorf("query may only reference the %q dataset", dataset)
				}
			}
		}
		if function, _ := node["function_name"].(string); isDangerousDuckDBFunction(function) {
			validationErr = fmt.Errorf("function %q is not allowed", function)
		}
	})
	return validationErr
}

func walkDuckDBSQLAST(value any, visit func(map[string]any)) {
	switch typed := value.(type) {
	case map[string]any:
		visit(typed)
		for _, child := range typed {
			walkDuckDBSQLAST(child, visit)
		}
	case []any:
		for _, child := range typed {
			walkDuckDBSQLAST(child, visit)
		}
	}
}

func isDangerousDuckDBFunction(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read_parquet", "read_blob", "read_text", "read_file", "read_xlsx",
		"parquet_scan", "parquet_metadata", "parquet_schema", "sniff_csv",
		"read_csv", "read_csv_auto", "read_json", "read_json_auto", "read_ndjson",
		"glob", "httpfs", "sqlite_scan", "postgres_scan", "mysql_scan",
		"delta_scan", "iceberg_scan", "st_read", "pg_read_file", "pg_read_binary_file",
		"load_extension", "install_extension":
		return true
	default:
		return false
	}
}

func (e *LogEngine) planDuckDBSQLFiles(ctx context.Context, dataset string, from, to time.Time) ([]string, error) {
	var signal Signal
	switch dataset {
	case "logs":
		signal = SignalLogs
	case "metrics":
		signal = SignalMetrics
	case "traces":
		signal = SignalTraces
	default:
		return nil, fmt.Errorf("unsupported dataset %q", dataset)
	}
	chunks, err := e.store.ListChunkPaths(ctx, signal, from, to)
	if err != nil {
		return nil, fmt.Errorf("list %s chunks: %w", dataset, err)
	}
	if len(chunks) > maxDuckDBSQLFiles {
		return nil, fmt.Errorf("query would scan %d chunks; narrow the time range", len(chunks))
	}
	files := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		local, err := e.store.EnsureLocalParquet(ctx, chunkDataPath(signal, chunk))
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, fmt.Errorf("load %s chunk: %w", dataset, err)
		}
		files = append(files, local)
	}
	return files, nil
}

func normalizeDuckDBSQLValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		return truncateDuckDBSQLString(string(typed))
	case string:
		return truncateDuckDBSQLString(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case sql.RawBytes:
		return truncateDuckDBSQLString(string(typed))
	default:
		return value
	}
}

func truncateDuckDBSQLString(value string) string {
	if len(value) <= maxDuckDBSQLValueSize {
		return value
	}
	return value[:maxDuckDBSQLValueSize] + "… [truncated]"
}
