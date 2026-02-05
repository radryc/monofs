package search

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

func setupTestService(t *testing.T) (*Service, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "monofs-search-service-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg := Config{
		IndexDir:  filepath.Join(tmpDir, "indexes"),
		CacheDir:  filepath.Join(tmpDir, "cache"),
		Workers:   1,
		QueueSize: 10,
		Logger:    logger,
	}

	svc, err := NewService(cfg)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create service: %v", err)
	}

	return svc, tmpDir
}

func TestNewService(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	if svc == nil {
		t.Fatal("NewService returned nil")
	}

	// Verify directories were created
	if _, err := os.Stat(filepath.Join(tmpDir, "indexes")); os.IsNotExist(err) {
		t.Error("index directory was not created")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "cache")); os.IsNotExist(err) {
		t.Error("cache directory was not created")
	}
}

func TestService_Close(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)

	err := svc.Close()
	if err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

func TestService_GetStats(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	resp, err := svc.GetStats(context.Background(), &pb.StatsRequest{})
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}

	if resp == nil {
		t.Fatal("GetStats returned nil response")
	}

	if resp.QueueLength < 0 {
		t.Errorf("expected non-negative queue length, got %d", resp.QueueLength)
	}

	t.Logf("Stats: indexes=%d, searches=%d, queue=%d",
		resp.TotalIndexes, resp.SearchesTotal, resp.QueueLength)
}

func TestService_IndexRepository_Queue(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	req := &pb.IndexRequest{
		StorageId:   "test-repo-123",
		DisplayPath: "test/repo",
		Source:      "https://github.com/example/test.git",
		Ref:         "main",
	}

	resp, err := svc.IndexRepository(context.Background(), req)
	if err != nil {
		t.Fatalf("IndexRepository failed: %v", err)
	}

	if !resp.Queued {
		t.Error("expected job to be queued")
	}

	if resp.JobId == "" {
		t.Error("expected non-empty job ID")
	}

	t.Logf("Job queued: id=%s, message=%s", resp.JobId, resp.Message)
}

func TestService_GetIndexStatus_NotFound(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	resp, err := svc.GetIndexStatus(context.Background(), &pb.IndexStatusRequest{
		StorageId: "nonexistent-repo",
	})
	if err != nil {
		t.Fatalf("GetIndexStatus failed: %v", err)
	}

	if resp.Status != pb.IndexStatus_INDEX_STATUS_NOT_FOUND {
		t.Errorf("expected NOT_FOUND status, got %v", resp.Status)
	}
}

func TestService_ListIndexes_Empty(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	resp, err := svc.ListIndexes(context.Background(), &pb.ListIndexesRequest{})
	if err != nil {
		t.Fatalf("ListIndexes failed: %v", err)
	}

	if len(resp.Indexes) != 0 {
		t.Errorf("expected empty index list, got %d", len(resp.Indexes))
	}
}

func TestService_Search_Empty(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	resp, err := svc.Search(context.Background(), &pb.SearchRequest{
		Query:      "test",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if resp.TotalMatches != 0 {
		t.Errorf("expected 0 matches on empty index, got %d", resp.TotalMatches)
	}
}

func TestService_DeleteIndex_NotFound(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	resp, err := svc.DeleteIndex(context.Background(), &pb.DeleteIndexRequest{
		StorageId: "nonexistent-repo",
	})
	if err != nil {
		t.Fatalf("DeleteIndex failed: %v", err)
	}

	// Deleting non-existent index should still succeed
	if !resp.Success {
		t.Error("expected delete to succeed even for non-existent index")
	}
}

func TestService_QueueOverflow(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-search-queue-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Create service with very small queue
	cfg := Config{
		IndexDir:  filepath.Join(tmpDir, "indexes"),
		CacheDir:  filepath.Join(tmpDir, "cache"),
		Workers:   0, // No workers to prevent queue drain
		QueueSize: 2, // Very small queue
		Logger:    logger,
	}

	svc, err := NewService(cfg)
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}
	defer svc.Close()

	// Fill the queue
	for i := 0; i < 3; i++ {
		req := &pb.IndexRequest{
			StorageId:   "test-repo-" + string(rune('a'+i)),
			DisplayPath: "test/repo",
			Source:      "https://github.com/example/test.git",
			Ref:         "main",
		}

		resp, err := svc.IndexRepository(context.Background(), req)
		if err != nil {
			t.Fatalf("IndexRepository failed: %v", err)
		}

		if i < 2 {
			if !resp.Queued {
				t.Errorf("expected job %d to be queued", i)
			}
		} else {
			// Third request should fail due to full queue
			if resp.Queued {
				t.Error("expected queue to be full")
			}
		}
	}
}

func TestService_IndexAndSearch_Integration(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	// Create a test file directly in the index
	testFiles := map[string]string{
		"main.go": `package main

import "fmt"

func main() {
	fmt.Println("Hello from MonoFS!")
}
`,
	}

	// Create repo directory
	repoDir := filepath.Join(tmpDir, "test-repo")
	for path, content := range testFiles {
		fullPath := filepath.Join(repoDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
	}

	// Index directly using the indexer
	_, err := svc.indexer.IndexLocalDir(context.Background(), IndexLocalRequest{
		StorageID:   "test-repo",
		DisplayPath: "test/repo",
		SourceDir:   repoDir,
		Ref:         "main",
	})
	if err != nil {
		t.Fatalf("indexing failed: %v", err)
	}

	// Reload searcher
	if err := svc.indexer.ReloadSearcher(); err != nil {
		t.Fatalf("failed to reload searcher: %v", err)
	}

	// Give a moment for things to settle
	time.Sleep(100 * time.Millisecond)

	// Search
	resp, err := svc.Search(context.Background(), &pb.SearchRequest{
		Query:      "MonoFS",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if resp.TotalMatches == 0 {
		t.Error("expected at least one match for 'MonoFS'")
	}

	t.Logf("Search found %d matches in %dms", resp.TotalMatches, resp.DurationMs)
	for _, r := range resp.Results {
		t.Logf("  %s:%d - %s", r.FilePath, r.LineNumber, r.LineContent)
	}
}

func TestService_RebuildAllIndexes(t *testing.T) {
	svc, tmpDir := setupTestService(t)
	defer os.RemoveAll(tmpDir)
	defer svc.Close()

	// This should work even with no indexes
	resp, err := svc.RebuildAllIndexes(context.Background(), &pb.RebuildAllIndexesRequest{})
	if err != nil {
		t.Fatalf("RebuildAllIndexes failed: %v", err)
	}

	// With no indexes, queued count should be 0
	if resp.QueuedCount < 0 {
		t.Errorf("expected non-negative queued count, got %d", resp.QueuedCount)
	}

	t.Logf("RebuildAllIndexes: queued=%d, message=%s", resp.QueuedCount, resp.Message)
}
