package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	pb "github.com/radryc/monofs/api/proto"
	monogit "github.com/radryc/monofs/internal/git"
	blob "github.com/radryc/monofs/internal/storage/blob"
	"google.golang.org/grpc"
)

// loggerAccessor interface to get logger from writer
type loggerAccessor interface {
	GetLogger() *slog.Logger
}

// Service implements the BlobFetcher gRPC service.
type Service struct {
	pb.UnimplementedBlobFetcherServer
	pb.UnimplementedRepoSyncWorkerServer

	fetcherID string
	registry  *Registry
	logger    *slog.Logger
	repoMgr   *monogit.RepoManager

	// Prefetch queue
	prefetchQueue chan *prefetchJob
	prefetchWg    sync.WaitGroup

	// Stats
	startTime             time.Time
	totalRequests         atomic.Int64
	bytesServed           atomic.Int64
	activeRequests        atomic.Int64
	syncTotalJobs         atomic.Int64
	syncActiveJobs        atomic.Int64
	syncDoneJobs          atomic.Int64
	syncFailedJobs        atomic.Int64
	syncProbes            atomic.Int64
	syncProbeFails        atomic.Int64
	syncPublishJobs       atomic.Int64
	syncPublishedRepos    atomic.Int64
	syncStagedBundles     atomic.Int64
	syncStagedBundleBytes atomic.Int64
	syncWorktreeBytes     atomic.Int64
	syncStageFails        atomic.Int64
	stagedBundlesMu       sync.RWMutex
	stagedBundles         map[string]*syncWorkerBundle

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

	// SyncRepoCacheDir stores temporary Git mirrors used by refresh probes.
	SyncRepoCacheDir string
}

func DefaultServiceConfig() ServiceConfig {
	return ServiceConfig{
		PrefetchWorkers:      4,
		PrefetchQueueSize:    1000,
		MaxConcurrentFetches: 20,
		StreamChunkSize:      64 * 1024, // 64KB
		SyncRepoCacheDir:     "",
	}
}

type prefetchJob struct {
	req       *pb.FetchBlobRequest
	submitted time.Time
}

// NewService creates a new fetcher service.
func NewService(fetcherID string, registry *Registry, config ServiceConfig, logger *slog.Logger) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	var repoMgr *monogit.RepoManager
	if config.SyncRepoCacheDir != "" {
		if err := os.MkdirAll(config.SyncRepoCacheDir, 0755); err == nil {
			repoMgr, _ = monogit.NewRepoManager(config.SyncRepoCacheDir)
		}
	}

	s := &Service{
		fetcherID:     fetcherID,
		registry:      registry,
		logger:        logger,
		repoMgr:       repoMgr,
		prefetchQueue: make(chan *prefetchJob, config.PrefetchQueueSize),
		stagedBundles: make(map[string]*syncWorkerBundle),
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
	pb.RegisterRepoSyncWorkerServer(server, s)
}

// FetchBlob handles synchronous blob fetch requests.
func (s *Service) FetchBlob(req *pb.FetchBlobRequest, stream pb.BlobFetcher_FetchBlobServer) error {
	s.totalRequests.Add(1)
	s.activeRequests.Add(1)
	defer s.activeRequests.Add(-1)

	ctx := stream.Context()
	sourceType := protoToSourceType(req.SourceType)

	// [CONTENT_AUDIT] Log incoming fetch request at service entry point
	s.logger.Debug("[CONTENT_AUDIT] fetcher_service_request",
		"content_id", req.ContentId,
		"source_type", sourceType.String(),
		"source_key", getSourceKey(req),
		"request_id", req.RequestId,
		"source_config", req.SourceConfig)

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
		fetcherBlobErrorsTotal.WithLabelValues(sourceType.String()).Inc()
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
	fetcherBlobRequestsTotal.WithLabelValues(sourceType.String()).Inc()
	fetcherBlobBytesTotal.WithLabelValues(sourceType.String()).Add(float64(totalSent))
	s.logger.Debug("[CONTENT_AUDIT] fetcher_service_complete",
		"content_id", req.ContentId,
		"size", size,
		"total_sent", totalSent,
		"source_type", sourceType.String())

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
		fetcherBlobErrorsTotal.WithLabelValues(sourceType.String()).Inc()
		return &pb.FetchBlobBatchResponse{
			ContentId: req.ContentId,
			Error:     err.Error(),
		}
	}

	latency := time.Since(start).Milliseconds()
	s.bytesServed.Add(result.Size)
	fetcherBlobRequestsTotal.WithLabelValues(sourceType.String()).Inc()
	fetcherBlobBytesTotal.WithLabelValues(sourceType.String()).Add(float64(result.Size))

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
	fetcherPrefetchRequestsTotal.Inc()

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
				CacheBytes:   stats.CacheBytes,
			}
			resp.CacheHits += stats.CacheHits
			resp.CacheMisses += stats.CacheMisses
			resp.CacheEntries += stats.CachedItems
			resp.CacheSizeBytes += stats.CacheBytes

			// Expose per-storage-ID blob counts for the blob backend.
			if bb, ok := backend.(*blob.BlobBackend); ok {
				for storageID, count := range bb.StorageStats() {
					resp.SourceStats["storage:"+storageID] = &pb.SourceStats{
						CachedItems: count,
					}
				}
			}
		}
	}

	if resp.CacheHits+resp.CacheMisses > 0 {
		resp.CacheHitRate = float64(resp.CacheHits) / float64(resp.CacheHits+resp.CacheMisses)
	}
	resp.SyncWorker = s.syncWorkerStats()

	return resp, nil
}

