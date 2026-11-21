// Package sqlfederation adapts MonoFS nodes to the public sqlag coordinator
// API. It builds a per-dataset registry from the router's healthy node set and
// executes generated pushdowns through the internal QueryPartial RPC.
package sqlfederation

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

// Node is one healthy shard endpoint.
type Node struct {
	ID      string
	Address string
	Client  pb.MonoFSClient
}

// DefaultPartialLimit bounds one shard's partial result.
const DefaultPartialLimit = 200_000

type columnDef struct {
	name     string
	typ      sqlag.Type
	physical string // empty means the logical name
}

var datasetSchemas = map[string][]columnDef{
	"logs": {
		{"timestamp", sqlag.TypeTimestamp, "time"},
		{"timestamp_ns", sqlag.TypeInt64, "timestamp"},
		{"level", sqlag.TypeString, ""},
		{"service", sqlag.TypeString, ""},
		{"trace_id", sqlag.TypeString, ""},
		{"span_id", sqlag.TypeString, ""},
		{"raw_message", sqlag.TypeString, ""},
		{"labels_json", sqlag.TypeString, ""},
		{"partition", sqlag.TypeString, ""},
	},
	"metrics": {
		{"timestamp", sqlag.TypeTimestamp, "time"},
		{"timestamp_ns", sqlag.TypeInt64, "timestamp"},
		{"start_time", sqlag.TypeInt64, ""},
		{"service", sqlag.TypeString, ""},
		{"metric_name", sqlag.TypeString, ""},
		{"value", sqlag.TypeFloat64, ""},
		{"metric_type", sqlag.TypeString, ""},
		{"temporality", sqlag.TypeString, ""},
		{"labels_json", sqlag.TypeString, ""},
		{"partition", sqlag.TypeString, ""},
	},
	"traces": {
		{"timestamp", sqlag.TypeTimestamp, "time"},
		{"timestamp_ns", sqlag.TypeInt64, "timestamp"},
		{"end_time", sqlag.TypeInt64, ""},
		{"end_time_ts", sqlag.TypeTimestamp, ""},
		{"trace_id", sqlag.TypeString, ""},
		{"span_id", sqlag.TypeString, ""},
		{"parent_span_id", sqlag.TypeString, ""},
		{"service", sqlag.TypeString, ""},
		{"name", sqlag.TypeString, ""},
		{"status_code", sqlag.TypeString, ""},
		{"attributes_json", sqlag.TypeString, ""},
		{"partition", sqlag.TypeString, ""},
	},
}

// BuildCatalog creates a registry where every healthy node is a disjoint shard
// of the dataset view. expectedMembers is the number of shards the cluster is
// supposed to have; a smaller healthy set produces a coverage gap instead of a
// silently low aggregate.
func BuildCatalog(dataset string, nodes []Node, expectedMembers int) (*sqlag.Catalog, error) {
	dataset = strings.ToLower(strings.TrimSpace(dataset))
	defs, ok := datasetSchemas[dataset]
	if !ok {
		return nil, fmt.Errorf("unsupported dataset %q", dataset)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no healthy nodes")
	}
	if expectedMembers < len(nodes) {
		expectedMembers = len(nodes)
	}

	columns := make([]sqlag.Column, len(defs))
	physical := make(map[string]string, len(defs))
	for i, def := range defs {
		columns[i] = sqlag.Column{Name: def.name, Type: def.typ}
		if def.physical != "" {
			physical[def.name] = def.physical
		}
	}
	table := &sqlag.Table{
		Name:       dataset,
		Signal:     dataset,
		TimeColumn: "timestamp",
		Columns:    columns,
	}
	endpoints := make([]*sqlag.Endpoint, 0, len(nodes))
	for _, node := range nodes {
		cols := make(map[string]string, len(physical))
		for logical, phys := range physical {
			cols[logical] = phys
		}
		endpoints = append(endpoints, &sqlag.Endpoint{
			ID:            node.ID,
			Backend:       "duckdb-local",
			Tables:        []string{dataset},
			Role:          sqlag.RoleShard,
			CoverageGroup: "monofs-local",
			Source:        dataset,
			Tags:          map[string]string{"node": node.ID},
			Columns:       cols,
			Caps: sqlag.Capabilities{
				Signals:                []string{dataset},
				EnforcesPartitionScope: true,
			},
		})
	}
	groups := map[string]*sqlag.CoverageGroup{
		"monofs-local": {Mode: "hash", Members: expectedMembers, AllowMissing: true},
	}
	backends := map[string]*sqlag.BackendDefaults{
		"duckdb-local": {Signals: []string{dataset}, EnforcesPartitionScope: true},
	}
	return sqlag.NewCatalog(
		map[string]*sqlag.Table{dataset: table},
		endpoints,
		groups,
		backends,
		sqlag.Defaults{},
	)
}

