package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBlobCache_BasicOperations(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "monofs-blobcache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a mock RepoManager
	repoMgr, err := NewRepoManager(filepath.Join(tmpDir, "repos"))
	if err != nil {
		t.Fatalf("failed to create repo manager: %v", err)
	}

	// Create BlobCache with short TTL for testing
	cfg := BlobCacheConfig{
		CacheDir:        filepath.Join(tmpDir, "blobs"),
		MaxAge:          100 * time.Millisecond, // Very short for testing
		MaxRetries:      2,
		RetryDelay:      10 * time.Millisecond,
		CleanupInterval: 50 * time.Millisecond,
	}

	bc, err := NewBlobCache(repoMgr, cfg)
	if err != nil {
		t.Fatalf("failed to create blob cache: %v", err)
	}
	defer bc.Close()

	// Test RegisterBlob
	testHash := "abc123def456"
	bc.RegisterBlob(testHash, "https://github.com/test/repo", "github.com/test/repo", "main")

	// Verify registration
	bc.mu.RLock()
	meta, exists := bc.blobMetadata[testHash]
	bc.mu.RUnlock()

	if !exists {
		t.Error("blob should be registered")
	}
	if meta.RepoURL != "https://github.com/test/repo" {
		t.Errorf("wrong repo URL: got %s", meta.RepoURL)
	}
	if meta.DisplayPath != "github.com/test/repo" {
		t.Errorf("wrong display path: got %s", meta.DisplayPath)
	}
}

func TestBlobCache_CacheReadWrite(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-blobcache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	repoMgr, err := NewRepoManager(filepath.Join(tmpDir, "repos"))
	if err != nil {
		t.Fatalf("failed to create repo manager: %v", err)
	}

	cfg := DefaultBlobCacheConfig()
	cfg.CacheDir = filepath.Join(tmpDir, "blobs")

	bc, err := NewBlobCache(repoMgr, cfg)
	if err != nil {
		t.Fatalf("failed to create blob cache: %v", err)
	}
	defer bc.Close()

	// Test writeToCache and readFromCache
	testHash := "1234567890abcdef"
	testData := []byte("Hello, this is test content for blob caching")

	// Write to cache
	if err := bc.writeToCache(testHash, testData); err != nil {
		t.Fatalf("writeToCache failed: %v", err)
	}

	// Read back from cache
	data, err := bc.readFromCache(testHash)
	if err != nil {
		t.Fatalf("readFromCache failed: %v", err)
	}

	if string(data) != string(testData) {
		t.Errorf("data mismatch: got %q, want %q", string(data), string(testData))
	}
}

func TestBlobCache_TrackAccess(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-blobcache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	repoMgr, err := NewRepoManager(filepath.Join(tmpDir, "repos"))
	if err != nil {
		t.Fatalf("failed to create repo manager: %v", err)
	}

	cfg := DefaultBlobCacheConfig()
	cfg.CacheDir = filepath.Join(tmpDir, "blobs")

	bc, err := NewBlobCache(repoMgr, cfg)
	if err != nil {
		t.Fatalf("failed to create blob cache: %v", err)
	}
	defer bc.Close()

	testHash := "trackedblob123"

	// Track access
	bc.trackAccess(testHash)

	// Verify access time was recorded
	bc.mu.RLock()
	accessTime, exists := bc.accessTracker[testHash]
	bc.mu.RUnlock()

	if !exists {
		t.Error("access should be tracked")
	}

	if time.Since(accessTime) > time.Second {
		t.Error("access time should be recent")
	}
}

