package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/radryc/packager"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type countingWriter struct {
	w       io.Writer
	written int64
}

// StoreArchive writes a packager archive to local disk (cache), optionally
// uploads it to cloud storage, and indexes its contents.
// Called during ingestion when the router pushes pre-built archives.
func (bb *BlobBackend) StoreArchive(storageID string, chunkIndex int, data io.Reader) (int64, int, error) {
	if err := bb.keyGuardCheck(); err != nil {
		return 0, 0, err
	}
	archiveDir := filepath.Join(bb.config.CacheDir, "archives", storageID)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return 0, 0, fmt.Errorf("create archive dir: %w", err)
	}

	archivePath := filepath.Join(archiveDir, fmt.Sprintf("chunk-%04d.pack", chunkIndex))
	tmpPath := archivePath + ".tmp"

	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return 0, 0, fmt.Errorf("create temp archive: %w", err)
	}
	written, err := io.Copy(tmpFile, data)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("write temp archive: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("close temp archive: %w", err)
	}

	// Validate the archive (open + decrypt + list) before it replaces the live
	// file, so corrupt input cannot clobber a working archive.
	hashes, err := bb.archiveHashes(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return written, 0, fmt.Errorf("build archive index: %w", err)
	}

	if err := os.Rename(tmpPath, archivePath); err != nil {
		os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("rename archive: %w", err)
	}

	// Build the sidecar index before the archive is uploaded so other fetchers
	// can locate blobs in S3 without downloading every archive.
	if err := bb.writeArchiveIndex(archivePath, hashes); err != nil {
		os.Remove(archivePath)
		os.Remove(archivePath + archiveIndexSuffix)
		return written, 0, fmt.Errorf("write archive index: %w", err)
	}

	bb.evictArchiveReader(archivePath)

	// Drop any refs from a previous version of this archive before indexing
	// the replacement, so re-ingesting a chunk neither double-counts blobs nor
	// leaves entries that are absent from the new archive.
	bb.mu.Lock()
	removed := bb.removeArchiveFromIndexLocked(archivePath)
	bb.mu.Unlock()

	count, err := bb.indexArchive(archivePath)
	if err != nil {
		os.Remove(archivePath)
		os.Remove(archivePath + archiveIndexSuffix)
		return written, 0, fmt.Errorf("index archive: %w", err)
	}

	bb.mu.Lock()
	bb.storageIDs[storageID] = true
	bb.storageBlobCounts[storageID] += int64(count)
	bb.archivePaths[archivePath] = true
	bb.mu.Unlock()

	// Replicate to cloud. The WAL makes the archive recoverable if the
	// fetcher stops before the upload completes.
	if bb.hasCloudUpload() {
		if err := bb.queueCloudUpload(archivePath); err != nil {
			return written, count, fmt.Errorf("queue cloud upload: %w", err)
		}
	}

	stats := *bb.stats.Load()
	stats.CachedItems += int64(count) - removed
	if stats.CachedItems < 0 {
		stats.CachedItems = 0
	}
	stats.CacheBytes += written
	bb.stats.Store(&stats)

	packagerStoreArchiveBytesTotal.Add(float64(written))
	packagerStoreArchivesTotal.Inc()
	packagerIndexedBlobsGauge.Add(float64(count))

	return written, count, nil
}

