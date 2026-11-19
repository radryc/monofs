package sqlfederation

import (
	"context"
	"strings"
	"testing"

	"github.com/radryc/monofs/pkg/authz/policy"
	omnidriver "github.com/radryc/sqlagregator/drivers/omni"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

func TestOmniResolverCachesAndUsesShardRegion(t *testing.T) {
	created := 0
	var got omnidriver.Config
	resolver := NewOmniResolver(OmniResolverConfig{
		DefaultRegion: "us-east-1",
		SessionPrefix: "monofs",
	})
	resolver.newDriver = func(_ context.Context, _ RoutedEndpoint, cfg omnidriver.Config) (sqlag.Backend, error) {
		created++
		got = cfg
		return &fakeSQLBackend{id: cfg.EndpointID}, nil
	}
	routed := RoutedEndpoint{
		Endpoint:  &sqlag.Endpoint{ID: "observability/logs/aws-a/omni", Backend: "omni"},
		Shard:     policy.Shard{Region: "us-west-2"},
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
	if got.Region != "us-west-2" {
		t.Fatalf("region = %q", got.Region)
	}
	if !strings.Contains(got.SessionName, "observability") {
		t.Fatalf("session = %q", got.SessionName)
	}
	if _, err := resolver.Resolve(context.Background(), RoutedEndpoint{
		Endpoint: &sqlag.Endpoint{ID: "x", Backend: "clickhouse"},
	}); err == nil {
		t.Fatal("non-omni backend accepted")
	}
}

func TestMultiResolverDispatches(t *testing.T) {
	omni := NewOmniResolver(OmniResolverConfig{})
	omni.newDriver = func(_ context.Context, _ RoutedEndpoint, cfg omnidriver.Config) (sqlag.Backend, error) {
		return &fakeSQLBackend{id: cfg.EndpointID}, nil
	}
	m := MultiResolver{Resolvers: map[string]BackendResolver{"omni": omni}}
	if _, err := m.Resolve(context.Background(), RoutedEndpoint{
		Endpoint:  &sqlag.Endpoint{ID: "p/logs/s/omni", Backend: "omni"},
		Shard:     policy.Shard{Region: "us-east-1"},
		Partition: policy.PartitionPolicy{Name: "p"},
	}); err != nil {
		t.Fatalf("omni dispatch: %v", err)
	}
	if _, err := m.Resolve(context.Background(), RoutedEndpoint{
		Endpoint:  &sqlag.Endpoint{ID: "p/logs/s/ch", Backend: "clickhouse"},
		Partition: policy.PartitionPolicy{Name: "p"},
	}); err == nil {
		t.Fatal("missing backend resolver accepted")
	}
}