// NodeBackend executes a pushdown on one MonoFS node.
type NodeBackend struct {
	Node  Node
	Limit int
}

func (b *NodeBackend) ID() string { return b.Node.ID }

func (b *NodeBackend) Query(ctx context.Context, req sqlag.PartialRequest) (*sqlag.Stream, error) {
	limit := b.Limit
	if limit <= 0 {
		limit = DefaultPartialLimit
	}
	response, err := b.Node.Client.QueryPartial(ctx, &pb.QueryPartialRequest{
		Dataset:              req.Dataset,
		Sql:                  req.SQL,
		FromUnixNano:         unixNano(req.From),
		ToUnixNano:           unixNano(req.To),
		Limit:                int32(limit),
		VisiblePartitions:    req.Scope.Partitions,
		VisiblePartitionsSet: req.Scope.Restricted,
		ExpectedColumns:      req.Schema,
	})
	if err != nil {
		return nil, err
	}
	return decodePartial(b.Node.ID, response.GetResultsJson(), req.Schema)
}

// ConvertSelector maps the protobuf selector to the coordinator selector.
func ConvertSelector(in *pb.BackendSelector) sqlag.Selector {
	if in == nil {
		return sqlag.Selector{}
	}
	out := sqlag.Selector{
		IncludeReplicas: in.GetIncludeReplicas(),
		Max:             int(in.GetMax()),
		MinCoverage:     in.GetMinCoverage(),
	}
	for _, m := range in.GetInclude() {
		out.Include = append(out.Include, convertMatcher(m))
	}
	for _, m := range in.GetExclude() {
		out.Exclude = append(out.Exclude, convertMatcher(m))
	}
	for _, m := range in.GetTreatAsReplica() {
		out.TreatAsReplica = append(out.TreatAsReplica, convertMatcher(m))
	}
	return out
}

func convertMatcher(in *pb.BackendMatcher) sqlag.Matcher {
	if in == nil {
		return sqlag.Matcher{}
	}
	return sqlag.Matcher{
		IDs:      append([]string(nil), in.GetIds()...),
		Backends: append([]string(nil), in.GetBackends()...),
		Tags:     in.GetTags(),
		Roles:    append([]string(nil), in.GetRoles()...),
		Groups:   append([]string(nil), in.GetGroups()...),
	}
}

