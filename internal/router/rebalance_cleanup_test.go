package router

import (
	"log/slog"
	"os"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

// TestCleanupOldFileLocationsStreamsPlan verifies cleanup reads the temp move
// plan and deletes exactly the recorded source copies.
func TestCleanupOldFileLocationsStreamsPlan(t *testing.T) {
	old := rebalanceCleanupGracePeriod
	rebalanceCleanupGracePeriod = 0
	defer func() { rebalanceCleanupGracePeriod = old }()

	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	defer r.Close()

	client, node, closeNode := newGuardianTestNodeClient(t)
	defer closeNode()
	node.repos["sid"] = "guardian-system"

	r.mu.Lock()
	r.nodes["src"] = &nodeState{
		info:   &pb.NodeInfo{NodeId: "src", Healthy: true},
		client: client,
		status: NodeActive,
	}
	r.mu.Unlock()

	plan, err := newRebalanceMovePlan()
	if err != nil {
		t.Fatalf("newRebalanceMovePlan: %v", err)
	}
	for _, p := range []string{"a/1.go", "a/2.go", "b/3.go"} {
		if err := plan.record(rebalanceMoveEntry{From: "src", To: "dst", Path: p}); err != nil {
			t.Fatalf("record %q: %v", p, err)
		}
	}
	plan.close()

	r.cleanupOldFileLocations("sid", plan)

	node.mu.Lock()
	calls := node.deleteFileCalls
	node.mu.Unlock()
	if calls != 3 {
		t.Fatalf("DeleteFile calls = %d, want 3", calls)
	}

	if _, err := os.Stat(plan.path); !os.IsNotExist(err) {
		t.Fatalf("move plan file still present after cleanup: %v", err)
	}
}

// TestCleanupOldFileLocationsNilPlan ensures a nil plan (creation failure) is a
// safe no-op and never deletes anything.
func TestCleanupOldFileLocationsNilPlan(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	defer r.Close()

	// Should return immediately without panicking.
	r.cleanupOldFileLocations("sid", nil)
}
