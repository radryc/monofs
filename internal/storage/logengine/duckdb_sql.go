package logengine

import (
	"fmt"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage/logquery"
)

// duckSQLExpr is a SQL fragment with its bound arguments.
type duckSQLExpr struct {
	sql  string
	args []any
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func duckParquetSource(files []string) string {
	return duckParquetSourceWithUnion(files, true)
}

// duckMetricParquetSource omits union_by_name because every metric chunk uses
// the current metric schema. This avoids per-file schema unification cost.
func duckMetricParquetSource(files []string) string {
	return duckParquetSourceWithUnion(files, false)
}

func duckParquetSourceWithUnion(files []string, unionByName bool) string {
	quoted := make([]string, 0, len(files))
	for _, file := range files {
		quoted = append(quoted, sqlQuote(file))
	}
	options := "filename = true, hive_partitioning = true"
	if unionByName {
		options = "union_by_name = true, " + options
	}
	return "read_parquet([" + strings.Join(quoted, ", ") + "], " + options + ")"
}

func jsonPathExpr(column, name string) duckSQLExpr {
	keys := []string{name}
	if alt := strings.ReplaceAll(name, "_", "."); alt != name {
		keys = append(keys, alt)
	}
	if alt := strings.ReplaceAll(name, ".", "_"); alt != name {
		keys = append(keys, alt)
	}
	parts := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("nullif(json_extract_string(%s, ?), '')", column))
		args = append(args, "$."+jsonPathKey(key))
	}
	if len(parts) == 1 {
		return duckSQLExpr{sql: parts[0], args: args}
	}
	return duckSQLExpr{sql: "coalesce(" + strings.Join(parts, ", ") + ")", args: args}
}

func jsonPathKey(name string) string {
	// Quote every key so dots, spaces and reserved characters remain literal.
	return `"` + strings.ReplaceAll(name, `"`, `\"`) + `"`
}

func partitionExpr(jsonColumn string) duckSQLExpr {
	parts := []string{
		"nullif(partition_name, '')",
		// Use the file path rather than the Hive "partition" column: encrypted
		// chunks are materialized into a flat cache, where hive_partitioning
		// cannot derive the column. filename is always available.
		"nullif(regexp_extract(filename, 'partition=([^/]+)', 1), '')",
		"nullif(json_extract_string(" + jsonColumn + `, ?), '')`,
		"nullif(json_extract_string(" + jsonColumn + `, ?), '')`,
	}
	return duckSQLExpr{
		sql:  "coalesce(" + strings.Join(parts, ", ") + ")",
		args: []any{`$."guardian.partition"`, "$.guardian_partition"},
	}
}

func appendPredicate(where *[]string, args *[]any, expr duckSQLExpr, op string, value string) {
	valueExpr := "coalesce(" + expr.sql + ", '')"
	switch op {
	case "=":
		*where = append(*where, valueExpr+" = ?")
		*args = append(*args, expr.args...)
		*args = append(*args, value)
	case "!=":
		*where = append(*where, valueExpr+" <> ?")
		*args = append(*args, expr.args...)
		*args = append(*args, value)
	case "=~":
		*where = append(*where, "regexp_matches("+valueExpr+", ?)")
		*args = append(*args, expr.args...)
		*args = append(*args, value)
	case "!~":
		*where = append(*where, "NOT regexp_matches("+valueExpr+", ?)")
		*args = append(*args, expr.args...)
		*args = append(*args, value)
	}
}

func logMatcherExpr(name string) duckSQLExpr {
	switch name {
	case "service", "service_name":
		return duckSQLExpr{sql: "service"}
	case "level", "severity_text":
		return duckSQLExpr{sql: "level"}
	case "trace_id":
		return duckSQLExpr{sql: "trace_id"}
	case "span_id":
		return duckSQLExpr{sql: "span_id"}
	case "body", "raw_message":
		return duckSQLExpr{sql: "raw_message"}
	default:
		return jsonPathExpr("labels_json", name)
	}
}

func appendScope(where *[]string, args *[]any, expr duckSQLExpr, scope QueryScope) {
	if !scope.Restricted {
		return
	}
	if len(scope.Partitions) == 0 {
		*where = append(*where, "FALSE")
		return
	}
	placeholders := make([]string, 0, len(scope.Partitions))
	for range scope.Partitions {
		placeholders = append(placeholders, "?")
	}
	*where = append(*where, expr.sql+" IN ("+strings.Join(placeholders, ", ")+")")
	*args = append(*args, expr.args...)
	for _, partition := range scope.Partitions {
		*args = append(*args, partition)
	}
}

func withServiceMatcher(query logquery.Query, service string) logquery.Query {
	if strings.TrimSpace(service) == "" {
		return query
	}
	query.Matchers = append(query.Matchers, logquery.Matcher{Name: "service", Op: "=", Value: service})
	return query
}

