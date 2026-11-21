package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/radryc/monofs/internal/storage"
	"github.com/radryc/packager"
	"github.com/radryc/packager/pipeline"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	pkgstorage "github.com/radryc/packager/storage"
)

// FetchBlob reads a blob from a packager archive.
func (bb *BlobBackend) FetchBlob(ctx context.Context, req *storage.FetchRequest) (*storage.FetchResult, error) {
	if err := bb.keyGuardCheck(); err != nil {
		return nil, err
	}
	start := time.Now()
	stats := bb.stats.Load()
	newStats := *stats
	newStats.Requests++

	if cached, ok := bb.cachedBlob(req.ContentID); ok {
		data := applyFetchRange(cached, req.SourceConfig)
		newStats.CacheHits++
		newStats.BytesFetched += int64(len(data))
		bb.stats.Store(&newStats)
		storageType := string(bb.config.StorageType)
		if storageType == "" {
			storageType = "local"
		}
		packagerFetchBlobTotal.WithLabelValues(storageType).Inc()
		packagerFetchBytesTotal.WithLabelValues(storageType).Add(float64(len(data)))
		return &storage.FetchResult{
			Content:        data,
			Size:           int64(len(data)),
			FromCache:      true,
			FetchLatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	bb.mu.RLock()
	ref, ok := bb.blobIndex[req.ContentID]
	bb.mu.RUnlock()

	if !ok {
		storageID := strings.TrimSpace(req.SourceConfig["storage_id"])
		if storageID == "" {
			storageID = strings.TrimSpace(req.SourceKey)
		}

		recoveredRef, recovered, localErr := bb.findBlobOnDisk(req.ContentID)
		if localErr != nil && bb.logger != nil {
			bb.logger.Warn("blob miss rescan failed", "content_id", req.ContentID, "error", localErr)
		}
		if recovered {
			bb.mu.Lock()
			if existing, exists := bb.blobIndex[req.ContentID]; exists {
				ref = existing
			} else {
				bb.blobIndex[req.ContentID] = recoveredRef
				bb.archivePaths[recoveredRef.archivePath] = true
				if storageID := filepath.Base(filepath.Dir(recoveredRef.archivePath)); storageID != "" {
					bb.storageIDs[storageID] = true
				}
				ref = recoveredRef
			}
			bb.mu.Unlock()
			ok = true
			if bb.logger != nil {
				bb.logger.Info("recovered blob from disk", "content_id", req.ContentID, "archive_path", ref.archivePath)
			}
		}

		// Local cache miss: S3 is the source of truth. Use the sidecar index
		// to find and download the archive that owns this blob.
		if !ok && bb.hasCloudDownload() {
			cloudRef, cloudOK, cloudErr := bb.findBlobInCloud(ctx, storageID, req.ContentID)
			if cloudErr != nil && bb.logger != nil {
				bb.logger.Warn("blob miss cloud lookup failed", "content_id", req.ContentID, "error", cloudErr)
			}
			if cloudOK {
				ref = cloudRef
				ok = true
				if bb.logger != nil {
					bb.logger.Info("recovered blob from cloud", "content_id", req.ContentID, "archive_path", ref.archivePath)
				}
			}
		}

		if !ok && localErr != nil {
			newStats.CacheMisses++
			newStats.Errors++
			bb.stats.Store(&newStats)
			packagerFetchErrorsTotal.WithLabelValues(string(bb.config.StorageType)).Inc()
			if errors.Is(localErr, pipeline.ErrDecryption) || errors.Is(localErr, pipeline.ErrDecompression) {
				return nil, fmt.Errorf("blob %s: archives exist but cannot be decrypted (wrong encryption key?): %w", req.ContentID, localErr)
			}
			return nil, fmt.Errorf("blob %s: archives exist but failed to open: %w", req.ContentID, localErr)
		}
	}

	if !ok {
		newStats.CacheMisses++
		newStats.Errors++
		bb.stats.Store(&newStats)
		packagerFetchErrorsTotal.WithLabelValues(string(bb.config.StorageType)).Inc()
		return nil, fmt.Errorf("blob not found: %s", req.ContentID)
	}

	ar, err := bb.getArchiveReader(ref.archivePath)
	if err != nil {
		newStats.Errors++
		bb.stats.Store(&newStats)
		packagerFetchErrorsTotal.WithLabelValues(string(bb.config.StorageType)).Inc()
		return nil, fmt.Errorf("open archive for blob %s: %w", req.ContentID, err)
	}

	data, _, err := ar.GetFile(ref.entryPath)
	if err != nil {
		newStats.Errors++
		bb.stats.Store(&newStats)
		packagerFetchErrorsTotal.WithLabelValues(string(bb.config.StorageType)).Inc()
		return nil, fmt.Errorf("read/decrypt blob %s from archive: %w", req.ContentID, err)
	}

	// Cache the full blob before slicing so repeated range reads share it.
	bb.putBlob(req.ContentID, data)
	data = applyFetchRange(data, req.SourceConfig)

	newStats.CacheHits++
	newStats.BytesFetched += int64(len(data))
	bb.stats.Store(&newStats)

	storageType := string(bb.config.StorageType)
	if storageType == "" {
		storageType = "local"
	}
	packagerFetchBlobTotal.WithLabelValues(storageType).Inc()
	packagerFetchBytesTotal.WithLabelValues(storageType).Add(float64(len(data)))

	return &storage.FetchResult{
		Content:        data,
		Size:           int64(len(data)),
		FromCache:      true,
		FetchLatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// findBlobInCloud locates a blob through a deterministic content-addressed
// probe first, then the S3 sidecar index as a fallback. Loose archives are
// named "_loose/<content-id>.pack", so the common case is a single HEAD and
// never lists the bucket or scans sidecars.
func (bb *BlobBackend) findBlobInCloud(ctx context.Context, storageID, blobHash string) (archiveRef, bool, error) {
	if !bb.hasCloudDownload() {
		return archiveRef{}, false, nil
	}

	if bb.v2ReadEnabled() {
		if ref, ok, err := bb.v2FindBlob(ctx, blobHash); err == nil && ok {
			bb.cloudLookupHits.Add(1)
			if bb.otel.lookups != nil {
				bb.otel.lookups.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "hit")))
			}
			return ref, true, nil
		}
	}

	if archivePath, ok := bb.directLooseArchivePath(blobHash); ok {
		bb.directLookupHits.Add(1)
		bb.cloudLookupHits.Add(1)
		if bb.otel.lookups != nil {
			bb.otel.lookups.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "hit")))
		}
		return bb.loadCloudArchive(blobHash, archivePath)
	}
	bb.directLookupMisses.Add(1)

	if err := bb.ensureCloudBlobIndex(ctx, storageID); err != nil {
		bb.cloudLookupErrors.Add(1)
		if bb.otel.lookups != nil {
			bb.otel.lookups.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "error")))
		}
		return archiveRef{}, false, err
	}

	bb.cloudIndexMu.Lock()
	archiveKey, ok := bb.cloudBlobIndex[blobHash]
	bb.cloudIndexMu.Unlock()
	if !ok {
		bb.cloudLookupMisses.Add(1)
		if bb.otel.lookups != nil {
			bb.otel.lookups.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "miss")))
		}
		return archiveRef{}, false, nil
	}
	bb.cloudLookupHits.Add(1)
	if bb.otel.lookups != nil {
		bb.otel.lookups.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "hit")))
	}

	archivePath := filepath.Join(bb.config.CacheDir, "archives", filepath.FromSlash(archiveKey))
	return bb.loadCloudArchive(blobHash, archivePath)
}