// StoreBlob writes a single blob to a "loose" archive.
func (bb *BlobBackend) StoreBlob(blobHash string, content []byte) error {
	if err := bb.keyGuardCheck(); err != nil {
		return err
	}
	bb.mu.RLock()
	_, exists := bb.blobIndex[blobHash]
	bb.mu.RUnlock()
	if exists {
		return nil
	}

	_, err, _ := bb.storeGroup.Do(blobHash, func() (any, error) {
		bb.mu.RLock()
		_, exists := bb.blobIndex[blobHash]
		bb.mu.RUnlock()
		if exists {
			return nil, nil
		}

		archiveDir := filepath.Join(bb.config.CacheDir, "archives", "_loose")
		if err := os.MkdirAll(archiveDir, 0755); err != nil {
			return nil, fmt.Errorf("create loose archive dir: %w", err)
		}

		archivePath := filepath.Join(archiveDir, blobHash+".pack")

		tmpFile, err := os.CreateTemp(archiveDir, blobHash+".*.pack.tmp")
		if err != nil {
			return nil, fmt.Errorf("create loose archive temp file: %w", err)
		}
		tmpPath := tmpFile.Name()
		w := packager.NewArchiveWriter(tmpFile, bb.pipeline)

		if err := w.AddFile(blobHash, content, packager.AddFileOptions{
			Permission: 0644,
			OwnerUID:   0,
			Encrypt:    true,
		}); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return nil, fmt.Errorf("add blob to archive: %w", err)
		}
		if err := w.Close(); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return nil, fmt.Errorf("close archive writer: %w", err)
		}
		if err := tmpFile.Close(); err != nil {
			os.Remove(tmpPath)
			return nil, fmt.Errorf("close loose archive temp file: %w", err)
		}
		if err := os.Rename(tmpPath, archivePath); err != nil {
			os.Remove(tmpPath)
			if errors.Is(err, os.ErrExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("rename loose archive: %w", err)
		}

		if err := bb.writeArchiveIndex(archivePath, []string{blobHash}); err != nil {
			return nil, fmt.Errorf("write loose archive index: %w", err)
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

		if bb.hasCloudUpload() {
			if err := bb.queueCloudUpload(archivePath); err != nil {
				return nil, fmt.Errorf("queue loose archive upload: %w", err)
			}
		}

		stats := *bb.stats.Load()
		stats.CachedItems++
		stats.CacheBytes += int64(len(content))
		bb.stats.Store(&stats)

		packagerStoreBlobsTotal.WithLabelValues("single").Inc()
		packagerIndexedBlobsGauge.Inc()

		return nil, nil
	})
	return err
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
	tmpFile     *os.File
	counter     *countingWriter
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
	if err := w.startNewArchive(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *StoreBlobBatchWriter) startNewArchive() error {
	w.bb.pipeline.ResetEncoder()
	w.curHashes = w.curHashes[:0]
	w.archivePath = filepath.Join(w.archiveDir,
		fmt.Sprintf("batch-%d-%04d.pack", time.Now().UnixNano(), w.archiveSeq))
	w.tmpPath = w.archivePath + ".tmp"
	tmpFile, err := os.Create(w.tmpPath)
	if err != nil {
		return fmt.Errorf("create batch archive: %w", err)
	}
	w.tmpFile = tmpFile
	w.counter = &countingWriter{w: tmpFile}
	w.writer = packager.NewArchiveWriter(w.counter, w.bb.pipeline)
	return nil
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

	if w.writer == nil {
		w.result.Failed++
		return
	}

	if len(w.curHashes) > 0 && w.counter != nil && w.counter.written+int64(len(content)) > maxBatchArchiveBytes {
		if err := w.sealCurrentArchive(); err != nil {
			w.result.Failed++
			return
		}
		w.archiveSeq++
		if err := w.startNewArchive(); err != nil {
			w.result.Failed++
			return
		}
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
		if w.tmpFile != nil {
			if err := w.tmpFile.Close(); err != nil {
				os.Remove(w.tmpPath)
				return fmt.Errorf("close empty batch archive: %w", err)
			}
			os.Remove(w.tmpPath)
		}
		w.tmpFile = nil
		w.counter = nil
		w.writer = nil
		return nil
	}

	if err := w.writer.Close(); err != nil {
		if w.tmpFile != nil {
			w.tmpFile.Close()
		}
		os.Remove(w.tmpPath)
		w.tmpFile = nil
		w.counter = nil
		w.writer = nil
		return fmt.Errorf("close batch archive writer: %w", err)
	}
	if err := w.tmpFile.Close(); err != nil {
		os.Remove(w.tmpPath)
		w.tmpFile = nil
		w.counter = nil
		w.writer = nil
		return fmt.Errorf("close batch archive file: %w", err)
	}
	archiveBytes := int64(0)
	if w.counter != nil {
		archiveBytes = w.counter.written
	}
	w.result.ArchiveBytes += archiveBytes
	if err := os.Rename(w.tmpPath, w.archivePath); err != nil {
		os.Remove(w.tmpPath)
		w.tmpFile = nil
		w.counter = nil
		w.writer = nil
		return fmt.Errorf("rename batch archive: %w", err)
	}

	archivePath := w.archivePath
	if err := w.bb.writeArchiveIndex(archivePath, w.curHashes); err != nil {
		w.tmpFile = nil
		w.counter = nil
		w.writer = nil
		return fmt.Errorf("write batch archive index: %w", err)
	}

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

	// Replicate to cloud. The WAL makes the archive recoverable if the
	// fetcher stops before the upload completes.
	if w.bb.hasCloudUpload() {
		if err := w.bb.queueCloudUpload(archivePath); err != nil {
			w.tmpFile = nil
			w.counter = nil
			w.writer = nil
			return fmt.Errorf("queue batch archive upload: %w", err)
		}
	}

	stats := *w.bb.stats.Load()
	stats.CachedItems += int64(len(w.curHashes))
	stats.CacheBytes += archiveBytes
	w.bb.stats.Store(&stats)

	packagerStoreBlobsTotal.WithLabelValues("batch").Add(float64(len(w.curHashes)))
	packagerIndexedBlobsGauge.Add(float64(len(w.curHashes)))

	w.result.ArchivesCreated++
	w.curHashes = w.curHashes[:0]
	w.tmpFile = nil
	w.counter = nil
	w.writer = nil
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
	if err := bb.keyGuardCheck(); err != nil {
		return nil, err
	}
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
	Failed          int
	ArchivesRemoved int
}

// DeleteBlobs removes the given blob hashes from the in-memory index.
// If compact is true, empty archive files are deleted from disk.
// removeArchiveFromIndexLocked deletes every blob reference owned by
// archivePath and returns how many blobs were removed. Callers must hold bb.mu.
func (bb *BlobBackend) removeArchiveFromIndexLocked(archivePath string) int64 {
	var removed int64
	storageID := filepath.Base(filepath.Dir(archivePath))
	for hash, ref := range bb.blobIndex {
		if ref.archivePath != archivePath {
			continue
		}
		delete(bb.blobIndex, hash)
		removed++
		if storageID != "" {
			bb.storageBlobCounts[storageID]--
		}
	}
	if storageID != "" && bb.storageBlobCounts[storageID] <= 0 {
		delete(bb.storageBlobCounts, storageID)
		delete(bb.storageIDs, storageID)
	}
	delete(bb.archivePaths, archivePath)
	return removed
}

func (bb *BlobBackend) DeleteBlobs(hashes []string, compact bool) *DeleteBlobsResult {
	result := &DeleteBlobsResult{}
	if err := bb.keyGuardCheck(); err != nil {
		result.Failed = len(hashes)
		if bb.logger != nil {
			bb.logger.Warn("DeleteBlobs blocked by pending encryption key", "error", err)
		}
		return result
	}

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
				delete(bb.storageIDs, storageID)
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
			if archiveEntryCount[archivePath] != 0 {
				continue
			}

			// The archive is empty, so there is nothing left to upload. Drop
			// any pending WAL entry before deleting the local files.
			_ = bb.walRemove(archivePath)

			bb.archiveMu.Lock()
			if cached, ok := bb.archiveCache[archivePath]; ok {
				cached.reader.Close()
				cached.store.Close()
				delete(bb.archiveCache, archivePath)
			}
			bb.archiveMu.Unlock()

			// The sidecar must disappear with the archive. Otherwise a
			// read-through lookup can resolve a blob to a dead archive.
			if err := os.Remove(archivePath + archiveIndexSuffix); err != nil && !os.IsNotExist(err) && bb.logger != nil {
				bb.logger.Warn("failed to remove archive sidecar", "archive_path", archivePath, "error", err)
			}
			if err := os.Remove(archivePath); err != nil && !os.IsNotExist(err) {
				if bb.logger != nil {
					bb.logger.Warn("failed to remove archive", "archive_path", archivePath, "error", err)
				}
				continue
			}
			delete(bb.archivePaths, archivePath)
			result.ArchivesRemoved++
			bb.archiveDeletesTotal.Add(1)

			// S3 is authoritative. Remove both the pack and its index so other
			// fetchers cannot resurrect deleted blobs from the cloud copy.
			if bb.isCloudConfigured() {
				delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := bb.deleteCloudArchive(delCtx, archivePath); err != nil && bb.logger != nil {
					bb.logger.Warn("failed to delete archive from cloud", "archive_path", archivePath, "error", err)
				}
				cancel()
			}
			if bb.otel.archiveDeletes != nil {
				bb.otel.archiveDeletes.Add(context.Background(), 1,
					metric.WithAttributes(attribute.String("reason", "compaction")))
			}
		}
	}

	bb.mu.Unlock()

	// Invalidate cloud read-through entries for every deleted blob. Other
	// fetchers are invalidated by the client fan-out; offline fetchers drop
	// stale entries when their S3 prefix index is refreshed.
	bb.cloudIndexMu.Lock()
	for _, hash := range hashes {
		delete(bb.cloudBlobIndex, hash)
	}
	bb.cloudIndexMu.Unlock()
	if result.Deleted > 0 {
		bb.cacheInvalidationsTotal.Add(int64(result.Deleted))
		if bb.otel.invalidations != nil {
			bb.otel.invalidations.Add(context.Background(), int64(result.Deleted))
		}
	}

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

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.written += int64(n)
	return n, err
}
