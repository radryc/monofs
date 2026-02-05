// Package test provides proper integration tests for MonoFS server components.
// These tests verify the server functionality with actual NutsDB storage,
// gRPC communication, and proper file operations.
package test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// serverTestEnv encapsulates a test server environment.
type serverTestEnv struct {
	t          *testing.T
	server     *server.Server
	grpcServer *grpc.Server
	listener   net.Listener
	client     pb.MonoFSClient
	conn       *grpc.ClientConn
	baseDir    string
	stopOnce   sync.Once
}

// newServerTestEnv creates a new server test environment with unique port.
func newServerTestEnv(t *testing.T, nodeID string, port int) *serverTestEnv {
	t.Helper()

	baseDir := t.TempDir()
	dbPath := filepath.Join(baseDir, "db")
	gitCache := filepath.Join(baseDir, "git")

	if err := os.MkdirAll(dbPath, 0755); err != nil {
		t.Fatalf("Failed to create db dir: %v", err)
	}
	if err := os.MkdirAll(gitCache, 0755); err != nil {
		t.Fatalf("Failed to create git cache dir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv, err := server.NewServer(nodeID, fmt.Sprintf("localhost:%d", port), dbPath, gitCache, logger)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		srv.Close()
		t.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterMonoFSServer(grpcServer, srv)

	go grpcServer.Serve(listener)

	// Wait for server to be ready
	time.Sleep(100 * time.Millisecond)

	conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		grpcServer.Stop()
		srv.Close()
		t.Fatalf("Failed to connect: %v", err)
	}

	return &serverTestEnv{
		t:          t,
		server:     srv,
		grpcServer: grpcServer,
		listener:   listener,
		client:     pb.NewMonoFSClient(conn),
		conn:       conn,
		baseDir:    baseDir,
	}
}

// Close shuts down the test environment.
func (env *serverTestEnv) Close() {
	env.stopOnce.Do(func() {
		if env.conn != nil {
			env.conn.Close()
		}
		if env.grpcServer != nil {
			env.grpcServer.Stop()
		}
		if env.listener != nil {
			env.listener.Close()
		}
		if env.server != nil {
			env.server.Close()
		}
	})
}

// ============================================================================
// Server Core Operations Tests
// ============================================================================

// TestServerIngestAndLookup tests basic file ingestion and lookup.
func TestServerIngestAndLookup(t *testing.T) {
	env := newServerTestEnv(t, "test-node-1", 19100)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "test-storage-lookup"
	displayPath := "github_com/test/lookup"
	repoURL := "https://github.com/test/lookup.git"

	// Register repository
	t.Run("RegisterRepository", func(t *testing.T) {
		resp, err := env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      repoURL,
		})
		if err != nil {
			t.Fatalf("RegisterRepository failed: %v", err)
		}
		if !resp.Success {
			t.Fatalf("RegisterRepository returned failure: %s", resp.Message)
		}
		t.Logf("✓ Repository registered: %s", displayPath)
	})

	// Ingest files
	testFiles := []struct {
		path string
		size uint64
		mode uint32
	}{
		{"README.md", 1024, 0644},
		{"main.go", 2048, 0644},
		{"pkg/utils/helper.go", 512, 0644},
		{"pkg/utils/config.go", 768, 0644},
		{"cmd/app/main.go", 1536, 0644},
	}

	t.Run("IngestFiles", func(t *testing.T) {
		for _, f := range testFiles {
			resp, err := env.client.IngestFile(ctx, &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        f.path,
					StorageId:   storageID,
					DisplayPath: displayPath,
					Size:        f.size,
					Mode:        f.mode,
					Mtime:       time.Now().Unix(),
					BlobHash:    fmt.Sprintf("blob-%s", f.path),
					Ref:         "main",
					Source:      repoURL,
				},
			})
			if err != nil {
				t.Fatalf("IngestFile failed for %s: %v", f.path, err)
			}
			if !resp.Success {
				t.Fatalf("IngestFile returned failure for %s", f.path)
			}
		}
		t.Logf("✓ Ingested %d files", len(testFiles))
	})

	// Build directory indexes
	t.Run("BuildDirectoryIndexes", func(t *testing.T) {
		resp, err := env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})
		if err != nil {
			t.Fatalf("BuildDirectoryIndexes failed: %v", err)
		}
		if !resp.Success {
			t.Fatalf("BuildDirectoryIndexes returned failure: %s", resp.Message)
		}
		t.Logf("✓ Built %d directory indexes", resp.DirectoriesIndexed)
	})

	// Test Lookup operations
	t.Run("LookupFiles", func(t *testing.T) {
		for _, f := range testFiles {
			// Build full path as displayed
			fullPath := displayPath + "/" + f.path

			// Extract parent and name
			dir := filepath.Dir(fullPath)
			name := filepath.Base(fullPath)

			resp, err := env.client.Lookup(ctx, &pb.LookupRequest{
				ParentPath: dir,
				Name:       name,
			})
			if err != nil {
				t.Fatalf("Lookup failed for %s: %v", f.path, err)
			}
			if !resp.Found {
				t.Fatalf("Lookup: file not found: %s", f.path)
			}
			if resp.Size != f.size {
				t.Errorf("Lookup size mismatch for %s: got %d, want %d", f.path, resp.Size, f.size)
			}
			t.Logf("✓ Lookup %s: ino=%d size=%d", f.path, resp.Ino, resp.Size)
		}
	})

	// Test GetAttr operations
	t.Run("GetAttrFiles", func(t *testing.T) {
		for _, f := range testFiles {
			fullPath := displayPath + "/" + f.path
			resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: fullPath})
			if err != nil {
				t.Fatalf("GetAttr failed for %s: %v", f.path, err)
			}
			if !resp.Found {
				t.Fatalf("GetAttr: file not found: %s", f.path)
			}
			if resp.Size != f.size {
				t.Errorf("GetAttr size mismatch for %s: got %d, want %d", f.path, resp.Size, f.size)
			}
		}
		t.Logf("✓ GetAttr verified for %d files", len(testFiles))
	})
}

