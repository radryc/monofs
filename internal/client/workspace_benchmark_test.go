package client

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func BenchmarkListWorkspaceRepositories(b *testing.B) {
	for _, tc := range []struct {
		name      string
		nodeCount int
		repoCount int
	}{
		{name: "3nodes-64repos", nodeCount: 3, repoCount: 64},
		{name: "3nodes-256repos", nodeCount: 3, repoCount: 256},
	} {
		b.Run(tc.name, func(b *testing.B) {
			client, cleanup := newWorkspaceDiscoveryBenchmarkClient(b, tc.nodeCount, tc.repoCount)
			defer cleanup()

			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				repos, err := client.ListWorkspaceRepositories(ctx)
				if err != nil {
					b.Fatalf("ListWorkspaceRepositories() error = %v", err)
				}
				if len(repos) != tc.repoCount {
					b.Fatalf("ListWorkspaceRepositories() repos = %d, want %d", len(repos), tc.repoCount)
				}
			}
		})
	}
}

func BenchmarkRefreshWorkspaceRepositories(b *testing.B) {
	for _, repoCount := range []int{16, 64} {
		b.Run(fmt.Sprintf("%drepos", repoCount), func(b *testing.B) {
			client, cleanup := newWorkspaceRefreshBenchmarkClient(b)
			defer cleanup()

			repos := benchmarkWorkspaceRepositories(repoCount)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				result, err := client.RefreshWorkspaceRepositories(ctx, repos)
				if err != nil {
					b.Fatalf("RefreshWorkspaceRepositories() error = %v", err)
				}
				if result.Refreshed != repoCount {
					b.Fatalf("RefreshWorkspaceRepositories() refreshed = %d, want %d", result.Refreshed, repoCount)
				}
			}
		})
	}
}

type benchmarkWorkspaceServer struct {
	pb.UnimplementedMonoFSServer
	repositoryIDs []string
	repositories  map[string]*pb.GetRepositoryInfoResponse
}

func (s *benchmarkWorkspaceServer) ListRepositories(ctx context.Context, req *pb.ListRepositoriesRequest) (*pb.ListRepositoriesResponse, error) {
	return &pb.ListRepositoriesResponse{RepositoryIds: append([]string(nil), s.repositoryIDs...)}, nil
}

func (s *benchmarkWorkspaceServer) GetRepositoryInfo(ctx context.Context, req *pb.GetRepositoryInfoRequest) (*pb.GetRepositoryInfoResponse, error) {
	info, ok := s.repositories[req.GetStorageId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "repository not found")
	}
	return proto.Clone(info).(*pb.GetRepositoryInfoResponse), nil
}

type benchmarkWorkspaceRouter struct {
	pb.UnimplementedMonoFSRouterServer
}

func (s *benchmarkWorkspaceRouter) GetClusterInfo(ctx context.Context, req *pb.ClusterInfoRequest) (*pb.ClusterInfoResponse, error) {
	return &pb.ClusterInfoResponse{Version: 1}, nil
}

func (s *benchmarkWorkspaceRouter) IngestRepository(req *pb.IngestRequest, stream grpc.ServerStreamingServer[pb.IngestProgress]) error {
	return stream.Send(&pb.IngestProgress{
		Stage:   pb.IngestProgress_COMPLETED,
		Success: true,
		Message: "refreshed",
	})
}

func newWorkspaceDiscoveryBenchmarkClient(b *testing.B, nodeCount, repoCount int) (*ShardedClient, func()) {
	b.Helper()

	client := &ShardedClient{
		conns:       make(map[string]*grpc.ClientConn, nodeCount),
		clients:     make(map[string]pb.MonoFSClient, nodeCount),
		connected:   true,
		rpcTimeout:  time.Second,
		stopRefresh: make(chan struct{}),
	}

	servers := make([]*grpc.Server, 0, nodeCount)
	listeners := make([]*bufconn.Listener, 0, nodeCount)

	for nodeIndex := 0; nodeIndex < nodeCount; nodeIndex++ {
		repositoryIDs := make([]string, 0, repoCount/nodeCount+1)
		repositories := make(map[string]*pb.GetRepositoryInfoResponse)
		for repoIndex := nodeIndex; repoIndex < repoCount; repoIndex += nodeCount {
			repo := benchmarkWorkspaceRepository(repoIndex)
			repositoryIDs = append(repositoryIDs, repo.StorageID)
			repositories[repo.StorageID] = &pb.GetRepositoryInfoResponse{
				DisplayPath:   repo.DisplayPath,
				Source:        repo.Source,
				Ref:           repo.Ref,
				CommitHash:    repo.CommitHash,
				CommitTime:    repo.CommitTime,
				CommitMessage: repo.CommitMessage,
			}
		}

		listener := bufconn.Listen(1024 * 1024)
		server := grpc.NewServer()
		pb.RegisterMonoFSServer(server, &benchmarkWorkspaceServer{
			repositoryIDs: repositoryIDs,
			repositories:  repositories,
		})
		go func() {
			_ = server.Serve(listener)
		}()

		address := fmt.Sprintf("bench-node-%d", nodeIndex)
		conn, err := grpc.DialContext(
			context.Background(),
			address,
			grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
				return listener.Dial()
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			b.Fatalf("DialContext(%q) error = %v", address, err)
		}

		client.conns[address] = conn
		client.clients[address] = pb.NewMonoFSClient(conn)
		servers = append(servers, server)
		listeners = append(listeners, listener)
	}

	cleanup := func() {
		_ = client.Close()
		for _, server := range servers {
			server.Stop()
		}
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}

	return client, cleanup
}

func newWorkspaceRefreshBenchmarkClient(b *testing.B) (*ShardedClient, func()) {
	b.Helper()

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pb.RegisterMonoFSRouterServer(server, &benchmarkWorkspaceRouter{})
	go func() {
		_ = server.Serve(listener)
	}()

	conn, err := grpc.DialContext(
		context.Background(),
		"bench-router",
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		b.Fatalf("DialContext(router) error = %v", err)
	}

	client := &ShardedClient{
		conns:          make(map[string]*grpc.ClientConn),
		clients:        make(map[string]pb.MonoFSClient),
		routerConn:     conn,
		routerClient:   pb.NewMonoFSRouterClient(conn),
		connected:      true,
		clusterVersion: 1,
		rpcTimeout:     250 * time.Millisecond,
		stopRefresh:    make(chan struct{}),
	}

	cleanup := func() {
		_ = client.Close()
		server.Stop()
		_ = listener.Close()
	}

	return client, cleanup
}

func benchmarkWorkspaceRepositories(repoCount int) []WorkspaceRepository {
	repos := make([]WorkspaceRepository, 0, repoCount)
	for repoIndex := 0; repoIndex < repoCount; repoIndex++ {
		repos = append(repos, benchmarkWorkspaceRepository(repoIndex))
	}
	return repos
}

func benchmarkWorkspaceRepository(repoIndex int) WorkspaceRepository {
	storageID := fmt.Sprintf("repo-%04d", repoIndex)
	displayPath := fmt.Sprintf("github.com/acme/repo%04d", repoIndex)
	return WorkspaceRepository{
		StorageID:     storageID,
		DisplayPath:   displayPath,
		Source:        fmt.Sprintf("https://example.com/acme/repo%04d.git", repoIndex),
		Ref:           "main",
		CommitHash:    fmt.Sprintf("commit-%04d", repoIndex),
		CommitTime:    int64(1_700_000_000 + repoIndex),
		CommitMessage: "benchmark repository",
	}
}
