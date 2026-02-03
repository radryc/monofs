// Package git provides Git repository operations for MonoFS.
// This file implements a resilient blob cache with LRU eviction and auto-restore.
package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// BlobCache provides resilient blob reading with LRU-based cleanup.
// It tracks file access times and removes unused blobs to save space.
// When a cleaned blob is requested, it automatically restores from Git.
type BlobCache struct {
	mu sync.RWMutex

	// cacheDir is the directory where blobs are cached
	cacheDir string

	// repoMgr is used to restore blobs when needed
	repoMgr *RepoManager

	// accessTracker maps blobHash -> last access time
	accessTracker map[string]time.Time

	// blobMetadata maps blobHash -> metadata for restoration
	blobMetadata map[string]*blobMeta

	// maxAge is the duration after which unused blobs are eligible for cleanup
	maxAge time.Duration

	// maxRetries is the number of attempts to read/restore a blob
	maxRetries int

	// retryDelay is the base delay between retries
	retryDelay time.Duration

	// cleanupInterval is how often the cleanup goroutine runs
	cleanupInterval time.Duration

	// stopCleanup signals the cleanup goroutine to stop
	stopCleanup chan struct{}

	// cleanupWg waits for the cleanup goroutine to finish
	cleanupWg sync.WaitGroup
}

// blobMeta stores metadata needed to restore a blob from Git.
type blobMeta struct {
	RepoURL     string
	DisplayPath string
	Branch      string
	BlobHash    string
	LastAccess  time.Time
}

// BlobCacheConfig holds configuration for BlobCache.
type BlobCacheConfig struct {
	// CacheDir is the directory for cached blobs (default: os.TempDir()/monofs-blobs)
	CacheDir string

	// MaxAge is the maximum age for unused blobs (default: 1 hour)
	MaxAge time.Duration

	// MaxRetries is the number of read/restore attempts (default: 3)
	MaxRetries int

	// RetryDelay is the base delay between retries (default: 100ms)
	RetryDelay time.Duration

	// CleanupInterval is how often cleanup runs (default: 5 minutes)
	CleanupInterval time.Duration
}

// DefaultBlobCacheConfig returns the default configuration.
func DefaultBlobCacheConfig() BlobCacheConfig {
	return BlobCacheConfig{
		CacheDir:        filepath.Join(os.TempDir(), "monofs-blobs"),
		MaxAge:          1 * time.Hour,
		MaxRetries:      3,
		RetryDelay:      100 * time.Millisecond,
		CleanupInterval: 5 * time.Minute,
	}
}

// NewBlobCache creates a new BlobCache with the given configuration.
func NewBlobCache(repoMgr *RepoManager, cfg BlobCacheConfig) (*BlobCache, error) {
	if cfg.CacheDir == "" {
		cfg.CacheDir = filepath.Join(os.TempDir(), "monofs-blobs")
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 1 * time.Hour
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 100 * time.Millisecond
	}
	if cfg.CleanupInterval == 0 {
		cfg.CleanupInterval = 5 * time.Minute
	}

	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create blob cache dir: %w", err)
	}

	bc := &BlobCache{
		cacheDir:        cfg.CacheDir,
		repoMgr:         repoMgr,
		accessTracker:   make(map[string]time.Time),
		blobMetadata:    make(map[string]*blobMeta),
		maxAge:          cfg.MaxAge,
		maxRetries:      cfg.MaxRetries,
		retryDelay:      cfg.RetryDelay,
		cleanupInterval: cfg.CleanupInterval,
		stopCleanup:     make(chan struct{}),
	}

	// Start background cleanup goroutine
	bc.cleanupWg.Add(1)
	go bc.cleanupLoop()

	return bc, nil
}

// Close stops the cleanup goroutine and releases resources.
func (bc *BlobCache) Close() error {
	close(bc.stopCleanup)
	bc.cleanupWg.Wait()
	return nil
}

// blobPath returns the filesystem path for a cached blob.
func (bc *BlobCache) blobPath(blobHash string) string {
	// Use first 2 chars as subdirectory to avoid too many files in one dir
	if len(blobHash) < 2 {
		return filepath.Join(bc.cacheDir, blobHash)
	}
	return filepath.Join(bc.cacheDir, blobHash[:2], blobHash)
}