func TestBlobCache_Cleanup(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-blobcache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	repoMgr, err := NewRepoManager(filepath.Join(tmpDir, "repos"))
	if err != nil {
		t.Fatalf("failed to create repo manager: %v", err)
	}

	// Create cache with very short TTL
	cfg := BlobCacheConfig{
		CacheDir:        filepath.Join(tmpDir, "blobs"),
		MaxAge:          50 * time.Millisecond,
		MaxRetries:      2,
		RetryDelay:      10 * time.Millisecond,
		CleanupInterval: 1 * time.Hour, // Disable automatic cleanup
	}

	bc, err := NewBlobCache(repoMgr, cfg)
	if err != nil {
		t.Fatalf("failed to create blob cache: %v", err)
	}
	defer bc.Close()

	// Add some blobs to cache
	for i := 0; i < 5; i++ {
		hash := "blob" + string(rune('0'+i))
		data := []byte("test data " + hash)
		if err := bc.writeToCache(hash, data); err != nil {
			t.Fatalf("writeToCache failed: %v", err)
		}
		bc.trackAccess(hash)
	}

	// Verify blobs exist
	stats := bc.GetStats()
	if stats.TotalTracked != 5 {
		t.Errorf("expected 5 tracked blobs, got %d", stats.TotalTracked)
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// Force cleanup
	removed := bc.ForceCleanup()
	if removed != 5 {
		t.Errorf("expected 5 removed blobs, got %d", removed)
	}

	// Verify blobs are gone from tracker
	stats = bc.GetStats()
	if stats.TotalTracked != 0 {
		t.Errorf("expected 0 tracked blobs after cleanup, got %d", stats.TotalTracked)
	}
}

func TestBlobCache_CleanupByCount(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-blobcache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	repoMgr, err := NewRepoManager(filepath.Join(tmpDir, "repos"))
	if err != nil {
		t.Fatalf("failed to create repo manager: %v", err)
	}

	cfg := DefaultBlobCacheConfig()
	cfg.CacheDir = filepath.Join(tmpDir, "blobs")

	bc, err := NewBlobCache(repoMgr, cfg)
	if err != nil {
		t.Fatalf("failed to create blob cache: %v", err)
	}
	defer bc.Close()

	// Add blobs with staggered access times
	for i := 0; i < 10; i++ {
		hash := "blob" + string(rune('a'+i))
		data := []byte("test data " + hash)
		if err := bc.writeToCache(hash, data); err != nil {
			t.Fatalf("writeToCache failed: %v", err)
		}
		bc.trackAccess(hash)
		time.Sleep(5 * time.Millisecond) // Stagger access times
	}

	// Remove oldest 3
	removed := bc.CleanupByCount(3)
	if removed != 3 {
		t.Errorf("expected 3 removed, got %d", removed)
	}

	stats := bc.GetStats()
	if stats.TotalTracked != 7 {
		t.Errorf("expected 7 remaining, got %d", stats.TotalTracked)
	}
}

func TestBlobCache_GetStats(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-blobcache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	repoMgr, err := NewRepoManager(filepath.Join(tmpDir, "repos"))
	if err != nil {
		t.Fatalf("failed to create repo manager: %v", err)
	}

	cfg := DefaultBlobCacheConfig()
	cfg.CacheDir = filepath.Join(tmpDir, "blobs")
	cfg.MaxAge = 1 * time.Hour

	bc, err := NewBlobCache(repoMgr, cfg)
	if err != nil {
		t.Fatalf("failed to create blob cache: %v", err)
	}
	defer bc.Close()

	// Add some blobs
	totalSize := int64(0)
	for i := 0; i < 3; i++ {
		hash := "statsblob" + string(rune('0'+i))
		data := []byte("test data for stats " + hash)
		if err := bc.writeToCache(hash, data); err != nil {
			t.Fatalf("writeToCache failed: %v", err)
		}
		bc.trackAccess(hash)
		totalSize += int64(len(data))
	}

	stats := bc.GetStats()

	if stats.TotalTracked != 3 {
		t.Errorf("expected 3 tracked, got %d", stats.TotalTracked)
	}

	if stats.CachedOnDisk != 3 {
		t.Errorf("expected 3 on disk, got %d", stats.CachedOnDisk)
	}

	if stats.TotalCacheBytes != totalSize {
		t.Errorf("expected %d bytes, got %d", totalSize, stats.TotalCacheBytes)
	}

	if stats.ExpiredCount != 0 {
		t.Errorf("expected 0 expired, got %d", stats.ExpiredCount)
	}
}

func TestBlobCache_ContextCancellation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-blobcache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	repoMgr, err := NewRepoManager(filepath.Join(tmpDir, "repos"))
	if err != nil {
		t.Fatalf("failed to create repo manager: %v", err)
	}

	cfg := DefaultBlobCacheConfig()
	cfg.CacheDir = filepath.Join(tmpDir, "blobs")
	cfg.MaxRetries = 5
	cfg.RetryDelay = 100 * time.Millisecond

	bc, err := NewBlobCache(repoMgr, cfg)
	if err != nil {
		t.Fatalf("failed to create blob cache: %v", err)
	}
	defer bc.Close()

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Try to read a non-existent blob with cancelled context
	_, err = bc.ReadBlobWithRestore(ctx, "nonexistent", "https://invalid", "path", "main")

	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
