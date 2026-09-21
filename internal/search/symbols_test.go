package search

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectSymbolSupport(t *testing.T) {
	s := detectSymbolSupport()
	// Consistency: EnableUniversalCtags implies a binary is set.
	if s.EnableUniversalCtags && s.Binary == "" {
		t.Fatal("EnableUniversalCtags true but no binary path")
	}
	if s.Binary == "" && s.EnableUniversalCtags {
		t.Fatal("empty binary cannot be universal-ctags")
	}
}

func TestIndexerSymbolsEnabledFlag(t *testing.T) {
	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	// SymbolsEnabled must agree with the detected support.
	if got, want := indexer.SymbolsEnabled(), indexer.symbolsEnabled; got != want {
		t.Fatalf("SymbolsEnabled = %v, field = %v", got, want)
	}
}

func TestIndexer_SymbolSearch(t *testing.T) {
	if !detectSymbolSupport().EnableUniversalCtags {
		t.Skip("universal-ctags not available; skipping symbol search test")
	}

	indexer, tmpDir := setupTestIndexer(t)
	defer os.RemoveAll(tmpDir)
	defer indexer.Close()

	repoDir := createTestRepo(t, filepath.Join(tmpDir), map[string]string{
		"main.go": `package main

import "fmt"

func main() {
	fmt.Println("hello")
}

func uniqueSymbolName(a, b int) int {
	return a + b
}
`,
	})

	if _, err := indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "sym-repo",
		DisplayPath: "sym/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	}); err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	if err := indexer.ReloadSearcher(); err != nil {
		t.Fatalf("reload searcher: %v", err)
	}

	results, err := indexer.Search(context.Background(), SearchRequest{
		Query:      "sym:uniqueSymbolName",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("symbol search failed: %v", err)
	}
	if results.TotalMatches == 0 {
		t.Fatal("expected at least one match for sym:uniqueSymbolName")
	}
	for _, r := range results.Results {
		t.Logf("  %s:%d - %s", r.FilePath, r.LineNumber, r.LineContent)
	}
}
