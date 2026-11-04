package logengine

import (
	"sync"
	"time"
)

const (
	metricDiscoveryCacheTTL        = 60 * time.Second
	metricDiscoveryCacheMaxEntries = 256
)

type metricDiscoveryCacheEntry struct {
	records   []MetricRecord
	expiresAt time.Time
}

// metricDiscoveryCache keeps recent metric name/label discovery results.
// Discovery is intentionally approximate: the metric vocabulary is stable, so
// a short-lived cache removes repeated manifest walks for dashboards that
// refresh quickly without changing query semantics.
type metricDiscoveryCache struct {
	mu         sync.Mutex
	entries    map[string]metricDiscoveryCacheEntry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
}

func newMetricDiscoveryCache() *metricDiscoveryCache {
	return &metricDiscoveryCache{
		entries:    make(map[string]metricDiscoveryCacheEntry),
		ttl:        metricDiscoveryCacheTTL,
		maxEntries: metricDiscoveryCacheMaxEntries,
		now:        time.Now,
	}
}

func (c *metricDiscoveryCache) get(key string) ([]MetricRecord, bool) {
	if c == nil {
		return nil, false
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return append([]MetricRecord(nil), entry.records...), true
}

func (c *metricDiscoveryCache) put(key string, records []MetricRecord) {
	if c == nil {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxEntries {
		// Drop expired entries first. If the cache is still full, evict the
		// entry with the earliest expiry; discovery results are cheap to rebuild.
		for candidate, entry := range c.entries {
			if !now.Before(entry.expiresAt) {
				delete(c.entries, candidate)
			}
		}
		for len(c.entries) >= c.maxEntries {
			var (
				oldestKey string
				oldestAt  time.Time
			)
			for candidate, entry := range c.entries {
				if oldestKey == "" || entry.expiresAt.Before(oldestAt) {
					oldestKey = candidate
					oldestAt = entry.expiresAt
				}
			}
			if oldestKey == "" {
				break
			}
			delete(c.entries, oldestKey)
		}
	}
	c.entries[key] = metricDiscoveryCacheEntry{
		records:   append([]MetricRecord(nil), records...),
		expiresAt: now.Add(c.ttl),
	}
}
