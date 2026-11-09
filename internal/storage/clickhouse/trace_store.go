package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage/logengine"
	"github.com/radryc/monofs/internal/telemetrycrypto"
)

const (
	defaultDatabase     = "strata"
	defaultTable        = "spans"
	defaultLogsTable    = "logs"
	defaultMetricsTable = "metrics"
	defaultTenant       = "default"
	defaultTimeout      = 30 * time.Second
	// maxInsertRows bounds one HTTP insert request. ClickHouse recommends
	// batching thousands of rows rather than one request per span.
	maxInsertRows = 5000
)

// Config configures the ClickHouse trace store.
type Config struct {
	// URL is the ClickHouse HTTP endpoint, for example http://clickhouse:8123.
	URL string
	// Database and Table identify the span table.
	Database     string
	Table        string
	LogsTable    string
	MetricsTable string
	// Tenant is the logical tenant written to and queried from every row. The
	// MonoFS trace protocol does not carry a tenant yet, so this is a
	// deployment-level value.
	Tenant   string
	User     string
	Password string
	// Retention is the table TTL. Zero uses 30 days.
	Retention time.Duration
	// Timeout bounds one HTTP request.
	Timeout time.Duration
	// HTTPClient is optional and primarily used by tests.
	HTTPClient *http.Client
	// Encryption optionally seals sensitive payload columns. Filter columns
	// (tenant, time, service, trace/span id, level, metrics) stay plaintext.
	Encryption *telemetrycrypto.Provider
}

// TraceStore implements MonoFS telemetry ingest and query over ClickHouse's
// HTTP interface. The historical name is retained, but it owns logs, metrics,
// and spans. It intentionally depends only on the standard library so the
// server can enable it without adding a cgo or driver dependency.
type TraceStore struct {
	cfg       Config
	baseURL   string
	client    *http.Client
	transport *http.Transport
	stats     clickHouseStatsCache
}

