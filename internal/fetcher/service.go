package fetcher

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

// Service implements the BlobFetcher gRPC service.
type Service struct {
	pb.UnimplementedBlobFetcherServer

	fetcherID string
	registry  *Registry
	logger    *slog.Logger

	// Prefetch queue
	prefetchQueue chan *prefetchJob
	prefetchWg    sync.WaitGroup

	// Stats
	startTime      time.Time
	totalRequests  atomic.Int64
	bytesServed    atomic.Int64
	activeRequests atomic.Int64

	// Configuration
	config ServiceConfig

	ctx    context.Context
	cancel context.CancelFunc
}

type ServiceConfig struct {
	// PrefetchWorkers is the number of background prefetch workers.
	PrefetchWorkers int

	// PrefetchQueueSize is the max pending prefetch requests.
	PrefetchQueueSize int

	// MaxConcurrentFetches limits parallel fetch operations.
	MaxConcurrentFetches int

	// StreamChunkSize for streaming responses.
	StreamChunkSize int
}

func DefaultServiceConfig() ServiceConfig {
	return ServiceConfig{
		PrefetchWorkers:      4,
		PrefetchQueueSize:    1000,
		MaxConcurrentFetches: 20,
		StreamChunkSize:      64 * 1024, // 64KB
	}
}

type prefetchJob struct {
	req       *pb.FetchBlobRequest
	submitted time.Time
}

// NewService creates a new fetcher service.
func NewService(fetcherID string, registry *Registry, config ServiceConfig, logger *slog.Logger) *Service {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Service{
		fetcherID:     fetcherID,
		registry:      registry,
		logger:        logger,
		prefetchQueue: make(chan *prefetchJob, config.PrefetchQueueSize),
		config:        config,
		startTime:     time.Now(),
		ctx:           ctx,
		cancel:        cancel,
	}

	// Start prefetch workers
	for i := 0; i < config.PrefetchWorkers; i++ {
		s.prefetchWg.Add(1)
		go s.prefetchWorker(i)
	}

	return s
}

// RegisterService registers the fetcher service with a gRPC server.
func (s *Service) RegisterService(server *grpc.Server) {
	pb.RegisterBlobFetcherServer(server, s)
}

// FetchBlob handles synchronous blob fetch requests.
func (s *Service) FetchBlob(req *pb.FetchBlobRequest, stream pb.BlobFetcher_FetchBlobServer) error {
	s.totalRequests.Add(1)
	s.activeRequests.Add(1)
	defer s.activeRequests.Add(-1)

	ctx := stream.Context()
	sourceType := protoToSourceType(req.SourceType)

	backend, ok := s.registry.Get(sourceType)
	if !ok {
		return fmt.Errorf("unsupported source type: %s", sourceType)
	}

	// Build fetch request
	fetchReq := &FetchRequest{
		ContentID:    req.ContentId,
		SourceKey:    getSourceKey(req),
		SourceConfig: req.SourceConfig,
		RequestID:    req.RequestId,
		Priority:     int(req.Priority),
	}

	s.logger.Debug("fetching blob",
		"content_id", req.ContentId,
		"source_type", sourceType,
		"source_key", fetchReq.SourceKey,
	)

	// Fetch with streaming
	reader, size, err := backend.FetchBlobStream(ctx, fetchReq)
	if err != nil {
		s.logger.Error("fetch failed", "content_id", req.ContentId, "error", err)
		return err
	}
	defer reader.Close()

	// Stream content back
	buf := make([]byte, s.config.StreamChunkSize)
	totalSent := int64(0)

	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			chunk := &pb.DataChunk{
				Data:   buf[:n],
				Offset: totalSent,
			}
			if sendErr := stream.Send(chunk); sendErr != nil {
				return sendErr
			}
			totalSent += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	s.bytesServed.Add(totalSent)
	s.logger.Debug("blob fetched", "content_id", req.ContentId, "size", size)

	return nil
}

