package router

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type telemetryTestNodeServer struct {
	pb.UnimplementedMonoFSServer

	mu sync.Mutex

	logChunks    []string
	metricChunks []string
	traceChunks  []string

	logResults    []byte
	metricResults []byte
	traceResults  []byte
	sqlResults    []byte
	aggregateRows []byte
	sqlCalls      int

	streamGateOnce sync.Once
	streamBlocked  chan struct{}
	streamRelease  <-chan struct{}
}

type queryResultItemCollector struct {
	ctx   context.Context
	items [][]byte
}

func (c *queryResultItemCollector) Send(item *pb.QueryResultItem) error {
	c.items = append(c.items, append([]byte(nil), item.GetItemJson()...))
	return nil
}

func (c *queryResultItemCollector) SetHeader(metadata.MD) error { return nil }

func (c *queryResultItemCollector) SendHeader(metadata.MD) error { return nil }

func (c *queryResultItemCollector) SetTrailer(metadata.MD) {}

func (c *queryResultItemCollector) Context() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

func (c *queryResultItemCollector) SendMsg(any) error { return nil }

func (c *queryResultItemCollector) RecvMsg(any) error { return nil }

func streamTelemetryResults(payload []byte, send func(*pb.QueryResultItem) error) error {
	var items []json.RawMessage
	if err := json.Unmarshal(payload, &items); err != nil {
		return err
	}
	for _, item := range items {
		if err := send(&pb.QueryResultItem{ItemJson: append([]byte(nil), item...)}); err != nil {
			return err
		}
	}
	return nil
}

func (s *telemetryTestNodeServer) IngestLogs(_ context.Context, req *pb.IngestLogsRequest) (*pb.IngestLogsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logChunks = append(s.logChunks, req.GetChunkId())
	return &pb.IngestLogsResponse{Ok: true}, nil
}

func (s *telemetryTestNodeServer) IngestMetrics(_ context.Context, req *pb.IngestMetricsRequest) (*pb.IngestMetricsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metricChunks = append(s.metricChunks, req.GetChunkId())
	return &pb.IngestMetricsResponse{Ok: true}, nil
}

func (s *telemetryTestNodeServer) IngestTraces(_ context.Context, req *pb.IngestTracesRequest) (*pb.IngestTracesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traceChunks = append(s.traceChunks, req.GetChunkId())
	return &pb.IngestTracesResponse{Ok: true}, nil
}

func (s *telemetryTestNodeServer) QueryLogs(context.Context, *pb.QueryLogsRequest) (*pb.QueryLogsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.QueryLogsResponse{ResultsJson: append([]byte(nil), s.logResults...)}, nil
}

func (s *telemetryTestNodeServer) blockStream(streamCtx context.Context) error {
	s.streamGateOnce.Do(func() {
		if s.streamBlocked != nil {
			close(s.streamBlocked)
		}
	})
	if s.streamRelease == nil {
		return nil
	}
	select {
	case <-s.streamRelease:
		return nil
	case <-streamCtx.Done():
		return streamCtx.Err()
	}
}

func (s *telemetryTestNodeServer) StreamQueryLogs(_ *pb.QueryLogsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	s.mu.Lock()
	results := append([]byte(nil), s.logResults...)
	s.mu.Unlock()
	if err := s.blockStream(stream.Context()); err != nil {
		return err
	}
	return streamTelemetryResults(results, stream.Send)
}

func (s *telemetryTestNodeServer) QueryDuckDBSQL(context.Context, *pb.QueryDuckDBSQLRequest) (*pb.QueryDuckDBSQLResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sqlCalls++
	return &pb.QueryDuckDBSQLResponse{ResultsJson: string(append([]byte(nil), s.sqlResults...))}, nil
}

func (s *telemetryTestNodeServer) QueryMetrics(context.Context, *pb.QueryMetricsRequest) (*pb.QueryMetricsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.QueryMetricsResponse{ResultsJson: append([]byte(nil), s.metricResults...)}, nil
}