// directLooseArchivePath resolves the deterministic loose-archive path for a
// content ID. Loose archives are written as "_loose/<content-id>.pack", so a
// local stat or a single cloud HEAD is enough; no listing is needed.
func (bb *BlobBackend) directLooseArchivePath(contentID string) (string, bool) {
	id := strings.TrimSpace(contentID)
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", false
	}
	archivePath := filepath.Join(bb.config.CacheDir, "archives", "_loose", id+".pack")
	if _, err := os.Stat(archivePath); err == nil {
		return archivePath, true
	}
	if bb.headCloudObject == nil {
		return "", false
	}
	if _, ok := bb.headCloudObject(bb.cloudKey(archivePath)); !ok {
		return "", false
	}
	return archivePath, true
}

// loadCloudArchive ensures the archive is local, indexes it, and returns the
// reference for blobHash.
func (bb *BlobBackend) loadCloudArchive(blobHash, archivePath string) (archiveRef, bool, error) {
	bb.mu.RLock()
	ref, exists := bb.blobIndex[blobHash]
	if !exists {
		if bare := normalizeV2Entry(blobHash); bare != "" {
			ref, exists = bb.blobIndex[bare]
		}
	}
	bb.mu.RUnlock()
	if exists {
		if _, err := os.Stat(ref.archivePath); err == nil {
			return ref, true, nil
		}
	}

	if _, err := os.Stat(archivePath); err != nil {
		if err := bb.downloadFromCloud(archivePath); err != nil {
			return archiveRef{}, false, fmt.Errorf("download archive %s: %w", filepath.Base(archivePath), err)
		}
	}
	if _, err := bb.indexArchive(archivePath); err != nil {
		return archiveRef{}, false, fmt.Errorf("index downloaded archive %s: %w", filepath.Base(archivePath), err)
	}

	bb.mu.Lock()
	bb.archivePaths[archivePath] = true
	if rel, relErr := filepath.Rel(filepath.Join(bb.config.CacheDir, "archives"), archivePath); relErr == nil {
		if sid := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]; sid != "" && sid != "." {
			bb.storageIDs[sid] = true
		}
	}
	ref, exists = bb.blobIndex[blobHash]
	if !exists {
		if bare := normalizeV2Entry(blobHash); bare != "" {
			ref, exists = bb.blobIndex[bare]
		}
	}
	bb.mu.Unlock()
	if exists {
		return ref, true, nil
	}
	return archiveRef{archivePath: archivePath, entryPath: blobHash}, true, nil
}