// FetchBlobBatch handles batch fetch requests.
func (s *Service) FetchBlobBatch(req *pb.FetchBlobBatchRequest, stream pb.BlobFetcher_FetchBlobBatchServer) error {
	s.totalRequests.Add(1)
	ctx := stream.Context()

	concurrency := int(req.Concurrency)
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > s.config.MaxConcurrentFetches {
		concurrency = s.config.MaxConcurrentFetches
	}

	// Process blobs with limited concurrency
	results := make(chan *pb.FetchBlobBatchResponse, len(req.Blobs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, blobReq := range req.Blobs {
		wg.Add(1)
		go func(br *pb.FetchBlobRequest) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result := s.fetchSingleBlob(ctx, br)
			results <- result
		}(blobReq)
	}

	// Close results when all done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Stream results as they complete
	count := 0
	total := len(req.Blobs)
	for result := range results {
		count++
		result.BatchComplete = count == total
		if err := stream.Send(result); err != nil {
			return err
		}
		if req.FailFast && result.Error != "" {
			return fmt.Errorf("batch fetch failed: %s", result.Error)
		}
	}

	return nil
}

func (s *Service) fetchSingleBlob(ctx context.Context, req *pb.FetchBlobRequest) *pb.FetchBlobBatchResponse {
	start := time.Now()
	sourceType := protoToSourceType(req.SourceType)

	backend, ok := s.registry.Get(sourceType)
	if !ok {
		return &pb.FetchBlobBatchResponse{
			ContentId: req.ContentId,
			Error:     fmt.Sprintf("unsupported source type: %s", sourceType),
		}
	}

	fetchReq := &FetchRequest{
		ContentID:    req.ContentId,
		SourceKey:    getSourceKey(req),
		SourceConfig: req.SourceConfig,
		RequestID:    req.RequestId,
		Priority:     int(req.Priority),
	}

	result, err := backend.FetchBlob(ctx, fetchReq)
	if err != nil {
		return &pb.FetchBlobBatchResponse{
			ContentId: req.ContentId,
			Error:     err.Error(),
		}
	}

	latency := time.Since(start).Milliseconds()
	s.bytesServed.Add(result.Size)

	return &pb.FetchBlobBatchResponse{
		ContentId:      req.ContentId,
		Data:           result.Content,
		Size:           result.Size,
		FromCache:      result.FromCache,
		FetchLatencyMs: latency,
	}
}

// PrefetchBlobs queues blobs for background prefetching.
func (s *Service) PrefetchBlobs(ctx context.Context, req *pb.PrefetchRequest) (*pb.PrefetchResponse, error) {
	s.totalRequests.Add(1)

	accepted := int32(0)
	alreadyCached := int32(0)
	rejected := int32(0)

	for _, blobReq := range req.Blobs {
		// Check if already cached
		sourceType := protoToSourceType(blobReq.SourceType)
		backend, ok := s.registry.Get(sourceType)
		if !ok {
			rejected++
			continue
		}

		// Check if source is already warmed up (indicates likely cache hit)
		sourceKey := getSourceKey(blobReq)
		sources := backend.CachedSources()
		cached := false
		for _, src := range sources {
			if src == sourceKey {
				cached = true
				break
			}
		}
		if cached {
			alreadyCached++
			continue
		}

		// Queue for prefetch
		select {
		case s.prefetchQueue <- &prefetchJob{req: blobReq, submitted: time.Now()}:
			accepted++
		default:
			// Queue full
			rejected++
		}
	}

	return &pb.PrefetchResponse{
		JobId:         fmt.Sprintf("prefetch-%d", time.Now().UnixNano()),
		Accepted:      accepted,
		AlreadyCached: alreadyCached,
		Rejected:      rejected,
	}, nil
}

// CheckCache checks if blobs are in the fetcher's cache.
func (s *Service) CheckCache(ctx context.Context, req *pb.CheckCacheRequest) (*pb.CheckCacheResponse, error) {
	sourceType := protoToSourceType(req.SourceType)
	backend, ok := s.registry.Get(sourceType)
	if !ok {
		return nil, fmt.Errorf("unsupported source type: %s", sourceType)
	}

	cached := make(map[string]bool)
	sizes := make(map[string]int64)

	// Check cached sources
	cachedSources := backend.CachedSources()
	sourceSet := make(map[string]bool)
	for _, src := range cachedSources {
		sourceSet[src] = true
	}

	for _, contentID := range req.ContentIds {
		// Simple heuristic: if the source is cached, content might be available
		// For accurate check, would need to actually probe the backend
		cached[contentID] = sourceSet[contentID]
	}

	return &pb.CheckCacheResponse{
		Cached: cached,
		Sizes:  sizes,
	}, nil
}

// GetStats returns fetcher statistics.
func (s *Service) GetStats(ctx context.Context, req *pb.FetcherStatsRequest) (*pb.FetcherStatsResponse, error) {
	resp := &pb.FetcherStatsResponse{
		FetcherId:        s.fetcherID,
		UptimeSeconds:    int64(time.Since(s.startTime).Seconds()),
		TotalRequests:    s.totalRequests.Load(),
		ActiveFetches:    s.activeRequests.Load(),
		QueuedPrefetches: int64(len(s.prefetchQueue)),
		BytesServed:      s.bytesServed.Load(),
	}

	if req != nil && req.IncludeSourceStats {
		resp.SourceStats = make(map[string]*pb.SourceStats)
		for _, backend := range s.registry.All() {
			stats := backend.Stats()
			resp.SourceStats[backend.Type().String()] = &pb.SourceStats{
				Requests:     stats.Requests,
				Errors:       stats.Errors,
				BytesFetched: stats.BytesFetched,
				AvgLatencyMs: stats.AvgLatencyMs,
				CachedItems:  stats.CachedItems,
			}
			resp.CacheHits += stats.CacheHits
			resp.CacheMisses += stats.CacheMisses
			resp.CacheEntries += stats.CachedItems
			resp.CacheSizeBytes += stats.CacheBytes
		}
	}

	if resp.CacheHits+resp.CacheMisses > 0 {
		resp.CacheHitRate = float64(resp.CacheHits) / float64(resp.CacheHits+resp.CacheMisses)
	}

	return resp, nil
}

// prefetchWorker processes prefetch jobs in the background.
func (s *Service) prefetchWorker(id int) {
	defer s.prefetchWg.Done()

	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.prefetchQueue:
			s.processPrefetchJob(job)
		}
	}
}