func (s *telemetryTestNodeServer) StreamQueryMetrics(_ *pb.QueryMetricsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	s.mu.Lock()
	results := append([]byte(nil), s.metricResults...)
	s.mu.Unlock()
	if err := s.blockStream(stream.Context()); err != nil {
		return err
	}
	return streamTelemetryResults(results, stream.Send)
}

func (s *telemetryTestNodeServer) AggregateTelemetry(context.Context, *pb.AggregateTelemetryRequest) (*pb.AggregateTelemetryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.AggregateTelemetryResponse{RowsJson: append([]byte(nil), s.aggregateRows...)}, nil
}

func (s *telemetryTestNodeServer) QueryTraces(context.Context, *pb.QueryTracesRequest) (*pb.QueryTracesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.QueryTracesResponse{ResultsJson: append([]byte(nil), s.traceResults...)}, nil
}

func (s *telemetryTestNodeServer) StreamQueryTraces(_ *pb.QueryTracesRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	s.mu.Lock()
	results := append([]byte(nil), s.traceResults...)
	s.mu.Unlock()
	if err := s.blockStream(stream.Context()); err != nil {
		return err
	}
	return streamTelemetryResults(results, stream.Send)
}

func newTelemetryNodeClient(t *testing.T, serverImpl *telemetryTestNodeServer) (pb.MonoFSClient, func()) {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterMonoFSServer(server, serverImpl)
	go func() {
		_ = server.Serve(listener)
	}()

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

	return pb.NewMonoFSClient(conn), func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	}
}

func newTelemetryRouterHarness(t *testing.T, nodeIDs ...string) (*Router, map[string]*telemetryTestNodeServer, func()) {
	t.Helper()

	router := NewRouter(DefaultRouterConfig(), nil)
	servers := make(map[string]*telemetryTestNodeServer, len(nodeIDs))
	cleanups := make([]func(), 0, len(nodeIDs))

	for _, nodeID := range nodeIDs {
		server := &telemetryTestNodeServer{
			logResults:    []byte("[]"),
			metricResults: []byte("[]"),
			traceResults:  []byte("[]"),
			aggregateRows: []byte("[]"),
		}
		client, cleanup := newTelemetryNodeClient(t, server)
		router.nodes[nodeID] = &nodeState{
			info: &pb.NodeInfo{
				NodeId:  nodeID,
				Address: "bufnet-" + nodeID,
				Healthy: true,
				Weight:  1,
			},
			client: client,
			status: NodeActive,
		}
		servers[nodeID] = server
		cleanups = append(cleanups, cleanup)
	}

	return router, servers, func() {
		_ = router.Close()
		for _, cleanup := range cleanups {
			cleanup()
		}
	}
}

func TestTelemetryIngestUsesStableShardPerSignalAndChunk(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b", "node-c")
	defer cleanup()

	ctx := context.Background()
	logChunk := "logs-chunk-42"
	logNodeID, err := router.telemetryNodeID("logs", logChunk)
	if err != nil {
		t.Fatalf("telemetryNodeID(logs) error = %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := router.IngestLogs(ctx, &pb.IngestLogsRequest{ChunkId: logChunk}); err != nil {
			t.Fatalf("IngestLogs() error = %v", err)
		}
	}
	for nodeID, server := range servers {
		server.mu.Lock()
		got := len(server.logChunks)
		server.mu.Unlock()
		if nodeID == logNodeID {
			if got != 5 {
				t.Fatalf("log shard %s got %d requests, want 5", nodeID, got)
			}
			continue
		}
		if got != 0 {
			t.Fatalf("non-owner node %s got %d log requests, want 0", nodeID, got)
		}
	}

	metricChunk := "metrics-chunk-7"
	metricNodeID, err := router.telemetryNodeID("metrics", metricChunk)
	if err != nil {
		t.Fatalf("telemetryNodeID(metrics) error = %v", err)
	}
	if _, err := router.IngestMetrics(ctx, &pb.IngestMetricsRequest{ChunkId: metricChunk}); err != nil {
		t.Fatalf("IngestMetrics() error = %v", err)
	}
	for nodeID, server := range servers {
		server.mu.Lock()
		got := len(server.metricChunks)
		server.mu.Unlock()
		if nodeID == metricNodeID {
			if got != 1 {
				t.Fatalf("metric shard %s got %d requests, want 1", nodeID, got)
			}
			continue
		}
		if got != 0 {
			t.Fatalf("non-owner node %s got %d metric requests, want 0", nodeID, got)
		}
	}

	traceChunk := "trace-chunk-9"
	traceNodeID, err := router.telemetryNodeID("traces", traceChunk)
	if err != nil {
		t.Fatalf("telemetryNodeID(traces) error = %v", err)
	}
	if _, err := router.IngestTraces(ctx, &pb.IngestTracesRequest{ChunkId: traceChunk}); err != nil {
		t.Fatalf("IngestTraces() error = %v", err)
	}
	for nodeID, server := range servers {
		server.mu.Lock()
		got := len(server.traceChunks)
		server.mu.Unlock()
		if nodeID == traceNodeID {
			if got != 1 {
				t.Fatalf("trace shard %s got %d requests, want 1", nodeID, got)
			}
			continue
		}
		if got != 0 {
			t.Fatalf("non-owner node %s got %d trace requests, want 0", nodeID, got)
		}
	}
}

