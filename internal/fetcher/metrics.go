package fetcher

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// fetcherBlobRequestsTotal counts blob fetch requests handled by the fetcher service,
	// labelled by source_type (git, blob, s3, local).
	fetcherBlobRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "fetcher",
		Name:      "blob_requests_total",
		Help:      "Total blob fetch requests served by the fetcher, by source_type.",
	}, []string{"source_type"}) // source_type: git, blob, s3, local

	// fetcherBlobBytesTotal counts bytes of blob content served, by source_type.
	fetcherBlobBytesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "fetcher",
		Name:      "blob_bytes_total",
		Help:      "Total bytes of blob content served by the fetcher, by source_type.",
	}, []string{"source_type"})

	// fetcherBlobErrorsTotal counts fetch errors by source_type.
	fetcherBlobErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "fetcher",
		Name:      "blob_errors_total",
		Help:      "Total blob fetch errors on the fetcher, by source_type.",
	}, []string{"source_type"})

	// fetcherPrefetchRequestsTotal counts background prefetch requests enqueued.
	fetcherPrefetchRequestsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "fetcher",
		Name:      "prefetch_requests_total",
		Help:      "Total background prefetch requests enqueued on the fetcher.",
	})

	// fetcherStoreArchiveBytesTotal counts bytes of packager archives stored on the fetcher.
	fetcherStoreArchiveBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "fetcher",
		Name:      "store_archive_bytes_total",
		Help:      "Total bytes of packager archives stored by the fetcher.",
	})

	// fetcherStoreArchivesTotal counts packager archives stored on the fetcher.
	fetcherStoreArchivesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "fetcher",
		Name:      "store_archives_total",
		Help:      "Total packager archive chunks stored by the fetcher.",
	})

	// fetcherStoreBlobBytesTotal counts bytes from individual blob stores (non-archive).
	fetcherStoreBlobBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "fetcher",
		Name:      "store_blob_bytes_total",
		Help:      "Total bytes written as individual blobs on the fetcher.",
	})
)
