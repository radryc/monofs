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
	quoted := make([]string, 0, len(files))
	for _, file := range files {
		quoted = append(quoted, sqlQuote(file))
	}
	return "read_parquet([" + strings.Join(quoted, ", ") + "], union_by_name = true, hive_partitioning = true)"
}

func jsonPathExpr(column, name string) duckSQLExpr {
	keys := []string{name}
	if alt := strings.ReplaceAll(name, "_", "."); alt != name {
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

func partitionExpr(partitionColumn, jsonColumn string) duckSQLExpr {
	parts := []string{
		"nullif(partition_name, '')",
		"nullif(" + partitionColumn + ", '')",
		"nullif(json_extract_string(" + jsonColumn + `, ?), '')`,
		"nullif(json_extract_string(" + jsonColumn + `, ?), '')`,
	}
	return duckSQLExpr{
		sql:  "coalesce(" + strings.Join(parts, ", ") + ")",
		args: []any{`$."guardian.partition"`, "$.guardian_partition"},
	}
}

func appendPredicate(where *[]string, args *[]any, expr duckSQLExpr, op string, value string) {
	switch op {
	case "=":
		*where = append(*where, expr.sql+" = ?")
		*args = append(*args, expr.args...)
		*args = append(*args, value)
	case "!=":
		*where = append(*where, "("+expr.sql+" IS NULL OR "+expr.sql+" <> ?)")
		*args = append(*args, expr.args...)
		*args = append(*args, expr.args...)
		*args = append(*args, value)
	case "=~":
		*where = append(*where, "("+expr.sql+" IS NOT NULL AND regexp_matches("+expr.sql+", ?))")
		*args = append(*args, expr.args...)
		*args = append(*args, expr.args...)
		*args = append(*args, value)
	case "!~":
		*where = append(*where, "("+expr.sql+" IS NULL OR NOT regexp_matches("+expr.sql+", ?))")
		*args = append(*args, expr.args...)
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
	part := partitionExpr("partition", "labels_json")
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
		appendPredicate(&where, &args, logMatcherExpr(matcher.Name), matcher.Op, matcher.Value)
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
	appendScope(&where, &args, partitionExpr("partition", "labels_json"), scope)

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
	part := partitionExpr("partition", "labels_json")
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

	sql := "SELECT timestamp, service, metric_name, value, labels_json, " +
		part.sql + " AS partition FROM " + duckParquetSource(files)
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY timestamp DESC"
	return sql, args, nil
}

func buildTraceDuckSQL(files []string, traceID, service string, from, to time.Time, limit int, scope QueryScope) (string, []any, error) {
	where := make([]string, 0, 6)
	part := partitionExpr("partition", "attributes_json")
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
	appendScope(&where, &args, partitionExpr("partition", "attributes_json"), scope)

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
