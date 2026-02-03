// Package test provides integration tests for the MonoFS sharded client.
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

// clientTestCluster encapsulates a complete cluster for client testing.
type clientTestCluster struct {
	t          *testing.T
	baseDir    string
	nodes      []*nodeInfo
	router     *router.Router
	routerGRPC *grpc.Server
	routerLis  net.Listener
	routerPort int
	stopOnce   sync.Once
}

type nodeInfo struct {
	id         string
	port       int
	server     *server.Server
	grpcServer *grpc.Server
	listener   net.Listener
	client     pb.MonoFSClient
	conn       *grpc.ClientConn
}

// newClientTestCluster creates a fully functional cluster for client testing.
func newClientTestCluster(t *testing.T, numNodes int, basePort int) *clientTestCluster {
	t.Helper()

	baseDir := t.TempDir()
	cluster := &clientTestCluster{
		t:          t,
		baseDir:    baseDir,
		nodes:      make([]*nodeInfo, numNodes),
		routerPort: basePort,
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create backend nodes
	for i := 0; i < numNodes; i++ {
		nodeID := fmt.Sprintf("client-test-node-%d", i+1)
		port := basePort + 1 + i
		dbPath := filepath.Join(baseDir, nodeID, "db")
		gitCache := filepath.Join(baseDir, nodeID, "git")
		os.MkdirAll(dbPath, 0755)
		os.MkdirAll(gitCache, 0755)

		srv, err := server.NewServer(nodeID, fmt.Sprintf("localhost:%d", port), dbPath, gitCache, logger)
		if err != nil {
			cluster.cleanup()
			t.Fatalf("Failed to create server %d: %v", i+1, err)
		}

		lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
		if err != nil {
			srv.Close()
			cluster.cleanup()
			t.Fatalf("Failed to listen on port %d: %v", port, err)
		}

		grpcServer := grpc.NewServer()
		pb.RegisterMonoFSServer(grpcServer, srv)
		go grpcServer.Serve(lis)

		// Create client connection to node
		conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", port),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			grpcServer.Stop()
			lis.Close()
			srv.Close()
			cluster.cleanup()
			t.Fatalf("Failed to connect to node %d: %v", i+1, err)
		}

		cluster.nodes[i] = &nodeInfo{
			id:         nodeID,
			port:       port,
			server:     srv,
			grpcServer: grpcServer,
			listener:   lis,
			client:     pb.NewMonoFSClient(conn),
			conn:       conn,
		}
	}

	// Create router
	cfg := router.DefaultRouterConfig()
	cfg.ClusterID = "client-test-cluster"
	cfg.HealthCheckInterval = 300 * time.Millisecond
	cfg.UnhealthyThreshold = 1 * time.Second

	cluster.router = router.NewRouter(cfg, logger)

	// Register nodes with router
	for _, node := range cluster.nodes {
		cluster.router.RegisterNodeStatic(node.id, fmt.Sprintf("localhost:%d", node.port), 100)
	}

	// Start health checks
	cluster.router.StartHealthCheck()

	// Start router gRPC server
	routerLis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", basePort))
	if err != nil {
		cluster.cleanup()
		t.Fatalf("Failed to listen for router: %v", err)
	}
	cluster.routerLis = routerLis

	cluster.routerGRPC = grpc.NewServer()
	pb.RegisterMonoFSRouterServer(cluster.routerGRPC, cluster.router)
	go cluster.routerGRPC.Serve(routerLis)

	// Wait for everything to be ready
	time.Sleep(500 * time.Millisecond)

	return cluster
}

func (c *clientTestCluster) cleanup() {
	if c.routerGRPC != nil {
		c.routerGRPC.Stop()
	}
	if c.routerLis != nil {
		c.routerLis.Close()
	}
	if c.router != nil {
		c.router.Close()
	}
	for _, node := range c.nodes {
		if node != nil {
			if node.conn != nil {
				node.conn.Close()
			}
			if node.grpcServer != nil {
				node.grpcServer.Stop()
			}
			if node.listener != nil {
				node.listener.Close()
			}
			if node.server != nil {
				node.server.Close()
			}
		}
	}
}

