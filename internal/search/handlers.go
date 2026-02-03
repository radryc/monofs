// Package search provides gRPC handlers for the search service.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
)

// IndexRepository implements the IndexRepository RPC
func (s *Service) IndexRepository(ctx context.Context, req *pb.IndexRequest) (*pb.IndexResponse, error) {
	s.logger.Info("received index request",
		"storage_id", req.StorageId,
		"display_path", req.DisplayPath,
		"source", req.Source)

	// Create job
	job := &Job{
		ID:          generateJobID(req.StorageId),
		StorageID:   req.StorageId,
		DisplayPath: req.DisplayPath,
		RepoURL:     req.Source,
		Branch:      req.Ref,
		Status:      pb.IndexStatus_INDEX_STATUS_QUEUED,
		QueuedAt:    time.Now(),
	}

	// Try to queue the job
	select {
	case s.jobQueue <- job:
		s.mu.Lock()
		s.stats.JobsQueued++
		s.mu.Unlock()
		s.saveJob(job)
		s.logger.Info("job queued",
			"job_id", job.ID,
			"storage_id", req.StorageId)
		return &pb.IndexResponse{
			Queued:  true,
			Message: "Repository queued for indexing",
			JobId:   job.ID,
		}, nil
	default:
		// Track rejection
		s.mu.Lock()
		s.stats.JobsRejected++
		s.mu.Unlock()

		// Save rejected job so it appears in UI as failed
		job.Status = pb.IndexStatus_INDEX_STATUS_ERROR
		job.ErrorMessage = "Job queue is full - please retry later"
		job.CompletedAt = time.Now()
		s.saveJob(job)

		s.logger.Warn("job queue full, rejected indexing request",
			"storage_id", req.StorageId,
			"display_path", req.DisplayPath)

		return &pb.IndexResponse{
			Queued:  false,
			Message: "Job queue is full, please try again later",
			JobId:   job.ID,
		}, nil
	}
}

// Search implements the Search RPC
func (s *Service) Search(ctx context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	start := time.Now()

	s.logger.Debug("search request",
		"query", req.Query,
		"storage_id", req.StorageId,
		"max_results", req.MaxResults)

	results, err := s.indexer.Search(ctx, SearchRequest{
		Query:         req.Query,
		StorageID:     req.StorageId,
		MaxResults:    int(req.MaxResults),
		CaseSensitive: req.CaseSensitive,
		Regex:         req.Regex,
		FilePatterns:  req.FilePatterns,
	})
	if err != nil {
		return nil, err
	}

	// Update stats
	s.searchCount.Add(1)
	s.mu.Lock()
	s.stats.SearchesTotal++
	s.stats.SearchDurationTotal += results.Duration.Milliseconds()
	s.mu.Unlock()

	// Convert results
	pbResults := make([]*pb.SearchResult, 0, len(results.Results))
	for _, r := range results.Results {
		matches := make([]*pb.MatchRange, 0, len(r.Matches))
		for _, m := range r.Matches {
			matches = append(matches, &pb.MatchRange{
				Start: int32(m.Start),
				End:   int32(m.End),
			})
		}

		pbResults = append(pbResults, &pb.SearchResult{
			StorageId:     r.StorageID,
			DisplayPath:   r.DisplayPath,
			FilePath:      r.FilePath,
			LineNumber:    int32(r.LineNumber),
			LineContent:   r.LineContent,
			Matches:       matches,
			BeforeContext: r.BeforeContext,
			AfterContext:  r.AfterContext,
		})
	}

	return &pb.SearchResponse{
		Results:       pbResults,
		TotalMatches:  results.TotalMatches,
		FilesSearched: results.FilesSearched,
		DurationMs:    time.Since(start).Milliseconds(),
		Truncated:     results.Truncated,
	}, nil
}

// GetIndexStatus implements the GetIndexStatus RPC
func (s *Service) GetIndexStatus(ctx context.Context, req *pb.IndexStatusRequest) (*pb.IndexStatusResponse, error) {
	// Check if there's an active job
	var activeJob *Job
	s.activeJobs.Range(func(key, value interface{}) bool {
		j := value.(*Job)
		if j.StorageID == req.StorageId {
			activeJob = j
			return false
		}
		return true
	})

	if activeJob != nil {
		return &pb.IndexStatusResponse{
			StorageId:   activeJob.StorageID,
			DisplayPath: activeJob.DisplayPath,
			Status:      activeJob.Status,
			Progress:    activeJob.Progress,
			QueuedAt:    activeJob.QueuedAt.Format(time.RFC3339),
			StartedAt:   activeJob.StartedAt.Format(time.RFC3339),
		}, nil
	}

	// Check persisted job state
	job, err := s.loadJob(req.StorageId)
	if err == nil {
		return &pb.IndexStatusResponse{
			StorageId:      job.StorageID,
			DisplayPath:    job.DisplayPath,
			Status:         job.Status,
			FilesCount:     job.FilesCount,
			IndexSizeBytes: job.IndexSize,
			LastIndexed:    job.CompletedAt.Format(time.RFC3339),
			ErrorMessage:   job.ErrorMessage,
			Progress:       job.Progress,
			QueuedAt:       job.QueuedAt.Format(time.RFC3339),
			StartedAt:      job.StartedAt.Format(time.RFC3339),
		}, nil
	}

	// Check if index exists but no job record
	if s.indexer.IndexExists(req.StorageId) {
		size, _ := s.indexer.GetIndexSize(req.StorageId)
		meta, _ := s.loadRepoMeta(req.StorageId)
		if meta != nil {
			return &pb.IndexStatusResponse{
				StorageId:      meta.StorageID,
				DisplayPath:    meta.DisplayPath,
				Status:         pb.IndexStatus_INDEX_STATUS_READY,
				FilesCount:     meta.FilesCount,
				IndexSizeBytes: size,
				LastIndexed:    meta.LastIndexed.Format(time.RFC3339),
			}, nil
		}
	}

	return &pb.IndexStatusResponse{
		StorageId: req.StorageId,
		Status:    pb.IndexStatus_INDEX_STATUS_NOT_FOUND,
	}, nil
}

