package fetcher

import (
	"github.com/radryc/monofs/internal/storage"
)

// Backend is the canonical interface for fetch backends.
// This is an alias for storage.FetchBackend — all fetch backends implement
// this unified interface defined in the storage package.
type Backend = storage.FetchBackend

// SourceType identifies the data source backend.
// This is an alias for storage.FetchType, providing backward compatibility.
type SourceType = storage.FetchType

const (
	SourceTypeUnknown SourceType = storage.FetchTypeUnknown // Unknown/unset
	SourceTypeGit     SourceType = storage.FetchTypeGit     // Git repository
	SourceTypeBlob    SourceType = storage.FetchTypeBlob    // Packager-based blob archive store (default)
)

// ParseSourceType converts a string to SourceType.
func ParseSourceType(s string) SourceType {
	return storage.ParseFetchType(s)
}

// BackendConfig holds common configuration for all backends.
// This is an alias for storage.BackendConfig.
type BackendConfig = storage.BackendConfig

// FetchRequest contains all information needed to fetch a blob.
// This is an alias for storage.FetchRequest.
type FetchRequest = storage.FetchRequest

// FetchResult contains the fetched blob data.
// This is an alias for storage.FetchResult.
type FetchResult = storage.FetchResult

// BackendStats holds statistics for a backend.
// This is an alias for storage.BackendStats.
type BackendStats = storage.BackendStats

// Registry manages available fetch backend instances.
// Unlike storage.BackendRegistry (which stores factories), this stores
// initialized singleton instances used by the fetcher service at runtime.
type Registry struct {
	backends map[storage.FetchType]Backend
}

// NewRegistry creates a new backend registry.
func NewRegistry() *Registry {
	return &Registry{
		backends: make(map[storage.FetchType]Backend),
	}
}

// Register adds a backend to the registry.
func (r *Registry) Register(backend Backend) {
	r.backends[backend.Type()] = backend
}

// Get returns the backend for a source type.
func (r *Registry) Get(sourceType SourceType) (Backend, bool) {
	backend, ok := r.backends[sourceType]
	return backend, ok
}

// All returns all registered backends.
func (r *Registry) All() []Backend {
	backends := make([]Backend, 0, len(r.backends))
	for _, b := range r.backends {
		backends = append(backends, b)
	}
	return backends
}

// Close shuts down all backends.
func (r *Registry) Close() error {
	for _, b := range r.backends {
		if err := b.Close(); err != nil {
			// Log but continue closing others
		}
	}
	return nil
}
