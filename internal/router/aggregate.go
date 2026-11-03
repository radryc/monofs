package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"github.com/radryc/monofs/pkg/authz/policy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type aggregateWireRow struct {
	Group  map[string]string      `json:"group"`
	Values map[string]json.Number `json:"values"`
}

type aggregateSpecByAlias map[string]logengine.AggregateSpec

func aggregateSignal(name string) (logengine.Signal, policy.Signal, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "logs", "log":
		return logengine.SignalLogs, policy.SignalLogs, nil
	case "metrics", "metric":
		return logengine.SignalMetrics, policy.SignalMetrics, nil
	case "traces", "trace":
		return logengine.SignalTraces, policy.SignalTraces, nil
	default:
		return "", "", fmt.Errorf("unknown telemetry signal %q", name)
	}
}

// AggregateTelemetry implements the coordinator side of scatter-gather
// aggregation. Every healthy node returns partial groups; this method merges
// count/sum/min/max values by group key and applies the final limit.
func (r *Router) AggregateTelemetry(ctx context.Context, req *pb.AggregateTelemetryRequest) (*pb.AggregateTelemetryResponse, error) {
	signal, policySignal, err := aggregateSignal(req.GetSignal())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	req.Signal = string(signal)
	if partitions, restricted := r.queryScope(ctx, policySignal); restricted {
		req.VisiblePartitions = partitions
		req.VisiblePartitionsSet = true
	}
	specs, err := aggregateSpecsFromProto(req.GetAggregates())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	byAlias := make(aggregateSpecByAlias, len(specs))
	for _, spec := range specs {
		byAlias[spec.Alias] = spec
	}

	clients := r.allHealthyNodeClients()
	if len(clients) == 0 {
		return nil, status.Error(codes.Unavailable, "no healthy nodes available")
	}
	type result struct {
		rows []byte
		err  error
	}
	results := make(chan result, len(clients))
	for _, client := range clients {
		go func(client pb.MonoFSClient) {
			resp, err := client.AggregateTelemetry(ctx, req)
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{rows: resp.GetRowsJson()}
		}(client)
	}

	merged := make(map[string]*aggregateWireRow)
	for range clients {
		res := <-results
		if res.err != nil {
			return nil, status.Errorf(codes.Unavailable, "aggregate shard query failed: %v", res.err)
		}
		rows, err := decodeAggregateRows(res.rows)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "decode shard aggregate: %v", err)
		}
		for _, row := range rows {
			key := aggregateWireKey(row.Group, req.GetGroupBy())
			target := merged[key]
			if target == nil {
				target = &aggregateWireRow{Group: row.Group, Values: make(map[string]json.Number, len(row.Values))}
				merged[key] = target
			}
			for alias, value := range row.Values {
				spec, ok := byAlias[alias]
				if !ok {
					continue
				}
				target.Values[alias] = mergeAggregateNumber(target.Values[alias], value, spec.Op)
			}
		}
	}

	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if limit := int(req.GetLimit()); limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]aggregateWireRow, 0, len(keys))
	for _, key := range keys {
		out = append(out, *merged[key])
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal aggregate: %v", err)
	}
	return &pb.AggregateTelemetryResponse{RowsJson: data}, nil
}

func aggregateSpecsFromProto(specs []*pb.AggregateSpec) ([]logengine.AggregateSpec, error) {
	out := make([]logengine.AggregateSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, logengine.AggregateSpec{
			Op:    aggregateOp(spec.GetOp()),
			Field: spec.GetField(),
			Alias: spec.GetAlias(),
		})
	}
	return logengine.NormalizeAggregateSpecs(out)
}

func aggregateOp(op pb.AggregateOp) logengine.AggregateOp {
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

func decodeAggregateRows(data []byte) ([]aggregateWireRow, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var rows []aggregateWireRow
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func aggregateWireKey(group map[string]string, fields []string) string {
	var builder strings.Builder
	for _, field := range fields {
		builder.WriteString(field)
		builder.WriteByte('=')
		builder.WriteString(group[field])
		builder.WriteByte(0)
	}
	return builder.String()
}

func mergeAggregateNumber(current, incoming json.Number, op logengine.AggregateOp) json.Number {
	if incoming == "" {
		return current
	}
	if current == "" {
		return incoming
	}
	switch op {
	case logengine.AggregateCount, logengine.AggregateSum:
		return addJSONNumbers(current, incoming)
	case logengine.AggregateMin:
		if compareJSONNumbers(incoming, current) < 0 {
			return incoming
		}
	case logengine.AggregateMax:
		if compareJSONNumbers(incoming, current) > 0 {
			return incoming
		}
	}
	return current
}

func addJSONNumbers(left, right json.Number) json.Number {
	if strings.ContainsAny(left.String(), ".eE") || strings.ContainsAny(right.String(), ".eE") {
		l, _ := left.Float64()
		r, _ := right.Float64()
		return json.Number(formatFloat(l + r))
	}
	l, _ := left.Int64()
	r, _ := right.Int64()
	return json.Number(fmt.Sprintf("%d", l+r))
}

func compareJSONNumbers(left, right json.Number) int {
	if strings.ContainsAny(left.String(), ".eE") || strings.ContainsAny(right.String(), ".eE") {
		l, _ := left.Float64()
		r, _ := right.Float64()
		switch {
		case l < r:
			return -1
		case l > r:
			return 1
		default:
			return 0
		}
	}
	l, _ := left.Int64()
	r, _ := right.Int64()
	switch {
	case l < r:
		return -1
	case l > r:
		return 1
	default:
		return 0
	}
}

func formatFloat(value float64) string {
	return fmt.Sprintf("%g", value)
}
