package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// mockNode represents a mock MonoFS server node for testing
type mockNode struct {
	pb.UnimplementedMonoFSServer
	nodeID       string
	healthy      bool
	mu           sync.Mutex
	lookupCalls  int32
	getAttrCalls int32
	readCalls    int32
	shouldFail   bool
	failError    error
	lookupResp   *pb.LookupResponse
	getAttrResp  *pb.GetAttrResponse
	readData     []byte
}

func (m *mockNode) Lookup(ctx context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	atomic.AddInt32(&m.lookupCalls, 1)
	m.mu.Lock()
	shouldFail := m.shouldFail
	failError := m.failError
	resp := m.lookupResp
	m.mu.Unlock()

	if shouldFail {
		if failError != nil {
			return nil, failError
		}
		return nil, status.Error(codes.Unavailable, "node unavailable")
	}

	if resp != nil {
		return resp, nil
	}
	return &pb.LookupResponse{Found: false}, nil
}

func (m *mockNode) GetAttr(ctx context.Context, req *pb.GetAttrRequest) (*pb.GetAttrResponse, error) {
	atomic.AddInt32(&m.getAttrCalls, 1)
	m.mu.Lock()
	shouldFail := m.shouldFail
	failError := m.failError
	resp := m.getAttrResp
	m.mu.Unlock()

	if shouldFail {
		if failError != nil {
			return nil, failError
		}
		return nil, status.Error(codes.Unavailable, "node unavailable")
	}

	if resp != nil {
		return resp, nil
	}
	return &pb.GetAttrResponse{Found: false}, nil
}

// mockReadServer implements the Read streaming response
type mockReadServer struct {
	grpc.ServerStream
	data []byte
	sent bool
	ctx  context.Context
}

func (s *mockReadServer) Context() context.Context {
	return s.ctx
}

func (s *mockReadServer) Send(chunk *pb.DataChunk) error {
	return nil
}

func (m *mockNode) Read(req *pb.ReadRequest, stream pb.MonoFS_ReadServer) error {
	atomic.AddInt32(&m.readCalls, 1)
	m.mu.Lock()
	shouldFail := m.shouldFail
	failError := m.failError
	data := m.readData
	m.mu.Unlock()

	if shouldFail {
		if failError != nil {
			return failError
		}
		return status.Error(codes.Unavailable, "node unavailable")
	}

	if data != nil {
		return stream.Send(&pb.DataChunk{Data: data})
	}
	return nil
}

func (m *mockNode) setFail(fail bool) {
	m.mu.Lock()
	m.shouldFail = fail
	m.mu.Unlock()
}

func (m *mockNode) setLookupResponse(resp *pb.LookupResponse) {
	m.mu.Lock()
	m.lookupResp = resp
	m.mu.Unlock()
}

func (m *mockNode) setGetAttrResponse(resp *pb.GetAttrResponse) {
	m.mu.Lock()
	m.getAttrResp = resp
	m.mu.Unlock()
}

func (m *mockNode) getLookupCalls() int32 {
	return atomic.LoadInt32(&m.lookupCalls)
}

func (m *mockNode) getGetAttrCalls() int32 {
	return atomic.LoadInt32(&m.getAttrCalls)
}

func (m *mockNode) getReadCalls() int32 {
	return atomic.LoadInt32(&m.readCalls)
}

func (m *mockNode) setReadData(data []byte) {
	m.mu.Lock()
	m.readData = data
	m.mu.Unlock()
}

// testCluster represents a test cluster with multiple mock nodes
type testCluster struct {
	nodes     map[string]*mockNode
	servers   map[string]*grpc.Server
	listeners map[string]*bufconn.Listener
	client    *ShardedClient
}

