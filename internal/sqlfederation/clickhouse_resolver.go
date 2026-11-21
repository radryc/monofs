package sqlfederation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	clickhousedriver "github.com/radryc/sqlagregator/drivers/clickhouse"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

// ClickHouseResolverConfig configures the shared ClickHouse placement resolver.
type ClickHouseResolverConfig struct {
	URL      string
	Database string
	User     string
	Password string
	Timeout  time.Duration
	MaxRows  int
}

// ClickHouseResolver builds and caches one ClickHouse driver per endpoint.
// ClickHouse is a shared backend: every node reads the same tables, so a
// placement is one logical shard, never a node fan-out.
type ClickHouseResolver struct {
	cfg ClickHouseResolverConfig

	mu      sync.Mutex
	drivers map[string]sqlag.Backend

	newDriver func(context.Context, RoutedEndpoint, clickhousedriver.Config) (sqlag.Backend, error)
}

// NewClickHouseResolver creates a resolver.
func NewClickHouseResolver(cfg ClickHouseResolverConfig) *ClickHouseResolver {
	return &ClickHouseResolver{
		cfg:     cfg,
		drivers: map[string]sqlag.Backend{},
		newDriver: func(_ context.Context, _ RoutedEndpoint, cfg clickhousedriver.Config) (sqlag.Backend, error) {
			return clickhousedriver.New(cfg)
		},
	}
}

// Resolve implements BackendResolver.
func (r *ClickHouseResolver) Resolve(ctx context.Context, routed RoutedEndpoint) (sqlag.Backend, error) {
	if routed.Endpoint == nil || routed.Endpoint.Backend != "clickhouse" {
		return nil, fmt.Errorf("clickhouse resolver: backend %q is not clickhouse", backendName(routed))
	}
	if strings.TrimSpace(r.cfg.URL) == "" {
		return nil, fmt.Errorf("clickhouse resolver: no URL configured")
	}
	key := routed.Endpoint.ID
	r.mu.Lock()
	if driver, ok := r.drivers[key]; ok {
		r.mu.Unlock()
		return driver, nil
	}
	r.mu.Unlock()

	driver, err := r.newDriver(ctx, routed, clickhousedriver.Config{
		EndpointID: key,
		URL:        strings.TrimSpace(r.cfg.URL),
		Database:   strings.TrimSpace(r.cfg.Database),
		User:       r.cfg.User,
		Password:   r.cfg.Password,
		Timeout:    r.cfg.Timeout,
		MaxRows:    r.cfg.MaxRows,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse resolver: endpoint %s: %w", key, err)
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

var _ BackendResolver = (*ClickHouseResolver)(nil)
