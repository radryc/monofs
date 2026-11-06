package clickhouse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage/logengine"
)

const (
	defaultSQLRows = 1_000
	maxSQLRows     = 5_000
	maxSQLRange    = 7 * 24 * time.Hour
	maxSQLBytes    = 8 << 20
	maxSQLValueLen = 16 * 1024
)

var reSQLStart = regexp.MustCompile(`(?is)^\s*(select|with)\b`)

// QueryDuckDBSQL runs the Doctor SQL console against ClickHouse. The API kept
// its historical "DuckDB SQL" name because the request and response shape are
// shared with the logengine backend. The user query sees the same `logs`,
// `metrics`, and `traces` views, and the caller's partition scope is applied
// inside the view before the statement runs.
func (s *TraceStore) QueryDuckDBSQL(ctx context.Context, dataset, query string, from, to time.Time, limit int, scope logengine.QueryScope) (logengine.DuckDBSQLResult, error) {
	started := time.Now()
	result := logengine.DuckDBSQLResult{Dataset: strings.ToLower(strings.TrimSpace(dataset)), Rows: [][]any{}}

	dataset, query, err := normalizeClickHouseSQL(dataset, query)
	if err != nil {
		return result, err
	}
	if err := s.validateSQLAST(ctx, query, dataset); err != nil {
		return result, err
	}
	result.Dataset = dataset
	if limit <= 0 {
		limit = defaultSQLRows
	}
	if limit > maxSQLRows {
		limit = maxSQLRows
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
	if to.Sub(from) > maxSQLRange {
		return result, fmt.Errorf("query time range is limited to %s", maxSQLRange)
	}

	view, params := s.sqlDataset(dataset, from, to, scope)
	statement := "WITH " + dataset + " AS (" + view + ") SELECT * FROM (" + query + ") AS doctor_sql LIMIT " +
		strconv.Itoa(limit+1) + " FORMAT JSON"
	params.Set("readonly", "1")
	params.Set("max_execution_time", "20")
	params.Set("output_format_json_quote_64bit_integers", "0")

	response, err := s.do(ctx, statement, params, nil)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()

	var payload struct {
		Meta []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"meta"`
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return result, fmt.Errorf("decode clickhouse sql result: %w", err)
	}

	result.Columns = make([]string, len(payload.Meta))
	for i, column := range payload.Meta {
		result.Columns[i] = column.Name
	}
	encodedBytes := 0
	for _, rawRow := range payload.Data {
		if len(result.Rows) >= limit {
			result.Truncated = true
			break
		}
		row := make([]any, len(result.Columns))
		rowBytes := 8
		for i, column := range payload.Meta {
			raw, ok := rawRow[column.Name]
			if !ok || len(raw) == 0 || string(raw) == "null" {
				row[i] = nil
				continue
			}
			value, err := decodeClickHouseJSONValue(raw, column.Type)
			if err != nil {
				return result, fmt.Errorf("decode clickhouse sql column %q: %w", column.Name, err)
			}
			value = normalizeClickHouseSQLValue(value)
			row[i] = value
			rowBytes += len(fmt.Sprint(value)) + 8
		}
		if encodedBytes+rowBytes > maxSQLBytes {
			result.Truncated = true
			break
		}
		encodedBytes += rowBytes
		result.Rows = append(result.Rows, row)
	}
	result.RowCount = len(result.Rows)
	result.DurationMs = time.Since(started).Milliseconds()
	return result, nil
}

func normalizeClickHouseSQL(dataset, query string) (string, string, error) {
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
	if !reSQLStart.MatchString(q) {
		return dataset, q, fmt.Errorf("only SELECT and WITH queries are allowed")
	}
	if strings.Contains(q, "?") {
		return dataset, q, fmt.Errorf("query placeholders are not supported")
	}
	reDatasetCTE := regexp.MustCompile(`(?is)\b(?:with|,)\s*` + regexp.QuoteMeta(dataset) + `\s+as\s*\(`)
	if reDatasetCTE.MatchString(q) {
		return dataset, q, fmt.Errorf("query may not redefine the %q dataset", dataset)
	}
	lower := strings.ToLower(q)
	for _, marker := range []string{"into outfile", "into dumpfile"} {
		if strings.Contains(lower, marker) {
			return dataset, q, fmt.Errorf("%q is not allowed", marker)
		}
	}
	return dataset, q, nil
}

// validateSQLAST uses ClickHouse's parser to keep the console confined to the
// selected dataset. The user's statement may reference the dataset and its own
// CTEs, but not raw signal tables, schema-qualified tables, or table functions
// that could bypass the partition-scoped view.
func (s *TraceStore) validateSQLAST(ctx context.Context, query, dataset string) error {
	params := url.Values{}
	params.Set("readonly", "1")
	params.Set("max_execution_time", "20")
	response, err := s.do(ctx, "EXPLAIN AST "+query, params, nil)
	if err != nil {
		return fmt.Errorf("parse SQL: %w", err)
	}
	defer response.Body.Close()

	allowed := strings.ToLower(strings.TrimSpace(dataset))
	rawTables := map[string]struct{}{"logs": {}, "metrics": {}, "spans": {}}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 4<<20))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "TableIdentifier "):
			identifier := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "TableIdentifier ")))
			identifier = strings.Trim(identifier, "`\"")
			if strings.Contains(identifier, ".") {
				return fmt.Errorf("query may only reference the %q dataset", dataset)
			}
			if _, isRaw := rawTables[identifier]; isRaw && identifier != allowed {
				return fmt.Errorf("query may only reference the %q dataset", dataset)
			}
		case strings.HasPrefix(line, "TableFunction"):
			return fmt.Errorf("table functions are not allowed")
		case strings.HasPrefix(line, "Function "):
			fields := strings.Fields(strings.TrimPrefix(line, "Function "))
			if len(fields) > 0 && isBlockedSQLFunction(fields[0]) {
				return fmt.Errorf("function %q is not allowed", fields[0])
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("parse SQL AST: %w", err)
	}
	return nil
}

func isBlockedSQLFunction(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "file", "url", "s3", "remote", "remoteSecure", "cluster",
		"mysql", "postgresql", "jdbc", "odbc", "hdfs", "azureBlobStorage",
		"deltaLake", "iceberg", "input", "executable":
		return true
	default:
		return false
	}
}

// sqlDataset renders the dataset view exposed to the SQL console. ClickHouse
// stores typed columns, while the logengine/DuckDB view exposes nanosecond
// timestamps, RFC3339-ish time strings, and JSON strings for labels/attributes.
// Keep those shapes stable so the same Doctor queries work on both engines.
func (s *TraceStore) sqlDataset(dataset string, from, to time.Time, scope logengine.QueryScope) (string, url.Values) {
	params := url.Values{}
	params.Set("param_tenant", s.cfg.Tenant)
	where := []string{"l.tenant = {tenant:String}"}
	if !from.IsZero() {
		params.Set("param_from", formatClickHouseTime(from))
		where = append(where, "l.timestamp >= toDateTime64({from:String}, 9, 'UTC')")
	}
	if !to.IsZero() {
		params.Set("param_to", formatClickHouseTime(to))
		where = append(where, "l.timestamp <= toDateTime64({to:String}, 9, 'UTC')")
	}

	var partitionExpr string
	switch dataset {
	case "logs", "metrics":
		partitionExpr = "coalesce(nullIf(l.labels['guardian.partition'], ''), nullIf(l.labels['guardian_partition'], ''), '')"
	case "traces":
		partitionExpr = "coalesce(nullIf(l.attributes['guardian.partition'], ''), nullIf(l.attributes['guardian_partition'], ''), '')"
	}
	if scope.Restricted {
		if len(scope.Partitions) == 0 {
			where = append(where, "0")
		} else {
			placeholders := make([]string, 0, len(scope.Partitions))
			for i, partition := range scope.Partitions {
				name := "p" + strconv.Itoa(i)
				params.Set("param_"+name, partition)
				placeholders = append(placeholders, "{"+name+":String}")
			}
			where = append(where, partitionExpr+" IN ("+strings.Join(placeholders, ", ")+")")
		}
	}
	whereSQL := strings.Join(where, " AND ")

	timeExpr := func(column string) string {
		return "formatDateTime(l." + column + ", '%Y-%m-%dT%H:%i:%S.%fZ', 'UTC')"
	}
	switch dataset {
	case "logs":
		return "SELECT toUnixTimestamp64Nano(l.timestamp) AS timestamp, " +
			timeExpr("timestamp") + " AS time, l.level AS level, l.service AS service, " +
			"l.trace_id AS trace_id, l.span_id AS span_id, l.raw_message AS raw_message, " +
			"toJSONString(l.labels) AS labels_json, " + partitionExpr + " AS partition " +
			"FROM " + s.tableFor(s.cfg.LogsTable) + " AS l WHERE " + whereSQL, params
	case "metrics":
		return "SELECT toUnixTimestamp64Nano(l.timestamp) AS timestamp, " +
			timeExpr("timestamp") + " AS time, toUnixTimestamp64Nano(l.start_time) AS start_time, " +
			"l.service AS service, l.metric_name AS metric_name, l.value AS value, " +
			"l.metric_type AS metric_type, l.temporality AS temporality, " +
			"toJSONString(l.labels) AS labels_json, " + partitionExpr + " AS partition " +
			"FROM " + s.tableFor(s.cfg.MetricsTable) + " AS l WHERE " + whereSQL, params
	case "traces":
		return "SELECT toUnixTimestamp64Nano(l.timestamp) AS timestamp, " +
			timeExpr("timestamp") + " AS time, toUnixTimestamp64Nano(l.end_time) AS end_time, " +
			timeExpr("end_time") + " AS end_time_ts, l.trace_id AS trace_id, l.span_id AS span_id, " +
			"l.parent_span_id AS parent_span_id, l.service AS service, l.name AS name, " +
			"l.status_code AS status_code, toJSONString(l.attributes) AS attributes_json, " +
			partitionExpr + " AS partition FROM " + s.table() + " AS l WHERE " + whereSQL, params
	default:
		return "SELECT NULL AS timestamp WHERE 0", params
	}
}

func decodeClickHouseJSONValue(raw json.RawMessage, typ string) (any, error) {
	base := clickHouseBaseType(typ)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	number, isNumber := value.(json.Number)
	switch {
	case strings.HasPrefix(base, "Int"):
		if isNumber {
			if i, err := number.Int64(); err == nil {
				return i, nil
			}
		}
	case strings.HasPrefix(base, "UInt"):
		if isNumber {
			if u, err := strconv.ParseUint(number.String(), 10, 64); err == nil {
				return u, nil
			}
		}
	case strings.HasPrefix(base, "Float"), strings.HasPrefix(base, "Decimal"):
		if isNumber {
			if f, err := number.Float64(); err == nil {
				return f, nil
			}
		}
	case strings.HasPrefix(base, "DateTime64"), strings.HasPrefix(base, "DateTime"):
		if text, ok := value.(string); ok {
			if parsed, err := parseClickHouseDateTime(text); err == nil {
				return parsed.UTC().Format(time.RFC3339Nano), nil
			}
		}
	case strings.HasPrefix(base, "Date"):
		if text, ok := value.(string); ok {
			if parsed, err := time.Parse("2006-01-02", text); err == nil {
				return parsed.UTC().Format(time.RFC3339Nano), nil
			}
		}
	}
	return value, nil
}

func clickHouseBaseType(typ string) string {
	typ = strings.TrimSpace(typ)
	for {
		stripped := false
		for _, wrapper := range []string{"Nullable(", "LowCardinality("} {
			if strings.HasPrefix(typ, wrapper) && strings.HasSuffix(typ, ")") {
				typ = strings.TrimSpace(typ[len(wrapper) : len(typ)-1])
				stripped = true
				break
			}
		}
		if !stripped {
			return typ
		}
	}
}

func parseClickHouseDateTime(value string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized datetime %q", value)
}

func normalizeClickHouseSQLValue(value any) any {
	if text, ok := value.(string); ok && len(text) > maxSQLValueLen {
		return text[:maxSQLValueLen] + "… [truncated]"
	}
	return value
}
