package router

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"syscall"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestNativeMountInfoReturnsRootSnapshot(t *testing.T) {
	client, cleanup := newNativeNamespaceTestClient(t, &nativeNamespaceTestNodeServer{})
	defer cleanup()

	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()
	router.nodes["node-1"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:  "node-1",
			Address: "bufnet",
			Healthy: true,
			Weight:  1,
		},
		client: client,
		status: NodeActive,
	}

	info, err := router.NativeMountInfo(context.Background())
	if err != nil {
		t.Fatalf("NativeMountInfo() error = %v", err)
	}
	if !info.Root.GetFound() {
		t.Fatal("expected root entry to be present")
	}
	if got, want := info.Root.GetIno(), uint64(1); got != want {
		t.Fatalf("root ino = %d, want %d", got, want)
	}
	if got, want := uint64(info.NamespaceGeneration), router.nativeEffectiveGeneration(); got != want {
		t.Fatalf("namespace generation = %d, want %d", got, want)
	}
	if got, want := info.TTLs, DefaultNativeTTLConfig(); got != want {
		t.Fatalf("ttls = %+v, want %+v", got, want)
	}
}

func TestNativeLookupAndGetAttr(t *testing.T) {
	server := &nativeNamespaceTestNodeServer{
		lookup: map[string]*pb.LookupResponse{
			"github.com/acme/repo": {
				Found: true,
				Ino:   101,
				Mode:  0o755 | uint32(syscall.S_IFDIR),
			},
		},
		attr: map[string]*pb.GetAttrResponse{
			"github.com/acme/repo": {
				Found: true,
				Ino:   101,
				Mode:  0o755 | uint32(syscall.S_IFDIR),
				Nlink: 2,
			},
		},
	}
	client, cleanup := newNativeNamespaceTestClient(t, server)
	defer cleanup()

	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()
	router.nodes["node-1"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:  "node-1",
			Address: "bufnet",
			Healthy: true,
			Weight:  1,
		},
		client: client,
		status: NodeActive,
	}

	lookup, version, err := router.NativeLookup(context.Background(), "github.com/acme/repo")
	if err != nil {
		t.Fatalf("NativeLookup() error = %v", err)
	}
	if !lookup.GetFound() || lookup.GetIno() != 101 {
		t.Fatalf("NativeLookup() = %+v", lookup)
	}
	if got, want := uint64(version), router.nativeEffectiveGeneration(); got != want {
		t.Fatalf("lookup version = %d, want %d", got, want)
	}

	attr, version, err := router.NativeGetAttr(context.Background(), "github.com/acme/repo")
	if err != nil {
		t.Fatalf("NativeGetAttr() error = %v", err)
	}
	if !attr.GetFound() || attr.GetNlink() != 2 {
		t.Fatalf("NativeGetAttr() = %+v", attr)
	}
	if got, want := uint64(version), router.nativeEffectiveGeneration(); got != want {
		t.Fatalf("getattr version = %d, want %d", got, want)
	}
}

func TestNativeReadDirMergesHealthyNodes(t *testing.T) {
	clientA, cleanupA := newNativeNamespaceTestClient(t, &nativeNamespaceTestNodeServer{
		readdir: map[string][]*pb.DirEntry{
			"github.com/acme/repo": {
				{Name: "a.txt", Mode: 0o644 | uint32(syscall.S_IFREG), Ino: 11},
				{Name: "pkg", Mode: 0o755 | uint32(syscall.S_IFDIR), Ino: 12},
			},
		},
	})
	defer cleanupA()

	clientB, cleanupB := newNativeNamespaceTestClient(t, &nativeNamespaceTestNodeServer{
		readdir: map[string][]*pb.DirEntry{
			"github.com/acme/repo": {
				{Name: "b.txt", Mode: 0o644 | uint32(syscall.S_IFREG), Ino: 21},
				{Name: "pkg", Mode: 0o755 | uint32(syscall.S_IFDIR), Ino: 22},
			},
		},
	})
	defer cleanupB()

	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()
	router.nodes["node-a"] = &nodeState{
		info:   &pb.NodeInfo{NodeId: "node-a", Address: "bufnet", Healthy: true, Weight: 1},
		client: clientA,
		status: NodeActive,
	}
	router.nodes["node-b"] = &nodeState{
		info:   &pb.NodeInfo{NodeId: "node-b", Address: "bufnet", Healthy: true, Weight: 1},
		client: clientB,
		status: NodeActive,
	}

	entries, version, err := router.NativeReadDir(context.Background(), "github.com/acme/repo")
	if err != nil {
		t.Fatalf("NativeReadDir() error = %v", err)
	}
	if got, want := uint64(version), router.nativeEffectiveGeneration(); got != want {
		t.Fatalf("readdir version = %d, want %d", got, want)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.GetName())
	}
	if got, want := names, []string{"a.txt", "b.txt", "pkg"}; !equalStrings(got, want) {
		t.Fatalf("readdir names = %v, want %v", got, want)
	}
}