// ensureCloudBlobIndex loads .idx sidecars for the requested storage prefix,
// plus the shared _batch/_loose archive prefixes.
func (bb *BlobBackend) ensureCloudBlobIndex(ctx context.Context, storageID string) error {
	if !bb.hasCloudDownload() || bb.listCloudObjects == nil {
		return fmt.Errorf("cloud index unavailable")
	}

	prefixes := []string{"_batch/", "_loose/"}
	if storageID == "" {
		// Unknown storage scope: scan the whole archive namespace. This is
		// more expensive but still bounded and only happens on demand.
		prefixes = []string{""}
	} else if storageID != "_batch" && storageID != "_loose" {
		prefixes = append(prefixes, storageID+"/")
	}

	for _, prefix := range prefixes {
		bb.cloudIndexMu.Lock()
		loadedAt, loaded := bb.cloudIndexPrefixes[prefix]
		bb.cloudIndexMu.Unlock()
		if loaded && time.Since(loadedAt) < cloudIndexTTL {
			continue
		}

		objects, err := bb.listCloudObjects(ctx, prefix, 5000)
		if err != nil {
			return err
		}
		start := time.Now()
		entries := make(map[string]string)
		for _, object := range objects {
			if !strings.HasSuffix(object.Key, archiveIndexSuffix) {
				continue
			}
			data, err := bb.readCloudObject(ctx, object.Key)
			if err != nil {
				continue
			}
			var hashes []string
			if err := json.Unmarshal(data, &hashes); err != nil {
				continue
			}
			archiveKey := strings.TrimSuffix(object.Key, archiveIndexSuffix)
			for _, hash := range hashes {
				if hash != "" {
					entries[hash] = archiveKey
				}
			}
		}

		bb.cloudIndexMu.Lock()
		// Replace, don't merge, this prefix's entries. A deleted S3 archive
		// must not survive in a fetcher's in-memory index after refresh.
		for hash, key := range bb.cloudBlobIndex {
			if prefix == "" || strings.HasPrefix(key, prefix) {
				delete(bb.cloudBlobIndex, hash)
			}
		}
		for hash, key := range entries {
			bb.cloudBlobIndex[hash] = key
		}
		bb.cloudIndexPrefixes[prefix] = time.Now()
		bb.cloudIndexLoaded = true
		bb.cloudIndexAt = time.Now()
		bb.cloudIndexMu.Unlock()

		bb.cloudIndexLoadsTotal.Add(1)
		if bb.otel.indexLoads != nil {
			bb.otel.indexLoads.Add(context.Background(), 1)
		}
		if bb.otel.indexDuration != nil {
			bb.otel.indexDuration.Record(context.Background(), time.Since(start).Seconds())
		}
		if bb.otel.indexEntries != nil {
			bb.cloudIndexMu.Lock()
			totalEntries := len(bb.cloudBlobIndex)
			bb.cloudIndexMu.Unlock()
			bb.otel.indexEntries.Record(context.Background(), int64(totalEntries))
		}
	}
	return nil
}

