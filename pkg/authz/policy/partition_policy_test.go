package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

const testPartitionDefaultsYAML = `
attributes:
  environment: prod
quota:
  profile: medium
shard_templates:
  aws-logs:
    platform: aws
    signal: logs
    placements:
      - id: insights
        backend: cloudwatch_logs_insights
        log_groups: ["/strata/observability"]
        partition_binding:
          kind: log_group
`

const testPartitionYAML = `
name: observability
attributes:
  role: observability
shards:
  - name: aws-a
    template: aws-logs
    account: "111111111111"
    region: us-east-1
    attributes:
      region: us-east-1
  - name: k8s-eu
    signal: logs
    platform: kubernetes
    placements:
      - id: monofs
        backend: monofs_local
        source: logs
        partition_binding:
          kind: column
          column: partition
`

const testAccessWithSQLYAML = `
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
    backends: [omni, monofs_local]
    shards:
      include:
        - attributes: {region: us-east-1}
      exclude:
        - attributes: {account: "222222222222"}
`

func TestPartitionDefaultsAndTemplate(t *testing.T) {
	defaults, err := ParsePartitionDefaults([]byte(testPartitionDefaultsYAML))
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	partition, err := ParsePartitionPolicy([]byte(testPartitionYAML))
	if err != nil {
		t.Fatalf("parse partition: %v", err)
	}
	applied, err := defaults.Apply(partition)
	if err != nil {
		t.Fatalf("apply defaults: %v", err)
	}
	if applied.Attributes["environment"] != "prod" || applied.Attributes["role"] != "observability" {
		t.Fatalf("attributes not merged: %#v", applied.Attributes)
	}
	if applied.Quota == nil || applied.Quota.Profile != "medium" {
		t.Fatalf("quota not merged: %#v", applied.Quota)
	}
	if len(applied.Shards) != 2 {
		t.Fatalf("shards = %d", len(applied.Shards))
	}
	awsShard := applied.Shards[0]
	if awsShard.Platform != "aws" || awsShard.Signal != SignalLogs {
		t.Fatalf("template not applied: %#v", awsShard)
	}
	if awsShard.Template != "" {
		t.Fatalf("template should be cleared: %q", awsShard.Template)
	}
	if len(awsShard.Placements) != 1 || awsShard.Placements[0].Backend != "cloudwatch_logs_insights" {
		t.Fatalf("template placements not applied: %#v", awsShard.Placements)
	}
	// The input must not be mutated.
	if partition.Shards[0].Platform != "" || len(partition.Shards[0].Placements) != 0 {
		t.Fatalf("Apply mutated the input: %#v", partition.Shards[0])
	}
	if issues := applied.Validate(); hasPolicyError(issues) {
		t.Fatalf("applied partition has errors: %#v", issues)
	}
}

