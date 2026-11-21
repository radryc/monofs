package router

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/sqlfederation"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

type fakeRouteResolver struct {
	counts map[string]int64
}

func (f fakeRouteResolver) Resolve(_ context.Context, routed sqlfederation.RoutedEndpoint) (sqlag.Backend, error) {
	return &fakeRouteBackend{id: routed.Endpoint.ID, count: f.counts[routed.Shard.Name]}, nil
}

type fakeRouteBackend struct {
	id    string
	count int64
}

func (f *fakeRouteBackend) ID() string { return f.id }

func (f *fakeRouteBackend) Query(context.Context, sqlag.PartialRequest) (*sqlag.Stream, error) {
	return &sqlag.Stream{
		EndpointID: f.id,
		Schema:     sqlag.Schema{Columns: []string{"k0", "p0"}},
		Rows:       []sqlag.Row{{sqlag.Str("api"), sqlag.Int64(f.count)}},
	}, nil
}

func TestQuerySQLUsesPolicyShardRouting(t *testing.T) {
	access, err := policy.ParseFile([]byte(`
version: 1
defaults: {partitions: deny, logs: deny, traces: deny, sql: deny}
users:
  analyst:
    partitions: [observability]
    logs: [observability]
    sql: [observability]
`))
	if err != nil {
		t.Fatalf("parse access: %v", err)
	}
	column := &policy.PartitionBinding{Kind: "column", Column: "partition"}
	partition := policy.PartitionPolicy{
		Name:       "observability",
		Attributes: map[string]string{"role": "observability"},
		Shards: []policy.Shard{
			{Name: "k8s", Signal: policy.SignalLogs, Platform: "kubernetes",
				Placements: []policy.Placement{{ID: "ch", Backend: "clickhouse", Source: "strata.logs", Partition: column}}},
			{Name: "aws-a", Signal: policy.SignalLogs, Platform: "aws", Account: "111", Region: "us-east-1",
				Placements: []policy.Placement{{ID: "omni", Backend: "omni", Source: "logs.default", Partition: column}}},
			{Name: "aws-b", Signal: policy.SignalLogs, Platform: "aws", Account: "222", Region: "us-west-2",
				Placements: []policy.Placement{{ID: "ch", Backend: "clickhouse", Source: "strata.logs", Partition: column}}},
			{Name: "gcp", Signal: policy.SignalLogs, Platform: "gcp", Project: "p",
				Placements: []policy.Placement{{ID: "bq", Backend: "bigquery", Source: "p.d.logs", Partition: column}}},
		},
	}
	bundle, issues, err := policy.CompileWithPartitions(access, nil, []policy.PartitionPolicy{partition}, time.Now())
	if err != nil {
		t.Fatalf("compile: %v (%#v)", err, issues)
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	loader := policy.NewLoader(policy.FileSource{Path: path}, time.Hour, nil)
	if err := loader.Refresh(context.Background()); err != nil {
		t.Fatalf("load bundle: %v", err)
	}

	r := NewRouter(DefaultRouterConfig(), nil)
	defer r.Close()
	r.accessPolicyMu.Lock()
	r.accessPolicy = &accessPolicyState{loader: loader}
	r.accessPolicyMu.Unlock()
	r.SetPlacementBackendResolver(fakeRouteResolver{counts: map[string]int64{
		"k8s": 2, "aws-a": 3, "aws-b": 5, "gcp": 7,
	}})

	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "analyst"})
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	resp, err := r.QuerySQL(ctx, &pb.QuerySQLRequest{
		Dataset:      "logs",
		Query:        `SELECT service, count(*) AS n FROM logs GROUP BY service`,
		FromUnixNano: from.UnixNano(),
		ToUnixNano:   from.Add(24 * time.Hour).UnixNano(),
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("QuerySQL: %v", err)
	}
	var result struct {
		Rows     [][]any `json:"rows"`
		RowCount int     `json:"row_count"`
		Coverage struct {
			Complete bool `json:"complete"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(resp.GetResultsJson()), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.RowCount != 1 || result.Rows[0][1].(float64) != 17 {
		t.Fatalf("merged rows = %#v", result.Rows)
	}
	if !result.Coverage.Complete {
		t.Fatalf("coverage = %#v", result.Coverage)
	}
}
