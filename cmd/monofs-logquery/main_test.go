//go:build cgo && duckdb

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
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
