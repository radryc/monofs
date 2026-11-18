// Package blob implements the packager-archive blob fetch backend.
// Archives are built during ingestion and pushed to fetcher nodes.
// Each archive is encrypted (ChaCha20-Poly1305), compressed (zstd),
// and supports O(1) random-access reads.
package blob

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/packager/pipeline"
	"golang.org/x/sync/singleflight"

	gcStorage "cloud.google.com/go/storage"
	pkgstorage "github.com/radryc/packager/storage"
)

const (
	// maxOpenArchives limits concurrently open ArchiveReader handles.
	maxOpenArchives = 256

	// defaultCloudUploadQueueSize is the minimum number of archive uploads that
	// can be buffered locally before writers start applying backpressure.
	defaultCloudUploadQueueSize = 32

	// archiveIndexSuffix is the sidecar file next to a .pack archive. It lists
	// the blob hashes contained in the archive so another fetcher can locate a
	// blob in S3 without downloading every archive.
	archiveIndexSuffix = ".idx"

	// cloudIndexTTL bounds how long a fetcher trusts its S3 index snapshot.
	cloudIndexTTL = 5 * time.Minute
)

// BlobBackend stores and serves blobs using packager archives.
type BlobBackend struct {
	config   storage.BackendConfig
	pipeline *pipeline.Pipeline
	logger   *slog.Logger

	// blobIndex maps blobHash → archiveRef for O(1) lookup.
	mu        sync.RWMutex
	blobIndex map[string]archiveRef

	// archiveCache holds open ArchiveReader handles keyed by archive path.
	archiveMu    sync.Mutex
	archiveCache map[string]*openArchive

	// blobCache caches full small blobs to avoid repeated archive reads for
	// hot files. Content-addressed blobs are immutable.
	blobCacheMu        sync.Mutex
	blobCache          map[string][]byte
	blobCacheOrder     []string
	blobCacheBytes     int64
	blobCacheMaxBytes  int64
	blobCacheMaxObject int64

	// storageIDs tracks which storage IDs have archives on this fetcher.
	storageIDs map[string]bool

	// storageBlobCounts tracks the number of indexed files per storage ID.
	// Protected by mu. Special keys: "_batch" (batch writer), "_loose" (single files).
	storageBlobCounts map[string]int64

	// archivePaths tracks unique .pack files on disk. Protected by mu.
	archivePaths map[string]bool

	stats *storage.AtomicStats

	storeGroup singleflight.Group

	// Cloud storage clients (nil when StorageType == "local").
	s3Client  *s3.Client
	gcsClient *gcStorage.Client

	openCloudReader   func(key string) (io.ReadCloser, error)
	uploadArchiveFunc func(ctx context.Context, archivePath string) error
	headCloudObject   func(key string) (cloudObjectInfo, bool)
	listCloudObjects  func(ctx context.Context, prefix string, max int) ([]storage.StorageObject, error)
	cloudUploadQueue  chan cloudArchiveUploadJob
	cloudUploadWG     sync.WaitGroup
	// cloudUploadMu guards cloudUploadQueue against Close closing the channel
	// while a producer is still sending on it.
	cloudUploadMu sync.RWMutex
	closed        atomic.Bool

	// Upload WAL. Every archive is recorded here before it is queued for S3;
	// pending entries are replayed on startup so S3 remains the source of
	// truth across fetcher restarts or crashes.
	walMu      sync.Mutex
	walPath    string
	walPending map[string]struct{}

	// Cloud blob index: blob hash -> S3 archive object key. It is built
	// lazily from the .idx sidecars and lets any fetcher serve a cache miss
	// by downloading the owning archive from S3.
	cloudIndexMu       sync.Mutex
	cloudBlobIndex     map[string]string
	cloudIndexLoaded   bool
	cloudIndexAt       time.Time
	cloudIndexPrefixes map[string]time.Time

	otel blobOTelMetrics

	walRecordedTotal        atomic.Int64
	walReplayedTotal        atomic.Int64
	walUploadFailuresTotal  atomic.Int64
	cloudIndexLoadsTotal    atomic.Int64
	cloudLookupHits         atomic.Int64
	cloudLookupMisses       atomic.Int64
	cloudLookupErrors       atomic.Int64
	cloudDownloadsTotal     atomic.Int64
	cloudDownloadBytes      atomic.Int64
	archiveDeletesTotal     atomic.Int64
	cacheInvalidationsTotal atomic.Int64

	// Blob layout / compaction / batching telemetry. These are surfaced on the
	// fetcher UI so operators can see whether reads use the deterministic
	// content-addressed path or the legacy sidecar index.
	layoutMode                   string
	v2Objects                    v2ObjectStore
	v2Cache                      *v2Cache
	v2PackSeq                    atomic.Uint64
	directLookupHits             atomic.Int64
	directLookupMisses           atomic.Int64
	v2Manifests                  atomic.Int64
	v2Packs                      atomic.Int64
	batchPendingBytes            atomic.Int64
	batchOpenShards              atomic.Int64
	compactorRunsTotal           atomic.Int64
	compactorShardsTotal         atomic.Int64
	compactorPlansTotal          atomic.Int64
	compactorPacksRetiredTotal   atomic.Int64
	compactorPacksDeletedTotal   atomic.Int64
	compactorBytesRewrittenTotal atomic.Int64
	compactorLastRunUnix         atomic.Int64
	compactorLastError           atomic.Value // string

	cloudStores    atomic.Int64
	cloudRetrieves atomic.Int64

	storageHealthy atomic.Bool
	lastStorageErr atomic.Value // string

	// Background cloud-backup integrity scanner.
	scannerStop chan struct{}
	scannerWG   sync.WaitGroup

	// Encryption-key guard. The accepted key fingerprint is stored durably so
	// that a secret rotation cannot silently change the key and make existing
	// archives unreadable.
	keyGuardMu             sync.RWMutex
	keyGuardPending        atomic.Bool
	keyGuardFirstAuto      atomic.Bool
	keyGuardConfirmed      atomic.Bool
	acceptedKeyFingerprint string
	currentKeyFingerprint  string
}

