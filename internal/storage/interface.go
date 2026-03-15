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
	IngestionTypeS3   IngestionType = "s3"   // Future: S3 bucket
	IngestionTypeFile IngestionType = "file" // Future: Local filesystem
)

// FetchType identifies the backend for blob storage/retrieval.
// Used by both the fetcher service (runtime reads) and the router (ingestion).
type FetchType string

const (
	FetchTypeUnknown FetchType = ""      // Unknown/unset
	FetchTypeGit     FetchType = "git"   // Fetch from Git repository
	FetchTypeBlob    FetchType = "blob"  // Fetch from packager blob archive
	FetchTypeS3      FetchType = "s3"    // Fetch from S3 bucket
	FetchTypeLocal   FetchType = "local" // Fetch from local cache
)

// String returns the string representation of the FetchType.
func (ft FetchType) String() string {
	if ft == "" {
		return "unknown"
	}
	return string(ft)
}

// ParseFetchType converts a string to FetchType.
func ParseFetchType(s string) FetchType {
	switch s {
	case "git":
		return FetchTypeGit
	case "blob":
		return FetchTypeBlob
	case "s3":
		return FetchTypeS3
	case "local":
		return FetchTypeLocal
	default:
		return FetchTypeUnknown
	}
}

// StorageType identifies where packager archives are persisted.
type StorageType string

const (
	StorageTypeLocal StorageType = "local" // Local filesystem (default)
	StorageTypeS3    StorageType = "s3"    // Amazon S3 or S3-compatible
	StorageTypeGCS   StorageType = "gcs"   // Google Cloud Storage
)

// CloudStorageConfig holds configuration for storing packager archives
// in a cloud object store (S3 or GCS). Used by BlobBackend when
// StorageType is not "local".
type CloudStorageConfig struct {
	// --- S3 ---

	// S3Region is the AWS region (e.g. "us-east-1"). Required for S3.
	S3Region string
	// S3Bucket is the bucket name for archive storage.
	S3Bucket string
	// S3Prefix is an optional key prefix (e.g. "monofs/archives/").
	S3Prefix string
	// S3Endpoint overrides the default S3 endpoint (for MinIO, Ceph, etc.).
	S3Endpoint string
	// S3AccessKeyID for static credentials. Empty = use default AWS chain.
	S3AccessKeyID string
	// S3SecretAccessKey for static credentials.
	S3SecretAccessKey string
	// S3SessionToken for temporary credentials.
	S3SessionToken string
	// S3UsePathStyle forces path-style addressing (required for most
	// S3-compatible services).
	S3UsePathStyle bool

	// --- GCS ---

	// GCSBucket is the bucket name for archive storage.
	GCSBucket string
	// GCSPrefix is an optional object name prefix.
	GCSPrefix string
	// GCSCredentialsFile is the path to a service account JSON key file.
	// Empty = use Application Default Credentials.
	GCSCredentialsFile string
	// GCSCredentialsJSON is inline service account JSON key content.
	// Takes precedence over GCSCredentialsFile.
	GCSCredentialsJSON []byte
}

// BackendConfig holds common configuration for all fetch backends.
type BackendConfig struct {
	// CacheDir is the local directory for caching source data.
	// Git: cloned repos. Blob: packager archives.
	CacheDir string

	// MaxCacheSize is the maximum cache size in bytes.
	// 0 = unlimited.
	MaxCacheSize int64

	// MaxCacheAge is how long cached items live before eviction.
	MaxCacheAgeSecs int64

	// Concurrency limits parallel operations within the backend.
	Concurrency int

	// EncryptionKey is the 32-byte ChaCha20-Poly1305 key for packager archives.
	// Required for BlobBackend, ignored by other backends.
	EncryptionKey []byte

	// StorageType selects where packager archives are persisted.
	// "local" (default), "s3", or "gcs".
	// When set to "s3" or "gcs", archives are uploaded to the cloud
	// and also cached locally under CacheDir for fast reads.
	StorageType StorageType

	// Cloud holds S3/GCS configuration. Only used when StorageType != "local".
	Cloud CloudStorageConfig

	// Extra holds backend-specific configuration.
	Extra map[string]string
}

// FetchRequest contains all information needed to fetch a blob.
type FetchRequest struct {
	// ContentID is the blob identifier.
	// Git: blob SHA. Blob: blob hash within packager archive.
	ContentID string

	// SourceKey is used for repo-affinity routing.
	// Git: repo URL. Blob: storage ID.
	SourceKey string

	// SourceConfig contains backend-specific parameters.
	// Git: repo_url, branch, display_path
	// Blob: storage_id
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

// BackendStats holds statistics for a fetch backend.
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

// FileMetadata represents file metadata from any source
type FileMetadata struct {
	Path        string
	Size        uint64
	Mode        uint32
	ModTime     int64
	ContentHash string            // Hash of content (blob hash for Git, checksum for S3, etc.)
	Content     []byte            // File content (populated during ingestion for archive building)
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

// FetchBackend handles blob/content retrieval and caching.
// This is the canonical interface for all fetch backends (Git, Blob, S3, etc.).
// Used by the fetcher service (runtime reads) and available to the router.
type FetchBackend interface {
	// Type returns the fetch type identifier.
	Type() FetchType

	// Initialize prepares the backend with configuration.
	// Called once at service startup.
	Initialize(ctx context.Context, config BackendConfig) error

	// FetchBlob retrieves a blob by content ID.
	FetchBlob(ctx context.Context, req *FetchRequest) (*FetchResult, error)

	// FetchBlobStream is like FetchBlob but streams content.
	// Useful for large blobs to avoid memory pressure.
	FetchBlobStream(ctx context.Context, req *FetchRequest) (io.ReadCloser, int64, error)

	// Warmup prepares the backend for fetching from a specific source.
	// For Git: clones/fetches the repo. For Blob: no-op (archives pushed during ingestion).
	Warmup(ctx context.Context, sourceKey string, config map[string]string) error

	// CachedSources returns list of sources this backend has warmed up.
	CachedSources() []string

	// Cleanup releases resources for a specific source.
	Cleanup(ctx context.Context, sourceKey string) error

	// Close shuts down the backend and releases all resources.
	Close() error

	// Stats returns backend-specific statistics.
	Stats() BackendStats
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
