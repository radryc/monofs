package test

import (
	"context"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestDualAddressing verifies that the router returns external addresses when requested.
// This is an integration test that requires a running router on localhost:9090.
// It is skipped in short mode and when no router is available.
func TestDualAddressing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode - requires running cluster")
	}

	// Connect to router
	conn, err := grpc.Dial(
		"localhost:9090",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("Failed to connect to router: %v", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test 1: Request internal addresses (like Docker containers would)
	t.Log("Testing internal addresses (use_external_addresses=false)...")
	respInternal, err := client.GetClusterInfo(ctx, &pb.ClusterInfoRequest{
		ClientId:             "test-internal",
		UseExternalAddresses: false,
	})
	if err != nil {
		// Router not running - skip test
		t.Skipf("Router not available on localhost:9090 (requires running cluster): %v", err)
	}

	if len(respInternal.Nodes) == 0 {
		t.Fatal("No nodes returned for internal request")
	}

	// Verify internal addresses don't have localhost
	for _, node := range respInternal.Nodes {
		if len(node.Address) == 0 {
			t.Errorf("Node %s has empty address", node.NodeId)
		}
		// Internal addresses should be like "node1:9000" not "localhost:9001"
		if node.Address[:9] == "localhost" {
			t.Errorf("Node %s has external address when internal was requested: %s",
				node.NodeId, node.Address)
		}
		t.Logf("Node %s internal address: %s", node.NodeId, node.Address)
	}

	// Test 2: Request external addresses (like host-based clients would)
	t.Log("Testing external addresses (use_external_addresses=true)...")
	respExternal, err := client.GetClusterInfo(ctx, &pb.ClusterInfoRequest{
		ClientId:             "test-external",
		UseExternalAddresses: true,
	})
	if err != nil {
		t.Fatalf("Failed to get cluster info (external): %v", err)
	}

	if len(respExternal.Nodes) == 0 {
		t.Fatal("No nodes returned for external request")
	}

	// Verify external addresses have localhost
	externalCount := 0
	for _, node := range respExternal.Nodes {
		if len(node.Address) == 0 {
			t.Errorf("Node %s has empty address", node.NodeId)
		}
		// External addresses should be like "localhost:9001" not "node1:9000"
		if len(node.Address) >= 9 && node.Address[:9] == "localhost" {
			externalCount++
			t.Logf("Node %s external address: %s ✓", node.NodeId, node.Address)
		} else {
			t.Logf("Node %s address: %s (no external configured?)", node.NodeId, node.Address)
		}
	}

	if externalCount == 0 {
		t.Error("No nodes returned external addresses - dual addressing may not be configured")
	} else {
		t.Logf("SUCCESS: %d/%d nodes have external addresses configured",
			externalCount, len(respExternal.Nodes))
	}
}