// trackAccess updates the access time for a blob.
func (bc *BlobCache) trackAccess(blobHash string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.accessTracker[blobHash] = time.Now()
	if meta, ok := bc.blobMetadata[blobHash]; ok {
		meta.LastAccess = time.Now()
	}
}

// RegisterBlob registers a blob with metadata for future restoration.
// This should be called when blob metadata becomes available (e.g., during ingestion).
func (bc *BlobCache) RegisterBlob(blobHash, repoURL, displayPath, branch string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	bc.blobMetadata[blobHash] = &blobMeta{
		RepoURL:     repoURL,
		DisplayPath: displayPath,
		Branch:      branch,
		BlobHash:    blobHash,
		LastAccess:  time.Now(),
	}
	bc.accessTracker[blobHash] = time.Now()
}

// ReadBlob reads a blob with automatic retry and restoration.
// It first tries to read from the cache, then falls back to Git.
// If the blob was cleaned up, it automatically restores it.
func (bc *BlobCache) ReadBlob(ctx context.Context, repo *git.Repository, blobHash string) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt < bc.maxRetries; attempt++ {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Try to read from cache first
		data, err := bc.readFromCache(blobHash)
		if err == nil {
			bc.trackAccess(blobHash)
			return data, nil
		}

		// Try to read from Git repository
		data, err = bc.readFromGit(repo, blobHash)
		if err == nil {
			// Cache the blob for future reads
			if cacheErr := bc.writeToCache(blobHash, data); cacheErr != nil {
				// Log but don't fail - we have the data
				// In production, use proper logging
			}
			bc.trackAccess(blobHash)
			return data, nil
		}

		lastErr = err

		// Wait before retry (with exponential backoff)
		if attempt < bc.maxRetries-1 {
			delay := bc.retryDelay * time.Duration(1<<uint(attempt))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	return nil, fmt.Errorf("failed to read blob %s after %d attempts: %w", blobHash, bc.maxRetries, lastErr)
}

// ReadBlobWithRestore reads a blob and automatically restores it if needed.
// This is the primary method for resilient reads that handles missing repos.
func (bc *BlobCache) ReadBlobWithRestore(ctx context.Context, blobHash, repoURL, displayPath, branch string) ([]byte, error) {
	var lastErr error

	// Register metadata for future restoration
	bc.RegisterBlob(blobHash, repoURL, displayPath, branch)

	for attempt := 0; attempt < bc.maxRetries; attempt++ {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Try to read from cache first
		data, err := bc.readFromCache(blobHash)
		if err == nil {
			bc.trackAccess(blobHash)
			return data, nil
		}

		// Try to open/clone the repo and read the blob
		repo, err := bc.repoMgr.CloneOrOpen(ctx, repoURL, displayPath, branch)
		if err != nil {
			lastErr = fmt.Errorf("failed to open repo: %w", err)
			// Wait before retry
			if attempt < bc.maxRetries-1 {
				delay := bc.retryDelay * time.Duration(1<<uint(attempt))
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
			}
			continue
		}

		// Read blob from Git
		data, err = bc.readFromGit(repo, blobHash)
		if err != nil {
			lastErr = fmt.Errorf("failed to read blob from git: %w", err)
			// Wait before retry
			if attempt < bc.maxRetries-1 {
				delay := bc.retryDelay * time.Duration(1<<uint(attempt))
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
			}
			continue
		}

		// Cache the blob for future reads
		if cacheErr := bc.writeToCache(blobHash, data); cacheErr != nil {
			// Log but don't fail - we have the data
		}
		bc.trackAccess(blobHash)
		return data, nil
	}

	return nil, fmt.Errorf("failed to read/restore blob %s after %d attempts: %w", blobHash, bc.maxRetries, lastErr)
}

// readFromCache reads a blob from the filesystem cache.
func (bc *BlobCache) readFromCache(blobHash string) ([]byte, error) {
	path := bc.blobPath(blobHash)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("blob not in cache: %s", blobHash)
		}
		return nil, fmt.Errorf("failed to read cached blob: %w", err)
	}
	return data, nil
}

