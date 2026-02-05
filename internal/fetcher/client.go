package fetcher

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client provides access to the fetcher pool with repo-affinity routing.
// Storage nodes use this to request blobs from fetchers.
type Client struct {
	fetchers []*fetcherConn
	mu       sync.RWMutex

	// Affinity map: sourceKey -> preferred fetcher index
	affinity   map[string]int
	affinityMu sync.RWMutex

	logger *slog.Logger
	config ClientConfig

	// Stats
	totalRequests  atomic.Int64
	affinityHits   atomic.Int64
	affinityMisses atomic.Int64
}

type fetcherConn struct {
	address string
	conn    *grpc.ClientConn
	client  pb.BlobFetcherClient

	// Cached sources (updated periodically)
	cachedSources    map[string]bool
	cachedSourcesMu  sync.RWMutex
	lastSourceUpdate time.Time

	// Health tracking
	healthy    atomic.Bool
	lastError  time.Time
	errorCount atomic.Int64
}

// ClientConfig configures the fetcher client.
type ClientConfig struct {
	// Addresses of fetcher instances.
	FetcherAddresses []string

	// ConnectionTimeout for establishing gRPC connections.
	ConnectionTimeout time.Duration

	// RequestTimeout for individual fetch requests.
	RequestTimeout time.Duration

	// AffinityWeight controls how strongly affinity influences routing.
	// 0.0 = pure round-robin, 1.0 = strict affinity
	AffinityWeight float64

	// HealthCheckInterval for periodic health checks.
	HealthCheckInterval time.Duration

	// MaxRetries per request across fetchers.
	MaxRetries int
}

func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		ConnectionTimeout:   5 * time.Second,
		RequestTimeout:      5 * time.Minute,
		AffinityWeight:      0.8,
		HealthCheckInterval: 10 * time.Second,
		MaxRetries:          3,
	}
}

// NewClient creates a new fetcher client with repo-affinity routing.
func NewClient(config ClientConfig, logger *slog.Logger) (*Client, error) {
	c := &Client{
		fetchers: make([]*fetcherConn, 0, len(config.FetcherAddresses)),
		affinity: make(map[string]int),
		logger:   logger,
		config:   config,
	}

	// Connect to all fetchers
	for _, addr := range config.FetcherAddresses {
		fc, err := c.connectFetcher(addr)
		if err != nil {
			logger.Warn("failed to connect to fetcher", "address", addr, "error", err)
			continue
		}
		c.fetchers = append(c.fetchers, fc)
	}

	if len(c.fetchers) == 0 {
		return nil, fmt.Errorf("no fetchers available")
	}

	// Start health check loop
	go c.healthCheckLoop()

	// Start affinity update loop
	go c.affinityUpdateLoop()

	return c, nil
}

func (c *Client) connectFetcher(address string) (*fetcherConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.config.ConnectionTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, err
	}

	fc := &fetcherConn{
		address:       address,
		conn:          conn,
		client:        pb.NewBlobFetcherClient(conn),
		cachedSources: make(map[string]bool),
	}
	fc.healthy.Store(true)

	return fc, nil
}

// FetchBlob fetches a blob using repo-affinity routing.
func (c *Client) FetchBlob(ctx context.Context, req *FetchRequest, sourceType SourceType) ([]byte, error) {
	c.totalRequests.Add(1)

	// Build proto request
	protoReq := &pb.FetchBlobRequest{
		ContentId:    req.ContentID,
		SourceType:   sourceTypeToProto(sourceType),
		SourceConfig: req.SourceConfig,
		RequestId:    req.RequestID,
		StorageId:    req.SourceKey, // Used for affinity
		Priority:     int32(req.Priority),
	}

	// Get fetcher with affinity
	fetcher := c.selectFetcher(req.SourceKey)
	if fetcher == nil {
		return nil, fmt.Errorf("no healthy fetchers available")
	}

	// Fetch with retries
	var lastErr error
	for attempt := 0; attempt < c.config.MaxRetries; attempt++ {
		if attempt > 0 {
			// Switch to different fetcher on retry
			fetcher = c.selectFetcherExcluding(req.SourceKey, fetcher)
			if fetcher == nil {
				break
			}
		}

		data, err := c.doFetch(ctx, fetcher, protoReq)
		if err == nil {
			// Update affinity on success
			c.updateAffinity(req.SourceKey, fetcher)
			return data, nil
		}

		lastErr = err
		c.logger.Warn("fetch attempt failed",
			"attempt", attempt+1,
			"fetcher", fetcher.address,
			"error", err,
		)
	}

	return nil, fmt.Errorf("all fetch attempts failed: %w", lastErr)
}

