package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
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
	QueryLogs(ctx context.Context, query, service string, from, to time.Time, limit int) ([]logengine.LogRecord, error)
	QueryMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time) ([]logengine.MetricRecord, error)
	QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]logengine.SpanRecord, error)
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
	return &pb.LogEngineStats{
		Enabled:      true,
		LogChunks:    stats.LogChunks,
		MetricChunks: stats.MetricChunks,
		TraceChunks:  stats.TraceChunks,
	}
}

// SetDoctorBackend configures the telemetry backend for this server node.
func (s *Server) SetDoctorBackend(b DoctorBackend) {
	s.logEngine = b
}

// doctorBackendOrErr returns the configured backend or an error if not set.
func (s *Server) doctorBackendOrErr() (DoctorBackend, error) {
	if s.logEngine == nil {
		return nil, fmt.Errorf("doctor backend not configured on this node")
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
			RawMessage: e.RawMessage,
		})
	}
	return out
}

// protoToMetricRecords converts proto MetricEntry slice to logengine.MetricRecord slice.
func protoToMetricRecords(entries []*pb.MetricEntry) []logengine.MetricRecord {
	out := make([]logengine.MetricRecord, 0, len(entries))
	for _, e := range entries {
		out = append(out, logengine.MetricRecord{
			Timestamp:  time.Unix(0, e.TimestampUnixNano),
			Service:    e.Service,
			MetricName: e.MetricName,
			Value:      e.Value,
			Labels:     e.Labels,
		})
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
