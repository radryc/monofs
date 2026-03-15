// Package blob implements the packager-archive blob fetch backend.
// Archives are built during ingestion and pushed to fetcher nodes.
// Each archive is encrypted (ChaCha20-Poly1305), compressed (zstd),
// and supports O(1) random-access reads.
package blob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	gcStorage "cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/packager"
	"github.com/radryc/packager/pipeline"
	pkgstorage "github.com/radryc/packager/storage"
)

// maxOpenArchives limits concurrently open ArchiveReader handles.
const maxOpenArchives = 256

// archiveRef maps a blob hash to its location inside a packager archive.
type archiveRef struct {
	archivePath string // filesystem path to .pack file
	entryPath   string // path within the archive (the blob hash or file path)
}

// openArchive wraps a packager.ArchiveReader with LRU tracking.
type openArchive struct {
	reader   *packager.ArchiveReader
	store    pkgstorage.ObjectReader
	lastUsed time.Time
}

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

	// storageIDs tracks which storage IDs have archives on this fetcher.
	storageIDs map[string]bool

	// storageBlobCounts tracks the number of indexed files per storage ID.
	// Protected by mu. Special keys: "_batch" (batch writer), "_loose" (single files).
	storageBlobCounts map[string]int64

	// archivePaths tracks unique .pack files on disk. Protected by mu.
	archivePaths map[string]bool

	stats *storage.AtomicStats

	// Cloud storage clients (nil when StorageType == "local").
	s3Client  *s3.Client
	gcsClient *gcStorage.Client
}

// NewBlobBackend creates a new packager-based blob backend.
func NewBlobBackend() *BlobBackend {
	bb := &BlobBackend{
		blobIndex:         make(map[string]archiveRef),
		archiveCache:      make(map[string]*openArchive),
		storageIDs:        make(map[string]bool),
		storageBlobCounts: make(map[string]int64),
		archivePaths:      make(map[string]bool),
		stats:             storage.NewAtomicStats(),
	}
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
	}

	// Ensure local archive cache directory exists (used for all storage types).
	archiveDir := filepath.Join(config.CacheDir, "archives")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("failed to create archive dir: %w", err)
	}

	// Scan locally-cached archives and build blob index.
	if err := bb.scanArchives(archiveDir); err != nil {
		return fmt.Errorf("failed to scan existing archives: %w", err)
	}

	return nil
}

// SetLogger sets the logger for the blob backend.
func (bb *BlobBackend) SetLogger(logger *slog.Logger) {
	bb.logger = logger
}

// scanArchives discovers all .pack files on disk and indexes their contents.
func (bb *BlobBackend) scanArchives(archiveDir string) error {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var totalFiles int64
	var totalBytes int64

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		storageID := entry.Name()
		storageDir := filepath.Join(archiveDir, storageID)
		bb.storageIDs[storageID] = true

		packs, err := filepath.Glob(filepath.Join(storageDir, "*.pack"))
		if err != nil {
			continue
		}

		for _, packPath := range packs {
			info, err := os.Stat(packPath)
			if err != nil {
				continue
			}
			totalBytes += info.Size()

			count, err := bb.indexArchive(packPath)
			if err != nil {
				continue
			}
			totalFiles += int64(count)
			bb.storageBlobCounts[storageID] += int64(count)
			bb.archivePaths[packPath] = true
		}
	}

	bb.stats.Store(&storage.BackendStats{
		CachedItems: totalFiles,
		CacheBytes:  totalBytes,
	})

	return nil
}

// indexArchive opens an archive and adds all its file entries to the blob index.
// Returns the number of files indexed.
func (bb *BlobBackend) indexArchive(archivePath string) (int, error) {
	store, err := pkgstorage.NewLocalFileReader(archivePath)
	if err != nil {
		return 0, fmt.Errorf("open archive %s: %w", archivePath, err)
	}

	ar, err := packager.OpenArchive(store, bb.pipeline)
	if err != nil {
		store.Close()
		return 0, fmt.Errorf("parse archive %s: %w", archivePath, err)
	}

	files := ar.ListFiles()
	var indexed int
	bb.mu.Lock()
	for _, entry := range files {
		if strings.Contains(entry, "/") || strings.Contains(entry, ".") {
			continue
		}
		if !isHexString(entry) {
			continue
		}
		bb.blobIndex[entry] = archiveRef{
			archivePath: archivePath,
			entryPath:   entry,
		}
		indexed++
	}
	bb.mu.Unlock()

	ar.Close()
	store.Close()

	return indexed, nil
}

