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
)
