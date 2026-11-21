package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AuditEventsDDL is the append-only ClickHouse MergeTree schema for audit.events.
// It matches the Event JSON contract and keeps raw JSON as the evidence of
// record: verification re-hashes raw_json and cross-checks the indexed columns.
const AuditEventsDDL = `CREATE DATABASE IF NOT EXISTS audit;

CREATE TABLE IF NOT EXISTS audit.events
(
    timestamp      DateTime64(6, 'UTC'),
    day            Date DEFAULT toDate(timestamp),
    event_id       String,
    writer_id      LowCardinality(String),
    seq            UInt64,
    prev_hash      String,
    hash           String,
    service        LowCardinality(String),
    env            LowCardinality(String),
    scope          LowCardinality(String),
    action         LowCardinality(String),
    decision       LowCardinality(String),
    reason         String,
    resource_type  LowCardinality(String),
    actor_subject  String,
    actor_client_id String,
    email          String,
    groups         Array(String),
    tenant         LowCardinality(String),
    resource       String CODEC(ZSTD(3)),
    partition      LowCardinality(String),
    resource_hash  String,
    request_id     String,
    trace_id       String,
    method         LowCardinality(String),
    path           String,
    status         UInt16,
    result_count   UInt64,
    bytes          UInt64,
    occurrences    UInt32 DEFAULT 1,
    source_ip      String,
    user_agent     String CODEC(ZSTD(3)),
    policy_version String,
    detail         String CODEC(ZSTD(3)),
    correlation_id String,
    raw_json       String CODEC(ZSTD(3))
) ENGINE = MergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (day, service, scope, actor_subject, resource_hash, seq)
SETTINGS index_granularity = 8192`

// ClickHouseSink ships batches of audit events to ClickHouse over its HTTP
// interface using INSERT INTO <database>.events FORMAT JSONEachRow.
type ClickHouseSink struct {
	endpoint string
	database string
	user     string
	password string
	client   *http.Client
}

var _ Sink = (*ClickHouseSink)(nil)

// NewClickHouseSink constructs a ClickHouseSink targeting the given HTTP
// endpoint and database. If database is empty, it defaults to "audit".
func NewClickHouseSink(endpoint, database, user, password string) *ClickHouseSink {
	db := strings.TrimSpace(database)
	if db == "" {
		db = "audit"
	}
	return &ClickHouseSink{
		endpoint: strings.TrimRight(strings.TrimSpace(endpoint), "/"),
		database: db,
		user:     user,
		password: password,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// WriteBatch writes events to ClickHouse as JSONEachRow via HTTP POST.
func (s *ClickHouseSink) WriteBatch(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	if s.endpoint == "" {
		return errors.New("audit: clickhouse endpoint must not be empty")
	}

	u, err := url.Parse(s.endpoint)
	if err != nil {
		return fmt.Errorf("audit: parse clickhouse endpoint: %w", err)
	}
	sql := fmt.Sprintf("INSERT INTO %s.events FORMAT JSONEachRow", s.database)
	q := u.Query()
	q.Set("query", sql)
	q.Set("date_time_input_format", "best_effort")
	u.RawQuery = q.Encode()

	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, ev := range events {
		raw, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("audit: marshal event for clickhouse: %w", err)
		}
		row := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &row); err != nil {
			return fmt.Errorf("audit: decode event row: %w", err)
		}
		rawJSON := ev.RawJSON
		if rawJSON == "" {
			rawJSON = string(raw)
		}
		row["raw_json"] = json.RawMessage(strconv.Quote(rawJSON))
		if err := enc.Encode(row); err != nil {
			return fmt.Errorf("audit: marshal event for clickhouse: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return fmt.Errorf("audit: build clickhouse request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if s.user != "" || s.password != "" {
		req.SetBasicAuth(s.user, s.password)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("audit: clickhouse post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("audit: clickhouse insert failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
