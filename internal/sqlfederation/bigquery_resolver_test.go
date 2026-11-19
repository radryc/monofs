package sqlfederation

import (
	"context"
	"testing"

	"github.com/radryc/monofs/pkg/authz/policy"
	bigquerydriver "github.com/radryc/sqlagregator/drivers/bigquery"
	insightsdriver "github.com/radryc/sqlagregator/drivers/cloudwatchlogsinsights"
	omnidriver "github.com/radryc/sqlagregator/drivers/omni"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

func TestBigQueryResolverDerivesProjectAndCaches(t *testing.T) {
	created := 0
	var got bigquerydriver.Config
	resolver := NewBigQueryResolver(BigQueryResolverConfig{
		Location: "US",
		MaxRows:  1000,
	})
	resolver.newDriver = func(_ context.Context, _ RoutedEndpoint, cfg bigquerydriver.Config) (sqlag.Backend, error) {
		created++
		got = cfg
		return &fakeSQLBackend{id: cfg.EndpointID}, nil
	}
	routed := RoutedEndpoint{
		Endpoint: &sqlag.Endpoint{
			ID:      "diva/logs/gcp/bigquery",
			Backend: "bigquery",
			Source:  "strata-gcp-dev.strata_logs_diva.run_googleapis_com_stdout",
		},
		Partition: policy.PartitionPolicy{Name: "diva-shared"},
	}
	first, err := resolver.Resolve(context.Background(), routed)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := resolver.Resolve(context.Background(), routed)
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if first != second || created != 1 {
		t.Fatalf("not cached: created=%d", created)
	}
	if got.ProjectID != "strata-gcp-dev" {
		t.Fatalf("project = %q", got.ProjectID)
	}
	if got.MaxRows != 1000 || got.Location != "US" {
		t.Fatalf("cfg = %#v", got)
	}
	if _, err := resolver.Resolve(context.Background(), RoutedEndpoint{
		Endpoint: &sqlag.Endpoint{ID: "x", Backend: "omni"},
	}); err == nil {
		t.Fatal("non-bigquery backend accepted")
	}
}

func TestPlacementAssumeRoleReachesResolvers(t *testing.T) {
	var omniRole, insightsRole string
	omni := NewOmniResolver(OmniResolverConfig{})
	omni.newDriver = func(_ context.Context, _ RoutedEndpoint, cfg omnidriver.Config) (sqlag.Backend, error) {
		omniRole = cfg.AssumeRoleARN
		return &fakeSQLBackend{id: cfg.EndpointID}, nil
	}
	insights := NewInsightsResolver(InsightsResolverConfig{})
	insights.newDriver = func(_ context.Context, _ RoutedEndpoint, cfg insightsdriver.Config) (sqlag.Backend, error) {
		insightsRole = cfg.AssumeRoleARN
		return &fakeSQLBackend{id: cfg.EndpointID}, nil
	}
	routedOmni := RoutedEndpoint{
		Endpoint:  &sqlag.Endpoint{ID: "a/logs/aws/omni", Backend: "omni"},
		Partition: policy.PartitionPolicy{Name: "a"},
		Placement: policy.Placement{ID: "omni", Backend: "omni", AssumeRole: "arn:aws:iam::111:role/r"},
	}
	routedInsights := RoutedEndpoint{
		Endpoint:  &sqlag.Endpoint{ID: "b/logs/aws/insights", Backend: "cloudwatch_logs_insights"},
		Partition: policy.PartitionPolicy{Name: "b"},
		Placement: policy.Placement{ID: "insights", Backend: "cloudwatch_logs_insights", AssumeRole: "arn:aws:iam::222:role/r"},
	}
	if _, err := omni.Resolve(context.Background(), routedOmni); err != nil {
		t.Fatal(err)
	}
	if _, err := insights.Resolve(context.Background(), routedInsights); err != nil {
		t.Fatal(err)
	}
	if omniRole != "arn:aws:iam::111:role/r" {
		t.Fatalf("omni role = %q", omniRole)
	}
	if insightsRole != "arn:aws:iam::222:role/r" {
		t.Fatalf("insights role = %q", insightsRole)
	}
}

func TestLocalEndpointMapsTimestampToViewColumn(t *testing.T) {
	shard := policy.Shard{Name: "k8s", Signal: policy.SignalLogs, Platform: "kubernetes",
		Placements: []policy.Placement{{ID: "monofs", Backend: "monofs_local", Source: "logs"}}}
	partition := policy.PartitionPolicy{Name: "p", Shards: []policy.Shard{shard}}
	ep := buildEndpoint("logs", partition, shard, shard.Placements[0], map[string]string{})
	if ep.Columns["timestamp"] != "time" {
		t.Fatalf("timestamp mapping = %q, want time (%#v)", ep.Columns["timestamp"], ep.Columns)
	}
	if ep.Columns["timestamp_ns"] != "timestamp" {
		t.Fatalf("timestamp_ns mapping = %q", ep.Columns["timestamp_ns"])
	}
}