// TestServerReadDir tests directory listing operations.
func TestServerReadDir(t *testing.T) {
	env := newServerTestEnv(t, "test-node-2", 19101)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "test-storage-readdir"
	displayPath := "github_com/test/readdir"
	repoURL := "https://github.com/test/readdir.git"

	// Setup: Register and ingest files
	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	// Create a directory structure
	files := []string{
		"README.md",
		"go.mod",
		"main.go",
		"internal/server/server.go",
		"internal/server/handler.go",
		"internal/client/client.go",
		"cmd/app/main.go",
		"pkg/utils/helpers.go",
	}

	for _, f := range files {
		env.client.IngestFile(ctx, &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        f,
				StorageId:   storageID,
				DisplayPath: displayPath,
				Size:        1024,
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    fmt.Sprintf("blob-%s", f),
				Ref:         "main",
				Source:      repoURL,
			},
		})
	}

	env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})

	// Test ReadDir on root
	t.Run("ReadDirRoot", func(t *testing.T) {
		stream, err := env.client.ReadDir(ctx, &pb.ReadDirRequest{Path: displayPath})
		if err != nil {
			t.Fatalf("ReadDir failed: %v", err)
		}

		entries := make(map[string]uint32)
		for {
			entry, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("ReadDir stream error: %v", err)
			}
			entries[entry.Name] = entry.Mode
		}

		// Verify expected entries
		expected := []string{"README.md", "go.mod", "main.go", "internal", "cmd", "pkg"}
		for _, name := range expected {
			if _, ok := entries[name]; !ok {
				t.Errorf("Missing entry: %s", name)
			}
		}
		t.Logf("✓ Root directory has %d entries", len(entries))
	})

	// Test ReadDir on subdirectory
	t.Run("ReadDirSubdir", func(t *testing.T) {
		stream, err := env.client.ReadDir(ctx, &pb.ReadDirRequest{Path: displayPath + "/internal"})
		if err != nil {
			t.Fatalf("ReadDir failed: %v", err)
		}

		entries := make(map[string]bool)
		for {
			entry, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("ReadDir stream error: %v", err)
			}
			entries[entry.Name] = true
		}

		// Should have "server" and "client" subdirs
		if !entries["server"] {
			t.Error("Missing 'server' directory")
		}
		if !entries["client"] {
			t.Error("Missing 'client' directory")
		}
		t.Logf("✓ Subdirectory has %d entries", len(entries))
	})

	// Test ReadDir on nested subdirectory
	t.Run("ReadDirNestedSubdir", func(t *testing.T) {
		stream, err := env.client.ReadDir(ctx, &pb.ReadDirRequest{Path: displayPath + "/internal/server"})
		if err != nil {
			t.Fatalf("ReadDir failed: %v", err)
		}

		entries := make(map[string]bool)
		for {
			entry, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("ReadDir stream error: %v", err)
			}
			entries[entry.Name] = true
		}

		// Should have "server.go" and "handler.go"
		if !entries["server.go"] {
			t.Error("Missing 'server.go' file")
		}
		if !entries["handler.go"] {
			t.Error("Missing 'handler.go' file")
		}
		t.Logf("✓ Nested directory has %d entries", len(entries))
	})
}