func (c *Client) doFetch(ctx context.Context, fetcher *fetcherConn, req *pb.FetchBlobRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()

	stream, err := fetcher.client.FetchBlob(ctx, req)
	if err != nil {
		fetcher.recordError()
		return nil, err
	}

	var data []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fetcher.recordError()
			return nil, err
		}
		data = append(data, chunk.Data...)
	}

	fetcher.healthy.Store(true)
	return data, nil
}

// FetchBlobStream fetches a blob and returns a reader.
func (c *Client) FetchBlobStream(ctx context.Context, req *FetchRequest, sourceType SourceType) (io.ReadCloser, error) {
	c.totalRequests.Add(1)

	protoReq := &pb.FetchBlobRequest{
		ContentId:    req.ContentID,
		SourceType:   sourceTypeToProto(sourceType),
		SourceConfig: req.SourceConfig,
		RequestId:    req.RequestID,
		StorageId:    req.SourceKey,
		Priority:     int32(req.Priority),
	}

	fetcher := c.selectFetcher(req.SourceKey)
	if fetcher == nil {
		return nil, fmt.Errorf("no healthy fetchers available")
	}

	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)

	stream, err := fetcher.client.FetchBlob(ctx, protoReq)
	if err != nil {
		cancel()
		fetcher.recordError()
		return nil, err
	}

	return &streamReader{
		stream: stream,
		cancel: cancel,
	}, nil
}

// Prefetch queues blobs for background prefetching.
func (c *Client) Prefetch(ctx context.Context, requests []*FetchRequest, sourceType SourceType) error {
	// Group by source key for efficient batch prefetch
	byFetcher := make(map[*fetcherConn][]*pb.FetchBlobRequest)

	for _, req := range requests {
		fetcher := c.selectFetcher(req.SourceKey)
		if fetcher == nil {
			continue
		}

		protoReq := &pb.FetchBlobRequest{
			ContentId:    req.ContentID,
			SourceType:   sourceTypeToProto(sourceType),
			SourceConfig: req.SourceConfig,
			StorageId:    req.SourceKey,
			Priority:     int32(req.Priority),
		}
		byFetcher[fetcher] = append(byFetcher[fetcher], protoReq)
	}

	// Send prefetch requests to each fetcher
	for fetcher, reqs := range byFetcher {
		go func(f *fetcherConn, r []*pb.FetchBlobRequest) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, err := f.client.PrefetchBlobs(ctx, &pb.PrefetchRequest{Blobs: r})
			if err != nil {
				c.logger.Warn("prefetch request failed", "fetcher", f.address, "error", err)
			}
		}(fetcher, reqs)
	}

	return nil
}

// selectFetcher chooses a fetcher using repo-affinity routing.
func (c *Client) selectFetcher(sourceKey string) *fetcherConn {
	c.mu.RLock()
	fetchers := c.fetchers
	c.mu.RUnlock()

	if len(fetchers) == 0 {
		return nil
	}

	// Get healthy fetchers
	healthy := make([]*fetcherConn, 0, len(fetchers))
	for _, f := range fetchers {
		if f.healthy.Load() {
			healthy = append(healthy, f)
		}
	}
	if len(healthy) == 0 {
		// Fall back to any fetcher
		healthy = fetchers
	}

	// Check affinity
	c.affinityMu.RLock()
	preferredIdx, hasAffinity := c.affinity[sourceKey]
	c.affinityMu.RUnlock()

	if hasAffinity && preferredIdx < len(fetchers) {
		preferred := fetchers[preferredIdx]
		if preferred.healthy.Load() {
			c.affinityHits.Add(1)
			return preferred
		}
	}
	c.affinityMisses.Add(1)

	// Hash-based selection for consistent routing
	h := fnv.New32a()
	h.Write([]byte(sourceKey))
	idx := int(h.Sum32()) % len(healthy)

	return healthy[idx]
}

