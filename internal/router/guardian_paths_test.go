package router

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestGuardianPathsWriteReadDeleteAndHistory(t *testing.T) {
	router, nodeClient, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := router.UnregisterClient(ctx, &pb.UnregisterClientRequest{
		ClientId: "guardian-cli",
		Reason:   "test principal persistence",
	}); err != nil {
		t.Fatalf("UnregisterClient() error = %v", err)
	}

	if _, ok := router.validateGuardianToken("secret-token"); !ok {
		t.Fatal("expected persisted guardian token to remain valid after client disconnect")
	}

	writeResp, err := router.UpsertGuardianPaths(ctx, &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{{
			LogicalPath: "/partitions/genomics/intents/workers.yaml",
			Content:     []byte("apiVersion: guardian/v1alpha1\nkind: Intent\n"),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "test write",
			CorrelationId: "corr-write",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}
	if !writeResp.GetSuccess() || len(writeResp.GetVersions()) != 1 {
		t.Fatalf("unexpected upsert response: %+v", writeResp)
	}

	attr, err := nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian/genomics/intents/workers.yaml"})
	if err != nil {
		t.Fatalf("GetAttr() error = %v", err)
	}
	if !attr.GetFound() {
		t.Fatal("expected upserted file to be readable through MonoFS I/O")
	}

	content := readAllFromMonoFSClient(t, nodeClient, "guardian/genomics/intents/workers.yaml")
	if string(content) != "apiVersion: guardian/v1alpha1\nkind: Intent\n" {
		t.Fatalf("read content = %q", string(content))
	}

	deleteResp, err := router.DeleteGuardianPaths(ctx, &pb.DeleteGuardianPathsRequest{
		GuardianToken: "secret-token",
		Deletes: []*pb.GuardianPathDelete{{
			LogicalPath:       "/partitions/genomics/intents/workers.yaml",
			ExpectedVersionId: writeResp.GetVersions()[0].GetVersionId(),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "test delete",
			CorrelationId: "corr-delete",
		},
	})
	if err != nil {
		t.Fatalf("DeleteGuardianPaths() error = %v", err)
	}
	if !deleteResp.GetSuccess() || len(deleteResp.GetTombstones()) != 1 {
		t.Fatalf("unexpected delete response: %+v", deleteResp)
	}

	attr, err = nodeClient.GetAttr(ctx, &pb.GetAttrRequest{Path: "guardian/genomics/intents/workers.yaml"})
	if err != nil {
		t.Fatalf("GetAttr() after delete error = %v", err)
	}
	if attr.GetFound() {
		t.Fatal("expected deleted file to disappear from current MonoFS view")
	}

	listResp, err := router.ListGuardianVersions(ctx, &pb.ListGuardianVersionsRequest{
		GuardianToken: "secret-token",
		LogicalPath:   "/partitions/genomics/intents/workers.yaml",
	})
	if err != nil {
		t.Fatalf("ListGuardianVersions() error = %v", err)
	}
	if len(listResp.GetVersions()) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(listResp.GetVersions()))
	}
	if !listResp.GetVersions()[0].GetTombstone() {
		t.Fatal("expected newest version to be the tombstone")
	}

	getResp, err := router.GetGuardianVersion(ctx, &pb.GetGuardianVersionRequest{
		GuardianToken: "secret-token",
		LogicalPath:   "/partitions/genomics/intents/workers.yaml",
		VersionId:     writeResp.GetVersions()[0].GetVersionId(),
	})
	if err != nil {
		t.Fatalf("GetGuardianVersion() error = %v", err)
	}
	if string(getResp.GetContent()) != string(content) {
		t.Fatalf("historical content = %q, want %q", string(getResp.GetContent()), string(content))
	}
}

func TestSubscribeGuardianChangesReceivesLogicalEvents(t *testing.T) {
	router, _, cleanup := newGuardianRouterTestHarness(t)
	defer cleanup()

	stream := newMockGuardianLogicalChangeStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- router.SubscribeGuardianChanges(&pb.SubscribeGuardianChangesRequest{
			GuardianToken:        "secret-token",
			LogicalPrefixes:      []string{"/partitions/genomics/intents"},
			IncludeInlineContent: true,
		}, stream)
	}()

	waitForCondition(t, func() bool {
		router.guardianLogicalChangeSubsMu.RLock()
		defer router.guardianLogicalChangeSubsMu.RUnlock()
		return len(router.guardianLogicalChangeSubs) == 1
	})

	_, err := router.UpsertGuardianPaths(context.Background(), &pb.UpsertGuardianPathsRequest{
		GuardianToken: "secret-token",
		Writes: []*pb.GuardianPathWrite{{
			LogicalPath: "/partitions/genomics/intents/web.yaml",
			Content:     []byte("kind: Intent\n"),
		}},
		Context: &pb.GuardianMutationContext{
			Reason:        "watch test",
			CorrelationId: "corr-watch",
		},
	})
	if err != nil {
		t.Fatalf("UpsertGuardianPaths() error = %v", err)
	}

	waitForCondition(t, func() bool {
		return len(stream.Events()) == 1
	})

	stream.cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("SubscribeGuardianChanges() error = %v", err)
	}

	events := stream.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].GetLogicalPath() != "/partitions/genomics/intents/web.yaml" {
		t.Fatalf("logical path = %q", events[0].GetLogicalPath())
	}
	if events[0].GetVersionId() == "" {
		t.Fatal("expected version_id in logical change event")
	}
	if string(events[0].GetInlineContent()) != "kind: Intent\n" {
		t.Fatalf("inline content = %q", string(events[0].GetInlineContent()))
	}
}

