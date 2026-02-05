// Package test provides stress and concurrency tests for MonoFS.
package test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/client"
	"github.com/radryc/monofs/internal/router"
	"github.com/radryc/monofs/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ============================================================================
// High Concurrency Tests
// ============================================================================

// TestHighConcurrencyIngestion tests server under moderate concurrent load.
// Reduced from 50 workers x 100 files to 10 workers x 20 files for faster CI execution.
func TestHighConcurrencyIngestion(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	env := newServerTestEnv(t, "high-concurrency-node", 19500)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "high-concurrency-storage"
	displayPath := "github_com/test/high-concurrency"
	repoURL := "https://github.com/test/high-concurrency.git"

	// Register repository
	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	const numWorkers = 10     // Reduced from 50
	const filesPerWorker = 20 // Reduced from 100

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64
	startTime := time.Now()

	t.Run("ConcurrentIngestion", func(t *testing.T) {
		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				for f := 0; f < filesPerWorker; f++ {
					_, err := env.client.IngestFile(ctx, &pb.IngestFileRequest{
						Metadata: &pb.FileMetadata{
							Path:        fmt.Sprintf("worker%03d/subdir/file_%05d.txt", workerID, f),
							StorageId:   storageID,
							DisplayPath: displayPath,
							Size:        uint64(1024 + f*10),
							Mode:        0644,
							Mtime:       time.Now().Unix(),
							BlobHash:    fmt.Sprintf("blob-w%d-f%d", workerID, f),
							Ref:         "main",
							Source:      repoURL,
						},
					})
					if err != nil {
						errorCount.Add(1)
					} else {
						successCount.Add(1)
					}
				}
			}(w)
		}

		wg.Wait()
		elapsed := time.Since(startTime)

		totalFiles := int64(numWorkers * filesPerWorker)
		successRate := float64(successCount.Load()) / float64(totalFiles) * 100

		t.Logf("High concurrency ingestion results:")
		t.Logf("  Workers: %d", numWorkers)
		t.Logf("  Files per worker: %d", filesPerWorker)
		t.Logf("  Total attempted: %d", totalFiles)
		t.Logf("  Successful: %d (%.1f%%)", successCount.Load(), successRate)
		t.Logf("  Failed: %d", errorCount.Load())
		t.Logf("  Duration: %v", elapsed)
		t.Logf("  Throughput: %.0f files/sec", float64(successCount.Load())/elapsed.Seconds())

		if successRate < 95.0 {
			t.Errorf("Success rate too low: %.1f%% (expected >= 95%%)", successRate)
		}
	})

	// Build indexes and verify
	env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})

	// Reset counters for lookup test
	successCount.Store(0)
	errorCount.Store(0)
	startTime = time.Now()

	t.Run("ConcurrentLookup", func(t *testing.T) {
		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				for f := 0; f < filesPerWorker; f++ {
					path := fmt.Sprintf("%s/worker%03d/subdir/file_%05d.txt", displayPath, workerID, f)
					resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
					if err != nil {
						errorCount.Add(1)
					} else if resp.Found {
						successCount.Add(1)
					} else {
						errorCount.Add(1)
					}
				}
			}(w)
		}

		wg.Wait()
		elapsed := time.Since(startTime)

		t.Logf("High concurrency lookup results:")
		t.Logf("  Successful: %d", successCount.Load())
		t.Logf("  Failed: %d", errorCount.Load())
		t.Logf("  Duration: %v", elapsed)
		t.Logf("  Throughput: %.0f lookups/sec", float64(successCount.Load())/elapsed.Seconds())
	})
}

// TestBatchIngestionPerformance tests batch ingestion performance with reasonable batch sizes.
// Reduced from [10, 50, 100, 500, 1000] to [10, 50, 100] for faster CI execution.
func TestBatchIngestionPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	env := newServerTestEnv(t, "batch-perf-node", 19510)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "batch-perf-storage"
	displayPath := "github_com/test/batch-perf"
	repoURL := "https://github.com/test/batch-perf.git"

	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	batchSizes := []int{10, 50, 100} // Reduced from [10, 50, 100, 500, 1000]

	for _, batchSize := range batchSizes {
		t.Run(fmt.Sprintf("BatchSize%d", batchSize), func(t *testing.T) {
			files := make([]*pb.FileMetadata, batchSize)
			for i := 0; i < batchSize; i++ {
				files[i] = &pb.FileMetadata{
					Path:        fmt.Sprintf("batch%d/file_%05d.txt", batchSize, i),
					StorageId:   storageID,
					DisplayPath: displayPath,
					Size:        1024,
					Mode:        0644,
					Mtime:       time.Now().Unix(),
					BlobHash:    fmt.Sprintf("blob-%d-%d", batchSize, i),
					Ref:         "main",
					Source:      repoURL,
				}
			}

			start := time.Now()
			resp, err := env.client.IngestFileBatch(ctx, &pb.IngestFileBatchRequest{
				StorageId:   storageID,
				DisplayPath: displayPath,
				Source:      repoURL,
				Ref:         "main",
				Files:       files,
			})
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("IngestFileBatch failed: %v", err)
			}
			if !resp.Success {
				t.Fatalf("IngestFileBatch returned failure: %s", resp.ErrorMessage)
			}

			filesPerSec := float64(batchSize) / elapsed.Seconds()
			t.Logf("Batch size %d: %v (%.0f files/sec)", batchSize, elapsed, filesPerSec)
		})
	}
}

