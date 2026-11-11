//go:build cgo && duckdb

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"github.com/radryc/monofs/internal/telemetrycrypto"
)

func TestSidecarQueryLogsAndScope(t *testing.T) {
	dir := t.TempDir()
	engine := logengine.New(logengine.NewMockS3Store(filepath.Join(dir, "remote")), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "cache"),
	})
	if err := engine.EnableDuckDB(logengine.DuckDBConfig{
		MemoryLimit: "256MB",
		Threads:     2,
		TempDir:     filepath.Join(dir, "duckdb-tmp"),
	}); err != nil {
		t.Fatalf("EnableDuckDB() error = %v", err)
	}
	defer engine.Close()

	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(context.Background(), "chunk-sidecar", []logengine.LogRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", TraceID: "a", RawMessage: "timeout", Labels: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(2 * time.Minute), Service: "guardian", TraceID: "b", RawMessage: "ok", Labels: map[string]string{"guardian.partition": "prod-us"}},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}

	server := &logQueryServer{engine: engine}
	resp, err := server.QueryLogs(context.Background(), &pb.QueryLogsRequest{
		Query:                `{}`,
		FromUnixNano:         base.UnixNano(),
		ToUnixNano:           base.Add(time.Hour).UnixNano(),
		VisiblePartitions:    []string{"prod-us"},
		VisiblePartitionsSet: true,
	})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	var records []logengine.LogRecord
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("decode results: %v", err)
	}
	if len(records) != 1 || records[0].TraceID != "b" {
		t.Fatalf("scoped results = %+v, want trace b", records)
	}
}

func TestSidecarAggregateTelemetry(t *testing.T) {
	dir := t.TempDir()
	engine := logengine.New(logengine.NewMockS3Store(filepath.Join(dir, "remote")), logengine.Config{LocalCacheDir: filepath.Join(dir, "cache")})
	if err := engine.EnableDuckDB(logengine.DuckDBConfig{MemoryLimit: "256MB", Threads: 2, TempDir: filepath.Join(dir, "duckdb-tmp")}); err != nil {
		t.Fatalf("EnableDuckDB() error = %v", err)
	}
	defer engine.Close()
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := engine.IngestLogs(context.Background(), "chunk-sidecar-aggregate", []logengine.LogRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", RawMessage: "a", Labels: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(2 * time.Minute), Service: "guardian", RawMessage: "b", Labels: map[string]string{"guardian.partition": "prod-eu"}},
		{Timestamp: base.Add(3 * time.Minute), Service: "doctor", RawMessage: "c", Labels: map[string]string{"guardian.partition": "prod-eu"}},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	server := &logQueryServer{engine: engine}
	resp, err := server.AggregateTelemetry(context.Background(), &pb.AggregateTelemetryRequest{
		Signal:       "logs",
		FromUnixNano: base.UnixNano(),
		ToUnixNano:   base.Add(time.Hour).UnixNano(),
		GroupBy:      []string{"service"},
		Aggregates:   []*pb.AggregateSpec{{Op: pb.AggregateOp_AGGREGATE_OP_COUNT, Alias: "count"}},
	})
	if err != nil {
		t.Fatalf("AggregateTelemetry() error = %v", err)
	}
	var rows []logengine.AggregateRow
	decoder := json.NewDecoder(bytes.NewReader(resp.GetRowsJson()))
	decoder.UseNumber()
	if err := decoder.Decode(&rows); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 groups", rows)
	}
}

func testEncryptionProvider(t *testing.T, mode telemetrycrypto.Mode, write, read bool) *telemetrycrypto.Provider {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	cfg, err := telemetrycrypto.ParseKeyFile([]byte(`{
		"version": 1,
		"tenant": "default",
		"root_key_id": "root-v1",
		"root_key": "` + key + `"
	}`))
	if err != nil {
		t.Fatalf("ParseKeyFile() error = %v", err)
	}
	cfg.Enabled = write
	cfg.DecryptOnly = read && !write
	cfg.Mode = mode
	provider, err := telemetrycrypto.NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	return provider
}

