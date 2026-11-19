package sqlfederation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	omnidriver "github.com/radryc/sqlagregator/drivers/omni"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

// OmniResolverConfig configures the Omni placement resolver.
type OmniResolverConfig struct {
	// DefaultRegion is used when the shard does not declare a region.
	DefaultRegion string
	Profile       string
	Endpoint      string
	SessionPrefix string
	Timeout       time.Duration
}

// OmniResolver builds and caches one Omni driver per endpoint. The driver's
// session is reused across queries.
type OmniResolver struct {
	cfg OmniResolverConfig

	mu      sync.Mutex
	drivers map[string]sqlag.Backend

	// newDriver is replaceable in tests.
	newDriver func(context.Context, RoutedEndpoint, omnidriver.Config) (sqlag.Backend, error)
}

// NewOmniResolver creates a resolver.
func NewOmniResolver(cfg OmniResolverConfig) *OmniResolver {
	if strings.TrimSpace(cfg.SessionPrefix) == "" {
		cfg.SessionPrefix = "monofs"
	}
	return &OmniResolver{
		cfg:     cfg,
		drivers: map[string]sqlag.Backend{},
		newDriver: func(ctx context.Context, _ RoutedEndpoint, cfg omnidriver.Config) (sqlag.Backend, error) {
			return omnidriver.New(ctx, cfg)
		},
	}
}

// Resolve implements BackendResolver.
func (r *OmniResolver) Resolve(ctx context.Context, routed RoutedEndpoint) (sqlag.Backend, error) {
	if routed.Endpoint == nil || routed.Endpoint.Backend != "omni" {
		return nil, fmt.Errorf("omni resolver: backend %q is not omni", backendName(routed))
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
	session := strings.TrimSpace(r.cfg.SessionPrefix) + "-" + routed.Partition.Name
	driver, err := r.newDriver(ctx, routed, omnidriver.Config{
		EndpointID:    key,
		Region:        region,
		Profile:       r.cfg.Profile,
		Endpoint:      r.cfg.Endpoint,
		AssumeRoleARN: strings.TrimSpace(routed.Placement.AssumeRole),
		SessionName:   session,
		Timeout:       r.cfg.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("omni resolver: endpoint %s: %w", key, err)
	}
	if driver == nil {
		return nil, fmt.Errorf("omni resolver: endpoint %s: nil driver", key)
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

// MultiResolver dispatches by backend name.
type MultiResolver struct {
	Resolvers map[string]BackendResolver
}

// Resolve implements BackendResolver.
func (m MultiResolver) Resolve(ctx context.Context, routed RoutedEndpoint) (sqlag.Backend, error) {
	if routed.Endpoint == nil {
		return nil, fmt.Errorf("multi resolver: nil endpoint")
	}
	resolver, ok := m.Resolvers[routed.Endpoint.Backend]
	if !ok || resolver == nil {
		return nil, fmt.Errorf("multi resolver: no backend resolver for %q", routed.Endpoint.Backend)
	}
	return resolver.Resolve(ctx, routed)
}

func backendName(routed RoutedEndpoint) string {
	if routed.Endpoint == nil {
		return ""
	}
	return routed.Endpoint.Backend
}

var _ BackendResolver = (*OmniResolver)(nil)
var _ BackendResolver = MultiResolver{}
