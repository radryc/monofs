package router

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sharding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type fakeLedgerNodeServer struct {
	pb.UnimplementedMonoFSServer

	mu               sync.Mutex
	queryCalls       int
	appendCalls      int
	appendShouldFail bool
	queryResp        *pb.QueryLedgerResponse
}

func (f *fakeLedgerNodeServer) QueryLedger(_ context.Context, req *pb.QueryLedgerRequest) (*pb.QueryLedgerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryCalls++
	if f.queryResp == nil {
		return &pb.QueryLedgerResponse{}, nil
	}
	return f.queryResp, nil
}

func (f *fakeLedgerNodeServer) AppendLedgerEntries(_ context.Context, req *pb.AppendLedgerEntriesRequest) (*pb.AppendLedgerEntriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCalls++
	if f.appendShouldFail {
		return nil, fmt.Errorf("append failed")
	}
	accepted := len(req.GetCommits()) + len(req.GetPushOutcomes()) + len(req.GetRefreshEvents())
	return &pb.AppendLedgerEntriesResponse{Success: true, Accepted: int32(accepted)}, nil
}

func startFakeLedgerNode(t *testing.T, resp *pb.QueryLedgerResponse) (pb.MonoFSClient, *fakeLedgerNodeServer, func()) {
	t.Helper()

	impl := &fakeLedgerNodeServer{queryResp: resp}
	grpcServer := grpc.NewServer()
	pb.RegisterMonoFSServer(grpcServer, impl)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake ledger node: %v", err)
	}
	go func() {
		_ = grpcServer.Serve(lis)
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcServer.Stop()
		_ = lis.Close()
		t.Fatalf("dial fake ledger node: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		grpcServer.Stop()
		_ = lis.Close()
	}
	return pb.NewMonoFSClient(conn), impl, cleanup
}

func TestQueryLedgerRoutesToHRWNodeByWorkspaceAndClient(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() { _ = r.Close() }()

	clientA, srvA, cleanupA := startFakeLedgerNode(t, &pb.QueryLedgerResponse{PushOutcomes: []*pb.PushOutcome{{PushOutcomeId: "a"}}, TotalMatches: 1})
	defer cleanupA()
	clientB, srvB, cleanupB := startFakeLedgerNode(t, &pb.QueryLedgerResponse{PushOutcomes: []*pb.PushOutcome{{PushOutcomeId: "b"}}, TotalMatches: 1})
	defer cleanupB()

	r.nodes["node-a"] = &nodeState{info: &pb.NodeInfo{NodeId: "node-a", Address: "a", Weight: 100, Healthy: true}, status: NodeActive, client: clientA}
	r.nodes["node-b"] = &nodeState{info: &pb.NodeInfo{NodeId: "node-b", Address: "b", Weight: 100, Healthy: true}, status: NodeActive, client: clientB}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-client-id", "client-1"))
	workspaceID := "workspace-1"

	resp, err := r.QueryLedger(ctx, &pb.QueryLedgerRequest{WorkspaceId: workspaceID})
	if err != nil {
		t.Fatalf("query ledger: %v", err)
	}
	if resp.GetTotalMatches() != 1 {
		t.Fatalf("total matches = %d, want 1", resp.GetTotalMatches())
	}

	nodes := []sharding.Node{
		{ID: "node-a", Address: "a", Weight: 100, Healthy: true},
		{ID: "node-b", Address: "b", Weight: 100, Healthy: true},
	}
	expected := sharding.NewHRW(nodes).GetNode(workspaceID + "|client-1")
	if expected == nil {
		t.Fatal("expected HRW node, got nil")
	}

	srvA.mu.Lock()
	queryA := srvA.queryCalls
	srvA.mu.Unlock()
	srvB.mu.Lock()
	queryB := srvB.queryCalls
	srvB.mu.Unlock()

	if expected.ID == "node-a" {
		if queryA != 1 || queryB != 0 {
			t.Fatalf("expected node-a to handle query (a=%d b=%d)", queryA, queryB)
		}
	} else {
		if queryA != 0 || queryB != 1 {
			t.Fatalf("expected node-b to handle query (a=%d b=%d)", queryA, queryB)
		}
	}
}