func buildLogDuckSQL(files []string, query logquery.Query, service string, from, to time.Time, limit int, scope QueryScope) (string, []any, error) {
	query = withServiceMatcher(query, service)
	where := make([]string, 0, len(query.Matchers)+len(query.LineFilters)+2)
	part := partitionExpr("labels_json")
	args := append([]any(nil), part.args...)

	if !from.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, from.UTC().UnixNano())
	}
	if !to.IsZero() {
		where = append(where, "timestamp <= ?")
		args = append(args, to.UTC().UnixNano())
	}
	for _, matcher := range query.Matchers {
		value := matcher.Value
		if matcher.Op == "=~" || matcher.Op == "!~" {
			value = anchorLabelRegex(value)
		}
		appendPredicate(&where, &args, logMatcherExpr(matcher.Name), matcher.Op, value)
	}
	for _, filter := range query.LineFilters {
		expr := duckSQLExpr{sql: "raw_message"}
		switch filter.Op {
		case "|=":
			where = append(where, "contains("+expr.sql+", ?)")
			args = append(args, filter.Value)
		case "!=":
			where = append(where, "NOT contains("+expr.sql+", ?)")
			args = append(args, filter.Value)
		case "|~":
			where = append(where, "regexp_matches("+expr.sql+", ?)")
			args = append(args, filter.Value)
		case "!~":
			where = append(where, "NOT regexp_matches("+expr.sql+", ?)")
			args = append(args, filter.Value)
		}
	}
	appendScope(&where, &args, partitionExpr("labels_json"), scope)

	sql := "SELECT timestamp, level, service, trace_id, span_id, raw_message, labels_json, " +
		part.sql + " AS partition FROM " + duckParquetSource(files)
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY timestamp DESC"
	if limit > 0 {
		sql += " LIMIT ?"
		args = append(args, limit)
	}
	return sql, args, nil
}

func buildMetricDuckSQL(files []string, query MetricQuery, from, to time.Time) (string, []any, error) {
	where := make([]string, 0, len(query.LabelMatchers)+4)
	part := partitionExpr("labels_json")
	args := append([]any(nil), part.args...)
	if !from.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, from.UTC().UnixNano())
	}
	if !to.IsZero() {
		where = append(where, "timestamp <= ?")
		args = append(args, to.UTC().UnixNano())
	}
	if query.Service != "" {
		where = append(where, "service = ?")
		args = append(args, query.Service)
	}
	if query.MetricName != "" {
		where = append(where, "metric_name = ?")
		args = append(args, query.MetricName)
	}
	for _, matcher := range query.LabelMatchers {
		expr := jsonPathExpr("labels_json", matcher.Name)
		op := "="
		switch matcher.Type {
		case MetricMatchEqual:
			op = "="
		case MetricMatchNotEqual:
			op = "!="
		case MetricMatchRegexp:
			op = "=~"
		case MetricMatchNotRegexp:
			op = "!~"
		default:
			return "", nil, fmt.Errorf("unsupported metric matcher type %q", matcher.Type)
		}
		appendPredicate(&where, &args, expr, op, matcher.Value)
	}

	// Metric consumers (PromQL, dashboards) aggregate by series and do not
	// depend on row order. Skipping the sort lets DuckDB stream results without
	// materializing the full match set just to order it.
	sql := "SELECT timestamp, start_time, service, metric_name, value, metric_type, temporality, labels_json, " +
		part.sql + " AS partition FROM " + duckMetricParquetSource(files)
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	return sql, args, nil
}

func buildTraceDuckSQL(files []string, traceID, service string, from, to time.Time, limit int, scope QueryScope) (string, []any, error) {
	where := make([]string, 0, 6)
	part := partitionExpr("attributes_json")
	args := append([]any(nil), part.args...)
	if !from.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, from.UTC().UnixNano())
	}
	if !to.IsZero() {
		where = append(where, "timestamp <= ?")
		args = append(args, to.UTC().UnixNano())
	}
	if traceID != "" {
		where = append(where, "trace_id = ?")
		args = append(args, traceID)
	}
	if service != "" {
		where = append(where, "service = ?")
		args = append(args, service)
	}
	appendScope(&where, &args, partitionExpr("attributes_json"), scope)

	sql := "SELECT timestamp, end_time, trace_id, span_id, parent_span_id, service, name, status_code, attributes_json, " +
		part.sql + " AS partition FROM " + duckParquetSource(files)
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY timestamp DESC"
	if limit > 0 {
		sql += " LIMIT ?"
		args = append(args, limit)
	}
	return sql, args, nil
}

