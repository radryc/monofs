package router

import (
	"sync"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/git"
	"github.com/radryc/monofs/internal/sharding"
)

// TestShardingDistribution verifies that files are distributed across multiple nodes
func TestShardingDistribution(t *testing.T) {
	nodes := []sharding.Node{
		{ID: "node1", Address: "localhost:9001", Weight: 100, Healthy: true},
		{ID: "node2", Address: "localhost:9002", Weight: 100, Healthy: true},
		{ID: "node3", Address: "localhost:9003", Weight: 100, Healthy: true},
		{ID: "node4", Address: "localhost:9004", Weight: 100, Healthy: true},
		{ID: "node5", Address: "localhost:9005", Weight: 100, Healthy: true},
	}

	sharder := sharding.NewHRW(nodes)
	storageID := "test-storage-123"

	filePaths := []string{
		"README.md",
		"src/main.go",
		"docs/guide.md",
		"test/unit_test.go",
		"config/settings.yaml",
		"lib/utils.go",
		"cmd/server/main.go",
		"internal/handlers.go",
		"pkg/api/types.go",
		"scripts/deploy.sh",
	}

	nodeDistribution := make(map[string]int)

	for _, filePath := range filePaths {
		shardKey := storageID + ":" + filePath
		node := sharder.GetNode(shardKey)
		if node == nil {
			t.Fatalf("no node selected for path: %s", filePath)
		}
		nodeDistribution[node.ID]++
	}

	t.Logf("File distribution across nodes:")
	for nodeID, count := range nodeDistribution {
		t.Logf("  %s: %d files", nodeID, count)
	}

	if len(nodeDistribution) < 2 {
		t.Errorf("files not distributed: all files went to %d node(s), expected at least 2", len(nodeDistribution))
	}

	for nodeID, count := range nodeDistribution {
		if count == len(filePaths) {
			t.Errorf("all files went to single node %s, sharding not working", nodeID)
		}
	}
}

// TestShardingDifferentStorageIDs verifies that same file in different repos goes to different nodes
func TestShardingDifferentStorageIDs(t *testing.T) {
	nodes := []sharding.Node{
		{ID: "node1", Address: "localhost:9001", Weight: 100, Healthy: true},
		{ID: "node2", Address: "localhost:9002", Weight: 100, Healthy: true},
		{ID: "node3", Address: "localhost:9003", Weight: 100, Healthy: true},
		{ID: "node4", Address: "localhost:9004", Weight: 100, Healthy: true},
	}

	sharder := sharding.NewHRW(nodes)
	filePath := "README.md"

	storageIDs := []string{
		"storage-repo1",
		"storage-repo2",
		"storage-repo3",
		"storage-repo4",
		"storage-repo5",
	}

	uniqueNodes := make(map[string]bool)

	for _, storageID := range storageIDs {
		shardKey := storageID + ":" + filePath
		node := sharder.GetNode(shardKey)
		if node == nil {
			t.Fatalf("no node selected for %s", storageID)
		}
		uniqueNodes[node.ID] = true
		t.Logf("%s/%s -> %s", storageID, filePath, node.ID)
	}

	if len(uniqueNodes) < 2 {
		t.Errorf("all repos' README.md went to same node, storageID not being used in shard key")
	}
}

// TestBatchingByNode verifies files are correctly grouped by target node
func TestBatchingByNode(t *testing.T) {
	nodes := []sharding.Node{
		{ID: "node1", Address: "localhost:9001", Weight: 100, Healthy: true},
		{ID: "node2", Address: "localhost:9002", Weight: 100, Healthy: true},
		{ID: "node3", Address: "localhost:9003", Weight: 100, Healthy: true},
	}

	sharder := sharding.NewHRW(nodes)
	storageID := "test-storage-789"

	files := []git.FileMetadata{
		{Path: "file1.txt", Size: 100, Mode: 0644},
		{Path: "file2.txt", Size: 200, Mode: 0644},
		{Path: "file3.txt", Size: 300, Mode: 0644},
		{Path: "file4.txt", Size: 400, Mode: 0644},
		{Path: "file5.txt", Size: 500, Mode: 0644},
		{Path: "file6.txt", Size: 600, Mode: 0644},
	}

	batches := make(map[string][]*pb.FileMetadata)

	for _, meta := range files {
		shardKey := storageID + ":" + meta.Path
		node := sharder.GetNode(shardKey)
		if node == nil {
			t.Fatalf("no node for file: %s", meta.Path)
		}

		batches[node.ID] = append(batches[node.ID], &pb.FileMetadata{
			Path:        meta.Path,
			Size:        meta.Size,
			Mode:        meta.Mode,
			StorageId:   storageID,
			DisplayPath: "test/repo",
		})
	}

	t.Logf("Files grouped by node:")
	totalFiles := 0
	for nodeID, batch := range batches {
		t.Logf("  %s: %d files", nodeID, len(batch))
		totalFiles += len(batch)
	}

	if totalFiles != len(files) {
		t.Errorf("file count mismatch: expected %d, got %d", len(files), totalFiles)
	}

	if len(batches) < 2 {
		t.Errorf("files not distributed across nodes: only %d node(s) used", len(batches))
	}
}

// TestConcurrentSharding verifies sharding works correctly under concurrent access
func TestConcurrentSharding(t *testing.T) {
	nodes := []sharding.Node{
		{ID: "node1", Address: "localhost:9001", Weight: 100, Healthy: true},
		{ID: "node2", Address: "localhost:9002", Weight: 100, Healthy: true},
		{ID: "node3", Address: "localhost:9003", Weight: 100, Healthy: true},
	}

	sharder := sharding.NewHRW(nodes)
	storageID := "concurrent-test"

	numWorkers := 50
	filesPerWorker := 20

	var wg sync.WaitGroup
	results := make(chan string, numWorkers*filesPerWorker)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < filesPerWorker; i++ {
				filePath := "worker_" + string(rune('a'+(workerID%26))) + "_file_" + string(rune('0'+(i%10))) + ".txt"
				shardKey := storageID + ":" + filePath
				node := sharder.GetNode(shardKey)
				if node != nil {
					results <- node.ID
				}
			}
		}(w)
	}

	wg.Wait()
	close(results)

	distribution := make(map[string]int)
	totalCount := 0
	for nodeID := range results {
		distribution[nodeID]++
		totalCount++
	}

	t.Logf("Concurrent sharding distribution:")
	for nodeID, count := range distribution {
		t.Logf("  %s: %d files", nodeID, count)
	}

	if len(distribution) < 2 {
		t.Errorf("concurrent sharding failed: only %d node(s) used", len(distribution))
	}

	expectedTotal := numWorkers * filesPerWorker
	if totalCount != expectedTotal {
		t.Errorf("file count mismatch: expected %d, got %d", expectedTotal, totalCount)
	}
}
