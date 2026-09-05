// Package search provides code search functionality using Zoekt.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/client"
)

const (
	// Database buckets
	bucketJobs  = "jobs"  // Job state persistence
	bucketRepos = "repos" // Repository metadata
	bucketStats = "stats" // Service statistics
)

// Job represents an indexing job
type Job struct {
	ID           string         `json:"id"`
	StorageID    string         `json:"storage_id"`
	DisplayPath  string         `json:"display_path"`
	RepoURL      string         `json:"repo_url"`
	Branch       string         `json:"branch"`
	Status       pb.IndexStatus `json:"status"`
	Progress     float32        `json:"progress"`
	FilesCount   int64          `json:"files_count"`
	IndexSize    int64          `json:"index_size"`
	QueuedAt     time.Time      `json:"queued_at"`
	StartedAt    time.Time      `json:"started_at"`
	CompletedAt  time.Time      `json:"completed_at"`
	ErrorMessage string         `json:"error_message"`
}

// RepoMeta stores repository metadata for search
type RepoMeta struct {
	StorageID   string    `json:"storage_id"`
	DisplayPath string    `json:"display_path"`
	RepoURL     string    `json:"repo_url"`
	Branch      string    `json:"branch"`
	FilesCount  int64     `json:"files_count"`
	IndexSize   int64     `json:"index_size"`
	LastIndexed time.Time `json:"last_indexed"`
}

// ServiceStats tracks service statistics
type ServiceStats struct {
	SearchesTotal       int64     `json:"searches_total"`
	SearchDurationTotal int64     `json:"search_duration_total_ms"`
	StartedAt           time.Time `json:"started_at"`
	JobsQueued          int64     `json:"jobs_queued"`
	JobsCompleted       int64     `json:"jobs_completed"`
	JobsFailed          int64     `json:"jobs_failed"`
	JobsRejected        int64     `json:"jobs_rejected"`
}

// Service implements the MonoFSSearch gRPC service
type Service struct {
	pb.UnimplementedMonoFSSearchServer

	mu       sync.RWMutex
	indexDir string
	cacheDir string
	db       *nutsdb.DB
	indexer  *Indexer
	logger   *slog.Logger

	// Job queue
	jobQueue   chan *Job
	activeJobs sync.Map // jobID -> *Job
	jobsWg     sync.WaitGroup

	// Stats
	stats       ServiceStats
	searchCount atomic.Int64

	// Shutdown
	stopChan chan struct{}
	workers  int
}

// Config holds service configuration
type Config struct {
	IndexDir   string // Directory for Zoekt indexes
	CacheDir   string // Directory for git clones during indexing
	Workers    int    // Number of concurrent indexing workers
	QueueSize  int    // Size of job queue
	RouterAddr string // Router address for cluster access (enables fetching from storage nodes)
	Logger     *slog.Logger
}

// DefaultConfig returns default configuration
func DefaultConfig() Config {
	return Config{
		IndexDir:   "/data/index",
		CacheDir:   "/data/cache",
		Workers:    2,
		QueueSize:  100,
		RouterAddr: "",
		Logger:     slog.Default(),
	}
}