// TestMixedWorkload tests realistic mixed read/write workload.
// Reduced from 500 initial files to 100 for faster CI execution.
func TestMixedWorkload(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping mixed workload test in short mode")
	}

	env := newServerTestEnv(t, "mixed-workload-node", 19520)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "mixed-workload-storage"
	displayPath := "github_com/test/mixed-workload"
	repoURL := "https://github.com/test/mixed-workload.git"

	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	// Pre-populate with some files
	const initialFiles = 100 // Reduced from 500
	files := make([]*pb.FileMetadata, initialFiles)
	for i := 0; i < initialFiles; i++ {
		files[i] = &pb.FileMetadata{
			Path:        fmt.Sprintf("initial/file_%04d.txt", i),
			StorageId:   storageID,
			DisplayPath: displayPath,
			Size:        1024,
			Mode:        0644,
			Mtime:       time.Now().Unix(),
			BlobHash:    fmt.Sprintf("blob-init-%d", i),
			Ref:         "main",
			Source:      repoURL,
		}
	}
	env.client.IngestFileBatch(ctx, &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
		Ref:         "main",
		Files:       files,
	})
	env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})

	const numWorkers = 20
	const opsPerWorker = 100

	var wg sync.WaitGroup
	var writeCount atomic.Int64
	var readCount atomic.Int64
	var errorCount atomic.Int64

	t.Run("MixedOps", func(t *testing.T) {
		start := time.Now()

		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				for op := 0; op < opsPerWorker; op++ {
					// 70% reads, 30% writes
					if op%10 < 7 {
						// Read operation
						fileIdx := (workerID*opsPerWorker + op) % initialFiles
						path := fmt.Sprintf("%s/initial/file_%04d.txt", displayPath, fileIdx)
						resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
						if err != nil || !resp.Found {
							errorCount.Add(1)
						} else {
							readCount.Add(1)
						}
					} else {
						// Write operation
						_, err := env.client.IngestFile(ctx, &pb.IngestFileRequest{
							Metadata: &pb.FileMetadata{
								Path:        fmt.Sprintf("worker%d/file_%d.txt", workerID, op),
								StorageId:   storageID,
								DisplayPath: displayPath,
								Size:        1024,
								Mode:        0644,
								Mtime:       time.Now().Unix(),
								BlobHash:    fmt.Sprintf("blob-w%d-o%d", workerID, op),
								Ref:         "main",
								Source:      repoURL,
							},
						})
						if err != nil {
							errorCount.Add(1)
						} else {
							writeCount.Add(1)
						}
					}
				}
			}(w)
		}

		wg.Wait()
		elapsed := time.Since(start)

		totalOps := readCount.Load() + writeCount.Load()
		t.Logf("Mixed workload results:")
		t.Logf("  Reads: %d", readCount.Load())
		t.Logf("  Writes: %d", writeCount.Load())
		t.Logf("  Errors: %d", errorCount.Load())
		t.Logf("  Duration: %v", elapsed)
		t.Logf("  Throughput: %.0f ops/sec", float64(totalOps)/elapsed.Seconds())

		// Error rate should be low
		totalAttempts := numWorkers * opsPerWorker
		errorRate := float64(errorCount.Load()) / float64(totalAttempts) * 100
		if errorRate > 5.0 {
			t.Errorf("Error rate too high: %.1f%% (expected <= 5%%)", errorRate)
		}
	})
}

// ============================================================================
// Cluster Stress Tests
// ============================================================================