func TestQueryLedgerWithoutWorkspaceFansOut(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() { _ = r.Close() }()

	clientA, _, cleanupA := startFakeLedgerNode(t, &pb.QueryLedgerResponse{PushOutcomes: []*pb.PushOutcome{{PushOutcomeId: "a"}}, TotalMatches: 1})
	defer cleanupA()
	clientB, _, cleanupB := startFakeLedgerNode(t, &pb.QueryLedgerResponse{PushOutcomes: []*pb.PushOutcome{{PushOutcomeId: "b"}}, TotalMatches: 2})
	defer cleanupB()

	r.nodes["node-a"] = &nodeState{info: &pb.NodeInfo{NodeId: "node-a", Address: "a", Weight: 100, Healthy: true}, status: NodeActive, client: clientA}
	r.nodes["node-b"] = &nodeState{info: &pb.NodeInfo{NodeId: "node-b", Address: "b", Weight: 100, Healthy: true}, status: NodeActive, client: clientB}

	resp, err := r.QueryLedger(context.Background(), &pb.QueryLedgerRequest{})
	if err != nil {
		t.Fatalf("query ledger fanout: %v", err)
	}
	if got := len(resp.GetPushOutcomes()); got != 2 {
		t.Fatalf("push outcomes len = %d, want 2", got)
	}
	if got := resp.GetTotalMatches(); got != 3 {
		t.Fatalf("total matches = %d, want 3", got)
	}
}

func TestAppendPushOutcomeRoutesToHRWNode(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() { _ = r.Close() }()

	clientA, srvA, cleanupA := startFakeLedgerNode(t, nil)
	defer cleanupA()
	clientB, srvB, cleanupB := startFakeLedgerNode(t, nil)
	defer cleanupB()

	r.nodes["node-a"] = &nodeState{info: &pb.NodeInfo{NodeId: "node-a", Address: "a", Weight: 100, Healthy: true}, status: NodeActive, client: clientA}
	r.nodes["node-b"] = &nodeState{info: &pb.NodeInfo{NodeId: "node-b", Address: "b", Weight: 100, Healthy: true}, status: NodeActive, client: clientB}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-client-id", "client-2"))
	workspaceID := "workspace-2"
	err := r.appendPushOutcome(ctx, &pb.PushOutcome{WorkspaceId: workspaceID, PushOutcomeId: "outcome-1"})
	if err != nil {
		t.Fatalf("append push outcome: %v", err)
	}

	nodes := []sharding.Node{
		{ID: "node-a", Address: "a", Weight: 100, Healthy: true},
		{ID: "node-b", Address: "b", Weight: 100, Healthy: true},
	}
	expected := sharding.NewHRW(nodes).GetNode(workspaceID + "|client-2")
	if expected == nil {
		t.Fatal("expected HRW node, got nil")
	}

	srvA.mu.Lock()
	appendA := srvA.appendCalls
	srvA.mu.Unlock()
	srvB.mu.Lock()
	appendB := srvB.appendCalls
	srvB.mu.Unlock()

	if expected.ID == "node-a" {
		if appendA != 1 || appendB != 0 {
			t.Fatalf("expected node-a to handle append (a=%d b=%d)", appendA, appendB)
		}
	} else {
		if appendA != 0 || appendB != 1 {
			t.Fatalf("expected node-b to handle append (a=%d b=%d)", appendA, appendB)
		}
	}
}

func TestAppendPushOutcomeFallsBackToReplicaOnOwnerFailure(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() { _ = r.Close() }()

	clientA, srvA, cleanupA := startFakeLedgerNode(t, nil)
	defer cleanupA()
	clientB, srvB, cleanupB := startFakeLedgerNode(t, nil)
	defer cleanupB()

	r.nodes["node-a"] = &nodeState{info: &pb.NodeInfo{NodeId: "node-a", Address: "a", Weight: 100, Healthy: true}, status: NodeActive, client: clientA}
	r.nodes["node-b"] = &nodeState{info: &pb.NodeInfo{NodeId: "node-b", Address: "b", Weight: 100, Healthy: true}, status: NodeActive, client: clientB}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-client-id", "client-fallback"))
	workspaceID := "workspace-fallback"

	nodes := []sharding.Node{{ID: "node-a", Address: "a", Weight: 100, Healthy: true}, {ID: "node-b", Address: "b", Weight: 100, Healthy: true}}
	owner := sharding.NewHRW(nodes).GetNode(workspaceID + "|client-fallback")
	if owner == nil {
		t.Fatal("expected owner node")
	}

	if owner.ID == "node-a" {
		srvA.mu.Lock()
		srvA.appendShouldFail = true
		srvA.mu.Unlock()
	} else {
		srvB.mu.Lock()
		srvB.appendShouldFail = true
		srvB.mu.Unlock()
	}

	err := r.appendPushOutcome(ctx, &pb.PushOutcome{WorkspaceId: workspaceID, PushOutcomeId: "outcome-fallback"})
	if err != nil {
		t.Fatalf("append push outcome with fallback: %v", err)
	}
}