// ListIndexes implements the ListIndexes RPC
func (s *Service) ListIndexes(ctx context.Context, req *pb.ListIndexesRequest) (*pb.ListIndexesResponse, error) {
	var indexes []*pb.IndexStatusResponse

	// Get all repo metadata
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

			// Check for active job
			status := pb.IndexStatus_INDEX_STATUS_READY
			var progress float32 = 1.0
			var errMsg string

			s.activeJobs.Range(func(key, value interface{}) bool {
				j := value.(*Job)
				if j.StorageID == meta.StorageID {
					status = j.Status
					progress = j.Progress
					errMsg = j.ErrorMessage
					return false
				}
				return true
			})

			// Check persisted job for error state
			if job, err := s.loadJob(meta.StorageID); err == nil {
				if job.Status == pb.IndexStatus_INDEX_STATUS_ERROR {
					status = job.Status
					errMsg = job.ErrorMessage
				}
			}

			indexes = append(indexes, &pb.IndexStatusResponse{
				StorageId:      meta.StorageID,
				DisplayPath:    meta.DisplayPath,
				Status:         status,
				FilesCount:     meta.FilesCount,
				IndexSizeBytes: meta.IndexSize,
				LastIndexed:    meta.LastIndexed.Format(time.RFC3339),
				ErrorMessage:   errMsg,
				Progress:       progress,
			})
		}
		return nil
	})

	// Add queued jobs that don't have metadata yet
	s.activeJobs.Range(func(key, value interface{}) bool {
		j := value.(*Job)
		// Check if already in list
		found := false
		for _, idx := range indexes {
			if idx.StorageId == j.StorageID {
				found = true
				break
			}
		}
		if !found {
			indexes = append(indexes, &pb.IndexStatusResponse{
				StorageId:   j.StorageID,
				DisplayPath: j.DisplayPath,
				Status:      j.Status,
				Progress:    j.Progress,
				QueuedAt:    j.QueuedAt.Format(time.RFC3339),
				StartedAt:   j.StartedAt.Format(time.RFC3339),
			})
		}
		return true
	})

	// Add failed jobs that don't have repo metadata (never successfully indexed)
	s.db.View(func(tx *nutsdb.Tx) error {
		_, values, err := tx.GetAll(bucketJobs)
		if err != nil {
			return err
		}

		for _, val := range values {
			var job Job
			if err := json.Unmarshal(val, &job); err != nil {
				continue
			}

			// Only include ERROR jobs that aren't already in the list
			if job.Status != pb.IndexStatus_INDEX_STATUS_ERROR {
				continue
			}

			found := false
			for _, idx := range indexes {
				if idx.StorageId == job.StorageID {
					found = true
					break
				}
			}

			if !found {
				indexes = append(indexes, &pb.IndexStatusResponse{
					StorageId:    job.StorageID,
					DisplayPath:  job.DisplayPath,
					Status:       job.Status,
					ErrorMessage: job.ErrorMessage,
					QueuedAt:     job.QueuedAt.Format(time.RFC3339),
					StartedAt:    job.StartedAt.Format(time.RFC3339),
					LastIndexed:  job.CompletedAt.Format(time.RFC3339),
				})
			}
		}
		return nil
	})

	return &pb.ListIndexesResponse{
		Indexes: indexes,
	}, nil
}