func (c *Client) selectFetcherExcluding(sourceKey string, exclude *fetcherConn) *fetcherConn {
	c.mu.RLock()
	fetchers := c.fetchers
	c.mu.RUnlock()

	for _, f := range fetchers {
		if f != exclude && f.healthy.Load() {
			return f
		}
	}
	return nil
}

func (c *Client) updateAffinity(sourceKey string, fetcher *fetcherConn) {
	c.mu.RLock()
	idx := -1
	for i, f := range c.fetchers {
		if f == fetcher {
			idx = i
			break
		}
	}
	c.mu.RUnlock()

	if idx >= 0 {
		c.affinityMu.Lock()
		c.affinity[sourceKey] = idx
		c.affinityMu.Unlock()
	}
}

func (c *Client) healthCheckLoop() {
	ticker := time.NewTicker(c.config.HealthCheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		c.mu.RLock()
		fetchers := c.fetchers
		c.mu.RUnlock()

		for _, f := range fetchers {
			go c.checkFetcherHealth(f)
		}
	}
}

func (c *Client) checkFetcherHealth(f *fetcherConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := f.client.GetStats(ctx, &pb.FetcherStatsRequest{})
	if err != nil {
		f.healthy.Store(false)
		f.errorCount.Add(1)
		f.lastError = time.Now()
	} else {
		f.healthy.Store(true)
	}
}

func (c *Client) affinityUpdateLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		c.updateCachedSources()
	}
}

func (c *Client) updateCachedSources() {
	c.mu.RLock()
	fetchers := c.fetchers
	c.mu.RUnlock()

	// Collect cached sources from all fetchers
	type sourceInfo struct {
		fetcherIdx int
		count      int
	}
	sourceCounts := make(map[string]*sourceInfo)

	for idx, f := range fetchers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stats, err := f.client.GetStats(ctx, &pb.FetcherStatsRequest{IncludeSourceStats: true})
		cancel()

		if err != nil {
			continue
		}

		f.cachedSourcesMu.Lock()
		f.cachedSources = make(map[string]bool)
		for source := range stats.SourceStats {
			f.cachedSources[source] = true
			if info, ok := sourceCounts[source]; ok {
				info.count++
			} else {
				sourceCounts[source] = &sourceInfo{fetcherIdx: idx, count: 1}
			}
		}
		f.lastSourceUpdate = time.Now()
		f.cachedSourcesMu.Unlock()
	}

	// Update affinity based on cached sources
	c.affinityMu.Lock()
	for source, info := range sourceCounts {
		if info.count == 1 {
			// Source only on one fetcher, strong affinity
			c.affinity[source] = info.fetcherIdx
		}
	}
	c.affinityMu.Unlock()
}

// GetStats returns client statistics.
func (c *Client) GetStats() ClientStats {
	c.mu.RLock()
	healthyCount := 0
	for _, f := range c.fetchers {
		if f.healthy.Load() {
			healthyCount++
		}
	}
	totalFetchers := len(c.fetchers)
	c.mu.RUnlock()

	c.affinityMu.RLock()
	affinityEntries := len(c.affinity)
	c.affinityMu.RUnlock()

	return ClientStats{
		TotalRequests:   c.totalRequests.Load(),
		AffinityHits:    c.affinityHits.Load(),
		AffinityMisses:  c.affinityMisses.Load(),
		TotalFetchers:   totalFetchers,
		HealthyFetchers: healthyCount,
		AffinityEntries: affinityEntries,
	}
}

// Close closes all fetcher connections.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, f := range c.fetchers {
		if f.conn != nil {
			f.conn.Close()
		}
	}
	c.fetchers = nil
	return nil
}

// ClientStats holds client statistics.
type ClientStats struct {
	TotalRequests   int64
	AffinityHits    int64
	AffinityMisses  int64
	TotalFetchers   int
	HealthyFetchers int
	AffinityEntries int
}

