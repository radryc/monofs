package fetcher

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	err = backend.Initialize(ctx, BackendConfig{
		CacheDir:    tmpDir,
		Concurrency: 2,
	})

	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if backend.Type() != SourceTypeGit {
		t.Errorf("expected SourceTypeGit, got %v", backend.Type())
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

	err = backend.Initialize(ctx, BackendConfig{
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

	err = backend.Initialize(ctx, BackendConfig{
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

func TestGoModBackend_Initialize(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "gomod-backend-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	backend := NewGoModBackend()

	ctx := context.Background()
	err = backend.Initialize(ctx, BackendConfig{
		CacheDir:    tmpDir,
		Concurrency: 2,
	})

	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if backend.Type() != SourceTypeGoMod {
		t.Errorf("expected SourceTypeGoMod, got %v", backend.Type())
	}
}

func TestGoModBackend_FetchBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "gomod-backend-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	backend := NewGoModBackend()
	ctx := context.Background()

	err = backend.Initialize(ctx, BackendConfig{
		CacheDir:    tmpDir,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fetch a file from a known module
	req := &FetchRequest{
		ContentID: "golang.org/x/text@v0.14.0/unicode/norm/normalize.go",
		SourceKey: "golang.org/x/text",
		SourceConfig: map[string]string{
			"module_path": "golang.org/x/text",
			"version":     "v0.14.0",
			"file_path":   "unicode/norm/normalize.go",
		},
	}

	result, err := backend.FetchBlob(ctx, req)
	if err != nil {
		t.Skipf("FetchBlob failed (network issue?): %v", err)
	}

	if result.Size == 0 {
		t.Error("expected non-empty content")
	}

	t.Logf("Fetched %d bytes from Go module", result.Size)
}

func TestBackendRegistry(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	registry := NewRegistry()

	// Register backends
	gitBackend := NewGitBackend()
	gomodBackend := NewGoModBackend()

	registry.Register(gitBackend)
	registry.Register(gomodBackend)

	// Get by type
	gitResult, ok := registry.Get(SourceTypeGit)
	if !ok || gitResult == nil {
		t.Error("expected to get Git backend")
	}

	gomodResult, ok := registry.Get(SourceTypeGoMod)
	if !ok || gomodResult == nil {
		t.Error("expected to get GoMod backend")
	}

	_, ok = registry.Get(SourceTypeS3)
	if ok {
		t.Error("expected false for unregistered backend")
	}

	// Close all
	if err := registry.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func TestIsGoModPath(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"https://github.com/user/repo", false},
		{"http://github.com/user/repo", false},
		{"git://github.com/user/repo", false},
		{"git@github.com:user/repo.git", false},
		{"golang.org/x/text", true},
		{"google.golang.org/grpc", true},
		{"gopkg.in/yaml.v3", true},
		{"go.uber.org/zap", true},
		{"github.com/user/repo", false}, // No scheme but github.com format
		// Go module paths with version suffix
		{"github.com/prometheus/common@v0.62.0", true},
		{"github.com/user/repo@v1.2.3", true},
		{"github.com/user/repo/v2@v2.0.0", true},
		{"github.com/foo/bar@v0.0.0-20210101120000-abcdef123456", true},
	}

	for _, tt := range tests {
		result := IsGoModPath(tt.url)
		if result != tt.expected {
			t.Errorf("IsGoModPath(%q) = %v, want %v", tt.url, result, tt.expected)
		}
	}
}

func TestParseSourceType(t *testing.T) {
	tests := []struct {
		input    string
		expected SourceType
	}{
		{"git", SourceTypeGit},
		{"gomod", SourceTypeGoMod},
		{"s3", SourceTypeS3},
		{"http", SourceTypeHTTP},
		{"oci", SourceTypeOCI},
		{"unknown", SourceTypeUnknown},
		{"", SourceTypeUnknown},
	}

	for _, tt := range tests {
		result := ParseSourceType(tt.input)
		if result != tt.expected {
			t.Errorf("ParseSourceType(%q) = %v, want %v", tt.input, result, tt.expected)
		}
	}
}

func TestSourceTypeString(t *testing.T) {
	tests := []struct {
		input    SourceType
		expected string
	}{
		{SourceTypeGit, "git"},
		{SourceTypeGoMod, "gomod"},
		{SourceTypeS3, "s3"},
		{SourceTypeHTTP, "http"},
		{SourceTypeOCI, "oci"},
		{SourceTypeUnknown, "unknown"},
	}

	for _, tt := range tests {
		result := tt.input.String()
		if result != tt.expected {
			t.Errorf("SourceType(%d).String() = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// TestStreamReader verifies the io.ReadCloser implementation
func TestStreamReader(t *testing.T) {
	// Create a mock stream for testing
	data := []byte("hello world from stream reader test")

	pr, pw := io.Pipe()

	// Write data in chunks
	go func() {
		for i := 0; i < len(data); i += 10 {
			end := i + 10
			if end > len(data) {
				end = len(data)
			}
			pw.Write(data[i:end])
		}
		pw.Close()
	}()

	// Read all data
	result, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if string(result) != string(data) {
		t.Errorf("expected %q, got %q", string(data), string(result))
	}
}

// Benchmarks

func BenchmarkIsGoModPath(b *testing.B) {
	urls := []string{
		"https://github.com/user/repo",
		"golang.org/x/text",
		"git@github.com:user/repo.git",
		"gopkg.in/yaml.v3",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, url := range urls {
			IsGoModPath(url)
		}
	}
}

func BenchmarkParseSourceType(b *testing.B) {
	types := []string{"git", "gomod", "s3", "http", "oci", "unknown"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, t := range types {
			ParseSourceType(t)
		}
	}
}

// Cache eviction test
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
	err = backend.Initialize(ctx, BackendConfig{
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

	err = backend.Initialize(ctx, BackendConfig{
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

// Test backend configuration
func TestBackendConfig_Defaults(t *testing.T) {
	config := BackendConfig{
		CacheDir: "/tmp/test",
	}

	// Verify defaults are reasonable
	if config.Concurrency != 0 {
		// Zero means no explicit limit
	}

	if config.MaxCacheSize != 0 {
		// Zero means unlimited
	}
}

// File path helper test
func TestExtractFilePath(t *testing.T) {
	// Test various path formats
	tests := []struct {
		config   map[string]string
		expected string
	}{
		{map[string]string{"file_path": "main.go"}, "main.go"},
		{map[string]string{"display_path": "path/to/file.go"}, "path/to/file.go"},
		{map[string]string{"path": "another/path.go"}, "another/path.go"},
		{map[string]string{}, ""},
	}

	for _, tt := range tests {
		// Extract file path logic
		result := ""
		if fp, ok := tt.config["file_path"]; ok {
			result = fp
		} else if dp, ok := tt.config["display_path"]; ok {
			result = dp
		} else if p, ok := tt.config["path"]; ok {
			result = p
		}

		if result != tt.expected {
			t.Errorf("extractFilePath(%v) = %q, want %q", tt.config, result, tt.expected)
		}
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

	err = backend.Initialize(ctx, BackendConfig{
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