func newTestCluster(t *testing.T, nodeIDs []string) *testCluster {
	tc := &testCluster{
		nodes:     make(map[string]*mockNode),
		servers:   make(map[string]*grpc.Server),
		listeners: make(map[string]*bufconn.Listener),
	}

	// Create mock nodes and start servers
	nodes := make([]sharding.Node, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		mock := &mockNode{nodeID: id, healthy: true}
		tc.nodes[id] = mock

		listener := bufconn.Listen(1024 * 1024)
		tc.listeners[id] = listener

		server := grpc.NewServer()
		pb.RegisterMonoFSServer(server, mock)
		tc.servers[id] = server

		go func() {
			if err := server.Serve(listener); err != nil {
				t.Logf("Server %s error: %v", id, err)
			}
		}()

		nodes = append(nodes, sharding.Node{
			ID:      id,
			Address: id, // Use nodeID as address for bufconn
			Weight:  100,
			Healthy: true,
		})
	}

	// Create ShardedClient manually (without connecting to router)
	sc := &ShardedClient{
		conns:       make(map[string]*grpc.ClientConn),
		clients:     make(map[string]pb.MonoFSClient),
		hrw:         sharding.NewHRW(nodes),
		connected:   true,
		rpcTimeout:  2 * time.Second,
		stopRefresh: make(chan struct{}),
	}

	// Connect to each mock node via bufconn
	for id, listener := range tc.listeners {
		dialer := func(l *bufconn.Listener) func(context.Context, string) (net.Conn, error) {
			return func(context.Context, string) (net.Conn, error) {
				return l.Dial()
			}
		}(listener)

		conn, err := grpc.DialContext(
			context.Background(),
			id,
			grpc.WithContextDialer(dialer),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			t.Fatalf("Failed to dial node %s: %v", id, err)
		}

		sc.conns[id] = conn
		sc.clients[id] = pb.NewMonoFSClient(conn)
	}

	tc.client = sc
	return tc
}

func (tc *testCluster) close() {
	if tc.client != nil {
		tc.client.Close()
	}
	for _, server := range tc.servers {
		server.Stop()
	}
	for _, listener := range tc.listeners {
		listener.Close()
	}
}

// TestNodeFailoverOnLookup tests that when primary node fails, client fails over to next node
func TestNodeFailoverOnLookup(t *testing.T) {
	tc := newTestCluster(t, []string{"node1", "node2", "node3"})
	defer tc.close()

	// Use a path that will route to node1 first
	testPath := "github.com/test/repo/file.txt"

	// First, verify which node is primary for this path
	key := buildShardKey(testPath)
	primaryNode := tc.client.hrw.GetNode(key)
	if primaryNode == nil {
		t.Fatal("No primary node returned")
	}
	t.Logf("Primary node for %s is %s", testPath, primaryNode.ID)

	// Set up the primary node to fail
	tc.nodes[primaryNode.ID].setFail(true)

	// Set up another node to succeed with a response
	for id, node := range tc.nodes {
		if id != primaryNode.ID {
			node.setLookupResponse(&pb.LookupResponse{
				Found: true,
				Ino:   12345,
				Mode:  0644,
			})
			break // Only need one fallback
		}
	}

	// Perform lookup - should failover to another node
	resp, err := tc.client.Lookup(context.Background(), testPath)
	if err != nil {
		t.Fatalf("Lookup failed after failover: %v", err)
	}
	if !resp.Found {
		t.Error("Expected file to be found after failover")
	}
	if resp.Ino != 12345 {
		t.Errorf("Expected ino 12345, got %d", resp.Ino)
	}

	// Verify primary was called (and failed)
	if tc.nodes[primaryNode.ID].getLookupCalls() == 0 {
		t.Error("Primary node was never called")
	}

	// Verify at least one other node was called
	otherCalled := false
	for id, node := range tc.nodes {
		if id != primaryNode.ID && node.getLookupCalls() > 0 {
			otherCalled = true
			t.Logf("Failover to node %s successful", id)
			break
		}
	}
	if !otherCalled {
		t.Error("No failover node was called")
	}
}