func (s *Service) ProbeWorkspaceRefresh(req *pb.ProbeWorkspaceRefreshRequest, stream pb.RepoSyncWorker_ProbeWorkspaceRefreshServer) error {
	start := time.Now()
	resultLabel := "succeeded"
	s.syncTotalJobs.Add(1)
	s.syncActiveJobs.Add(1)
	fetcherGitSyncActiveJobs.Inc()
	defer s.syncActiveJobs.Add(-1)
	defer fetcherGitSyncActiveJobs.Dec()
	defer func() {
		fetcherGitSyncDurationSeconds.WithLabelValues("refresh", resultLabel).Observe(time.Since(start).Seconds())
	}()

	ctx := stream.Context()
	jobFailed := false
	for _, repo := range req.GetRepositories() {
		s.syncProbes.Add(1)
		fetcherGitSyncRemoteOpsTotal.WithLabelValues("probe_refresh", "started").Inc()
		progress := s.probeWorkspaceRepository(ctx, repo)
		if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED ||
			progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR ||
			progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_AUTH_FAILED {
			jobFailed = true
			s.syncProbeFails.Add(1)
			fetcherGitSyncRemoteOpsTotal.WithLabelValues("probe_refresh", "failed").Inc()
		} else {
			fetcherGitSyncRemoteOpsTotal.WithLabelValues("probe_refresh", "succeeded").Inc()
		}
		if progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_DIVERGED || progress.GetStatus() == pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH {
			fetcherGitSyncConflictsTotal.WithLabelValues("refresh", strings.ToLower(strings.TrimPrefix(progress.GetStatus().String(), "REPO_SYNC_STATUS_"))).Inc()
		}
		if err := stream.Send(progress); err != nil {
			jobFailed = true
			resultLabel = "failed"
			s.syncProbeFails.Add(1)
			s.syncFailedJobs.Add(1)
			fetcherGitSyncJobsTotal.WithLabelValues("refresh", "failed").Inc()
			return err
		}
	}

	s.syncDoneJobs.Add(1)
	if jobFailed {
		resultLabel = "failed"
		s.syncFailedJobs.Add(1)
		fetcherGitSyncJobsTotal.WithLabelValues("refresh", "failed").Inc()
		return nil
	}
	fetcherGitSyncJobsTotal.WithLabelValues("refresh", "succeeded").Inc()
	return nil
}

func (s *Service) GetSyncWorkerStats(ctx context.Context, req *pb.SyncWorkerStatsRequest) (*pb.SyncWorkerStatsResponse, error) {
	return &pb.SyncWorkerStatsResponse{Stats: s.syncWorkerStats()}, nil
}