// TestServerBatchIngest tests batch file ingestion.
func TestServerBatchIngest(t *testing.T) {
	env := newServerTestEnv(t, "test-node-3", 19102)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "test-storage-batch"
	displayPath := "github_com/test/batch"
	repoURL := "https://github.com/test/batch.git"

	// Register repository
	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	// Create batch of files
	const batchSize = 100
	files := make([]*pb.FileMetadata, batchSize)
	for i := 0; i < batchSize; i++ {
		files[i] = &pb.FileMetadata{
			Path:        fmt.Sprintf("file_%03d.txt", i),
			StorageId:   storageID,
			DisplayPath: displayPath,
			Size:        uint64(i * 100),
			Mode:        0644,
			Mtime:       time.Now().Unix(),
			BlobHash:    fmt.Sprintf("blob-%d", i),
			Ref:         "main",
			Source:      repoURL,
		}
	}

	t.Run("IngestBatch", func(t *testing.T) {
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
		if resp.FilesIngested != int64(batchSize) {
			t.Errorf("Expected %d files ingested, got %d", batchSize, resp.FilesIngested)
		}
		t.Logf("✓ Batch ingested %d files in %v", batchSize, elapsed)
	})

	// Build indexes and verify
	env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})

	t.Run("VerifyBatchLookup", func(t *testing.T) {
		// Verify a sample of files
		for i := 0; i < 10; i++ {
			path := displayPath + "/" + fmt.Sprintf("file_%03d.txt", i*10)
			resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
			if err != nil {
				t.Fatalf("GetAttr failed for %s: %v", path, err)
			}
			if !resp.Found {
				t.Errorf("File not found: %s", path)
			}
		}
		t.Logf("✓ Verified sample of batch-ingested files")
	})
}

// TestServerConcurrentOperations tests thread safety of server operations.
func TestServerConcurrentOperations(t *testing.T) {
	env := newServerTestEnv(t, "test-node-4", 19103)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	storageID := "test-storage-concurrent"
	displayPath := "github_com/test/concurrent"
	repoURL := "https://github.com/test/concurrent.git"

	// Register repository
	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	const numWorkers = 20
	const filesPerWorker = 50

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	t.Run("ConcurrentIngest", func(t *testing.T) {
		start := time.Now()

		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				for f := 0; f < filesPerWorker; f++ {
					_, err := env.client.IngestFile(ctx, &pb.IngestFileRequest{
						Metadata: &pb.FileMetadata{
							Path:        fmt.Sprintf("worker%d/file_%03d.txt", workerID, f),
							StorageId:   storageID,
							DisplayPath: displayPath,
							Size:        1024,
							Mode:        0644,
							Mtime:       time.Now().Unix(),
							BlobHash:    fmt.Sprintf("blob-w%d-f%d", workerID, f),
							Ref:         "main",
							Source:      repoURL,
						},
					})
					if err != nil {
						errorCount.Add(1)
						return
					}
					successCount.Add(1)
				}
			}(w)
		}

		wg.Wait()
		elapsed := time.Since(start)

		totalFiles := numWorkers * filesPerWorker
		if successCount.Load() != int64(totalFiles) {
			t.Errorf("Expected %d successes, got %d (errors: %d)",
				totalFiles, successCount.Load(), errorCount.Load())
		}
		t.Logf("✓ Concurrent ingestion: %d files in %v (%.0f files/sec)",
			successCount.Load(), elapsed, float64(successCount.Load())/elapsed.Seconds())
	})

	// Build indexes
	env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})

	// Reset counters
	successCount.Store(0)
	errorCount.Store(0)

	t.Run("ConcurrentLookup", func(t *testing.T) {
		start := time.Now()

		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				for f := 0; f < filesPerWorker; f++ {
					path := fmt.Sprintf("%s/worker%d/file_%03d.txt", displayPath, workerID, f)
					resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
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

		totalFiles := numWorkers * filesPerWorker
		if successCount.Load() != int64(totalFiles) {
			t.Errorf("Expected %d found, got %d (not found: %d)",
				totalFiles, successCount.Load(), errorCount.Load())
		}
		t.Logf("✓ Concurrent lookup: %d files in %v (%.0f lookups/sec)",
			successCount.Load(), elapsed, float64(successCount.Load())/elapsed.Seconds())
	})
}

