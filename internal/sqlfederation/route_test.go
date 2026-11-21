package sqlfederation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

func testPartition(insights bool) policy.PartitionPolicy {
	column := &policy.PartitionBinding{Kind: "column", Column: "partition"}
	logGroup := &policy.PartitionBinding{Kind: "log_group", LogGroupField: "@logGroupName"}
	attribute := &policy.PartitionBinding{Kind: "attribute", Path: []string{"attributes", "guardian.partition"}}
	accountRegion := &policy.PartitionBinding{Kind: "account_region", AccountField: "@aws.account", RegionField: "@aws.region"}

	awsBPlacements := []policy.Placement{{
		ID: "insights", Backend: "cloudwatch_logs_insights", LogGroups: []string{"/strata/observability"},
		Partition: logGroup,
	}}
	if !insights {
		awsBPlacements = []policy.Placement{{
			ID: "clickhouse", Backend: "clickhouse", Source: "strata.logs", Partition: column,
		}}
	}
	gcpBackend := "bigquery"
	gcpBinding := column
	if insights {
		gcpBackend = "bigquery"
		gcpBinding = column
	}
	_ = accountRegion
	return policy.PartitionPolicy{
		Name:       "observability",
		Attributes: map[string]string{"role": "observability"},
		Shards: []policy.Shard{
			{
				Name: "k8s", Signal: policy.SignalLogs, Platform: "kubernetes",
				Attributes: map[string]string{"region": "eu-west-1"},
				Placements: []policy.Placement{{
					ID: "monofs", Backend: "monofs_local", Source: "logs",
					Columns: map[string]string{"timestamp": "time"}, Partition: column,
				}},
			},
			{
				Name: "aws-a", Signal: policy.SignalLogs, Platform: "aws",
				Account: "111111111111", Region: "us-east-1",
				Attributes: map[string]string{"region": "us-east-1"},
				Placements: []policy.Placement{
					{
						ID: "omni", Backend: "omni", Source: "logs.default",
						LogGroups: []string{"/strata/observability"}, Partition: attribute,
					},
					{
						ID: "insights", Backend: "cloudwatch_logs_insights",
						LogGroups: []string{"/strata/observability"}, ReplicaOf: "omni", Partition: logGroup,
					},
				},
			},
			{
				Name: "aws-b", Signal: policy.SignalLogs, Platform: "aws",
				Account: "222222222222", Region: "us-west-2",
				Attributes: map[string]string{"region": "us-west-2"},
				Placements: awsBPlacements,
			},
			{
				Name: "gcp", Signal: policy.SignalLogs, Platform: "gcp",
				Project: "strata-prod", Attributes: map[string]string{"region": "europe-west1"},
				Placements: []policy.Placement{{
					ID: "bq", Backend: gcpBackend, Source: "strata_prod.telemetry.logs", Partition: gcpBinding,
				}},
			},
		},
	}
}

func compileRoutePolicy(t *testing.T, shardYAML string) *policy.Policy {
	t.Helper()
	file, err := policy.ParseFile([]byte(fmt.Sprintf(`
version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
  sql: deny
users:
  analyst:
    partitions: [observability]
    logs: [observability]
    sql: [observability]
%s
`, shardYAML)))
	if err != nil {
		t.Fatalf("parse access: %v", err)
	}
	bundle, issues, err := policy.CompileWithPartitions(file, nil, []policy.PartitionPolicy{testPartition(true)}, time.Now())
	if err != nil {
		t.Fatalf("compile: %v (issues %#v)", err, issues)
	}
	return policy.New(*bundle)
}

func routeIdentity() authz.Identity { return authz.Identity{Subject: "analyst"} }

func TestRouteBuildsOneEndpointPerShard(t *testing.T) {
	pol := compileRoutePolicy(t, "")
	plan, err := Route(pol, routeIdentity(), RouteRequest{Dataset: "logs", Visible: []string{"observability"}})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if len(plan.Endpoints) != 4 {
		t.Fatalf("endpoints = %d, want 4", len(plan.Endpoints))
	}
	if group := plan.Coverage["observability/logs"]; group == nil || group.Members != 4 {
		t.Fatalf("coverage group = %#v", group)
	}
	byShard := map[string]RoutedEndpoint{}
	for _, routed := range plan.Endpoints {
		byShard[routed.Shard.Name] = routed
	}
	if got := byShard["aws-a"].Placement.ID; got != "omni" {
		t.Fatalf("aws-a primary = %q, want omni", got)
	}
	if got := byShard["aws-a"].Endpoint.Binding; got == nil || got.Account != "111111111111" {
		t.Fatalf("aws-a binding = %#v", got)
	}
	if got := byShard["aws-b"].Endpoint.Binding; got == nil || got.Account != "222222222222" {
		t.Fatalf("aws-b binding = %#v", got)
	}
	if got := byShard["aws-b"].Endpoint.Backend; got != "cloudwatch_logs_insights" {
		t.Fatalf("aws-b backend = %q", got)
	}
	if got := byShard["gcp"].Endpoint.Backend; got != "bigquery" {
		t.Fatalf("gcp backend = %q", got)
	}
}

