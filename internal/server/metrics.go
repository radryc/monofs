package server

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// serverOpsTotal counts gRPC operations handled by the MonoFS node server,
	// labelled by operation (lookup, getattr, read, readdir, write, delete,
	// ingest, register_repository).
	serverOpsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "ops_total",
		Help:      "Total number of operations handled by the MonoFS node server, by op.",
	}, []string{"op"}) // op: lookup, getattr, read, readdir, write, delete, ingest, register_repository

	// serverReadBytesTotal counts raw file bytes sent to clients via Read RPC.
	serverReadBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "read_bytes_total",
		Help:      "Total bytes of file content streamed to clients via the Read RPC.",
	})

	// serverIngestFilesTotal counts individual files ingested into the node.
	serverIngestFilesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "ingest_files_total",
		Help:      "Total number of files ingested (metadata stored) on this node.",
	})

	// serverIngestBytesTotal counts bytes of file metadata/content ingested.
	serverIngestBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "ingest_bytes_total",
		Help:      "Total bytes of file content ingested (metadata stored) on this node.",
	})

	// serverKVSReadOpsTotal counts read operations routed to the KVS backend by the server.
	serverKVSReadOpsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "kvs_read_ops_total",
		Help:      "Total KVS-path read operations resolved by this server node, by op.",
	}, []string{"op"}) // op: lookup, getattr, read, readdir

	// serverKVSWriteOpsTotal counts write operations routed to the KVS backend by the server.
	serverKVSWriteOpsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "kvs_write_ops_total",
		Help:      "Total KVS-path write operations committed by this server node, by op.",
	}, []string{"op"}) // op: upsert, delete

	// serverOpDurationSeconds is the end-to-end latency of a node RPC, by op.
	serverOpDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "op_duration_seconds",
		Help:      "End-to-end latency of MonoFS node RPCs, by op.",
		Buckets:   []float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"op"})

	// serverFetchDurationSeconds isolates the fetcher/S3 portion of Read.
	serverFetchDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "fetch_duration_seconds",
		Help:      "Latency of blob fetches from a node, by result (ok, not_found, error).",
		Buckets:   []float64{0.0001, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"result"})
)

func observeServerOp(op string, start time.Time) {
	serverOpDurationSeconds.WithLabelValues(op).Observe(time.Since(start).Seconds())
}
