package blob

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type cloudArchiveUploadJob struct {
	archivePath string
}

func (bb *BlobBackend) startCloudUploadWorkers() {
	if !bb.hasCloudUpload() || bb.cloudUploadQueue != nil {
		return
	}

	workers := max(bb.config.Concurrency, 1)
	queueSize := max(defaultCloudUploadQueueSize, workers*4)
	bb.cloudUploadQueue = make(chan cloudArchiveUploadJob, queueSize)

	for i := 0; i < workers; i++ {
		bb.cloudUploadWG.Add(1)
		go bb.cloudUploadWorker(i + 1)
	}
}

// initUploadWAL loads the durable upload queue from disk.
func (bb *BlobBackend) initUploadWAL(archiveDir string) error {
	if !bb.hasCloudUpload() {
		return nil
	}
	bb.walMu.Lock()
	defer bb.walMu.Unlock()

	bb.walPath = filepath.Join(archiveDir, "upload.wal.json")
	data, err := os.ReadFile(bb.walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var pending []string
	if err := json.Unmarshal(data, &pending); err != nil {
		return fmt.Errorf("decode %s: %w", bb.walPath, err)
	}
	for _, path := range pending {
		if strings.TrimSpace(path) != "" {
			bb.walPending[path] = struct{}{}
		}
	}
	if bb.otel.walPending != nil {
		bb.otel.walPending.Record(context.Background(), int64(len(bb.walPending)))
	}
	return nil
}

// persistUploadWALLocked writes the WAL atomically. Caller must hold walMu.
func (bb *BlobBackend) persistUploadWALLocked() error {
	if bb.walPath == "" {
		return nil
	}
	paths := make([]string, 0, len(bb.walPending))
	for path := range bb.walPending {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := bb.walPath + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, bb.walPath); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(bb.walPath)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (bb *BlobBackend) walAdd(archivePath string) error {
	bb.walMu.Lock()
	defer bb.walMu.Unlock()
	if _, exists := bb.walPending[archivePath]; exists {
		return nil
	}
	bb.walPending[archivePath] = struct{}{}
	if err := bb.persistUploadWALLocked(); err != nil {
		delete(bb.walPending, archivePath)
		return err
	}
	bb.walRecordedTotal.Add(1)
	if bb.otel.walRecords != nil {
		bb.otel.walRecords.Add(context.Background(), 1)
	}
	if bb.otel.walPending != nil {
		bb.otel.walPending.Record(context.Background(), int64(len(bb.walPending)))
	}
	return nil
}

func (bb *BlobBackend) walRemove(archivePath string) error {
	bb.walMu.Lock()
	defer bb.walMu.Unlock()
	if _, exists := bb.walPending[archivePath]; !exists {
		return nil
	}
	delete(bb.walPending, archivePath)
	if err := bb.persistUploadWALLocked(); err != nil {
		bb.walPending[archivePath] = struct{}{}
		return err
	}
	if bb.otel.walPending != nil {
		bb.otel.walPending.Record(context.Background(), int64(len(bb.walPending)))
	}
	return nil
}

// replayUploadWAL re-queues archives that were pending when the process
// stopped. Archives that no longer exist are dropped from the WAL.
func (bb *BlobBackend) replayUploadWAL(ctx context.Context) {
	if !bb.hasCloudUpload() {
		return
	}
	bb.walMu.Lock()
	pending := make([]string, 0, len(bb.walPending))
	for path := range bb.walPending {
		pending = append(pending, path)
	}
	bb.walMu.Unlock()

	for _, path := range pending {
		if _, err := os.Stat(path); err != nil {
			_ = bb.walRemove(path)
			continue
		}
		bb.walReplayedTotal.Add(1)
		if bb.otel.walReplays != nil {
			bb.otel.walReplays.Add(context.Background(), 1)
		}
		if err := bb.queueCloudUpload(path); err != nil && bb.logger != nil {
			bb.logger.Warn("failed to replay cloud upload from WAL",
				"archive_path", path, "error", err)
		}
	}
	if len(pending) > 0 && bb.logger != nil {
		bb.logger.Info("replayed cloud upload WAL", "entries", len(pending))
	}
}

// backfillCloudArchiveIndexes uploads sidecars for local archives whose .pack
// already exists in S3. This bootstraps read-through for archives created
// before sidecar indexes were introduced.
func (bb *BlobBackend) backfillCloudArchiveIndexes(ctx context.Context) {
	if !bb.hasCloudUpload() {
		return
	}
	bb.mu.RLock()
	archives := make([]string, 0, len(bb.archivePaths))
	for path := range bb.archivePaths {
		archives = append(archives, path)
	}
	bb.mu.RUnlock()

	for _, path := range archives {
		if _, err := os.Stat(path + archiveIndexSuffix); err != nil {
			hashes, hashErr := bb.archiveHashes(path)
			if hashErr != nil {
				if bb.logger != nil {
					bb.logger.Warn("failed to build archive index", "archive_path", path, "error", hashErr)
				}
				continue
			}
			if err := bb.writeArchiveIndex(path, hashes); err != nil {
				if bb.logger != nil {
					bb.logger.Warn("failed to write archive index", "archive_path", path, "error", err)
				}
				continue
			}
		}
		if !bb.cloudArchiveExists(path) || bb.cloudArchiveIndexExists(path) {
			continue
		}
		if err := bb.uploadArchiveIndex(ctx, path); err != nil && bb.logger != nil {
			bb.logger.Warn("failed to backfill cloud archive index", "archive_path", path, "error", err)
		}
	}
}

func (bb *BlobBackend) cloudUploadWorker(workerID int) {
	defer bb.cloudUploadWG.Done()

	for job := range bb.cloudUploadQueue {
		start := time.Now()
		var uploadErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * time.Second)
			}
			if uploadErr = bb.uploadArchiveFunc(context.Background(), job.archivePath); uploadErr == nil {
				break
			}
		}
		duration := time.Since(start).Seconds()
		if uploadErr != nil {
			bb.storageHealthy.Store(false)
			bb.lastStorageErr.Store(uploadErr.Error())
			bb.walUploadFailuresTotal.Add(1)
			if bb.otel.walFailures != nil {
				bb.otel.walFailures.Add(context.Background(), 1)
			}
			if bb.otel.uploads != nil {
				bb.otel.uploads.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "failure")))
			}
			if bb.otel.uploadDuration != nil {
				bb.otel.uploadDuration.Record(context.Background(), duration, metric.WithAttributes(attribute.String("result", "failure")))
			}
			if bb.logger != nil {
				bb.logger.Error("failed to upload archive to cloud",
					"archive_path", job.archivePath,
					"worker", workerID,
					"error", uploadErr)
			}
			continue
		}
		bb.cloudStores.Add(1)
		bb.storageHealthy.Store(true)
		bb.lastStorageErr.Store("")
		if bb.otel.uploads != nil {
			bb.otel.uploads.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "success")))
		}
		if bb.otel.uploadDuration != nil {
			bb.otel.uploadDuration.Record(context.Background(), duration, metric.WithAttributes(attribute.String("result", "success")))
		}
		if err := bb.walRemove(job.archivePath); err != nil && bb.logger != nil {
			bb.logger.Warn("failed to remove uploaded archive from WAL",
				"archive_path", job.archivePath, "error", err)
		}
	}
}