func (s *Service) processPrefetchJob(job *prefetchJob) {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()

	sourceType := protoToSourceType(job.req.SourceType)
	backend, ok := s.registry.Get(sourceType)
	if !ok {
		return
	}

	sourceKey := getSourceKey(job.req)

	// Warmup the source (clone repo, download module, etc.)
	if err := backend.Warmup(ctx, sourceKey, job.req.SourceConfig); err != nil {
		s.logger.Warn("prefetch warmup failed",
			"source_key", sourceKey,
			"error", err,
		)
		return
	}

	// Actually fetch the blob to ensure it's cached
	fetchReq := &FetchRequest{
		ContentID:    job.req.ContentId,
		SourceKey:    sourceKey,
		SourceConfig: job.req.SourceConfig,
		RequestID:    job.req.RequestId,
		Priority:     int(job.req.Priority),
	}

	_, err := backend.FetchBlob(ctx, fetchReq)
	if err != nil {
		s.logger.Warn("prefetch failed",
			"content_id", job.req.ContentId,
			"error", err,
		)
		return
	}

	s.logger.Debug("prefetch completed",
		"content_id", job.req.ContentId,
		"latency_ms", time.Since(job.submitted).Milliseconds(),
	)
}

// Close shuts down the service.
func (s *Service) Close() error {
	s.cancel()
	close(s.prefetchQueue)
	s.prefetchWg.Wait()
	return nil
}

// Helper functions

func protoToSourceType(pt pb.SourceType) SourceType {
	switch pt {
	case pb.SourceType_SOURCE_TYPE_GIT:
		return SourceTypeGit
	case pb.SourceType_SOURCE_TYPE_GOMOD:
		return SourceTypeGoMod
	case pb.SourceType_SOURCE_TYPE_S3:
		return SourceTypeS3
	case pb.SourceType_SOURCE_TYPE_HTTP:
		return SourceTypeHTTP
	case pb.SourceType_SOURCE_TYPE_OCI:
		return SourceTypeOCI
	default:
		return SourceTypeUnknown
	}
}

func getSourceKey(req *pb.FetchBlobRequest) string {
	// Use storage_id if provided (for affinity routing)
	if req.StorageId != "" {
		return req.StorageId
	}
	// Fall back to source-specific key
	switch req.SourceType {
	case pb.SourceType_SOURCE_TYPE_GIT:
		return req.SourceConfig["repo_url"]
	case pb.SourceType_SOURCE_TYPE_GOMOD:
		return req.SourceConfig["module_path"]
	default:
		return req.ContentId
	}
}
