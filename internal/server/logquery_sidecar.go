package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sidecarBackend routes telemetry read queries to the node-local DuckDB
// sidecar while keeping ingest and stats on the native logengine. The sidecar
// exposes the same query RPCs, so the router, UI and Doctor do not need a
// second API surface.
type sidecarBackend struct {
	DoctorBackend
	client   pb.MonoFSClient
	logger   *slog.Logger
	fallback bool
}

func newSidecarBackend(local DoctorBackend, client pb.MonoFSClient, logger *slog.Logger, fallback bool) *sidecarBackend {
	return &sidecarBackend{DoctorBackend: local, client: client, logger: logger, fallback: fallback}
}

func (b *sidecarBackend) StreamLogs(ctx context.Context, query, service string, from, to time.Time, limit int, yield func(logengine.LogRecord) error) error {
	req := &pb.QueryLogsRequest{
		Query:        query,
		Service:      service,
		FromUnixNano: unixNanoOrZero(from),
		ToUnixNano:   unixNanoOrZero(to),
		Limit:        int32(limit),
		BatchSize:    0,
	}
	applyLogScope(ctx, req)
	stream, err := b.client.StreamQueryLogs(ctx, req)
	if err != nil {
		return b.fallbackLogs(ctx, err, query, service, from, to, limit, yield)
	}
	sent := 0
	for {
		item, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return nil
		}
		if recvErr != nil {
			if sent == 0 && b.fallback && shouldFallbackSidecar(recvErr) {
				b.logger.Warn("logquery sidecar stream failed, falling back to native scanner", "error", recvErr)
				return b.DoctorBackend.StreamLogs(ctx, query, service, from, to, limit, yield)
			}
			return recvErr
		}
		if err := forEachSidecarItem(item, func(raw []byte) error {
			var record logengine.LogRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			sent++
			return yield(record)
		}); err != nil {
			return err
		}
	}
}

func shouldFallbackSidecar(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

func (b *sidecarBackend) fallbackLogs(ctx context.Context, err error, query, service string, from, to time.Time, limit int, yield func(logengine.LogRecord) error) error {
	if !b.fallback || !shouldFallbackSidecar(err) {
		return err
	}
	b.logger.Warn("logquery sidecar unavailable, falling back to native scanner", "error", err)
	return b.DoctorBackend.StreamLogs(ctx, query, service, from, to, limit, yield)
}

func (b *sidecarBackend) QueryLogs(ctx context.Context, query, service string, from, to time.Time, limit int) ([]logengine.LogRecord, error) {
	req := &pb.QueryLogsRequest{
		Query:        query,
		Service:      service,
		FromUnixNano: unixNanoOrZero(from),
		ToUnixNano:   unixNanoOrZero(to),
		Limit:        int32(limit),
	}
	applyLogScope(ctx, req)
	resp, err := b.client.QueryLogs(ctx, req)
	if err != nil {
		if b.fallback && shouldFallbackSidecar(err) {
			b.logger.Warn("logquery sidecar unavailable, falling back to native scanner", "error", err)
			return b.DoctorBackend.QueryLogs(ctx, query, service, from, to, limit)
		}
		return nil, err
	}
	var records []logengine.LogRecord
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		return nil, fmt.Errorf("decode sidecar log results: %w", err)
	}
	return records, nil
}

func (b *sidecarBackend) StreamMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricRecord) error) error {
	stream, err := b.client.StreamQueryMetrics(ctx, metricRequest(query, from, to))
	if err != nil {
		return b.fallbackMetrics(ctx, err, query, from, to, yield)
	}
	sent := 0
	for {
		item, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return nil
		}
		if recvErr != nil {
			if sent == 0 && b.fallback && shouldFallbackSidecar(recvErr) {
				b.logger.Warn("logquery sidecar stream failed, falling back to native scanner", "error", recvErr)
				return b.DoctorBackend.StreamMetrics(ctx, query, from, to, yield)
			}
			return recvErr
		}
		if err := forEachSidecarItem(item, func(raw []byte) error {
			var record logengine.MetricRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			sent++
			return yield(record)
		}); err != nil {
			return err
		}
	}
}

func (b *sidecarBackend) fallbackMetrics(ctx context.Context, err error, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricRecord) error) error {
	if !b.fallback || !shouldFallbackSidecar(err) {
		return err
	}
	b.logger.Warn("logquery sidecar unavailable, falling back to native scanner", "error", err)
	return b.DoctorBackend.StreamMetrics(ctx, query, from, to, yield)
}

