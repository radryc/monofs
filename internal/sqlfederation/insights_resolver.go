package sqlfederation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	insightsdriver "github.com/radryc/sqlagregator/drivers/cloudwatchlogsinsights"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

// InsightsResolverConfig configures the CloudWatch Logs Insights resolver.
type InsightsResolverConfig struct {
	// DefaultRegion is used when the shard does not declare a region.
	DefaultRegion string
	Profile       string
	Endpoint      string
	// Timeout bounds one Insights query.
	Timeout time.Duration
	// MaxRows bounds one partial result.
	MaxRows int
}

// InsightsResolver builds and caches one Logs Insights driver per endpoint.
type InsightsResolver struct {
	cfg InsightsResolverConfig

	mu      sync.Mutex
	drivers map[string]sqlag.Backend

	newDriver func(context.Context, RoutedEndpoint, insightsdriver.Config) (sqlag.Backend, error)
}

// NewInsightsResolver creates a resolver.
func NewInsightsResolver(cfg InsightsResolverConfig) *InsightsResolver {
	return &InsightsResolver{
		cfg:     cfg,
		drivers: map[string]sqlag.Backend{},
		newDriver: func(ctx context.Context, _ RoutedEndpoint, cfg insightsdriver.Config) (sqlag.Backend, error) {
			return insightsdriver.New(ctx, cfg)
		},
	}
}

// Resolve implements BackendResolver.
func (r *InsightsResolver) Resolve(ctx context.Context, routed RoutedEndpoint) (sqlag.Backend, error) {
	if routed.Endpoint == nil || routed.Endpoint.Backend != "cloudwatch_logs_insights" {
		return nil, fmt.Errorf("insights resolver: backend %q is not cloudwatch_logs_insights", backendName(routed))
	}
	key := routed.Endpoint.ID
	r.mu.Lock()
	if driver, ok := r.drivers[key]; ok {
		r.mu.Unlock()
		return driver, nil
	}
	r.mu.Unlock()

	region := strings.TrimSpace(routed.Shard.Region)
	if region == "" {
		region = strings.TrimSpace(r.cfg.DefaultRegion)
	}
	driver, err := r.newDriver(ctx, routed, insightsdriver.Config{
		EndpointID:    key,
		Region:        region,
		Profile:       r.cfg.Profile,
		Endpoint:      r.cfg.Endpoint,
		AssumeRoleARN: strings.TrimSpace(routed.Placement.AssumeRole),
		Timeout:       r.cfg.Timeout,
		MaxRows:       r.cfg.MaxRows,
	})
	if err != nil {
		return nil, fmt.Errorf("insights resolver: endpoint %s: %w", key, err)
	}
	if driver == nil {
		return nil, fmt.Errorf("insights resolver: endpoint %s: nil driver", key)
	}
	r.mu.Lock()
	if existing, ok := r.drivers[key]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.drivers[key] = driver
	r.mu.Unlock()
	return driver, nil
}

var _ BackendResolver = (*InsightsResolver)(nil)
