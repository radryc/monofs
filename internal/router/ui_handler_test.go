package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestBuildStatusDataIncludesKVSStatus(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), nil)
	r.nodes["node-a"] = &nodeState{
		info:   &pb.NodeInfo{NodeId: "node-a", Address: "10.0.0.1:9000", Healthy: true, Weight: 100},
		status: NodeActive,
		kvsStatus: &pb.KVSNodeStatus{
			Enabled:   true,
			Healthy:   true,
			Mode:      "raft",
			Role:      "leader",
			LeaderId:  "node-a",
			PeerCount: 3,
			KeyCount:  42,
		},
	}
	r.nodes["node-b"] = &nodeState{
		info:   &pb.NodeInfo{NodeId: "node-b", Address: "10.0.0.2:9000", Healthy: true, Weight: 100},
		status: NodeActive,
	}

	data := r.buildStatusData()
	if len(data.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(data.Nodes))
	}

	nodeA := statusNodeByID(t, data.Nodes, "node-a")
	kvsA, ok := nodeA["kvs"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected kvs status map for node-a, got %#v", nodeA["kvs"])
	}
	if got := kvsA["enabled"]; got != true {
		t.Fatalf("expected kvs enabled for node-a, got %#v", got)
	}
	if got := kvsA["mode"]; got != "raft" {
		t.Fatalf("expected raft kvs mode for node-a, got %#v", got)
	}
	if got := kvsA["role"]; got != "leader" {
		t.Fatalf("expected leader kvs role for node-a, got %#v", got)
	}
	if got := kvsA["leader_id"]; got != "node-a" {
		t.Fatalf("expected leader_id node-a, got %#v", got)
	}
	if got := kvsA["peer_count"]; got != int32(3) {
		t.Fatalf("expected kvs peer count 3, got %#v", got)
	}
	if got := kvsA["key_count"]; got != int64(42) {
		t.Fatalf("expected kvs key count 42, got %#v", got)
	}

	nodeB := statusNodeByID(t, data.Nodes, "node-b")
	kvsB, ok := nodeB["kvs"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected kvs status map for node-b, got %#v", nodeB["kvs"])
	}
	if got := kvsB["enabled"]; got != false {
		t.Fatalf("expected kvs disabled for node-b, got %#v", got)
	}
	if got := kvsB["mode"]; got != "disabled" {
		t.Fatalf("expected disabled kvs mode for node-b, got %#v", got)
	}
}

func statusNodeByID(t *testing.T, nodes []map[string]interface{}, nodeID string) map[string]interface{} {
	t.Helper()
	for _, node := range nodes {
		if node["id"] == nodeID {
			return node
		}
	}
	t.Fatalf("node %q not found in status payload", nodeID)
	return nil
}

func TestDedupeGuardianClientsPrefersFreshestEntry(t *testing.T) {
	input := []guardianClientJSON{
		{
			ClientID:      "guardian-control-plane-123",
			BaseURL:       "http://127.0.0.1:8090",
			LastHeartbeat: 100,
			State:         "stale",
			Router:        "router-a",
		},
		{
			ClientID:      "guardian-pusher-k8s-456",
			LastHeartbeat: 150,
			State:         "connected",
			Router:        "router-a",
		},
		{
			ClientID:      "guardian-control-plane-123",
			BaseURL:       "http://127.0.0.1:8090",
			LastHeartbeat: 200,
			State:         "connected",
			Router:        "router-b",
		},
	}

	got := dedupeGuardianClients(input)
	if len(got) != 2 {
		t.Fatalf("dedupeGuardianClients() len = %d, want 2", len(got))
	}

	if got[0].ClientID != "guardian-control-plane-123" {
		t.Fatalf("first client ID = %q, want guardian-control-plane-123", got[0].ClientID)
	}
	if got[0].State != "connected" {
		t.Fatalf("guardian-control-plane state = %q, want connected", got[0].State)
	}
	if got[0].LastHeartbeat != 200 {
		t.Fatalf("guardian-control-plane last heartbeat = %d, want 200", got[0].LastHeartbeat)
	}
	if got[0].Router != "router-b" {
		t.Fatalf("guardian-control-plane router = %q, want router-b", got[0].Router)
	}

	if got[1].ClientID != "guardian-pusher-k8s-456" {
		t.Fatalf("second client ID = %q, want guardian-pusher-k8s-456", got[1].ClientID)
	}
}