// TestServerMultipleRepositories tests isolation between repositories.
func TestServerMultipleRepositories(t *testing.T) {
	env := newServerTestEnv(t, "test-node-5", 19104)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repos := []struct {
		storageID   string
		displayPath string
		files       []string
	}{
		{
			storageID:   "storage-repo1",
			displayPath: "github_com/org1/repo1",
			files:       []string{"README.md", "main.go", "config.yaml"},
		},
		{
			storageID:   "storage-repo2",
			displayPath: "github_com/org2/repo2",
			files:       []string{"README.md", "index.js", "package.json"},
		},
		{
			storageID:   "storage-repo3",
			displayPath: "gitlab_com/team/project",
			files:       []string{"README.md", "src/lib.rs", "Cargo.toml"},
		},
	}

	// Register and ingest files for each repo
	for _, repo := range repos {
		env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   repo.storageID,
			DisplayPath: repo.displayPath,
			Source:      fmt.Sprintf("https://%s.git", repo.displayPath),
		})

		for _, f := range repo.files {
			env.client.IngestFile(ctx, &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        f,
					StorageId:   repo.storageID,
					DisplayPath: repo.displayPath,
					Size:        1024,
					Mode:        0644,
					Mtime:       time.Now().Unix(),
					BlobHash:    fmt.Sprintf("blob-%s-%s", repo.storageID, f),
					Ref:         "main",
					Source:      fmt.Sprintf("https://%s.git", repo.displayPath),
				},
			})
		}

		env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
			StorageId: repo.storageID,
		})
	}

	// Verify files are isolated
	t.Run("VerifyIsolation", func(t *testing.T) {
		for _, repo := range repos {
			for _, f := range repo.files {
				path := repo.displayPath + "/" + f
				resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
				if err != nil {
					t.Errorf("GetAttr failed for %s: %v", path, err)
					continue
				}
				if !resp.Found {
					t.Errorf("File not found: %s", path)
				}
			}
			t.Logf("✓ Verified %d files in %s", len(repo.files), repo.displayPath)
		}
	})

	// Verify cross-repo isolation (files from repo1 should not appear in repo2's namespace)
	t.Run("VerifyCrossRepoIsolation", func(t *testing.T) {
		// Try to look up repo1's file in repo2's namespace
		resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{
			Path: repos[1].displayPath + "/main.go", // main.go only exists in repo1
		})
		if err != nil {
			// Error is acceptable for not found
			t.Logf("✓ Cross-repo lookup correctly returned error")
			return
		}
		if resp.Found {
			t.Errorf("Cross-repo isolation violated: found repo1's file in repo2's namespace")
		} else {
			t.Logf("✓ Cross-repo isolation verified")
		}
	})

	// Verify ListRepositories
	t.Run("ListRepositories", func(t *testing.T) {
		resp, err := env.client.ListRepositories(ctx, &pb.ListRepositoriesRequest{})
		if err != nil {
			t.Fatalf("ListRepositories failed: %v", err)
		}
		if len(resp.RepositoryIds) != len(repos) {
			t.Errorf("Expected %d repositories, got %d", len(repos), len(resp.RepositoryIds))
		}
		t.Logf("✓ Listed %d repositories", len(resp.RepositoryIds))
	})
}

