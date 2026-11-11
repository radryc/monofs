package clickhouse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage/logengine"
)

type fakeClickHouse struct {
	mu         sync.Mutex
	statements []string
	params     []url.Values
	databases  []string
	inserts    []string
	sqlAST     string
}

func (f *fakeClickHouse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	statement := r.URL.Query().Get("query")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.statements = append(f.statements, statement)
	f.params = append(f.params, r.URL.Query())
	f.databases = append(f.databases, r.URL.Query().Get("database"))
	if strings.HasPrefix(strings.TrimSpace(statement), "INSERT") {
		f.inserts = append(f.inserts, string(body))
	}
	ast := f.sqlAST
	f.mu.Unlock()

	switch {
	case strings.HasPrefix(strings.TrimSpace(statement), "CREATE"),
		strings.HasPrefix(strings.TrimSpace(statement), "INSERT"),
		strings.HasPrefix(strings.TrimSpace(statement), "SELECT 1"):
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(strings.TrimSpace(statement), "EXPLAIN AST"):
		if ast == "" {
			ast = "SelectWithUnionQuery (children 1)\n TableIdentifier logs\n"
		}
		io.WriteString(w, ast)
	case strings.Contains(statement, "SELECT version() AS version"):
		io.WriteString(w, `{"version":"25.8.33.6"}`+"\n")
	case strings.Contains(statement, "FROM system.disks"):
		io.WriteString(w, `{"total_bytes":1000,"free_bytes":400}`+"\n")
	case strings.Contains(statement, "FROM system.parts"):
		io.WriteString(w, `{"name":"spans","rows":10,"bytes":100,"parts":2}`+"\n"+`{"name":"logs","rows":5,"bytes":50,"parts":1}`+"\n")
	case strings.Contains(statement, "AS doctor_sql"):
		io.WriteString(w, `{"meta":[{"name":"service","type":"LowCardinality(String)"},{"name":"n","type":"UInt64"}],"data":[{"service":"api","n":2}],"rows":1}`)
	case strings.Contains(statement, "arrayJoin(mapKeys(attributes))"):
		io.WriteString(w, `{"value":"http.method"}`+"\n"+`{"value":"service.name"}`+"\n")
	case strings.Contains(statement, "DISTINCT service AS value"):
		io.WriteString(w, `{"value":"api"}`+"\n"+`{"value":"worker"}`+"\n")
	case strings.Contains(statement, "attributes[{tag:String}] AS value"):
		io.WriteString(w, `{"value":"GET"}`+"\n")
	case strings.Contains(statement, "DISTINCT name AS value"):
		io.WriteString(w, `{"value":"GET /orders"}`+"\n")
	case strings.Contains(statement, "DISTINCT status_code AS value"):
		io.WriteString(w, `{"value":"OK"}`+"\n")
	case strings.Contains(statement, "raw_message") && strings.Contains(statement, "ORDER BY timestamp DESC"):
		io.WriteString(w, `{"timestamp_ns":300,"level":"error","service":"api","trace_id":"t1","span_id":"s1","raw_message":"boom","labels_json":"{\"env\":\"prod\"}"}`+"\n")
	case strings.Contains(statement, "GROUP BY metric_name"):
		io.WriteString(w, `{"metric_name":"requests_total","service":"api","labels_json":"{\"env\":\"prod\"}","min_ns":100,"max_ns":200}`+"\n")
	case strings.Contains(statement, "metric_name, value") && strings.Contains(statement, "ORDER BY timestamp"):
		io.WriteString(w, `{"timestamp_ns":200,"service":"api","metric_name":"requests_total","value":3.5,"labels_json":"{\"env\":\"prod\"}"}`+"\n")
	case strings.Contains(statement, "arrayJoin(mapKeys(labels))"):
		io.WriteString(w, `{"value":"env"}`+"\n")
	case strings.Contains(statement, "SELECT DISTINCT metric_name AS value"):
		io.WriteString(w, `{"value":"requests_total"}`+"\n")
	case strings.Contains(statement, "SELECT DISTINCT value FROM ("):
		io.WriteString(w, `{"value":"prod"}`+"\n")
	case strings.Contains(statement, "ORDER BY timestamp DESC"):
		io.WriteString(w, `{"timestamp_ns":100,"end_time_ns":200,"trace_id":"t1","span_id":"s1","parent_span_id":"","service":"api","name":"GET /orders","status_code":"OK","attributes_json":"{\"http.method\":\"GET\"}"}`+"\n")
	default:
		http.Error(w, "unexpected statement: "+statement, http.StatusBadRequest)
	}
}

