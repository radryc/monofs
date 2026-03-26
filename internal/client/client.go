// Package client provides a gRPC client wrapper for MonoFS backend communication.
package client

import (
	"context"

	pb "github.com/radryc/monofs/api/proto"
)

// MonoFSClient is the interface for MonoFS client operations.
// Both simple Client and ShardedClient implement this interface.
type MonoFSClient interface {
	Lookup(ctx context.Context, path string) (*pb.LookupResponse, error)
	GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error)
	ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error)
	Read(ctx context.Context, path string, offset, size int64) ([]byte, error)
	Close() error
	// Metrics tracking
	RecordOperation()
	RecordBytesRead(n int64)
	RecordError()
	// Guardian visibility
	IsGuardianVisible() bool
}