// New creates a ClickHouse trace store. Call Bootstrap before serving traffic.
func New(cfg Config) (*TraceStore, error) {
	cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if cfg.URL == "" {
		return nil, fmt.Errorf("clickhouse url is required")
	}
	parsed, err := url.Parse(cfg.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("clickhouse url must be an http(s) endpoint with a host")
	}
	if cfg.Database == "" {
		cfg.Database = defaultDatabase
	}
	if cfg.Table == "" {
		cfg.Table = defaultTable
	}
	if cfg.LogsTable == "" {
		cfg.LogsTable = defaultLogsTable
	}
	if cfg.MetricsTable == "" {
		cfg.MetricsTable = defaultMetricsTable
	}
	if cfg.Tenant == "" {
		cfg.Tenant = defaultTenant
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 30 * 24 * time.Hour
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if !validIdentifier(cfg.Database) {
		return nil, fmt.Errorf("invalid clickhouse database %q", cfg.Database)
	}
	if !validIdentifier(cfg.Table) {
		return nil, fmt.Errorf("invalid clickhouse table %q", cfg.Table)
	}
	if !validIdentifier(cfg.LogsTable) {
		return nil, fmt.Errorf("invalid clickhouse logs table %q", cfg.LogsTable)
	}
	if !validIdentifier(cfg.MetricsTable) {
		return nil, fmt.Errorf("invalid clickhouse metrics table %q", cfg.MetricsTable)
	}

	client := cfg.HTTPClient
	transport := (*http.Transport)(nil)
	if client == nil {
		if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = defaultTransport.Clone()
		} else {
			transport = &http.Transport{}
		}
		client = &http.Client{Timeout: cfg.Timeout, Transport: transport}
	}
	return &TraceStore{cfg: cfg, baseURL: cfg.URL, client: client, transport: transport}, nil
}

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for idx, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (idx > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func (s *TraceStore) table() string {
	return s.cfg.Database + "." + s.cfg.Table
}

func (s *TraceStore) tableFor(name string) string {
	return s.cfg.Database + "." + name
}

// Bootstrap creates the database and the three telemetry tables. The schema is
// intentionally simple: MergeTree tables with daily partitions, signal-aware
// ordering, and TTL-based retention.
func (s *TraceStore) Bootstrap(ctx context.Context) error {
	// ClickHouse applies the `database` URL parameter before executing the
	// statement, so CREATE DATABASE must run against a database that already
	// exists (default).
	createParams := url.Values{"database": {"default"}}
	if err := s.exec(ctx, "CREATE DATABASE IF NOT EXISTS "+s.cfg.Database, createParams, nil); err != nil {
		return fmt.Errorf("create clickhouse database: %w", err)
	}
	ttl := ""
	if s.cfg.Retention > 0 {
		days := int64(s.cfg.Retention / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		ttl = fmt.Sprintf(" TTL toDateTime(timestamp) + INTERVAL %d DAY DELETE", days)
	}
	statements := []struct {
		name string
		sql  string
	}{
		{
			name: "spans",
			sql: "CREATE TABLE IF NOT EXISTS " + s.table() + ` (
		tenant LowCardinality(String),
		timestamp DateTime64(9, 'UTC'),
		end_time DateTime64(9, 'UTC'),
		duration_ns Int64,
		trace_id String,
		span_id String,
		parent_span_id String,
		service LowCardinality(String),
		name String,
		status_code LowCardinality(String),
		attributes Map(String, String)
	) ENGINE = MergeTree
	PARTITION BY toYYYYMMDD(timestamp)
	ORDER BY (tenant, service, toStartOfHour(timestamp), trace_id, span_id)` + ttl + `
	SETTINGS ttl_only_drop_parts = 1`,
		},
		{
			name: "logs",
			sql: "CREATE TABLE IF NOT EXISTS " + s.tableFor(s.cfg.LogsTable) + ` (
		tenant LowCardinality(String),
		timestamp DateTime64(9, 'UTC'),
		level LowCardinality(String),
		service LowCardinality(String),
		trace_id String,
		span_id String,
		raw_message String,
		labels Map(String, String)
	) ENGINE = MergeTree
	PARTITION BY toYYYYMMDD(timestamp)
	ORDER BY (tenant, service, toStartOfHour(timestamp), level)` + ttl + `
	SETTINGS ttl_only_drop_parts = 1`,
		},
		{
			name: "metrics",
			sql: "CREATE TABLE IF NOT EXISTS " + s.tableFor(s.cfg.MetricsTable) + ` (
		tenant LowCardinality(String),
		timestamp DateTime64(9, 'UTC'),
		start_time DateTime64(9, 'UTC'),
		service LowCardinality(String),
		metric_name String,
		value Float64,
		metric_type LowCardinality(String),
		temporality LowCardinality(String),
		unit String,
		description String,
		labels Map(String, String)
	) ENGINE = MergeTree
	PARTITION BY toYYYYMMDD(timestamp)
	ORDER BY (tenant, metric_name, toStartOfHour(timestamp))` + ttl + `
	SETTINGS ttl_only_drop_parts = 1`,
		},
	}
	for _, entry := range statements {
		if err := s.exec(ctx, entry.sql, nil, nil); err != nil {
			return fmt.Errorf("create clickhouse %s table: %w", entry.name, err)
		}
	}
	return nil
}

// Ping verifies the ClickHouse endpoint is reachable.
func (s *TraceStore) Ping(ctx context.Context) error {
	return s.exec(ctx, "SELECT 1", nil, nil)
}

// IngestTraces writes spans in JSONEachRow batches.
func (s *TraceStore) IngestTraces(ctx context.Context, _ string, spans []logengine.SpanRecord) error {
	if len(spans) == 0 {
		return nil
	}
	for start := 0; start < len(spans); start += maxInsertRows {
		end := start + maxInsertRows
		if end > len(spans) {
			end = len(spans)
		}
		if err := s.insertBatch(ctx, spans[start:end]); err != nil {
			return err
		}
	}
	return nil
}

type insertSpan struct {
	Tenant       string            `json:"tenant"`
	Timestamp    string            `json:"timestamp"`
	EndTime      string            `json:"end_time"`
	DurationNS   int64             `json:"duration_ns"`
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id"`
	Service      string            `json:"service"`
	Name         string            `json:"name"`
	StatusCode   string            `json:"status_code"`
	Attributes   map[string]string `json:"attributes"`
}

func (s *TraceStore) insertBatch(ctx context.Context, spans []logengine.SpanRecord) error {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, span := range spans {
		attributes := span.Attributes
		if attributes == nil {
			attributes = map[string]string{}
		}
		start := span.Timestamp
		if start.IsZero() {
			start = time.Unix(0, 0)
		}
		end := span.EndTime
		if end.IsZero() {
			end = start
		}
		duration := end.Sub(start)
		if duration < 0 {
			duration = 0
		}
		name, err := s.encryptField(partitionFor(attributes), "spans.name", span.Name)
		if err != nil {
			return err
		}
		if err := encoder.Encode(insertSpan{
			Tenant:       s.cfg.Tenant,
			Timestamp:    formatClickHouseTime(start),
			EndTime:      formatClickHouseTime(end),
			DurationNS:   int64(duration),
			TraceID:      span.TraceID,
			SpanID:       span.SpanID,
			ParentSpanID: span.ParentSpanID,
			Service:      span.Service,
			Name:         name,
			StatusCode:   span.StatusCode,
			Attributes:   attributes,
		}); err != nil {
			return fmt.Errorf("encode clickhouse span: %w", err)
		}
	}
	statement := "INSERT INTO " + s.table() + " (tenant, timestamp, end_time, duration_ns, trace_id, span_id, parent_span_id, service, name, status_code, attributes) FORMAT JSONEachRow"
	if err := s.exec(ctx, statement, nil, &body); err != nil {
		return fmt.Errorf("insert clickhouse spans: %w", err)
	}
	return nil
}

func formatClickHouseTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000000")
}

// QueryTraces returns spans matching the requested trace/service and range.
func (s *TraceStore) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]logengine.SpanRecord, error) {
	records := make([]logengine.SpanRecord, 0)
	err := s.StreamTraces(ctx, traceID, service, from, to, limit, func(record logengine.SpanRecord) error {
		records = append(records, record)
		return nil
	})
	return records, err
}

