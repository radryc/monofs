package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DoctorBackend is the interface the server uses to ingest and query telemetry signals.
// *logengine.LogEngine satisfies this interface.
type DoctorBackend interface {
	IngestLogs(ctx context.Context, chunkID string, logs []logengine.LogRecord) error
	IngestMetrics(ctx context.Context, chunkID string, metrics []logengine.MetricRecord) error
	IngestTraces(ctx context.Context, chunkID string, spans []logengine.SpanRecord) error
	StreamLogs(ctx context.Context, query, service string, from, to time.Time, limit int, yield func(logengine.LogRecord) error) error
	StreamMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricRecord) error) error
	StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(logengine.SpanRecord) error) error
	StreamMetricSeries(ctx context.Context, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricSeriesMeta) error) error
	QueryLogs(ctx context.Context, query, service string, from, to time.Time, limit int) ([]logengine.LogRecord, error)
	QueryMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time) ([]logengine.MetricRecord, error)
	QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]logengine.SpanRecord, error)
	Aggregate(ctx context.Context, query logengine.AggregateQuery) ([]logengine.AggregateRow, error)
	QueryDuckDBSQL(ctx context.Context, dataset, query string, from, to time.Time, limit int, scope logengine.QueryScope) (logengine.DuckDBSQLResult, error)
	Stats(ctx context.Context) (logengine.LogEngineStats, error)
}

// logEngineProtoStats returns a *pb.LogEngineStats for the current node.
// Returns a disabled marker if no backend is configured.
func (s *Server) logEngineProtoStats(ctx context.Context) *pb.LogEngineStats {
	if s.logEngine == nil {
		return &pb.LogEngineStats{Enabled: false}
	}
	stats, err := s.logEngine.Stats(ctx)
	if err != nil {
		return &pb.LogEngineStats{Enabled: true}
	}
	out := &pb.LogEngineStats{
		Enabled:                true,
		LogChunks:              stats.LogChunks,
		MetricChunks:           stats.MetricChunks,
		TraceChunks:            stats.TraceChunks,
		LogBuckets:             stats.LogBuckets,
		MetricBuckets:          stats.MetricBuckets,
		TraceBuckets:           stats.TraceBuckets,
		LogCompactedBuckets:    stats.LogCompactedBuckets,
		MetricCompactedBuckets: stats.MetricCompactedBuckets,
		TraceCompactedBuckets:  stats.TraceCompactedBuckets,
	}
	if provider, ok := s.logEngine.(interface {
		DuckDBStats() logengine.DuckDBStats
	}); ok {
		duck := provider.DuckDBStats()
		out.DuckdbEnabled = duck.Enabled
		out.DuckdbMemoryLimit = duck.Memory
		out.DuckdbThreads = int32(duck.Threads)
		out.DuckdbQueries = duck.Queries
		out.DuckdbErrors = duck.Errors
		out.DuckdbTempDir = duck.TempDir
		out.DuckdbFallbacks = duck.Fallbacks
	}
	if s.logQuerySidecar != nil {
		out.DuckdbEnabled = true
	}
	if provider, ok := s.logEngine.(interface {
		CompactionStats() logengine.CompactionStats
	}); ok {
		compaction := provider.CompactionStats()
		out.CompactionRuns = compaction.Runs
		out.CompactionInputChunks = compaction.InputChunks
		out.CompactionOutputChunks = compaction.OutputChunks
		out.CompactionDeletedChunks = compaction.DeletedChunks
		out.CompactionRecords = compaction.Records
		out.CompactionRunning = compaction.Running
		out.CompactionTargetBytes = compaction.TargetBytes
		if !compaction.LastRun.IsZero() {
			out.LastCompactionUnixNano = compaction.LastRun.UnixNano()
		}
	}
	if provider, ok := s.logEngine.(interface {
		ClickHouseStats(context.Context) logengine.ClickHouseStats
	}); ok {
		stats := provider.ClickHouseStats(ctx)
		out.Clickhouse = clickHouseProtoStats(stats)
	}
	return out
}

func clickHouseProtoStats(stats logengine.ClickHouseStats) *pb.ClickHouseStats {
	if !stats.Enabled {
		return nil
	}
	out := &pb.ClickHouseStats{
		Enabled:          stats.Enabled,
		Healthy:          stats.Healthy,
		Version:          stats.Version,
		Url:              stats.URL,
		Database:         stats.Database,
		Tenant:           stats.Tenant,
		DiskTotalBytes:   stats.DiskTotalBytes,
		DiskFreeBytes:    stats.DiskFreeBytes,
		DiskUsedBytes:    stats.DiskUsedBytes,
		TotalBytes:       stats.TotalBytes,
		TotalRows:        stats.TotalRows,
		Parts:            stats.Parts,
		SpansBytes:       stats.SpansBytes,
		SpansRows:        stats.SpansRows,
		LogsBytes:        stats.LogsBytes,
		LogsRows:         stats.LogsRows,
		MetricsBytes:     stats.MetricsBytes,
		MetricsRows:      stats.MetricsRows,
		RetentionSeconds: stats.RetentionSeconds,
		Error:            stats.Error,
	}
	if !stats.Measured.IsZero() {
		out.MeasuredUnixNano = stats.Measured.UnixNano()
	}
	return out
}