func TestQueryLogsUsesSingleNodeForSharedBackend(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	router.config.SpanEngine = "clickhouse"

	servers["node-a"].logResults = []byte(`[{"body":"a"}]`)
	servers["node-b"].logResults = []byte(`[{"body":"a"}]`)

	resp, err := router.QueryLogs(context.Background(), &pb.QueryLogsRequest{Query: `{service="x"}`, Limit: 10})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal logs: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("log count = %d, want one result from one shared backend", len(records))
	}
}

func TestQueryMetricsUsesSingleNodeForSharedBackend(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	router.config.SpanEngine = "clickhouse"

	servers["node-a"].metricResults = []byte(`[{"metric_name":"requests","value":1}]`)
	servers["node-b"].metricResults = []byte(`[{"metric_name":"requests","value":1}]`)

	resp, err := router.QueryMetrics(context.Background(), &pb.QueryMetricsRequest{MetricName: "requests"})
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal metrics: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("metric count = %d, want one result from one shared backend", len(records))
	}
}

func TestQueryTracesUsesSingleNodeForSharedBackend(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	router.config.SpanEngine = "clickhouse"

	servers["node-a"].traceResults = []byte(`[{"trace_id":"t1","span_id":"s1","service":"api"}]`)
	servers["node-b"].traceResults = []byte(`[{"trace_id":"t1","span_id":"s1","service":"api"}]`)

	resp, err := router.QueryTraces(context.Background(), &pb.QueryTracesRequest{TraceId: "t1", Limit: 10})
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal traces: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("trace count = %d, want one result from one shared backend", len(records))
	}
}

func TestStreamQueryTracesUsesSingleNodeForSharedBackend(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	router.config.SpanEngine = "clickhouse"

	servers["node-a"].traceResults = []byte(`[{"trace_id":"t1","span_id":"s1"}]`)
	servers["node-b"].traceResults = []byte(`[{"trace_id":"t1","span_id":"s1"}]`)

	collector := &queryResultItemCollector{ctx: context.Background()}
	if err := router.StreamQueryTraces(&pb.QueryTracesRequest{TraceId: "t1", Limit: 10}, collector); err != nil {
		t.Fatalf("StreamQueryTraces() error = %v", err)
	}
	if len(collector.items) != 1 {
		t.Fatalf("streamed item count = %d, want 1", len(collector.items))
	}
}

