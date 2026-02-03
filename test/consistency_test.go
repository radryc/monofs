// Package test provides data consistency and integrity tests for MonoFS.
package test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/router"
	"github.com/radryc/monofs/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ============================================================================
// Data Consistency Tests
// ============================================================================

// TestDataPersistence verifies that data survives server restart.
func TestDataPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping persistence test in short mode")
	}

	baseDir := t.TempDir()
	dbPath := filepath.Join(baseDir, "db")
	gitCache := filepath.Join(baseDir, "git")
	os.MkdirAll(dbPath, 0755)
	os.MkdirAll(gitCache, 0755)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	port := 19700

	storageID := "persistence-test-storage"
	displayPath := "github_com/test/persistence"
	repoURL := "https://github.com/test/persistence.git"

	ctx := context.Background()

	// Phase 1: Create server, ingest data, close server
	t.Run("Phase1_IngestData", func(t *testing.T) {
		srv, err := server.NewServer("persist-node", fmt.Sprintf("localhost:%d", port), dbPath, gitCache, logger)
		if err != nil {
			t.Fatalf("Failed to create server: %v", err)
		}

		lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
		if err != nil {
			srv.Close()
			t.Fatalf("Failed to listen: %v", err)
		}

		grpcServer := grpc.NewServer()
		pb.RegisterMonoFSServer(grpcServer, srv)
		go grpcServer.Serve(lis)

		time.Sleep(100 * time.Millisecond)

		conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", port),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			grpcServer.Stop()
			srv.Close()
			t.Fatalf("Failed to connect: %v", err)
		}

		client := pb.NewMonoFSClient(conn)

		// Register and ingest
		client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      repoURL,
		})

		for i := 0; i < 10; i++ {
			client.IngestFile(ctx, &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        fmt.Sprintf("file_%02d.txt", i),
					StorageId:   storageID,
					DisplayPath: displayPath,
					Size:        uint64(1000 + i*100),
					Mode:        0644,
					Mtime:       time.Now().Unix(),
					BlobHash:    fmt.Sprintf("blob-%d", i),
					Ref:         "main",
					Source:      repoURL,
				},
			})
		}

		client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})

		t.Logf("✓ Phase 1: Ingested 10 files")

		// Clean shutdown
		conn.Close()
		grpcServer.Stop()
		lis.Close()
		srv.Close()

		// Wait for clean shutdown
		time.Sleep(500 * time.Millisecond)
	})

	// Phase 2: Restart server and verify data
	t.Run("Phase2_VerifyPersistence", func(t *testing.T) {
		port2 := port + 1 // Use different port to avoid bind issues

		srv, err := server.NewServer("persist-node", fmt.Sprintf("localhost:%d", port2), dbPath, gitCache, logger)
		if err != nil {
			t.Fatalf("Failed to create server: %v", err)
		}
		defer srv.Close()

		lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port2))
		if err != nil {
			t.Fatalf("Failed to listen: %v", err)
		}
		defer lis.Close()

		grpcServer := grpc.NewServer()
		pb.RegisterMonoFSServer(grpcServer, srv)
		go grpcServer.Serve(lis)
		defer grpcServer.Stop()

		time.Sleep(100 * time.Millisecond)

		conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", port2),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			t.Fatalf("Failed to connect: %v", err)
		}
		defer conn.Close()

		client := pb.NewMonoFSClient(conn)

		// Verify files exist
		for i := 0; i < 10; i++ {
			path := fmt.Sprintf("%s/file_%02d.txt", displayPath, i)
			resp, err := client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
			if err != nil {
				t.Errorf("GetAttr failed for %s: %v", path, err)
				continue
			}
			if !resp.Found {
				t.Errorf("File not found after restart: %s", path)
			}
		}

		// Verify repository listing
		repos, err := client.ListRepositories(ctx, &pb.ListRepositoriesRequest{})
		if err != nil {
			t.Fatalf("ListRepositories failed: %v", err)
		}
		if len(repos.RepositoryIds) == 0 {
			t.Error("No repositories found after restart")
		}

		t.Logf("✓ Phase 2: All 10 files persisted across restart")
	})
}