func (fc *fetcherConn) recordError() {
	fc.healthy.Store(false)
	fc.errorCount.Add(1)
	fc.lastError = time.Now()
}

// streamReader wraps a gRPC stream as an io.ReadCloser.
type streamReader struct {
	stream pb.BlobFetcher_FetchBlobClient
	cancel context.CancelFunc
	buf    []byte
	pos    int
}

func (r *streamReader) Read(p []byte) (int, error) {
	// Drain buffer first
	if r.pos < len(r.buf) {
		n := copy(p, r.buf[r.pos:])
		r.pos += n
		return n, nil
	}

	// Get next chunk
	chunk, err := r.stream.Recv()
	if err == io.EOF {
		return 0, io.EOF
	}
	if err != nil {
		return 0, err
	}

	// Copy to output
	n := copy(p, chunk.Data)
	if n < len(chunk.Data) {
		// Buffer remainder
		r.buf = chunk.Data[n:]
		r.pos = 0
	} else {
		r.buf = nil
	}

	return n, nil
}

func (r *streamReader) Close() error {
	r.cancel()
	return nil
}

func sourceTypeToProto(st SourceType) pb.SourceType {
	switch st {
	case SourceTypeGit:
		return pb.SourceType_SOURCE_TYPE_GIT
	case SourceTypeGoMod:
		return pb.SourceType_SOURCE_TYPE_GOMOD
	case SourceTypeS3:
		return pb.SourceType_SOURCE_TYPE_S3
	case SourceTypeHTTP:
		return pb.SourceType_SOURCE_TYPE_HTTP
	case SourceTypeOCI:
		return pb.SourceType_SOURCE_TYPE_OCI
	default:
		return pb.SourceType_SOURCE_TYPE_UNKNOWN
	}
}

// HealthyFetchers returns a sorted list of healthy fetcher addresses.
func (c *Client) HealthyFetchers() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	addresses := make([]string, 0, len(c.fetchers))
	for _, f := range c.fetchers {
		if f.healthy.Load() {
			addresses = append(addresses, f.address)
		}
	}
	sort.Strings(addresses)
	return addresses
}

// FetchBlobSimple is a convenience wrapper for FetchBlob with common Git/GoMod params.
func (c *Client) FetchBlobSimple(ctx context.Context, sourceURL, blobHash, filePath, branch string, sourceType SourceType) ([]byte, error) {
	sourceConfig := map[string]string{
		"repo_url":     sourceURL,
		"branch":       branch,
		"display_path": filePath,
		"file_path":    filePath,
	}

	// For Go modules, parse module_path and version from sourceURL (e.g., "google.golang.org/grpc@v1.75.0")
	if sourceType == SourceTypeGoMod {
		if idx := strings.LastIndex(sourceURL, "@"); idx != -1 {
			sourceConfig["module_path"] = sourceURL[:idx]
			sourceConfig["version"] = sourceURL[idx+1:]
		}
	}

	req := &FetchRequest{
		ContentID:    blobHash,
		SourceKey:    sourceURL,
		SourceConfig: sourceConfig,
		Priority:     5,
	}

	return c.FetchBlob(ctx, req, sourceType)
}

// CheckCacheSimple checks if a blob is in the prefetch cache of any fetcher.
func (c *Client) CheckCacheSimple(ctx context.Context, sourceURL, blobHash string) (bool, error) {
	c.mu.RLock()
	fetchers := c.fetchers
	c.mu.RUnlock()

	for _, f := range fetchers {
		if !f.healthy.Load() {
			continue
		}

		resp, err := f.client.CheckCache(ctx, &pb.CheckCacheRequest{
			ContentIds: []string{blobHash},
			SourceType: pb.SourceType_SOURCE_TYPE_GIT, // Default, doesn't affect cache lookup
		})
		if err != nil {
			continue
		}
		if cached, ok := resp.Cached[blobHash]; ok && cached {
			return true, nil
		}
	}

	return false, nil
}

// PrefetchFile contains information for prefetching a single file.
type PrefetchFile struct {
	SourceURL  string
	BlobHash   string
	FilePath   string
	Branch     string
	SourceType SourceType
	Confidence float32
}

