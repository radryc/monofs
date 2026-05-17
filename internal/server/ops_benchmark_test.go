package server

import (
	"context"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

// noopReadDirStream discards all sent entries; used to benchmark ReadDir without network overhead.
type noopReadDirStream struct {
	grpc.ServerStream
}

func (noopReadDirStream) Send(*pb.DirEntry) error { return nil }
func (noopReadDirStream) Context() context.Context { return context.Background() }

func newBenchStub(b *testing.B) *StubServer {
	b.Helper()
	s := NewStubServer("bench", "localhost:9999", nil)
	return s
}

func BenchmarkLookupHit(b *testing.B) {
	s := newBenchStub(b)
	req := &pb.LookupRequest{ParentPath: "src", Name: "main.go"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Lookup(ctx, req)
	}
}

func BenchmarkLookupMiss(b *testing.B) {
	s := newBenchStub(b)
	req := &pb.LookupRequest{ParentPath: "src", Name: "nonexistent.go"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Lookup(ctx, req)
	}
}

func BenchmarkGetAttrFile(b *testing.B) {
	s := newBenchStub(b)
	req := &pb.GetAttrRequest{Path: "README.md"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.GetAttr(ctx, req)
	}
}

func BenchmarkGetAttrDir(b *testing.B) {
	s := newBenchStub(b)
	req := &pb.GetAttrRequest{Path: "src"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.GetAttr(ctx, req)
	}
}

func BenchmarkReadDirRoot(b *testing.B) {
	s := newBenchStub(b)
	req := &pb.ReadDirRequest{Path: ""}
	stream := noopReadDirStream{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.ReadDir(req, stream)
	}
}

func BenchmarkReadDirSubdir(b *testing.B) {
	s := newBenchStub(b)
	req := &pb.ReadDirRequest{Path: "src"}
	stream := noopReadDirStream{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.ReadDir(req, stream)
	}
}