func (s *Service) syncWorkerStats() *pb.SyncWorkerStats {
	stats := &pb.SyncWorkerStats{
		TotalJobs:             s.syncTotalJobs.Load(),
		ActiveJobs:            s.syncActiveJobs.Load(),
		CompletedJobs:         s.syncDoneJobs.Load(),
		FailedJobs:            s.syncFailedJobs.Load(),
		RefreshProbes:         s.syncProbes.Load(),
		RefreshProbeFailures:  s.syncProbeFails.Load(),
		PublishJobs:           s.syncPublishJobs.Load(),
		PublishedRepositories: s.syncPublishedRepos.Load(),
		StagedBundles:         s.syncStagedBundles.Load(),
		StagedBundleBytes:     s.syncStagedBundleBytes.Load(),
		WorktreeBytes:         s.syncWorktreeBytes.Load(),
		BundleStageFailures:   s.syncStageFails.Load(),
	}
	if s.repoMgr != nil {
		if entries, err := os.ReadDir(s.config.SyncRepoCacheDir); err == nil {
			stats.GitCacheEntries = int64(len(entries))
		}
	}
	return stats
}

func (s *Service) probeWorkspaceRepository(ctx context.Context, repo *pb.WorkspaceRepositoryRef) *pb.RepoSyncProgress {
	progress := &pb.RepoSyncProgress{Repository: repo}
	if repo == nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = "repository is required"
		return progress
	}
	if s.repoMgr == nil {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = "sync worker git cache is not configured"
		return progress
	}
	if repo.GetRepoUrl() == "" {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
		progress.Message = "repo_url is required"
		return progress
	}

	branch := repo.GetBranch()
	if branch == "" {
		branch = "main"
	}
	repoID := repo.GetStorageId()
	if repoID == "" {
		repoID = repo.GetDisplayPath()
	}

	gitRepo, err := s.repoMgr.CloneOrOpen(ctx, repo.GetRepoUrl(), repoID, branch)
	if err != nil {
		progress.Status = mapRepoSyncError(err)
		progress.Message = err.Error()
		return progress
	}

	remoteCommit, err := resolveRepoBranchCommit(gitRepo, branch)
	if err != nil {
		progress.Status = mapRepoSyncError(err)
		progress.Message = err.Error()
		return progress
	}
	progress.RemoteCommit = remoteCommit

	if repo.GetBaseCommit() == "" {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR
		progress.Message = "base_commit is required"
		return progress
	}
	if remoteCommit == repo.GetBaseCommit() {
		progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_UNCHANGED
		progress.Message = "upstream branch unchanged"
		return progress
	}

	progress.Status = pb.RepoSyncStatus_REPO_SYNC_STATUS_ADVANCED
	progress.Message = "upstream branch advanced"
	return progress
}

func resolveRepoBranchCommit(repo *gogit.Repository, branch string) (string, error) {
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err == nil {
		return ref.Hash().String(), nil
	}
	ref, err = repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err == nil {
		return ref.Hash().String(), nil
	}
	return "", err
}

func mapRepoSyncError(err error) pb.RepoSyncStatus {
	if err == nil {
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_UNSPECIFIED
	}
	if err == plumbing.ErrReferenceNotFound {
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "authentication") || strings.Contains(message, "authorization") || strings.Contains(message, "access denied"):
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_AUTH_FAILED
	case strings.Contains(message, "not found") && strings.Contains(message, "reference"):
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_MISSING_BRANCH
	case strings.Contains(message, "timeout") || strings.Contains(message, "temporary") || strings.Contains(message, "connection"):
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_TRANSIENT_ERROR
	default:
		return pb.RepoSyncStatus_REPO_SYNC_STATUS_FAILED
	}
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
	case pb.SourceType_SOURCE_TYPE_BLOB:
		return SourceTypeBlob
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
	case pb.SourceType_SOURCE_TYPE_BLOB:
		return req.SourceConfig["storage_id"]
	default:
		return req.ContentId
	}
}