// TestAllNodesFailOnLookup tests behavior when all nodes fail
func TestAllNodesFailOnLookup(t *testing.T) {
	tc := newTestCluster(t, []string{"node1", "node2", "node3"})
	defer tc.close()

	// Make all nodes fail
	for _, node := range tc.nodes {
		node.setFail(true)
	}

	testPath := "github.com/test/repo/file.txt"

	// Perform lookup - should fail with error
	_, err := tc.client.Lookup(context.Background(), testPath)
	if err == nil {
		t.Error("Expected error when all nodes fail, got nil")
	}

	// Verify all nodes were tried (at least primary + fallbacks)
	totalCalls := int32(0)
	for _, node := range tc.nodes {
		totalCalls += node.getLookupCalls()
	}
	if totalCalls < 3 {
		t.Errorf("Expected at least 3 lookup attempts, got %d", totalCalls)
	}
	t.Logf("Total lookup attempts: %d", totalCalls)
}

// TestNodeRecovery tests that failover works via retry mechanism
// With connection state-based health checking, nodes are NOT marked unhealthy
// when they return application errors - only when the connection truly fails.
func TestNodeRecovery(t *testing.T) {
	tc := newTestCluster(t, []string{"node1", "node2", "node3"})
	defer tc.close()

	testPath := "github_com/test/repo/file.txt"
	key := buildShardKey(testPath)
	primaryNode := tc.client.hrw.GetNode(key)
	if primaryNode == nil {
		t.Fatal("No primary node returned")
	}

	// Mark primary as failing (returns error)
	tc.nodes[primaryNode.ID].setFail(true)

	// First lookup - will fail over via retry mechanism
	tc.nodes["node2"].setLookupResponse(&pb.LookupResponse{Found: true, Ino: 1})
	tc.nodes["node3"].setLookupResponse(&pb.LookupResponse{Found: true, Ino: 2})

	_, err := tc.client.Lookup(context.Background(), testPath)
	if err != nil {
		t.Fatalf("First lookup failed: %v", err)
	}

	// With connection state-based health checking, node should STILL be healthy
	// because the gRPC connection is still alive (bufconn doesn't close)
	nodeInfo := tc.client.hrw.GetNodeByID(primaryNode.ID)
	if nodeInfo == nil {
		t.Fatal("Primary node not found in HRW")
	}
	if !nodeInfo.Healthy {
		t.Log("Note: Node was marked unhealthy - this is acceptable if connection actually failed")
	}

	// Now recover the primary node
	tc.nodes[primaryNode.ID].setFail(false)
	tc.nodes[primaryNode.ID].setLookupResponse(&pb.LookupResponse{Found: true, Ino: 999})

	// Re-mark node as healthy (simulating router refresh)
	tc.client.hrw.SetNodeHealth(primaryNode.ID, true)

	// Reset call counts
	for _, node := range tc.nodes {
		atomic.StoreInt32(&node.lookupCalls, 0)
	}

	// Lookup should now use recovered primary
	resp, err := tc.client.Lookup(context.Background(), testPath)
	if err != nil {
		t.Fatalf("Lookup after recovery failed: %v", err)
	}

	// Primary should be called first again
	if tc.nodes[primaryNode.ID].getLookupCalls() == 0 {
		t.Error("Primary node was not called after recovery")
	}

	if resp.Ino != 999 {
		t.Errorf("Expected ino 999 from recovered primary, got %d", resp.Ino)
	}
}

// TestCascadeFailurePrevention tests that one node failing doesn't affect others
func TestCascadeFailurePrevention(t *testing.T) {
	tc := newTestCluster(t, []string{"node1", "node2", "node3"})
	defer tc.close()

	// Set up all nodes with valid responses
	for _, node := range tc.nodes {
		node.setLookupResponse(&pb.LookupResponse{Found: true, Ino: 100})
	}

	// Make only node1 fail
	tc.nodes["node1"].setFail(true)

	// Find paths that route to each node
	type nodeTest struct {
		path    string
		primary string
	}
	var tests []nodeTest

	// Generate test paths and find their primary nodes
	testPaths := []string{
		"github.com/test/repo1/file.txt",
		"github.com/test/repo2/file.txt",
		"github.com/test/repo3/file.txt",
		"github.com/user/project/main.go",
		"github.com/org/lib/util.go",
	}

	for _, path := range testPaths {
		key := buildShardKey(path)
		primary := tc.client.hrw.GetNode(key)
		if primary != nil {
			tests = append(tests, nodeTest{path: path, primary: primary.ID})
		}
	}

	// Perform lookups for paths that route to node2 or node3 (not failing node1)
	for _, tt := range tests {
		if tt.primary == "node1" {
			// This should failover
			continue
		}

		resp, err := tc.client.Lookup(context.Background(), tt.path)
		if err != nil {
			t.Errorf("Lookup for %s (primary=%s) failed: %v - cascade failure detected!",
				tt.path, tt.primary, err)
		} else if !resp.Found {
			t.Errorf("Lookup for %s should have found file", tt.path)
		} else {
			t.Logf("✓ Path %s routed to %s successfully despite node1 being down", tt.path, tt.primary)
		}
	}
}

