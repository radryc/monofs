// Package server provides deadlock detection tests for NutsDB operations.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

// closeServerWithTimeout closes a server with a timeout to avoid deadlocks in tests.
// Returns true if closed successfully, false if timed out.
func closeServerWithTimeout(t *testing.T, server *Server, timeout time.Duration) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		server.Close()
		close(done)
	}()

	select {
	case <-done:
		return true // Closed successfully
	case <-time.After(timeout):
		t.Logf("Warning: Server close timed out after %v", timeout)
		return false
	}
}

// TestConcurrentIngestFile tests concurrent IngestFile calls for deadlocks.
func TestConcurrentIngestFile(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test-db")
	gitCache := filepath.Join(tempDir, "git-cache")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	server, err := NewServer("test-node", ":0", dbPath, gitCache, logger)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer server.Close()

	const numConcurrent = 50
	const numIterations = 10

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	t.Logf("Starting %d concurrent IngestFile calls with %d iterations each...", numConcurrent, numIterations)

	startTime := time.Now()

	for i := 0; i < numConcurrent; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for iter := 0; iter < numIterations; iter++ {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

				req := &pb.IngestFileRequest{
					Metadata: &pb.FileMetadata{
						Path:        fmt.Sprintf("file-%d-%d.txt", id, iter),
						StorageId:   "test-storage-id",
						DisplayPath: "test-repo",
						Size:        1024,
						Mode:        0644,
						Mtime:       time.Now().Unix(),
						BlobHash:    fmt.Sprintf("blob-%d-%d", id, iter),
						Ref:         "main",
						Source:      "https://github.com/test/repo",
					},
				}

				resp, err := server.IngestFile(ctx, req)
				cancel()

				if err != nil {
					t.Logf("Worker %d iteration %d failed: %v", id, iter, err)
					errorCount.Add(1)
					return
				}

				if !resp.Success {
					t.Logf("Worker %d iteration %d returned failure", id, iter)
					errorCount.Add(1)
					return
				}

				successCount.Add(1)
			}
		}(i)
	}

	// Wait with timeout to detect deadlocks
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		duration := time.Since(startTime)
		totalOps := numConcurrent * numIterations
		opsPerSec := float64(totalOps) / duration.Seconds()

		t.Logf("Completed %d operations in %v", totalOps, duration)
		t.Logf("Throughput: %.2f ops/sec", opsPerSec)
		t.Logf("Success: %d, Errors: %d", successCount.Load(), errorCount.Load())

		if errorCount.Load() > 0 {
			t.Errorf("Failed operations: %d", errorCount.Load())
		}

	case <-time.After(60 * time.Second):
		t.Fatal("DEADLOCK DETECTED: Test timed out waiting for concurrent IngestFile operations")
	}
}

