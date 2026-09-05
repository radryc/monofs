package server

import (
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

	// serverWriteBytesTotal counts raw bytes received from clients via Write RPC.
	serverWriteBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "server",
		Name:      "write_bytes_total",
		Help:      "Total bytes of file content received from clients via the Write RPC.",
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
)