// TestDirectoryIndexConsistency verifies directory index correctness.
func TestDirectoryIndexConsistency(t *testing.T) {
	env := newServerTestEnv(t, "dir-consistency-node", 19710)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "dir-consistency-storage"
	displayPath := "github_com/test/dir-consistency"
	repoURL := "https://github.com/test/dir-consistency.git"

	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	// Create a complex directory structure
	files := []string{
		"README.md",
		"go.mod",
		"main.go",
		"internal/server/server.go",
		"internal/server/handler.go",
		"internal/server/middleware.go",
		"internal/client/client.go",
		"internal/client/transport.go",
		"pkg/utils/strings.go",
		"pkg/utils/numbers.go",
		"pkg/config/config.go",
		"cmd/app/main.go",
		"cmd/tool/main.go",
		"docs/README.md",
		"docs/api/endpoints.md",
		"docs/api/auth.md",
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

	// Test various directory listings
	testCases := []struct {
		path     string
		expected []string
	}{
		{
			path:     displayPath,
			expected: []string{"README.md", "go.mod", "main.go", "internal", "pkg", "cmd", "docs"},
		},
		{
			path:     displayPath + "/internal",
			expected: []string{"server", "client"},
		},
		{
			path:     displayPath + "/internal/server",
			expected: []string{"server.go", "handler.go", "middleware.go"},
		},
		{
			path:     displayPath + "/pkg/utils",
			expected: []string{"strings.go", "numbers.go"},
		},
		{
			path:     displayPath + "/docs",
			expected: []string{"README.md", "api"},
		},
		{
			path:     displayPath + "/docs/api",
			expected: []string{"endpoints.md", "auth.md"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.path, func(t *testing.T) {
			stream, err := env.client.ReadDir(ctx, &pb.ReadDirRequest{Path: tc.path})
			if err != nil {
				t.Fatalf("ReadDir failed: %v", err)
			}

			var entries []string
			for {
				entry, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("ReadDir stream error: %v", err)
				}
				entries = append(entries, entry.Name)
			}

			// Sort for comparison
			sort.Strings(entries)
			sort.Strings(tc.expected)

			if len(entries) != len(tc.expected) {
				t.Errorf("Entry count mismatch for %s: got %d, want %d\nGot: %v\nWant: %v",
					tc.path, len(entries), len(tc.expected), entries, tc.expected)
				return
			}

			for i, name := range entries {
				if name != tc.expected[i] {
					t.Errorf("Entry mismatch at index %d: got %s, want %s", i, name, tc.expected[i])
				}
			}
		})
	}
}

// TestMetadataIntegrity verifies file metadata is stored and retrieved correctly.
func TestMetadataIntegrity(t *testing.T) {
	env := newServerTestEnv(t, "metadata-integrity-node", 19720)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	storageID := "metadata-integrity-storage"
	displayPath := "github_com/test/metadata"
	repoURL := "https://github.com/test/metadata.git"

	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	testFiles := []struct {
		path     string
		size     uint64
		mode     uint32
		mtime    int64
		blobHash string
	}{
		{"small.txt", 100, 0644, 1700000000, "abc123"},
		{"medium.bin", 1048576, 0755, 1700100000, "def456"},
		{"large.dat", 10485760, 0600, 1700200000, "ghi789"},
		{"script.sh", 2048, 0755, 1700300000, "jkl012"},
		{"config.yaml", 512, 0644, 1700400000, "mno345"},
	}

	// Ingest files with specific metadata
	for _, f := range testFiles {
		_, err := env.client.IngestFile(ctx, &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        f.path,
				StorageId:   storageID,
				DisplayPath: displayPath,
				Size:        f.size,
				Mode:        f.mode,
				Mtime:       f.mtime,
				BlobHash:    f.blobHash,
				Ref:         "main",
				Source:      repoURL,
			},
		})
		if err != nil {
			t.Fatalf("IngestFile failed for %s: %v", f.path, err)
		}
	}

	env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})

	// Verify metadata is correctly stored
	for _, f := range testFiles {
		t.Run(f.path, func(t *testing.T) {
			path := displayPath + "/" + f.path
			resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
			if err != nil {
				t.Fatalf("GetAttr failed: %v", err)
			}
			if !resp.Found {
				t.Fatal("File not found")
			}

			if resp.Size != f.size {
				t.Errorf("Size mismatch: got %d, want %d", resp.Size, f.size)
			}
			// Compare permission bits only (mask out file type bits)
			gotPerm := resp.Mode & 0777
			wantPerm := f.mode & 0777
			if gotPerm != wantPerm {
				t.Errorf("Mode mismatch: got %o, want %o", gotPerm, wantPerm)
			}
			if resp.Mtime != f.mtime {
				t.Errorf("Mtime mismatch: got %d, want %d", resp.Mtime, f.mtime)
			}
		})
	}
}