func (b *sidecarBackend) QueryMetrics(ctx context.Context, query logengine.MetricQuery, from, to time.Time) ([]logengine.MetricRecord, error) {
	resp, err := b.client.QueryMetrics(ctx, metricRequest(query, from, to))
	if err != nil {
		if b.fallback && shouldFallbackSidecar(err) {
			b.logger.Warn("logquery sidecar unavailable, falling back to native scanner", "error", err)
			return b.DoctorBackend.QueryMetrics(ctx, query, from, to)
		}
		return nil, err
	}
	var records []logengine.MetricRecord
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		return nil, fmt.Errorf("decode sidecar metric results: %w", err)
	}
	return records, nil
}

func (b *sidecarBackend) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(logengine.SpanRecord) error) error {
	req := &pb.QueryTracesRequest{
		TraceId:      traceID,
		Service:      service,
		FromUnixNano: unixNanoOrZero(from),
		ToUnixNano:   unixNanoOrZero(to),
		Limit:        int32(limit),
	}
	applyTraceScope(ctx, req)
	stream, err := b.client.StreamQueryTraces(ctx, req)
	if err != nil {
		return b.fallbackTraces(ctx, err, traceID, service, from, to, limit, yield)
	}
	sent := 0
	for {
		item, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return nil
		}
		if recvErr != nil {
			if sent == 0 && b.fallback && shouldFallbackSidecar(recvErr) {
				b.logger.Warn("logquery sidecar stream failed, falling back to native scanner", "error", recvErr)
				return b.DoctorBackend.StreamTraces(ctx, traceID, service, from, to, limit, yield)
			}
			return recvErr
		}
		if err := forEachSidecarItem(item, func(raw []byte) error {
			var record logengine.SpanRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			sent++
			return yield(record)
		}); err != nil {
			return err
		}
	}
}

func (b *sidecarBackend) fallbackTraces(ctx context.Context, err error, traceID, service string, from, to time.Time, limit int, yield func(logengine.SpanRecord) error) error {
	if !b.fallback || !shouldFallbackSidecar(err) {
		return err
	}
	b.logger.Warn("logquery sidecar unavailable, falling back to native scanner", "error", err)
	return b.DoctorBackend.StreamTraces(ctx, traceID, service, from, to, limit, yield)
}

func (b *sidecarBackend) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]logengine.SpanRecord, error) {
	req := &pb.QueryTracesRequest{
		TraceId:      traceID,
		Service:      service,
		FromUnixNano: unixNanoOrZero(from),
		ToUnixNano:   unixNanoOrZero(to),
		Limit:        int32(limit),
	}
	applyTraceScope(ctx, req)
	resp, err := b.client.QueryTraces(ctx, req)
	if err != nil {
		if b.fallback && shouldFallbackSidecar(err) {
			b.logger.Warn("logquery sidecar unavailable, falling back to native scanner", "error", err)
			return b.DoctorBackend.QueryTraces(ctx, traceID, service, from, to, limit)
		}
		return nil, err
	}
	var records []logengine.SpanRecord
	if err := json.Unmarshal(resp.GetResultsJson(), &records); err != nil {
		return nil, fmt.Errorf("decode sidecar trace results: %w", err)
	}
	return records, nil
}

func (b *sidecarBackend) StreamMetricSeries(ctx context.Context, query logengine.MetricQuery, from, to time.Time, yield func(logengine.MetricSeriesMeta) error) error {
	stream, err := b.client.StreamMetricSeries(ctx, metricRequest(query, from, to))
	if err != nil {
		return err
	}
	for {
		item, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return nil
		}
		if recvErr != nil {
			return recvErr
		}
		if err := yield(logengine.MetricSeriesMeta{
			MetricName: item.GetMetricName(),
			Service:    item.GetService(),
			Labels:     item.GetLabels(),
			MinTime:    time.Unix(0, item.GetMinTimeUnixNano()).UTC(),
			MaxTime:    time.Unix(0, item.GetMaxTimeUnixNano()).UTC(),
		}); err != nil {
			return err
		}
	}
}

func metricRequest(query logengine.MetricQuery, from, to time.Time) *pb.QueryMetricsRequest {
	matchers := make([]*pb.MetricLabelMatcher, 0, len(query.LabelMatchers))
	for _, matcher := range query.LabelMatchers {
		matchers = append(matchers, &pb.MetricLabelMatcher{
			Name:  matcher.Name,
			Value: matcher.Value,
			Type:  metricMatcherTypePB(matcher.Type),
		})
	}
	return &pb.QueryMetricsRequest{
		MetricName:    query.MetricName,
		Service:       query.Service,
		FromUnixNano:  unixNanoOrZero(from),
		ToUnixNano:    unixNanoOrZero(to),
		LabelMatchers: matchers,
	}
}