// StreamTraces streams spans from ClickHouse in newest-first order.
func (s *TraceStore) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(logengine.SpanRecord) error) error {
	query, params := s.traceQuery(traceID, service, from, to, limit)
	response, err := s.do(ctx, query, params, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	decoder := json.NewDecoder(response.Body)
	for {
		var row traceRow
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode clickhouse span: %w", err)
		}
		record, err := row.record(s.encryption())
		if err != nil {
			return err
		}
		if err := yield(record); err != nil {
			return err
		}
	}
}

type traceRow struct {
	TimestampNS  int64  `json:"timestamp_ns"`
	EndTimeNS    int64  `json:"end_time_ns"`
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id"`
	Service      string `json:"service"`
	Name         string `json:"name"`
	StatusCode   string `json:"status_code"`
	Attributes   string `json:"attributes_json"`
}

func (r traceRow) record(enc *telemetrycrypto.Provider) (logengine.SpanRecord, error) {
	name := r.Name
	if enc != nil {
		decrypted, err := enc.DecryptField(r.Name, "spans.name")
		if err != nil {
			return logengine.SpanRecord{}, err
		}
		name = decrypted
	}
	record := logengine.SpanRecord{
		Timestamp:    time.Unix(0, r.TimestampNS).UTC(),
		EndTime:      time.Unix(0, r.EndTimeNS).UTC(),
		TraceID:      r.TraceID,
		SpanID:       r.SpanID,
		ParentSpanID: r.ParentSpanID,
		Service:      r.Service,
		Name:         name,
		StatusCode:   r.StatusCode,
	}
	if strings.TrimSpace(r.Attributes) != "" {
		_ = json.Unmarshal([]byte(r.Attributes), &record.Attributes)
	}
	return record, nil
}

