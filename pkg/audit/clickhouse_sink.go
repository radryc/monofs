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
	"strings"
	"time"
)

// AuditEventsDDL is the append-only ClickHouse MergeTree schema for audit.events.
const AuditEventsDDL = `CREATE TABLE IF NOT EXISTS audit.events (
    event_id String,
    timestamp DateTime64(6, 'UTC'),
    actor_subject String,
    actor_client_id String,
    action LowCardinality(String),
    resource String,
    partition LowCardinality(String),
    decision LowCardinality(String),
    correlation_id String,
    source_ip String,
    prev_hash String,
    hash String
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY (timestamp, event_id)`

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
	u.RawQuery = q.Encode()

	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
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
