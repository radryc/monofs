package router

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// routerGuardianUpsertBatchesTotal counts UpsertGuardianPaths RPC calls.
	routerGuardianUpsertBatchesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "guardian_upsert_batches_total",
		Help:      "Total number of UpsertGuardianPaths RPC batches processed by the router.",
	})

	// routerGuardianUpsertFilesTotal counts individual file upserts forwarded to nodes.
	routerGuardianUpsertFilesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "guardian_upsert_files_total",
		Help:      "Total number of guardian file upserts forwarded by the router.",
	})

	// routerGuardianUpsertBytesTotal counts raw bytes in upserted file content.
	routerGuardianUpsertBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "guardian_upsert_bytes_total",
		Help:      "Total bytes of guardian file content upserted via the router.",
	})

	// routerGuardianDeleteBatchesTotal counts DeleteGuardianPaths RPC calls.
	routerGuardianDeleteBatchesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "guardian_delete_batches_total",
		Help:      "Total number of DeleteGuardianPaths RPC batches processed by the router.",
	})

	// routerGuardianDeleteFilesTotal counts individual file deletes processed.
	routerGuardianDeleteFilesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "guardian_delete_files_total",
		Help:      "Total number of guardian file deletes forwarded by the router.",
	})

	// routerIngestRepositoriesTotal counts IngestRepository RPC calls handled by the router.
	routerIngestRepositoriesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "ingest_repositories_total",
		Help:      "Total number of repository ingest requests processed by the router.",
	})

	// routerIngestFilesTotal counts individual files ingested through the router.
	routerIngestFilesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "ingest_files_total",
		Help:      "Total number of files ingested (forwarded to nodes) by the router.",
	})

	// routerIngestBytesTotal counts raw bytes of file content ingested through the router.
	routerIngestBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "ingest_bytes_total",
		Help:      "Total bytes of file content ingested by the router.",
	})

	// routerNativeReadOpsTotal counts NativeRead (FUSE-path) read operations through the router.
	routerNativeReadOpsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "native_read_ops_total",
		Help:      "Total NativeRead operations (FUSE path reads) routed by the router.",
	})

	// routerNativeReadBytesTotal counts bytes returned via NativeRead.
	routerNativeReadBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "native_read_bytes_total",
		Help:      "Total bytes returned by NativeRead operations through the router.",
	})

	// routerGuardianVersionStoreWriteBytesTotal counts bytes written to the guardian_versions.json snapshot file.
	// Each full-file rewrite adds len(serialized snapshot) bytes — exposes write amplification.
	routerGuardianVersionStoreWriteBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "guardian_version_store_write_bytes_total",
		Help:      "Total bytes written to the guardian_versions.json state snapshot (full rewrites).",
	})

	// routerGuardianVersionStoreFileBytes tracks the current on-disk size of guardian_versions.json.
	routerGuardianVersionStoreFileBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "guardian_version_store_file_bytes",
		Help:      "Current size in bytes of the guardian_versions.json state snapshot file.",
	})

	routerWorkspaceSyncJobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "workspace_sync_jobs_total",
		Help:      "Total workspace sync jobs by action and result.",
	}, []string{"action", "result"})

	routerWorkspaceSyncActiveJobs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "workspace_sync_active_jobs",
		Help:      "Currently active workspace sync jobs by action.",
	}, []string{"action"})

	routerWorkspaceSyncDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "workspace_sync_duration_seconds",
		Help:      "Duration of workspace sync jobs by action and result.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"action", "result"})

	routerWorkspaceSyncRepositoriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "workspace_sync_repositories_total",
		Help:      "Total repository outcomes observed during workspace sync jobs.",
	}, []string{"action", "result"})

	routerWorkspaceSyncBundleBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "workspace_sync_bundle_bytes_total",
		Help:      "Total bytes received through workspace bundle uploads.",
	})

	routerWorkspaceSyncConflictsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "workspace_sync_conflicts_total",
		Help:      "Total workspace sync conflicts by action and reason.",
	}, []string{"action", "reason"})

	routerWorkspaceSyncReingestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "workspace_sync_reingest_total",
		Help:      "Total repository re-ingest attempts triggered by workspace refresh.",
	}, []string{"result"})
)
