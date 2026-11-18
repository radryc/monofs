package router

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type authRecordingSearchServer struct {
	pb.UnimplementedMonoFSSearchServer
	mu   sync.Mutex
	auth []string
}

func (s *authRecordingSearchServer) GetStats(ctx context.Context, _ *pb.StatsRequest) (*pb.StatsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	value := ""
	if vals := md.Get("authorization"); len(vals) > 0 {
		value = vals[0]
	}
	s.mu.Lock()
	s.auth = append(s.auth, value)
	s.mu.Unlock()
	return &pb.StatsResponse{}, nil
}

func (s *authRecordingSearchServer) lastAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auth) == 0 {
		return ""
	}
	return s.auth[len(s.auth)-1]
}

func startAuthRecordingSearch(t *testing.T) (*authRecordingSearchServer, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	impl := &authRecordingSearchServer{}
	server := grpc.NewServer()
	pb.RegisterMonoFSSearchServer(server, impl)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(func() {
		server.Stop()
		_ = lis.Close()
	})
	return impl, lis.Addr().String()
}

func newSearchAuthTestRouter(t *testing.T, addr, token string) *Router {
	t.Helper()
	cfg := DefaultRouterConfig()
	cfg.SearchToken = token
	r := NewRouter(cfg, slog.New(slog.DiscardHandler))
	if err := r.SetSearchClient(addr); err != nil {
		t.Fatalf("SetSearchClient: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestSearchDialPresentsConfiguredBearer(t *testing.T) {
	impl, addr := startAuthRecordingSearch(t)
	r := newSearchAuthTestRouter(t, addr, "router-token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.searchClientSnapshot().GetStats(ctx, &pb.StatsRequest{}); err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if got := impl.lastAuth(); got != "Bearer router-token" {
		t.Fatalf("search authorization = %q, want %q", got, "Bearer router-token")
	}
}

func TestSearchDialPreservesForwardedCallerBearer(t *testing.T) {
	impl, addr := startAuthRecordingSearch(t)
	r := newSearchAuthTestRouter(t, addr, "router-token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer caller-token")
	if _, err := r.searchClientSnapshot().GetStats(ctx, &pb.StatsRequest{}); err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if got := impl.lastAuth(); got != "Bearer caller-token" {
		t.Fatalf("search authorization = %q, want the forwarded caller token", got)
	}
}

func TestSearchDialWithoutTokenSendsNoCredential(t *testing.T) {
	impl, addr := startAuthRecordingSearch(t)
	r := newSearchAuthTestRouter(t, addr, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.searchClientSnapshot().GetStats(ctx, &pb.StatsRequest{}); err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if got := impl.lastAuth(); got != "" {
		t.Fatalf("search authorization = %q, want no credential", got)
	}
}
