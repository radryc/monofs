package search

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIndexer_GoModuleIndexing tests indexing of Go modules
func TestIndexer_GoModuleIndexing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Go module test in short mode")
	}

	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	tests := []struct {
		name        string
		moduleRef   string
		wantFiles   []string // Files we expect to find
		searchTerm  string   // Term to search for to verify indexing
		expectError bool
	}{
		{
			name:       "small stable module",
			moduleRef:  "github.com/pkg/errors@v0.9.1",
			wantFiles:  []string{"errors.go", "stack.go"},
			searchTerm: "package errors",
		},
		{
			name:        "invalid module format",
			moduleRef:   "github.com/pkg/errors", // missing @version
			expectError: true,
		},
		{
			name:        "nonexistent module",
			moduleRef:   "github.com/nonexistent/fakemodulethatdoesnotexist@v1.0.0",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			req := IndexRequest{
				StorageID:   "test-gomod-" + strings.ReplaceAll(tt.name, " ", "-"),
				DisplayPath: tt.moduleRef,
				RepoURL:     tt.moduleRef,
				Ref:         "main", // Not used for Go modules but required
			}

			result, err := indexer.IndexRepository(ctx, req)

			if tt.expectError {
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result == nil {
				t.Fatal("result is nil")
			}

			// Verify some files were indexed
			if result.FilesIndexed == 0 {
				t.Error("no files were indexed")
			}

			t.Logf("Indexed %d files, index size: %d bytes, took %v",
				result.FilesIndexed, result.IndexSizeBytes, result.Duration)

			// Wait a moment for index to be loaded
			time.Sleep(100 * time.Millisecond)

			// Verify the index exists (IndexExists takes displayPath, not storageID)
			if !indexer.IndexExists(req.DisplayPath) {
				t.Error("index does not exist after indexing")
			}

			// Test search functionality
			if tt.searchTerm != "" {
				searchReq := SearchRequest{
					Query:      tt.searchTerm,
					StorageID:  req.StorageID,
					MaxResults: 10,
				}

				searchResults, err := indexer.Search(ctx, searchReq)
				if err != nil {
					t.Errorf("search failed: %v", err)
				} else if len(searchResults.Results) == 0 {
					t.Errorf("search for %q returned no results", tt.searchTerm)
				} else {
					t.Logf("Search found %d matches for %q", len(searchResults.Results), tt.searchTerm)
				}
			}

			// Cleanup (DeleteIndex takes displayPath, not storageID)
			if err := indexer.DeleteIndex(req.DisplayPath); err != nil {
				t.Errorf("failed to delete index: %v", err)
			}
		})
	}
}

// TestIndexer_GoModuleVsGitRepo tests that the indexer correctly handles both types
func TestIndexer_GoModuleVsGitRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping comparison test in short mode")
	}

	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Index the same package as both Go module and Git repo
	modulePath := "github.com/pkg/errors"
	moduleVersion := "v0.9.1"

	// 1. Index as Go module
	gomodReq := IndexRequest{
		StorageID:   "test-gomod-pkgerrors",
		DisplayPath: modulePath + "@" + moduleVersion,
		RepoURL:     modulePath + "@" + moduleVersion,
		Ref:         "main",
	}

	gomodResult, err := indexer.IndexRepository(ctx, gomodReq)
	if err != nil {
		t.Fatalf("Go module indexing failed: %v", err)
	}

	// 2. Index as Git repo
	gitReq := IndexRequest{
		StorageID:   "test-git-pkgerrors",
		DisplayPath: "github.com/pkg/errors",
		RepoURL:     "https://github.com/pkg/errors.git",
		Ref:         moduleVersion,
	}

	gitResult, err := indexer.IndexRepository(ctx, gitReq)
	if err != nil {
		t.Fatalf("Git repo indexing failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Compare results
	t.Logf("Go module: %d files, %d bytes", gomodResult.FilesIndexed, gomodResult.IndexSizeBytes)
	t.Logf("Git repo: %d files, %d bytes", gitResult.FilesIndexed, gitResult.IndexSizeBytes)

	// Both should have indexed files
	if gomodResult.FilesIndexed == 0 {
		t.Error("Go module indexed no files")
	}
	if gitResult.FilesIndexed == 0 {
		t.Error("Git repo indexed no files")
	}

	// Search in both indexes
	searchTerm := "package errors"
	for _, storageID := range []string{gomodReq.StorageID, gitReq.StorageID} {
		searchReq := SearchRequest{
			Query:      searchTerm,
			StorageID:  storageID,
			MaxResults: 10,
		}

		results, err := indexer.Search(ctx, searchReq)
		if err != nil {
			t.Errorf("search in %s failed: %v", storageID, err)
			continue
		}

		if len(results.Results) == 0 {
			t.Errorf("no results in %s for %q", storageID, searchTerm)
		} else {
			t.Logf("Found %d matches in %s", len(results.Results), storageID)
		}
	}

	// Cleanup
	indexer.DeleteIndex(gomodReq.DisplayPath)
	indexer.DeleteIndex(gitReq.DisplayPath)
}

// TestIndexer_GoModuleWithVersion tests proper version handling
func TestIndexer_GoModuleWithVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping version test in short mode")
	}

	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Test multiple versions of the same module
	versions := []string{"v0.8.0", "v0.8.1", "v0.9.1"}

	for _, version := range versions {
		t.Run("version_"+version, func(t *testing.T) {
			storageID := "test-gomod-errors-" + strings.ReplaceAll(version, ".", "-")
			req := IndexRequest{
				StorageID:   storageID,
				DisplayPath: "github.com/pkg/errors@" + version,
				RepoURL:     "github.com/pkg/errors@" + version,
				Ref:         "main",
			}

			result, err := indexer.IndexRepository(ctx, req)
			if err != nil {
				t.Fatalf("failed to index %s: %v", version, err)
			}

			if result.FilesIndexed == 0 {
				t.Errorf("no files indexed for %s", version)
			}

			t.Logf("Version %s: %d files indexed", version, result.FilesIndexed)

			// Cleanup
			indexer.DeleteIndex(req.DisplayPath)
		})
	}
}