func metricMatcherTypePB(value logengine.MetricMatchType) pb.MetricLabelMatcherType {
	switch value {
	case logengine.MetricMatchNotEqual:
		return pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_NOT_EQUAL
	case logengine.MetricMatchRegexp:
		return pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_REGEXP
	case logengine.MetricMatchNotRegexp:
		return pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_NOT_REGEXP
	default:
		return pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_EQUAL
	}
}

func unixNanoOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func forEachSidecarItem(item *pb.QueryResultItem, fn func([]byte) error) error {
	if item == nil {
		return nil
	}
	items := item.GetItemsJson()
	if len(items) == 0 {
		if raw := item.GetItemJson(); len(raw) > 0 {
			items = [][]byte{raw}
		}
	}
	for _, raw := range items {
		if len(raw) == 0 {
			continue
		}
		if err := fn(raw); err != nil {
			return err
		}
	}
	return nil
}

func applyLogScope(ctx context.Context, req *pb.QueryLogsRequest) {
	if scope, ok := logengine.QueryScopeFromContext(ctx); ok && scope.Restricted {
		req.VisiblePartitions = append([]string(nil), scope.Partitions...)
		req.VisiblePartitionsSet = true
	}
}

func applyTraceScope(ctx context.Context, req *pb.QueryTracesRequest) {
	if scope, ok := logengine.QueryScopeFromContext(ctx); ok && scope.Restricted {
		req.VisiblePartitions = append([]string(nil), scope.Partitions...)
		req.VisiblePartitionsSet = true
	}
}

func (b *sidecarBackend) Aggregate(ctx context.Context, query logengine.AggregateQuery) ([]logengine.AggregateRow, error) {
	req := aggregateRequest(query)
	applyAggregateScope(ctx, req)
	resp, err := b.client.AggregateTelemetry(ctx, req)
	if err != nil {
		if b.fallback && shouldFallbackSidecar(err) {
			b.logger.Warn("logquery sidecar unavailable, falling back to native scanner", "error", err)
			return b.DoctorBackend.Aggregate(ctx, query)
		}
		return nil, err
	}
	if len(resp.GetRowsJson()) == 0 {
		return nil, nil
	}
	var rows []logengine.AggregateRow
	decoder := json.NewDecoder(bytes.NewReader(resp.GetRowsJson()))
	decoder.UseNumber()
	if err := decoder.Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode sidecar aggregate: %w", err)
	}
	return rows, nil
}

func aggregateRequest(query logengine.AggregateQuery) *pb.AggregateTelemetryRequest {
	specs := make([]*pb.AggregateSpec, 0, len(query.Aggregates))
	for _, spec := range query.Aggregates {
		specs = append(specs, &pb.AggregateSpec{
			Op:    aggregateOpPB(spec.Op),
			Field: spec.Field,
			Alias: spec.Alias,
		})
	}
	matchers := make([]*pb.MetricLabelMatcher, 0, len(query.MetricQuery.LabelMatchers))
	for _, matcher := range query.MetricQuery.LabelMatchers {
		matchers = append(matchers, &pb.MetricLabelMatcher{
			Name:  matcher.Name,
			Value: matcher.Value,
			Type:  metricMatcherTypePB(matcher.Type),
		})
	}
	return &pb.AggregateTelemetryRequest{
		Signal:        string(query.Signal),
		Query:         query.Query,
		FromUnixNano:  unixNanoOrZero(query.From),
		ToUnixNano:    unixNanoOrZero(query.To),
		Service:       query.Service,
		MetricName:    query.MetricQuery.MetricName,
		TraceId:       query.TraceID,
		LabelMatchers: matchers,
		GroupBy:       append([]string(nil), query.GroupBy...),
		Aggregates:    specs,
		Limit:         int32(query.Limit),
	}
}

func applyAggregateScope(ctx context.Context, req *pb.AggregateTelemetryRequest) {
	if scope, ok := logengine.QueryScopeFromContext(ctx); ok && scope.Restricted {
		req.VisiblePartitions = append([]string(nil), scope.Partitions...)
		req.VisiblePartitionsSet = true
	}
}

func aggregateOpPB(op logengine.AggregateOp) pb.AggregateOp {
	switch op {
	case logengine.AggregateCount:
		return pb.AggregateOp_AGGREGATE_OP_COUNT
	case logengine.AggregateSum:
		return pb.AggregateOp_AGGREGATE_OP_SUM
	case logengine.AggregateMin:
		return pb.AggregateOp_AGGREGATE_OP_MIN
	case logengine.AggregateMax:
		return pb.AggregateOp_AGGREGATE_OP_MAX
	default:
		return pb.AggregateOp_AGGREGATE_OP_UNSPECIFIED
	}
}