// writeToCache writes a blob to the filesystem cache.
func (bc *BlobCache) writeToCache(blobHash string, data []byte) error {
	path := bc.blobPath(blobHash)

	// Create parent directory
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create cache dir: %w", err)
	}

	// Write atomically using temp file + rename
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up temp file
		return fmt.Errorf("failed to rename cache file: %w", err)
	}

	return nil
}

// readFromGit reads a blob directly from the Git repository.
func (bc *BlobCache) readFromGit(repo *git.Repository, blobHash string) ([]byte, error) {
	hash := plumbing.NewHash(blobHash)
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get blob object: %w", err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to get blob reader: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob data: %w", err)
	}

	return data, nil
}

// cleanupLoop periodically removes blobs that haven't been accessed recently.
func (bc *BlobCache) cleanupLoop() {
	defer bc.cleanupWg.Done()

	ticker := time.NewTicker(bc.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-bc.stopCleanup:
			return
		case <-ticker.C:
			bc.cleanupExpiredBlobs()
		}
	}
}

// cleanupExpiredBlobs removes blobs that haven't been accessed within maxAge.
func (bc *BlobCache) cleanupExpiredBlobs() {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-bc.maxAge)

	// Find blobs to remove
	var toRemove []string
	for blobHash, lastAccess := range bc.accessTracker {
		if lastAccess.Before(cutoff) {
			toRemove = append(toRemove, blobHash)
		}
	}

	// Remove expired blobs
	for _, blobHash := range toRemove {
		path := bc.blobPath(blobHash)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			// Log error but continue cleanup
			continue
		}

		// Don't delete metadata - we need it for restoration!
		delete(bc.accessTracker, blobHash)
	}
}

// CleanupStats returns statistics about the cleanup state.
type CleanupStats struct {
	TotalTracked    int
	CachedOnDisk    int
	ExpiredCount    int
	OldestAccess    time.Time
	NewestAccess    time.Time
	TotalCacheBytes int64
}

// GetStats returns current cache statistics.
func (bc *BlobCache) GetStats() CleanupStats {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	stats := CleanupStats{
		TotalTracked: len(bc.accessTracker),
	}

	now := time.Now()
	cutoff := now.Add(-bc.maxAge)

	for _, lastAccess := range bc.accessTracker {
		if lastAccess.Before(cutoff) {
			stats.ExpiredCount++
		}
		if stats.OldestAccess.IsZero() || lastAccess.Before(stats.OldestAccess) {
			stats.OldestAccess = lastAccess
		}
		if lastAccess.After(stats.NewestAccess) {
			stats.NewestAccess = lastAccess
		}
	}

	// Count cached files and their size
	filepath.Walk(bc.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			stats.CachedOnDisk++
			stats.TotalCacheBytes += info.Size()
		}
		return nil
	})

	return stats
}

// ForceCleanup immediately removes all expired blobs.
func (bc *BlobCache) ForceCleanup() int {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-bc.maxAge)

	removed := 0
	for blobHash, lastAccess := range bc.accessTracker {
		if lastAccess.Before(cutoff) {
			path := bc.blobPath(blobHash)
			if err := os.Remove(path); err == nil || os.IsNotExist(err) {
				delete(bc.accessTracker, blobHash)
				removed++
			}
		}
	}

	return removed
}

// accessEntry is used for sorting by access time.
type accessEntry struct {
	hash       string
	lastAccess time.Time
}

// CleanupByCount removes the oldest N blobs to free space.
// This is useful when disk space is low and you need to free space immediately.
func (bc *BlobCache) CleanupByCount(count int) int {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if count <= 0 || len(bc.accessTracker) == 0 {
		return 0
	}

	// Sort blobs by access time (oldest first)
	entries := make([]accessEntry, 0, len(bc.accessTracker))
	for hash, lastAccess := range bc.accessTracker {
		entries = append(entries, accessEntry{hash: hash, lastAccess: lastAccess})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].lastAccess.Before(entries[j].lastAccess)
	})

	// Remove the oldest N entries
	removed := 0
	for i := 0; i < count && i < len(entries); i++ {
		blobHash := entries[i].hash
		path := bc.blobPath(blobHash)
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			delete(bc.accessTracker, blobHash)
			removed++
		}
	}

	return removed
}