// buildSQLDataset renders the read-only view exposed to the DuckDB SQL
// console. The view applies the requested time range and the caller's
// partition scope so user SQL cannot widen access by writing its own WHERE
// clause. Empty file sets return a typed empty relation instead of calling
// read_parquet with an empty list.
func buildSQLDataset(files []string, dataset string, from, to time.Time, scope QueryScope) (string, []any) {
	if len(files) == 0 {
		return emptySQLDataset(dataset), nil
	}
	source := duckParquetSource(files)
	where := make([]string, 0, 4)
	args := make([]any, 0, 4)

	switch dataset {
	case "logs":
		part := partitionExpr("labels_json")
		args = append(args, part.args...)
		if !from.IsZero() {
			where = append(where, "timestamp >= ?")
			args = append(args, from.UTC().UnixNano())
		}
		if !to.IsZero() {
			where = append(where, "timestamp <= ?")
			args = append(args, to.UTC().UnixNano())
		}
		appendScope(&where, &args, part, scope)
		sql := "SELECT timestamp, to_timestamp(timestamp / 1000000000.0) AS time, level, service, trace_id, span_id, raw_message, labels_json, " +
			part.sql + " AS partition FROM " + source
		if len(where) > 0 {
			sql += " WHERE " + strings.Join(where, " AND ")
		}
		return sql, args

	case "metrics":
		source = duckMetricParquetSource(files)
		part := partitionExpr("labels_json")
		args = append(args, part.args...)
		if !from.IsZero() {
			where = append(where, "timestamp >= ?")
			args = append(args, from.UTC().UnixNano())
		}
		if !to.IsZero() {
			where = append(where, "timestamp <= ?")
			args = append(args, to.UTC().UnixNano())
		}
		appendScope(&where, &args, part, scope)
		sql := "SELECT timestamp, to_timestamp(timestamp / 1000000000.0) AS time, start_time, service, metric_name, value, metric_type, temporality, labels_json, " +
			part.sql + " AS partition FROM " + source
		if len(where) > 0 {
			sql += " WHERE " + strings.Join(where, " AND ")
		}
		return sql, args

	case "traces":
		part := partitionExpr("attributes_json")
		args = append(args, part.args...)
		if !from.IsZero() {
			where = append(where, "timestamp >= ?")
			args = append(args, from.UTC().UnixNano())
		}
		if !to.IsZero() {
			where = append(where, "timestamp <= ?")
			args = append(args, to.UTC().UnixNano())
		}
		appendScope(&where, &args, part, scope)
		sql := "SELECT timestamp, to_timestamp(timestamp / 1000000000.0) AS time, end_time, " +
			"to_timestamp(end_time / 1000000000.0) AS end_time_ts, trace_id, span_id, parent_span_id, service, name, status_code, attributes_json, " +
			part.sql + " AS partition FROM " + source
		if len(where) > 0 {
			sql += " WHERE " + strings.Join(where, " AND ")
		}
		return sql, args
	}
	return emptySQLDataset(dataset), nil
}

func emptySQLDataset(dataset string) string {
	switch dataset {
	case "logs":
		return "SELECT CAST(NULL AS BIGINT) AS timestamp, CAST(NULL AS TIMESTAMP) AS time, " +
			"CAST(NULL AS VARCHAR) AS level, CAST(NULL AS VARCHAR) AS service, CAST(NULL AS VARCHAR) AS trace_id, " +
			"CAST(NULL AS VARCHAR) AS span_id, CAST(NULL AS VARCHAR) AS raw_message, CAST(NULL AS VARCHAR) AS labels_json, " +
			"CAST(NULL AS VARCHAR) AS partition WHERE FALSE"
	case "metrics":
		return "SELECT CAST(NULL AS BIGINT) AS timestamp, CAST(NULL AS TIMESTAMP) AS time, CAST(NULL AS BIGINT) AS start_time, " +
			"CAST(NULL AS VARCHAR) AS service, CAST(NULL AS VARCHAR) AS metric_name, CAST(NULL AS DOUBLE) AS value, " +
			"CAST(NULL AS VARCHAR) AS metric_type, CAST(NULL AS VARCHAR) AS temporality, " +
			"CAST(NULL AS VARCHAR) AS labels_json, CAST(NULL AS VARCHAR) AS partition WHERE FALSE"
	case "traces":
		return "SELECT CAST(NULL AS BIGINT) AS timestamp, CAST(NULL AS TIMESTAMP) AS time, " +
			"CAST(NULL AS BIGINT) AS end_time, CAST(NULL AS TIMESTAMP) AS end_time_ts, " +
			"CAST(NULL AS VARCHAR) AS trace_id, CAST(NULL AS VARCHAR) AS span_id, CAST(NULL AS VARCHAR) AS parent_span_id, " +
			"CAST(NULL AS VARCHAR) AS service, CAST(NULL AS VARCHAR) AS name, CAST(NULL AS VARCHAR) AS status_code, " +
			"CAST(NULL AS VARCHAR) AS attributes_json, CAST(NULL AS VARCHAR) AS partition WHERE FALSE"
	default:
		return "SELECT CAST(NULL AS BIGINT) AS timestamp WHERE FALSE"
	}
}
