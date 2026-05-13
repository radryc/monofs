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

func (s *telemetryTestNodeServer) QueryMetrics(context.Context, *pb.QueryMetricsRequest) (*pb.QueryMetricsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.QueryMetricsResponse{ResultsJson: append([]byte(nil), s.metricResults...)}, nil
}

func (s *telemetryTestNodeServer) QueryTraces(context.Context, *pb.QueryTracesRequest) (*pb.QueryTracesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.QueryTracesResponse{ResultsJson: append([]byte(nil), s.traceResults...)}, nil
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
