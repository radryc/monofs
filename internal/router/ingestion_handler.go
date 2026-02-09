package router

import (
	"fmt"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage"
)

// IngestionHandler defines how to process a specific ingestion type.
// Each handler implements type-specific logic for normalizing paths,
// adding directory prefixes, and validating sources.
type IngestionHandler interface {
	// Type returns the ingestion type this handler supports
	Type() pb.IngestionType

	// NormalizeDisplayPath converts the source to a display path
	// source: the original source string (e.g., "lodash@4.17.21")
	// sourceID: optional custom display path provided by user
	NormalizeDisplayPath(source string, sourceID string) string

	// AddDirectoryPrefix adds the proper directory prefix for this type
	// displayPath: the normalized package name
	// source: the original source for reference
	AddDirectoryPrefix(displayPath string, source string) string

	// GetDefaultRef returns the default ref/version if not specified
	GetDefaultRef() string

	// ValidateSource validates the source format
	ValidateSource(source string, ref string) error

	// GetStorageType returns the storage ingestion type
	GetStorageType() storage.IngestionType

	// GetFetchType returns the source type for blob fetching
	GetFetchType() pb.SourceType

	// ExtractCanonicalPath extracts the canonical repository path from backend metadata
	// This is called during ingestion to determine the actual storage location
	// Returns empty string if no canonical path can be determined
	ExtractCanonicalPath(metadata map[string]string) string
}

// IngestionRegistry manages all ingestion handlers.
// It provides a central registry for looking up handlers by ingestion type.
type IngestionRegistry struct {
	handlers map[pb.IngestionType]IngestionHandler
}

// NewIngestionRegistry creates a new registry with all handlers registered.
// To add a new ingestion type, create a new handler file and register it here.
func NewIngestionRegistry() *IngestionRegistry {
	r := &IngestionRegistry{
		handlers: make(map[pb.IngestionType]IngestionHandler),
	}

	// Register all handlers
	r.Register(&GitIngestionHandler{})
	r.Register(&GoModIngestionHandler{})
	r.Register(&NpmIngestionHandler{})
	r.Register(&CargoIngestionHandler{})
	r.Register(&MavenIngestionHandler{})
	r.Register(&S3IngestionHandler{})

	return r
}

// Register adds a handler to the registry.
func (r *IngestionRegistry) Register(handler IngestionHandler) {
	r.handlers[handler.Type()] = handler
}

// Get retrieves a handler for the given ingestion type.
// Returns an error if the type is not supported.
func (r *IngestionRegistry) Get(typ pb.IngestionType) (IngestionHandler, error) {
	handler, ok := r.handlers[typ]
	if !ok {
		return nil, fmt.Errorf("unsupported ingestion type: %v", typ)
	}
	return handler, nil
}
