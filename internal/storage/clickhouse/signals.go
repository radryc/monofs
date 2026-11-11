package clickhouse

import (
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
	"github.com/radryc/monofs/internal/storage/logquery"
)

const (
	maxLogRows          = 100_000
	maxMetricRows       = 1_000_000
	maxMetricSeries     = 200_000
	maxDiscoveredNames  = 20_000
	maxDiscoveredValues = 10_000
)

// IngestLogs writes logs in JSONEachRow batches.
func (s *TraceStore) IngestLogs(ctx context.Context, _ string, logs []logengine.LogRecord) error {
	if len(logs) == 0 {
		return nil
	}
	for start := 0; start < len(logs); start += maxInsertRows {
		end := start + maxInsertRows
		if end > len(logs) {
			end = len(logs)
		}
		if err := s.insertLogBatch(ctx, logs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

type insertLog struct {
	Tenant     string            `json:"tenant"`
	Timestamp  string            `json:"timestamp"`
	Level      string            `json:"level"`
	Service    string            `json:"service"`
	TraceID    string            `json:"trace_id"`
	SpanID     string            `json:"span_id"`
	RawMessage string            `json:"raw_message"`
	Labels     map[string]string `json:"labels"`
}

func (s *TraceStore) insertLogBatch(ctx context.Context, logs []logengine.LogRecord) error {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, record := range logs {
		labels := record.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		ts := record.Timestamp
		if ts.IsZero() {
			ts = time.Unix(0, 0)
		}
		message, err := s.encryptField(partitionFor(labels), "logs.raw_message", record.RawMessage)
		if err != nil {
			return err
		}
		if err := encoder.Encode(insertLog{
			Tenant:     s.cfg.Tenant,
			Timestamp:  formatClickHouseTime(ts),
			Level:      record.Level,
			Service:    record.Service,
			TraceID:    record.TraceID,
			SpanID:     record.SpanID,
			RawMessage: message,
			Labels:     labels,
		}); err != nil {
			return fmt.Errorf("encode clickhouse log: %w", err)
		}
	}
	statement := "INSERT INTO " + s.tableFor(s.cfg.LogsTable) +
		" (tenant, timestamp, level, service, trace_id, span_id, raw_message, labels) FORMAT JSONEachRow"
	if err := s.exec(ctx, statement, nil, &body); err != nil {
		return fmt.Errorf("insert clickhouse logs: %w", err)
	}
	return nil
}

// StreamLogs streams logs from ClickHouse.
func (s *TraceStore) StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(logengine.LogRecord) error) error {
	if strings.TrimSpace(queryStr) == "" {
		queryStr = `{service=~".+"}`
	}
	parsed, err := logquery.Parse(queryStr)
	if err != nil {
		return fmt.Errorf("failed to parse log query: %w", err)
	}
	scope, _ := logengine.QueryScopeFromContext(ctx)
	filter := s.newFilter()
	filter.addTimeRange(from, to)
	if scope.Restricted {
		filter.addPartitionScope("labels", scope)
	}
	if service != "" {
		filter.addLogMatcher("service", "=", service)
	}
	for _, matcher := range parsed.Matchers {
		filter.addLogMatcher(matcher.Name, matcher.Op, matcher.Value)
	}
	// Line filters cannot be pushed into ClickHouse when raw_message is
	// ciphertext. Evaluate them after decryption instead.
	postLineFilters := parsed.LineFilters
	if s.encryption() != nil {
		postLineFilters = parsed.LineFilters
	} else {
		for _, lineFilter := range parsed.LineFilters {
			filter.addLineFilter(lineFilter)
		}
		postLineFilters = nil
	}
	if limit <= 0 {
		limit = maxLogRows
	}
	if limit > maxLogRows {
		return fmt.Errorf("%w: log limit %d exceeds %d", logengine.ErrQueryTooBroad, limit, maxLogRows)
	}
	fetchLimit := limit
	if len(postLineFilters) > 0 {
		// Decryption reduces the matching set, so over-fetch a bounded batch
		// before applying the filters in Go.
		fetchLimit = limit * 8
		if fetchLimit > maxLogRows {
			fetchLimit = maxLogRows
		}
	}
	statement := "SELECT toUnixTimestamp64Nano(timestamp) AS timestamp_ns, level, service, trace_id, span_id, raw_message, toJSONString(labels) AS labels_json FROM " +
		s.tableFor(s.cfg.LogsTable) + " WHERE " + filter.whereSQL() +
		" ORDER BY timestamp DESC LIMIT " + strconv.Itoa(fetchLimit) + " FORMAT JSONEachRow"
	response, err := s.do(ctx, statement, filter.params, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	for {
		var row struct {
			TimestampNS int64  `json:"timestamp_ns"`
			Level       string `json:"level"`
			Service     string `json:"service"`
			TraceID     string `json:"trace_id"`
			SpanID      string `json:"span_id"`
			RawMessage  string `json:"raw_message"`
			LabelsJSON  string `json:"labels_json"`
		}
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode clickhouse log: %w", err)
		}
		message, err := s.decryptField(row.RawMessage, "logs.raw_message")
		if err != nil {
			return err
		}
		record := logengine.LogRecord{
			Timestamp:  time.Unix(0, row.TimestampNS).UTC(),
			Level:      row.Level,
			Service:    row.Service,
			TraceID:    row.TraceID,
			SpanID:     row.SpanID,
			RawMessage: message,
		}
		if row.LabelsJSON != "" {
			_ = json.Unmarshal([]byte(row.LabelsJSON), &record.Labels)
		}
		// Match the Parquet engine's scope semantics; the SQL predicate is an
		// optimisation and the record-level scope remains authoritative.
		if scope.Restricted && !scope.AllowsPartitionLabels(record.Labels) {
			continue
		}
		if len(postLineFilters) > 0 {
			matched, matchErr := lineFiltersMatch(postLineFilters, message)
			if matchErr != nil {
				return matchErr
			}
			if !matched {
				continue
			}
		}
		if err := yield(record); err != nil {
			return err
		}
	}
}

func lineFiltersMatch(filters []logquery.LineFilter, message string) (bool, error) {
	for _, filter := range filters {
		switch filter.Op {
		case "|=":
			if !strings.Contains(message, filter.Value) {
				return false, nil
			}
		case "!=":
			if strings.Contains(message, filter.Value) {
				return false, nil
			}
		case "|~":
			re, err := regexp.Compile(filter.Value)
			if err != nil {
				return false, fmt.Errorf("invalid line filter regex %q: %w", filter.Value, err)
			}
			if !re.MatchString(message) {
				return false, nil
			}
		case "!~":
			re, err := regexp.Compile(filter.Value)
			if err != nil {
				return false, fmt.Errorf("invalid line filter regex %q: %w", filter.Value, err)
			}
			if re.MatchString(message) {
				return false, nil
			}
		}
	}
	return true, nil
}

// QueryLogs collects logs from ClickHouse.
func (s *TraceStore) QueryLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int) ([]logengine.LogRecord, error) {
	records := make([]logengine.LogRecord, 0)
	err := s.StreamLogs(ctx, queryStr, service, from, to, limit, func(record logengine.LogRecord) error {
		records = append(records, record)
		return nil
	})
	return records, err
}

// IngestMetrics writes metric points in JSONEachRow batches.
func (s *TraceStore) IngestMetrics(ctx context.Context, _ string, metrics []logengine.MetricRecord) error {
	if len(metrics) == 0 {
		return nil
	}
	for start := 0; start < len(metrics); start += maxInsertRows {
		end := start + maxInsertRows
		if end > len(metrics) {
			end = len(metrics)
		}
		if err := s.insertMetricBatch(ctx, metrics[start:end]); err != nil {
			return err
		}
	}
	return nil
}

type insertMetric struct {
	Tenant      string            `json:"tenant"`
	Timestamp   string            `json:"timestamp"`
	StartTime   string            `json:"start_time"`
	Service     string            `json:"service"`
	MetricName  string            `json:"metric_name"`
	Value       float64           `json:"value"`
	MetricType  string            `json:"metric_type"`
	Temporality string            `json:"temporality"`
	Unit        string            `json:"unit"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
}

func (s *TraceStore) insertMetricBatch(ctx context.Context, metrics []logengine.MetricRecord) error {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, record := range metrics {
		labels := record.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		ts := record.Timestamp
		if ts.IsZero() {
			ts = time.Unix(0, 0)
		}
		startTime := record.StartTime
		if startTime.IsZero() {
			startTime = time.Unix(0, 0)
		}
		if err := encoder.Encode(insertMetric{
			Tenant:      s.cfg.Tenant,
			Timestamp:   formatClickHouseTime(ts),
			StartTime:   formatClickHouseTime(startTime),
			Service:     record.Service,
			MetricName:  record.MetricName,
			Value:       record.Value,
			MetricType:  record.MetricType,
			Temporality: record.Temporality,
			Unit:        record.Unit,
			Description: record.Description,
			Labels:      labels,
		}); err != nil {
			return fmt.Errorf("encode clickhouse metric: %w", err)
		}
	}
	statement := "INSERT INTO " + s.tableFor(s.cfg.MetricsTable) +
		" (tenant, timestamp, start_time, service, metric_name, value, metric_type, temporality, unit, description, labels) FORMAT JSONEachRow"
	if err := s.exec(ctx, statement, nil, &body); err != nil {
		return fmt.Errorf("insert clickhouse metrics: %w", err)
	}
	return nil
}

// StreamMetrics streams metric points or discovery results from ClickHouse.
func (s *TraceStore) StreamMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricRecord) error) error {
	query, discoveryMode := logengine.StripMetricDiscoveryMatchers(query)
	if discoveryMode != "" {
		return s.streamMetricDiscovery(ctx, query, discoveryMode, from, to, yield)
	}
	filter := s.newFilter()
	filter.addTimeRange(from, to)
	filter.addMetricQuery(query)
	statement := "SELECT toUnixTimestamp64Nano(timestamp) AS timestamp_ns, toUnixTimestamp64Nano(start_time) AS start_time_ns, " +
		"service, metric_name, value, metric_type, temporality, unit, description, toJSONString(labels) AS labels_json FROM " +
		s.tableFor(s.cfg.MetricsTable) + " WHERE " + filter.whereSQL() +
		" ORDER BY timestamp LIMIT " + strconv.Itoa(maxMetricRows+1) + " FORMAT JSONEachRow"
	response, err := s.do(ctx, statement, filter.params, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	count := 0
	for {
		var row struct {
			TimestampNS int64   `json:"timestamp_ns"`
			StartTimeNS int64   `json:"start_time_ns"`
			Service     string  `json:"service"`
			MetricName  string  `json:"metric_name"`
			Value       float64 `json:"value"`
			MetricType  string  `json:"metric_type"`
			Temporality string  `json:"temporality"`
			Unit        string  `json:"unit"`
			Description string  `json:"description"`
			LabelsJSON  string  `json:"labels_json"`
		}
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode clickhouse metric: %w", err)
		}
		count++
		if count > maxMetricRows {
			return fmt.Errorf("%w: more than %d metric samples", logengine.ErrQueryTooBroad, maxMetricRows)
		}
		record := logengine.MetricRecord{
			Timestamp:   time.Unix(0, row.TimestampNS).UTC(),
			Service:     row.Service,
			MetricName:  row.MetricName,
			Value:       row.Value,
			MetricType:  row.MetricType,
			Temporality: row.Temporality,
			Unit:        row.Unit,
			Description: row.Description,
		}
		if row.StartTimeNS != 0 {
			record.StartTime = time.Unix(0, row.StartTimeNS).UTC()
		}
		if row.LabelsJSON != "" {
			_ = json.Unmarshal([]byte(row.LabelsJSON), &record.Labels)
		}
		if err := yield(record); err != nil {
			return err
		}
	}
}

// QueryMetrics collects metric points from ClickHouse.
func (s *TraceStore) QueryMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time) ([]logengine.MetricRecord, error) {
	records := make([]logengine.MetricRecord, 0)
	err := s.StreamMetrics(ctx, query, from, to, func(record logengine.MetricRecord) error {
		records = append(records, record)
		return nil
	})
	return records, err
}

func (s *TraceStore) streamMetricDiscovery(ctx context.Context, query logengine.MetricQuery, mode string, from, to time.Time, yield func(logengine.MetricRecord) error) error {
	filter := s.newFilter()
	filter.addTimeRange(from, to)
	filter.addMetricQuery(query)

	statement := ""
	limit := maxDiscoveredValues
	labelName := ""
	switch {
	case mode == logengine.MetricDiscoveryModeNames:
		statement = "SELECT DISTINCT metric_name AS value FROM " + s.tableFor(s.cfg.MetricsTable) +
			" WHERE " + filter.whereSQL() + " ORDER BY value LIMIT " + strconv.Itoa(maxDiscoveredNames) + " FORMAT JSONEachRow"
	case mode == logengine.MetricDiscoveryModeLabels:
		statement = "SELECT DISTINCT arrayJoin(mapKeys(labels)) AS value FROM " + s.tableFor(s.cfg.MetricsTable) +
			" WHERE " + filter.whereSQL() + " ORDER BY value LIMIT " + strconv.Itoa(limit) + " FORMAT JSONEachRow"
	case strings.HasPrefix(mode, logengine.MetricDiscoveryModeValues):
		labelName = strings.TrimPrefix(mode, logengine.MetricDiscoveryModeValues)
		if labelName == "" {
			return fmt.Errorf("label name is required")
		}
		keys := filter.labelKeys(labelName)
		labelExpressions := make([]string, 0, len(keys))
		exists := make([]string, 0, len(keys))
		for _, key := range keys {
			param := filter.param(key)
			labelExpressions = append(labelExpressions, "labels["+param+"]")
			exists = append(exists, "mapContains(labels, "+param+")")
		}
		filter.where = append(filter.where, "("+strings.Join(exists, " OR ")+")")
		inner := "SELECT arrayJoin([" + strings.Join(labelExpressions, ", ") + "]) AS value FROM " +
			s.tableFor(s.cfg.MetricsTable) + " WHERE " + filter.whereSQL()
		statement = "SELECT DISTINCT value FROM (" + inner + ") WHERE value != '' ORDER BY value LIMIT " +
			strconv.Itoa(limit) + " FORMAT JSONEachRow"
	default:
		return fmt.Errorf("unsupported metric discovery mode %q", mode)
	}
	response, err := s.do(ctx, statement, filter.params, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	for {
		var row struct {
			Value string `json:"value"`
		}
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode clickhouse metric discovery: %w", err)
		}
		if row.Value == "" {
			continue
		}
		record := logengine.MetricRecord{}
		switch {
		case mode == logengine.MetricDiscoveryModeNames:
			record.MetricName = row.Value
		case mode == logengine.MetricDiscoveryModeLabels:
			record.Labels = map[string]string{row.Value: ""}
		default:
			record.Labels = map[string]string{labelName: row.Value}
		}
		if err := yield(record); err != nil {
			return err
		}
	}
}

// StreamMetricSeries streams one metadata row per distinct series.
func (s *TraceStore) StreamMetricSeries(ctx context.Context, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricSeriesMeta) error) error {
	query, _ = logengine.StripMetricDiscoveryMatchers(query)
	filter := s.newFilter()
	filter.addTimeRange(from, to)
	filter.addMetricQuery(query)
	statement := "SELECT metric_name, service, any(metric_type) AS metric_type, any(unit) AS unit, " +
		"any(description) AS description, toJSONString(labels) AS labels_json, " +
		"toUnixTimestamp64Nano(min(timestamp)) AS min_ns, toUnixTimestamp64Nano(max(timestamp)) AS max_ns FROM " +
		s.tableFor(s.cfg.MetricsTable) + " WHERE " + filter.whereSQL() +
		" GROUP BY metric_name, service, toJSONString(labels) " +
		"ORDER BY metric_name, service LIMIT " + strconv.Itoa(maxMetricSeries+1) + " FORMAT JSONEachRow"
	response, err := s.do(ctx, statement, filter.params, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	count := 0
	for {
		var row struct {
			MetricName  string `json:"metric_name"`
			Service     string `json:"service"`
			MetricType  string `json:"metric_type"`
			Unit        string `json:"unit"`
			Description string `json:"description"`
			LabelsJSON  string `json:"labels_json"`
			MinNS       int64  `json:"min_ns"`
			MaxNS       int64  `json:"max_ns"`
		}
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode clickhouse metric series: %w", err)
		}
		count++
		if count > maxMetricSeries {
			return fmt.Errorf("%w: more than %d distinct series", logengine.ErrMetricSeriesLimit, maxMetricSeries)
		}
		meta := logengine.MetricSeriesMeta{
			MetricName:  row.MetricName,
			Service:     row.Service,
			MetricType:  row.MetricType,
			Unit:        row.Unit,
			Description: row.Description,
			MinTime:     time.Unix(0, row.MinNS).UTC(),
			MaxTime:     time.Unix(0, row.MaxNS).UTC(),
		}
		if row.LabelsJSON != "" {
			_ = json.Unmarshal([]byte(row.LabelsJSON), &meta.Labels)
		}
		if err := yield(meta); err != nil {
			return err
		}
	}
}

// Aggregate is not implemented for the ClickHouse backend yet. The normal
// Doctor log/metric/trace query paths stream raw records and aggregate in the
// query service; the shard-local aggregate RPC is a follow-up.
func (s *TraceStore) Aggregate(context.Context, logengine.AggregateQuery) ([]logengine.AggregateRow, error) {
	return nil, fmt.Errorf("telemetry aggregation is not supported by the ClickHouse backend yet")
}

// Stats returns an empty local-chunk snapshot. ClickHouse owns the telemetry
// data, so chunk counts are not meaningful here.
func (s *TraceStore) Stats(context.Context) (logengine.LogEngineStats, error) {
	return logengine.LogEngineStats{}, nil
}

type signalFilter struct {
	params url.Values
	where  []string
	next   int
}

func (s *TraceStore) newFilter() *signalFilter {
	params := url.Values{}
	params.Set("param_tenant", s.cfg.Tenant)
	return &signalFilter{
		params: params,
		where:  []string{"tenant = {tenant:String}"},
	}
}

func (f *signalFilter) param(value string) string {
	name := "p" + strconv.Itoa(f.next)
	f.next++
	f.params.Set("param_"+name, value)
	return "{" + name + ":String}"
}

func (f *signalFilter) whereSQL() string {
	return strings.Join(f.where, " AND ")
}

func (f *signalFilter) addTimeRange(from, to time.Time) {
	if !from.IsZero() {
		f.params.Set("param_from", formatClickHouseTime(from))
		f.where = append(f.where, "timestamp >= toDateTime64({from:String}, 9, 'UTC')")
	}
	if !to.IsZero() {
		f.params.Set("param_to", formatClickHouseTime(to))
		f.where = append(f.where, "timestamp <= toDateTime64({to:String}, 9, 'UTC')")
	}
}

func (f *signalFilter) addLogMatcher(name, op, value string) {
	expr := ""
	switch name {
	case "service", "service_name":
		expr = "coalesce(service, '')"
	case "level", "severity_text":
		expr = "coalesce(level, '')"
	case "trace_id":
		expr = "coalesce(trace_id, '')"
	case "span_id":
		expr = "coalesce(span_id, '')"
	case "body", "raw_message":
		expr = "coalesce(raw_message, '')"
	default:
		expr = f.labelValueExpr(name)
	}
	f.addPredicate(expr, op, value)
}

func (f *signalFilter) addMetricLabelMatcher(name, op, value string) {
	expr := ""
	switch name {
	case "service":
		expr = "coalesce(service, '')"
	case "__name__", "metric_name":
		expr = "coalesce(metric_name, '')"
	default:
		expr = f.labelValueExpr(name)
	}
	f.addPredicate(expr, op, value)
}

// labelValueExpr looks up a label in both dotted and underscore spellings.
// OTel resource attributes keep dots while PromQL/LogQL selectors normalize
// them to underscores; the store may contain either.
func (f *signalFilter) labelValueExpr(name string) string {
	parts := make([]string, 0, 3)
	seen := make(map[string]struct{})
	for _, key := range []string{name, strings.ReplaceAll(name, "_", "."), strings.ReplaceAll(name, ".", "_")} {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		parts = append(parts, "nullif(labels["+f.param(key)+"], '')")
	}
	return "coalesce(" + strings.Join(parts, ", ") + ", '')"
}

func (f *signalFilter) labelKeys(name string) []string {
	keys := make([]string, 0, 3)
	seen := make(map[string]struct{})
	for _, key := range []string{name, strings.ReplaceAll(name, "_", "."), strings.ReplaceAll(name, ".", "_")} {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// addPartitionScope pushes a restricted log query's partition scope into SQL
// before ORDER BY/LIMIT, so a scoped caller's page is not consumed by rows
// from partitions they cannot read.
func (f *signalFilter) addPartitionScope(column string, scope logengine.QueryScope) {
	if !scope.Restricted {
		return
	}
	placeholders := make([]string, 0, len(scope.Partitions))
	for _, partition := range scope.Partitions {
		partition = strings.TrimSpace(partition)
		if partition == "" {
			continue
		}
		placeholders = append(placeholders, f.param(partition))
	}
	if len(placeholders) == 0 {
		f.where = append(f.where, "0")
		return
	}
	list := strings.Join(placeholders, ", ")
	f.where = append(f.where, "("+
		"coalesce("+column+"['guardian_partition'], '') IN ("+list+") OR "+
		"coalesce("+column+"['guardian.partition'], '') IN ("+list+")"+
		")")
}

func (f *signalFilter) addPredicate(expr, op, value string) {
	if op == "=~" || op == "!~" {
		value = "^(?:" + value + ")$"
	}
	p := f.param(value)
	switch op {
	case "=":
		f.where = append(f.where, expr+" = "+p)
	case "!=":
		f.where = append(f.where, expr+" <> "+p)
	case "=~":
		f.where = append(f.where, "match("+expr+", "+p+")")
	case "!~":
		f.where = append(f.where, "NOT match("+expr+", "+p+")")
	}
}

func (f *signalFilter) addLineFilter(filter logquery.LineFilter) {
	p := f.param(filter.Value)
	switch filter.Op {
	case "|=":
		f.where = append(f.where, "position(raw_message, "+p+") > 0")
	case "!=":
		f.where = append(f.where, "position(raw_message, "+p+") = 0")
	case "|~":
		f.where = append(f.where, "match(raw_message, "+p+")")
	case "!~":
		f.where = append(f.where, "NOT match(raw_message, "+p+")")
	}
}

func (f *signalFilter) addMetricQuery(query logengine.MetricQuery) {
	if query.Service != "" {
		f.params.Set("param_service", query.Service)
		f.where = append(f.where, "service = {service:String}")
	}
	if query.MetricName != "" {
		f.params.Set("param_metric_name", query.MetricName)
		f.where = append(f.where, "metric_name = {metric_name:String}")
	}
	for _, matcher := range query.LabelMatchers {
		op := "="
		switch matcher.Type {
		case logengine.MetricMatchNotEqual:
			op = "!="
		case logengine.MetricMatchRegexp:
			op = "=~"
		case logengine.MetricMatchNotRegexp:
			op = "!~"
		}
		f.addMetricLabelMatcher(matcher.Name, op, matcher.Value)
	}
}
