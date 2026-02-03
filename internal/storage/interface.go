package storage

import (
	"context"
	"fmt"
	"io"
)

// IngestionType identifies the source type for ingestion
type IngestionType string

const (
	IngestionTypeGit  IngestionType = "git"
	IngestionTypeGo   IngestionType = "go"   // Future: Go module cache
	IngestionTypeS3   IngestionType = "s3"   // Future: S3 bucket
	IngestionTypeFile IngestionType = "file" // Future: Local filesystem
)

// FetchType identifies the backend for blob storage
type FetchType string

const (
	FetchTypeGit   FetchType = "git"   // Fetch from Git repository
	FetchTypeGoMod FetchType = "gomod" // Fetch from Go module cache
	FetchTypeS3    FetchType = "s3"    // Fetch from S3 bucket
	FetchTypeLocal FetchType = "local" // Fetch from local cache
)

// FileMetadata represents file metadata from any source
type FileMetadata struct {
	Path        string
	Size        uint64
	Mode        uint32
	ModTime     int64
	ContentHash string            // Hash of content (blob hash for Git, checksum for S3, etc.)
	Metadata    map[string]string // Backend-specific metadata
}

// IngestionBackend handles metadata extraction from a source
type IngestionBackend interface {
	// Type returns the ingestion type identifier
	Type() IngestionType

	// Initialize prepares the backend (clone repo, download index, etc.)
	Initialize(ctx context.Context, sourceURL string, config map[string]string) error

	// WalkFiles walks all files and yields metadata
	// Callback returns error to stop iteration
	WalkFiles(ctx context.Context, fn func(FileMetadata) error) error

	// GetMetadata retrieves metadata for a specific file path
	GetMetadata(ctx context.Context, path string) (*FileMetadata, error)

	// Cleanup releases resources (delete cloned repo, cleanup cache, etc.)
	Cleanup() error

	// Validate checks if source is accessible and valid
	Validate(ctx context.Context, sourceURL string, config map[string]string) error
}

// FetchBackend handles blob/content retrieval
type FetchBackend interface {
	// Type returns the fetch type identifier
	Type() FetchType

	// Initialize prepares the backend (open repo, connect to S3, etc.)
	Initialize(ctx context.Context, config map[string]string) error

	// FetchBlob retrieves blob content by hash/key
	FetchBlob(ctx context.Context, blobID string) ([]byte, error)

	// FetchBlobStream retrieves blob content as a stream (for large files)
	FetchBlobStream(ctx context.Context, blobID string) (io.ReadCloser, error)

	// StoreBlob stores blob content (for ingestion with data replication)
	// Returns the blob ID (hash/key)
	StoreBlob(ctx context.Context, data []byte) (string, error)

	// StoreBlobStream stores blob content from a stream
	StoreBlobStream(ctx context.Context, reader io.Reader) (string, error)

	// Cleanup releases resources
	Cleanup() error
}

// BackendRegistry manages available backends
type BackendRegistry struct {
	ingestionBackends map[IngestionType]func() IngestionBackend
	fetchBackends     map[FetchType]func() FetchBackend
}

// DefaultRegistry is the global registry
var DefaultRegistry = NewBackendRegistry()

// NewBackendRegistry creates a new backend registry
func NewBackendRegistry() *BackendRegistry {
	return &BackendRegistry{
		ingestionBackends: make(map[IngestionType]func() IngestionBackend),
		fetchBackends:     make(map[FetchType]func() FetchBackend),
	}
}

// RegisterIngestionBackend registers an ingestion backend factory
func (r *BackendRegistry) RegisterIngestionBackend(t IngestionType, factory func() IngestionBackend) {
	r.ingestionBackends[t] = factory
}

// RegisterFetchBackend registers a fetch backend factory
func (r *BackendRegistry) RegisterFetchBackend(t FetchType, factory func() FetchBackend) {
	r.fetchBackends[t] = factory
}

// CreateIngestionBackend creates a new ingestion backend instance
func (r *BackendRegistry) CreateIngestionBackend(t IngestionType) (IngestionBackend, error) {
	factory, ok := r.ingestionBackends[t]
	if !ok {
		return nil, fmt.Errorf("unknown ingestion type: %s", t)
	}
	return factory(), nil
}

// CreateFetchBackend creates a new fetch backend instance
func (r *BackendRegistry) CreateFetchBackend(t FetchType) (FetchBackend, error) {
	factory, ok := r.fetchBackends[t]
	if !ok {
		return nil, fmt.Errorf("unknown fetch type: %s", t)
	}
	return factory(), nil
}

// ListIngestionTypes returns available ingestion types
func (r *BackendRegistry) ListIngestionTypes() []IngestionType {
	types := make([]IngestionType, 0, len(r.ingestionBackends))
	for t := range r.ingestionBackends {
		types = append(types, t)
	}
	return types
}

// ListFetchTypes returns available fetch types
func (r *BackendRegistry) ListFetchTypes() []FetchType {
	types := make([]FetchType, 0, len(r.fetchBackends))
	for t := range r.fetchBackends {
		types = append(types, t)
	}
	return types
}