func TestRouteShardSelectorsNarrowFanout(t *testing.T) {
	pol := compileRoutePolicy(t, `
    shards:
      include:
        - attributes: {region: us-east-1}
`)
	plan, err := Route(pol, routeIdentity(), RouteRequest{Dataset: "logs", Visible: []string{"observability"}})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if len(plan.Endpoints) != 1 || plan.Endpoints[0].Shard.Name != "aws-a" {
		t.Fatalf("endpoints = %#v", plan.Endpoints)
	}
	if group := plan.Coverage["observability/logs"]; group == nil || group.Members != 1 {
		t.Fatalf("coverage = %#v", plan.Coverage)
	}
	if len(plan.MissingShards) != 0 {
		t.Fatalf("out-of-scope shards must not be gaps: %#v", plan.MissingShards)
	}
}

func TestRouteBackendAllowlistAndReplicaFallback(t *testing.T) {
	pol := compileRoutePolicy(t, "")
	plan, err := Route(pol, routeIdentity(), RouteRequest{
		Dataset: "logs", Visible: []string{"observability"}, AllowedBackends: []string{"omni"},
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if len(plan.Endpoints) != 1 || plan.Endpoints[0].Placement.Backend != "omni" {
		t.Fatalf("endpoints = %#v", plan.Endpoints)
	}
	if len(plan.MissingShards) != 3 {
		t.Fatalf("missing = %#v, want 3", plan.MissingShards)
	}
	if group := plan.Coverage["observability/logs"]; group == nil || group.Members != 4 {
		t.Fatalf("coverage = %#v", plan.Coverage)
	}

	plan, err = Route(pol, routeIdentity(), RouteRequest{
		Dataset: "logs", Visible: []string{"observability"}, AllowedBackends: []string{"cloudwatch_logs_insights"},
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if len(plan.Endpoints) != 2 {
		t.Fatalf("endpoints = %#v, want aws-a replica + aws-b", plan.Endpoints)
	}
	var sawReplicaWarning bool
	for _, warning := range plan.Warnings {
		if strings.Contains(warning, "using replica") {
			sawReplicaWarning = true
		}
	}
	if !sawReplicaWarning {
		t.Fatalf("expected replica warning: %#v", plan.Warnings)
	}
}

type fakeSQLBackend struct {
	id     string
	counts int64
}

func (f *fakeSQLBackend) ID() string { return f.id }

func (f *fakeSQLBackend) Query(context.Context, sqlag.PartialRequest) (*sqlag.Stream, error) {
	return &sqlag.Stream{
		EndpointID: f.id,
		Schema:     sqlag.Schema{Columns: []string{"k0", "p0"}},
		Rows:       []sqlag.Row{{sqlag.Str("api"), sqlag.Int64(f.counts)}},
	}, nil
}

func TestRoutePlanMergesMixedBackends(t *testing.T) {
	pol := compileRoutePolicyMerge(t)
	plan, err := Route(pol, routeIdentity(), RouteRequest{Dataset: "logs", Visible: []string{"observability"}})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if len(plan.Endpoints) != 4 {
		t.Fatalf("endpoints = %d", len(plan.Endpoints))
	}
	cat, err := plan.Catalog("logs")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	coord := sqlag.New(cat)
	counts := []int64{2, 3, 5, 7}
	for i, routed := range plan.Endpoints {
		coord.Register(&fakeSQLBackend{id: routed.Endpoint.ID, counts: counts[i]})
	}
	res, err := coord.Query(context.Background(), sqlag.Request{
		SQL:  `SELECT service, count(*) AS n FROM logs GROUP BY service`,
		From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.RowCount != 1 || res.Rows[0][1].(int64) != 17 {
		t.Fatalf("merged rows = %#v", res.Rows)
	}
}

func compileRoutePolicyMerge(t *testing.T) *policy.Policy {
	t.Helper()
	file, err := policy.ParseFile([]byte(`
version: 1
defaults: {partitions: deny, logs: deny, traces: deny, sql: deny}
users:
  analyst:
    partitions: [observability]
    logs: [observability]
    sql: [observability]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
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
	bundle, issues, err := policy.CompileWithPartitions(file, nil, []policy.PartitionPolicy{partition}, time.Now())
	if err != nil {
		t.Fatalf("compile: %v (%#v)", err, issues)
	}
	return policy.New(*bundle)
}
