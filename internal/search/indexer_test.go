package search

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestIndexer(t *testing.T) (*Indexer, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "monofs-search-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	indexDir := filepath.Join(tmpDir, "indexes")
	cacheDir := filepath.Join(tmpDir, "cache")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	indexer, err := NewIndexer(indexDir, cacheDir, nil, logger) // nil client = fallback to external fetch
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create indexer: %v", err)
	}

	return indexer, tmpDir
}

func createTestRepo(t *testing.T, baseDir string, files map[string]string) string {
	t.Helper()
	repoDir := filepath.Join(baseDir, "test-repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("failed to create repo dir: %v", err)
	}

	for path, content := range files {
		fullPath := filepath.Join(repoDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("failed to create parent dir for %s: %v", path, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write test file %s: %v", path, err)
		}
	}

	return repoDir
}

func TestNewIndexer(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	if indexer == nil {
		t.Fatal("NewIndexer returned nil")
	}

	// Verify directories were created
	indexDir := filepath.Join(tmpDir, "indexes")
	cacheDir := filepath.Join(tmpDir, "cache")

	if _, err := os.Stat(indexDir); os.IsNotExist(err) {
		t.Error("index directory was not created")
	}
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		t.Error("cache directory was not created")
	}
}

func TestNewIndexer_InvalidDir(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Try to create indexer with invalid path
	_, err := NewIndexer("/nonexistent/read-only/path", "/tmp/cache", nil, logger)
	if err == nil {
		t.Error("expected error for invalid index directory")
	}
}

func TestIndexer_Close(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)

	// Should not panic
	err := indexer.Close()
	if err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

func TestIndexer_IndexLocalDir(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	// Create test files
	testFiles := map[string]string{
		"main.go": `package main

import "fmt"

func main() {
	fmt.Println("Hello, World!")
}
`,
		"lib/helper.go": `package lib

func Add(a, b int) int {
	return a + b
}

func Multiply(a, b int) int {
	return a * b
}
`,
		"README.md": `# Test Project

This is a test project for MonoFS search engine.

## Features

- Code search
- Indexing
`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)

	// Index the local directory directly
	result, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo-1",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})

	if err != nil {
		t.Fatalf("IndexLocalDir failed: %v", err)
	}

	if result.FilesIndexed != 3 {
		t.Errorf("expected 3 files indexed, got %d", result.FilesIndexed)
	}

	if result.IndexSizeBytes <= 0 {
		t.Error("expected positive index size")
	}

	t.Logf("Indexed %d files, size: %d bytes, duration: %v",
		result.FilesIndexed, result.IndexSizeBytes, result.Duration)
}