// NewService creates a new search service
func NewService(cfg Config) (*Service, error) {
	// Create directories
	if err := os.MkdirAll(cfg.IndexDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create index dir: %w", err)
	}
	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache dir: %w", err)
	}

	// Open NutsDB for state persistence
	dbPath := filepath.Join(cfg.IndexDir, "state.db")
	dbOpts := nutsdb.DefaultOptions
	dbOpts.Dir = dbPath
	dbOpts.SegmentSize = 8 * 1024 * 1024 // 8MB segments

	db, err := nutsdb.Open(dbOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to open state db: %w", err)
	}

	// Create buckets
	if err := db.Update(func(tx *nutsdb.Tx) error {
		for _, bucket := range []string{bucketJobs, bucketRepos, bucketStats} {
			if err := tx.NewBucket(nutsdb.DataStructureBTree, bucket); err != nil && err != nutsdb.ErrBucketAlreadyExist {
				return err
			}
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create buckets: %w", err)
	}

	// Create MonoFS client if router address is provided
	var monofsClient client.MonoFSClient
	if cfg.RouterAddr != "" {
		cfg.Logger.Info("connecting to MonoFS cluster for file fetching",
			"router_addr", cfg.RouterAddr)

		// Try to connect with retries (router might not be up yet due to startup order)
		var shardedClient *client.ShardedClient
		var connectErr error
		for attempt := 1; attempt <= 5; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			shardedClient, connectErr = client.NewShardedClient(ctx, client.ShardedClientConfig{
				RouterAddr:           cfg.RouterAddr,
				ClientID:             "search-indexer",
				RefreshInterval:      60 * time.Second,
				RPCTimeout:           5 * time.Minute, // Long timeout for large files
				UseExternalAddresses: false,           // Use internal cluster addresses
				Logger:               cfg.Logger,
				Hostname:             "search-service",
				Version:              "indexer",
			})
			cancel()

			if connectErr == nil {
				monofsClient = shardedClient
				cfg.Logger.Info("connected to MonoFS cluster", "attempt", attempt)
				break
			}

			cfg.Logger.Warn("failed to connect to MonoFS cluster, retrying...",
				"attempt", attempt,
				"error", connectErr)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}

		if connectErr != nil {
			cfg.Logger.Warn("could not connect to MonoFS cluster after retries, will use direct git clone/go mod download",
				"router_addr", cfg.RouterAddr,
				"error", connectErr)
		}
	} else {
		cfg.Logger.Warn("no router address configured, will use direct git clone/go mod download (requires external network access)")
	}

	// Create indexer
	indexer, err := NewIndexer(cfg.IndexDir, cfg.CacheDir, monofsClient, cfg.Logger)
	if err != nil {
		if monofsClient != nil {
			monofsClient.Close()
		}
		db.Close()
		return nil, fmt.Errorf("failed to create indexer: %w", err)
	}

	s := &Service{
		indexDir: cfg.IndexDir,
		cacheDir: cfg.CacheDir,
		db:       db,
		indexer:  indexer,
		logger:   cfg.Logger,
		jobQueue: make(chan *Job, cfg.QueueSize),
		stopChan: make(chan struct{}),
		workers:  cfg.Workers,
		stats: ServiceStats{
			StartedAt: time.Now(),
		},
	}

	// Load stats from DB
	s.loadStats()

	// Load all repo mappings into indexer for search results
	s.loadRepoMappings()

	// Start workers
	for i := 0; i < cfg.Workers; i++ {
		go s.worker(i)
	}

	// Restore pending jobs from DB
	s.restorePendingJobs()

	cfg.Logger.Info("search service initialized",
		"index_dir", cfg.IndexDir,
		"cache_dir", cfg.CacheDir,
		"workers", cfg.Workers)

	return s, nil
}

// Close shuts down the service
func (s *Service) Close() error {
	close(s.stopChan)
	s.jobsWg.Wait()
	s.saveStats()
	if err := s.indexer.Close(); err != nil {
		s.logger.Warn("failed to close indexer", "error", err)
	}
	return s.db.Close()
}

// worker processes indexing jobs
func (s *Service) worker(id int) {
	s.logger.Info("indexing worker started", "worker_id", id)

	for {
		select {
		case <-s.stopChan:
			s.logger.Info("indexing worker stopping", "worker_id", id)
			return
		case job := <-s.jobQueue:
			s.processJob(job)
		}
	}
}

// processJob executes an indexing job
func (s *Service) processJob(job *Job) {
	s.jobsWg.Add(1)
	defer s.jobsWg.Done()

	s.logger.Info("processing indexing job",
		"job_id", job.ID,
		"storage_id", job.StorageID,
		"display_path", job.DisplayPath)

	// Update status to indexing
	job.Status = pb.IndexStatus_INDEX_STATUS_INDEXING
	job.StartedAt = time.Now()
	s.activeJobs.Store(job.ID, job)
	s.saveJob(job)

	// Create context with timeout for indexing (30 minutes max per repo)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Perform indexing
	result, err := s.indexer.IndexRepository(ctx, IndexRequest{
		StorageID:   job.StorageID,
		DisplayPath: job.DisplayPath,
		RepoURL:     job.RepoURL,
		Ref:         job.Branch,
	})

	// Update job status based on result
	if err != nil {
		job.Status = pb.IndexStatus_INDEX_STATUS_ERROR
		job.ErrorMessage = err.Error()
		s.logger.Error("indexing failed",
			"job_id", job.ID,
			"storage_id", job.StorageID,
			"error", err)

		// Track failure
		s.mu.Lock()
		s.stats.JobsFailed++
		s.mu.Unlock()
	} else {
		job.Status = pb.IndexStatus_INDEX_STATUS_READY
		job.FilesCount = result.FilesIndexed
		job.IndexSize = result.IndexSizeBytes
		job.Progress = 1.0
		s.logger.Info("indexing completed",
			"job_id", job.ID,
			"storage_id", job.StorageID,
			"files", result.FilesIndexed,
			"size", result.IndexSizeBytes)

		// Track completion
		s.mu.Lock()
		s.stats.JobsCompleted++
		s.mu.Unlock()

		// Save repo metadata
		s.saveRepoMeta(&RepoMeta{
			StorageID:   job.StorageID,
			DisplayPath: job.DisplayPath,
			RepoURL:     job.RepoURL,
			Branch:      job.Branch,
			FilesCount:  result.FilesIndexed,
			IndexSize:   result.IndexSizeBytes,
			LastIndexed: time.Now(),
		})
	}

	job.CompletedAt = time.Now()
	s.activeJobs.Delete(job.ID)
	s.saveJob(job)
}