// cloudKey returns the S3/GCS object key for a local archive path.
// The key is: <prefix>/<storageID>/<filename.pack>
func (bb *BlobBackend) cloudKey(archivePath string) string {
	archiveDir := filepath.Join(bb.config.CacheDir, "archives")
	rel, err := filepath.Rel(archiveDir, archivePath)
	if err != nil {
		// Shouldn't happen; fall back to basename.
		rel = filepath.Base(archivePath)
	}
	// Normalise to forward slashes for cloud keys.
	rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")

	prefix := bb.config.Cloud.S3Prefix
	if bb.config.StorageType == storage.StorageTypeGCS {
		prefix = bb.config.Cloud.GCSPrefix
	}
	if prefix != "" {
		return strings.TrimRight(prefix, "/") + "/" + rel
	}
	return rel
}

// uploadToCloud uploads archiveData to the configured cloud bucket.
// It is safe to call when StorageType is "local" (no-op).
func (bb *BlobBackend) uploadToCloud(archivePath string, archiveData []byte) {
	switch bb.config.StorageType {
	case storage.StorageTypeS3:
		bb.uploadToS3(archivePath, archiveData)
	case storage.StorageTypeGCS:
		bb.uploadToGCS(archivePath, archiveData)
	}
}

func (bb *BlobBackend) uploadToS3(archivePath string, data []byte) {
	key := bb.cloudKey(archivePath)
	_, err := bb.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bb.config.Cloud.S3Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		if bb.logger != nil {
			bb.logger.Error("failed to upload archive to S3", "key", key, "error", err)
		}
	}
}

func (bb *BlobBackend) uploadToGCS(archivePath string, data []byte) {
	key := bb.cloudKey(archivePath)
	bucket := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket)
	writer := bucket.Object(key).NewWriter(context.Background())
	if _, err := writer.Write(data); err != nil {
		writer.Close()
		if bb.logger != nil {
			bb.logger.Error("failed to upload archive to GCS", "key", key, "error", err)
		}
		return
	}
	if err := writer.Close(); err != nil {
		if bb.logger != nil {
			bb.logger.Error("failed to close GCS writer", "key", key, "error", err)
		}
	}
}

// downloadFromCloud fetches an archive from the cloud and caches it locally.
// Returns the local path on success.
func (bb *BlobBackend) downloadFromCloud(archivePath string) error {
	key := bb.cloudKey(archivePath)

	var data []byte
	var err error

	switch bb.config.StorageType {
	case storage.StorageTypeS3:
		data, err = bb.downloadFromS3(key)
	case storage.StorageTypeGCS:
		data, err = bb.downloadFromGCS(key)
	default:
		return fmt.Errorf("cloud download not available for storage type %q", bb.config.StorageType)
	}
	if err != nil {
		return err
	}

	// Cache locally.
	if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
		return fmt.Errorf("create local cache dir: %w", err)
	}
	tmpPath := archivePath + ".dl.tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write local cache: %w", err)
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename local cache: %w", err)
	}
	return nil
}