func TestSidecarReadsEncryptedChunks(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote")

	writer := logengine.New(logengine.NewMockS3Store(remote), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "writer-cache"),
		Encryption:    testEncryptionProvider(t, telemetrycrypto.ModeObserve, true, true),
	})
	defer writer.Close()
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := writer.IngestLogs(context.Background(), "chunk-encrypted", []logengine.LogRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", TraceID: "enc", RawMessage: "encrypted record"},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	if err := writer.IngestMetrics(context.Background(), "metrics-encrypted", []logengine.MetricRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", MetricName: "requests", Value: 3},
	}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}

	reader := logengine.New(logengine.NewMockS3Store(remote), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "reader-cache"),
		Encryption:    testEncryptionProvider(t, telemetrycrypto.ModeObserve, false, true),
	})
	defer reader.Close()
	if err := reader.EnableDuckDB(logengine.DuckDBConfig{
		MemoryLimit: "256MB",
		Threads:     2,
		TempDir:     filepath.Join(dir, "duckdb-tmp"),
	}); err != nil {
		t.Fatalf("EnableDuckDB() error = %v", err)
	}

	server := &logQueryServer{engine: reader}
	resp, err := server.QueryLogs(context.Background(), &pb.QueryLogsRequest{
		FromUnixNano: base.UnixNano(),
		ToUnixNano:   base.Add(time.Hour).UnixNano(),
	})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	var records []logengine.LogRecord
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(records) != 1 || records[0].TraceID != "enc" {
		t.Fatalf("encrypted logs = %+v, want trace enc", records)
	}

	sqlResp, err := server.QueryDuckDBSQL(context.Background(), &pb.QueryDuckDBSQLRequest{
		Dataset:      "logs",
		Query:        "SELECT count(*) AS n FROM logs",
		FromUnixNano: base.UnixNano(),
		ToUnixNano:   base.Add(time.Hour).UnixNano(),
	})
	if err != nil {
		t.Fatalf("QueryDuckDBSQL() error = %v", err)
	}
	if !strings.Contains(sqlResp.GetResultsJson(), "1") {
		t.Fatalf("duckdb results = %q, want a count of 1", sqlResp.GetResultsJson())
	}
}

func TestSidecarNativeReadsEncryptedChunks(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote")
	writer := logengine.New(logengine.NewMockS3Store(remote), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "writer-cache"),
		Encryption:    testEncryptionProvider(t, telemetrycrypto.ModeObserve, true, true),
	})
	defer writer.Close()
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := writer.IngestLogs(context.Background(), "chunk-native-encrypted", []logengine.LogRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", TraceID: "native", RawMessage: "native encrypted"},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	if err := writer.IngestMetrics(context.Background(), "chunk-native-metrics", []logengine.MetricRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", MetricName: "requests", Value: 2},
	}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}

	reader := logengine.New(logengine.NewMockS3Store(remote), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "reader-cache"),
		Encryption:    testEncryptionProvider(t, telemetrycrypto.ModeObserve, false, true),
		QueryMode:     logengine.QueryModeNative,
	})
	defer reader.Close()
	server := &logQueryServer{engine: reader}
	resp, err := server.QueryLogs(context.Background(), &pb.QueryLogsRequest{
		FromUnixNano: base.UnixNano(),
		ToUnixNano:   base.Add(time.Hour).UnixNano(),
	})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	var records []logengine.LogRecord
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(records) != 1 || records[0].TraceID != "native" {
		t.Fatalf("native encrypted logs = %+v, want trace native", records)
	}

	metrics, err := server.QueryMetrics(context.Background(), &pb.QueryMetricsRequest{
		FromUnixNano: base.UnixNano(),
		ToUnixNano:   base.Add(time.Hour).UnixNano(),
	})
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}
	if !strings.Contains(string(metrics.GetResultsJson()), "requests") {
		t.Fatalf("metrics results = %q, want requests", metrics.GetResultsJson())
	}
}

func TestSidecarReadsLegacyPlaintextInObserveMode(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote")
	writer := logengine.New(logengine.NewMockS3Store(remote), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "writer-cache"),
	})
	defer writer.Close()
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := writer.IngestLogs(context.Background(), "chunk-plaintext", []logengine.LogRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", TraceID: "plain", RawMessage: "legacy plaintext"},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}

	reader := logengine.New(logengine.NewMockS3Store(remote), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "reader-cache"),
		Encryption:    testEncryptionProvider(t, telemetrycrypto.ModeObserve, false, true),
	})
	defer reader.Close()
	server := &logQueryServer{engine: reader}
	resp, err := server.QueryLogs(context.Background(), &pb.QueryLogsRequest{
		FromUnixNano: base.UnixNano(),
		ToUnixNano:   base.Add(time.Hour).UnixNano(),
	})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	var records []logengine.LogRecord
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(records) != 1 || records[0].TraceID != "plain" {
		t.Fatalf("plaintext logs = %+v, want trace plain", records)
	}
}

func TestSidecarEnforceRejectsLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote")
	writer := logengine.New(logengine.NewMockS3Store(remote), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "writer-cache"),
	})
	defer writer.Close()
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := writer.IngestLogs(context.Background(), "chunk-plaintext-enforce", []logengine.LogRecord{
		{Timestamp: base.Add(time.Minute), Service: "guardian", TraceID: "plain", RawMessage: "legacy plaintext"},
	}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}

	reader := logengine.New(logengine.NewMockS3Store(remote), logengine.Config{
		LocalCacheDir: filepath.Join(dir, "reader-cache"),
		Encryption:    testEncryptionProvider(t, telemetrycrypto.ModeEnforce, false, true),
	})
	defer reader.Close()
	server := &logQueryServer{engine: reader}
	if _, err := server.QueryLogs(context.Background(), &pb.QueryLogsRequest{
		FromUnixNano: base.UnixNano(),
		ToUnixNano:   base.Add(time.Hour).UnixNano(),
	}); err == nil {
		t.Fatal("expected enforce mode to reject plaintext chunks")
	}
}