// TestRepositoryIsolation ensures complete isolation between repositories.
func TestRepositoryIsolation(t *testing.T) {
	env := newServerTestEnv(t, "isolation-node", 19730)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create two repositories with same file names
	repos := []struct {
		storageID   string
		displayPath string
		repoURL     string
		files       []struct {
			name string
			size uint64
		}
	}{
		{
			storageID:   "isolation-repo-1",
			displayPath: "github_com/org1/project",
			repoURL:     "https://github.com/org1/project.git",
			files: []struct {
				name string
				size uint64
			}{
				{"README.md", 1000},
				{"main.go", 2000},
				{"config.yaml", 3000},
			},
		},
		{
			storageID:   "isolation-repo-2",
			displayPath: "github_com/org2/project",
			repoURL:     "https://github.com/org2/project.git",
			files: []struct {
				name string
				size uint64
			}{
				{"README.md", 4000},   // Same name, different size
				{"main.go", 5000},     // Same name, different size
				{"config.yaml", 6000}, // Same name, different size
			},
		},
	}

	// Ingest both repositories
	for _, repo := range repos {
		env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   repo.storageID,
			DisplayPath: repo.displayPath,
			Source:      repo.repoURL,
		})

		for _, f := range repo.files {
			env.client.IngestFile(ctx, &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        f.name,
					StorageId:   repo.storageID,
					DisplayPath: repo.displayPath,
					Size:        f.size,
					Mode:        0644,
					Mtime:       time.Now().Unix(),
					BlobHash:    fmt.Sprintf("blob-%s-%s", repo.storageID, f.name),
					Ref:         "main",
					Source:      repo.repoURL,
				},
			})
		}

		env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
			StorageId: repo.storageID,
		})
	}

	// Verify each repository has correct file sizes (proves isolation)
	for _, repo := range repos {
		for _, f := range repo.files {
			path := repo.displayPath + "/" + f.name
			resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
			if err != nil {
				t.Fatalf("GetAttr failed for %s: %v", path, err)
			}
			if !resp.Found {
				t.Errorf("File not found: %s", path)
				continue
			}
			if resp.Size != f.size {
				t.Errorf("Size mismatch for %s: got %d, want %d (isolation breach!)",
					path, resp.Size, f.size)
			}
		}
	}

	t.Logf("✓ Repository isolation verified - same filenames have different sizes per repo")
}

// TestConcurrentReadWrite tests consistency under concurrent read/write operations.
func TestConcurrentReadWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping concurrent read/write test in short mode")
	}

	env := newServerTestEnv(t, "concurrent-rw-node", 19740)
	defer env.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	storageID := "concurrent-rw-storage"
	displayPath := "github_com/test/concurrent-rw"
	repoURL := "https://github.com/test/concurrent-rw.git"

	env.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      repoURL,
	})

	const numFiles = 100
	const numReaders = 10
	const numWriters = 5
	const iterations = 20

	var wg sync.WaitGroup
	errors := make(chan string, 1000)

	// Pre-populate some files
	for i := 0; i < numFiles/2; i++ {
		env.client.IngestFile(ctx, &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        fmt.Sprintf("file_%03d.txt", i),
				StorageId:   storageID,
				DisplayPath: displayPath,
				Size:        1024,
				Mode:        0644,
				Mtime:       time.Now().Unix(),
				BlobHash:    fmt.Sprintf("initial-blob-%d", i),
				Ref:         "main",
				Source:      repoURL,
			},
		})
	}
	env.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})

	// Start readers
	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				fileIdx := (readerID*iterations + i) % (numFiles / 2)
				path := fmt.Sprintf("%s/file_%03d.txt", displayPath, fileIdx)
				resp, err := env.client.GetAttr(ctx, &pb.GetAttrRequest{Path: path})
				if err != nil {
					errors <- fmt.Sprintf("Reader %d: GetAttr error: %v", readerID, err)
					continue
				}
				if !resp.Found {
					errors <- fmt.Sprintf("Reader %d: File not found: %s", readerID, path)
				}
			}
		}(r)
	}

	// Start writers
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				fileIdx := numFiles/2 + writerID*iterations + i
				_, err := env.client.IngestFile(ctx, &pb.IngestFileRequest{
					Metadata: &pb.FileMetadata{
						Path:        fmt.Sprintf("file_%03d.txt", fileIdx),
						StorageId:   storageID,
						DisplayPath: displayPath,
						Size:        uint64(2048 + writerID*100),
						Mode:        0644,
						Mtime:       time.Now().Unix(),
						BlobHash:    fmt.Sprintf("writer-%d-blob-%d", writerID, i),
						Ref:         "main",
						Source:      repoURL,
					},
				})
				if err != nil {
					errors <- fmt.Sprintf("Writer %d: IngestFile error: %v", writerID, err)
				}
			}
		}(w)
	}

	wg.Wait()
	close(errors)

	// Collect errors
	var errorList []string
	for e := range errors {
		errorList = append(errorList, e)
	}

	if len(errorList) > 0 {
		t.Errorf("Concurrent read/write errors (%d):", len(errorList))
		for i, e := range errorList {
			if i < 10 {
				t.Logf("  %s", e)
			}
		}
		if len(errorList) > 10 {
			t.Logf("  ... and %d more errors", len(errorList)-10)
		}
	} else {
		t.Logf("✓ Concurrent read/write test passed with %d readers, %d writers",
			numReaders, numWriters)
	}
}