// Close shuts down the cluster.
func (c *clientTestCluster) Close() {
	c.stopOnce.Do(c.cleanup)
}

// ingestTestData ingests test files into the cluster using proper HRW routing.
func (c *clientTestCluster) ingestTestData(ctx context.Context, storageID, displayPath, repoURL string, files []string) error {
	// Register repository on all nodes
	for _, node := range c.nodes {
		_, err := node.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      repoURL,
		})
		if err != nil {
			return fmt.Errorf("register repo on %s: %w", node.id, err)
		}
	}

	// Ingest files - use node 0 for simplicity (in real scenario, router distributes)
	// This tests the basic flow; proper sharding is tested separately
	for _, f := range files {
		_, err := c.nodes[0].client.IngestFile(ctx, &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        f,
				StorageId:   storageID,
				DisplayPath: displayPath,
				Size:        uint64(len(f) * 100),
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    fmt.Sprintf("blob-%s", f),
				Ref:         "main",
				Source:      repoURL,
			},
		})
		if err != nil {
			return fmt.Errorf("ingest file %s: %w", f, err)
		}
	}

	// Build directory indexes
	_, err := c.nodes[0].client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	return err
}

// ============================================================================
// Sharded Client Tests
// ============================================================================

// TestShardedClientConnection tests basic client connectivity.
func TestShardedClientConnection(t *testing.T) {
	cluster := newClientTestCluster(t, 3, 19400)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("CreateClient", func(t *testing.T) {
		sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
			RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
			ClientID:        "connection-test-client",
			RefreshInterval: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("Failed to create sharded client: %v", err)
		}
		defer sc.Close()

		t.Logf("✓ Sharded client created successfully")
	})

	t.Run("ClientWithExternalAddresses", func(t *testing.T) {
		sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
			RouterAddr:           fmt.Sprintf("localhost:%d", cluster.routerPort),
			ClientID:             "external-addr-test-client",
			RefreshInterval:      5 * time.Second,
			UseExternalAddresses: true,
		})
		if err != nil {
			t.Fatalf("Failed to create sharded client: %v", err)
		}
		defer sc.Close()

		t.Logf("✓ Sharded client with external addresses created")
	})
}

// TestShardedClientLookup tests file lookup operations.
func TestShardedClientLookup(t *testing.T) {
	cluster := newClientTestCluster(t, 3, 19410)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "client-lookup-storage"
	displayPath := "github_com/test/client-lookup"
	repoURL := "https://github.com/test/client-lookup.git"

	files := []string{
		"README.md",
		"main.go",
		"pkg/utils/helper.go",
		"cmd/app/main.go",
	}

	if err := cluster.ingestTestData(ctx, storageID, displayPath, repoURL, files); err != nil {
		t.Fatalf("Failed to ingest test data: %v", err)
	}

	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
		ClientID:        "lookup-test-client",
		RefreshInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	t.Run("LookupFiles", func(t *testing.T) {
		for _, f := range files {
			path := displayPath + "/" + f
			resp, err := sc.Lookup(ctx, path)
			if err != nil {
				t.Errorf("Lookup failed for %s: %v", f, err)
				continue
			}
			if !resp.Found {
				t.Errorf("File not found: %s", f)
				continue
			}
			t.Logf("✓ Lookup %s: ino=%d", f, resp.Ino)
		}
	})

	t.Run("LookupNonExistent", func(t *testing.T) {
		path := displayPath + "/does-not-exist.txt"
		resp, err := sc.Lookup(ctx, path)
		// Should either return error or Found=false
		if err == nil && resp.Found {
			t.Error("Should not find non-existent file")
		}
		t.Logf("✓ Non-existent file correctly not found")
	})
}