func (bb *BlobBackend) downloadFromS3(key string) ([]byte, error) {
	resp, err := bb.s3Client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bb.config.Cloud.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 GetObject %s: %w", key, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (bb *BlobBackend) downloadFromGCS(key string) ([]byte, error) {
	rc, err := bb.gcsClient.Bucket(bb.config.Cloud.GCSBucket).Object(key).NewReader(context.Background())
	if err != nil {
		return nil, fmt.Errorf("gcs NewReader %s: %w", key, err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// isCloudConfigured returns true when archives should be replicated to cloud.
func (bb *BlobBackend) isCloudConfigured() bool {
	return bb.config.StorageType == storage.StorageTypeS3 || bb.config.StorageType == storage.StorageTypeGCS
}

// StoreArchive writes a packager archive to local disk (cache), optionally
// uploads it to cloud storage, and indexes its contents.
// Called during ingestion when the router pushes pre-built archives.
func (bb *BlobBackend) StoreArchive(storageID string, chunkIndex int, data io.Reader) (int64, int, error) {
	archiveDir := filepath.Join(bb.config.CacheDir, "archives", storageID)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return 0, 0, fmt.Errorf("create archive dir: %w", err)
	}

	archivePath := filepath.Join(archiveDir, fmt.Sprintf("chunk-%04d.pack", chunkIndex))
	tmpPath := archivePath + ".tmp"

	// Buffer data in memory so we can write locally and upload to cloud.
	var rawBuf bytes.Buffer
	written, err := io.Copy(&rawBuf, data)
	if err != nil {
		return 0, 0, fmt.Errorf("buffer archive: %w", err)
	}
	archiveData := rawBuf.Bytes()

	// Write to local cache.
	if err := os.WriteFile(tmpPath, archiveData, 0644); err != nil {
		return 0, 0, fmt.Errorf("write temp archive: %w", err)
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("rename archive: %w", err)
	}

	// Upload to cloud (non-blocking log on error, local is authoritative).
	if bb.isCloudConfigured() {
		bb.uploadToCloud(archivePath, archiveData)
	}

	bb.evictArchiveReader(archivePath)

	count, err := bb.indexArchive(archivePath)
	if err != nil {
		return written, 0, fmt.Errorf("index archive: %w", err)
	}

	bb.mu.Lock()
	bb.storageIDs[storageID] = true
	bb.storageBlobCounts[storageID] += int64(count)
	bb.archivePaths[archivePath] = true
	bb.mu.Unlock()

	stats := *bb.stats.Load()
	stats.CachedItems += int64(count)
	stats.CacheBytes += written
	bb.stats.Store(&stats)

	return written, count, nil
}

// StoreBlob writes a single blob to a "loose" archive.
func (bb *BlobBackend) StoreBlob(blobHash string, content []byte) error {
	bb.mu.RLock()
	_, exists := bb.blobIndex[blobHash]
	bb.mu.RUnlock()
	if exists {
		return nil
	}

	archiveDir := filepath.Join(bb.config.CacheDir, "archives", "_loose")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("create loose archive dir: %w", err)
	}

	archivePath := filepath.Join(archiveDir, blobHash+".pack")
	tmpPath := archivePath + ".tmp"

	var buf bytes.Buffer
	w := packager.NewArchiveWriter(&buf, bb.pipeline)

	if err := w.AddFile(blobHash, content, packager.AddFileOptions{
		Permission: 0644,
		OwnerUID:   0,
		Encrypt:    true,
	}); err != nil {
		return fmt.Errorf("add blob to archive: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close archive writer: %w", err)
	}

	if err := os.WriteFile(tmpPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("write loose archive: %w", err)
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename loose archive: %w", err)
	}

	// Upload to cloud.
	if bb.isCloudConfigured() {
		bb.uploadToCloud(archivePath, buf.Bytes())
	}

	bb.mu.Lock()
	bb.blobIndex[blobHash] = archiveRef{
		archivePath: archivePath,
		entryPath:   blobHash,
	}
	bb.storageIDs["_loose"] = true
	bb.storageBlobCounts["_loose"]++
	bb.archivePaths[archivePath] = true
	bb.mu.Unlock()

	stats := *bb.stats.Load()
	stats.CachedItems++
	stats.CacheBytes += int64(len(content))
	bb.stats.Store(&stats)

	return nil
}

// StoreBlobBatchResult holds the outcome of a batched blob write.
type StoreBlobBatchResult struct {
	Stored          int
	Skipped         int
	Failed          int
	ArchiveBytes    int64
	ArchivesCreated int
}

// maxBatchArchiveBytes is the target split size for streamed batch archives.
const maxBatchArchiveBytes = 512 * 1024 * 1024 // 512 MB

// StoreBlobBatchWriter accumulates blob entries from a stream and packs
// them into archive(s), splitting at ~512 MB.
type StoreBlobBatchWriter struct {
	bb     *BlobBackend
	result StoreBlobBatchResult

	archiveDir  string
	buf         bytes.Buffer
	writer      *packager.ArchiveWriter
	archivePath string
	tmpPath     string
	curHashes   []string
	archiveSeq  int
}

// NewStoreBlobBatchWriter creates a writer that packs streamed blobs into archives.
func (bb *BlobBackend) NewStoreBlobBatchWriter() (*StoreBlobBatchWriter, error) {
	archiveDir := filepath.Join(bb.config.CacheDir, "archives", "_batch")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return nil, fmt.Errorf("create batch archive dir: %w", err)
	}
	w := &StoreBlobBatchWriter{
		bb:         bb,
		archiveDir: archiveDir,
	}
	w.startNewArchive()
	return w, nil
}