func TestQueryDuckDBSQLUsesSingleNodeForSharedBackend(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	router.config.SpanEngine = "clickhouse"

	sqlResult := []byte(`{"dataset":"logs","columns":["service"],"rows":[["api"]],"row_count":1}`)
	servers["node-a"].sqlResults = sqlResult
	servers["node-b"].sqlResults = sqlResult

	resp, err := router.QueryDuckDBSQL(context.Background(), &pb.QueryDuckDBSQLRequest{
		Dataset: "logs",
		Query:   "SELECT service FROM logs",
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("QueryDuckDBSQL() error = %v", err)
	}
	var envelope struct {
		Nodes []struct {
			NodeID string          `json:"node_id"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(resp.GetResultsJson()), &envelope); err != nil {
		t.Fatalf("unmarshal sql results: %v", err)
	}
	if len(envelope.Nodes) != 1 {
		t.Fatalf("node results = %d, want 1 shared-backend result", len(envelope.Nodes))
	}
	servers["node-a"].mu.Lock()
	callsA := servers["node-a"].sqlCalls
	servers["node-a"].mu.Unlock()
	servers["node-b"].mu.Lock()
	callsB := servers["node-b"].sqlCalls
	servers["node-b"].mu.Unlock()
	if callsA+callsB != 1 {
		t.Fatalf("SQL backend calls = %d, want 1", callsA+callsB)
	}
}

func TestQueryLogsMergesDistributedResults(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b", "node-c")
	defer cleanup()

	servers["node-a"].logResults = []byte(`[{"body":"a"}]`)
	servers["node-b"].logResults = []byte(`[{"body":"b"}]`)
	servers["node-c"].logResults = []byte(`[]`)

	resp, err := router.QueryLogs(context.Background(), &pb.QueryLogsRequest{Query: `{service="x"}`, Limit: 10})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal merged logs: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("merged log count = %d, want 2", len(records))
	}
}

func TestQueryLogsHonorsMergedLimitWithStreamedNodeResults(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()

	servers["node-a"].logResults = []byte(`[{"body":"a"},{"body":"b"}]`)
	servers["node-b"].logResults = []byte(`[{"body":"c"}]`)

	resp, err := router.QueryLogs(context.Background(), &pb.QueryLogsRequest{Query: `{service="x"}`, Limit: 2})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal merged logs: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("merged log count = %d, want 2", len(records))
	}
}

func TestStreamQueryLogsForwardsMergedItems(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()

	servers["node-a"].logResults = []byte(`[{"body":"a"}]`)
	servers["node-b"].logResults = []byte(`[{"body":"b"}]`)

	collector := &queryResultItemCollector{ctx: context.Background()}
	if err := router.StreamQueryLogs(&pb.QueryLogsRequest{Query: `{service="x"}`, Limit: 2}, collector); err != nil {
		t.Fatalf("StreamQueryLogs() error = %v", err)
	}
	if len(collector.items) != 2 {
		t.Fatalf("streamed item count = %d, want 2", len(collector.items))
	}
}

func TestQueryMetricsMergesDistributedResults(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()

	servers["node-a"].metricResults = []byte(`[{"service":"doctor","metric_name":"requests","value":1}]`)
	servers["node-b"].metricResults = []byte(`[{"service":"monofs","metric_name":"requests","value":2}]`)

	resp, err := router.QueryMetrics(context.Background(), &pb.QueryMetricsRequest{MetricName: "requests"})
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal merged metrics: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("merged metric count = %d, want 2", len(records))
	}
}

func TestQueryMetricsSkipsHealthyNodeWithoutClient(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a")
	defer cleanup()

	router.nodes["node-missing-client"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:  "node-missing-client",
			Address: "missing:9000",
			Healthy: true,
			Weight:  1,
		},
		status: NodeActive,
	}
	servers["node-a"].metricResults = []byte(`[{"service":"doctor","metric_name":"requests","value":1}]`)

	resp, err := router.QueryMetrics(context.Background(), &pb.QueryMetricsRequest{MetricName: "requests"})
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal merged metrics: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("merged metric count = %d, want 1", len(records))
	}
}

func TestStreamQueryMetricsForwardsMergedItems(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()

	servers["node-a"].metricResults = []byte(`[{"service":"doctor","metric_name":"requests","value":1}]`)
	servers["node-b"].metricResults = []byte(`[{"service":"monofs","metric_name":"requests","value":2}]`)

	collector := &queryResultItemCollector{ctx: context.Background()}
	if err := router.StreamQueryMetrics(&pb.QueryMetricsRequest{MetricName: "requests"}, collector); err != nil {
		t.Fatalf("StreamQueryMetrics() error = %v", err)
	}
	if len(collector.items) != 2 {
		t.Fatalf("streamed metric count = %d, want 2", len(collector.items))
	}
	var first map[string]any
	if err := json.Unmarshal(collector.items[0], &first); err != nil {
		t.Fatalf("unmarshal first streamed metric: %v", err)
	}
	if first["metric_name"] != "requests" {
		t.Fatalf("first streamed metric name = %v, want requests", first["metric_name"])
	}
	var second map[string]any
	if err := json.Unmarshal(collector.items[1], &second); err != nil {
		t.Fatalf("unmarshal second streamed metric: %v", err)
	}
	if second["metric_name"] != "requests" {
		t.Fatalf("second streamed metric name = %v, want requests", second["metric_name"])
	}
}

func TestQueryTracesMergesDistributedResults(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()

	servers["node-a"].traceResults = []byte(`[{"trace_id":"trace-1","span_id":"span-a"}]`)
	servers["node-b"].traceResults = []byte(`[{"trace_id":"trace-1","span_id":"span-b"}]`)

	resp, err := router.QueryTraces(context.Background(), &pb.QueryTracesRequest{TraceId: "trace-1", Limit: 10})
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal merged traces: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("merged trace count = %d, want 2", len(records))
	}
}

func TestStreamQueryTracesHonorsMergedLimit(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()

	servers["node-a"].traceResults = []byte(`[{"trace_id":"trace-1","span_id":"span-a"},{"trace_id":"trace-1","span_id":"span-b"}]`)
	servers["node-b"].traceResults = []byte(`[{"trace_id":"trace-1","span_id":"span-c"}]`)

	collector := &queryResultItemCollector{ctx: context.Background()}
	if err := router.StreamQueryTraces(&pb.QueryTracesRequest{TraceId: "trace-1", Limit: 2}, collector); err != nil {
		t.Fatalf("StreamQueryTraces() error = %v", err)
	}
	if len(collector.items) != 2 {
		t.Fatalf("streamed trace count = %d, want 2", len(collector.items))
	}
}

func TestAggregateTelemetryMergesShardPartials(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	for _, server := range servers {
		server.mu.Lock()
		server.aggregateRows = []byte(`[{"group":{"service":"guardian"},"values":{"count":2,"sum_value":7,"min_ts":100,"max_ts":200}}]`)
		server.mu.Unlock()
	}
	resp, err := router.AggregateTelemetry(context.Background(), &pb.AggregateTelemetryRequest{
		Signal:  "metrics",
		GroupBy: []string{"service"},
		Aggregates: []*pb.AggregateSpec{
			{Op: pb.AggregateOp_AGGREGATE_OP_COUNT, Alias: "count"},
			{Op: pb.AggregateOp_AGGREGATE_OP_SUM, Field: "value", Alias: "sum_value"},
			{Op: pb.AggregateOp_AGGREGATE_OP_MIN, Field: "timestamp", Alias: "min_ts"},
			{Op: pb.AggregateOp_AGGREGATE_OP_MAX, Field: "timestamp", Alias: "max_ts"},
		},
	})
	if err != nil {
		t.Fatalf("AggregateTelemetry() error = %v", err)
	}
	var rows []aggregateWireRow
	decoder := json.NewDecoder(bytes.NewReader(resp.GetRowsJson()))
	decoder.UseNumber()
	if err := decoder.Decode(&rows); err != nil {
		t.Fatalf("decode aggregate rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one merged group", rows)
	}
	if got := rows[0].Values["count"].String(); got != "4" {
		t.Fatalf("count = %s, want 4", got)
	}
	if got := rows[0].Values["sum_value"].String(); got != "14" {
		t.Fatalf("sum = %s, want 14", got)
	}
	if got := rows[0].Values["max_ts"].String(); got != "200" {
		t.Fatalf("max = %s, want 200", got)
	}
}