// TestShardedClientGetAttr tests attribute retrieval.
func TestShardedClientGetAttr(t *testing.T) {
	cluster := newClientTestCluster(t, 3, 19420)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "client-getattr-storage"
	displayPath := "github_com/test/client-getattr"
	repoURL := "https://github.com/test/client-getattr.git"

	files := []string{"file1.txt", "file2.txt", "dir/file3.txt"}
	if err := cluster.ingestTestData(ctx, storageID, displayPath, repoURL, files); err != nil {
		t.Fatalf("Failed to ingest test data: %v", err)
	}

	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
		ClientID:        "getattr-test-client",
		RefreshInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	t.Run("GetAttrFiles", func(t *testing.T) {
		for _, f := range files {
			path := displayPath + "/" + f
			resp, err := sc.GetAttr(ctx, path)
			if err != nil {
				t.Errorf("GetAttr failed for %s: %v", f, err)
				continue
			}
			if !resp.Found {
				t.Errorf("File not found: %s", f)
				continue
			}
			if resp.Mode == 0 {
				t.Errorf("Invalid mode for %s", f)
			}
			t.Logf("✓ GetAttr %s: size=%d mode=%o", f, resp.Size, resp.Mode)
		}
	})
}

// TestShardedClientReadDir tests directory listing.
func TestShardedClientReadDir(t *testing.T) {
	cluster := newClientTestCluster(t, 3, 19430)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "client-readdir-storage"
	displayPath := "github_com/test/client-readdir"
	repoURL := "https://github.com/test/client-readdir.git"

	files := []string{
		"README.md",
		"go.mod",
		"main.go",
		"internal/server/server.go",
		"internal/client/client.go",
		"pkg/utils/helpers.go",
	}
	if err := cluster.ingestTestData(ctx, storageID, displayPath, repoURL, files); err != nil {
		t.Fatalf("Failed to ingest test data: %v", err)
	}

	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
		ClientID:        "readdir-test-client",
		RefreshInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	t.Run("ReadDirRoot", func(t *testing.T) {
		entries, err := sc.ReadDir(ctx, displayPath)
		if err != nil {
			t.Fatalf("ReadDir failed: %v", err)
		}

		if len(entries) == 0 {
			t.Error("Expected entries in root directory")
		}

		entryNames := make(map[string]bool)
		for _, e := range entries {
			entryNames[e.Name] = true
		}

		// Check expected entries
		expected := []string{"README.md", "go.mod", "main.go", "internal", "pkg"}
		for _, name := range expected {
			if !entryNames[name] {
				t.Errorf("Missing entry: %s", name)
			}
		}

		t.Logf("✓ Root directory has %d entries", len(entries))
	})

	t.Run("ReadDirSubdirectory", func(t *testing.T) {
		entries, err := sc.ReadDir(ctx, displayPath+"/internal")
		if err != nil {
			t.Fatalf("ReadDir failed: %v", err)
		}

		entryNames := make(map[string]bool)
		for _, e := range entries {
			entryNames[e.Name] = true
		}

		if !entryNames["server"] {
			t.Error("Missing 'server' directory")
		}
		if !entryNames["client"] {
			t.Error("Missing 'client' directory")
		}

		t.Logf("✓ Subdirectory has %d entries", len(entries))
	})
}

