package logengine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AggregateOp is a partial aggregation operation.
type AggregateOp string

const (
	AggregateCount AggregateOp = "count"
	AggregateSum   AggregateOp = "sum"
	AggregateMin   AggregateOp = "min"
	AggregateMax   AggregateOp = "max"
)

// AggregateSpec describes one output aggregate. Alias defaults to
// "<op>_<field>" (or "count" for count with no field).
type AggregateSpec struct {
	Op    AggregateOp
	Field string
	Alias string
}

// AggregateQuery is a partial aggregation over one telemetry signal.
type AggregateQuery struct {
	Signal      Signal
	Query       string
	Service     string
	MetricQuery MetricQuery
	TraceID     string
	From        time.Time
	To          time.Time
	GroupBy     []string
	Aggregates  []AggregateSpec
	Limit       int
	// Partial marks a shard-local query. The coordinator applies the final
	// LIMIT after merging; shard-local queries must not truncate groups.
	Partial bool
}

// AggregateRow is one partial group row.
type AggregateRow struct {
	Group  map[string]string `json:"group"`
	Values map[string]any    `json:"values"`
}

const maxAggregateGroups = 10000

// NormalizeAggregateSpecs fills aliases and validates operation names.
func NormalizeAggregateSpecs(specs []AggregateSpec) ([]AggregateSpec, error) {
	out := make([]AggregateSpec, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		switch spec.Op {
		case AggregateCount, AggregateSum, AggregateMin, AggregateMax:
		default:
			return nil, fmt.Errorf("unsupported aggregate op %q", spec.Op)
		}
		field := strings.TrimSpace(spec.Field)
		if spec.Op != AggregateCount && field == "" {
			return nil, fmt.Errorf("aggregate %s requires a field", spec.Op)
		}
		alias := strings.TrimSpace(spec.Alias)
		if alias == "" {
			if spec.Op == AggregateCount && field == "" {
				alias = "count"
			} else {
				alias = string(spec.Op) + "_" + field
			}
		}
		if _, ok := seen[alias]; ok {
			return nil, fmt.Errorf("duplicate aggregate alias %q", alias)
		}
		seen[alias] = struct{}{}
		out = append(out, AggregateSpec{Op: spec.Op, Field: field, Alias: alias})
	}
	if len(out) == 0 {
		out = append(out, AggregateSpec{Op: AggregateCount, Alias: "count"})
	}
	return out, nil
}

// Aggregate executes a shard-local partial aggregation. The router merges the
// returned rows and applies the final limit.
func (e *LogEngine) Aggregate(ctx context.Context, query AggregateQuery) ([]AggregateRow, error) {
	specs, err := NormalizeAggregateSpecs(query.Aggregates)
	if err != nil {
		return nil, err
	}
	query.Aggregates = specs
	if query.Signal == "" {
		query.Signal = SignalLogs
	}
	for _, field := range query.GroupBy {
		if _, ok := aggregateGroupField(query.Signal, field); !ok {
			return nil, fmt.Errorf("unsupported group_by field %q for %s", field, query.Signal)
		}
	}
	for _, spec := range specs {
		if spec.Op == AggregateCount {
			continue
		}
		if !aggregateNumericField(query.Signal, spec.Field) {
			return nil, fmt.Errorf("aggregate field %q is not numeric for %s", spec.Field, query.Signal)
		}
	}

	groups := make(map[string]*aggregateGroup)
	keyOrder := make([]string, 0)
	add := func(group map[string]string, record any) error {
		key := aggregateGroupKey(group, query.GroupBy)
		state := groups[key]
		if state == nil {
			if len(groups) >= maxAggregateGroups {
				return fmt.Errorf("too many aggregation groups (limit %d)", maxAggregateGroups)
			}
			state = &aggregateGroup{group: group, values: make([]aggregateValue, len(specs))}
			groups[key] = state
			keyOrder = append(keyOrder, key)
		}
		for idx, spec := range specs {
			state.values[idx].count++
			if spec.Op == AggregateCount {
				continue
			}
			value, ok := aggregateNumericValue(query.Signal, spec.Field, record)
			if !ok {
				return fmt.Errorf("field %q has no numeric value", spec.Field)
			}
			state.values[idx].add(spec.Op, value)
		}
		return nil
	}

	switch query.Signal {
	case SignalLogs:
		queryStr := strings.TrimSpace(query.Query)
		if queryStr == "" {
			queryStr = "{}"
		}
		err = e.StreamLogs(ctx, queryStr, query.Service, query.From, query.To, 0, func(record LogRecord) error {
			return add(aggregateLogGroup(record, query.GroupBy), record)
		})
	case SignalMetrics:
		err = e.StreamMetrics(ctx, query.MetricQuery, query.From, query.To, func(record MetricRecord) error {
			return add(aggregateMetricGroup(record, query.GroupBy), record)
		})
	case SignalTraces:
		err = e.StreamTraces(ctx, query.TraceID, query.Service, query.From, query.To, 0, func(record SpanRecord) error {
			return add(aggregateSpanGroup(record, query.GroupBy), record)
		})
	default:
		return nil, fmt.Errorf("unknown signal %q", query.Signal)
	}
	if err != nil {
		return nil, err
	}

	sort.Slice(keyOrder, func(i, j int) bool { return keyOrder[i] < keyOrder[j] })
	if query.Limit > 0 && !query.Partial && len(keyOrder) > query.Limit {
		keyOrder = keyOrder[:query.Limit]
	}
	out := make([]AggregateRow, 0, len(keyOrder))
	for _, key := range keyOrder {
		state := groups[key]
		values := make(map[string]any, len(specs))
		for idx, spec := range specs {
			values[spec.Alias] = state.values[idx].result(spec.Op)
		}
		out = append(out, AggregateRow{Group: state.group, Values: values})
	}
	return out, nil
}