func TestIndexer_SearchBasic(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	// Create and index test files
	testFiles := map[string]string{
		"main.go": `package main

import "fmt"

func main() {
	fmt.Println("Hello, World!")
}
`,
		"lib/math.go": `package lib

// Add adds two integers
func Add(a, b int) int {
	return a + b
}

// Subtract subtracts two integers  
func Subtract(a, b int) int {
	return a - b
}
`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	// Reload searcher to pick up new indexes
	if err := indexer.ReloadSearcher(); err != nil {
		t.Fatalf("failed to reload searcher: %v", err)
	}

	// Search for "func Add"
	results, err := indexer.Search(context.Background(), SearchRequest{
		Query:      "func Add",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if results.TotalMatches == 0 {
		t.Error("expected at least one match for 'func Add'")
	}

	t.Logf("Found %d matches in %v", results.TotalMatches, results.Duration)
	for _, r := range results.Results {
		t.Logf("  %s:%d - %s", r.FilePath, r.LineNumber, r.LineContent)
	}
}

func TestIndexer_SearchCaseSensitive(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	testFiles := map[string]string{
		"test.go": `package test

const Value = "Hello"
const value = "hello"
const VALUE = "HELLO"
`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	if err := indexer.ReloadSearcher(); err != nil {
		t.Fatalf("failed to reload searcher: %v", err)
	}

	// Case insensitive search should find all (Value, value, VALUE)
	results, err := indexer.Search(context.Background(), SearchRequest{
		Query:         "value",
		CaseSensitive: false,
		MaxResults:    10,
	})
	if err != nil {
		t.Fatalf("case-insensitive search failed: %v", err)
	}

	t.Logf("Case-insensitive search for 'value' found %d matches", results.TotalMatches)
	for _, r := range results.Results {
		t.Logf("  Line %d: %s", r.LineNumber, r.LineContent)
	}

	if results.TotalMatches < 3 {
		t.Errorf("expected at least 3 matches for case-insensitive 'value', got %d", results.TotalMatches)
	}

	// Case sensitive search for lowercase "value" should find only lowercase
	results, err = indexer.Search(context.Background(), SearchRequest{
		Query:         "value",
		CaseSensitive: true,
		MaxResults:    10,
	})
	if err != nil {
		t.Fatalf("case-sensitive search failed: %v", err)
	}

	t.Logf("Case-sensitive search for 'value' found %d matches", results.TotalMatches)
	for _, r := range results.Results {
		t.Logf("  Line %d: %s", r.LineNumber, r.LineContent)
	}
}

func TestIndexer_SearchRegex(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	testFiles := map[string]string{
		"functions.go": `package main

func handleRequest() {}
func handleResponse() {}
func processData() {}
func validateInput() {}
`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	if err := indexer.ReloadSearcher(); err != nil {
		t.Fatalf("failed to reload searcher: %v", err)
	}

	// Regex search for all handle* functions
	results, err := indexer.Search(context.Background(), SearchRequest{
		Query:      "handle[A-Z][a-z]+",
		Regex:      true,
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("regex search failed: %v", err)
	}

	if results.TotalMatches < 2 {
		t.Errorf("expected at least 2 matches for 'handle*' pattern, got %d", results.TotalMatches)
	}

	t.Logf("Regex search found %d matches", results.TotalMatches)
	for _, r := range results.Results {
		t.Logf("  %s:%d - %s", r.FilePath, r.LineNumber, r.LineContent)
	}
}

func TestIndexer_SearchStructKeyword(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	testFiles := map[string]string{
		"models.go": `package models

type User struct {
	ID   int
	Name string
}

type Config struct {
	Host string
	Port int
}
`,
		"handler.py": `class User:
    def __init__(self):
        self.name = "test"
`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	if err := indexer.ReloadSearcher(); err != nil {
		t.Fatalf("failed to reload searcher: %v", err)
	}

	// Test 1: Search for "struct" - should find Go structs
	results, err := indexer.Search(context.Background(), SearchRequest{
		Query:      "struct",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("search for 'struct' failed: %v", err)
	}

	if results.TotalMatches == 0 {
		t.Error("expected matches for 'struct' keyword in Go files")
	}
	t.Logf("Search for 'struct' found %d matches", results.TotalMatches)
	for _, r := range results.Results {
		t.Logf("  %s:%d - %s", r.FilePath, r.LineNumber, r.LineContent)
	}

	// Test 2: Search with lang:go syntax
	resultsLang, err := indexer.Search(context.Background(), SearchRequest{
		Query:      "struct lang:go",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("search for 'struct lang:go' failed: %v", err)
	}

	t.Logf("Search for 'struct lang:go' found %d matches", resultsLang.TotalMatches)
	for _, r := range resultsLang.Results {
		t.Logf("  %s:%d - %s", r.FilePath, r.LineNumber, r.LineContent)
		// Verify only .go files are matched
		if !strings.HasSuffix(r.FilePath, ".go") {
			t.Errorf("lang:go should only match .go files, got: %s", r.FilePath)
		}
	}

	// Test 3: Search with file pattern for .go files
	resultsPattern, err := indexer.Search(context.Background(), SearchRequest{
		Query:        "struct",
		FilePatterns: []string{"*.go"},
		MaxResults:   10,
	})
	if err != nil {
		t.Fatalf("search for 'struct' with *.go pattern failed: %v", err)
	}

	if resultsPattern.TotalMatches == 0 {
		t.Error("expected matches for 'struct' with *.go file pattern")
	}
	t.Logf("Search for 'struct' with *.go pattern found %d matches", resultsPattern.TotalMatches)
}

func TestIndexer_SearchFilePatterns(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	testFiles := map[string]string{
		"main.go":      `package main; func main() { fmt.Println("hello") }`,
		"lib/lib.go":   `package lib; func Helper() { fmt.Println("hello") }`,
		"main_test.go": `package main; func TestMain() { fmt.Println("hello") }`,
		"README.md":    `Hello world documentation`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	if err := indexer.ReloadSearcher(); err != nil {
		t.Fatalf("failed to reload searcher: %v", err)
	}

	// Search with file pattern filter for .go files only
	results, err := indexer.Search(context.Background(), SearchRequest{
		Query:        "hello",
		FilePatterns: []string{".go"},
		MaxResults:   10,
	})
	if err != nil {
		t.Fatalf("search with file pattern failed: %v", err)
	}

	t.Logf("Search with .go filter found %d matches", results.TotalMatches)
	for _, r := range results.Results {
		t.Logf("  %s:%d", r.FilePath, r.LineNumber)
	}

	// Test with glob pattern *.go (user-friendly format)
	resultsGlob, err := indexer.Search(context.Background(), SearchRequest{
		Query:        "hello",
		FilePatterns: []string{"*.go"},
		MaxResults:   10,
	})
	if err != nil {
		t.Fatalf("search with glob file pattern failed: %v", err)
	}

	t.Logf("Search with *.go glob filter found %d matches", resultsGlob.TotalMatches)
	if resultsGlob.TotalMatches != results.TotalMatches {
		t.Errorf("glob pattern *.go should match same files as .go: got %d, want %d",
			resultsGlob.TotalMatches, results.TotalMatches)
	}

	// Verify README.md is excluded
	for _, r := range resultsGlob.Results {
		if strings.HasSuffix(r.FilePath, ".md") {
			t.Errorf("*.go pattern should not match .md files: %s", r.FilePath)
		}
	}
}

func TestIndexer_DeleteIndex(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	testFiles := map[string]string{
		"main.go": `package main; func main() {}`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	// Verify index exists (shard files in index dir)
	if !indexer.IndexExists("test/repo") {
		t.Fatal("index should exist after indexing")
	}

	// Delete index by display path
	if err := indexer.DeleteIndex("test/repo"); err != nil {
		t.Fatalf("DeleteIndex failed: %v", err)
	}

	// Verify index is deleted
	if indexer.IndexExists("test/repo") {
		t.Error("index should be deleted")
	}
}

func TestIndexer_GetIndexSize(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	testFiles := map[string]string{
		"main.go": `package main

import "fmt"

func main() {
	fmt.Println("Hello, World!")
	fmt.Println("Line 2")
	fmt.Println("Line 3")
}
`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	size, err := indexer.GetIndexSize("test/repo")
	if err != nil {
		t.Fatalf("GetIndexSize failed: %v", err)
	}

	if size <= 0 {
		t.Errorf("expected positive index size, got %d", size)
	}

	t.Logf("Index size: %d bytes", size)
}

func TestIndexer_SearchNoResults(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	testFiles := map[string]string{
		"main.go": `package main; func main() {}`,
	}

	repoDir := createTestRepo(t, tmpDir, testFiles)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	if err := indexer.ReloadSearcher(); err != nil {
		t.Fatalf("failed to reload searcher: %v", err)
	}

	// Search for something that doesn't exist
	results, err := indexer.Search(context.Background(), SearchRequest{
		Query:      "nonexistent_unique_string_xyz123",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if results.TotalMatches != 0 {
		t.Errorf("expected 0 matches, got %d", results.TotalMatches)
	}
}

func TestIndexer_SearchMultipleRepos(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	// Create and index first repo
	repo1Files := map[string]string{
		"main.go": `package main; func FindMe() {}`,
	}
	repo1Dir := createTestRepo(t, filepath.Join(tmpDir, "repo1"), repo1Files)
	_, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "repo-1",
		DisplayPath: "org/repo1",
		SourceDir:   repo1Dir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing repo1 failed: %v", err)
	}

	// Create and index second repo
	repo2Files := map[string]string{
		"lib.go": `package lib; func FindMe() {}`,
	}
	repo2Dir := createTestRepo(t, filepath.Join(tmpDir, "repo2"), repo2Files)
	_, err = indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "repo-2",
		DisplayPath: "org/repo2",
		SourceDir:   repo2Dir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing repo2 failed: %v", err)
	}

	if err := indexer.ReloadSearcher(); err != nil {
		t.Fatalf("failed to reload searcher: %v", err)
	}

	// Search across all repos
	results, err := indexer.Search(context.Background(), SearchRequest{
		Query:      "FindMe",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if results.TotalMatches < 2 {
		t.Errorf("expected at least 2 matches from both repos, got %d", results.TotalMatches)
	}

	t.Logf("Found %d matches across repos", results.TotalMatches)
	for _, r := range results.Results {
		t.Logf("  %s: %s:%d", r.DisplayPath, r.FilePath, r.LineNumber)
	}
}
