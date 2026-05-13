// Package client provides a gRPC client wrapper for MonoFS backend communication.
package client

import (
	"context"
	"io"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/fsstat"
)

// MonoFSClient is the interface for MonoFS client operations.
// Both simple Client and ShardedClient implement this interface.
type MonoFSClient interface {
	Lookup(ctx context.Context, path string) (*pb.LookupResponse, error)
	GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error)
	ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error)
	Read(ctx context.Context, path string, offset, size int64) ([]byte, error)
	StatFS(ctx context.Context) (fsstat.Snapshot, error)
	QueryLogs(ctx context.Context, query string) ([]byte, error)
	WriteQueryLogs(ctx context.Context, query string, writer io.Writer) error
	Close() error
	// Metrics tracking
	RecordOperation()
	RecordBytesRead(n int64)
	RecordError()
	// Guardian visibility
	IsGuardianVisible() bool
}
