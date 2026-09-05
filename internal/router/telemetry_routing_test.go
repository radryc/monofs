package router

import (
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

func (s *telemetryTestNodeServer) StreamQueryLogs(_ *pb.QueryLogsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	s.mu.Lock()
	results := append([]byte(nil), s.logResults...)
	s.mu.Unlock()
	return streamTelemetryResults(results, stream.Send)
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
	return streamTelemetryResults(results, stream.Send)
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
