package clickhouse

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage/logengine"
	"github.com/radryc/monofs/internal/storage/logquery"
	"github.com/radryc/monofs/internal/telemetrycrypto"
)

func TestClickHouseIngestEncryptsLogsAndSpanNames(t *testing.T) {
	store, fake := newFakeStore(t)
	store.cfg.Encryption = testClickHouseProvider(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	if err := store.IngestLogs(ctx, "chunk-1", []logengine.LogRecord{{
		Timestamp:  start,
		Level:      "error",
		Service:    "diva-shared.app.service",
		RawMessage: "payment declined for order 42",
		Labels:     map[string]string{"guardian.partition": "diva-shared"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.IngestTraces(ctx, "chunk-2", []logengine.SpanRecord{{
		Timestamp:  start,
		EndTime:    start.Add(time.Second),
		TraceID:    "trace-1",
		SpanID:     "span-1",
		Service:    "diva-shared.app.service",
		Name:       "POST /checkout",
		Attributes: map[string]string{"guardian.partition": "diva-shared"},
	}}); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	inserts := strings.Join(fake.inserts, "\n")
	fake.mu.Unlock()
	if !strings.Contains(inserts, "enc:v1:") {
		t.Fatalf("inserts do not contain encrypted fields: %s", inserts)
	}
	if strings.Contains(inserts, "payment declined") || strings.Contains(inserts, "POST /checkout") {
		t.Fatalf("inserts contain plaintext: %s", inserts)
	}
}

func TestClickHouseSpanNameDecrypts(t *testing.T) {
	provider := testClickHouseProvider(t)
	sealed, err := provider.EncryptField("POST /checkout", "default", "diva-shared", "spans.name")
	if err != nil {
		t.Fatal(err)
	}
	row := traceRow{
		TimestampNS: 1,
		EndTimeNS:   2,
		TraceID:     "trace-1",
		SpanID:      "span-1",
		Service:     "diva-shared.app.service",
		Name:        sealed,
		StatusCode:  "OK",
		Attributes:  `{"guardian.partition":"diva-shared"}`,
	}
	record, err := row.record(provider)
	if err != nil {
		t.Fatal(err)
	}
	if record.Name != "POST /checkout" {
		t.Fatalf("name = %q", record.Name)
	}
}

func TestLineFiltersMatchAfterDecrypt(t *testing.T) {
	filters := []logquery.LineFilter{
		{Op: "|=", Value: "declined"},
	}
	matched, err := lineFiltersMatch(filters, "payment declined for order 42")
	if err != nil || !matched {
		t.Fatalf("matched = %v, err = %v", matched, err)
	}
	matched, err = lineFiltersMatch(filters, "payment accepted")
	if err != nil || matched {
		t.Fatalf("matched = %v, err = %v", matched, err)
	}
}

func testClickHouseProvider(t *testing.T) *telemetrycrypto.Provider {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	cfg, err := telemetrycrypto.ParseKeyFile([]byte(`{
		"version": 1,
		"tenant": "default",
		"root_key_id": "root-v1",
		"root_key": "` + key + `"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	provider, err := telemetrycrypto.NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