func TestPartitionValidation(t *testing.T) {
	base := PartitionPolicy{
		Name: "observability",
		Shards: []Shard{{
			Name:     "aws",
			Signal:   SignalLogs,
			Platform: "aws",
			Account:  "111111111111",
			Region:   "us-east-1",
			Placements: []Placement{{
				ID:      "omni",
				Backend: "omni",
				Partition: &PartitionBinding{
					Kind: "attribute",
					Path: []string{"attributes", "guardian.partition"},
				},
			}},
		}},
	}
	if issues := base.Validate(); hasPolicyError(issues) {
		t.Fatalf("base partition errors: %#v", issues)
	}

	cases := []struct {
		name   string
		mutate func(*PartitionPolicy)
		want   string
	}{
		{"missing primary", func(p *PartitionPolicy) {
			p.Shards[0].Placements[0].ReplicaOf = "omni"
		}, "primary"},
		{"two primaries", func(p *PartitionPolicy) {
			p.Shards[0].Placements = append(p.Shards[0].Placements, Placement{ID: "second", Backend: "clickhouse"})
		}, "multiple non-replica"},
		{"unknown replica", func(p *PartitionPolicy) {
			p.Shards[0].Placements[0].ReplicaOf = "missing"
		}, "not a primary"},
		{"fixed without values", func(p *PartitionPolicy) {
			p.Shards[0].Placements[0].Partition = &PartitionBinding{Kind: "fixed"}
		}, "values are required"},
		{"attribute without path", func(p *PartitionPolicy) {
			p.Shards[0].Placements[0].Partition = &PartitionBinding{Kind: "attribute"}
		}, "path is required"},
		{"invalid signal", func(p *PartitionPolicy) {
			p.Shards[0].Signal = "events"
		}, "invalid signal"},
		{"duplicate placement", func(p *PartitionPolicy) {
			p.Shards[0].Placements = append(p.Shards[0].Placements, Placement{ID: "omni", Backend: "omni"})
		}, "duplicate placement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.Shards = append([]Shard(nil), base.Shards...)
			p.Shards[0].Placements = append([]Placement(nil), base.Shards[0].Placements...)
			placement := base.Shards[0].Placements[0]
			binding := *base.Shards[0].Placements[0].Partition
			placement.Partition = &binding
			p.Shards[0].Placements[0] = placement
			tc.mutate(&p)
			issues := p.Validate()
			if !hasPolicyErrorContaining(issues, tc.want) {
				t.Fatalf("issues = %#v, want error containing %q", issues, tc.want)
			}
		})
	}
}

func TestShardSelector(t *testing.T) {
	sel := &ShardSelector{
		Include: []AttributeMatcher{{Attributes: map[string]string{"region": "us-east-1"}}},
		Exclude: []AttributeMatcher{{Attributes: map[string]string{"account": "222"}}},
	}
	if !sel.Permits(map[string]string{"region": "us-east-1", "account": "111"}) {
		t.Fatal("expected permit")
	}
	if sel.Permits(map[string]string{"region": "us-west-2", "account": "111"}) {
		t.Fatal("expected include miss")
	}
	if sel.Permits(map[string]string{"region": "us-east-1", "account": "222"}) {
		t.Fatal("expected exclude")
	}
	var nilSel *ShardSelector
	if !nilSel.Permits(map[string]string{"region": "eu"}) {
		t.Fatal("nil selector must permit all")
	}
}

func TestCompileWithPartitionsAndSQLPolicy(t *testing.T) {
	file, err := ParseFile([]byte(testAccessWithSQLYAML))
	if err != nil {
		t.Fatalf("parse access: %v", err)
	}
	defaults, err := ParsePartitionDefaults([]byte(testPartitionDefaultsYAML))
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	raw, err := ParsePartitionPolicy([]byte(testPartitionYAML))
	if err != nil {
		t.Fatalf("parse partition: %v", err)
	}
	partition, err := defaults.Apply(raw)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	bundle, issues, err := CompileWithPartitions(file, nil, []PartitionPolicy{*partition}, time.Now())
	if err != nil {
		t.Fatalf("compile: %v (issues %#v)", err, issues)
	}
	if len(bundle.Partitions) != 1 || bundle.Partitions[0].Name != "observability" {
		t.Fatalf("partitions = %#v", bundle.Partitions)
	}
	if len(bundle.Grants) != 1 {
		t.Fatalf("grants = %#v", bundle.Grants)
	}
	grant := bundle.Grants[0]
	if len(grant.SQL) != 1 || grant.SQL[0] != "observability" {
		t.Fatalf("grant SQL = %#v", grant.SQL)
	}
	if len(grant.Backends) != 2 {
		t.Fatalf("grant backends = %#v", grant.Backends)
	}
	if grant.Shards == nil || len(grant.Shards.Include) != 1 || len(grant.Shards.Exclude) != 1 {
		t.Fatalf("grant shards = %#v", grant.Shards)
	}
	if bundle.Quotas == nil || !containsString(bundle.Quotas.KnownPartitions, "observability") {
		t.Fatalf("quota known partitions = %#v", bundle.Quotas)
	}

	p := New(*bundle)
	id := authz.Identity{Subject: "analyst"}
	if !p.CanReadSQL(id, "observability") {
		t.Fatal("expected SQL access")
	}
	backends, ok := p.AllowedBackends(id, "observability")
	if !ok || len(backends) != 2 {
		t.Fatalf("allowed backends = %#v ok=%v", backends, ok)
	}
	if !p.ShardAllowed(id, "observability", map[string]string{"region": "us-east-1", "account": "111"}) {
		t.Fatal("expected shard access")
	}
	if p.ShardAllowed(id, "observability", map[string]string{"region": "us-west-2", "account": "111"}) {
		t.Fatal("expected shard include miss")
	}
	if p.ShardAllowed(id, "observability", map[string]string{"region": "us-east-1", "account": "222222222222"}) {
		t.Fatal("expected shard exclude")
	}
	visible := p.ResolveVisibleSQLPartitions(id, []string{"observability", "other"})
	if len(visible) != 1 || visible[0] != "observability" {
		t.Fatalf("visible SQL partitions = %#v", visible)
	}
}

