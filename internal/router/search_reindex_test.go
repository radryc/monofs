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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeSearchServer records IndexRepository calls so tests can assert re-index
// triggers.
type fakeSearchServer struct {
	pb.UnimplementedMonoFSSearchServer
	mu     sync.Mutex
	ids    []string
	queued bool
}

func (f *fakeSearchServer) IndexRepository(_ context.Context, req *pb.IndexRequest) (*pb.IndexResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, req.GetStorageId())
	return &pb.IndexResponse{Queued: f.queued}, nil
}

func (f *fakeSearchServer) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

func newFakeSearchClient(t *testing.T, impl *fakeSearchServer) pb.MonoFSSearchClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterMonoFSSearchServer(server, impl)
	go func() { _ = server.Serve(listener) }()

	dialer := func(context.Context, string) (net.Conn, error) { return listener.Dial() }
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial fake search: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	})
	return pb.NewMonoFSSearchClient(conn)
}

func testSearchRouter(t *testing.T, impl *fakeSearchServer) *Router {
	t.Helper()
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	r.searchClient = newFakeSearchClient(t, impl)
	return r
}

func waitForCalls(t *testing.T, impl *fakeSearchServer, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(impl.called()) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d search calls, got %d (%v)", want, len(impl.called()), impl.called())
}

func TestTriggerSearchReindex(t *testing.T) {
	impl := &fakeSearchServer{queued: true}
	r := testSearchRouter(t, impl)

	r.triggerSearchReindex("storage-1", "repo/a", "https://example.com/a", "main", "test")
	waitForCalls(t, impl, 1)

	if got := impl.called(); len(got) != 1 || got[0] != "storage-1" {
		t.Fatalf("expected one call for storage-1, got %v", got)
	}
}

func TestTriggerSearchReindexNoClient(t *testing.T) {
	r := NewRouter(DefaultRouterConfig(), slog.New(slog.DiscardHandler))
	// searchClient is nil; must be a no-op that does not panic.
	r.triggerSearchReindex("storage-1", "repo/a", "s", "main", "test")
}

func TestRequestSearchReindexDebounced(t *testing.T) {
	oldDelay := searchReindexDebounceDelay
	searchReindexDebounceDelay = 30 * time.Millisecond
	defer func() { searchReindexDebounceDelay = oldDelay }()

	impl := &fakeSearchServer{queued: true}
	r := testSearchRouter(t, impl)

	// Three rapid requests for the same storageID coalesce into one.
	r.requestSearchReindexDebounced("storage-1", "repo/a", "s", "main", "guardian_upsert")
	r.requestSearchReindexDebounced("storage-1", "repo/a", "s", "main", "guardian_upsert")
	r.requestSearchReindexDebounced("storage-1", "repo/a", "s", "main", "guardian_upsert")
	// A different storageID is tracked separately.
	r.requestSearchReindexDebounced("storage-2", "repo/b", "s", "main", "guardian_upsert")

	waitForCalls(t, impl, 2)

	// Allow any trailing debounce timers to fire, then assert the final count
	// is exactly two (one per storageID).
	time.Sleep(80 * time.Millisecond)
	got := impl.called()
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 reindex calls (one per storageID), got %d (%v)", len(got), got)
	}
}