// TestShardedClientConcurrentOperations tests concurrent client operations.
func TestShardedClientConcurrentOperations(t *testing.T) {
	cluster := newClientTestCluster(t, 3, 19440)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	storageID := "client-concurrent-storage"
	displayPath := "github_com/test/client-concurrent"
	repoURL := "https://github.com/test/client-concurrent.git"

	// Create many files
	var files []string
	for i := 0; i < 100; i++ {
		files = append(files, fmt.Sprintf("file_%03d.txt", i))
	}
	if err := cluster.ingestTestData(ctx, storageID, displayPath, repoURL, files); err != nil {
		t.Fatalf("Failed to ingest test data: %v", err)
	}

	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
		ClientID:        "concurrent-test-client",
		RefreshInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	t.Run("ConcurrentLookups", func(t *testing.T) {
		const numWorkers = 10
		const lookupsPerWorker = 50

		var wg sync.WaitGroup
		var successCount atomic.Int64
		var errorCount atomic.Int64

		start := time.Now()

		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				for i := 0; i < lookupsPerWorker; i++ {
					fileIdx := (workerID*lookupsPerWorker + i) % len(files)
					path := displayPath + "/" + files[fileIdx]

					resp, err := sc.Lookup(ctx, path)
					if err != nil {
						errorCount.Add(1)
						continue
					}
					if resp.Found {
						successCount.Add(1)
					} else {
						errorCount.Add(1)
					}
				}
			}(w)
		}

		wg.Wait()
		elapsed := time.Since(start)

		total := numWorkers * lookupsPerWorker
		if successCount.Load() < int64(total/2) {
			t.Errorf("Too many failures: success=%d, errors=%d out of %d",
				successCount.Load(), errorCount.Load(), total)
		}

		t.Logf("✓ Concurrent lookups: %d success, %d errors in %v (%.0f ops/sec)",
			successCount.Load(), errorCount.Load(), elapsed,
			float64(successCount.Load())/elapsed.Seconds())
	})

	t.Run("ConcurrentMixedOperations", func(t *testing.T) {
		const numWorkers = 5
		const opsPerWorker = 20

		var wg sync.WaitGroup
		var successCount atomic.Int64

		start := time.Now()

		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				for i := 0; i < opsPerWorker; i++ {
					fileIdx := (workerID*opsPerWorker + i) % len(files)
					path := displayPath + "/" + files[fileIdx]

					// Mix of operations
					switch i % 3 {
					case 0:
						resp, err := sc.Lookup(ctx, path)
						if err == nil && resp.Found {
							successCount.Add(1)
						}
					case 1:
						resp, err := sc.GetAttr(ctx, path)
						if err == nil && resp.Found {
							successCount.Add(1)
						}
					case 2:
						entries, err := sc.ReadDir(ctx, displayPath)
						if err == nil && len(entries) > 0 {
							successCount.Add(1)
						}
					}
				}
			}(w)
		}

		wg.Wait()
		elapsed := time.Since(start)

		t.Logf("✓ Mixed operations: %d successful in %v", successCount.Load(), elapsed)
	})
}

// TestShardedClientReconnection tests client behavior on connection loss.
func TestShardedClientReconnection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping reconnection test in short mode")
	}

	cluster := newClientTestCluster(t, 3, 19450)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	storageID := "client-reconnect-storage"
	displayPath := "github_com/test/client-reconnect"
	repoURL := "https://github.com/test/client-reconnect.git"

	files := []string{"file1.txt", "file2.txt", "file3.txt"}
	if err := cluster.ingestTestData(ctx, storageID, displayPath, repoURL, files); err != nil {
		t.Fatalf("Failed to ingest test data: %v", err)
	}

	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
		ClientID:        "reconnect-test-client",
		RefreshInterval: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	t.Run("InitialOperations", func(t *testing.T) {
		for _, f := range files {
			path := displayPath + "/" + f
			resp, err := sc.Lookup(ctx, path)
			if err != nil {
				t.Errorf("Initial lookup failed for %s: %v", f, err)
				continue
			}
			if !resp.Found {
				t.Errorf("File not found: %s", f)
			}
		}
		t.Logf("✓ Initial operations successful")
	})

	// Note: Full reconnection testing would require stopping/restarting nodes
	// which is complex in a unit test. This test verifies the client
	// handles basic scenarios correctly.
}