// StoreBlob stores a single blob on the fetcher.
func (s *Service) StoreBlob(ctx context.Context, req *pb.StoreBlobRequest) (*pb.StoreBlobResponse, error) {
	if req.BlobHash == "" {
		return &pb.StoreBlobResponse{
			Success:      false,
			ErrorMessage: "blob_hash is required",
		}, nil
	}
	if len(req.Content) == 0 {
		return &pb.StoreBlobResponse{
			Success:      false,
			ErrorMessage: "content is empty",
		}, nil
	}

	backend, ok := s.registry.Get(SourceTypeBlob)
	if !ok {
		return &pb.StoreBlobResponse{
			Success:      false,
			ErrorMessage: "blob backend not registered",
		}, nil
	}

	blobBackend, ok := backend.(*blob.BlobBackend)
	if !ok {
		return &pb.StoreBlobResponse{
			Success:      false,
			ErrorMessage: "blob backend type assertion failed",
		}, nil
	}

	if err := blobBackend.StoreBlob(req.BlobHash, req.Content); err != nil {
		s.logger.Error("failed to store blob",
			"blob_hash", req.BlobHash,
			"size", len(req.Content),
			"error", err)
		return &pb.StoreBlobResponse{
			Success:      false,
			ErrorMessage: err.Error(),
		}, nil
	}

	fetcherStoreBlobBytesTotal.Add(float64(len(req.Content)))
	s.logger.Debug("stored blob",
		"blob_hash", req.BlobHash,
		"size", len(req.Content))

	return &pb.StoreBlobResponse{
		Success: true,
		Size:    int64(len(req.Content)),
	}, nil
}

// StoreBlobBatchStream receives a stream of StoreBlobEntry messages and
// packs them into archive(s), splitting at ~512 MB. This supports
// arbitrarily large pushes without gRPC message size limits.
// Large blobs may arrive as multiple messages sharing the same blob_hash
// with incrementing chunk_index; they are reassembled before archiving.
func (s *Service) StoreBlobBatchStream(stream pb.BlobFetcher_StoreBlobBatchStreamServer) error {
	backend, ok := s.registry.Get(SourceTypeBlob)
	if !ok {
		return stream.SendAndClose(&pb.StoreBlobBatchResponse{
			Success:      false,
			ErrorMessage: "blob backend not registered",
		})
	}

	blobBackend, ok := backend.(*blob.BlobBackend)
	if !ok {
		return stream.SendAndClose(&pb.StoreBlobBatchResponse{
			Success:      false,
			ErrorMessage: "blob backend type assertion failed",
		})
	}

	writer, err := blobBackend.NewStoreBlobBatchWriter()
	if err != nil {
		return stream.SendAndClose(&pb.StoreBlobBatchResponse{
			Success:      false,
			ErrorMessage: err.Error(),
		})
	}

	var received int

	// Chunk reassembly state for a single in-progress large blob.
	var chunkedHash string
	var chunkedBuf []byte

	for {
		entry, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Seal what we have so far, then report the error
			result, _ := writer.Finish()
			s.logger.Error("stream receive error during blob batch",
				"received", received,
				"stored", result.Stored,
				"error", err)
			return stream.SendAndClose(&pb.StoreBlobBatchResponse{
				Success:         false,
				ErrorMessage:    fmt.Sprintf("stream error after %d entries: %v", received, err),
				Stored:          int32(result.Stored),
				Skipped:         int32(result.Skipped),
				Failed:          int32(result.Failed),
				ArchiveBytes:    result.ArchiveBytes,
				ArchivesCreated: int32(result.ArchivesCreated),
			})
		}
		received++

		// Handle chunked blobs: if ChunkIndex > 0 or IsLast is explicitly
		// false on chunk 0, the blob is split across messages.
		if entry.ChunkIndex == 0 && entry.IsLast {
			// Fast path — single-message blob (common case)
			// Flush any pending chunked blob first (shouldn't happen in
			// well-formed streams, but be safe).
			if len(chunkedBuf) > 0 {
				writer.AddBlob(chunkedHash, chunkedBuf)
				chunkedBuf = nil
				chunkedHash = ""
			}
			writer.AddBlob(entry.BlobHash, entry.Content)
			continue
		}

		// Multi-chunk blob
		if entry.ChunkIndex == 0 {
			// First chunk of a new large blob — flush any previous
			if len(chunkedBuf) > 0 {
				writer.AddBlob(chunkedHash, chunkedBuf)
			}
			chunkedHash = entry.BlobHash
			if entry.TotalSize > 0 {
				chunkedBuf = make([]byte, 0, entry.TotalSize)
			} else {
				chunkedBuf = make([]byte, 0, len(entry.Content)*2)
			}
			chunkedBuf = append(chunkedBuf, entry.Content...)
		} else {
			// Continuation chunk
			chunkedBuf = append(chunkedBuf, entry.Content...)
		}

		if entry.IsLast {
			writer.AddBlob(chunkedHash, chunkedBuf)
			chunkedBuf = nil
			chunkedHash = ""
		}
	}

	// Flush any trailing chunked blob (stream ended without is_last)
	if len(chunkedBuf) > 0 {
		writer.AddBlob(chunkedHash, chunkedBuf)
	}

	result, err := writer.Finish()
	if err != nil {
		s.logger.Error("failed to finish blob batch",
			"received", received,
			"error", err)
		return stream.SendAndClose(&pb.StoreBlobBatchResponse{
			Success:         false,
			ErrorMessage:    err.Error(),
			Stored:          int32(result.Stored),
			Skipped:         int32(result.Skipped),
			Failed:          int32(result.Failed),
			ArchiveBytes:    result.ArchiveBytes,
			ArchivesCreated: int32(result.ArchivesCreated),
		})
	}

	s.logger.Info("stored blob batch (stream)",
		"received", received,
		"stored", result.Stored,
		"skipped", result.Skipped,
		"archives", result.ArchivesCreated,
		"archive_bytes", result.ArchiveBytes)

	return stream.SendAndClose(&pb.StoreBlobBatchResponse{
		Success:         true,
		Stored:          int32(result.Stored),
		Skipped:         int32(result.Skipped),
		Failed:          int32(result.Failed),
		ArchiveBytes:    result.ArchiveBytes,
		ArchivesCreated: int32(result.ArchivesCreated),
	})
}