// BlobBackend implements storage.FetchBackend and storage.ObjectLister.
var _ storage.FetchBackend = (*BlobBackend)(nil)

// NewBlobBackend creates a new packager-based blob backend.
func NewBlobBackend() *BlobBackend {
	bb := &BlobBackend{
		blobIndex:          make(map[string]archiveRef),
		archiveCache:       make(map[string]*openArchive),
		storageIDs:         make(map[string]bool),
		storageBlobCounts:  make(map[string]int64),
		archivePaths:       make(map[string]bool),
		walPending:         make(map[string]struct{}),
		cloudBlobIndex:     make(map[string]string),
		cloudIndexPrefixes: make(map[string]time.Time),
		blobCache:          make(map[string][]byte),
		v2Cache:            newV2Cache(),
		blobCacheMaxBytes:  int64Env("MONOFS_FETCHER_BLOB_CACHE_BYTES", defaultBlobCacheBytes),
		blobCacheMaxObject: int64Env("MONOFS_FETCHER_BLOB_CACHE_MAX_OBJECT", defaultBlobCacheMaxObject),
		stats:              storage.NewAtomicStats(),
	}
	bb.storageHealthy.Store(true)
	bb.lastStorageErr.Store("")
	bb.compactorLastError.Store("")
	return bb
}

func (bb *BlobBackend) Type() storage.FetchType {
	return storage.FetchTypeBlob
}

