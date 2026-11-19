package sqlfederation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz/policy"
	insightsdriver "github.com/radryc/sqlagregator/drivers/cloudwatchlogsinsights"
	omnidriver "github.com/radryc/sqlagregator/drivers/omni"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

func TestInsightsResolverCachesAndUsesShardRegion(t *testing.T) {
	created := 0
	var got insightsdriver.Config
	resolver := NewInsightsResolver(InsightsResolverConfig{
		DefaultRegion: "us-east-1",
		Timeout:       5 * time.Second,
		MaxRows:       500,
	})
	resolver.newDriver = func(_ context.Context, _ RoutedEndpoint, cfg insightsdriver.Config) (sqlag.Backend, error) {
		created++
		got = cfg
		return &fakeSQLBackend{id: cfg.EndpointID}, nil
	}
	routed := RoutedEndpoint{
		Endpoint:  &sqlag.Endpoint{ID: "observability/logs/aws-b/insights", Backend: "cloudwatch_logs_insights"},
		Shard:     policy.Shard{Region: "eu-west-1"},
		Partition: policy.PartitionPolicy{Name: "observability"},
	}
	first, err := resolver.Resolve(context.Background(), routed)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := resolver.Resolve(context.Background(), routed)
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if first != second {
		t.Fatal("expected cached driver")
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	if got.Region != "eu-west-1" {
		t.Fatalf("region = %q", got.Region)
	}
	if got.MaxRows != 500 {
		t.Fatalf("max rows = %d", got.MaxRows)
	}
	if _, err := resolver.Resolve(context.Background(), RoutedEndpoint{
		Endpoint: &sqlag.Endpoint{ID: "x", Backend: "omni"},
	}); err == nil {
		t.Fatal("non-insights backend accepted")
	}
	if !strings.HasPrefix(backendName(routed), "cloudwatch") {
		t.Fatalf("backendName = %q", backendName(routed))
	}
}

func TestMultiResolverDispatchesInsights(t *testing.T) {
	insights := NewInsightsResolver(InsightsResolverConfig{})
	insights.newDriver = func(_ context.Context, _ RoutedEndpoint, cfg insightsdriver.Config) (sqlag.Backend, error) {
		return &fakeSQLBackend{id: cfg.EndpointID}, nil
	}
	omni := NewOmniResolver(OmniResolverConfig{})
	omni.newDriver = func(_ context.Context, _ RoutedEndpoint, _ omnidriver.Config) (sqlag.Backend, error) {
		t.Fatal("omni resolver should not be called")
		return nil, nil
	}
	m := MultiResolver{Resolvers: map[string]BackendResolver{
		"omni":                     omni,
		"cloudwatch_logs_insights": insights,
	}}
	if _, err := m.Resolve(context.Background(), RoutedEndpoint{
		Endpoint:  &sqlag.Endpoint{ID: "p/logs/s/insights", Backend: "cloudwatch_logs_insights"},
		Shard:     policy.Shard{Region: "us-east-1"},
		Partition: policy.PartitionPolicy{Name: "p"},
	}); err != nil {
		t.Fatalf("insights dispatch: %v", err)
	}
}
