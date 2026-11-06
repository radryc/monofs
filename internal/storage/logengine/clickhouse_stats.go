package logengine

import "time"

// ClickHouseStats is a point-in-time snapshot of the shared ClickHouse
// telemetry backend. It is reported by a node because the router does not hold
// ClickHouse credentials, and every node writes to the same tables.
type ClickHouseStats struct {
	Enabled  bool
	Healthy  bool
	Version  string
	URL      string
	Database string
	Tenant   string

	// Disk space is the ClickHouse server filesystem, not the MonoFS node.
	DiskTotalBytes uint64
	DiskFreeBytes  uint64
	DiskUsedBytes  uint64

	// Active parts across the spans, logs, and metrics tables.
	TotalBytes uint64
	TotalRows  uint64
	Parts      uint64

	SpansBytes   uint64
	SpansRows    uint64
	LogsBytes    uint64
	LogsRows     uint64
	MetricsBytes uint64
	MetricsRows  uint64

	RetentionSeconds int64
	Error            string
	Measured         time.Time
}