func newFakeStore(t *testing.T) (*TraceStore, *fakeClickHouse) {
	t.Helper()
	fake := &fakeClickHouse{}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store, err := New(Config{
		URL:        server.URL,
		Tenant:     "tenant-a",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, fake
}

func TestTraceStoreBootstrapAndIngest(t *testing.T) {
	store, fake := newFakeStore(t)
	ctx := context.Background()
	if err := store.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	if len(fake.databases) < 2 || fake.databases[0] != "default" || fake.databases[1] != "strata" {
		t.Fatalf("bootstrap databases = %#v, want [default strata]", fake.databases)
	}
	fake.mu.Unlock()
	start := time.Date(2026, 11, 4, 12, 0, 0, 123456789, time.UTC)
	if err := store.IngestTraces(ctx, "chunk-1", []logengine.SpanRecord{{
		Timestamp:    start,
		EndTime:      start.Add(2 * time.Second),
		TraceID:      "trace-a",
		SpanID:       "span-a",
		ParentSpanID: "span-root",
		Service:      "api",
		Name:         "GET /orders",
		StatusCode:   "OK",
		Attributes:   map[string]string{"http.method": "GET"},
	}}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.inserts) != 1 {
		t.Fatalf("inserts = %d, want 1", len(fake.inserts))
	}
	insert := fake.inserts[0]
	for _, want := range []string{`"tenant":"tenant-a"`, `"trace_id":"trace-a"`, `"timestamp":"2026-11-04 12:00:00.123456789"`, `"http.method":"GET"`} {
		if !strings.Contains(insert, want) {
			t.Fatalf("insert body %q missing %q", insert, want)
		}
	}
}

func TestTraceStoreQueryAndStream(t *testing.T) {
	store, _ := newFakeStore(t)
	ctx := context.Background()
	records, err := store.QueryTraces(ctx, "t1", "api", time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("QueryTraces() returned %d records, want 1", len(records))
	}
	if records[0].TraceID != "t1" || records[0].Attributes["http.method"] != "GET" {
		t.Fatalf("record = %+v", records[0])
	}
	if !records[0].Timestamp.Equal(time.Unix(0, 100)) || !records[0].EndTime.Equal(time.Unix(0, 200)) {
		t.Fatalf("timestamps = %v..%v", records[0].Timestamp, records[0].EndTime)
	}

	var streamed []logengine.SpanRecord
	if err := store.StreamTraces(ctx, "", "", time.Time{}, time.Time{}, 5, func(record logengine.SpanRecord) error {
		streamed = append(streamed, record)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 1 {
		t.Fatalf("StreamTraces() returned %d records, want 1", len(streamed))
	}
}

func TestTraceStoreDiscovery(t *testing.T) {
	store, _ := newFakeStore(t)
	ctx := context.Background()
	services, err := store.DiscoverTraceServices(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 || services[0] != "api" || services[1] != "worker" {
		t.Fatalf("services = %#v", services)
	}
	tags, err := store.DiscoverTraceTags(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("tags = %#v", tags)
	}
	values, err := store.DiscoverTraceTagValues(ctx, time.Time{}, time.Time{}, "http.method")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0] != "GET" {
		t.Fatalf("tag values = %#v", values)
	}
}

func TestTraceStoreLogs(t *testing.T) {
	store, fake := newFakeStore(t)
	ctx := context.Background()
	if err := store.IngestLogs(ctx, "chunk", []logengine.LogRecord{{
		Timestamp:  time.Unix(0, 300),
		Level:      "error",
		Service:    "api",
		TraceID:    "t1",
		SpanID:     "s1",
		RawMessage: "boom",
		Labels:     map[string]string{"env": "prod"},
	}}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	insert := fake.inserts[0]
	fake.mu.Unlock()
	for _, want := range []string{`"raw_message":"boom"`, `"env":"prod"`, `"tenant":"tenant-a"`} {
		if !strings.Contains(insert, want) {
			t.Fatalf("log insert %q missing %q", insert, want)
		}
	}
	records, err := store.QueryLogs(ctx, `{service="api"} |= "boom"`, "", time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RawMessage != "boom" || records[0].Labels["env"] != "prod" {
		t.Fatalf("QueryLogs() = %+v", records)
	}
}

func TestTraceStoreMetricsAndSeries(t *testing.T) {
	store, fake := newFakeStore(t)
	ctx := context.Background()
	if err := store.IngestMetrics(ctx, "chunk", []logengine.MetricRecord{{
		Timestamp:  time.Unix(0, 200),
		Service:    "api",
		MetricName: "requests_total",
		Value:      3.5,
		Labels:     map[string]string{"env": "prod"},
	}}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	insert := fake.inserts[0]
	fake.mu.Unlock()
	if !strings.Contains(insert, `"metric_name":"requests_total"`) || !strings.Contains(insert, `"value":3.5`) {
		t.Fatalf("metric insert = %q", insert)
	}
	records, err := store.QueryMetrics(ctx, logengine.MetricQuery{MetricName: "requests_total"}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Value != 3.5 || records[0].Labels["env"] != "prod" {
		t.Fatalf("QueryMetrics() = %+v", records)
	}
	var series []logengine.MetricSeriesMeta
	if err := store.StreamMetricSeries(ctx, logengine.MetricQuery{}, time.Time{}, time.Time{}, func(meta logengine.MetricSeriesMeta) error {
		series = append(series, meta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].MetricName != "requests_total" {
		t.Fatalf("StreamMetricSeries() = %+v", series)
	}
}

func TestTraceStoreMetricDiscovery(t *testing.T) {
	store, _ := newFakeStore(t)
	ctx := context.Background()
	names, err := store.QueryMetrics(ctx, logengine.MetricQuery{
		LabelMatchers: []logengine.MetricLabelMatcher{{
			Name: logengine.MetricDiscoveryMatcherName, Value: logengine.MetricDiscoveryModeNames, Type: logengine.MetricMatchEqual,
		}},
	}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].MetricName != "requests_total" {
		t.Fatalf("name discovery = %+v", names)
	}
	labels, err := store.QueryMetrics(ctx, logengine.MetricQuery{
		LabelMatchers: []logengine.MetricLabelMatcher{{
			Name: logengine.MetricDiscoveryMatcherName, Value: logengine.MetricDiscoveryModeLabels, Type: logengine.MetricMatchEqual,
		}},
	}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0].Labels["env"] != "" {
		t.Fatalf("label discovery = %+v", labels)
	}
	values, err := store.QueryMetrics(ctx, logengine.MetricQuery{
		LabelMatchers: []logengine.MetricLabelMatcher{{
			Name: logengine.MetricDiscoveryMatcherName, Value: logengine.MetricDiscoveryModeValues + "env", Type: logengine.MetricMatchEqual,
		}},
	}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Labels["env"] != "prod" {
		t.Fatalf("value discovery = %+v", values)
	}
}

func TestQueryDuckDBSQLRunsAgainstClickHouseView(t *testing.T) {
	store, fake := newFakeStore(t)
	from := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	result, err := store.QueryDuckDBSQL(context.Background(), "logs",
		"SELECT service, count(*) AS n FROM logs GROUP BY service ORDER BY n DESC",
		from, from.Add(time.Hour), 10,
		logengine.QueryScope{Restricted: true, Partitions: []string{"prod-us"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Columns) != 2 || result.Columns[0] != "service" || result.Columns[1] != "n" {
		t.Fatalf("columns = %#v", result.Columns)
	}
	if result.RowCount != 1 || len(result.Rows) != 1 || result.Rows[0][0] != "api" {
		t.Fatalf("result = %+v", result)
	}
	if rows := result.Rows[0][1]; rows != uint64(2) {
		t.Fatalf("count = %#v (%T), want uint64(2)", rows, rows)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	var statement string
	for _, candidate := range fake.statements {
		if strings.Contains(candidate, "AS doctor_sql") {
			statement = candidate
			break
		}
	}
	if statement == "" {
		t.Fatalf("no executed SQL statement in %#v", fake.statements)
	}
	for _, want := range []string{
		"WITH logs AS (SELECT toUnixTimestamp64Nano(l.timestamp) AS timestamp",
		"toJSONString(l.labels) AS labels_json",
		"guardian.partition",
		"IN ({p0:String})",
		"AS doctor_sql LIMIT 11 FORMAT JSON",
	} {
		if !strings.Contains(statement, want) {
			t.Fatalf("statement %q missing %q", statement, want)
		}
	}
}

func TestQueryDuckDBSQLRejectsWritesAndEmptyScope(t *testing.T) {
	store, fake := newFakeStore(t)
	from := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if _, err := store.QueryDuckDBSQL(context.Background(), "logs", "DELETE FROM logs", from, from.Add(time.Hour), 10, logengine.QueryScope{}); err == nil {
		t.Fatal("expected write query to be rejected")
	}
	if _, err := store.QueryDuckDBSQL(context.Background(), "logs", "SELECT * FROM logs", from, from.Add(time.Hour), 10,
		logengine.QueryScope{Restricted: true}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var statement string
	for _, candidate := range fake.statements {
		if strings.Contains(candidate, "AS doctor_sql") {
			statement = candidate
			break
		}
	}
	if statement == "" {
		t.Fatalf("no executed SQL statement in %#v", fake.statements)
	}
	if !strings.Contains(statement, "AND 0") {
		t.Fatalf("empty scope statement %q missing AND 0", statement)
	}
}

func TestQueryDuckDBSQLRejectsRawTablesAndTableFunctions(t *testing.T) {
	store, fake := newFakeStore(t)
	from := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	fake.mu.Lock()
	fake.sqlAST = "SelectWithUnionQuery (children 1)\n TableIdentifier spans\n"
	fake.mu.Unlock()
	if _, err := store.QueryDuckDBSQL(context.Background(), "logs", "SELECT * FROM spans", from, from.Add(time.Hour), 10, logengine.QueryScope{}); err == nil {
		t.Fatal("expected raw signal table to be rejected")
	}

	fake.mu.Lock()
	fake.sqlAST = "SelectWithUnionQuery (children 1)\n Function file (children 1)\n"
	fake.mu.Unlock()
	if _, err := store.QueryDuckDBSQL(context.Background(), "logs", "SELECT * FROM file('/etc/passwd')", from, from.Add(time.Hour), 10, logengine.QueryScope{}); err == nil {
		t.Fatal("expected table function to be rejected")
	}
}

func TestTraceStoreMarshalsBatchJSONL(t *testing.T) {
	store, fake := newFakeStore(t)
	ctx := context.Background()
	spans := make([]logengine.SpanRecord, 0, 3)
	for i := 0; i < 3; i++ {
		spans = append(spans, logengine.SpanRecord{
			Timestamp: time.Unix(0, int64(i+1)),
			EndTime:   time.Unix(0, int64(i+2)),
			TraceID:   "t",
			SpanID:    "s",
			Service:   "svc",
		})
	}
	if err := store.IngestTraces(ctx, "chunk", spans); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(fake.inserts[0]), "\n")
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("insert line is not JSON: %v (%q)", err, line)
		}
	}
}

func TestTraceStorePushesPartitionScopeToSQL(t *testing.T) {
	store, fake := newFakeStore(t)
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ctx := logengine.WithQueryScope(context.Background(), logengine.QueryScope{
		Restricted: true,
		Partitions: []string{"match"},
	})

	returned := 0
	err := store.StreamTraces(ctx, "", "", base, base.Add(time.Hour), 10, func(logengine.SpanRecord) error {
		returned++
		return nil
	})
	if err != nil {
		t.Fatalf("StreamTraces() error = %v", err)
	}
	// The fake row has no partition label. The SQL predicate over-fetches it
	// deliberately; the record guard must drop it.
	if returned != 0 {
		t.Fatalf("returned %d unpartitioned spans, want 0", returned)
	}

	fake.mu.Lock()
	statement := fake.statements[len(fake.statements)-1]
	params := fake.params[len(fake.params)-1]
	fake.mu.Unlock()
	if !strings.Contains(statement, "guardian_partition") || !strings.Contains(statement, "guardian.partition") {
		t.Fatalf("span query missing partition predicate: %s", statement)
	}
	if !strings.Contains(statement, "IN ({scope_0:String})") {
		t.Fatalf("span query missing scope parameter: %s", statement)
	}
	if params.Get("param_scope_0") != "match" {
		t.Fatalf("scope parameter = %q, want match", params.Get("param_scope_0"))
	}
}

func TestLogStorePushesPartitionScopeToSQL(t *testing.T) {
	store, fake := newFakeStore(t)
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ctx := logengine.WithQueryScope(context.Background(), logengine.QueryScope{
		Restricted: true,
		Partitions: []string{"match"},
	})

	returned := 0
	err := store.StreamLogs(ctx, `{service=~".+"}`, "", base, base.Add(time.Hour), 10, func(logengine.LogRecord) error {
		returned++
		return nil
	})
	if err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	if returned != 0 {
		t.Fatalf("returned %d unpartitioned logs, want 0", returned)
	}

	fake.mu.Lock()
	statement := fake.statements[len(fake.statements)-1]
	params := fake.params[len(fake.params)-1]
	fake.mu.Unlock()
	if !strings.Contains(statement, "labels['guardian_partition']") || !strings.Contains(statement, "labels['guardian.partition']") {
		t.Fatalf("log query missing partition predicate: %s", statement)
	}
	if params.Get("param_p0") != "match" {
		t.Fatalf("scope parameter = %q, want match", params.Get("param_p0"))
	}
}

func TestClickHouseStats(t *testing.T) {
	store, fake := newFakeStore(t)
	ctx := context.Background()

	stats := store.ClickHouseStats(ctx)
	if !stats.Enabled || !stats.Healthy {
		t.Fatalf("stats enabled/healthy = %v/%v, error=%q", stats.Enabled, stats.Healthy, stats.Error)
	}
	if stats.Version != "25.8.33.6" {
		t.Fatalf("version = %q, want 25.8.33.6", stats.Version)
	}
	if stats.DiskTotalBytes != 1000 || stats.DiskFreeBytes != 400 || stats.DiskUsedBytes != 600 {
		t.Fatalf("disk stats = total %d free %d used %d", stats.DiskTotalBytes, stats.DiskFreeBytes, stats.DiskUsedBytes)
	}
	if stats.SpansRows != 10 || stats.SpansBytes != 100 {
		t.Fatalf("spans stats = %d rows %d bytes", stats.SpansRows, stats.SpansBytes)
	}
	if stats.LogsRows != 5 || stats.LogsBytes != 50 {
		t.Fatalf("logs stats = %d rows %d bytes", stats.LogsRows, stats.LogsBytes)
	}
	if stats.TotalRows != 15 || stats.TotalBytes != 150 || stats.Parts != 3 {
		t.Fatalf("totals = %d rows %d bytes %d parts", stats.TotalRows, stats.TotalBytes, stats.Parts)
	}
	if stats.RetentionSeconds != int64((30*24*time.Hour)/time.Second) {
		t.Fatalf("retention = %d", stats.RetentionSeconds)
	}

	// A second call inside the cache window must not hit ClickHouse again.
	fake.mu.Lock()
	before := len(fake.statements)
	fake.mu.Unlock()
	_ = store.ClickHouseStats(ctx)
	fake.mu.Lock()
	after := len(fake.statements)
	fake.mu.Unlock()
	if after != before {
		t.Fatalf("stats cache issued %d extra queries", after-before)
	}
}
