package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AggregateTelemetry executes a shard-local partial aggregation. The router
// fans this out and merges the rows.
func (s *Server) AggregateTelemetry(ctx context.Context, req *pb.AggregateTelemetryRequest) (*pb.AggregateTelemetryResponse, error) {
	backend, err := s.doctorBackendOrErr()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	signal, err := aggregateSignalName(req.GetSignal())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	from := time.Unix(0, req.GetFromUnixNano()).UTC()
	to := time.Unix(0, req.GetToUnixNano()).UTC()
	if req.GetFromUnixNano() == 0 {
		from = time.Time{}
	}
	if req.GetToUnixNano() == 0 {
		to = time.Time{}
	}
	if req.GetVisiblePartitionsSet() {
		ctx = logengine.WithQueryScope(ctx, requestQueryScope(req.GetVisiblePartitions(), true))
	}
	query := logengine.AggregateQuery{
		Signal:  signal,
		Query:   req.GetQuery(),
		Service: req.GetService(),
		TraceID: req.GetTraceId(),
		MetricQuery: logengine.MetricQuery{
			MetricName:    req.GetMetricName(),
			Service:       req.GetService(),
			LabelMatchers: protoMetricMatchers(req.GetLabelMatchers()),
		},
		From:       from,
		To:         to,
		GroupBy:    append([]string(nil), req.GetGroupBy()...),
		Aggregates: aggregateSpecsFromProto(req.GetAggregates()),
		Limit:      int(req.GetLimit()),
		Partial:    true,
	}
	rows, err := backend.Aggregate(ctx, query)
	if err != nil {
		return nil, telemetryQueryStatus("aggregate telemetry failed", err)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal aggregate rows: %v", err)
	}
	return &pb.AggregateTelemetryResponse{RowsJson: data}, nil
}

func aggregateSignalName(name string) (logengine.Signal, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "logs", "log":
		return logengine.SignalLogs, nil
	case "metrics", "metric":
		return logengine.SignalMetrics, nil
	case "traces", "trace":
		return logengine.SignalTraces, nil
	default:
		return "", fmt.Errorf("unknown telemetry signal %q", name)
	}
}

func aggregateSpecsFromProto(specs []*pb.AggregateSpec) []logengine.AggregateSpec {
	out := make([]logengine.AggregateSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, logengine.AggregateSpec{
			Op:    aggregateOpName(spec.GetOp()),
			Field: spec.GetField(),
			Alias: spec.GetAlias(),
		})
	}
	return out
}

func aggregateOpName(op pb.AggregateOp) logengine.AggregateOp {
	switch op {
	case pb.AggregateOp_AGGREGATE_OP_COUNT:
		return logengine.AggregateCount
	case pb.AggregateOp_AGGREGATE_OP_SUM:
		return logengine.AggregateSum
	case pb.AggregateOp_AGGREGATE_OP_MIN:
		return logengine.AggregateMin
	case pb.AggregateOp_AGGREGATE_OP_MAX:
		return logengine.AggregateMax
	default:
		return logengine.AggregateOp(strings.ToLower(strings.TrimPrefix(op.String(), "AGGREGATE_OP_")))
	}
}