func (w *StoreBlobBatchWriter) startNewArchive() {
	w.buf.Reset()
	w.writer = packager.NewArchiveWriter(&w.buf, w.bb.pipeline)
	w.curHashes = w.curHashes[:0]
	w.archivePath = filepath.Join(w.archiveDir,
		fmt.Sprintf("batch-%d-%04d.pack", time.Now().UnixNano(), w.archiveSeq))
	w.tmpPath = w.archivePath + ".tmp"
}

// AddBlob adds one blob entry to the current archive.
func (w *StoreBlobBatchWriter) AddBlob(blobHash string, content []byte) {
	if blobHash == "" || len(content) == 0 {
		w.result.Failed++
		return
	}

	w.bb.mu.RLock()
	_, exists := w.bb.blobIndex[blobHash]
	w.bb.mu.RUnlock()
	if exists {
		w.result.Skipped++
		return
	}

	if len(w.curHashes) > 0 && int64(w.buf.Len())+int64(len(content)) > maxBatchArchiveBytes {
		if err := w.sealCurrentArchive(); err != nil {
			w.result.Failed++
			return
		}
		w.archiveSeq++
		w.startNewArchive()
	}

	if err := w.writer.AddFile(blobHash, content, packager.AddFileOptions{
		Permission: 0644,
		OwnerUID:   0,
		Encrypt:    true,
	}); err != nil {
		w.result.Failed++
		return
	}

	w.curHashes = append(w.curHashes, blobHash)
	w.result.Stored++
}

func (w *StoreBlobBatchWriter) sealCurrentArchive() error {
	if len(w.curHashes) == 0 {
		return nil
	}

	if err := w.writer.Close(); err != nil {
		return fmt.Errorf("close batch archive writer: %w", err)
	}

	archiveData := w.buf.Bytes()
	w.result.ArchiveBytes += int64(len(archiveData))

	if err := os.WriteFile(w.tmpPath, archiveData, 0644); err != nil {
		return fmt.Errorf("write batch archive: %w", err)
	}
	if err := os.Rename(w.tmpPath, w.archivePath); err != nil {
		os.Remove(w.tmpPath)
		return fmt.Errorf("rename batch archive: %w", err)
	}

	// Upload to cloud.
	if w.bb.isCloudConfigured() {
		w.bb.uploadToCloud(w.archivePath, archiveData)
	}

	archivePath := w.archivePath
	w.bb.mu.Lock()
	for _, hash := range w.curHashes {
		w.bb.blobIndex[hash] = archiveRef{
			archivePath: archivePath,
			entryPath:   hash,
		}
	}
	w.bb.storageIDs["_batch"] = true
	w.bb.storageBlobCounts["_batch"] += int64(len(w.curHashes))
	w.bb.archivePaths[archivePath] = true
	w.bb.mu.Unlock()

	stats := *w.bb.stats.Load()
	stats.CachedItems += int64(len(w.curHashes))
	stats.CacheBytes += int64(len(archiveData))
	w.bb.stats.Store(&stats)

	w.result.ArchivesCreated++
	return nil
}

// Finish seals the last archive and returns the final result.
func (w *StoreBlobBatchWriter) Finish() (*StoreBlobBatchResult, error) {
	if err := w.sealCurrentArchive(); err != nil {
		return &w.result, err
	}
	return &w.result, nil
}

// StoreBlobBatch packs all blobs in the map into archive(s).
func (bb *BlobBackend) StoreBlobBatch(blobs map[string][]byte) (*StoreBlobBatchResult, error) {
	w, err := bb.NewStoreBlobBatchWriter()
	if err != nil {
		return nil, err
	}

	hashes := make([]string, 0, len(blobs))
	for h := range blobs {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)

	for _, hash := range hashes {
		w.AddBlob(hash, blobs[hash])
	}

	return w.Finish()
}

// DeleteBlobsResult summarises a blob deletion operation.
type DeleteBlobsResult struct {
	Deleted         int
	NotFound        int
	ArchivesRemoved int
}

