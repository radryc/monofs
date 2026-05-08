package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type traceReadStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *traceReadStream) Send(*pb.DataChunk) error {
	return nil
}

func (s *traceReadStream) Context() context.Context {
	return s.ctx
}

func TestReadMetadataMissAnnotatesTraceWithPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-read-trace-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	server, err := NewServer("test-node", ":9000", filepath.Join(tmpDir, "db"), filepath.Join(tmpDir, "git-cache"), nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	if _, err := server.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId:   "test-storage-id",
		DisplayPath: "repo",
		Source:      "https://example.com/repo.git",
	}); err != nil {
		t.Fatalf("register repository: %v", err)
	}

	recorder := tracetest.NewSpanRecorder()
	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() {
		_ = traceProvider.Shutdown(context.Background())
	}()

	readCtx, span := traceProvider.Tracer("monofs/test").Start(context.Background(), "read")
	err = server.Read(&pb.ReadRequest{Path: "repo/missing.txt"}, &traceReadStream{ctx: readCtx})
	span.End()

	if status.Code(err) != codes.NotFound {
		t.Fatalf("Read() error code = %v, want %v (err=%v)", status.Code(err), codes.NotFound, err)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}

	gotAttrs := map[string]string{}
	for _, attr := range spans[0].Attributes() {
		gotAttrs[string(attr.Key)] = attr.Value.AsString()
	}
	if gotAttrs["monofs.read.path"] != "repo/missing.txt" {
		t.Fatalf("monofs.read.path = %q, want %q", gotAttrs["monofs.read.path"], "repo/missing.txt")
	}
	if gotAttrs["monofs.read.storage_id"] != "test-storage-id" {
		t.Fatalf("monofs.read.storage_id = %q, want %q", gotAttrs["monofs.read.storage_id"], "test-storage-id")
	}
	if gotAttrs["monofs.read.file_path"] != "missing.txt" {
		t.Fatalf("monofs.read.file_path = %q, want %q", gotAttrs["monofs.read.file_path"], "missing.txt")
	}

	events := spans[0].Events()
	if len(events) != 1 {
		t.Fatalf("span events = %d, want 1", len(events))
	}
	if events[0].Name != "monofs.read.metadata_not_found" {
		t.Fatalf("event name = %q, want %q", events[0].Name, "monofs.read.metadata_not_found")
	}

	eventAttrs := map[string]string{}
	for _, attr := range events[0].Attributes {
		eventAttrs[string(attr.Key)] = attr.Value.AsString()
	}
	if eventAttrs["monofs.read.path"] != "repo/missing.txt" {
		t.Fatalf("event monofs.read.path = %q, want %q", eventAttrs["monofs.read.path"], "repo/missing.txt")
	}
	if eventAttrs["monofs.read.storage_id"] != "test-storage-id" {
		t.Fatalf("event monofs.read.storage_id = %q, want %q", eventAttrs["monofs.read.storage_id"], "test-storage-id")
	}
	if eventAttrs["monofs.read.file_path"] != "missing.txt" {
		t.Fatalf("event monofs.read.file_path = %q, want %q", eventAttrs["monofs.read.file_path"], "missing.txt")
	}
	if eventAttrs["monofs.read.lookup_error"] == "" {
		t.Fatal("event monofs.read.lookup_error should not be empty")
	}
}