func TestQuotaSQLFields(t *testing.T) {
	q := QueryQuota{
		MaxShards:        4,
		MaxBackends:      2,
		MaxRows:          1000,
		MaxBytesScanned:  1 << 20,
		AllowedBackends:  []string{"omni"},
		AllowedSignals:   []string{"logs", "traces"},
		MaxQueriesPerDay: 100,
	}
	clone := cloneQuery(&q)
	clone.AllowedBackends[0] = "clickhouse"
	if q.AllowedBackends[0] != "omni" {
		t.Fatal("cloneQuery aliased the backends slice")
	}
	merged := MergeQuota(Quota{Query: &QueryQuota{MaxRows: 100}}, Quota{Query: &q})
	if merged.Query == nil || merged.Query.MaxRows != 1000 || merged.Query.MaxShards != 4 {
		t.Fatalf("merged quota = %#v", merged.Query)
	}
	if err := ValidateQuota(Quota{Query: &q}); err != nil {
		t.Fatalf("valid quota rejected: %v", err)
	}
	if err := ValidateQuota(Quota{Query: &QueryQuota{AllowedBackends: []string{"nope"}}}); err == nil {
		t.Fatal("unknown backend accepted")
	}
	if err := ValidateQuota(Quota{Query: &QueryQuota{AllowedSignals: []string{"events"}}}); err == nil {
		t.Fatal("unknown signal accepted")
	}
	if err := ValidateQuota(Quota{Query: &QueryQuota{MaxShards: -1}}); err == nil {
		t.Fatal("negative max shards accepted")
	}
}

func hasPolicyError(issues []Issue) bool {
	for _, issue := range issues {
		if issue.Severity == "error" {
			return true
		}
	}
	return false
}

func hasPolicyErrorContaining(issues []Issue, want string) bool {
	for _, issue := range issues {
		if issue.Severity == "error" && strings.Contains(issue.Message, want) {
			return true
		}
	}
	return false
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestOmniSpaceMayServeMultipleAccountsAndRegions(t *testing.T) {
	p := PartitionPolicy{
		Name: "observability",
		Shards: []Shard{
			{
				Name: "a", Signal: SignalLogs, Platform: "aws",
				Account: "111", Region: "us-east-1",
				Placements: []Placement{{
					ID: "omni", Backend: "omni", Source: "logs.shared",
					Partition: &PartitionBinding{Kind: "account_region", AccountField: "@aws.account", RegionField: "@aws.region"},
				}},
			},
			{
				Name: "b", Signal: SignalLogs, Platform: "aws",
				Account: "222", Region: "us-west-2",
				Placements: []Placement{{
					ID: "omni", Backend: "omni", Source: "logs.shared",
					Partition: &PartitionBinding{Kind: "account_region", AccountField: "@aws.account", RegionField: "@aws.region"},
				}},
			},
		},
	}
	if hasPolicyError(p.Validate()) {
		t.Fatalf("a shared Omni space across accounts/regions must be allowed: %#v", p.Validate())
	}
}