type aggregateGroup struct {
	group  map[string]string
	values []aggregateValue
}

type aggregateValue struct {
	count int64
	value any
}

func (v *aggregateValue) add(op AggregateOp, value any) {
	switch op {
	case AggregateSum:
		v.value = addNumbers(v.value, value)
	case AggregateMin:
		if v.value == nil || compareNumbers(value, v.value) < 0 {
			v.value = value
		}
	case AggregateMax:
		if v.value == nil || compareNumbers(value, v.value) > 0 {
			v.value = value
		}
	}
}

func (v aggregateValue) result(op AggregateOp) any {
	if op == AggregateCount {
		return v.count
	}
	return v.value
}

func aggregateGroupKey(group map[string]string, fields []string) string {
	var builder strings.Builder
	for _, field := range fields {
		builder.WriteString(field)
		builder.WriteByte('=')
		builder.WriteString(group[field])
		builder.WriteByte(0)
	}
	return builder.String()
}

func aggregateLogGroup(record LogRecord, fields []string) map[string]string {
	group := make(map[string]string, len(fields))
	for _, field := range fields {
		switch field {
		case "partition":
			group[field] = recordPartition(record.Labels)
		case "service", "service_name":
			group[field] = record.Service
		case "level", "severity_text":
			group[field] = record.Level
		case "trace_id":
			group[field] = record.TraceID
		case "span_id":
			group[field] = record.SpanID
		case "body", "raw_message":
			group[field] = record.RawMessage
		default:
			group[field] = aggregateLabelValue(record.Labels, field)
		}
	}
	return group
}

func aggregateMetricGroup(record MetricRecord, fields []string) map[string]string {
	group := make(map[string]string, len(fields))
	for _, field := range fields {
		switch field {
		case "partition":
			group[field] = recordPartition(record.Labels)
		case "service":
			group[field] = record.Service
		case "metric_name":
			group[field] = record.MetricName
		default:
			group[field] = aggregateLabelValue(record.Labels, field)
		}
	}
	return group
}

func aggregateSpanGroup(record SpanRecord, fields []string) map[string]string {
	group := make(map[string]string, len(fields))
	for _, field := range fields {
		switch field {
		case "partition":
			group[field] = recordPartition(record.Attributes)
		case "service":
			group[field] = record.Service
		case "trace_id":
			group[field] = record.TraceID
		case "span_id":
			group[field] = record.SpanID
		case "name":
			group[field] = record.Name
		case "status_code":
			group[field] = record.StatusCode
		default:
			group[field] = aggregateLabelValue(record.Attributes, field)
		}
	}
	return group
}

func aggregateLabelValue(values map[string]string, field string) string {
	for _, prefix := range []string{"labels.", "attributes."} {
		if key, ok := strings.CutPrefix(field, prefix); ok {
			value, _ := lookupLabelValue(values, key)
			return value
		}
	}
	value, _ := lookupLabelValue(values, field)
	return value
}

func aggregateGroupField(signal Signal, field string) (string, bool) {
	field = strings.TrimSpace(field)
	if field == "" {
		return "", false
	}
	switch signal {
	case SignalLogs:
		switch field {
		case "partition", "service", "service_name", "level", "severity_text", "trace_id", "span_id", "body", "raw_message":
			return field, true
		}
	case SignalMetrics:
		switch field {
		case "partition", "service", "metric_name":
			return field, true
		}
	case SignalTraces:
		switch field {
		case "partition", "service", "trace_id", "span_id", "name", "status_code":
			return field, true
		}
	}
	if strings.HasPrefix(field, "labels.") || strings.HasPrefix(field, "attributes.") {
		return field, true
	}
	return "", false
}

func aggregateNumericField(signal Signal, field string) bool {
	switch signal {
	case SignalLogs:
		return field == "timestamp"
	case SignalMetrics:
		return field == "timestamp" || field == "value"
	case SignalTraces:
		return field == "timestamp" || field == "end_time"
	}
	return false
}

func aggregateNumericValue(signal Signal, field string, record any) (any, bool) {
	switch signal {
	case SignalLogs:
		if field == "timestamp" {
			return record.(LogRecord).Timestamp.UnixNano(), true
		}
	case SignalMetrics:
		switch field {
		case "timestamp":
			return record.(MetricRecord).Timestamp.UnixNano(), true
		case "value":
			return record.(MetricRecord).Value, true
		}
	case SignalTraces:
		switch field {
		case "timestamp":
			return record.(SpanRecord).Timestamp.UnixNano(), true
		case "end_time":
			return record.(SpanRecord).EndTime.UnixNano(), true
		}
	}
	return nil, false
}

func addNumbers(left, right any) any {
	switch l := left.(type) {
	case nil:
		return right
	case int64:
		switch r := right.(type) {
		case int64:
			return l + r
		case float64:
			return float64(l) + r
		}
	case float64:
		switch r := right.(type) {
		case int64:
			return l + float64(r)
		case float64:
			return l + r
		}
	}
	return left
}

func compareNumbers(left, right any) int {
	switch l := left.(type) {
	case int64:
		switch r := right.(type) {
		case int64:
			switch {
			case l < r:
				return -1
			case l > r:
				return 1
			}
			return 0
		case float64:
			return compareFloat(float64(l), r)
		}
	case float64:
		switch r := right.(type) {
		case int64:
			return compareFloat(l, float64(r))
		case float64:
			return compareFloat(l, r)
		}
	}
	return 0
}

func compareFloat(left, right float64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