func TestNativeReadDirFailsWhenNodeNeverCompletes(t *testing.T) {
	clientA, cleanupA := newNativeNamespaceTestClient(t, &nativeNamespaceTestNodeServer{
		readdir: map[string][]*pb.DirEntry{
			"github.com/acme/repo": {
				{Name: "a.txt", Mode: 0o644 | uint32(syscall.S_IFREG), Ino: 11},
			},
		},
	})
	defer cleanupA()

	clientB, cleanupB := newNativeNamespaceTestClient(t, &nativeNamespaceTestNodeServer{
		readdirFailures: map[string]int{
			"github.com/acme/repo": 2,
		},
	})
	defer cleanupB()

	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()
	router.nodes["node-a"] = &nodeState{
		info:   &pb.NodeInfo{NodeId: "node-a", Address: "bufnet", Healthy: true, Weight: 1},
		client: clientA,
		status: NodeActive,
	}
	router.nodes["node-b"] = &nodeState{
		info:   &pb.NodeInfo{NodeId: "node-b", Address: "bufnet", Healthy: true, Weight: 1},
		client: clientB,
		status: NodeActive,
	}

	if _, _, err := router.NativeReadDir(context.Background(), "github.com/acme/repo"); err == nil {
		t.Fatal("expected NativeReadDir() to fail on persistent node error")
	}
}

func TestNativeStatFSAggregatesHealthyActiveNodes(t *testing.T) {
	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()
	router.nodes["node-a"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:         "node-a",
			Healthy:        true,
			DiskTotalBytes: 10 * nativeNamespaceBlockSize,
			DiskFreeBytes:  6 * nativeNamespaceBlockSize,
			TotalFiles:     100,
		},
		status: NodeActive,
	}
	router.nodes["node-b"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:         "node-b",
			Healthy:        true,
			DiskTotalBytes: 20 * nativeNamespaceBlockSize,
			DiskFreeBytes:  5 * nativeNamespaceBlockSize,
			TotalFiles:     50,
		},
		status: NodeActive,
	}
	router.nodes["node-c"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:         "node-c",
			Healthy:        false,
			DiskTotalBytes: 99 * nativeNamespaceBlockSize,
			DiskFreeBytes:  99 * nativeNamespaceBlockSize,
			TotalFiles:     9999,
		},
		status: NodeActive,
	}

	statfs, err := router.NativeStatFS(context.Background())
	if err != nil {
		t.Fatalf("NativeStatFS() error = %v", err)
	}
	if got, want := statfs.Blocks, uint64(30); got != want {
		t.Fatalf("blocks = %d, want %d", got, want)
	}
	if got, want := statfs.Bfree, uint64(11); got != want {
		t.Fatalf("bfree = %d, want %d", got, want)
	}
	if got, want := statfs.Files, uint64(150+nativeNamespaceFreeFiles); got != want {
		t.Fatalf("files = %d, want %d", got, want)
	}
}

func TestDeleteRepositoryBumpsNativeGeneration(t *testing.T) {
	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()

	router.ingestedRepos["repo-1"] = &ingestedRepo{
		repoID:     "github.com/acme/repo",
		filesCount: 1,
	}

	before := router.nativeEffectiveGeneration()
	resp, err := router.DeleteRepository(context.Background(), &pb.DeleteRepositoryRequest{
		StorageId: "repo-1",
	})
	if err != nil {
		t.Fatalf("DeleteRepository() error = %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("DeleteRepository() = %+v", resp)
	}
	if after := router.nativeEffectiveGeneration(); after == before {
		t.Fatalf("native generation did not change: before=%d after=%d", before, after)
	}
}

type nativeNamespaceTestNodeServer struct {
	pb.UnimplementedMonoFSServer
	mu              sync.Mutex
	lookup          map[string]*pb.LookupResponse
	attr            map[string]*pb.GetAttrResponse
	readdir         map[string][]*pb.DirEntry
	read            map[string][]byte
	readdirFailures map[string]int
}

func newNativeNamespaceTestClient(t *testing.T, serverImpl *nativeNamespaceTestNodeServer) (pb.MonoFSClient, func()) {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterMonoFSServer(server, serverImpl)
	go func() {
		_ = server.Serve(listener)
	}()

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.DialContext() error = %v", err)
	}

	return pb.NewMonoFSClient(conn), func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	}
}

func (s *nativeNamespaceTestNodeServer) Lookup(_ context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := req.GetParentPath()
	if path == "" && req.GetName() != "" {
		path = req.GetName()
	} else if path != "" && req.GetName() != "" {
		path = path + "/" + req.GetName()
	}

	if resp, ok := s.lookup[path]; ok {
		return resp, nil
	}
	return &pb.LookupResponse{Found: false}, nil
}

func (s *nativeNamespaceTestNodeServer) GetAttr(_ context.Context, req *pb.GetAttrRequest) (*pb.GetAttrResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if resp, ok := s.attr[req.GetPath()]; ok {
		return resp, nil
	}
	return &pb.GetAttrResponse{Found: false}, nil
}

func (s *nativeNamespaceTestNodeServer) ReadDir(req *pb.ReadDirRequest, stream grpc.ServerStreamingServer[pb.DirEntry]) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if remaining := s.readdirFailures[req.GetPath()]; remaining > 0 {
		s.readdirFailures[req.GetPath()] = remaining - 1
		return status.Error(codes.Unavailable, "forced readdir failure")
	}

	entries := append([]*pb.DirEntry(nil), s.readdir[req.GetPath()]...)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].GetName() < entries[j].GetName()
	})

	for _, entry := range entries {
		if err := stream.Send(entry); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return err
		}
	}
	return nil
}

func (s *nativeNamespaceTestNodeServer) Read(req *pb.ReadRequest, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, ok := s.read[req.GetPath()]
	if !ok {
		return status.Error(codes.NotFound, "file not found")
	}

	offset := req.GetOffset()
	if offset > int64(len(data)) {
		return nil
	}
	data = data[offset:]
	if size := req.GetSize(); size > 0 && size < int64(len(data)) {
		data = data[:size]
	}

	return stream.Send(&pb.DataChunk{
		Data:   append([]byte(nil), data...),
		Offset: offset,
	})
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