// PrefetchSimple sends prefetch requests using the simpler PrefetchFile struct.
func (c *Client) PrefetchSimple(ctx context.Context, files []PrefetchFile) (int, error) {
	if len(files) == 0 {
		return 0, nil
	}

	// Group by fetcher using affinity
	byFetcher := make(map[*fetcherConn][]*pb.FetchBlobRequest)
	for _, file := range files {
		fetcher := c.selectFetcher(file.SourceURL)
		if fetcher == nil {
			continue
		}

		protoReq := &pb.FetchBlobRequest{
			ContentId:  file.BlobHash,
			SourceType: sourceTypeToProto(file.SourceType),
			SourceConfig: map[string]string{
				"repo_url":     file.SourceURL,
				"branch":       file.Branch,
				"display_path": file.FilePath,
				"file_path":    file.FilePath,
			},
			StorageId: file.SourceURL,
			Priority:  int32(10 - int(file.Confidence*10)), // Higher confidence = higher priority
		}
		byFetcher[fetcher] = append(byFetcher[fetcher], protoReq)
	}

	// Send prefetch requests
	queued := 0
	for fetcher, reqs := range byFetcher {
		go func(f *fetcherConn, r []*pb.FetchBlobRequest) {
			pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := f.client.PrefetchBlobs(pctx, &pb.PrefetchRequest{Blobs: r})
			if err != nil {
				c.logger.Warn("prefetch request failed", "fetcher", f.address, "error", err)
				return
			}
			c.logger.Debug("prefetch queued", "fetcher", f.address, "accepted", resp.Accepted)
		}(fetcher, reqs)
		queued += len(reqs)
	}

	return queued, nil
}

// FetcherStats contains statistics for a single fetcher instance.
type FetcherStats struct {
	Address          string                     `json:"address"`
	FetcherID        string                     `json:"fetcher_id"`
	Healthy          bool                       `json:"healthy"`
	UptimeSeconds    int64                      `json:"uptime_seconds"`
	TotalRequests    int64                      `json:"total_requests"`
	CacheHits        int64                      `json:"cache_hits"`
	CacheMisses      int64                      `json:"cache_misses"`
	CacheHitRate     float64                    `json:"cache_hit_rate"`
	CacheSizeBytes   int64                      `json:"cache_size_bytes"`
	CacheEntries     int64                      `json:"cache_entries"`
	ActiveFetches    int64                      `json:"active_fetches"`
	QueuedPrefetches int64                      `json:"queued_prefetches"`
	BytesFetched     int64                      `json:"bytes_fetched"`
	BytesServed      int64                      `json:"bytes_served"`
	SourceStats      map[string]SourceStatsInfo `json:"source_stats,omitempty"`
	ErrorCount       int64                      `json:"error_count"`
	LastError        string                     `json:"last_error,omitempty"`
}

