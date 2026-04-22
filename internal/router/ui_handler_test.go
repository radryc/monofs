package router

import (
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