func TestNormalizePprofProfiles(t *testing.T) {
	profiles := normalizePprofProfiles([]string{"CPU", "heap", "goroutine", "invalid", "heap"})
	if len(profiles) != 3 {
		t.Fatalf("expected 3 normalized profiles, got %d", len(profiles))
	}
	if profiles[0] != "cpu" || profiles[1] != "heap" || profiles[2] != "goroutine" {
		t.Fatalf("unexpected normalized profiles: %#v", profiles)
	}
}

func TestAddressWithOffset(t *testing.T) {
	addr, err := addressWithOffset("node-a:9000", 100)
	if err != nil {
		t.Fatalf("addressWithOffset returned error: %v", err)
	}
	if addr != "node-a:9100" {
		t.Fatalf("addressWithOffset = %q, want %q", addr, "node-a:9100")
	}
}

func TestRouterBaseURLFromRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "http://localhost:8080/api/pprof/collect", nil)
	req.Header.Set("X-Forwarded-Host", "example.local:8080")
	req.Header.Set("X-Forwarded-Proto", "https")

	baseURL := routerBaseURLFromRequest(req)
	if baseURL != "https://example.local:8080" {
		t.Fatalf("routerBaseURLFromRequest = %q, want %q", baseURL, "https://example.local:8080")
	}
}

func TestCollectPprofTargetsUsesExplicitDiagnosticsAddresses(t *testing.T) {
	config := DefaultRouterConfig()
	config.RouterName = "router-a"
	config.SearchDiagnostics = "search-index:9101"
	config.FetcherDiagnostics = []string{"fetcher-a:9201", "http://fetcher-b:9201"}
	config.ServerDiagnostics = map[string]string{
		"node-a": "node-a:9150",
		"node-b": "http://node-b:9150",
	}

	r := NewRouter(config, nil)
	r.RegisterNodeStatic("node-a", "node-a:9000", 100)
	r.RegisterNodeStatic("node-b", "node-b:9000", 100)

	req := httptest.NewRequest(http.MethodPost, "http://router-a:8080/api/pprof/collect", nil)
	targets := r.collectPprofTargets(req)

	if !hasTarget(targets, "search", "search-index:9101", "http://search-index:9101") {
		t.Fatalf("expected explicit search diagnostics target, got %#v", targets)
	}
	if !hasTarget(targets, "fetcher", "fetcher-a:9201", "http://fetcher-a:9201") {
		t.Fatalf("expected explicit fetcher target fetcher-a:9201, got %#v", targets)
	}
	if !hasTarget(targets, "fetcher", "fetcher-b:9201", "http://fetcher-b:9201") {
		t.Fatalf("expected explicit fetcher target fetcher-b:9201, got %#v", targets)
	}
	if !hasTarget(targets, "server", "node-a:9150", "http://node-a:9150") {
		t.Fatalf("expected explicit server diagnostics target node-a:9150, got %#v", targets)
	}
	if !hasTarget(targets, "server", "node-b:9150", "http://node-b:9150") {
		t.Fatalf("expected explicit server diagnostics target node-b:9150, got %#v", targets)
	}
}

func hasTarget(targets []pprofTarget, serviceType, address, baseURL string) bool {
	for _, target := range targets {
		if target.ServiceType == serviceType && target.Address == address && target.BaseURL == baseURL {
			return true
		}
	}
	return false
}