func (bb *BlobBackend) queueCloudUpload(archivePath string) error {
	if !bb.hasCloudUpload() {
		return nil
	}
	if err := bb.walAdd(archivePath); err != nil {
		return fmt.Errorf("record archive in upload WAL: %w", err)
	}

	if bb.cloudUploadQueue == nil {
		start := time.Now()
		if err := bb.uploadArchiveFunc(context.Background(), archivePath); err != nil {
			bb.storageHealthy.Store(false)
			bb.lastStorageErr.Store(err.Error())
			bb.walUploadFailuresTotal.Add(1)
			if bb.otel.walFailures != nil {
				bb.otel.walFailures.Add(context.Background(), 1)
			}
			if bb.otel.uploads != nil {
				bb.otel.uploads.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "failure")))
			}
			if bb.otel.uploadDuration != nil {
				bb.otel.uploadDuration.Record(context.Background(), time.Since(start).Seconds(), metric.WithAttributes(attribute.String("result", "failure")))
			}
			if bb.logger != nil {
				bb.logger.Error("failed to upload archive to cloud",
					"archive_path", archivePath,
					"error", err)
			}
			return err
		}
		bb.cloudStores.Add(1)
		bb.storageHealthy.Store(true)
		bb.lastStorageErr.Store("")
		if bb.otel.uploads != nil {
			bb.otel.uploads.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", "success")))
		}
		if bb.otel.uploadDuration != nil {
			bb.otel.uploadDuration.Record(context.Background(), time.Since(start).Seconds(), metric.WithAttributes(attribute.String("result", "success")))
		}
		if err := bb.walRemove(archivePath); err != nil && bb.logger != nil {
			bb.logger.Warn("failed to remove uploaded archive from WAL",
				"archive_path", archivePath, "error", err)
		}
		return nil
	}

	job := cloudArchiveUploadJob{archivePath: archivePath}
	select {
	case bb.cloudUploadQueue <- job:
	default:
		if bb.logger != nil {
			bb.logger.Warn("cloud upload queue full; waiting for worker",
				"archive_path", archivePath,
				"queue_length", len(bb.cloudUploadQueue))
		}
		bb.cloudUploadQueue <- job
	}
	return nil
}