// TestClusterDataDistribution verifies data is properly distributed across nodes.
func TestClusterDataDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster distribution test in short mode")
	}

	basePort := 19750
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create 3 nodes
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
			fmt.Sprintf("dist-node-%d", i+1),
			fmt.Sprintf("localhost:%d", port),
			dbPath, gitCache, logger,
		)
		if err != nil {
			t.Fatalf("Failed to create node %d: %v", i+1, err)
		}

		lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
		if err != nil {
			srv.Close()
			t.Fatalf("Failed to listen: %v", err)
		}

		grpcServer := grpc.NewServer()
		pb.RegisterMonoFSServer(grpcServer, srv)
		go grpcServer.Serve(lis)

		conn, err := grpc.NewClient(fmt.Sprintf("localhost:%d", port),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			grpcServer.Stop()
			lis.Close()
			srv.Close()
			t.Fatalf("Failed to connect: %v", err)
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
			n.conn.Close()
			n.grpcServer.Stop()
			n.listener.Close()
			n.server.Close()
		}
	}()

	// Create router
	cfg := router.DefaultRouterConfig()
	cfg.HealthCheckInterval = 500 * time.Millisecond
	r := router.NewRouter(cfg, logger)

	for i := 0; i < 3; i++ {
		r.RegisterNodeStatic(
			fmt.Sprintf("dist-node-%d", i+1),
			fmt.Sprintf("localhost:%d", basePort+1+i),
			100,
		)
	}

	r.StartHealthCheck()
	defer r.Close()

	time.Sleep(1 * time.Second)

	ctx := context.Background()
	storageID := "distribution-test-storage"
	displayPath := "github_com/test/distribution"
	repoURL := "https://github.com/test/distribution.git"

	// Register repo on all nodes
	for _, n := range nodes {
		n.client.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   storageID,
			DisplayPath: displayPath,
			Source:      repoURL,
		})
	}

	// Ingest 30 files, 10 per node
	filesPerNode := 10
	for nodeIdx, n := range nodes {
		for i := 0; i < filesPerNode; i++ {
			n.client.IngestFile(ctx, &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        fmt.Sprintf("node%d/file_%02d.txt", nodeIdx+1, i),
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
		n.client.BuildDirectoryIndexes(ctx, &pb.BuildDirectoryIndexesRequest{
			StorageId: storageID,
		})
	}

	// Verify each node has its files
	for nodeIdx, n := range nodes {
		resp, err := n.client.GetRepositoryFiles(ctx, &pb.GetRepositoryFilesRequest{
			StorageId: storageID,
		})
		if err != nil {
			t.Fatalf("GetRepositoryFiles failed on node %d: %v", nodeIdx+1, err)
		}
		if len(resp.Files) != filesPerNode {
			t.Errorf("Node %d: expected %d files, got %d", nodeIdx+1, filesPerNode, len(resp.Files))
		}
		t.Logf("Node %d: %d files", nodeIdx+1, len(resp.Files))
	}

	t.Logf("✓ Data correctly distributed: 10 files per node")
}