// SetDoctorBackend configures the telemetry backend for this server node.
func (s *Server) SetDoctorBackend(b DoctorBackend) {
	s.logEngine = b
}

// SetLogQuerySidecar routes telemetry read queries to a DuckDB sidecar. The
// sidecar must expose the MonoFS telemetry query RPCs. Ingest and stats stay
// on the local logengine. proxyTraces is false when traces are served by a
// different engine, such as ClickHouse.
func (s *Server) SetLogQuerySidecar(client pb.MonoFSClient, fallback, proxyTraces bool) {
	s.logQuerySidecar = client
	s.logQuerySidecarFallback = fallback
	s.logQuerySidecarProxyTraces = proxyTraces
}

// doctorBackendOrErr returns the configured backend or an error if not set.
func (s *Server) doctorBackendOrErr() (DoctorBackend, error) {
	if s.logEngine == nil {
		return nil, fmt.Errorf("doctor backend not configured on this node")
	}
	if s.logQuerySidecar != nil {
		return newSidecarBackend(s.logEngine, s.logQuerySidecar, s.logger, s.logQuerySidecarFallback, s.logQuerySidecarProxyTraces), nil
	}
	return s.logEngine, nil
}

// protoToLogRecords converts proto LogEntry slice to logengine.LogRecord slice.
func protoToLogRecords(entries []*pb.LogEntry) []logengine.LogRecord {
	out := make([]logengine.LogRecord, 0, len(entries))
	for _, e := range entries {
		out = append(out, logengine.LogRecord{
			Timestamp:  time.Unix(0, e.TimestampUnixNano),
			Level:      e.Level,
			Service:    e.Service,
			TraceID:    e.TraceId,
			SpanID:     e.SpanId,
			RawMessage: e.RawMessage,
			Labels:     e.Labels,
		})
	}
	return out
}

// protoToMetricRecords converts proto MetricEntry slice to logengine.MetricRecord slice.
func protoToMetricRecords(entries []*pb.MetricEntry) []logengine.MetricRecord {
	out := make([]logengine.MetricRecord, 0, len(entries))
	for _, e := range entries {
		record := logengine.MetricRecord{
			Timestamp:   time.Unix(0, e.TimestampUnixNano),
			Service:     e.Service,
			MetricName:  e.MetricName,
			Value:       e.Value,
			MetricType:  e.MetricType,
			Temporality: e.Temporality,
			Unit:        e.Unit,
			Description: e.Description,
			Labels:      e.Labels,
		}
		if e.StartTimeUnixNano != 0 {
			record.StartTime = time.Unix(0, e.StartTimeUnixNano)
		}
		out = append(out, record)
	}
	return out
}

// protoToSpanRecords converts proto SpanEntry slice to logengine.SpanRecord slice.
func protoToSpanRecords(entries []*pb.SpanEntry) []logengine.SpanRecord {
	out := make([]logengine.SpanRecord, 0, len(entries))
	for _, e := range entries {
		out = append(out, logengine.SpanRecord{
			Timestamp:    time.Unix(0, e.StartUnixNano),
			EndTime:      time.Unix(0, e.EndUnixNano),
			TraceID:      e.TraceId,
			SpanID:       e.SpanId,
			ParentSpanID: e.ParentSpanId,
			Service:      e.Service,
			Name:         e.Name,
			StatusCode:   e.StatusCode,
			Attributes:   e.Attributes,
		})
	}
	return out
}

func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func requestQueryScope(partitions []string, restricted bool) logengine.QueryScope {
	if !restricted {
		return logengine.QueryScope{}
	}
	return logengine.QueryScope{Partitions: append([]string(nil), partitions...), Restricted: true}
}

// telemetryQueryStatus maps storage budget errors to gRPC status codes instead
// of reporting every failure as Internal.
func telemetryQueryStatus(prefix string, err error) error {
	switch {
	case errors.Is(err, logengine.ErrQueryTooBroad), errors.Is(err, logengine.ErrMetricSeriesLimit):
		return status.Errorf(codes.ResourceExhausted, "%s: %v", prefix, err)
	case errors.Is(err, logengine.ErrDuckDBUnavailable):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", prefix, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", prefix, err)
	}
}
