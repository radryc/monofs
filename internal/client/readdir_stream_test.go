package client

import (
	"context"
	"net"
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

// readDirTestNode serves a fixed (sorted) directory listing.
type readDirTestNode struct {
	pb.UnimplementedMonoFSServer
	entries   []*pb.DirEntry
	failAfter int // >0: return an error after this many entries
}

func (n *readDirTestNode) ReadDir(_ *pb.ReadDirRequest, stream pb.MonoFS_ReadDirServer) error {
	for i, e := range n.entries {
		if n.failAfter > 0 && i >= n.failAfter {
			return status.Error(codes.Internal, "boom")
		}
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	return nil
}

func newReadDirTestClient(t *testing.T, nodes map[string]*readDirTestNode) *ShardedClient {
	t.Helper()
	sc := &ShardedClient{
		conns:       make(map[string]*grpc.ClientConn),
		clients:     make(map[string]pb.MonoFSClient),
		connected:   true,
		rpcTimeout:  2 * time.Second,
		stopRefresh: make(chan struct{}),
	}
	shardNodes := make([]sharding.Node, 0, len(nodes))
	for id, node := range nodes {
		id, node := id, node
		listener := bufconn.Listen(1 << 20)
		server := grpc.NewServer()
		pb.RegisterMonoFSServer(server, node)
		go func() { _ = server.Serve(listener) }()

		dialer := func(context.Context, string) (net.Conn, error) { return listener.Dial() }
		conn, err := grpc.DialContext(context.Background(), "bufnet",
			grpc.WithContextDialer(dialer),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dial %s: %v", id, err)
		}
		sc.conns[id] = conn
		sc.clients[id] = pb.NewMonoFSClient(conn)
		shardNodes = append(shardNodes, sharding.Node{ID: id, Healthy: true, Weight: 1})

		t.Cleanup(func() {
			conn.Close()
			server.Stop()
			listener.Close()
		})
	}
	sc.hrw = sharding.NewHRW(shardNodes)
	return sc
}

func TestReadDirStreamMergesAndDeduplicates(t *testing.T) {
	sc := newReadDirTestClient(t, map[string]*readDirTestNode{
		"n1": {entries: []*pb.DirEntry{{Name: "a"}, {Name: "c"}}},
		"n2": {entries: []*pb.DirEntry{{Name: "b"}, {Name: "c"}}},
		"n3": {entries: []*pb.DirEntry{{Name: "a"}, {Name: "d"}}},
	})

	var got []string
	if err := sc.ReadDirStream(context.Background(), "repo/dir", func(e *pb.DirEntry) error {
		got = append(got, e.Name)
		return nil
	}); err != nil {
		t.Fatalf("ReadDirStream: %v", err)
	}

	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("merged entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("merged entries = %v, want %v (sorted, deduplicated)", got, want)
		}
	}
}

func TestReadDirStreamPropagatesNodeError(t *testing.T) {
	sc := newReadDirTestClient(t, map[string]*readDirTestNode{
		"ok":  {entries: []*pb.DirEntry{{Name: "a"}, {Name: "b"}}},
		"bad": {entries: []*pb.DirEntry{{Name: "a"}, {Name: "c"}}, failAfter: 1},
	})

	err := sc.ReadDirStream(context.Background(), "repo/dir", func(e *pb.DirEntry) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error when a node stream fails")
	}
}