type partialResult struct {
	Columns   []string `json:"columns"`
	Types     []string `json:"types"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

func decodePartial(endpointID, payload string, expected []string) (*sqlag.Stream, error) {
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	var result partialResult
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode partial from %s: %w", endpointID, err)
	}
	if result.Truncated {
		return nil, fmt.Errorf("partial result from %s was truncated", endpointID)
	}
	present := make(map[string]int, len(result.Columns))
	for i, name := range result.Columns {
		present[name] = i
	}
	for _, name := range expected {
		if _, ok := present[name]; !ok {
			return nil, fmt.Errorf("partial result from %s is missing column %q", endpointID, name)
		}
	}
	rows := make([]sqlag.Row, 0, len(result.Rows))
	for _, raw := range result.Rows {
		row := make(sqlag.Row, len(result.Columns))
		for i, column := range result.Columns {
			typ := ""
			if i < len(result.Types) {
				typ = result.Types[i]
			}
			v, err := decodeValue(raw[i], typ)
			if err != nil {
				return nil, fmt.Errorf("partial %s column %q (%s): %w", endpointID, column, typ, err)
			}
			row[i] = v
		}
		rows = append(rows, row)
	}
	return &sqlag.Stream{
		EndpointID: endpointID,
		Schema:     sqlag.Schema{Columns: result.Columns},
		Rows:       rows,
	}, nil
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func decodeValue(raw any, typ string) (sqlag.Value, error) {
	if raw == nil {
		return sqlag.Null(), nil
	}
	t := normalizeType(typ)
	switch {
	case t == "BIGINT" || t == "INTEGER" || t == "INT" || t == "INT64" ||
		t == "SMALLINT" || t == "TINYINT" || t == "INT32" || t == "INT16" || t == "INT8":
		i, err := parseInt64(raw)
		if err != nil {
			return sqlag.Value{}, err
		}
		return sqlag.Int64(i), nil
	case t == "UBIGINT" || t == "UINTEGER" || t == "UINT64" || t == "UINT32" ||
		t == "USMALLINT" || t == "UTINYINT" || t == "UINT16" || t == "UINT8":
		u, err := parseUint64(raw)
		if err != nil {
			return sqlag.Value{}, err
		}
		return sqlag.Uint64(u), nil
	case t == "DOUBLE" || t == "FLOAT" || t == "FLOAT64" || t == "FLOAT32" || t == "REAL":
		f, err := parseFloat64(raw)
		if err != nil {
			return sqlag.Value{}, err
		}
		return sqlag.Float64(f), nil
	case strings.HasPrefix(t, "DECIMAL") || t == "NUMERIC" || t == "HUGEINT" || t == "UHUGEINT":
		s, err := numericText(raw)
		if err != nil {
			return sqlag.Value{}, err
		}
		d, ok := sqlag.Decimal(s)
		if !ok {
			return sqlag.Value{}, fmt.Errorf("invalid decimal %q", s)
		}
		return d, nil
	case t == "BOOLEAN" || t == "BOOL":
		b, err := parseBool(raw)
		if err != nil {
			return sqlag.Value{}, err
		}
		return sqlag.Boolean(b), nil
	case strings.HasPrefix(t, "TIMESTAMP") || strings.HasPrefix(t, "DATETIME"):
		s, ok := raw.(string)
		if !ok {
			return sqlag.Value{}, fmt.Errorf("expected timestamp string, got %T", raw)
		}
		parsed, err := parseTimestamp(s)
		if err != nil {
			return sqlag.Value{}, err
		}
		return sqlag.Timestamp(parsed), nil
	case t == "DATE" || t == "DATE32":
		s, ok := raw.(string)
		if !ok {
			return sqlag.Value{}, fmt.Errorf("expected date string, got %T", raw)
		}
		parsed, err := time.Parse("2006-01-02", s)
		if err != nil {
			return sqlag.Value{}, err
		}
		return sqlag.Timestamp(parsed), nil
	case t == "VARCHAR" || t == "STRING" || t == "TEXT" || t == "JSON" || t == "UUID" ||
		t == "BLOB" || strings.HasPrefix(t, "FIXEDSTRING") || strings.HasPrefix(t, "ENUM"):
		return sqlag.Str(valueText(raw)), nil
	case t == "":
		return inferValue(raw)
	default:
		return inferValue(raw)
	}
}

func normalizeType(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	// Nullable(...), LowCardinality(...)
	for {
		switch {
		case strings.HasPrefix(t, "NULLABLE(") && strings.HasSuffix(t, ")"):
			t = strings.TrimSuffix(strings.TrimPrefix(t, "NULLABLE("), ")")
		case strings.HasPrefix(t, "LOWCARDINALITY(") && strings.HasSuffix(t, ")"):
			t = strings.TrimSuffix(strings.TrimPrefix(t, "LOWCARDINALITY("), ")")
		default:
			return t
		}
	}
}

func inferValue(raw any) (sqlag.Value, error) {
	switch v := raw.(type) {
	case json.Number:
		s := v.String()
		if strings.ContainsAny(s, ".eE") {
			f, err := v.Float64()
			if err != nil {
				return sqlag.Value{}, err
			}
			return sqlag.Float64(f), nil
		}
		i, err := v.Int64()
		if err != nil {
			if d, ok := sqlag.Decimal(s); ok {
				return d, nil
			}
			return sqlag.Value{}, err
		}
		return sqlag.Int64(i), nil
	case string:
		return sqlag.Str(v), nil
	case bool:
		return sqlag.Boolean(v), nil
	case float64:
		return sqlag.Float64(v), nil
	default:
		return sqlag.Str(fmt.Sprint(v)), nil
	}
}

func parseInt64(raw any) (int64, error) {
	switch v := raw.(type) {
	case json.Number:
		return v.Int64()
	case string:
		return strconv.ParseInt(v, 10, 64)
	case float64:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("expected integer, got %T", raw)
	}
}

func parseUint64(raw any) (uint64, error) {
	switch v := raw.(type) {
	case json.Number:
		return strconv.ParseUint(v.String(), 10, 64)
	case string:
		return strconv.ParseUint(v, 10, 64)
	case float64:
		return uint64(v), nil
	default:
		return 0, fmt.Errorf("expected unsigned integer, got %T", raw)
	}
}

func parseFloat64(raw any) (float64, error) {
	switch v := raw.(type) {
	case json.Number:
		return v.Float64()
	case string:
		return strconv.ParseFloat(v, 64)
	case float64:
		return v, nil
	default:
		return 0, fmt.Errorf("expected float, got %T", raw)
	}
}

func parseBool(raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(v) {
		case "true", "1":
			return true, nil
		case "false", "0":
			return false, nil
		}
	case json.Number:
		return v.String() != "0", nil
	}
	return false, fmt.Errorf("expected bool, got %T", raw)
}

func numericText(raw any) (string, error) {
	switch v := raw.(type) {
	case json.Number:
		return v.String(), nil
	case string:
		return v, nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("expected numeric text, got %T", raw)
	}
}

func valueText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprint(v)
	}
}

func parseTimestamp(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", s)
}