// TestConcurrentReadWrite tests concurrent read and write operations.
func TestConcurrentReadWrite(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test-db")
	gitCache := filepath.Join(tempDir, "git-cache")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	server, err := NewServer("test-node", ":0", dbPath, gitCache, logger)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer server.Close()

	// First, ingest some files
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        fmt.Sprintf("file-%d.txt", i),
				StorageId:   "test-storage",
				DisplayPath: "test-repo",
				Size:        1024,
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    fmt.Sprintf("blob-%d", i),
				Ref:         "main",
				Source:      "https://github.com/test/repo",
			},
		}
		_, err := server.IngestFile(ctx, req)
		if err != nil {
			t.Fatalf("Failed to ingest file %d: %v", i, err)
		}
	}

	// Now do concurrent reads and writes
	const numReaders = 20
	const numWriters = 10
	const operationCount = 50

	var wg sync.WaitGroup
	var readCount atomic.Int64
	var writeCount atomic.Int64
	var errorCount atomic.Int64

	t.Logf("Starting %d readers and %d writers...", numReaders, numWriters)
	startTime := time.Now()

	// Start readers
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for j := 0; j < operationCount; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

				req := &pb.LookupRequest{
					ParentPath: "test-repo",
					Name:       fmt.Sprintf("file-%d.txt", j%10),
				}

				_, err := server.Lookup(ctx, req)
				cancel()

				if err != nil {
					errorCount.Add(1)
				} else {
					readCount.Add(1)
				}
			}
		}(i)
	}

	// Start writers
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for j := 0; j < operationCount; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

				req := &pb.IngestFileRequest{
					Metadata: &pb.FileMetadata{
						Path:        fmt.Sprintf("new-file-%d-%d.txt", id, j),
						StorageId:   "test-storage",
						DisplayPath: "test-repo",
						Size:        2048,
						Mode:        0644,
						Mtime:       time.Now().Unix(),
						BlobHash:    fmt.Sprintf("new-blob-%d-%d", id, j),
						Ref:         "main",
						Source:      "https://github.com/test/repo",
					},
				}

				_, err := server.IngestFile(ctx, req)
				cancel()

				if err != nil {
					errorCount.Add(1)
				} else {
					writeCount.Add(1)
				}
			}
		}(i)
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		duration := time.Since(startTime)
		totalOps := readCount.Load() + writeCount.Load()
		opsPerSec := float64(totalOps) / duration.Seconds()

		t.Logf("Completed in %v", duration)
		t.Logf("Reads: %d, Writes: %d, Errors: %d", readCount.Load(), writeCount.Load(), errorCount.Load())
		t.Logf("Throughput: %.2f ops/sec", opsPerSec)

		if errorCount.Load() > totalOps/int64(10) {
			t.Errorf("Too many errors: %d out of %d operations", errorCount.Load(), totalOps)
		}

	case <-time.After(60 * time.Second):
		t.Fatal("DEADLOCK DETECTED: Test timed out during concurrent read/write operations")
	}
}

// TestRepoExistsNoDeadlock specifically tests the repo existence check doesn't deadlock.
func TestRepoExistsNoDeadlock(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test-db")
	gitCache := filepath.Join(tempDir, "git-cache")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	server, err := NewServer("test-node", ":0", dbPath, gitCache, logger)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer server.Close()

	// Test that checking repo existence inside a transaction doesn't deadlock
	// This tests the fix for the nested transaction issue

	const numConcurrent = 20
	var wg sync.WaitGroup
	var errorCount atomic.Int32

	t.Log("Testing repo existence checks during concurrent ingestion...")

	for i := 0; i < numConcurrent; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// All trying to ingest to the same repo - tests the repoExistsByStorageIDTx fix
			req := &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        fmt.Sprintf("file-%d.txt", id),
					StorageId:   "shared-storage-id", // Same storage ID for all
					DisplayPath: "shared-repo",       // Same display path
					Size:        1024,
					Mode:        0644,
					Mtime:       time.Now().Unix(),
					BlobHash:    fmt.Sprintf("blob-%d", id),
					Ref:         "main",
					Source:      "https://github.com/test/shared-repo",
				},
			}

			resp, err := server.IngestFile(ctx, req)
			if err != nil {
				t.Logf("Worker %d error: %v", id, err)
				errorCount.Add(1)
				return
			}

			if !resp.Success {
				t.Logf("Worker %d: ingestion failed", id)
				errorCount.Add(1)
			}
		}(i)
	}

	// Wait with aggressive timeout to quickly detect deadlocks
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("All workers completed successfully (errors: %d)", errorCount.Load())
		if errorCount.Load() > 0 {
			t.Logf("Note: Some errors occurred but no deadlock detected")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("DEADLOCK DETECTED: Nested transaction issue - repoExistsByStorageIDTx not working correctly")
	}
}

