package client

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type topologyHookRouter struct {
	pb.UnimplementedMonoFSRouterServer

	mu       sync.Mutex
	version  int64
	nodeAddr string
}

func (r *topologyHookRouter) GetClusterInfo(ctx context.Context, req *pb.ClusterInfoRequest) (*pb.ClusterInfoResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &pb.ClusterInfoResponse{
		Version: r.version,
		Nodes: []*pb.NodeInfo{{
			NodeId:  "node-1",
			Address: r.nodeAddr,
			Healthy: true,
		}},
	}, nil
}

func (r *topologyHookRouter) setVersion(version int64) {
	r.mu.Lock()
	r.version = version
	r.mu.Unlock()
}

type topologyHookNode struct {
	pb.UnimplementedMonoFSServer
}

func TestShardedClientTopologyChangeHookFiresOnInitialAndVersionChanges(t *testing.T) {
	nodeServer := grpc.NewServer()
	pb.RegisterMonoFSServer(nodeServer, &topologyHookNode{})
	nodeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(node) error = %v", err)
	}
	defer nodeListener.Close()
	go func() {
		_ = nodeServer.Serve(nodeListener)
	}()
	defer nodeServer.Stop()

	routerImpl := &topologyHookRouter{version: 1, nodeAddr: nodeListener.Addr().String()}
	routerServer := grpc.NewServer()
	pb.RegisterMonoFSRouterServer(routerServer, routerImpl)
	routerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(router) error = %v", err)
	}
	defer routerListener.Close()
	go func() {
		_ = routerServer.Serve(routerListener)
	}()
	defer routerServer.Stop()

	routerConn, err := grpc.NewClient(routerListener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient(router) error = %v", err)
	}
	defer routerConn.Close()

	client := &ShardedClient{
		conns:         make(map[string]*grpc.ClientConn),
		clients:       make(map[string]pb.MonoFSClient),
		routerConn:    routerConn,
		routerClient:  pb.NewMonoFSRouterClient(routerConn),
		stopRefresh:   make(chan struct{}),
		stopHeartbeat: make(chan struct{}),
		rpcTimeout:    time.Second,
	}
	defer func() {
		for _, conn := range client.conns {
			_ = conn.Close()
		}
	}()

	hookCalls := make(chan struct{}, 4)
	client.SetTopologyChangeHook(func() {
		hookCalls <- struct{}{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.refreshClusterInfo(ctx); err != nil {
		t.Fatalf("refreshClusterInfo(initial) error = %v", err)
	}
	mustReceiveHookCall(t, hookCalls, "initial topology load")

	if err := client.refreshClusterInfo(ctx); err != nil {
		t.Fatalf("refreshClusterInfo(same version) error = %v", err)
	}
	mustNotReceiveHookCall(t, hookCalls, "unchanged topology version")

	routerImpl.setVersion(2)
	if err := client.refreshClusterInfo(ctx); err != nil {
		t.Fatalf("refreshClusterInfo(new version) error = %v", err)
	}
	mustReceiveHookCall(t, hookCalls, "new topology version")
}

func mustReceiveHookCall(t *testing.T, calls <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected topology hook call for %s", label)
	}
}

func mustNotReceiveHookCall(t *testing.T, calls <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-calls:
		t.Fatalf("unexpected topology hook call for %s", label)
	case <-time.After(200 * time.Millisecond):
	}
}
