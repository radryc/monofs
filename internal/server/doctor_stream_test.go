package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type doctorStreamBackendStub struct {
	logs    []logengine.LogRecord
	metrics []logengine.MetricRecord
	traces  []logengine.SpanRecord

	queryLogsCalls    int
	streamLogsCalls   int
	queryMetricCalls  int
	streamMetricCalls int
	queryTraceCalls   int
	streamTraceCalls  int
}

func (s *doctorStreamBackendStub) IngestLogs(context.Context, string, []logengine.LogRecord) error {
	return nil
}

func (s *doctorStreamBackendStub) IngestMetrics(context.Context, string, []logengine.MetricRecord) error {
	return nil
}

func (s *doctorStreamBackendStub) IngestTraces(context.Context, string, []logengine.SpanRecord) error {
	return nil
}

func (s *doctorStreamBackendStub) QueryLogs(context.Context, string, string, time.Time, time.Time, int) ([]logengine.LogRecord, error) {
	s.queryLogsCalls++
	return append([]logengine.LogRecord(nil), s.logs...), nil
}

func (s *doctorStreamBackendStub) StreamLogs(_ context.Context, _ string, _ string, _ time.Time, _ time.Time, _ int, yield func(logengine.LogRecord) error) error {
	s.streamLogsCalls++
	for _, record := range s.logs {
		if err := yield(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *doctorStreamBackendStub) QueryMetrics(context.Context, logengine.MetricQuery, time.Time, time.Time) ([]logengine.MetricRecord, error) {
	s.queryMetricCalls++
	return append([]logengine.MetricRecord(nil), s.metrics...), nil
}

func (s *doctorStreamBackendStub) StreamMetrics(_ context.Context, _ logengine.MetricQuery, _ time.Time, _ time.Time, yield func(logengine.MetricRecord) error) error {
	s.streamMetricCalls++
	for _, record := range s.metrics {
		if err := yield(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *doctorStreamBackendStub) QueryTraces(context.Context, string, string, time.Time, time.Time, int) ([]logengine.SpanRecord, error) {
	s.queryTraceCalls++
	return append([]logengine.SpanRecord(nil), s.traces...), nil
}

func (s *doctorStreamBackendStub) StreamTraces(_ context.Context, _ string, _ string, _ time.Time, _ time.Time, _ int, yield func(logengine.SpanRecord) error) error {
	s.streamTraceCalls++
	for _, record := range s.traces {
		if err := yield(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *doctorStreamBackendStub) Stats(context.Context) (logengine.LogEngineStats, error) {
	return logengine.LogEngineStats{}, nil
}

func TestStreamQueryLogsStreamsIndividualJSONItems(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	backend := &doctorStreamBackendStub{logs: []logengine.LogRecord{
		{Timestamp: time.Unix(10, 0).UTC(), Level: "info", Service: "doctor", TraceID: "tr-1", RawMessage: "first"},
		{Timestamp: time.Unix(20, 0).UTC(), Level: "warn", Service: "doctor", TraceID: "tr-2", RawMessage: "second"},
	}}
	grpcServer := grpc.NewServer()
	pb.RegisterMonoFSServer(grpcServer, &Server{logEngine: backend})
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	defer grpcServer.Stop()

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.DialContext() error = %v", err)
	}
	defer conn.Close()

	stream, err := pb.NewMonoFSClient(conn).StreamQueryLogs(context.Background(), &pb.QueryLogsRequest{Query: `{service="doctor"}`})
	if err != nil {
		t.Fatalf("StreamQueryLogs() error = %v", err)
	}

	var items []map[string]any
	for {
		item, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("stream.Recv() error = %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(item.GetItemJson(), &decoded); err != nil {
			t.Fatalf("json.Unmarshal(item) error = %v", err)
		}
		items = append(items, decoded)
	}

	if len(items) != 2 {
		t.Fatalf("streamed item count = %d, want 2", len(items))
	}
	if items[0]["body"] != "first" {
		t.Fatalf("first item body = %v, want first", items[0]["body"])
	}
	if items[1]["body"] != "second" {
		t.Fatalf("second item body = %v, want second", items[1]["body"])
	}
	if backend.streamLogsCalls != 1 || backend.queryLogsCalls != 0 {
		t.Fatalf("log stream/query calls = %d/%d, want 1/0", backend.streamLogsCalls, backend.queryLogsCalls)
	}
}

func TestStreamQueryMetricsUsesBackendStreamPath(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	backend := &doctorStreamBackendStub{metrics: []logengine.MetricRecord{{
		Timestamp:  time.Unix(30, 0).UTC(),
		Service:    "doctor",
		MetricName: "requests_total",
		Value:      3,
		Labels:     map[string]string{"env": "prod"},
	}}}
	grpcServer := grpc.NewServer()
	pb.RegisterMonoFSServer(grpcServer, &Server{logEngine: backend})
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	defer grpcServer.Stop()

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.DialContext() error = %v", err)
	}
	defer conn.Close()

	stream, err := pb.NewMonoFSClient(conn).StreamQueryMetrics(context.Background(), &pb.QueryMetricsRequest{MetricName: "requests_total"})
	if err != nil {
		t.Fatalf("StreamQueryMetrics() error = %v", err)
	}

	item, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream.Recv() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(item.GetItemJson(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal(item) error = %v", err)
	}
	if decoded["MetricName"] != "requests_total" {
		t.Fatalf("metric name = %v, want requests_total", decoded["MetricName"])
	}
	if backend.streamMetricCalls != 1 || backend.queryMetricCalls != 0 {
		t.Fatalf("metric stream/query calls = %d/%d, want 1/0", backend.streamMetricCalls, backend.queryMetricCalls)
	}
}

func TestStreamQueryTracesUsesBackendStreamPath(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	backend := &doctorStreamBackendStub{traces: []logengine.SpanRecord{{
		Timestamp: time.Unix(40, 0).UTC(),
		EndTime:   time.Unix(41, 0).UTC(),
		TraceID:   "trace-1",
		SpanID:    "span-1",
		Service:   "doctor",
		Name:      "query",
	}}}
	grpcServer := grpc.NewServer()
	pb.RegisterMonoFSServer(grpcServer, &Server{logEngine: backend})
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	defer grpcServer.Stop()

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.DialContext() error = %v", err)
	}
	defer conn.Close()

	stream, err := pb.NewMonoFSClient(conn).StreamQueryTraces(context.Background(), &pb.QueryTracesRequest{TraceId: "trace-1"})
	if err != nil {
		t.Fatalf("StreamQueryTraces() error = %v", err)
	}

	item, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream.Recv() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(item.GetItemJson(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal(item) error = %v", err)
	}
	if decoded["trace_id"] != "trace-1" {
		t.Fatalf("trace id = %v, want trace-1", decoded["trace_id"])
	}
	if backend.streamTraceCalls != 1 || backend.queryTraceCalls != 0 {
		t.Fatalf("trace stream/query calls = %d/%d, want 1/0", backend.streamTraceCalls, backend.queryTraceCalls)
	}
}