// startCloudBackupScanner starts a background goroutine that periodically
// verifies every local archive has a corresponding cloud object, and re-uploads
// any that are missing. This closes the gap left by best-effort async uploads.
func (bb *BlobBackend) startCloudBackupScanner() {
	if !bb.isCloudConfigured() || bb.scannerStop != nil {
		return
	}

	bb.scannerStop = make(chan struct{})
	bb.scannerWG.Add(1)
	go bb.cloudBackupScannerLoop()
}

func (bb *BlobBackend) cloudBackupScannerLoop() {
	defer bb.scannerWG.Done()

	interval := 1 * time.Hour
	if bb.config.Cloud.CloudScannerIntervalSecs > 0 {
		interval = time.Duration(bb.config.Cloud.CloudScannerIntervalSecs) * time.Second
	}

	time.Sleep(30 * time.Second)
	bb.runCloudBackupScan()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-bb.scannerStop:
			return
		case <-ticker.C:
			bb.runCloudBackupScan()
		}
	}
}

func (bb *BlobBackend) runCloudBackupScan() {
	start := time.Now()

	bb.mu.RLock()
	archives := make([]string, 0, len(bb.archivePaths))
	for path := range bb.archivePaths {
		archives = append(archives, path)
	}
	bb.mu.RUnlock()

	var missing int64
	var repaired int64
	for _, archivePath := range archives {
		if bb.cloudArchiveExists(archivePath) {
			_ = bb.walRemove(archivePath)
			continue
		}
		missing++
		packagerCloudBackupMissingTotal.Inc()
		if bb.logger != nil {
			bb.logger.Warn("cloud backup missing for local archive; repairing",
				"archive_path", archivePath)
		}
		uploadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := bb.ensureArchiveInCloud(uploadCtx, archivePath)
		cancel()
		if err != nil {
			if bb.logger != nil {
				bb.logger.Error("failed to repair cloud backup",
					"archive_path", archivePath,
					"error", err)
			}
			continue
		}
		repaired++
		_ = bb.walRemove(archivePath)
		packagerCloudBackupRepairedTotal.Inc()
		if bb.logger != nil {
			bb.logger.Info("repaired cloud backup for local archive",
				"archive_path", archivePath)
		}
	}

	packagerCloudBackupScanDurationSeconds.Observe(time.Since(start).Seconds())
	if bb.logger != nil && (missing > 0 || repaired > 0) {
		bb.logger.Info("cloud backup scan complete",
			"archives_checked", len(archives),
			"missing", missing,
			"repaired", repaired,
			"duration", time.Since(start))
	}
}