// saveJob persists job state to DB
func (s *Service) saveJob(job *Job) {
	data, err := json.Marshal(job)
	if err != nil {
		s.logger.Error("failed to marshal job", "error", err)
		return
	}

	if err := s.db.Update(func(tx *nutsdb.Tx) error {
		return tx.Put(bucketJobs, []byte(job.StorageID), data, 0)
	}); err != nil {
		s.logger.Error("failed to save job", "error", err)
	}
}

// loadJob loads job state from DB
func (s *Service) loadJob(storageID string) (*Job, error) {
	var job Job
	err := s.db.View(func(tx *nutsdb.Tx) error {
		val, err := tx.Get(bucketJobs, []byte(storageID))
		if err != nil {
			return err
		}
		return json.Unmarshal(val, &job)
	})
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// saveRepoMeta persists repository metadata
func (s *Service) saveRepoMeta(meta *RepoMeta) {
	data, err := json.Marshal(meta)
	if err != nil {
		s.logger.Error("failed to marshal repo meta", "error", err)
		return
	}

	if err := s.db.Update(func(tx *nutsdb.Tx) error {
		return tx.Put(bucketRepos, []byte(meta.StorageID), data, 0)
	}); err != nil {
		s.logger.Error("failed to save repo meta", "error", err)
	}
}

// loadRepoMeta loads repository metadata
func (s *Service) loadRepoMeta(storageID string) (*RepoMeta, error) {
	var meta RepoMeta
	err := s.db.View(func(tx *nutsdb.Tx) error {
		val, err := tx.Get(bucketRepos, []byte(storageID))
		if err != nil {
			return err
		}
		return json.Unmarshal(val, &meta)
	})
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// loadStats loads service statistics from DB
func (s *Service) loadStats() {
	if err := s.db.View(func(tx *nutsdb.Tx) error {
		val, err := tx.Get(bucketStats, []byte("stats"))
		if err != nil {
			return err
		}
		return json.Unmarshal(val, &s.stats)
	}); err != nil {
		// Stats not found, use defaults
		s.stats = ServiceStats{StartedAt: time.Now()}
	}
}

// loadRepoMappings loads all repository DisplayPath->StorageID mappings into the indexer.
// This is needed so that search results include StorageID.
func (s *Service) loadRepoMappings() {
	count := 0
	s.db.View(func(tx *nutsdb.Tx) error {
		_, values, err := tx.GetAll(bucketRepos)
		if err != nil {
			return err
		}

		for _, val := range values {
			var meta RepoMeta
			if err := json.Unmarshal(val, &meta); err != nil {
				continue
			}
			s.indexer.RegisterStorageMapping(meta.DisplayPath, meta.StorageID)
			count++
		}
		return nil
	})

	if count > 0 {
		s.logger.Info("loaded repository mappings", "count", count)
	}
}

// saveStats persists service statistics
func (s *Service) saveStats() {
	data, err := json.Marshal(s.stats)
	if err != nil {
		return
	}

	s.db.Update(func(tx *nutsdb.Tx) error {
		return tx.Put(bucketStats, []byte("stats"), data, 0)
	})
}

// restorePendingJobs restores incomplete jobs after restart
func (s *Service) restorePendingJobs() {
	s.db.View(func(tx *nutsdb.Tx) error {
		keys, values, err := tx.GetAll(bucketJobs)
		if err != nil {
			return err
		}

		for i, val := range values {
			var job Job
			if err := json.Unmarshal(val, &job); err != nil {
				s.logger.Warn("failed to unmarshal job", "key", string(keys[i]), "error", err)
				continue
			}

			// Re-queue incomplete jobs
			if job.Status == pb.IndexStatus_INDEX_STATUS_QUEUED ||
				job.Status == pb.IndexStatus_INDEX_STATUS_INDEXING {
				job.Status = pb.IndexStatus_INDEX_STATUS_QUEUED
				select {
				case s.jobQueue <- &job:
					s.logger.Info("restored pending job",
						"storage_id", job.StorageID,
						"display_path", job.DisplayPath)
				default:
					s.logger.Warn("job queue full, skipping restore",
						"storage_id", job.StorageID)
				}
			}
		}
		return nil
	})
}

// generateJobID creates a unique job ID
func generateJobID(storageID string) string {
	prefix := storageID
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
