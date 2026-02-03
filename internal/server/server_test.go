package server

import (
	"context"
	"io"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

func TestNewStubServer(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)

	if server == nil {
		t.Fatal("NewStubServer returned nil")
	}

	if server.nodeID != "node1" {
		t.Errorf("unexpected nodeID: %s", server.nodeID)
	}

	if len(server.files) == 0 {
		t.Error("expected stub data to be initialized")
	}
}

func TestLookupSuccess(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)
	ctx := context.Background()

	req := &pb.LookupRequest{
		ParentPath: "",
		Name:       "README.md",
	}

	resp, err := server.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}

	if !resp.Found {
		t.Error("expected file to be found")
	}

	if resp.Ino == 0 {
		t.Error("expected non-zero inode")
	}
}

func TestGetAttrSuccess(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)
	ctx := context.Background()

	req := &pb.GetAttrRequest{
		Path: "README.md",
	}

	resp, err := server.GetAttr(ctx, req)
	if err != nil {
		t.Fatalf("GetAttr failed: %v", err)
	}

	if !resp.Found {
		t.Error("expected file to be found")
	}

	if resp.Mode == 0 {
		t.Error("expected non-zero mode")
	}
}

// mockReadDirStream implements grpc.ServerStreamingServer for testing
type mockReadDirStream struct {
	grpc.ServerStream
	entries []*pb.DirEntry
}

func (m *mockReadDirStream) Send(entry *pb.DirEntry) error {
	m.entries = append(m.entries, entry)
	return nil
}

func (m *mockReadDirStream) Context() context.Context {
	return context.Background()
}

func TestReadDirRoot(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)

	req := &pb.ReadDirRequest{
		Path: "",
	}

	stream := &mockReadDirStream{}
	err := server.ReadDir(req, stream)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	if len(stream.entries) == 0 {
		t.Error("expected entries in root directory")
	}

	// Check for known entries
	found := make(map[string]bool)
	for _, entry := range stream.entries {
		found[entry.Name] = true
	}

	if !found["README.md"] {
		t.Error("expected README.md in root directory")
	}
}

// mockReadStream implements grpc.ServerStreamingServer for testing
type mockReadStream struct {
	grpc.ServerStream
	chunks []*pb.DataChunk
}

func (m *mockReadStream) Send(chunk *pb.DataChunk) error {
	m.chunks = append(m.chunks, chunk)
	return nil
}

func (m *mockReadStream) Context() context.Context {
	return context.Background()
}

func TestReadFile(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)

	req := &pb.ReadRequest{
		Path:   "README.md",
		Offset: 0,
		Size:   0, // Read all
	}

	stream := &mockReadStream{}
	err := server.Read(req, stream)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if len(stream.chunks) == 0 {
		t.Error("expected data chunks")
	}

	// Reconstruct content
	var content []byte
	for _, chunk := range stream.chunks {
		content = append(content, chunk.Data...)
	}

	if len(content) == 0 {
		t.Error("expected non-zero content length")
	}
}

func TestReadNonexistentFile(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)

	req := &pb.ReadRequest{
		Path:   "nonexistent.txt",
		Offset: 0,
		Size:   0,
	}

	stream := &mockReadStream{}
	err := server.Read(req, stream)
	if err != io.EOF {
		t.Errorf("expected EOF for nonexistent file, got %v", err)
	}
}

func TestCreate(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)
	ctx := context.Background()

	req := &pb.CreateRequest{
		ParentPath: "",
		Name:       "newfile.txt",
		Mode:       0644,
		Flags:      0,
	}

	resp, err := server.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if !resp.Success {
		t.Error("Create was not successful")
	}

	if resp.Ino == 0 {
		t.Error("expected non-zero inode")
	}
}

func TestAuthenticate(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)
	ctx := context.Background()

	req := &pb.AuthRequest{
		Token: "test-token",
	}

	resp, err := server.Authenticate(ctx, req)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}

	if !resp.Success {
		t.Error("authentication should succeed (stub accepts all)")
	}

	if resp.SessionId == "" {
		t.Error("expected session ID")
	}
}

func TestGetNodeInfo(t *testing.T) {
	server := NewStubServer("node1", "localhost:9000", nil)
	ctx := context.Background()

	req := &pb.NodeInfoRequest{}

	resp, err := server.GetNodeInfo(ctx, req)
	if err != nil {
		t.Fatalf("GetNodeInfo failed: %v", err)
	}

	if resp.NodeId != "node1" {
		t.Errorf("unexpected node ID: %s", resp.NodeId)
	}

	if resp.Address != "localhost:9000" {
		t.Errorf("unexpected address: %s", resp.Address)
	}
}

func BenchmarkLookup(b *testing.B) {
	server := NewStubServer("node1", "localhost:9000", nil)
	ctx := context.Background()

	req := &pb.LookupRequest{
		ParentPath: "",
		Name:       "README.md",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		server.Lookup(ctx, req)
	}
}

func BenchmarkGetAttr(b *testing.B) {
	server := NewStubServer("node1", "localhost:9000", nil)
	ctx := context.Background()

	req := &pb.GetAttrRequest{
		Path: "README.md",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		server.GetAttr(ctx, req)
	}
}
