package router

import (
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestNormalizedKVSNodeStatusPreservesLeaderDetails(t *testing.T) {
	in := &pb.KVSNodeStatus{
		Enabled:       true,
		Healthy:       true,
		Mode:          "raft",
		Role:          "follower",
		LeaderId:      "node-c",
		LeaderAddress: "node-c:7000",
		Term:          5,
		PeerCount:     5,
		KeyCount:      42,
	}
	out := normalizedKVSNodeStatus(in)
	if out.GetLeaderId() != "node-c" {
		t.Fatalf("leader id = %q, want node-c", out.GetLeaderId())
	}
	if out.GetLeaderAddress() != "node-c:7000" {
		t.Fatalf("leader address = %q, want node-c:7000", out.GetLeaderAddress())
	}
	if out.GetTerm() != 5 {
		t.Fatalf("term = %d, want 5", out.GetTerm())
	}
	if out.GetPeerCount() != 5 || out.GetKeyCount() != 42 {
		t.Fatalf("unexpected peer/key count: %+v", out)
	}
}
