package router

import (
	"context"
	"log/slog"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

func newStatsTestRouter() *Router {
	r := &Router{
		nodes: map[string]*nodeState{
			"healthy": {
				info:            &pb.NodeInfo{NodeId: "healthy", Address: "10.0.0.1:9001"},
				lastSeen:        time.Now(),
				status:          NodeActive,
				ownedFilesCount: 10,
				diskUsedBytes:   100,
				diskTotalBytes:  1000,
				diskFreeBytes:   900,
				backingUpNodes:  []string{"other"},
			},
			"never-seen": {
				info:   &pb.NodeInfo{NodeId: "never-seen", Address: "10.0.0.2:9001"},
				status: NodeStaging,
			},
			"syncing": {
				info:          &pb.NodeInfo{NodeId: "syncing", Address: "10.0.0.3:9001"},
				lastSeen:      time.Now(),
				status:        NodeSyncing,
				syncProgress:  0.5,
				diskUsedBytes: 50,
			},
		},
		ingestedRepos: map[string]*ingestedRepo{
			"repo-a": {filesCount: 10},
			"repo-b": {filesCount: 5},
		},
		logger: slog.New(slog.DiscardHandler),
	}
	r.version.Store(7)
	r.failoverMap.Store("never-seen", "healthy")
	return r
}

func TestGetClusterStats(t *testing.T) {
	r := newStatsTestRouter()

	resp, err := r.GetClusterStats(context.Background(), &pb.ClusterStatsRequest{})
	if err != nil {
		t.Fatalf("GetClusterStats: %v", err)
	}
	if resp.GetTotalNodes() != 3 {
		t.Fatalf("total nodes = %d", resp.GetTotalNodes())
	}
	if resp.GetHealthyNodes() != 2 {
		t.Fatalf("healthy nodes = %d, want 2", resp.GetHealthyNodes())
	}
	if resp.GetUnhealthyNodes() != 1 {
		t.Fatalf("unhealthy nodes = %d, want 1", resp.GetUnhealthyNodes())
	}
	if resp.GetSyncingNodes() != 1 {
		t.Fatalf("syncing nodes = %d, want 1", resp.GetSyncingNodes())
	}
	if resp.GetTotalRepositories() != 2 || resp.GetTotalFiles() != 15 {
		t.Fatalf("repos = %d files = %d", resp.GetTotalRepositories(), resp.GetTotalFiles())
	}
	if resp.GetClusterVersion() != 7 {
		t.Fatalf("cluster version = %d", resp.GetClusterVersion())
	}
	if got := resp.GetFailovers()["never-seen"]; got != "healthy" {
		t.Fatalf("failover = %q", got)
	}

	// Mutating the response must not affect the router's failover map.
	resp.GetFailovers()["x"] = "y"
	if _, ok := r.failoverMap.Load("x"); ok {
		t.Fatal("failover map leaked into the response")
	}
}

func TestGetNodeStats(t *testing.T) {
	r := newStatsTestRouter()

	resp, err := r.GetNodeStats(context.Background(), &pb.NodeStatsRequest{})
	if err != nil {
		t.Fatalf("GetNodeStats: %v", err)
	}
	if len(resp.GetNodes()) != 3 {
		t.Fatalf("nodes = %d, want 3", len(resp.GetNodes()))
	}

	byID := make(map[string]*pb.NodeStatInfo, len(resp.GetNodes()))
	for _, node := range resp.GetNodes() {
		byID[node.GetNodeId()] = node
	}

	healthy := byID["healthy"]
	if healthy == nil || healthy.GetStatus() != "Active" || !healthy.GetHealthy() {
		t.Fatalf("healthy node = %+v", healthy)
	}
	if healthy.GetAddress() != "10.0.0.1:9001" || healthy.GetFileCount() != 10 {
		t.Fatalf("healthy node fields = %+v", healthy)
	}
	if healthy.GetLastHeartbeat() == 0 {
		t.Fatal("healthy node heartbeat not reported")
	}
	if len(healthy.GetBackingUpNodes()) != 1 || healthy.GetBackingUpNodes()[0] != "other" {
		t.Fatalf("backing up nodes = %+v", healthy.GetBackingUpNodes())
	}

	neverSeen := byID["never-seen"]
	if neverSeen == nil || neverSeen.GetHealthy() || neverSeen.GetLastHeartbeat() != 0 {
		t.Fatalf("never-seen node = %+v", neverSeen)
	}
	if neverSeen.GetStatus() != "Staging" {
		t.Fatalf("never-seen status = %q", neverSeen.GetStatus())
	}

	syncing := byID["syncing"]
	if syncing.GetStatus() != "Syncing" || syncing.GetSyncProgress() != 0.5 {
		t.Fatalf("syncing node = %+v", syncing)
	}

	// The returned backing-up slice must be a copy.
	healthy.GetBackingUpNodes()[0] = "mutated"
	r.mu.RLock()
	original := r.nodes["healthy"].backingUpNodes[0]
	r.mu.RUnlock()
	if original != "other" {
		t.Fatalf("node backing-up slice was mutated: %q", original)
	}
}