func TestGetClusterInfoGuardianAlwaysVisible(t *testing.T) {
	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()

	resp, err := router.GetClusterInfo(context.Background(), &pb.ClusterInfoRequest{})
	if err != nil {
		t.Fatalf("GetClusterInfo() error = %v", err)
	}
	if !resp.GetGuardianVisible() {
		t.Fatal("expected guardian namespace to always be visible")
	}
}

func newGuardianRouterTestHarness(t *testing.T) (*Router, pb.MonoFSClient, func()) {
	t.Helper()

	nodeClient, stopNode := newGuardianTestNodeClient(t)
	cfg := DefaultRouterConfig()
	cfg.GuardianStateDir = t.TempDir()
	router := NewRouter(cfg, nil)
	router.nodes["node-1"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:  "node-1",
			Address: "bufnet",
			Healthy: true,
			Weight:  1,
		},
		client: nodeClient,
		status: NodeActive,
	}

	registerResp, err := router.RegisterClient(context.Background(), &pb.RegisterClientRequest{
		ClientId: "guardian-cli",
		GuardianConfig: &pb.GuardianConfig{
			BaseUrl:   "http://guardian.local",
			AuthToken: "secret-token",
		},
	})
	if err != nil {
		t.Fatalf("RegisterClient() error = %v", err)
	}
	if !registerResp.GetSuccess() {
		t.Fatalf("RegisterClient() failed: %+v", registerResp)
	}

	return router, nodeClient, func() {
		_ = router.Close()
		stopNode()
	}
}

type guardianTestNodeServer struct {
	pb.UnimplementedMonoFSServer
	mu       sync.Mutex
	repos    map[string]string
	files    map[string][]byte
	fileMode map[string]uint32
}

func newGuardianTestNodeClient(t *testing.T) (pb.MonoFSClient, func()) {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	node := &guardianTestNodeServer{
		repos:    make(map[string]string),
		files:    make(map[string][]byte),
		fileMode: make(map[string]uint32),
	}
	pb.RegisterMonoFSServer(server, node)
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
		t.Fatalf("grpc.NewClient() error = %v", err)
	}

	return pb.NewMonoFSClient(conn), func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	}
}

func (s *guardianTestNodeServer) RegisterRepository(_ context.Context, req *pb.RegisterRepositoryRequest) (*pb.RegisterRepositoryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[req.GetStorageId()] = req.GetDisplayPath()
	return &pb.RegisterRepositoryResponse{Success: true, Message: "ok"}, nil
}

func (s *guardianTestNodeServer) IngestFileBatch(_ context.Context, req *pb.IngestFileBatchRequest) (*pb.IngestFileBatchResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, file := range req.GetFiles() {
		fullPath := guardianDisplayPathJoin(req.GetDisplayPath(), cleanGuardianRelativePath(file.GetPath()))
		s.files[fullPath] = append([]byte(nil), file.GetInlineContent()...)
		s.fileMode[fullPath] = 0o644 | uint32(syscall.S_IFREG)
	}
	s.repos[req.GetStorageId()] = req.GetDisplayPath()
	return &pb.IngestFileBatchResponse{
		Success:       true,
		FilesIngested: int64(len(req.GetFiles())),
	}, nil
}

func (s *guardianTestNodeServer) GetAttr(_ context.Context, req *pb.GetAttrRequest) (*pb.GetAttrResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if content, ok := s.files[req.GetPath()]; ok {
		return &pb.GetAttrResponse{
			Found: true,
			Mode:  s.fileMode[req.GetPath()],
			Size:  uint64(len(content)),
		}, nil
	}
	if s.hasDirectoryLocked(req.GetPath()) {
		return &pb.GetAttrResponse{
			Found: true,
			Mode:  0o755 | uint32(syscall.S_IFDIR),
		}, nil
	}
	return &pb.GetAttrResponse{Found: false}, nil
}

func (s *guardianTestNodeServer) Read(req *pb.ReadRequest, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	s.mu.Lock()
	content, ok := s.files[req.GetPath()]
	s.mu.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "file not found")
	}

	offset := req.GetOffset()
	if offset > int64(len(content)) {
		return nil
	}
	content = content[offset:]
	if size := req.GetSize(); size > 0 && size < int64(len(content)) {
		content = content[:size]
	}
	return stream.Send(&pb.DataChunk{
		Data:   append([]byte(nil), content...),
		Offset: offset,
	})
}

