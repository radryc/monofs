package router

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

func configureFastFanoutTimeouts(router *Router) {
	router.config.QueryNodeStartTimeout = 2 * time.Second
	router.config.QueryNodeStragglerGrace = 50 * time.Millisecond
	router.config.QueryNodeIdleTimeout = 100 * time.Millisecond
}

func blockNodeStream(server *telemetryTestNodeServer) (blocked <-chan struct{}, release chan<- struct{}) {
	blockedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	server.streamBlocked = blockedCh
	server.streamRelease = releaseCh
	return blockedCh, releaseCh
}

func TestQueryLogsSkipsStragglerNode(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	configureFastFanoutTimeouts(router)

	servers["node-a"].logResults = []byte(`[{"body":"a"}]`)
	servers["node-b"].logResults = []byte(`[{"body":"b"}]`)
	blocked, release := blockNodeStream(servers["node-b"])
	defer close(release)

	start := time.Now()
	resp, err := router.QueryLogs(context.Background(), &pb.QueryLogsRequest{Query: `{service="x"}`, Limit: 10})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if elapsed > 750*time.Millisecond {
		t.Fatalf("QueryLogs() took %s, want it to skip the straggler quickly", elapsed)
	}

	var records []map[string]any
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		t.Fatalf("unmarshal merged logs: %v", err)
	}
	if len(records) != 1 || records[0]["body"] != "a" {
		t.Fatalf("merged logs = %#v, want the healthy node's record only", records)
	}

	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("slow node stream was never reached")
	}
}

func TestStreamQueryMetricsSkipsStragglerNode(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	configureFastFanoutTimeouts(router)

	servers["node-a"].metricResults = []byte(`[{"service":"a","metric_name":"m","value":1}]`)
	servers["node-b"].metricResults = []byte(`[{"service":"b","metric_name":"m","value":2}]`)
	_, release := blockNodeStream(servers["node-b"])
	defer close(release)

	collector := &queryResultItemCollector{ctx: context.Background()}
	start := time.Now()
	if err := router.StreamQueryMetrics(&pb.QueryMetricsRequest{}, collector); err != nil {
		t.Fatalf("StreamQueryMetrics() error = %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 750*time.Millisecond {
		t.Fatalf("StreamQueryMetrics() took %s, want it to skip the straggler quickly", elapsed)
	}
	if len(collector.items) != 1 {
		t.Fatalf("streamed metric item count = %d, want 1", len(collector.items))
	}
}

func TestConcurrentQueriesDoNotBlockOnStraggler(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	configureFastFanoutTimeouts(router)

	servers["node-a"].logResults = []byte(`[{"body":"a"}]`)
	servers["node-b"].logResults = []byte(`[{"body":"b"}]`)
	_, release := blockNodeStream(servers["node-b"])
	defer close(release)

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := router.QueryLogs(context.Background(), &pb.QueryLogsRequest{Query: `{service="x"}`, Limit: 10})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("QueryLogs() error = %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("concurrent queries took %s, want them to proceed independently of the straggler", elapsed)
	}
}

func TestQueryLogsFailsWhenNoNodeRespondsWithinStartTimeout(t *testing.T) {
	router, servers, cleanup := newTelemetryRouterHarness(t, "node-a", "node-b")
	defer cleanup()
	router.config.QueryNodeStartTimeout = 200 * time.Millisecond
	router.config.QueryNodeStragglerGrace = 50 * time.Millisecond
	router.config.QueryNodeIdleTimeout = 100 * time.Millisecond

	_, releaseA := blockNodeStream(servers["node-a"])
	_, releaseB := blockNodeStream(servers["node-b"])
	defer close(releaseA)
	defer close(releaseB)

	start := time.Now()
	_, err := router.QueryLogs(context.Background(), &pb.QueryLogsRequest{Query: `{service="x"}`, Limit: 10})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("QueryLogs() error = nil, want start timeout")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("QueryLogs() took %s, want the start timeout to bound it", elapsed)
	}
}