func (bb *BlobBackend) Initialize(ctx context.Context, config storage.BackendConfig) error {
	bb.config = config

	if len(config.EncryptionKey) != 32 {
		return fmt.Errorf("blob backend requires a 32-byte encryption key, got %d bytes", len(config.EncryptionKey))
	}

	p, err := pipeline.NewPipeline(config.EncryptionKey)
	if err != nil {
		return fmt.Errorf("failed to create packager pipeline: %w", err)
	}
	bb.pipeline = p
	bb.layoutMode = resolveBlobLayoutMode()
	bb.initOTelMetrics()

	// Initialize cloud storage client when configured.
	switch config.StorageType {
	case storage.StorageTypeS3:
		c := config.Cloud
		if c.S3Bucket == "" {
			return fmt.Errorf("blob backend: S3 bucket is required when storage_type=s3")
		}
		s3Cfg := pkgstorage.S3Config{
			Region:          c.S3Region,
			Endpoint:        c.S3Endpoint,
			AccessKeyID:     c.S3AccessKeyID,
			SecretAccessKey: c.S3SecretAccessKey,
			SessionToken:    c.S3SessionToken,
			UsePathStyle:    c.S3UsePathStyle,
		}
		client, err := pkgstorage.NewS3Client(ctx, s3Cfg)
		if err != nil {
			return fmt.Errorf("blob backend: create S3 client: %w", err)
		}
		bb.s3Client = client
		bb.gcsClient = nil
		bb.v2Objects = &s3V2ObjectStore{bb: bb}
		bb.openCloudReader = bb.openS3Reader
		bb.uploadArchiveFunc = bb.uploadArchiveToS3
		bb.headCloudObject = bb.headS3Object
		bb.listCloudObjects = func(ctx context.Context, prefix string, max int) ([]storage.StorageObject, error) {
			return bb.listS3ObjectsPrefix(ctx, prefix, max)
		}

	case storage.StorageTypeGCS:
		c := config.Cloud
		if c.GCSBucket == "" {
			return fmt.Errorf("blob backend: GCS bucket is required when storage_type=gcs")
		}
		gcsCfg := pkgstorage.GCSConfig{
			CredentialsFile: c.GCSCredentialsFile,
			CredentialsJSON: c.GCSCredentialsJSON,
		}
		client, err := pkgstorage.NewGCSClient(ctx, gcsCfg)
		if err != nil {
			return fmt.Errorf("blob backend: create GCS client: %w", err)
		}
		bb.gcsClient = client
		bb.s3Client = nil
		bb.openCloudReader = bb.openGCSReader
		bb.uploadArchiveFunc = bb.uploadArchiveToGCS
		bb.headCloudObject = bb.headGCSObject
		bb.listCloudObjects = bb.listGCSObjectsPrefix

	default:
		bb.s3Client = nil
		bb.gcsClient = nil
		bb.openCloudReader = nil
		bb.uploadArchiveFunc = nil
		bb.headCloudObject = nil
		bb.listCloudObjects = nil
	}

	// Ensure local archive cache directory exists (used for all storage types).
	archiveDir := filepath.Join(config.CacheDir, "archives")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("failed to create archive dir: %w", err)
	}

	// Restore the upload WAL before scanning so pending S3 uploads are not
	// lost if the process restarted between archive creation and upload.
	if err := bb.initUploadWAL(archiveDir); err != nil {
		return fmt.Errorf("initialize upload WAL: %w", err)
	}

	// Scan locally-cached archives and build blob index.
	if err := bb.scanArchives(archiveDir); err != nil {
		return fmt.Errorf("failed to scan existing archives: %w", err)
	}

	bb.startCloudUploadWorkers()
	bb.replayUploadWAL(ctx)
	bb.startCloudBackupScanner()

	// Verify the encryption key against the durably-stored accepted fingerprint.
	// A mismatch means the secret was rotated; we require explicit confirmation
	// before serving blobs so existing archives are not silently lost.
	if err := bb.verifyEncryptionKey(); err != nil {
		return fmt.Errorf("encryption key verification failed: %w", err)
	}

	// Backfill sidecars for archives created before the sidecar format
	// existed. Run it in the background so startup is not blocked. Skip the
	// goroutine entirely when cloud upload is not configured; callers may
	// enable upload after Initialize in tests.
	if bb.hasCloudUpload() {
		go bb.backfillCloudArchiveIndexes(context.Background())
	}

	return nil
}

// SetLogger sets the logger for the blob backend.
func (bb *BlobBackend) SetLogger(logger *slog.Logger) {
	bb.logger = logger
}

func (bb *BlobBackend) hasCloudUpload() bool {
	return bb.uploadArchiveFunc != nil
}

func (bb *BlobBackend) hasCloudDownload() bool {
	return bb.openCloudReader != nil
}

// isCloudConfigured returns true when archives should be replicated to cloud.
func (bb *BlobBackend) isCloudConfigured() bool {
	return bb.hasCloudUpload() || bb.hasCloudDownload()
}
