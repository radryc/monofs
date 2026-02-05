package server

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
)

// TestGetRepositoryFiles tests retrieving files owned by a node.
func TestGetRepositoryFiles(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "test-storage-123"

	// Register repository first
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "github.com/test/repo",
		Source:      "https://github.com/test/repo.git",
	})
	if err != nil {
		t.Fatalf("failed to register repository: %v", err)
	}

	// Ingest some files
	files := []string{"README.md", "src/main.go", "src/utils.go"}
	for _, file := range files {
		_, err := server.IngestFile(ctx, &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        file,
				StorageId:   storageID,
				DisplayPath: "github.com/test/repo",
				Size:        100,
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    "abc123",
				Ref:         "main",
				Source:      "https://github.com/test/repo.git",
			},
		})
		if err != nil {
			t.Fatalf("failed to ingest file %s: %v", file, err)
		}
	}

	// Get repository files
	resp, err := server.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("GetRepositoryFiles failed: %v", err)
	}

	if resp.NodeId != "test-node" {
		t.Errorf("expected node_id='test-node', got '%s'", resp.NodeId)
	}

	if len(resp.Files) != len(files) {
		t.Errorf("expected %d files, got %d", len(files), len(resp.Files))
	}

	// Verify all files are present
	fileMap := make(map[string]bool)
	for _, f := range resp.Files {
		fileMap[f] = true
	}

	for _, expected := range files {
		if !fileMap[expected] {
			t.Errorf("expected file %s not found in response", expected)
		}
	}
}

// TestGetRepositoryFilesEmpty tests getting files for empty repository.
func TestGetRepositoryFilesEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()

	resp, err := server.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
		StorageId: "nonexistent-repo",
	})
	if err != nil {
		t.Fatalf("GetRepositoryFiles failed: %v", err)
	}

	if len(resp.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(resp.Files))
	}
}

// TestSyncMetadataFromNode tests metadata sync for recovery.
func TestSyncMetadataFromNode(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "test-storage-456"

	// Register repository
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "github.com/user/repo",
		Source:      "https://github.com/user/repo.git",
	})
	if err != nil {
		t.Fatalf("failed to register repository: %v", err)
	}

	// SyncMetadataFromNode syncs from replicaFiles to failover cache
	// Since we don't have replica data, it should succeed but report missing files
	resp, err := server.SyncMetadataFromNode(ctx, &pb.SyncMetadataFromNodeRequest{
		SourceNodeId: "failed-node",
		TargetNodeId: "backup-node",
		Files: []*pb.FileInfo{
			{StorageId: storageID, FilePath: "test.txt"},
		},
	})

	// Sync should succeed even if files are missing (counts them as missing)
	if err != nil {
		t.Fatalf("SyncMetadataFromNode failed: %v", err)
	}

	if !resp.Success {
		t.Error("expected success=true (sync succeeded, but with missing files)")
	}

	// Should have 0 files synced since we had no replica data
	if resp.FilesSynced != 0 {
		t.Errorf("expected 0 files synced, got %d", resp.FilesSynced)
	}
}

// TestClearFailoverCache tests clearing failover metadata.
func TestClearFailoverCache(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	failedNodeID := "failed-node-123"
	storageID := "test-storage-789"

	// Add entries to bucketFailover (the actual failover cache)
	// Key format: "failedNodeID:storageID:filePath"
	err = server.db.Update(func(tx *nutsdb.Tx) error {
		if err := tx.Put(bucketFailover, []byte(failedNodeID+":"+storageID+":file1.txt"), []byte("metadata1"), 0); err != nil {
			return err
		}
		if err := tx.Put(bucketFailover, []byte(failedNodeID+":"+storageID+":file2.txt"), []byte("metadata2"), 0); err != nil {
			return err
		}
		if err := tx.Put(bucketFailover, []byte(failedNodeID+":"+storageID+":file3.txt"), []byte("metadata3"), 0); err != nil {
			return err
		}
		// Add an entry for a different node that should NOT be cleared
		return tx.Put(bucketFailover, []byte("other-node:other-storage:file4.txt"), []byte("metadata4"), 0)
	})
	if err != nil {
		t.Fatalf("failed to setup test data: %v", err)
	}

	// Clear failover cache
	resp, err := server.ClearFailoverCache(ctx, &pb.ClearFailoverCacheRequest{
		RecoveredNodeId: failedNodeID,
	})
	if err != nil {
		t.Fatalf("ClearFailoverCache failed: %v", err)
	}

	if !resp.Success {
		t.Error("expected success=true")
	}

	if resp.EntriesCleared != 3 {
		t.Errorf("expected 3 entries cleared, got %d", resp.EntriesCleared)
	}

	// Verify only the failed node's entries were cleared
	count := 0
	err = server.db.View(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.PrefixScanEntries(bucketFailover, []byte(""), "", 0, -1, true, false)
		if err != nil && err != nutsdb.ErrBucketNotFound && err != nutsdb.ErrPrefixScan {
			return err
		}
		count = len(keys)
		return nil
	})
	if err != nil {
		t.Fatalf("failed to verify cleanup: %v", err)
	}

	if count != 1 {
		t.Errorf("expected 1 remaining failover entry (other-node), got %d", count)
	}
}