// RebuildIndex implements the RebuildIndex RPC
func (s *Service) RebuildIndex(ctx context.Context, req *pb.RebuildIndexRequest) (*pb.RebuildIndexResponse, error) {
	var storageID, displayPath, repoURL, branch string

	// Try to load existing metadata first
	meta, err := s.loadRepoMeta(req.StorageId)
	if err == nil {
		storageID = meta.StorageID
		displayPath = meta.DisplayPath
		repoURL = meta.RepoURL
		branch = meta.Branch
	} else {
		// Fall back to job data (for failed jobs that never created metadata)
		job, err := s.loadJob(req.StorageId)
		if err != nil {
			return &pb.RebuildIndexResponse{
				Queued:  false,
				Message: "Repository not found",
			}, nil
		}
		storageID = job.StorageID
		displayPath = job.DisplayPath
		repoURL = job.RepoURL
		branch = job.Branch
	}

	// Delete existing index if force rebuild
	if req.Force {
		s.indexer.DeleteIndex(req.StorageId)
	}

	// Create new indexing job
	job := &Job{
		ID:          generateJobID(storageID),
		StorageID:   storageID,
		DisplayPath: displayPath,
		RepoURL:     repoURL,
		Branch:      branch,
		Status:      pb.IndexStatus_INDEX_STATUS_QUEUED,
		QueuedAt:    time.Now(),
	}

	select {
	case s.jobQueue <- job:
		s.saveJob(job)
		s.logger.Info("rebuild job queued",
			"job_id", job.ID,
			"storage_id", req.StorageId)
		return &pb.RebuildIndexResponse{
			Queued:  true,
			Message: "Repository queued for re-indexing",
			JobId:   job.ID,
		}, nil
	default:
		return &pb.RebuildIndexResponse{
			Queued:  false,
			Message: "Job queue is full, please try again later",
		}, nil
	}
}

// RebuildAllIndexes implements the RebuildAllIndexes RPC
func (s *Service) RebuildAllIndexes(ctx context.Context, req *pb.RebuildAllIndexesRequest) (*pb.RebuildAllIndexesResponse, error) {
	var queuedCount int32

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

			// Delete existing index if force
			if req.Force {
				s.indexer.DeleteIndex(meta.StorageID)
			}

			// Create job
			job := &Job{
				ID:          generateJobID(meta.StorageID),
				StorageID:   meta.StorageID,
				DisplayPath: meta.DisplayPath,
				RepoURL:     meta.RepoURL,
				Branch:      meta.Branch,
				Status:      pb.IndexStatus_INDEX_STATUS_QUEUED,
				QueuedAt:    time.Now(),
			}

			select {
			case s.jobQueue <- job:
				s.saveJob(job)
				queuedCount++
			default:
				// Queue full, stop adding
				return nil
			}
		}
		return nil
	})

	return &pb.RebuildAllIndexesResponse{
		QueuedCount: queuedCount,
		Message:     fmt.Sprintf("Queued %d repositories for re-indexing", queuedCount),
	}, nil
}

// DeleteIndex implements the DeleteIndex RPC
func (s *Service) DeleteIndex(ctx context.Context, req *pb.DeleteIndexRequest) (*pb.DeleteIndexResponse, error) {
	// Remove from indexer
	if err := s.indexer.DeleteIndex(req.StorageId); err != nil {
		return &pb.DeleteIndexResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to delete index: %v", err),
		}, nil
	}

	// Remove metadata
	s.db.Update(func(tx *nutsdb.Tx) error {
		tx.Delete(bucketRepos, []byte(req.StorageId))
		tx.Delete(bucketJobs, []byte(req.StorageId))
		return nil
	})

	s.logger.Info("deleted index", "storage_id", req.StorageId)

	return &pb.DeleteIndexResponse{
		Success: true,
		Message: "Index deleted successfully",
	}, nil
}

// GetStats implements the GetStats RPC
func (s *Service) GetStats(ctx context.Context, req *pb.StatsRequest) (*pb.StatsResponse, error) {
	s.mu.RLock()
	stats := s.stats
	s.mu.RUnlock()

	var totalIndexes int64
	var totalFiles int64
	var totalSize int64
	var errorCount int64

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
			totalIndexes++
			totalFiles += meta.FilesCount
			totalSize += meta.IndexSize
		}
		return nil
	})

	// Count error jobs from jobs bucket
	s.db.View(func(tx *nutsdb.Tx) error {
		_, values, err := tx.GetAll(bucketJobs)
		if err != nil {
			return err
		}

		for _, val := range values {
			var job Job
			if err := json.Unmarshal(val, &job); err != nil {
				continue
			}
			if job.Status == pb.IndexStatus_INDEX_STATUS_ERROR {
				errorCount++
			}
		}
		return nil
	})

	var activeJobs int32
	s.activeJobs.Range(func(key, value interface{}) bool {
		activeJobs++
		return true
	})

	var avgDuration int64
	if stats.SearchesTotal > 0 {
		avgDuration = stats.SearchDurationTotal / stats.SearchesTotal
	}

	return &pb.StatsResponse{
		TotalIndexes:        totalIndexes,
		TotalFilesIndexed:   totalFiles,
		TotalIndexSizeBytes: totalSize,
		QueueLength:         int32(len(s.jobQueue)),
		ActiveJobs:          activeJobs,
		SearchesTotal:       stats.SearchesTotal,
		AvgSearchDurationMs: avgDuration,
		Uptime:              time.Since(stats.StartedAt).String(),
		JobsQueued:          stats.JobsQueued,
		JobsCompleted:       stats.JobsCompleted,
		JobsFailed:          stats.JobsFailed + errorCount,
		JobsRejected:        stats.JobsRejected,
	}, nil
}