func (bb *BlobBackend) readCloudObject(ctx context.Context, key string) ([]byte, error) {
	if !bb.hasCloudDownload() {
		return nil, fmt.Errorf("cloud download not configured")
	}
	reader, err := bb.openCloudReader(key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func (bb *BlobBackend) findBlobOnDisk(blobHash string) (archiveRef, bool, error) {
	archiveDir := filepath.Join(bb.config.CacheDir, "archives")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return archiveRef{}, false, nil
		}
		return archiveRef{}, false, err
	}

	var openErrors []error

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		storageDir := filepath.Join(archiveDir, entry.Name())
		packs, err := filepath.Glob(filepath.Join(storageDir, "*.pack"))
		if err != nil {
			continue
		}
		for _, packPath := range packs {
			store, err := pkgstorage.NewLocalFileReader(packPath)
			if err != nil {
				if bb.logger != nil {
					bb.logger.Warn("findBlobOnDisk: failed to open archive file", "path", packPath, "error", err)
				}
				continue
			}

			ar, err := packager.OpenArchive(store, bb.pipeline)
			if err != nil {
				store.Close()
				openErrors = append(openErrors, fmt.Errorf("archive %s: %w", filepath.Base(packPath), err))
				if bb.logger != nil {
					bb.logger.Warn("findBlobOnDisk: failed to open archive (wrong encryption key?)", "path", packPath, "error", err)
				}
				continue
			}

			foundEntry := ""
			for _, entryPath := range ar.ListFiles() {
				if normalizeBlobEntry(entryPath) == blobHash {
					foundEntry = entryPath
					break
				}
			}
			ar.Close()
			store.Close()

			if foundEntry != "" {
				return archiveRef{
					archivePath: packPath,
					entryPath:   foundEntry,
				}, true, nil
			}
		}
	}

	if len(openErrors) > 0 {
		hasDecrypt := false
		for _, e := range openErrors {
			if errors.Is(e, pipeline.ErrDecryption) || errors.Is(e, pipeline.ErrDecompression) {
				hasDecrypt = true
				break
			}
		}
		if hasDecrypt {
			return archiveRef{}, false, fmt.Errorf("failed to open archives (decryption/authentication error — wrong encryption key?): %w", errors.Join(openErrors...))
		}
		return archiveRef{}, false, fmt.Errorf("failed to open archives (parse/IO errors): %w", errors.Join(openErrors...))
	}

	return archiveRef{}, false, nil
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

// pathWithinDir reports whether path is dir itself or lives below it. Unlike
// a bare strings.HasPrefix it cannot match a sibling such as "/archives/abc2"
// when cleaning up "/archives/abc".
func pathWithinDir(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// Cleanup removes archives for a specific storage ID.
func (bb *BlobBackend) Cleanup(ctx context.Context, sourceKey string) error {
	archiveDir := filepath.Join(bb.config.CacheDir, "archives", sourceKey)

	bb.archiveMu.Lock()
	for path, cached := range bb.archiveCache {
		if pathWithinDir(path, archiveDir) {
			cached.reader.Close()
			cached.store.Close()
			delete(bb.archiveCache, path)
		}
	}
	bb.archiveMu.Unlock()

	bb.mu.Lock()
	for hash, ref := range bb.blobIndex {
		if pathWithinDir(ref.archivePath, archiveDir) {
			delete(bb.blobIndex, hash)
		}
	}
	for path := range bb.archivePaths {
		if pathWithinDir(path, archiveDir) {
			delete(bb.archivePaths, path)
		}
	}
	delete(bb.storageIDs, sourceKey)
	delete(bb.storageBlobCounts, sourceKey)
	bb.mu.Unlock()

	os.RemoveAll(archiveDir)

	return nil
}

// Close shuts down the backend, closing all open archive readers and cloud clients.
func (bb *BlobBackend) Close() error {
	bb.closed.Store(true)

	if bb.scannerStop != nil {
		close(bb.scannerStop)
		bb.scannerWG.Wait()
		bb.scannerStop = nil
	}

	bb.cloudUploadMu.Lock()
	if bb.cloudUploadQueue != nil {
		close(bb.cloudUploadQueue)
		bb.cloudUploadWG.Wait()
		bb.cloudUploadQueue = nil
	}
	bb.cloudUploadMu.Unlock()

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