// TestFailoverCacheCheck tests checking failover cache.
func TestFailoverCacheCheck(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	// Test non-existent file
	meta, found := server.checkFailoverCache("test-storage", "nonexistent.txt")
	if found {
		t.Error("expected not found for nonexistent file")
	}
	if meta != nil {
		t.Error("expected nil metadata for nonexistent key")
	}
}

// TestOwnershipTracking tests file ownership tracking.
func TestOwnershipTracking(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "ownership-test"

	// Register repository
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "github.com/owner/repo",
		Source:      "https://github.com/owner/repo.git",
	})
	if err != nil {
		t.Fatalf("failed to register repository: %v", err)
	}

	// Ingest file
	filePath := "owned-file.txt"
	_, err = server.IngestFile(ctx, &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        filePath,
			StorageId:   storageID,
			DisplayPath: "github.com/owner/repo",
			Size:        50,
			Mode:        0644,
			Mtime:       time.Now().Unix(),
			BlobHash:    "hash123",
			Ref:         "main",
			Source:      "https://github.com/owner/repo.git",
		},
	})
	if err != nil {
		t.Fatalf("failed to ingest file: %v", err)
	}

	// Verify ownership tracking
	ownershipKey := []byte(storageID + ":" + filePath)
	err = server.db.View(func(tx *nutsdb.Tx) error {
		_, err := tx.Get(bucketOwnedFiles, ownershipKey)
		return err
	})
	if err != nil {
		t.Errorf("expected file to be tracked as owned: %v", err)
	}
}

// TestReplicaTracking tests replica file tracking.
func TestReplicaTracking(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	// Manually add replica tracking
	ownerNodeID := "primary-node"
	storageID := "replica-test"
	filePath := "replica-file.txt"

	key := []byte(storageID + ":" + filePath)
	err = server.db.Update(func(tx *nutsdb.Tx) error {
		return tx.Put(bucketReplicaFiles, key, []byte(ownerNodeID), 0)
	})
	if err != nil {
		t.Fatalf("failed to add replica tracking: %v", err)
	}

	// Verify retrieval
	var value []byte
	err = server.db.View(func(tx *nutsdb.Tx) error {
		val, err := tx.Get(bucketReplicaFiles, key)
		if err != nil {
			return err
		}
		value = val
		return nil
	})
	if err != nil {
		t.Errorf("expected replica to be tracked: %v", err)
	}

	if string(value) != ownerNodeID {
		t.Errorf("expected owner='%s', got '%s'", ownerNodeID, string(value))
	}
}

