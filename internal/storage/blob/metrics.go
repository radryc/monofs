package blob

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// packagerFetchBlobTotal counts blob fetch operations from packager archives,
	// labelled by storage_type (local, s3, gcs).
	packagerFetchBlobTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "fetch_blob_total",
		Help:      "Total blob fetches from packager archive backend, by storage_type.",
	}, []string{"storage_type"}) // storage_type: local, s3, gcs

	// packagerFetchBytesTotal counts bytes returned by packager blob reads.
	packagerFetchBytesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "fetch_bytes_total",
		Help:      "Total bytes read from packager archive backend, by storage_type.",
	}, []string{"storage_type"})

	// packagerFetchErrorsTotal counts errors during packager blob reads.
	packagerFetchErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "fetch_errors_total",
		Help:      "Total blob fetch errors in the packager archive backend, by storage_type.",
	}, []string{"storage_type"})

	// packagerStoreArchiveBytesTotal counts bytes written as packager archives.
	packagerStoreArchiveBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "store_archive_bytes_total",
		Help:      "Total bytes written as packager archive files (.pack).",
	})

	// packagerStoreArchivesTotal counts packager archive chunks stored.
	packagerStoreArchivesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "store_archives_total",
		Help:      "Total number of packager archive chunks stored.",
	})

	// packagerStoreBlobsTotal counts individual blobs stored (single/loose + batch).
	packagerStoreBlobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "store_blobs_total",
		Help:      "Total blobs stored in the packager backend, by store_type (single, batch).",
	}, []string{"store_type"}) // store_type: single, batch

	// packagerIndexedBlobsGauge tracks the number of blobs currently indexed in memory.
	packagerIndexedBlobsGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "indexed_blobs",
		Help:      "Current number of blobs indexed in the packager archive backend.",
	})

	// packagerCloudBackupMissingTotal counts local archives discovered without a
	// corresponding cloud object during background integrity scans.
	packagerCloudBackupMissingTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "cloud_backup_missing_total",
		Help:      "Local archives found missing from cloud backup by the integrity scanner.",
	})

	// packagerCloudBackupRepairedTotal counts local archives successfully
	// re-uploaded to cloud by the integrity scanner.
	packagerCloudBackupRepairedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "cloud_backup_repaired_total",
		Help:      "Local archives re-uploaded to cloud backup by the integrity scanner.",
	})

	// packagerCloudBackupScanDurationSeconds records the time taken by each
	// background integrity scan.
	packagerCloudBackupScanDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "cloud_backup_scan_duration_seconds",
		Help:      "Duration of background cloud-backup integrity scans.",
		Buckets:   []float64{1, 5, 15, 30, 60, 120, 300},
	})

	// packagerLocalArchiveDeletionBlockedTotal counts local archive deletions
	// that were blocked because the archive had no verified cloud backup.
	packagerLocalArchiveDeletionBlockedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "local_archive_deletion_blocked_total",
		Help:      "Local archive deletions blocked due to missing cloud backup.",
	})

	// packagerEncryptionKeyPending is 1 when the fetcher is running with an
	// encryption key that has not yet been explicitly accepted, 0 otherwise.
	packagerEncryptionKeyPending = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "monofs",
		Subsystem: "packager",
		Name:      "encryption_key_pending",
		Help:      "Set to 1 when the encryption key requires explicit confirmation.",
	})
)