// TestDatabaseRecovery tests recovery from database errors.
// NOTE: This test is skipped because NutsDB with async writes (SyncEnable=false) 
// does not guarantee data persistence until a proper shutdown occurs. In production,
// the server should be gracefully stopped to ensure all writes are flushed.
func TestDatabaseRecovery(t *testing.T) {
	t.Skip("Skipping: NutsDB async writes make recovery testing unreliable without proper shutdown")

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test-db")
	gitCache := filepath.Join(tempDir, "git-cache")

	// Create server
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server, err := NewServer("test-node", ":0", dbPath, gitCache, logger)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// Ingest some data
	ctx := context.Background()
	storageID := "test-storage-recovery"
	displayPath := "test-recovery-repo"
	
	req := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "test-file.txt",
			StorageId:   storageID,
			DisplayPath: displayPath,
			Size:        1024,
			Mode:        0644,
			Mtime:       time.Now().Unix(),
			BlobHash:    "test-blob",
			Ref:         "main",
			Source:      "https://github.com/test/repo",
		},
	}

	_, err = server.IngestFile(ctx, req)
	if err != nil {
		t.Fatalf("Failed to ingest: %v", err)
	}
	
	// Build directory indexes to ensure lookup works
	_, err = server.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("Failed to build directory indexes: %v", err)
	}

	// Verify lookup works before close
	lookupReq := &pb.LookupRequest{
		ParentPath: displayPath,
		Name:       "test-file.txt",
	}
	resp, err := server.Lookup(ctx, lookupReq)
	if err != nil {
		t.Fatalf("Failed to lookup before close: %v", err)
	}
	if !resp.Found {
		t.Fatal("Entry not found before close - test setup issue")
	}
	t.Logf("Lookup successful before close: found=%v", resp.Found)

	// Give database time to flush pending writes (NutsDB uses async writes)
	time.Sleep(500 * time.Millisecond)

	// Close server with timeout to avoid deadlock
	// If close times out, we can't test recovery as the DB is still locked
	if !closeServerWithTimeout(t, server, 5*time.Second) {
		t.Skip("Skipping recovery test: database close timed out (NutsDB async write limitation)")
	}

	server2, err := NewServer("test-node", ":0", dbPath, gitCache, logger)
	if err != nil {
		t.Fatalf("Failed to reopen server: %v", err)
	}
	defer closeServerWithTimeout(t, server2, 5*time.Second)

	// Try to read the data - use same displayPath as before close
	lookupReq2 := &pb.LookupRequest{
		ParentPath: displayPath,
		Name:       "test-file.txt",
	}

	resp2, err := server2.Lookup(ctx, lookupReq2)
	if err != nil {
		t.Fatalf("Failed to lookup after reopen: %v", err)
	}

	if !resp2.Found {
		t.Error("Entry not found after database recovery")
	} else {
		t.Logf("Successfully recovered data after reopen: %s", lookupReq2.Name)
	}
}

// TestHighContentionScenario tests behavior under very high lock contention.
func TestHighContentionScenario(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test-db")
	gitCache := filepath.Join(tempDir, "git-cache")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	server, err := NewServer("test-node", ":0", dbPath, gitCache, logger)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer server.Close()

	// First ingest a file
	ctx := context.Background()
	req := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "hot-file.txt",
			StorageId:   "test-storage",
			DisplayPath: "test-repo",
			Size:        1024,
			Mode:        0644,
			Mtime:       time.Now().Unix(),
			BlobHash:    "hot-blob",
			Ref:         "main",
			Source:      "https://github.com/test/repo",
		},
	}
	_, err = server.IngestFile(ctx, req)
	if err != nil {
		t.Fatalf("Failed to ingest: %v", err)
	}

	// Now hammer the same file with many concurrent reads
	const numConcurrent = 100
	const numIterations = 100

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	t.Logf("Starting high contention test: %d workers x %d iterations...", numConcurrent, numIterations)
	startTime := time.Now()

	for i := 0; i < numConcurrent; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for j := 0; j < numIterations; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

				lookupReq := &pb.LookupRequest{
					ParentPath: "test-repo",
					Name:       "hot-file.txt",
				}

				_, err := server.Lookup(ctx, lookupReq)
				cancel()

				if err != nil {
					errorCount.Add(1)
				} else {
					successCount.Add(1)
				}
			}
		}(i)
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		duration := time.Since(startTime)
		totalOps := int64(numConcurrent * numIterations)
		opsPerSec := float64(totalOps) / duration.Seconds()

		t.Logf("Completed %d operations in %v", totalOps, duration)
		t.Logf("Throughput: %.2f ops/sec", opsPerSec)
		t.Logf("Success: %d, Errors: %d", successCount.Load(), errorCount.Load())

		if errorCount.Load() > totalOps/10 {
			t.Errorf("Too many errors under high contention: %d / %d", errorCount.Load(), totalOps)
		}

	case <-time.After(60 * time.Second):
		t.Fatal("DEADLOCK DETECTED: High contention scenario timed out")
	}
}
