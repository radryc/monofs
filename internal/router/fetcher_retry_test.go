package router

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

type testBlobFetcherServer struct {
	pb.UnimplementedBlobFetcherServer
}

func (s *testBlobFetcherServer) GetStats(context.Context, *pb.FetcherStatsRequest) (*pb.FetcherStatsResponse, error) {
	return &pb.FetcherStatsResponse{
		FetcherId: "test-fetcher",
	}, nil
}

func TestSetFetcherClientRetriesUntilFetcherBecomesAvailable(t *testing.T) {
	oldInterval := fetcherReconnectInterval
	fetcherReconnectInterval = 50 * time.Millisecond
	defer func() {
		fetcherReconnectInterval = oldInterval
	}()

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}

	router := NewRouter(DefaultRouterConfig(), slog.Default())
	defer func() {
		_ = router.Close()
	}()

	if err := router.SetFetcherClient([]string{addr}); err == nil {
		t.Fatalf("expected initial fetcher connection to fail")
	}
	if router.getFetcherClient() != nil {
		t.Fatalf("fetcher client should not be configured yet")
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("start fetcher listener: %v", err)
	}
	defer func() {
		_ = lis.Close()
	}()

	server := grpc.NewServer()
	pb.RegisterBlobFetcherServer(server, &testBlobFetcherServer{})
	defer server.Stop()

	go func() {
		_ = server.Serve(lis)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if router.getFetcherClient() != nil {
			stats, err := router.GetFetcherClusterStats(context.Background(), false)
			if err != nil {
				t.Fatalf("get fetcher cluster stats: %v", err)
			}
			if stats.TotalFetchers != 1 {
				t.Fatalf("expected 1 fetcher, got %d", stats.TotalFetchers)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatal("fetcher client was not configured after fetcher became available")
}
