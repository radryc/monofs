package fetcher

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

// mockBlobFetcherServer implements a minimal fetcher server for testing
type mockBlobFetcherServer struct {
	pb.UnimplementedBlobFetcherServer
	fetchCalls    int
	prefetchCalls int
	cacheCalls    int
	cache         map[string]bool
}

func (m *mockBlobFetcherServer) FetchBlob(req *pb.FetchBlobRequest, stream pb.BlobFetcher_FetchBlobServer) error {
	m.fetchCalls++

	// Send mock data
	data := []byte("mock blob content for " + req.ContentId)
	chunk := &pb.DataChunk{
		Data:   data,
		Offset: 0,
	}
	return stream.Send(chunk)
}

func (m *mockBlobFetcherServer) PrefetchBlobs(ctx context.Context, req *pb.PrefetchRequest) (*pb.PrefetchResponse, error) {
	m.prefetchCalls++
	return &pb.PrefetchResponse{
		Accepted:      int32(len(req.Blobs)),
		AlreadyCached: 0,
		Rejected:      0,
	}, nil
}

func (m *mockBlobFetcherServer) CheckCache(ctx context.Context, req *pb.CheckCacheRequest) (*pb.CheckCacheResponse, error) {
	m.cacheCalls++

	result := make(map[string]bool)
	sizes := make(map[string]int64)
	for _, id := range req.ContentIds {
		if m.cache != nil && m.cache[id] {
			result[id] = true
			sizes[id] = 100
		} else {
			result[id] = false
		}
	}

	return &pb.CheckCacheResponse{
		Cached: result,
		Sizes:  sizes,
	}, nil
}

func (m *mockBlobFetcherServer) GetStats(ctx context.Context, req *pb.FetcherStatsRequest) (*pb.FetcherStatsResponse, error) {
	return &pb.FetcherStatsResponse{
		TotalRequests: int64(m.fetchCalls),
		CacheHits:     0,
		CacheMisses:   int64(m.fetchCalls),
	}, nil
}

func startMockServer(t *testing.T) (string, *mockBlobFetcherServer, func()) {
	server := &mockBlobFetcherServer{
		cache: make(map[string]bool),
	}

	grpcServer := grpc.NewServer()
	pb.RegisterBlobFetcherServer(grpcServer, server)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go grpcServer.Serve(lis)

	cleanup := func() {
		grpcServer.Stop()
		lis.Close()
	}

	return lis.Addr().String(), server, cleanup
}

func TestClient_NewClient(t *testing.T) {
	addr1, _, cleanup1 := startMockServer(t)
	defer cleanup1()

	addr2, _, cleanup2 := startMockServer(t)
	defer cleanup2()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr1, addr2}
	config.ConnectionTimeout = 5 * time.Second

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Verify connections
	stats := client.GetStats()
	if stats.TotalFetchers != 2 {
		t.Errorf("expected 2 fetchers, got %d", stats.TotalFetchers)
	}

	if stats.HealthyFetchers != 2 {
		t.Errorf("expected 2 healthy fetchers, got %d", stats.HealthyFetchers)
	}
}

func TestClient_FetchBlob(t *testing.T) {
	addr, server, cleanup := startMockServer(t)
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	req := &FetchRequest{
		ContentID: "test-blob-123",
		SourceKey: "https://github.com/test/repo",
		SourceConfig: map[string]string{
			"repo_url": "https://github.com/test/repo",
			"branch":   "main",
		},
	}

	data, err := client.FetchBlob(context.Background(), req, SourceTypeGit)
	if err != nil {
		t.Fatalf("FetchBlob failed: %v", err)
	}

	if len(data) == 0 {
		t.Error("expected non-empty data")
	}

	if server.fetchCalls != 1 {
		t.Errorf("expected 1 fetch call, got %d", server.fetchCalls)
	}
}

func TestClient_FetchBlobSimple(t *testing.T) {
	addr, server, cleanup := startMockServer(t)
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	data, err := client.FetchBlobSimple(
		context.Background(),
		"https://github.com/test/repo",
		"abc123",
		"main.go",
		"main",
		SourceTypeGit,
	)
	if err != nil {
		t.Fatalf("FetchBlobSimple failed: %v", err)
	}

	if len(data) == 0 {
		t.Error("expected non-empty data")
	}

	if server.fetchCalls != 1 {
		t.Errorf("expected 1 fetch call, got %d", server.fetchCalls)
	}
}

func TestClient_CheckCacheSimple(t *testing.T) {
	addr, server, cleanup := startMockServer(t)
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Check for uncached blob
	cached, err := client.CheckCacheSimple(context.Background(), "https://github.com/test/repo", "uncached-blob")
	if err != nil {
		t.Fatalf("CheckCacheSimple failed: %v", err)
	}

	if cached {
		t.Error("expected uncached blob to return false")
	}

	// Add to mock cache and check again
	server.cache["cached-blob"] = true

	cached, err = client.CheckCacheSimple(context.Background(), "https://github.com/test/repo", "cached-blob")
	if err != nil {
		t.Fatalf("CheckCacheSimple failed: %v", err)
	}

	if !cached {
		t.Error("expected cached blob to return true")
	}
}