// DeleteBlobs removes blobs from the fetcher's index and optionally
// cleans up empty archive files.
func (s *Service) DeleteBlobs(ctx context.Context, req *pb.DeleteBlobsRequest) (*pb.DeleteBlobsResponse, error) {
	if len(req.BlobHashes) == 0 {
		return &pb.DeleteBlobsResponse{Success: true}, nil
	}

	backend, ok := s.registry.Get(SourceTypeBlob)
	if !ok {
		return &pb.DeleteBlobsResponse{
			Success:      false,
			ErrorMessage: "blob backend not registered",
		}, nil
	}

	blobBackend, ok := backend.(*blob.BlobBackend)
	if !ok {
		return &pb.DeleteBlobsResponse{
			Success:      false,
			ErrorMessage: "blob backend type assertion failed",
		}, nil
	}

	result := blobBackend.DeleteBlobs(req.BlobHashes, req.Compact)

	s.logger.Info("deleted blobs",
		"requested", len(req.BlobHashes),
		"deleted", result.Deleted,
		"not_found", result.NotFound,
		"archives_removed", result.ArchivesRemoved)

	return &pb.DeleteBlobsResponse{
		Success:         true,
		Deleted:         int32(result.Deleted),
		NotFound:        int32(result.NotFound),
		ArchivesRemoved: int32(result.ArchivesRemoved),
	}, nil
}

// StoreArchive receives a packager archive stream and stores it on the fetcher.
func (s *Service) StoreArchive(stream pb.BlobFetcher_StoreArchiveServer) error {
	backend, ok := s.registry.Get(SourceTypeBlob)
	if !ok {
		return fmt.Errorf("blob backend not registered")
	}

	blobBackend, ok := backend.(*blob.BlobBackend)
	if !ok {
		return fmt.Errorf("blob backend type assertion failed")
	}

	var storageID string
	var chunkIndex int32
	var buf bytes.Buffer

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("receive archive chunk: %w", err)
		}

		if storageID == "" {
			storageID = chunk.StorageId
			chunkIndex = chunk.ChunkIndex
		}

		buf.Write(chunk.Data)

		if chunk.IsLast {
			break
		}
	}

	if storageID == "" {
		return fmt.Errorf("no storage_id provided in archive stream")
	}

	totalBytes, filesIndexed, err := blobBackend.StoreArchive(storageID, int(chunkIndex), &buf)
	if err != nil {
		return fmt.Errorf("store archive: %w", err)
	}

	s.logger.Info("stored archive",
		"storage_id", storageID,
		"chunk_index", chunkIndex,
		"total_bytes", totalBytes,
		"files_indexed", filesIndexed)
	fetcherStoreArchiveBytesTotal.Add(float64(totalBytes))
	fetcherStoreArchivesTotal.Inc()

	return stream.SendAndClose(&pb.StoreArchiveResponse{
		Success:      true,
		TotalBytes:   totalBytes,
		FilesIndexed: int64(filesIndexed),
	})
}