// TestConcurrentFailoverOperations tests concurrent failover operations.
func TestConcurrentFailoverOperations(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "concurrent-test"

	// Register repository
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "github.com/concurrent/repo",
		Source:      "https://github.com/concurrent/repo.git",
	})
	if err != nil {
		t.Fatalf("failed to register repository: %v", err)
	}

	// Concurrently ingest files and query ownership
	done := make(chan bool)
	numGoroutines := 10
	filesPerGoroutine := 5

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer func() { done <- true }()

			for j := 0; j < filesPerGoroutine; j++ {
				filePath := filepath.Join("dir", "file", string(rune('a'+id)), string(rune('0'+j))+".txt")

				// Ingest
				_, err := server.IngestFile(ctx, &pb.IngestFileRequest{
					Metadata: &pb.FileMetadata{
						Path:        filePath,
						StorageId:   storageID,
						DisplayPath: "github.com/concurrent/repo",
						Size:        10,
						Mode:        0644,
						Mtime:       time.Now().Unix(),
						BlobHash:    "hash",
						Ref:         "main",
						Source:      "https://github.com/concurrent/repo.git",
					},
				})
				if err != nil {
					t.Errorf("goroutine %d: failed to ingest file: %v", id, err)
				}
			}
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Verify all files are tracked
	resp, err := server.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("GetRepositoryFiles failed: %v", err)
	}

	expectedCount := numGoroutines * filesPerGoroutine
	if len(resp.Files) != expectedCount {
		t.Errorf("expected %d files, got %d", expectedCount, len(resp.Files))
	}
}

// TestMultipleNodeFailoverCleanup tests cleaning up after multiple node failures.
func TestMultipleNodeFailoverCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()

	// Simulate multiple node failures - add entries to bucketFailover (the actual failover cache)
	nodes := []string{"node1", "node2", "node3"}
	err = server.db.Update(func(tx *nutsdb.Tx) error {
		for i, nodeID := range nodes {
			for j := 0; j < 5; j++ {
				// Key format: "nodeID:storageID:filePath"
				key := nodeID + ":storage" + string(rune('A'+i)) + ":file" + string(rune('0'+j)) + ".txt"
				if err := tx.Put(bucketFailover, []byte(key), []byte("metadata"), 0); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to setup test data: %v", err)
	}

	// Clear each node's failover cache
	for _, nodeID := range nodes {
		resp, err := server.ClearFailoverCache(ctx, &pb.ClearFailoverCacheRequest{
			RecoveredNodeId: nodeID,
		})
		if err != nil {
			t.Fatalf("failed to clear failover cache for %s: %v", nodeID, err)
		}
		if !resp.Success {
			t.Errorf("expected success for node %s", nodeID)
		}
	}

	// Verify all failover entries are cleaned up
	count := 0
	err = server.db.View(func(tx *nutsdb.Tx) error {
		keys, _, err := tx.PrefixScanEntries(bucketFailover, []byte(""), "", 0, -1, true, false)
		if err != nil && err != nutsdb.ErrBucketNotFound && err != nutsdb.ErrPrefixScan {
			return err
		}
		count = len(keys)
		return nil
	})
	if err != nil {
		t.Fatalf("failed to verify cleanup: %v", err)
	}

	if count != 0 {
		t.Errorf("expected 0 remaining failover entries, got %d", count)
	}
}

// BenchmarkGetRepositoryFiles benchmarks file ownership retrieval.
func BenchmarkGetRepositoryFiles(b *testing.B) {
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "bench.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("bench-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		b.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "bench-storage"

	// Register repository
	server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "github.com/bench/repo",
		Source:      "https://github.com/bench/repo.git",
	})

	// Add 1000 files
	for i := 0; i < 1000; i++ {
		filePath := filepath.Join("dir", "file"+string(rune(i))+".txt")
		server.IngestFile(ctx, &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        filePath,
				StorageId:   storageID,
				DisplayPath: "github.com/bench/repo",
				Size:        100,
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    "hash",
				Ref:         "main",
				Source:      "https://github.com/bench/repo.git",
			},
		})
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := server.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
			StorageId: storageID,
		})
		if err != nil {
			b.Fatalf("GetRepositoryFiles failed: %v", err)
		}
	}
}

// ============================================================================
// N-Replica Ingestion Tests
// ============================================================================

