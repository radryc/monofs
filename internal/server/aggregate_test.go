package server

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
)

func TestAggregateTelemetryHandler(t *testing.T) {
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	backend := &doctorStreamBackendStub{
		aggregateRows: []logengine.AggregateRow{{
			Group:  map[string]string{"service": "guardian"},
			Values: map[string]any{"count": int64(3)},
		}},
	}
	srv := &Server{logEngine: backend}
	resp, err := srv.AggregateTelemetry(context.Background(), &pb.AggregateTelemetryRequest{
		Signal:               "logs",
		FromUnixNano:         base.UnixNano(),
		ToUnixNano:           base.Add(time.Hour).UnixNano(),
		GroupBy:              []string{"service"},
		VisiblePartitions:    []string{"prod-eu"},
		VisiblePartitionsSet: true,
		Aggregates:           []*pb.AggregateSpec{{Op: pb.AggregateOp_AGGREGATE_OP_COUNT, Alias: "count"}},
	})
	if err != nil {
		t.Fatalf("AggregateTelemetry() error = %v", err)
	}
	if !backend.lastAggregate.Partial {
		t.Fatal("shard aggregate must be partial")
	}
	if len(backend.lastAggregate.GroupBy) != 1 || backend.lastAggregate.GroupBy[0] != "service" {
		t.Fatalf("group_by = %v", backend.lastAggregate.GroupBy)
	}
	var rows []logengine.AggregateRow
	decoder := json.NewDecoder(bytes.NewReader(resp.GetRowsJson()))
	decoder.UseNumber()
	if err := decoder.Decode(&rows); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Values["count"].(json.Number).String() != "3" {
		t.Fatalf("rows = %+v", rows)
	}
}