func (s *guardianTestNodeServer) DeleteFile(_ context.Context, req *pb.DeleteFileRequest) (*pb.DeleteFileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	displayPath, ok := s.repos[req.GetStorageId()]
	if !ok {
		return &pb.DeleteFileResponse{Success: false, Message: "unknown storage id"}, nil
	}
	fullPath := guardianDisplayPathJoin(displayPath, cleanGuardianRelativePath(req.GetFilePath()))
	delete(s.files, fullPath)
	delete(s.fileMode, fullPath)
	return &pb.DeleteFileResponse{Success: true, Message: "deleted"}, nil
}

func (s *guardianTestNodeServer) DeleteDirectoryRecursive(_ context.Context, req *pb.DeleteDirectoryRecursiveRequest) (*pb.DeleteDirectoryRecursiveResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	displayPath, ok := s.repos[req.GetStorageId()]
	if !ok {
		return &pb.DeleteDirectoryRecursiveResponse{Success: false, Message: "unknown storage id"}, nil
	}
	prefix := guardianDisplayPathJoin(displayPath, cleanGuardianRelativePath(req.GetDirPath()))
	if prefix != "" {
		prefix += "/"
	}

	var filesDeleted int64
	for path := range s.files {
		if strings.HasPrefix(path, prefix) || path == strings.TrimSuffix(prefix, "/") {
			delete(s.files, path)
			delete(s.fileMode, path)
			filesDeleted++
		}
	}
	return &pb.DeleteDirectoryRecursiveResponse{
		Success:      true,
		Message:      "deleted",
		FilesDeleted: filesDeleted,
		DirsDeleted:  1,
	}, nil
}

func (s *guardianTestNodeServer) DeleteRepository(_ context.Context, req *pb.DeleteRepositoryOnNodeRequest) (*pb.DeleteRepositoryOnNodeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	displayPath, ok := s.repos[req.GetStorageId()]
	if !ok {
		return &pb.DeleteRepositoryOnNodeResponse{Success: true, Message: "nothing to delete"}, nil
	}

	var filesDeleted int64
	for path := range s.files {
		if path == displayPath || strings.HasPrefix(path, displayPath+"/") {
			delete(s.files, path)
			delete(s.fileMode, path)
			filesDeleted++
		}
	}
	delete(s.repos, req.GetStorageId())
	return &pb.DeleteRepositoryOnNodeResponse{
		Success:      true,
		Message:      "deleted",
		FilesDeleted: filesDeleted,
		DirsDeleted:  1,
	}, nil
}

func (s *guardianTestNodeServer) hasDirectoryLocked(path string) bool {
	if path == "" {
		return true
	}
	for _, repoPath := range s.repos {
		if repoPath == path || strings.HasPrefix(repoPath, path+"/") {
			return true
		}
	}
	prefix := path + "/"
	for filePath := range s.files {
		if strings.HasPrefix(filePath, prefix) {
			return true
		}
	}
	return false
}

func readAllFromMonoFSClient(t *testing.T, client pb.MonoFSClient, path string) []byte {
	t.Helper()

	stream, err := client.Read(context.Background(), &pb.ReadRequest{
		Path:   path,
		Offset: 0,
		Size:   0,
	})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	var out []byte
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, context.Canceled) {
			t.Fatalf("Read() canceled unexpectedly: %v", err)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Read().Recv() error = %v", err)
		}
		out = append(out, chunk.GetData()...)
	}
	return out
}

type mockGuardianLogicalChangeStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	events []*pb.GuardianChangeEvent
}

func newMockGuardianLogicalChangeStream() *mockGuardianLogicalChangeStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &mockGuardianLogicalChangeStream{
		ctx:    ctx,
		cancel: cancel,
	}
}

func (m *mockGuardianLogicalChangeStream) SetHeader(metadata.MD) error { return nil }

func (m *mockGuardianLogicalChangeStream) SendHeader(metadata.MD) error { return nil }

func (m *mockGuardianLogicalChangeStream) SetTrailer(metadata.MD) {}

func (m *mockGuardianLogicalChangeStream) Context() context.Context { return m.ctx }

func (m *mockGuardianLogicalChangeStream) SendMsg(any) error { return nil }

func (m *mockGuardianLogicalChangeStream) RecvMsg(any) error { return nil }

func (m *mockGuardianLogicalChangeStream) Send(event *pb.GuardianChangeEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, cloneGuardianLogicalChangeEvent(event))
	return nil
}

func (m *mockGuardianLogicalChangeStream) Events() []*pb.GuardianChangeEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*pb.GuardianChangeEvent, len(m.events))
	copy(result, m.events)
	return result
}
