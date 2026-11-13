package server

import (
	"context"

	"github.com/radryc/monofs/internal/fetcher"
)

// BlobFetcher is the subset of the fetcher client the storage node depends on.
// It is defined here, in the consumer, so tests can supply an in-memory
// implementation without constructing a gRPC client.
type BlobFetcher interface {
	// CheckCacheSimple reports whether the fetcher already has the blob.
	CheckCacheSimple(ctx context.Context, sourceURL, blobHash string) (bool, error)
	// FetchBlobSimple returns the requested window of a blob's content.
	FetchBlobSimple(ctx context.Context, sourceKey, sourceURL, blobHash, filePath, branch string, offset, length int64, sourceType fetcher.SourceType) ([]byte, error)
	// Prefetch warms the fetcher cache for predicted reads.
	Prefetch(ctx context.Context, requests []*fetcher.FetchRequest, sourceType fetcher.SourceType) error
	// StoreBlobBatch stores a batch of blobs that were written locally.
	StoreBlobBatch(ctx context.Context, sourceKey string, blobs map[string][]byte) (stored int, failed int, err error)
}

// Ensure the concrete client keeps satisfying the seam.
var _ BlobFetcher = (*fetcher.Client)(nil)