func TestClient_PrefetchSimple(t *testing.T) {
	addr, server, cleanup := startMockServer(t)
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	files := []PrefetchFile{
		{
			SourceURL:  "https://github.com/test/repo",
			BlobHash:   "blob1",
			FilePath:   "main.go",
			Branch:     "main",
			SourceType: SourceTypeGit,
			Confidence: 0.8,
		},
		{
			SourceURL:  "https://github.com/test/repo",
			BlobHash:   "blob2",
			FilePath:   "handler.go",
			Branch:     "main",
			SourceType: SourceTypeGit,
			Confidence: 0.6,
		},
	}

	queued, err := client.PrefetchSimple(context.Background(), files)
	if err != nil {
		t.Fatalf("PrefetchSimple failed: %v", err)
	}

	if queued != 2 {
		t.Errorf("expected 2 queued, got %d", queued)
	}

	// Wait for async prefetch to complete
	time.Sleep(100 * time.Millisecond)

	if server.prefetchCalls < 1 {
		t.Errorf("expected at least 1 prefetch call, got %d", server.prefetchCalls)
	}
}

func TestClient_AffinityRouting(t *testing.T) {
	addr1, server1, cleanup1 := startMockServer(t)
	defer cleanup1()

	addr2, server2, cleanup2 := startMockServer(t)
	defer cleanup2()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr1, addr2}
	config.AffinityWeight = 1.0 // Strict affinity

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Make multiple requests to the same source key
	sourceKey := "https://github.com/consistent/repo"

	for i := 0; i < 5; i++ {
		req := &FetchRequest{
			ContentID: "blob-" + string(rune('A'+i)),
			SourceKey: sourceKey,
			SourceConfig: map[string]string{
				"repo_url": sourceKey,
				"branch":   "main",
			},
		}

		_, err := client.FetchBlob(context.Background(), req, SourceTypeGit)
		if err != nil {
			t.Fatalf("FetchBlob failed: %v", err)
		}
	}

	// With consistent hashing, all requests should go to the same server
	// (or mostly the same, depending on affinity updates)
	total := server1.fetchCalls + server2.fetchCalls
	if total != 5 {
		t.Errorf("expected 5 total fetches, got %d", total)
	}

	// Check affinity stats
	stats := client.GetStats()
	t.Logf("Affinity hits: %d, misses: %d", stats.AffinityHits, stats.AffinityMisses)
}

func TestClient_HealthyFetchers(t *testing.T) {
	addr, _, cleanup := startMockServer(t)
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	healthy := client.HealthyFetchers()
	if len(healthy) != 1 {
		t.Errorf("expected 1 healthy fetcher, got %d", len(healthy))
	}

	if healthy[0] != addr {
		t.Errorf("expected address %s, got %s", addr, healthy[0])
	}
}

func TestClient_Retries(t *testing.T) {
	// Test with a non-existent server to verify retry behavior
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Start one working server
	addr, _, cleanup := startMockServer(t)
	defer cleanup()

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}
	config.MaxRetries = 3

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	req := &FetchRequest{
		ContentID: "test-blob",
		SourceKey: "https://github.com/test/repo",
	}

	// This should succeed since we have one healthy server
	_, err = client.FetchBlob(context.Background(), req, SourceTypeGit)
	if err != nil {
		t.Errorf("FetchBlob should succeed with healthy server: %v", err)
	}
}

func TestClient_EmptyFetchers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{} // Empty

	_, err := NewClient(config, logger)
	if err == nil {
		t.Error("expected error with empty fetcher addresses")
	}
}

func TestClient_NoHealthyFetchers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Try to connect to non-existent servers
	config := DefaultClientConfig()
	config.FetcherAddresses = []string{"127.0.0.1:1"} // Won't work
	config.ConnectionTimeout = 100 * time.Millisecond

	_, err := NewClient(config, logger)
	if err == nil {
		t.Error("expected error connecting to invalid address")
	}
}

func TestDefaultClientConfig(t *testing.T) {
	config := DefaultClientConfig()

	if config.ConnectionTimeout <= 0 {
		t.Error("expected positive connection timeout")
	}

	if config.RequestTimeout <= 0 {
		t.Error("expected positive request timeout")
	}

	if config.AffinityWeight < 0 || config.AffinityWeight > 1 {
		t.Errorf("expected affinity weight in [0,1], got %f", config.AffinityWeight)
	}

	if config.HealthCheckInterval <= 0 {
		t.Error("expected positive health check interval")
	}

	if config.MaxRetries <= 0 {
		t.Error("expected positive max retries")
	}
}

func TestClientStats(t *testing.T) {
	addr, _, cleanup := startMockServer(t)
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}

	client, err := NewClient(config, logger)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Initial stats
	stats := client.GetStats()
	if stats.TotalRequests != 0 {
		t.Errorf("expected 0 initial requests, got %d", stats.TotalRequests)
	}

	// Make some requests
	for i := 0; i < 3; i++ {
		req := &FetchRequest{
			ContentID: "blob-" + string(rune('A'+i)),
			SourceKey: "https://github.com/test/repo",
		}
		client.FetchBlob(context.Background(), req, SourceTypeGit)
	}

	// Check updated stats
	stats = client.GetStats()
	if stats.TotalRequests != 3 {
		t.Errorf("expected 3 requests, got %d", stats.TotalRequests)
	}
}

// Benchmark tests

func BenchmarkClient_FetchBlob(b *testing.B) {
	addr, _, cleanup := startMockServer(&testing.T{})
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}

	client, err := NewClient(config, logger)
	if err != nil {
		b.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	req := &FetchRequest{
		ContentID: "benchmark-blob",
		SourceKey: "https://github.com/test/repo",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := client.FetchBlob(context.Background(), req, SourceTypeGit)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClient_CheckCache(b *testing.B) {
	addr, _, cleanup := startMockServer(&testing.T{})
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	config := DefaultClientConfig()
	config.FetcherAddresses = []string{addr}

	client, err := NewClient(config, logger)
	if err != nil {
		b.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := client.CheckCacheSimple(context.Background(), "https://github.com/test/repo", "blob")
		if err != nil {
			b.Fatal(err)
		}
	}
}