// TestServerNodeInfo tests node information retrieval.
func TestServerNodeInfo(t *testing.T) {
	env := newServerTestEnv(t, "info-test-node", 19105)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("GetNodeInfo", func(t *testing.T) {
		resp, err := env.client.GetNodeInfo(ctx, &pb.NodeInfoRequest{})
		if err != nil {
			t.Fatalf("GetNodeInfo failed: %v", err)
		}

		if resp.NodeId != "info-test-node" {
			t.Errorf("Unexpected node ID: %s", resp.NodeId)
		}
		if resp.UptimeSeconds < 0 {
			t.Errorf("Unexpected uptime: %d", resp.UptimeSeconds)
		}

		t.Logf("✓ Node info: id=%s, uptime=%ds, files=%d",
			resp.NodeId, resp.UptimeSeconds, resp.TotalFiles)
	})
}

// TestServerOnboardingStatus tests onboarding status tracking.
func TestServerOnboardingStatus(t *testing.T) {
	env := newServerTestEnv(t, "onboard-test-node", 19106)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "test-onboarding-storage"
	displayPath := "github_com/test/onboarding"
	repoURL := "https://github.com/test/onboarding.git"

	// Register repository
	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	// Check initial onboarding status (should be false)
	t.Run("InitialStatus", func(t *testing.T) {
		resp, err := env.client.GetOnboardingStatus(ctx, &pb.OnboardingStatusRequest{
			NodeId: "onboard-test-node",
		})
		if err != nil {
			t.Fatalf("GetOnboardingStatus failed: %v", err)
		}

		// Initially no repos should be marked as onboarded
		if len(resp.Repositories) > 0 {
			status := resp.Repositories[storageID]
			if status {
				t.Error("Repository should not be onboarded initially")
			}
		}
		t.Logf("✓ Initial onboarding status verified")
	})

	// Mark repository as onboarded
	t.Run("MarkOnboarded", func(t *testing.T) {
		resp, err := env.client.MarkRepositoryOnboarded(ctx, &pb.MarkRepositoryOnboardedRequest{
			StorageId: storageID,
		})
		if err != nil {
			t.Fatalf("MarkRepositoryOnboarded failed: %v", err)
		}
		if !resp.Success {
			t.Error("MarkRepositoryOnboarded returned failure")
		}
		t.Logf("✓ Repository marked as onboarded")
	})

	// Verify onboarding status
	t.Run("VerifyOnboarded", func(t *testing.T) {
		resp, err := env.client.GetOnboardingStatus(ctx, &pb.OnboardingStatusRequest{
			NodeId: "onboard-test-node",
		})
		if err != nil {
			t.Fatalf("GetOnboardingStatus failed: %v", err)
		}

		status, exists := resp.Repositories[storageID]
		if !exists {
			t.Error("Repository not in onboarding status")
		} else if !status {
			t.Error("Repository should be onboarded")
		}
		t.Logf("✓ Onboarding status verified as complete")
	})
}

// TestServerFailoverMetadata tests failover metadata operations.
func TestServerFailoverMetadata(t *testing.T) {
	env := newServerTestEnv(t, "failover-test-node", 19107)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "test-failover-storage"
	displayPath := "github_com/test/failover"
	repoURL := "https://github.com/test/failover.git"

	// Setup: Register and ingest files
	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	for i := 0; i < 5; i++ {
		env.client.IngestFile(ctx, &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        fmt.Sprintf("file_%d.txt", i),
				StorageId:   storageID,
				DisplayPath: displayPath,
				Size:        1024,
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    fmt.Sprintf("blob-%d", i),
				Ref:         "main",
				Source:      repoURL,
			},
		})
	}

	// Get repository files
	t.Run("GetRepositoryFiles", func(t *testing.T) {
		resp, err := env.client.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
			StorageId: storageID,
		})
		if err != nil {
			t.Fatalf("GetRepositoryFiles failed: %v", err)
		}
		if len(resp.Files) != 5 {
			t.Errorf("Expected 5 files, got %d", len(resp.Files))
		}
		t.Logf("✓ Got %d repository files", len(resp.Files))
	})

	// Test ClearFailoverCache
	t.Run("ClearFailoverCache", func(t *testing.T) {
		resp, err := env.client.ClearFailoverCache(ctx, &pb.ClearFailoverCacheRequest{
			RecoveredNodeId: "failed-node-test",
		})
		if err != nil {
			t.Fatalf("ClearFailoverCache failed: %v", err)
		}
		if !resp.Success {
			t.Error("ClearFailoverCache returned failure")
		}
		t.Logf("✓ Cleared failover cache (%d entries)", resp.EntriesCleared)
	})
}