// DeleteBlobs removes the given blob hashes from the in-memory index.
// If compact is true, empty archive files are deleted from disk.
func (bb *BlobBackend) DeleteBlobs(hashes []string, compact bool) *DeleteBlobsResult {
	result := &DeleteBlobsResult{}

	bb.mu.Lock()

	affectedArchives := make(map[string]bool)

	for _, hash := range hashes {
		ref, exists := bb.blobIndex[hash]
		if !exists {
			result.NotFound++
			continue
		}
		affectedArchives[ref.archivePath] = true
		delete(bb.blobIndex, hash)
		result.Deleted++
		if storageID := filepath.Base(filepath.Dir(ref.archivePath)); storageID != "" {
			bb.storageBlobCounts[storageID]--
			if bb.storageBlobCounts[storageID] <= 0 {
				delete(bb.storageBlobCounts, storageID)
			}
		}
	}

	if compact && len(affectedArchives) > 0 {
		archiveEntryCount := make(map[string]int)
		for _, ref := range bb.blobIndex {
			if affectedArchives[ref.archivePath] {
				archiveEntryCount[ref.archivePath]++
			}
		}

		for archivePath := range affectedArchives {
			if archiveEntryCount[archivePath] == 0 {
				bb.archiveMu.Lock()
				if cached, ok := bb.archiveCache[archivePath]; ok {
					cached.reader.Close()
					cached.store.Close()
					delete(bb.archiveCache, archivePath)
				}
				bb.archiveMu.Unlock()

				os.Remove(archivePath)
				delete(bb.archivePaths, archivePath)
				result.ArchivesRemoved++
			}
		}
	}

	bb.mu.Unlock()

	if result.Deleted > 0 {
		stats := *bb.stats.Load()
		stats.CachedItems -= int64(result.Deleted)
		if stats.CachedItems < 0 {
			stats.CachedItems = 0
		}
		bb.stats.Store(&stats)
	}

	return result
}

// getArchiveReader returns a cached or freshly opened ArchiveReader.
// If the local file is missing but cloud storage is configured, the archive
// is downloaded from the cloud and cached locally before opening.
func (bb *BlobBackend) getArchiveReader(archivePath string) (*packager.ArchiveReader, error) {
	bb.archiveMu.Lock()
	defer bb.archiveMu.Unlock()

	if cached, ok := bb.archiveCache[archivePath]; ok {
		cached.lastUsed = time.Now()
		return cached.reader, nil
	}

	if len(bb.archiveCache) >= maxOpenArchives {
		bb.evictOldestLocked()
	}

	// Try opening the local file first.
	store, err := pkgstorage.NewLocalFileReader(archivePath)
	if err != nil && bb.isCloudConfigured() {
		// Local cache miss — download from cloud and retry.
		if dlErr := bb.downloadFromCloud(archivePath); dlErr != nil {
			return nil, fmt.Errorf("open archive file (cloud fallback failed: %v): %w", dlErr, err)
		}
		store, err = pkgstorage.NewLocalFileReader(archivePath)
	}
	if err != nil {
		return nil, fmt.Errorf("open archive file: %w", err)
	}

	ar, err := packager.OpenArchive(store, bb.pipeline)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("parse archive: %w", err)
	}

	bb.archiveCache[archivePath] = &openArchive{
		reader:   ar,
		store:    store,
		lastUsed: time.Now(),
	}

	return ar, nil
}

func (bb *BlobBackend) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time

	for key, cached := range bb.archiveCache {
		if oldestKey == "" || cached.lastUsed.Before(oldestTime) {
			oldestKey = key
			oldestTime = cached.lastUsed
		}
	}

	if oldestKey != "" {
		cached := bb.archiveCache[oldestKey]
		cached.reader.Close()
		cached.store.Close()
		delete(bb.archiveCache, oldestKey)
	}
}

func (bb *BlobBackend) evictArchiveReader(archivePath string) {
	bb.archiveMu.Lock()
	defer bb.archiveMu.Unlock()

	if cached, ok := bb.archiveCache[archivePath]; ok {
		cached.reader.Close()
		cached.store.Close()
		delete(bb.archiveCache, archivePath)
	}
}

