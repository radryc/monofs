package blob

import (
	"os"
	"strings"
	"time"

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
	Archives                int    `json:"archives"`
	Blobs                   int    `json:"blobs"`

	// Layout and lookup mode.
	Layout             string `json:"layout"`
	DirectLookupHits   int64  `json:"direct_lookup_hits"`
	DirectLookupMisses int64  `json:"direct_lookup_misses"`
	V2Manifests        int64  `json:"v2_manifests"`
	V2Packs            int64  `json:"v2_packs"`

	// Ingest batching.
	BatchPendingBytes int64 `json:"batch_pending_bytes"`
	BatchOpenShards   int64 `json:"batch_open_shards"`

	// Compaction.
	CompactorRunsTotal           int64  `json:"compactor_runs_total"`
	CompactorShardsTotal         int64  `json:"compactor_shards_total"`
	CompactorPlansTotal          int64  `json:"compactor_plans_total"`
	CompactorPacksRetiredTotal   int64  `json:"compactor_packs_retired_total"`
	CompactorPacksDeletedTotal   int64  `json:"compactor_packs_deleted_total"`
	CompactorBytesRewrittenTotal int64  `json:"compactor_bytes_rewritten_total"`
	CompactorLastRunUnix         int64  `json:"compactor_last_run_unix"`
	CompactorLastError           string `json:"compactor_last_error,omitempty"`
}

// resolveBlobLayoutMode returns the configured blob layout. v2 is the default;
// set MONOFS_BLOB_LAYOUT_V2 to a false value to opt back into the legacy
// per-object archive layout.
func resolveBlobLayoutMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MONOFS_BLOB_LAYOUT_V2"))) {
	case "false", "0", "no", "off":
		return "legacy"
	default:
		return "v2"
	}
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

	bb.mu.RLock()
	archives := len(bb.archivePaths)
	blobs := len(bb.blobIndex)
	bb.mu.RUnlock()

	bb.cloudIndexMu.Lock()
	indexLoaded := bb.cloudIndexLoaded
	prefixes := len(bb.cloudIndexPrefixes)
	entries := len(bb.cloudBlobIndex)
	bb.cloudIndexMu.Unlock()

	v2Packs, v2Manifests := bb.v2Counts()

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
		Archives:                archives,
		Blobs:                   blobs,

		Layout:             bb.LayoutMode(),
		DirectLookupHits:   bb.directLookupHits.Load(),
		DirectLookupMisses: bb.directLookupMisses.Load(),
		V2Manifests:        v2Manifests,
		V2Packs:            v2Packs,

		BatchPendingBytes: bb.batchPendingBytes.Load(),
		BatchOpenShards:   bb.batchOpenShards.Load(),

		CompactorRunsTotal:           bb.compactorRunsTotal.Load(),
		CompactorShardsTotal:         bb.compactorShardsTotal.Load(),
		CompactorPlansTotal:          bb.compactorPlansTotal.Load(),
		CompactorPacksRetiredTotal:   bb.compactorPacksRetiredTotal.Load(),
		CompactorPacksDeletedTotal:   bb.compactorPacksDeletedTotal.Load(),
		CompactorBytesRewrittenTotal: bb.compactorBytesRewrittenTotal.Load(),
		CompactorLastRunUnix:         bb.compactorLastRunUnix.Load(),
		CompactorLastError:           compactorLastError(bb),
	}
}

func compactorLastError(bb *BlobBackend) string {
	if v := bb.compactorLastError.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// LayoutMode returns the effective blob layout: "v2" when the backend has a
// v2-capable object store, "legacy" otherwise. Reporting the effective mode
// keeps the fetcher UI honest for deployments without a v2 object store.
func (bb *BlobBackend) LayoutMode() string {
	if bb.layoutMode == "v2" && bb.v2Objects != nil {
		return "v2"
	}
	return "legacy"
}

// SetBatchStats updates the ingest-batching gauges shown on the fetcher UI.
func (bb *BlobBackend) SetBatchStats(pendingBytes, openShards int64) {
	bb.batchPendingBytes.Store(pendingBytes)
	bb.batchOpenShards.Store(openShards)
}

// RecordCompactorRun records the outcome of one compaction cycle.
func (bb *BlobBackend) RecordCompactorRun(shards, plans, retired, deleted, bytesRewritten int64, err error) {
	bb.compactorRunsTotal.Add(1)
	bb.compactorShardsTotal.Add(shards)
	bb.compactorPlansTotal.Add(plans)
	bb.compactorPacksRetiredTotal.Add(retired)
	bb.compactorPacksDeletedTotal.Add(deleted)
	bb.compactorBytesRewrittenTotal.Add(bytesRewritten)
	bb.compactorLastRunUnix.Store(time.Now().UTC().Unix())
	if err != nil {
		bb.compactorLastError.Store(err.Error())
		return
	}
	bb.compactorLastError.Store("")
}

// Stats returns backend statistics. CachedItems is the number of indexed
// blobs, matching the counter maintained by Store/Delete and the per-storage
// counts. Archive (pack) counts are exposed through Observability().Archives.
func (bb *BlobBackend) Stats() storage.BackendStats {
	s := *bb.stats.Load()
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

// ArchiveCount returns the number of .pack archives on this fetcher.
func (bb *BlobBackend) ArchiveCount() int {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return len(bb.archivePaths)
}

// StorageIDCount returns the number of storage IDs with archives.
func (bb *BlobBackend) StorageIDCount() int {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return len(bb.storageIDs)
}