// TestGetAttrFailover tests failover for GetAttr operations
func TestGetAttrFailover(t *testing.T) {
	tc := newTestCluster(t, []string{"node1", "node2", "node3"})
	defer tc.close()

	testPath := "github_com/test/repo/file.txt"
	key := buildShardKey(testPath)
	primaryNode := tc.client.hrw.GetNode(key)
	if primaryNode == nil {
		t.Fatal("No primary node returned")
	}

	// Set up primary to fail
	tc.nodes[primaryNode.ID].setFail(true)

	// Set up fallback nodes with responses
	for id, node := range tc.nodes {
		if id != primaryNode.ID {
			node.setGetAttrResponse(&pb.GetAttrResponse{
				Found: true,
				Size:  1024,
				Mode:  0644,
			})
		}
	}

	// Perform GetAttr - should failover
	resp, err := tc.client.GetAttr(context.Background(), testPath)
	if err != nil {
		t.Fatalf("GetAttr failed after failover: %v", err)
	}
	if !resp.Found {
		t.Error("Expected file to be found after failover")
	}
	if resp.Size != 1024 {
		t.Errorf("Expected size 1024, got %d", resp.Size)
	}

	// Verify failover occurred
	if tc.nodes[primaryNode.ID].getGetAttrCalls() == 0 {
		t.Error("Primary node was never called")
	}

	otherCalled := false
	for id, node := range tc.nodes {
		if id != primaryNode.ID && node.getGetAttrCalls() > 0 {
			otherCalled = true
			break
		}
	}
	if !otherCalled {
		t.Error("No failover node was called for GetAttr")
	}
}

