package router

import (
	"context"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

func TestDefaultRouterConfig(t *testing.T) {
	cfg := DefaultRouterConfig()

	if cfg.ClusterID != "monofs-cluster" {
		t.Errorf("unexpected ClusterID: %s", cfg.ClusterID)
	}
	if cfg.HealthCheckInterval != 5*time.Second {
		t.Errorf("unexpected HealthCheckInterval: %v", cfg.HealthCheckInterval)
	}
	if cfg.UnhealthyThreshold != 15*time.Second {
		t.Errorf("unexpected UnhealthyThreshold: %v", cfg.UnhealthyThreshold)
	}
}

func TestNewRouter(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	if router == nil {
		t.Fatal("NewRouter returned nil")
	}

	if router.NodeCount() != 0 {
		t.Errorf("new router should have 0 nodes, got %d", router.NodeCount())
	}

	if router.version.Load() != 1 {
		t.Errorf("initial version should be 1, got %d", router.version.Load())
	}
}

func TestRegisterNodeStatic(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)

	if router.NodeCount() != 1 {
		t.Errorf("expected 1 node, got %d", router.NodeCount())
	}

	if router.HealthyNodeCount() != 1 {
		t.Errorf("expected 1 healthy node, got %d", router.HealthyNodeCount())
	}
}

func TestRegisterMultipleNodes(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)
	router.RegisterNodeStatic("node2", "localhost:9002", 200)
	router.RegisterNodeStatic("node3", "localhost:9003", 100)

	if router.NodeCount() != 3 {
		t.Errorf("expected 3 nodes, got %d", router.NodeCount())
	}

	if router.HealthyNodeCount() != 3 {
		t.Errorf("expected 3 healthy nodes, got %d", router.HealthyNodeCount())
	}
}

func TestUnregisterNode(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)
	router.RegisterNodeStatic("node2", "localhost:9002", 100)

	if router.NodeCount() != 2 {
		t.Errorf("expected 2 nodes, got %d", router.NodeCount())
	}

	router.UnregisterNode("node1")

	if router.NodeCount() != 1 {
		t.Errorf("expected 1 node after unregister, got %d", router.NodeCount())
	}
}

func TestUnregisterNonexistentNode(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)

	// Should not panic or error
	router.UnregisterNode("nonexistent")

	if router.NodeCount() != 1 {
		t.Errorf("expected 1 node, got %d", router.NodeCount())
	}
}

func TestGetClusterInfo(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.ClusterID = "test-cluster"
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)
	router.RegisterNodeStatic("node2", "localhost:9002", 200)

	ctx := context.Background()
	req := &pb.ClusterInfoRequest{ClientId: "test-client"}

	resp, err := router.GetClusterInfo(ctx, req)
	if err != nil {
		t.Fatalf("GetClusterInfo failed: %v", err)
	}

	if resp.ClusterId != "test-cluster" {
		t.Errorf("unexpected ClusterId: %s", resp.ClusterId)
	}

	if len(resp.Nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(resp.Nodes))
	}

	if resp.Version < 1 {
		t.Errorf("unexpected version: %d", resp.Version)
	}

	// Verify node details
	foundNode1 := false
	foundNode2 := false
	for _, node := range resp.Nodes {
		if node.NodeId == "node1" {
			foundNode1 = true
			if node.Weight != 100 {
				t.Errorf("node1 weight: expected 100, got %d", node.Weight)
			}
		}
		if node.NodeId == "node2" {
			foundNode2 = true
			if node.Weight != 200 {
				t.Errorf("node2 weight: expected 200, got %d", node.Weight)
			}
		}
	}

	if !foundNode1 || !foundNode2 {
		t.Error("not all nodes found in response")
	}
}

func TestHeartbeat(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)

	ctx := context.Background()
	req := &pb.HeartbeatRequest{
		NodeId:    "node1",
		Timestamp: time.Now().Unix(),
	}

	resp, err := router.Heartbeat(ctx, req)
	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}

	if !resp.Acknowledged {
		t.Error("heartbeat not acknowledged")
	}

	if resp.ClusterVersion < 1 {
		t.Errorf("unexpected cluster version: %d", resp.ClusterVersion)
	}
}

func TestHeartbeatNonexistentNode(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	ctx := context.Background()
	req := &pb.HeartbeatRequest{
		NodeId:    "nonexistent",
		Timestamp: time.Now().Unix(),
	}

	// Should not error, just not update anything
	resp, err := router.Heartbeat(ctx, req)
	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}

	if !resp.Acknowledged {
		t.Error("heartbeat not acknowledged")
	}
}

func TestVersionIncrement(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	initialVersion := router.version.Load()

	// Register node should increment version
	router.RegisterNodeStatic("node1", "localhost:9001", 100)
	version1 := router.version.Load()
	if version1 <= initialVersion {
		t.Error("version did not increment after RegisterNodeStatic")
	}

	// Unregister should increment version
	router.UnregisterNode("node1")
	version2 := router.version.Load()
	if version2 <= version1 {
		t.Error("version did not increment after UnregisterNode")
	}
}