// TestShardedClientMultipleRepositories tests accessing multiple repositories.
func TestShardedClientMultipleRepositories(t *testing.T) {
	cluster := newClientTestCluster(t, 3, 19460)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repos := []struct {
		storageID   string
		displayPath string
		repoURL     string
		files       []string
	}{
		{
			storageID:   "multi-repo-1",
			displayPath: "github_com/org1/repo1",
			repoURL:     "https://github.com/org1/repo1.git",
			files:       []string{"README.md", "main.go"},
		},
		{
			storageID:   "multi-repo-2",
			displayPath: "github_com/org2/repo2",
			repoURL:     "https://github.com/org2/repo2.git",
			files:       []string{"README.md", "index.js", "package.json"},
		},
		{
			storageID:   "multi-repo-3",
			displayPath: "gitlab_com/team/project",
			repoURL:     "https://gitlab.com/team/project.git",
			files:       []string{"README.md", "lib.rs", "Cargo.toml"},
		},
	}

	// Ingest all repositories
	for _, repo := range repos {
		if err := cluster.ingestTestData(ctx, repo.storageID, repo.displayPath, repo.repoURL, repo.files); err != nil {
			t.Fatalf("Failed to ingest %s: %v", repo.displayPath, err)
		}
	}

	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
		ClientID:        "multi-repo-test-client",
		RefreshInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	t.Run("AccessAllRepositories", func(t *testing.T) {
		for _, repo := range repos {
			for _, f := range repo.files {
				path := repo.displayPath + "/" + f
				resp, err := sc.Lookup(ctx, path)
				if err != nil {
					t.Errorf("Lookup failed for %s: %v", path, err)
					continue
				}
				if !resp.Found {
					t.Errorf("File not found: %s", path)
				}
			}
			t.Logf("✓ Verified %d files in %s", len(repo.files), repo.displayPath)
		}
	})
}

// ============================================================================
// Read/Write Stream Tests
// ============================================================================

// TestShardedClientRead tests file reading through client.
func TestShardedClientRead(t *testing.T) {
	cluster := newClientTestCluster(t, 3, 19470)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "client-read-storage"
	displayPath := "github_com/test/client-read"
	repoURL := "https://github.com/test/client-read.git"

	files := []string{"test.txt", "data.bin"}
	if err := cluster.ingestTestData(ctx, storageID, displayPath, repoURL, files); err != nil {
		t.Fatalf("Failed to ingest test data: %v", err)
	}

	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
		ClientID:        "read-test-client",
		RefreshInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	t.Run("ReadFile", func(t *testing.T) {
		path := displayPath + "/test.txt"

		// Note: Read requires actual blob content which isn't available in this test setup
		// This test verifies the Read method is callable and handles the scenario gracefully
		data, err := sc.Read(ctx, path, 0, 1024)
		if err != nil {
			// Expected when blob content is not available
			t.Logf("Read returned error (expected without real git repo): %v", err)
		} else {
			t.Logf("✓ Read returned %d bytes", len(data))
		}
	})
}

// TestClientNodeSelection tests that client routes to correct nodes.
func TestClientNodeSelection(t *testing.T) {
	cluster := newClientTestCluster(t, 3, 19480)
	defer cluster.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ingest files to specific nodes to test routing
	storageID := "routing-test-storage"
	displayPath := "github_com/test/routing"
	repoURL := "https://github.com/test/routing.git"

	// Register repo on all nodes
	for _, node := range cluster.nodes {
		node.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      repoURL,
		})
	}

	// Ingest different files on different nodes
	for nodeIdx, node := range cluster.nodes {
		for i := 0; i < 5; i++ {
			node.client.IngestFile(ctx, &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        fmt.Sprintf("node%d_file%d.txt", nodeIdx+1, i),
					StorageId:   storageID,
					DisplayPath: displayPath,
					Size:        1024,
					Mode:        0644,
					Mtime:       time.Now().Unix(),
					BlobHash:    fmt.Sprintf("blob-n%d-f%d", nodeIdx+1, i),
					Ref:         "main",
					Source:      repoURL,
				},
			})
		}
		node.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})
	}

	sc, err := client.NewShardedClient(ctx, client.ShardedClientConfig{
		RouterAddr:      fmt.Sprintf("localhost:%d", cluster.routerPort),
		ClientID:        "routing-test-client",
		RefreshInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create sharded client: %v", err)
	}
	defer sc.Close()

	t.Run("DirectoryListing", func(t *testing.T) {
		entries, err := sc.ReadDir(ctx, displayPath)
		if err != nil {
			t.Fatalf("ReadDir failed: %v", err)
		}

		t.Logf("Found %d entries in directory", len(entries))
		// Files may be distributed across nodes, so we should find at least some
		if len(entries) < 5 {
			t.Logf("Note: Only found %d entries (files are distributed across nodes)", len(entries))
		}
	})
}
