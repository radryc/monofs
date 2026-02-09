package storage

import (
	"context"
	"fmt"
)

// IngestionType identifies the source type for ingestion.
type IngestionType string

const (
	IngestionTypeGit   IngestionType = "git"
	IngestionTypeGo    IngestionType = "go"    // Go module cache
	IngestionTypeS3    IngestionType = "s3"    // S3 bucket
	IngestionTypeNpm   IngestionType = "npm"   // npm packages
	IngestionTypeMaven IngestionType = "maven" // Maven artifacts
	IngestionTypeCargo IngestionType = "cargo" // Cargo crates
)

// FileMetadata represents file metadata from any source.
type FileMetadata struct {
	Path        string
	Size        uint64
	Mode        uint32
	ModTime     int64
	ContentHash string            // Hash of content (blob hash for Git, checksum for S3, etc.)
	Metadata    map[string]string // Backend-specific metadata
}

// IngestionBackend handles metadata extraction from a source.
type IngestionBackend interface {
	// Type returns the ingestion type identifier.
	Type() IngestionType

	// Initialize prepares the backend (clone repo, download index, etc.).
	Initialize(ctx context.Context, sourceURL string, config map[string]string) error

	// WalkFiles walks all files and yields metadata.
	// Callback returns error to stop iteration.
	WalkFiles(ctx context.Context, fn func(FileMetadata) error) error

	// GetMetadata retrieves metadata for a specific file path.
	GetMetadata(ctx context.Context, path string) (*FileMetadata, error)

	// Cleanup releases resources (delete cloned repo, cleanup cache, etc.).
	Cleanup() error

	// Validate checks if source is accessible and valid.
	Validate(ctx context.Context, sourceURL string, config map[string]string) error
}

// BackendRegistry manages available ingestion backends.
type BackendRegistry struct {
	ingestionBackends map[IngestionType]func() IngestionBackend
}

// DefaultRegistry is the global registry.
var DefaultRegistry = NewBackendRegistry()

// NewBackendRegistry creates a new backend registry.
func NewBackendRegistry() *BackendRegistry {
	return &BackendRegistry{
		ingestionBackends: make(map[IngestionType]func() IngestionBackend),
	}
}

// RegisterIngestionBackend registers an ingestion backend factory.
func (r *BackendRegistry) RegisterIngestionBackend(t IngestionType, factory func() IngestionBackend) {
	r.ingestionBackends[t] = factory
}

// CreateIngestionBackend creates a new ingestion backend instance.
func (r *BackendRegistry) CreateIngestionBackend(t IngestionType) (IngestionBackend, error) {
	factory, ok := r.ingestionBackends[t]
	if !ok {
		return nil, fmt.Errorf("unknown ingestion type: %s", t)
	}
	return factory(), nil
}

// ListIngestionTypes returns available ingestion types.
func (r *BackendRegistry) ListIngestionTypes() []IngestionType {
	types := make([]IngestionType, 0, len(r.ingestionBackends))
	for t := range r.ingestionBackends {
		types = append(types, t)
	}
	return types
}