func TestHealthyNodeCount(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.UnhealthyThreshold = 100 * time.Millisecond
	router := NewRouter(cfg, nil)

	// Use fake addresses that won't connect (to avoid background health recovery)
	router.RegisterNodeStatic("node1", "fake-node-1.invalid:9001", 100)
	router.RegisterNodeStatic("node2", "fake-node-2.invalid:9002", 100)

	// Give a moment for initial registration
	time.Sleep(10 * time.Millisecond)

	if router.HealthyNodeCount() != 2 {
		t.Errorf("expected 2 healthy nodes, got %d", router.HealthyNodeCount())
	}

	// Simulate node going unhealthy by making it old
	router.mu.Lock()
	if state, ok := router.nodes["node1"]; ok {
		// Set lastSeen far enough in past to exceed unhealthy threshold
		state.lastSeen = time.Now().Add(-cfg.UnhealthyThreshold - 50*time.Millisecond)
	}
	router.mu.Unlock()

	// Run health check
	router.checkAllNodes()

	// Give it a moment for async updates
	time.Sleep(50 * time.Millisecond)

	healthyCount := router.HealthyNodeCount()
	if healthyCount != 1 {
		t.Errorf("expected 1 healthy node after timeout, got %d", healthyCount)
	}
}

func TestHeartbeatRestoresHealth(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.UnhealthyThreshold = 100 * time.Millisecond
	router := NewRouter(cfg, nil)

	// Use fake address that won't connect (to avoid background health recovery)
	router.RegisterNodeStatic("node1", "fake-node-1.invalid:9001", 100)

	// Give a moment for initial registration
	time.Sleep(10 * time.Millisecond)

	// Make node unhealthy
	router.mu.Lock()
	if state, ok := router.nodes["node1"]; ok {
		state.lastSeen = time.Now().Add(-200 * time.Millisecond)
	}
	router.mu.Unlock()

	router.checkAllNodes()

	// Give a moment for async updates
	time.Sleep(10 * time.Millisecond)

	if router.HealthyNodeCount() != 0 {
		t.Errorf("expected 0 healthy nodes, got %d", router.HealthyNodeCount())
	}

	// Send heartbeat
	ctx := context.Background()
	req := &pb.HeartbeatRequest{
		NodeId:    "node1",
		Timestamp: time.Now().Unix(),
	}
	_, err := router.Heartbeat(ctx, req)
	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}

	// Should be healthy again
	if router.HealthyNodeCount() != 1 {
		t.Errorf("expected 1 healthy node after heartbeat, got %d", router.HealthyNodeCount())
	}
}

func TestClose(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)

	err := router.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	// Verify nodes are cleared
	if router.NodeCount() != 0 {
		t.Errorf("expected 0 nodes after close, got %d", router.NodeCount())
	}
}

func TestConcurrentAccess(t *testing.T) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	done := make(chan bool)

	// Concurrent registrations
	for i := 0; i < 10; i++ {
		go func(id int) {
			nodeID := string(rune('A' + id))
			router.RegisterNodeStatic(nodeID, "localhost:9000", 100)
			done <- true
		}(i)
	}

	// Wait for all registrations
	for i := 0; i < 10; i++ {
		<-done
	}

	if router.NodeCount() != 10 {
		t.Errorf("expected 10 nodes, got %d", router.NodeCount())
	}

	// Concurrent reads
	for i := 0; i < 10; i++ {
		go func() {
			ctx := context.Background()
			req := &pb.ClusterInfoRequest{ClientId: "test"}
			_, _ = router.GetClusterInfo(ctx, req)
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestStartStopHealthCheck(t *testing.T) {
	cfg := DefaultRouterConfig()
	cfg.HealthCheckInterval = 50 * time.Millisecond
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)

	// Start health checking
	router.StartHealthCheck()

	// Let it run briefly
	time.Sleep(200 * time.Millisecond)

	// Stop should not hang
	router.StopHealthCheck()
}

func BenchmarkGetClusterInfo(b *testing.B) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	// Register multiple nodes
	for i := 0; i < 10; i++ {
		nodeID := string(rune('A' + i))
		router.RegisterNodeStatic(nodeID, "localhost:9000", 100)
	}

	ctx := context.Background()
	req := &pb.ClusterInfoRequest{ClientId: "bench"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		router.GetClusterInfo(ctx, req)
	}
}

func BenchmarkHeartbeat(b *testing.B) {
	cfg := DefaultRouterConfig()
	router := NewRouter(cfg, nil)

	router.RegisterNodeStatic("node1", "localhost:9001", 100)

	ctx := context.Background()
	req := &pb.HeartbeatRequest{
		NodeId:    "node1",
		Timestamp: time.Now().Unix(),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		router.Heartbeat(ctx, req)
	}
}
