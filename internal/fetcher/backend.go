package fetcher

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync/atomic"

	pb "github.com/radryc/monofs/api/proto"
)

// Backend defines the interface for fetch backends (Git, Go modules, etc.).
// Each backend handles a specific source type and knows how to retrieve blobs.
type Backend interface {
	// Type returns the source type this backend handles.
	Type() pb.SourceType

	// Initialize sets up the backend with configuration.
	// Called once at service startup.
	Initialize(ctx context.Context, config BackendConfig) error

	// FetchBlob retrieves a blob by content ID.
	// Returns a reader for the blob content and its size.
	// The caller is responsible for closing the reader.
	FetchBlob(ctx context.Context, req *FetchRequest) (*FetchResult, error)

	// FetchBlobStream is like FetchBlob but streams content.
	// Useful for large blobs to avoid memory pressure.
	// Default implementation wraps FetchBlob; override for true streaming (e.g. Git).
	FetchBlobStream(ctx context.Context, req *FetchRequest) (io.ReadCloser, int64, error)

	// Warmup prepares the backend for fetching from a specific source.
	// For Git: clones/fetches the repo. For GoMod: downloads module index.
	// This is called proactively for repo-affinity routing.
	Warmup(ctx context.Context, sourceKey string, config map[string]string) error

	// CachedSources returns list of sources this backend has warmed up.
	CachedSources() []string

	// Cleanup releases resources for a specific source.
	// Called when source hasn't been accessed for a while.
	Cleanup(ctx context.Context, sourceKey string) error

	// Close shuts down the backend and releases all resources.
	Close() error

	// Stats returns backend-specific statistics.
	Stats() BackendStats
}

// IsGoModPath detects if a URL is a Go module path (vs Git URL).
// Go modules typically don't start with http:// or git:// and follow
// domain/path format like "github.com/user/repo" or "golang.org/x/tools".
func IsGoModPath(url string) bool {
	// Git URLs have explicit scheme
	if len(url) > 8 && (url[:8] == "https://" || url[:7] == "http://" || url[:6] == "git://") {
		return false
	}
	// SSH URLs
	if len(url) > 4 && url[:4] == "git@" {
		return false
	}
	// Check for Go module version suffix (e.g., @v1.2.3, @v0.0.0-20210101120000-abcdef123456)
	// This is the most reliable indicator of a Go module path
	if strings.Contains(url, "@v") {
		return true
	}
	// Check for Go module patterns (domain/path without scheme)
	// Common Go module hosts
	goModHosts := []string{
		"golang.org/",
		"google.golang.org/",
		"gopkg.in/",
		"go.uber.org/",
	}
	for _, host := range goModHosts {
		if len(url) >= len(host) && url[:len(host)] == host {
			return true
		}
	}
	return false
}

// BackendConfig holds common configuration for all backends.
type BackendConfig struct {
	// CacheDir is the local directory for caching source data.
	// Git: cloned repos. GoMod: downloaded modules.
	CacheDir string

	// MaxCacheSize is the maximum cache size in bytes.
	// 0 = unlimited.
	MaxCacheSize int64

	// MaxCacheAge is how long cached items live before eviction.
	MaxCacheAgeSecs int64

	// Concurrency limits parallel operations within the backend.
	Concurrency int

	// Backend-specific configuration.
	Extra map[string]string
}

// FetchRequest contains all information needed to fetch a blob.
type FetchRequest struct {
	// ContentID is the blob identifier.
	// Git: blob SHA. GoMod: module@version/path.
	ContentID string

	// SourceKey is used for repo-affinity routing.
	// Git: repo URL. GoMod: module path.
	SourceKey string

	// SourceConfig contains backend-specific parameters.
	// Git: repo_url, branch, display_path
	// GoMod: module_path, version
	SourceConfig map[string]string

	// RequestID for tracing.
	RequestID string

	// Priority: 0 = highest, 10 = lowest.
	Priority int
}

// FetchResult contains the fetched blob data.
type FetchResult struct {
	// Content is the blob data (for non-streaming).
	Content []byte

	// Size is the blob size in bytes.
	Size int64

	// FromCache indicates if this was served from local cache.
	FromCache bool

	// FetchLatencyMs is the remote fetch time (0 if from cache).
	FetchLatencyMs int64
}

// BackendStats holds statistics for a backend.
type BackendStats struct {
	Requests     int64
	Errors       int64
	BytesFetched int64
	CacheHits    int64
	CacheMisses  int64
	CachedItems  int64
	CacheBytes   int64
	AvgLatencyMs float64
}

// BaseBackend provides shared stats tracking and a default FetchBlobStream
// implementation. Embed this in concrete backends to eliminate boilerplate.
type BaseBackend struct {
	stats atomic.Pointer[BackendStats]
}

// InitStats initialises the stats pointer. Call from the backend constructor.
func (b *BaseBackend) InitStats() {
	b.stats.Store(&BackendStats{})
}

// Stats returns a snapshot of the current backend statistics.
func (b *BaseBackend) Stats() BackendStats {
	return *b.stats.Load()
}

// RecordSuccess atomically updates stats after a successful fetch.
func (b *BaseBackend) RecordSuccess(bytesFetched int64, latencyMs int64, fromCache bool, cachedItems int64) {
	for {
		old := b.stats.Load()
		next := &BackendStats{
			Requests:     old.Requests + 1,
			Errors:       old.Errors,
			BytesFetched: old.BytesFetched + bytesFetched,
			CachedItems:  cachedItems,
		}
		if fromCache {
			next.CacheHits = old.CacheHits + 1
		} else {
			next.CacheMisses = old.CacheMisses + 1
		}
		next.AvgLatencyMs = (old.AvgLatencyMs*float64(old.Requests) + float64(latencyMs)) / float64(next.Requests)
		if b.stats.CompareAndSwap(old, next) {
			return
		}
	}
}

// RecordError atomically updates stats after a failed fetch.
func (b *BaseBackend) RecordError() {
	for {
		old := b.stats.Load()
		next := &BackendStats{
			Requests:     old.Requests + 1,
			Errors:       old.Errors + 1,
			BytesFetched: old.BytesFetched,
			CacheHits:    old.CacheHits,
			CacheMisses:  old.CacheMisses + 1,
			CachedItems:  old.CachedItems,
			AvgLatencyMs: old.AvgLatencyMs,
		}
		if b.stats.CompareAndSwap(old, next) {
			return
		}
	}
}

// DefaultFetchBlobStream wraps FetchBlob into a streaming reader.
// Use this for backends whose underlying data is already fully buffered.
func DefaultFetchBlobStream(b Backend, ctx context.Context, req *FetchRequest) (io.ReadCloser, int64, error) {
	result, err := b.FetchBlob(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(result.Content)), result.Size, nil
}

// Registry manages available backends.
type Registry struct {
	backends map[pb.SourceType]Backend
}

// NewRegistry creates a new backend registry.
func NewRegistry() *Registry {
	return &Registry{
		backends: make(map[pb.SourceType]Backend),
	}
}

// Register adds a backend to the registry.
func (r *Registry) Register(backend Backend) {
	r.backends[backend.Type()] = backend
}

// Get returns the backend for a source type.
func (r *Registry) Get(sourceType pb.SourceType) (Backend, bool) {
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