// TestClusterConcurrentOperations tests cluster under moderate concurrent load.
// Timeout reduced from 180s to 60s for faster CI execution.
func TestClusterConcurrentOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster stress test in short mode")
	}

	basePort := 19550
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create 3 backend nodes
	type nodeEntry struct {
		server     *server.Server
		grpcServer *grpc.Server
		listener   net.Listener
		client     pb.MonoFSClient
		conn       *grpc.ClientConn
	}

	nodes := make([]*nodeEntry, 3)
	for i := 0; i < 3; i++ {
		port := basePort + 1 + i
		dbPath := filepath.Join(tmpDir, fmt.Sprintf("node%d", i+1), "db")
		gitCache := filepath.Join(tmpDir, fmt.Sprintf("node%d", i+1), "git")
		os.MkdirAll(dbPath, 0755)
		os.MkdirAll(gitCache, 0755)

		srv, err := server.NewServer(
			fmt.Sprintf("stress-node-%d", i+1),
			fmt.Sprintf("localhost:%d", port),
			dbPath, gitCache, logger,
		)
		if err != nil {
			t.Fatalf("Failed to create node %d: %v", i+1, err)
		}

		lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
		if err != nil {
			srv.Close()
			t.Fatalf("Failed to listen on port %d: %v", port, err)
		}

		grpcServer := grpc.NewServer()
		pb.RegisterMonoFSServer(grpcServer, srv)
		go grpcServer.Serve(lis)

		conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", port),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			grpcServer.Stop()
			lis.Close()
			srv.Close()
			t.Fatalf("Failed to connect to node %d: %v", i+1, err)
		}

		nodes[i] = &nodeEntry{
			server:     srv,
			grpcServer: grpcServer,
			listener:   lis,
			client:     pb.NewMonoFSClient(conn),
			conn:       conn,
		}
	}

	defer func() {
		for _, n := range nodes {
			if n.conn != nil {
				n.conn.Close()
			}
			if n.grpcServer != nil {
				n.grpcServer.Stop()
			}
			if n.listener != nil {
				n.listener.Close()
			}
			if n.server != nil {
				n.server.Close()
			}
		}
	}()

	// Create router
	cfg := router.DefaultRouterConfig()
	cfg.HealthCheckInterval = 500 * time.Millisecond
	r := router.NewRouter(cfg, logger)

	for i := 0; i < 3; i++ {
		r.RegisterNodeStatic(
			fmt.Sprintf("stress-node-%d", i+1),
			fmt.Sprintf("localhost:%d", basePort+1+i),
			100,
		)
	}

	r.StartHealthCheck()
	defer r.Close()

	// Start router gRPC
	routerLis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", basePort))
	if err != nil {
		t.Fatalf("Failed to listen for router: %v", err)
	}
	defer routerLis.Close()

	routerGRPC := grpc.NewServer()
	pb.RegisterMonoFSRouterServer(routerGRPC, r)
	go routerGRPC.Serve(routerLis)
	defer routerGRPC.Stop()

	time.Sleep(1 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Create sharded client
	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", basePort),
		ClientID:        "stress-test-client",
		RefreshInterval: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	// Register repository on all nodes
	storageID := "cluster-stress-storage"
	displayPath := "github_com/test/cluster-stress"
	repoURL := "https://github.com/test/cluster-stress.git"

	for _, n := range nodes {
		n.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      repoURL,
		})
	}

	// Ingest files to all nodes
	const filesPerNode = 100
	for nodeIdx, n := range nodes {
		for f := 0; f < filesPerNode; f++ {
			n.client.IngestFile(ctx, &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        fmt.Sprintf("node%d/file_%04d.txt", nodeIdx+1, f),
					StorageId:   storageID,
					DisplayPath: displayPath,
					Size:        1024,
					Mode:        0644,
					Mtime:       time.Now().Unix(),
					BlobHash:    fmt.Sprintf("blob-n%d-f%d", nodeIdx+1, f),
					Ref:         "main",
					Source:      repoURL,
				},
			})
		}
		n.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})
	}

	const numClients = 10
	const opsPerClient = 50

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	t.Run("ConcurrentClusterAccess", func(t *testing.T) {
		start := time.Now()

		for c := 0; c < numClients; c++ {
			wg.Add(1)
			go func(clientID int) {
				defer wg.Done()

				for op := 0; op < opsPerClient; op++ {
					// Access files from different nodes
					nodeIdx := (clientID + op) % 3
					fileIdx := (clientID*opsPerClient + op) % filesPerNode
					path := fmt.Sprintf("%s/node%d/file_%04d.txt", displayPath, nodeIdx+1, fileIdx)

					resp, err := sc.GetAttr(ctx, path)
					if err != nil {
						errorCount.Add(1)
					} else if resp.Found {
						successCount.Add(1)
					} else {
						errorCount.Add(1)
					}
				}
			}(c)
		}

		wg.Wait()
		elapsed := time.Since(start)

		t.Logf("Cluster concurrent access results:")
		t.Logf("  Clients: %d", numClients)
		t.Logf("  Ops per client: %d", opsPerClient)
		t.Logf("  Successful: %d", successCount.Load())
		t.Logf("  Failed: %d", errorCount.Load())
		t.Logf("  Duration: %v", elapsed)
		t.Logf("  Throughput: %.0f ops/sec", float64(successCount.Load())/elapsed.Seconds())
	})
}

// TestRapidClusterUpdates tests cluster behavior with rapid topology changes.
// Reduced from 100 nodes to 50 for faster CI execution.
func TestRapidClusterUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping rapid cluster updates test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := router.DefaultRouterConfig()
	cfg.HealthCheckInterval = 100 * time.Millisecond
	r := router.NewRouter(cfg, logger)
	defer r.Close()

	// Rapidly add and remove nodes (reduced from 100 to 50)
	t.Run("RapidNodeUpdates", func(t *testing.T) {
		start := time.Now()

		for i := 0; i < 50; i++ { // Reduced from 100
			nodeID := fmt.Sprintf("rapid-node-%d", i)
			r.RegisterNodeStatic(nodeID, fmt.Sprintf("localhost:%d", 19600+i), 100)
		}

		elapsed := time.Since(start)
		t.Logf("Registered 50 nodes in %v", elapsed)
	})
}