// SourceStatsInfo contains per-source statistics.
type SourceStatsInfo struct {
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	BytesFetched int64   `json:"bytes_fetched"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	CachedItems  int64   `json:"cached_items"`
}

// ClusterStats contains aggregated statistics for the entire fetcher cluster.
type ClusterStats struct {
	TotalFetchers        int            `json:"total_fetchers"`
	HealthyFetchers      int            `json:"healthy_fetchers"`
	TotalRequests        int64          `json:"total_requests"`
	TotalCacheHits       int64          `json:"total_cache_hits"`
	TotalCacheMisses     int64          `json:"total_cache_misses"`
	AggregatedHitRate    float64        `json:"aggregated_hit_rate"`
	TotalCacheSizeBytes  int64          `json:"total_cache_size_bytes"`
	TotalCacheEntries    int64          `json:"total_cache_entries"`
	TotalActiveFetches   int64          `json:"total_active_fetches"`
	TotalQueuedPrefetch  int64          `json:"total_queued_prefetch"`
	TotalBytesFetched    int64          `json:"total_bytes_fetched"`
	TotalBytesServed     int64          `json:"total_bytes_served"`
	Fetchers             []FetcherStats `json:"fetchers"`
	ClientAffinityHits   int64          `json:"client_affinity_hits"`
	ClientAffinityMisses int64          `json:"client_affinity_misses"`
	ClientTotalRequests  int64          `json:"client_total_requests"`
}

// GetClusterStats retrieves statistics from all fetchers in the cluster.
func (c *Client) GetClusterStats(ctx context.Context, includeSourceStats bool) (*ClusterStats, error) {
	c.mu.RLock()
	fetchers := c.fetchers
	c.mu.RUnlock()

	stats := &ClusterStats{
		TotalFetchers:        len(fetchers),
		Fetchers:             make([]FetcherStats, 0, len(fetchers)),
		ClientTotalRequests:  c.totalRequests.Load(),
		ClientAffinityHits:   c.affinityHits.Load(),
		ClientAffinityMisses: c.affinityMisses.Load(),
	}

	// Query each fetcher in parallel
	type fetcherResult struct {
		stats *FetcherStats
		err   error
	}
	results := make(chan fetcherResult, len(fetchers))

	for _, f := range fetchers {
		go func(fetcher *fetcherConn) {
			fs := &FetcherStats{
				Address:    fetcher.address,
				Healthy:    fetcher.healthy.Load(),
				ErrorCount: fetcher.errorCount.Load(),
			}

			// Try to get stats from fetcher
			reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			resp, err := fetcher.client.GetStats(reqCtx, &pb.FetcherStatsRequest{
				IncludeSourceStats: includeSourceStats,
				IncludeCacheStats:  true,
			})
			if err != nil {
				fs.LastError = err.Error()
				fs.Healthy = false
				results <- fetcherResult{stats: fs, err: nil}
				return
			}

			fs.FetcherID = resp.FetcherId
			fs.UptimeSeconds = resp.UptimeSeconds
			fs.TotalRequests = resp.TotalRequests
			fs.CacheHits = resp.CacheHits
			fs.CacheMisses = resp.CacheMisses
			fs.CacheHitRate = resp.CacheHitRate
			fs.CacheSizeBytes = resp.CacheSizeBytes
			fs.CacheEntries = resp.CacheEntries
			fs.ActiveFetches = resp.ActiveFetches
			fs.QueuedPrefetches = resp.QueuedPrefetches
			fs.BytesFetched = resp.BytesFetched
			fs.BytesServed = resp.BytesServed

			if includeSourceStats && len(resp.SourceStats) > 0 {
				fs.SourceStats = make(map[string]SourceStatsInfo)
				for k, v := range resp.SourceStats {
					fs.SourceStats[k] = SourceStatsInfo{
						Requests:     v.Requests,
						Errors:       v.Errors,
						BytesFetched: v.BytesFetched,
						AvgLatencyMs: v.AvgLatencyMs,
						CachedItems:  v.CachedItems,
					}
				}
			}

			results <- fetcherResult{stats: fs, err: nil}
		}(f)
	}

	// Collect results
	for i := 0; i < len(fetchers); i++ {
		result := <-results
		if result.stats != nil {
			stats.Fetchers = append(stats.Fetchers, *result.stats)

			// Aggregate
			if result.stats.Healthy {
				stats.HealthyFetchers++
			}
			stats.TotalRequests += result.stats.TotalRequests
			stats.TotalCacheHits += result.stats.CacheHits
			stats.TotalCacheMisses += result.stats.CacheMisses
			stats.TotalCacheSizeBytes += result.stats.CacheSizeBytes
			stats.TotalCacheEntries += result.stats.CacheEntries
			stats.TotalActiveFetches += result.stats.ActiveFetches
			stats.TotalQueuedPrefetch += result.stats.QueuedPrefetches
			stats.TotalBytesFetched += result.stats.BytesFetched
			stats.TotalBytesServed += result.stats.BytesServed
		}
	}

	// Calculate aggregate hit rate
	totalOps := stats.TotalCacheHits + stats.TotalCacheMisses
	if totalOps > 0 {
		stats.AggregatedHitRate = float64(stats.TotalCacheHits) / float64(totalOps)
	}

	// Sort fetchers by address for consistent output
	sort.Slice(stats.Fetchers, func(i, j int) bool {
		return stats.Fetchers[i].Address < stats.Fetchers[j].Address
	})

	return stats, nil
}