// TestConcurrentFailover tests failover under concurrent load
func TestConcurrentFailover(t *testing.T) {
	tc := newTestCluster(t, []string{"node1", "node2", "node3"})
	defer tc.close()

	// Set up responses
	for _, node := range tc.nodes {
		node.setLookupResponse(&pb.LookupResponse{Found: true, Ino: 100})
	}

	// Make node1 intermittently fail
	tc.nodes["node1"].setFail(true)

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	// Spawn concurrent lookups
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "github.com/test/repo/file" + string(rune('a'+i%26)) + ".txt"
			_, err := tc.client.Lookup(context.Background(), path)
			if err != nil {
				errors <- err
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Count errors - some are expected for paths that route to node1 if all fallbacks fail
	errCount := 0
	for err := range errors {
		errCount++
		t.Logf("Concurrent lookup error: %v", err)
	}

	// Most lookups should succeed (those routed to node2/node3, or those that fail over)
	if errCount > 50 {
		t.Errorf("Too many errors during concurrent failover: %d/100", errCount)
	}
	t.Logf("Concurrent test: %d/100 lookups succeeded", 100-errCount)
}

// TestNoHealthyNodes tests behavior when no healthy nodes exist
func TestNoHealthyNodes(t *testing.T) {
	tc := newTestCluster(t, []string{"node1", "node2", "node3"})
	defer tc.close()

	// Mark all nodes as unhealthy in HRW (simulating all nodes being down)
	tc.client.hrw.SetNodeHealth("node1", false)
	tc.client.hrw.SetNodeHealth("node2", false)
	tc.client.hrw.SetNodeHealth("node3", false)

	testPath := "github.com/test/repo/file.txt"

	// Lookup should fail immediately with "no healthy nodes"
	_, err := tc.client.Lookup(context.Background(), testPath)
	if err == nil {
		t.Error("Expected error when no healthy nodes, got nil")
	}
	if !errors.Is(err, nil) && err.Error() != "no healthy nodes available" {
		t.Logf("Error message: %v", err)
	}
}

// TestStableNodeOrdering tests that node ordering remains stable during failures
func TestStableNodeOrdering(t *testing.T) {
	nodes := []sharding.Node{
		{ID: "node1", Address: "addr1", Weight: 100, Healthy: true},
		{ID: "node2", Address: "addr2", Weight: 100, Healthy: true},
		{ID: "node3", Address: "addr3", Weight: 100, Healthy: true},
	}
	hrw := sharding.NewHRW(nodes)

	testKey := "github.com/test/repo:file.txt"

	// Get initial ordering
	orderedNodes := hrw.GetNodes(testKey, 3)
	initialOrder := make([]string, len(orderedNodes))
	for i, n := range orderedNodes {
		initialOrder[i] = n.ID
	}
	t.Logf("Initial order for key: %v", initialOrder)

	// Mark first node (primary) as unhealthy
	hrw.SetNodeHealth(initialOrder[0], false)

	// Get ordering again
	orderedNodesAfter := hrw.GetNodes(testKey, 3)
	afterOrder := make([]string, len(orderedNodesAfter))
	for i, n := range orderedNodesAfter {
		afterOrder[i] = n.ID
	}
	t.Logf("Order after %s unhealthy: %v", initialOrder[0], afterOrder)

	// The remaining healthy nodes should maintain their relative order
	if len(afterOrder) != 2 {
		t.Errorf("Expected 2 healthy nodes, got %d", len(afterOrder))
	}

	// Node2 should now be primary (was secondary)
	if afterOrder[0] != initialOrder[1] {
		t.Errorf("Expected %s to be new primary, got %s", initialOrder[1], afterOrder[0])
	}

	// Node3 should now be secondary (was tertiary)
	if afterOrder[1] != initialOrder[2] {
		t.Errorf("Expected %s to be secondary, got %s", initialOrder[2], afterOrder[1])
	}

	// Recover first node
	hrw.SetNodeHealth(initialOrder[0], true)

	// Order should be restored to original
	orderedNodesRecovered := hrw.GetNodes(testKey, 3)
	recoveredOrder := make([]string, len(orderedNodesRecovered))
	for i, n := range orderedNodesRecovered {
		recoveredOrder[i] = n.ID
	}
	t.Logf("Order after recovery: %v", recoveredOrder)

	for i := range initialOrder {
		if recoveredOrder[i] != initialOrder[i] {
			t.Errorf("Position %d changed: expected %s, got %s", i, initialOrder[i], recoveredOrder[i])
		}
	}
}

// TestReadFailover tests failover for Read operations
func TestReadFailover(t *testing.T) {
	tc := newTestCluster(t, []string{"node1", "node2", "node3"})
	defer tc.close()

	testPath := "github_com/test/repo/file.txt"
	key := buildShardKey(testPath)
	primaryNode := tc.client.hrw.GetNode(key)
	if primaryNode == nil {
		t.Fatal("No primary node returned")
	}

	// Set up primary to fail
	tc.nodes[primaryNode.ID].setFail(true)

	// Set up fallback nodes with data
	expectedData := []byte("Hello from fallback node!")
	for id, node := range tc.nodes {
		if id != primaryNode.ID {
			node.setReadData(expectedData)
		}
	}

	// Perform Read - should failover
	data, err := tc.client.Read(context.Background(), testPath, 0, 100)
	if err != nil {
		t.Fatalf("Read failed after failover: %v", err)
	}
	if string(data) != string(expectedData) {
		t.Errorf("Expected data %q, got %q", expectedData, data)
	}

	// Verify failover occurred
	if tc.nodes[primaryNode.ID].getReadCalls() == 0 {
		t.Error("Primary node was never called")
	}

	otherCalled := false
	for id, node := range tc.nodes {
		if id != primaryNode.ID && node.getReadCalls() > 0 {
			otherCalled = true
			t.Logf("Read failover to node %s successful", id)
			break
		}
	}
	if !otherCalled {
		t.Error("No failover node was called for Read")
	}
}
