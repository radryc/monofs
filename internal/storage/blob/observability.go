package blob

import (
	"github.com/radryc/monofs/internal/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

type blobOTelMetrics struct {
	walPending     metric.Int64Gauge
	walRecords     metric.Int64Counter
	walReplays     metric.Int64Counter
	walFailures    metric.Int64Counter
	uploads        metric.Int64Counter
	uploadDuration metric.Float64Histogram
	indexLoads     metric.Int64Counter
	indexEntries   metric.Int64Gauge
	indexDuration  metric.Float64Histogram
	lookups        metric.Int64Counter
	downloads      metric.Int64Counter
	downloadBytes  metric.Int64Counter
	archiveDeletes metric.Int64Counter
	invalidations  metric.Int64Counter
}

// BlobObservability is the JSON view of WAL and S3 read-through state exposed
// to the fetch diagnostics endpoint and the router UI.
type BlobObservability struct {
	WALPath                 string `json:"wal_path"`
	WALPending              int    `json:"wal_pending"`
	WALRecordedTotal        int64  `json:"wal_recorded_total"`
	WALReplayedTotal        int64  `json:"wal_replayed_total"`
	WALUploadFailuresTotal  int64  `json:"wal_upload_failures_total"`
	CloudIndexLoaded        bool   `json:"cloud_index_loaded"`
	CloudIndexPrefixes      int    `json:"cloud_index_prefixes"`
	CloudIndexEntries       int    `json:"cloud_index_entries"`
	CloudIndexLoadsTotal    int64  `json:"cloud_index_loads_total"`
	CloudLookupHits         int64  `json:"cloud_lookup_hits"`
	CloudLookupMisses       int64  `json:"cloud_lookup_misses"`
	CloudLookupErrors       int64  `json:"cloud_lookup_errors"`
	CloudDownloadsTotal     int64  `json:"cloud_downloads_total"`
	CloudDownloadBytes      int64  `json:"cloud_download_bytes"`
	ArchiveDeletesTotal     int64  `json:"archive_deletes_total"`
	CacheInvalidationsTotal int64  `json:"cache_invalidations_total"`
}

func (bb *BlobBackend) initOTelMetrics() {
	meter := otel.Meter("monofs.packager")
	bb.otel.walPending, _ = meter.Int64Gauge("monofs.packager.wal.pending",
		metric.WithDescription("Archives waiting for durable S3 upload."))
	bb.otel.walRecords, _ = meter.Int64Counter("monofs.packager.wal.records",
		metric.WithDescription("Archive upload WAL records written."))
	bb.otel.walReplays, _ = meter.Int64Counter("monofs.packager.wal.replays",
		metric.WithDescription("Archive upload WAL entries replayed after restart."))
	bb.otel.walFailures, _ = meter.Int64Counter("monofs.packager.wal.upload_failures",
		metric.WithDescription("Archive uploads that exhausted retries and remain in the WAL."))
	bb.otel.uploads, _ = meter.Int64Counter("monofs.packager.archive.uploads",
		metric.WithDescription("Archive upload attempts by result."))
	bb.otel.uploadDuration, _ = meter.Float64Histogram("monofs.packager.archive.upload.duration",
		metric.WithDescription("Archive upload duration in seconds."),
		metric.WithUnit("s"))
	bb.otel.indexLoads, _ = meter.Int64Counter("monofs.packager.cloud.index.loads",
		metric.WithDescription("S3 archive index prefix loads."))
	bb.otel.indexEntries, _ = meter.Int64Gauge("monofs.packager.cloud.index.entries",
		metric.WithDescription("Blob-to-S3-archive entries currently indexed."))
	bb.otel.indexDuration, _ = meter.Float64Histogram("monofs.packager.cloud.index.load.duration",
		metric.WithDescription("S3 archive index prefix load duration in seconds."),
		metric.WithUnit("s"))
	bb.otel.lookups, _ = meter.Int64Counter("monofs.packager.cloud.lookup",
		metric.WithDescription("Cloud blob index lookups by result."))
	bb.otel.downloads, _ = meter.Int64Counter("monofs.packager.cloud.downloads",
		metric.WithDescription("Archives downloaded from S3 on a local cache miss."))
	bb.otel.downloadBytes, _ = meter.Int64Counter("monofs.packager.cloud.download.bytes",
		metric.WithDescription("Bytes downloaded from S3 on cache misses."),
		metric.WithUnit("By"))
	bb.otel.archiveDeletes, _ = meter.Int64Counter("monofs.packager.archive.deletes",
		metric.WithDescription("Archives removed by compaction or deletion."))
	bb.otel.invalidations, _ = meter.Int64Counter("monofs.packager.cache.invalidations",
		metric.WithDescription("Blob index entries invalidated by deletion."))
}

// Observability returns the WAL and cloud read-through state for the UI.
func (bb *BlobBackend) Observability() BlobObservability {
	bb.walMu.Lock()
	walPath := bb.walPath
	pending := len(bb.walPending)
	bb.walMu.Unlock()

	bb.cloudIndexMu.Lock()
	indexLoaded := bb.cloudIndexLoaded
	prefixes := len(bb.cloudIndexPrefixes)
	entries := len(bb.cloudBlobIndex)
	bb.cloudIndexMu.Unlock()

	return BlobObservability{
		WALPath:                 walPath,
		WALPending:              pending,
		WALRecordedTotal:        bb.walRecordedTotal.Load(),
		WALReplayedTotal:        bb.walReplayedTotal.Load(),
		WALUploadFailuresTotal:  bb.walUploadFailuresTotal.Load(),
		CloudIndexLoaded:        indexLoaded,
		CloudIndexPrefixes:      prefixes,
		CloudIndexEntries:       entries,
		CloudIndexLoadsTotal:    bb.cloudIndexLoadsTotal.Load(),
		CloudLookupHits:         bb.cloudLookupHits.Load(),
		CloudLookupMisses:       bb.cloudLookupMisses.Load(),
		CloudLookupErrors:       bb.cloudLookupErrors.Load(),
		CloudDownloadsTotal:     bb.cloudDownloadsTotal.Load(),
		CloudDownloadBytes:      bb.cloudDownloadBytes.Load(),
		ArchiveDeletesTotal:     bb.archiveDeletesTotal.Load(),
		CacheInvalidationsTotal: bb.cacheInvalidationsTotal.Load(),
	}
}

// Stats returns backend statistics with live archive (pack) count.
func (bb *BlobBackend) Stats() storage.BackendStats {
	bb.mu.RLock()
	packCount := int64(len(bb.archivePaths))
	bb.mu.RUnlock()
	s := *bb.stats.Load()
	s.CachedItems = packCount
	s.CloudStores = bb.cloudStores.Load()
	s.CloudRetrieves = bb.cloudRetrieves.Load()
	return s
}

// StorageHealth returns whether the cloud storage backend is reachable
// and the last error message (empty string if healthy).
func (bb *BlobBackend) StorageHealth() (bool, string) {
	errVal := bb.lastStorageErr.Load()
	if errVal == nil {
		return bb.storageHealthy.Load(), ""
	}
	return bb.storageHealthy.Load(), errVal.(string)
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