// TestIngestReplicaBatch tests replica metadata ingestion for failover.
func TestIngestReplicaBatch(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "test-storage-replica"
	displayPath := "github.com/test/replicated"
	primaryNodeID := "primary-node-1"

	// Ingest replica batch
	files := []*pb.FileMetadata{
		{Path: "README.md", StorageId: storageID, DisplayPath: displayPath, Size: 100, Mode: 0644, Mtime: time.Now().Unix(), BlobHash: "hash1", Ref: "main", Source: "https://github.com/test/replicated.git"},
		{Path: "src/main.go", StorageId: storageID, DisplayPath: displayPath, Size: 200, Mode: 0644, Mtime: time.Now().Unix(), BlobHash: "hash2", Ref: "main", Source: "https://github.com/test/replicated.git"},
		{Path: "src/utils.go", StorageId: storageID, DisplayPath: displayPath, Size: 300, Mode: 0644, Mtime: time.Now().Unix(), BlobHash: "hash3", Ref: "main", Source: "https://github.com/test/replicated.git"},
	}

	resp, err := server.IngestReplicaBatch(ctx, &pb.IngestReplicaBatchRequest{
		Files:         files,
		StorageId:     storageID,
		DisplayPath:   displayPath,
		PrimaryNodeId: primaryNodeID,
		Source:        "https://github.com/test/replicated.git",
		Ref:           "main",
	})

	if err != nil {
		t.Fatalf("IngestReplicaBatch failed: %v", err)
	}

	if !resp.Success {
		t.Errorf("expected success=true, got false: %s", resp.ErrorMessage)
	}

	if resp.FilesReplicated != 3 {
		t.Errorf("expected 3 files replicated, got %d", resp.FilesReplicated)
	}

	// Verify replica data is in bucketReplicaFiles
	replicaCount := 0
	err = server.db.View(func(tx *nutsdb.Tx) error {
		for _, file := range files {
			replicaKey := makeReplicaKey(storageID, file.Path, primaryNodeID)
			_, err := tx.Get(bucketReplicaFiles, replicaKey)
			if err == nil {
				replicaCount++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to verify replica storage: %v", err)
	}

	if replicaCount != 3 {
		t.Errorf("expected 3 replicas in bucketReplicaFiles, got %d", replicaCount)
	}

	// Verify metadata is also stored (for serving during failover)
	metadataCount := 0
	err = server.db.View(func(tx *nutsdb.Tx) error {
		for _, file := range files {
			metaKey := makeStorageKey(storageID, file.Path)
			_, err := tx.Get(bucketMetadata, metaKey)
			if err == nil {
				metadataCount++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to verify metadata storage: %v", err)
	}

	if metadataCount != 3 {
		t.Errorf("expected 3 metadata entries, got %d", metadataCount)
	}
}

// TestIngestReplicaBatchEmpty tests empty batch handling.
func TestIngestReplicaBatchEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()

	resp, err := server.IngestReplicaBatch(ctx, &pb.IngestReplicaBatchRequest{
		Files:         []*pb.FileMetadata{},
		StorageId:     "empty-storage",
		DisplayPath:   "github.com/empty/repo",
		PrimaryNodeId: "primary-node",
	})

	if err != nil {
		t.Fatalf("IngestReplicaBatch failed for empty batch: %v", err)
	}

	if !resp.Success {
		t.Error("expected success=true for empty batch")
	}

	if resp.FilesReplicated != 0 {
		t.Errorf("expected 0 files replicated, got %d", resp.FilesReplicated)
	}
}

// TestIngestReplicaBatchRegistersRepo tests that replica ingestion auto-registers repo.
func TestIngestReplicaBatchRegistersRepo(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "auto-registered-repo"
	displayPath := "github.com/auto/repo"

	// Ingest replica WITHOUT prior RegisterRepository
	files := []*pb.FileMetadata{
		{Path: "file.txt", StorageId: storageID, DisplayPath: displayPath, Size: 50, Mode: 0644, Mtime: time.Now().Unix(), BlobHash: "hashX", Ref: "main", Source: "https://github.com/auto/repo.git"},
	}

	resp, err := server.IngestReplicaBatch(ctx, &pb.IngestReplicaBatchRequest{
		Files:         files,
		StorageId:     storageID,
		DisplayPath:   displayPath,
		PrimaryNodeId: "primary-node",
		Source:        "https://github.com/auto/repo.git",
		Ref:           "main",
	})

	if err != nil {
		t.Fatalf("IngestReplicaBatch failed: %v", err)
	}

	if !resp.Success {
		t.Errorf("expected success: %s", resp.ErrorMessage)
	}

	// Verify repo was auto-registered
	var repoExists bool
	err = server.db.View(func(tx *nutsdb.Tx) error {
		_, err := tx.Get(bucketRepos, []byte(storageID))
		repoExists = (err == nil)
		return nil
	})
	if err != nil {
		t.Fatalf("failed to check repo registration: %v", err)
	}

	if !repoExists {
		t.Error("expected repo to be auto-registered during replica ingestion")
	}
}

// TestReplicaDataUsedDuringFailover tests that replica data is used when SyncMetadataFromNode is called.
func TestReplicaDataUsedDuringFailover(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	storageID := "failover-test-storage"
	displayPath := "github.com/failover/repo"
	primaryNodeID := "failed-primary-node"

	// Step 1: Ingest replica data (simulating normal N-replica ingestion)
	files := []*pb.FileMetadata{
		{Path: "important.txt", StorageId: storageID, DisplayPath: displayPath, Size: 500, Mode: 0644, Mtime: time.Now().Unix(), BlobHash: "critical-hash", Ref: "main", Source: "https://github.com/failover/repo.git"},
	}

	_, err = server.IngestReplicaBatch(ctx, &pb.IngestReplicaBatchRequest{
		Files:         files,
		StorageId:     storageID,
		DisplayPath:   displayPath,
		PrimaryNodeId: primaryNodeID,
		Source:        "https://github.com/failover/repo.git",
		Ref:           "main",
	})
	if err != nil {
		t.Fatalf("IngestReplicaBatch failed: %v", err)
	}

	// Step 2: Simulate failover - SyncMetadataFromNode should find replica data
	syncResp, err := server.SyncMetadataFromNode(ctx, &pb.SyncMetadataFromNodeRequest{
		SourceNodeId: primaryNodeID,
		TargetNodeId: "backup-node",
		Files: []*pb.FileInfo{
			{StorageId: storageID, FilePath: "important.txt"},
		},
	})

	if err != nil {
		t.Fatalf("SyncMetadataFromNode failed: %v", err)
	}

	if !syncResp.Success {
		t.Errorf("expected sync success: %s", syncResp.Message)
	}

	// With replica data present, files should be synced to failover cache
	if syncResp.FilesSynced != 1 {
		t.Errorf("expected 1 file synced (from replica data), got %d", syncResp.FilesSynced)
	}

	// Step 3: Verify failover cache has the data
	var failoverCacheHasData bool
	err = server.db.View(func(tx *nutsdb.Tx) error {
		failoverKey := makeFailoverKey(primaryNodeID, storageID, "important.txt")
		_, err := tx.Get(bucketFailover, failoverKey)
		failoverCacheHasData = (err == nil)
		return nil
	})
	if err != nil {
		t.Fatalf("failed to check failover cache: %v", err)
	}

	if !failoverCacheHasData {
		t.Error("expected failover cache to have data after SyncMetadataFromNode")
	}
}

// TestConcurrentReplicaIngestion tests concurrent replica batch ingestion.
func TestConcurrentReplicaIngestion(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	server, err := NewServer("backup-node", "localhost:9001", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	numGoroutines := 5
	filesPerGoroutine := 20

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			storageID := "concurrent-repo-" + string(rune('A'+goroutineID))
			displayPath := "github.com/concurrent/repo" + string(rune('A'+goroutineID))

			files := make([]*pb.FileMetadata, filesPerGoroutine)
			for i := 0; i < filesPerGoroutine; i++ {
				files[i] = &pb.FileMetadata{
					Path:        "file" + string(rune('0'+i)) + ".txt",
					StorageId:   storageID,
					DisplayPath: displayPath,
					Size:        uint64(100 + i),
					Mode:        0644,
					Mtime:       time.Now().Unix(),
					BlobHash:    "hash-" + string(rune('A'+goroutineID)) + string(rune('0'+i)),
					Ref:         "main",
					Source:      "https://github.com/concurrent/repo.git",
				}
			}

			resp, err := server.IngestReplicaBatch(ctx, &pb.IngestReplicaBatchRequest{
				Files:         files,
				StorageId:     storageID,
				DisplayPath:   displayPath,
				PrimaryNodeId: "primary-node",
				Source:        "https://github.com/concurrent/repo.git",
				Ref:           "main",
			})

			if err != nil {
				errors <- err
				return
			}

			if !resp.Success {
				errors <- err
				return
			}

			if resp.FilesReplicated != int64(filesPerGoroutine) {
				errors <- err
			}
		}(g)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		if err != nil {
			t.Errorf("concurrent ingestion error: %v", err)
		}
	}
}
