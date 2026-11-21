package sqlfederation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	bigquerydriver "github.com/radryc/sqlagregator/drivers/bigquery"
	"github.com/radryc/sqlagregator/pkg/sqlag"
)

// BigQueryResolverConfig configures the BigQuery placement resolver.
type BigQueryResolverConfig struct {
	// ProjectID is the billing project. Empty derives it from the shard's
	// project or the placement source's first segment.
	ProjectID string
	Location  string
	// CredentialsFile is the service-account JSON used for jobs.
	CredentialsFile string
	Endpoint        string
	DisableAuth     bool
	Timeout         time.Duration
	MaxRows         int
}

// BigQueryResolver builds and caches one BigQuery driver per endpoint.
type BigQueryResolver struct {
	cfg BigQueryResolverConfig

	mu      sync.Mutex
	drivers map[string]sqlag.Backend

	newDriver func(context.Context, RoutedEndpoint, bigquerydriver.Config) (sqlag.Backend, error)
}

// NewBigQueryResolver creates a resolver.
func NewBigQueryResolver(cfg BigQueryResolverConfig) *BigQueryResolver {
	return &BigQueryResolver{
		cfg:     cfg,
		drivers: map[string]sqlag.Backend{},
		newDriver: func(ctx context.Context, _ RoutedEndpoint, cfg bigquerydriver.Config) (sqlag.Backend, error) {
			return bigquerydriver.New(ctx, cfg)
		},
	}
}

// Resolve implements BackendResolver.
func (r *BigQueryResolver) Resolve(ctx context.Context, routed RoutedEndpoint) (sqlag.Backend, error) {
	if routed.Endpoint == nil || routed.Endpoint.Backend != "bigquery" {
		return nil, fmt.Errorf("bigquery resolver: backend %q is not bigquery", backendName(routed))
	}
	key := routed.Endpoint.ID
	r.mu.Lock()
	if driver, ok := r.drivers[key]; ok {
		r.mu.Unlock()
		return driver, nil
	}
	r.mu.Unlock()

	project := strings.TrimSpace(r.cfg.ProjectID)
	if project == "" {
		project = strings.TrimSpace(routed.Shard.Project)
	}
	if project == "" && routed.Endpoint.Source != "" {
		if parts := strings.SplitN(routed.Endpoint.Source, ".", 2); len(parts) == 2 {
			project = parts[0]
		}
	}
	if project == "" {
		return nil, fmt.Errorf("bigquery resolver: endpoint %s has no project", key)
	}
	driver, err := r.newDriver(ctx, routed, bigquerydriver.Config{
		EndpointID:      key,
		ProjectID:       project,
		Location:        strings.TrimSpace(r.cfg.Location),
		CredentialsFile: strings.TrimSpace(r.cfg.CredentialsFile),
		Endpoint:        strings.TrimSpace(r.cfg.Endpoint),
		DisableAuth:     r.cfg.DisableAuth,
		Timeout:         r.cfg.Timeout,
		MaxRows:         r.cfg.MaxRows,
	})
	if err != nil {
		return nil, fmt.Errorf("bigquery resolver: endpoint %s: %w", key, err)
	}
	if driver == nil {
		return nil, fmt.Errorf("bigquery resolver: endpoint %s: nil driver", key)
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

var _ BackendResolver = (*BigQueryResolver)(nil)