func (s *TraceStore) traceQuery(traceID, service string, from, to time.Time, limit int) (string, url.Values) {
	params := url.Values{}
	params.Set("param_tenant", s.cfg.Tenant)
	where := []string{"tenant = {tenant:String}"}
	if !from.IsZero() {
		params.Set("param_from", formatClickHouseTime(from))
		where = append(where, "timestamp >= toDateTime64({from:String}, 9, 'UTC')")
	}
	if !to.IsZero() {
		params.Set("param_to", formatClickHouseTime(to))
		where = append(where, "timestamp <= toDateTime64({to:String}, 9, 'UTC')")
	}
	if traceID != "" {
		params.Set("param_trace_id", traceID)
		where = append(where, "trace_id = {trace_id:String}")
	}
	if service != "" {
		params.Set("param_service", service)
		where = append(where, "service = {service:String}")
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > 100_000 {
		limit = 100_000
	}
	query := "SELECT toUnixTimestamp64Nano(timestamp) AS timestamp_ns, " +
		"toUnixTimestamp64Nano(end_time) AS end_time_ns, " +
		"trace_id, span_id, parent_span_id, service, name, status_code, " +
		"toJSONString(attributes) AS attributes_json " +
		"FROM " + s.table() + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY timestamp DESC LIMIT " + strconv.Itoa(limit) + " FORMAT JSONEachRow"
	return query, params
}

// DiscoverTraceServices returns distinct services in the time window.
func (s *TraceStore) DiscoverTraceServices(ctx context.Context, from, to time.Time) ([]string, error) {
	query, params := s.discoveryQuery("service", from, to, "")
	return s.queryStrings(ctx, query, params)
}

// DiscoverTraceTags returns the fixed span tags plus all attribute map keys.
func (s *TraceStore) DiscoverTraceTags(ctx context.Context, from, to time.Time) ([]string, error) {
	params := url.Values{}
	params.Set("param_tenant", s.cfg.Tenant)
	where := []string{"tenant = {tenant:String}"}
	if !from.IsZero() {
		params.Set("param_from", formatClickHouseTime(from))
		where = append(where, "timestamp >= toDateTime64({from:String}, 9, 'UTC')")
	}
	if !to.IsZero() {
		params.Set("param_to", formatClickHouseTime(to))
		where = append(where, "timestamp <= toDateTime64({to:String}, 9, 'UTC')")
	}
	query := "SELECT tag AS value FROM (" +
		"SELECT arrayJoin(mapKeys(attributes)) AS tag FROM " + s.table() + " WHERE " + strings.Join(where, " AND ") +
		" UNION ALL SELECT 'service.name' AS tag UNION ALL SELECT 'name' AS tag UNION ALL SELECT 'status' AS tag" +
		") GROUP BY tag ORDER BY tag FORMAT JSONEachRow"
	return s.queryStrings(ctx, query, params)
}

// DiscoverTraceTagValues returns distinct values for one tag.
func (s *TraceStore) DiscoverTraceTagValues(ctx context.Context, from, to time.Time, tag string) ([]string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, fmt.Errorf("trace discovery tag is required")
	}
	if tag == "service.name" || tag == "service" {
		return s.DiscoverTraceServices(ctx, from, to)
	}
	query, params := s.discoveryQuery(tag, from, to, tag)
	return s.queryStrings(ctx, query, params)
}

func (s *TraceStore) discoveryQuery(kind string, from, to time.Time, tag string) (string, url.Values) {
	params := url.Values{}
	params.Set("param_tenant", s.cfg.Tenant)
	where := []string{"tenant = {tenant:String}"}
	if !from.IsZero() {
		params.Set("param_from", formatClickHouseTime(from))
		where = append(where, "timestamp >= toDateTime64({from:String}, 9, 'UTC')")
	}
	if !to.IsZero() {
		params.Set("param_to", formatClickHouseTime(to))
		where = append(where, "timestamp <= toDateTime64({to:String}, 9, 'UTC')")
	}
	column := kind
	if kind == "status" {
		column = "status_code"
	}
	if tag != "" && kind != "name" && kind != "status" {
		params.Set("param_tag", tag)
		where = append(where, "mapContains(attributes, {tag:String})")
		column = "attributes[{tag:String}]"
	}
	query := "SELECT DISTINCT " + column + " AS value FROM " + s.table() +
		" WHERE " + strings.Join(where, " AND ") + " ORDER BY value FORMAT JSONEachRow"
	return query, params
}

func (s *TraceStore) queryStrings(ctx context.Context, query string, params url.Values) ([]string, error) {
	response, err := s.do(ctx, query, params, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	values := make([]string, 0)
	for {
		var row struct {
			Value string `json:"value"`
		}
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode clickhouse discovery row: %w", err)
		}
		if row.Value != "" {
			values = append(values, row.Value)
		}
	}
	sort.Strings(values)
	return values, nil
}

func (s *TraceStore) exec(ctx context.Context, statement string, params url.Values, body io.Reader) error {
	response, err := s.do(ctx, statement, params, body)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return response.Body.Close()
}

func (s *TraceStore) do(ctx context.Context, statement string, params url.Values, body io.Reader) (*http.Response, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("query", statement)
	if params.Get("database") == "" {
		params.Set("database", s.cfg.Database)
	}
	params.Set("output_format_json_quote_64bit_integers", "0")
	endpoint := s.baseURL + "/?" + params.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	if s.cfg.User != "" {
		request.Header.Set("X-ClickHouse-User", s.cfg.User)
	}
	if s.cfg.Password != "" {
		request.Header.Set("X-ClickHouse-Key", s.cfg.Password)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		response.Body.Close()
		return nil, fmt.Errorf("clickhouse http %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return response, nil
}

// Close releases idle HTTP connections.
func (s *TraceStore) Close() error {
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
	return nil
}
