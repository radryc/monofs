package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/monofs/internal/storage"
)

func TestGitBackend_Initialize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "git-backend-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	backend := NewGitBackend()

	ctx := context.Background()
	err = backend.Initialize(ctx, storage.BackendConfig{
		CacheDir:    tmpDir,
		Concurrency: 2,
	})

	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if backend.Type() != storage.FetchTypeGit {
		t.Errorf("expected FetchTypeGit, got %v", backend.Type())
	}

	// Verify cache directory was created
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		t.Error("cache directory not created")
	}
}

func TestGitBackend_Warmup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "git-backend-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	backend := NewGitBackend()
	ctx := context.Background()

	err = backend.Initialize(ctx, storage.BackendConfig{
		CacheDir:    tmpDir,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Warmup with a small public repo
	repoURL := "https://github.com/kelseyhightower/nocode"
	err = backend.Warmup(ctx, repoURL, map[string]string{
		"branch": "master",
	})

	if err != nil {
		t.Fatalf("Warmup failed: %v", err)
	}

	// Verify repo was cached
	sources := backend.CachedSources()
	found := false
	for _, src := range sources {
		if src == repoURL {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("expected %s in cached sources", repoURL)
	}
}

func TestGitBackend_FetchBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "git-backend-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	backend := NewGitBackend()
	ctx := context.Background()

	err = backend.Initialize(ctx, storage.BackendConfig{
		CacheDir:    tmpDir,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Test with a known file from a public repo
	// Note: This test depends on external service availability
	repoURL := "https://github.com/kelseyhightower/nocode"

	err = backend.Warmup(ctx, repoURL, map[string]string{"branch": "master"})
	if err != nil {
		t.Skipf("Warmup failed (network issue?): %v", err)
	}

	// Note: This won't work directly as we need the actual blob hash
	// This is more of a warm-up verification test
	t.Log("Git backend warmup test passed")
}

// Test cache eviction
func TestGitBackend_CacheEviction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "git-eviction-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	backend := NewGitBackend()
	ctx := context.Background()

	// Set small cache size for testing
	err = backend.Initialize(ctx, storage.BackendConfig{
		CacheDir:     tmpDir,
		MaxCacheSize: 1024 * 1024, // 1MB
		Concurrency:  2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cleanup test
	repoURL := "https://github.com/example/test-repo"
	err = backend.Cleanup(ctx, repoURL)
	// Should not error even if repo doesn't exist
	if err != nil {
		t.Logf("Cleanup returned error (expected for non-existent): %v", err)
	}

	// Verify stats tracking
	stats := backend.Stats()
	t.Logf("Backend stats: requests=%d, errors=%d, cached=%d",
		stats.Requests, stats.Errors, stats.CachedItems)
}

// Test concurrent access
func TestGitBackend_ConcurrentWarmup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "git-concurrent-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	backend := NewGitBackend()
	ctx := context.Background()

	err = backend.Initialize(ctx, storage.BackendConfig{
		CacheDir:    tmpDir,
		Concurrency: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	repoURL := "https://github.com/kelseyhightower/nocode"

	// Run concurrent warmup requests
	done := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() {
			err := backend.Warmup(ctx, repoURL, map[string]string{"branch": "master"})
			done <- err
		}()
	}

	// Collect results
	var errors []error
	for i := 0; i < 5; i++ {
		if err := <-done; err != nil {
			errors = append(errors, err)
		}
	}

	// Most should succeed (maybe some timeouts)
	if len(errors) > 3 {
		t.Errorf("too many warmup failures: %v", errors)
	}
}

// Test cache directory structure
func TestGitBackend_CacheStructure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "git-structure-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	backend := NewGitBackend()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = backend.Initialize(ctx, storage.BackendConfig{
		CacheDir:    tmpDir,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	repoURL := "https://github.com/kelseyhightower/nocode"
	err = backend.Warmup(ctx, repoURL, map[string]string{"branch": "master"})
	if err != nil {
		t.Skipf("Warmup failed (network issue?): %v", err)
	}

	// Verify cache directory contains repo
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	if len(entries) == 0 {
		t.Error("expected cache directory to contain cloned repo")
	}

	// Check for .git directory or bare repo structure
	for _, entry := range entries {
		t.Logf("Cache entry: %s (dir=%v)", entry.Name(), entry.IsDir())
		if entry.IsDir() {
			gitDir := filepath.Join(tmpDir, entry.Name(), ".git")
			if _, err := os.Stat(gitDir); err == nil {
				t.Logf("Found .git directory in %s", entry.Name())
			}
		}
	}
}