// TestIndexer_GoModuleSearch tests search functionality on Go modules
func TestIndexer_GoModuleSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping search test in short mode")
	}

	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Index a well-known module
	req := IndexRequest{
		StorageID:   "test-search-gomod",
		DisplayPath: "github.com/pkg/errors@v0.9.1",
		RepoURL:     "github.com/pkg/errors@v0.9.1",
		Ref:         "main",
	}

	_, err := indexer.IndexRepository(ctx, req)
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}
	defer indexer.DeleteIndex(req.DisplayPath)

	time.Sleep(100 * time.Millisecond)

	searchTests := []struct {
		name          string
		query         string
		expectResults bool
		caseSensitive bool
		regex         bool
	}{
		{
			name:          "simple text search",
			query:         "New",
			expectResults: true,
		},
		{
			name:          "package declaration",
			query:         "package errors",
			expectResults: true,
		},
		{
			name:          "function name",
			query:         "func Wrap",
			expectResults: true,
		},
		{
			name:          "case sensitive match",
			query:         "ERROR",
			caseSensitive: true,
			expectResults: false, // Should not match "error"
		},
		{
			name:          "regex pattern",
			query:         "func (New|Wrap)",
			regex:         true,
			expectResults: true,
		},
		{
			name:          "nonexistent term",
			query:         "xyznonexistentterm123",
			expectResults: false,
		},
	}

	for _, tt := range searchTests {
		t.Run(tt.name, func(t *testing.T) {
			searchReq := SearchRequest{
				Query:         tt.query,
				StorageID:     req.StorageID,
				MaxResults:    50,
				CaseSensitive: tt.caseSensitive,
				Regex:         tt.regex,
			}

			results, err := indexer.Search(ctx, searchReq)
			if err != nil {
				t.Fatalf("search failed: %v", err)
			}

			hasResults := len(results.Results) > 0

			if tt.expectResults && !hasResults {
				t.Errorf("expected results for query %q but got none", tt.query)
			}

			if !tt.expectResults && hasResults {
				t.Errorf("expected no results for query %q but got %d", tt.query, len(results.Results))
			}

			if hasResults {
				t.Logf("Query %q: found %d matches in %d files",
					tt.query, results.TotalMatches, results.FilesSearched)

				// Verify result structure
				for i, result := range results.Results {
					if result.StorageID != req.StorageID {
						t.Errorf("result[%d]: wrong storage_id: got %s, want %s",
							i, result.StorageID, req.StorageID)
					}
					if result.FilePath == "" {
						t.Errorf("result[%d]: empty file path", i)
					}
					if result.LineNumber <= 0 {
						t.Errorf("result[%d]: invalid line number: %d", i, result.LineNumber)
					}
					if result.LineContent == "" {
						t.Errorf("result[%d]: empty line content", i)
					}
				}
			}
		})
	}
}

// TestIndexer_GoModuleConcurrent tests concurrent Go module indexing
func TestIndexer_GoModuleConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent test in short mode")
	}

	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Different small modules to index concurrently
	modules := []string{
		"github.com/pkg/errors@v0.9.1",
		"github.com/davecgh/go-spew@v1.1.1",
		"github.com/pmezard/go-difflib@v1.0.0",
	}

	errChan := make(chan error, len(modules))

	for i, module := range modules {
		go func(idx int, mod string) {
			req := IndexRequest{
				StorageID:   filepath.Base(mod) + "-concurrent-" + string(rune('a'+idx)),
				DisplayPath: mod,
				RepoURL:     mod,
				Ref:         "main",
			}

			_, err := indexer.IndexRepository(ctx, req)
			errChan <- err

			if err == nil {
				// Cleanup after success
				indexer.DeleteIndex(req.DisplayPath)
			}
		}(i, module)
	}

	// Collect results
	for i := 0; i < len(modules); i++ {
		if err := <-errChan; err != nil {
			t.Errorf("concurrent indexing failed: %v", err)
		}
	}
}