// FetchBlob reads a blob from a packager archive.
func (bb *BlobBackend) FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error) {
	start := time.Now()
	stats := bb.stats.Load()
	newStats := *stats
	newStats.Requests++

	bb.mu.RLock()
	ref, ok := bb.blobIndex[req.ContentID]
	bb.mu.RUnlock()

	if !ok {
		newStats.CacheMisses++
		newStats.Errors++
		bb.stats.Store(&newStats)
		return nil, fmt.Errorf("blob not found: %s", req.ContentID)
	}

	ar, err := bb.getArchiveReader(ref.archivePath)
	if err != nil {
		newStats.Errors++
		bb.stats.Store(&newStats)
		return nil, fmt.Errorf("open archive for blob %s: %w", req.ContentID, err)
	}

	data, _, err := ar.GetFile(ref.entryPath)
	if err != nil {
		newStats.Errors++
		bb.stats.Store(&newStats)
		return nil, fmt.Errorf("read blob %s from archive: %w", req.ContentID, err)
	}

	newStats.CacheHits++
	newStats.BytesFetched += int64(len(data))
	bb.stats.Store(&newStats)

	return &storage.FetchResult{
		Content:        data,
		Size:           int64(len(data)),
		FromCache:      true,
		FetchLatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// FetchBlobStream returns a reader for the blob content.
func (bb *BlobBackend) FetchBlobStream(ctx context.Context, req *storage.FetchRequest) (io.ReadCloser, int64, error) {
	result, err := bb.FetchBlob(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(result.Content)), result.Size, nil
}

// Warmup is a no-op for blob backend (archives are pushed during ingestion).
func (bb *BlobBackend) Warmup(ctx context.Context, sourceKey string, config map[string]string) error {
	return nil
}

// CachedSources returns the storage IDs that have archives on this fetcher.
func (bb *BlobBackend) CachedSources() []string {
	bb.mu.RLock()
	defer bb.mu.RUnlock()

	sources := make([]string, 0, len(bb.storageIDs))
	for id := range bb.storageIDs {
		sources = append(sources, id)
	}
	sort.Strings(sources)
	return sources
}

// Cleanup removes archives for a specific storage ID.
func (bb *BlobBackend) Cleanup(ctx context.Context, sourceKey string) error {
	archiveDir := filepath.Join(bb.config.CacheDir, "archives", sourceKey)

	bb.archiveMu.Lock()
	for path, cached := range bb.archiveCache {
		if strings.HasPrefix(path, archiveDir) {
			cached.reader.Close()
			cached.store.Close()
			delete(bb.archiveCache, path)
		}
	}
	bb.archiveMu.Unlock()

	bb.mu.Lock()
	for hash, ref := range bb.blobIndex {
		if strings.HasPrefix(ref.archivePath, archiveDir) {
			delete(bb.blobIndex, hash)
		}
	}
	delete(bb.storageIDs, sourceKey)
	bb.mu.Unlock()

	os.RemoveAll(archiveDir)

	return nil
}

// Close shuts down the backend, closing all open archive readers and cloud clients.
func (bb *BlobBackend) Close() error {
	bb.archiveMu.Lock()
	defer bb.archiveMu.Unlock()

	for _, cached := range bb.archiveCache {
		cached.reader.Close()
		cached.store.Close()
	}
	bb.archiveCache = nil

	if bb.gcsClient != nil {
		bb.gcsClient.Close()
		bb.gcsClient = nil
	}
	// S3 client has no Close method.

	return nil
}

// Stats returns backend statistics with live archive (pack) count.
func (bb *BlobBackend) Stats() storage.BackendStats {
	bb.mu.RLock()
	packCount := int64(len(bb.archivePaths))
	bb.mu.RUnlock()
	s := *bb.stats.Load()
	s.CachedItems = packCount
	return s
}

// StorageStats returns a snapshot of the file count per storage ID.
// Special keys: "_batch" (batch-writer archives) and "_loose" (single-file archives).
func (bb *BlobBackend) StorageStats() map[string]int64 {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	result := make(map[string]int64, len(bb.storageBlobCounts))
	for k, v := range bb.storageBlobCounts {
		result[k] = v
	}
	return result
}

// HasBlob checks if a blob exists in the index.
func (bb *BlobBackend) HasBlob(blobHash string) bool {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	_, ok := bb.blobIndex[blobHash]
	return ok
}

// ArchiveCount returns the number of storage IDs with archives.
func (bb *BlobBackend) ArchiveCount() int {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return len(bb.storageIDs)
}

func isHexString(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
