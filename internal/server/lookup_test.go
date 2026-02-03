// Package server provides tests for Lookup and intermediate directory functionality.
package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

// TestLookupWithIntermediateDirectories tests the full Lookup flow including
// intermediate directory detection. This would have caught the isIntermediateDir bug.
func TestLookupWithIntermediateDirectories(t *testing.T) {
	// Create temporary directory for database
	tmpDir, err := os.MkdirTemp("", "monofs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create real server with database
	dbPath := filepath.Join(tmpDir, "db")
	server, err := NewServer("test-node", ":9000", dbPath, tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()

	// Step 1: Register a repository with nested display path
	// This mimics what the router does during ingestion
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   "test-storage-id-123",
		DisplayPath: "github.com/user/myrepo",
		Source:      "https://github.com/user/myrepo",
	})
	if err != nil {
		t.Fatalf("failed to register repository: %v", err)
	}

	// Step 2: Test looking up intermediate directories
	// This is the critical test that would catch the isIntermediateDir bug

	// Test 1: Lookup root should work
	t.Run("lookup root", func(t *testing.T) {
		resp, err := server.Lookup(ctx, &pb.LookupRequest{
			ParentPath: "",
			Name:       "",
		})
		if err != nil {
			t.Fatalf("lookup root failed: %v", err)
		}
		if !resp.Found {
			t.Error("root should be found")
		}
	})

	// Test 2: Lookup "github.com" (intermediate directory)
	// THIS IS THE BUG - before fix, this would fail
	t.Run("lookup github.com", func(t *testing.T) {
		resp, err := server.Lookup(ctx, &pb.LookupRequest{
			ParentPath: "",
			Name:       "github.com",
		})
		if err != nil {
			t.Fatalf("lookup github.com failed: %v", err)
		}
		if !resp.Found {
			t.Error("github.com should be found as intermediate directory")
		}
		if resp.Mode&0040000 == 0 {
			t.Error("github.com should be a directory")
		}
	})

	// Test 3: Lookup "github.com/user" (another intermediate directory)
	t.Run("lookup github.com/user", func(t *testing.T) {
		resp, err := server.Lookup(ctx, &pb.LookupRequest{
			ParentPath: "github.com",
			Name:       "user",
		})
		if err != nil {
			t.Fatalf("lookup user failed: %v", err)
		}
		if !resp.Found {
			t.Error("github.com/user should be found as intermediate directory")
		}
	})

	// Test 4: Lookup "github.com/user/myrepo" (actual repository)
	t.Run("lookup github.com/user/myrepo", func(t *testing.T) {
		resp, err := server.Lookup(ctx, &pb.LookupRequest{
			ParentPath: "github.com/user",
			Name:       "myrepo",
		})
		if err != nil {
			t.Fatalf("lookup myrepo failed: %v", err)
		}
		if !resp.Found {
			t.Error("myrepo should be found")
		}
	})

	// Test 5: Non-existent path
	t.Run("lookup non-existent", func(t *testing.T) {
		resp, err := server.Lookup(ctx, &pb.LookupRequest{
			ParentPath: "",
			Name:       "nonexistent",
		})
		if err != nil {
			t.Fatalf("lookup should not error: %v", err)
		}
		if resp.Found {
			t.Error("nonexistent path should not be found")
		}
	})
}

// TestIsIntermediateDir directly tests the isIntermediateDir function.
// This is a white-box test that ensures the function works correctly.
func TestIsIntermediateDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")
	server, err := NewServer("test-node", ":9000", dbPath, tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()

	// Register repositories with different display paths
	repos := []struct {
		storageID   string
		displayPath string
	}{
		{"storage1", "github.com/user1/repo1"},
		{"storage2", "github.com/user1/repo2"},
		{"storage3", "github.com/user2/repo3"},
		{"storage4", "gitlab.com/org/project"},
	}

	for _, repo := range repos {
		_, err := server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
			StorageId:   repo.storageID,
			DisplayPath: repo.displayPath,
			Source:      "https://example.com/" + repo.displayPath,
		})
		if err != nil {
			t.Fatalf("failed to register %s: %v", repo.displayPath, err)
		}
	}

	// Test cases
	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{"github.com is intermediate", "github.com", true},
		{"github.com/user1 is intermediate", "github.com/user1", true},
		{"github.com/user2 is intermediate", "github.com/user2", true},
		{"gitlab.com is intermediate", "gitlab.com", true},
		{"gitlab.com/org is intermediate", "gitlab.com/org", true},
		{"nonexistent is not intermediate", "nonexistent", false},
		{"github.com/user3 does not exist", "github.com/user3", false},
		{"partial match should not work", "git", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := server.isIntermediateDir(tt.path)
			if result != tt.expected {
				t.Errorf("isIntermediateDir(%q) = %v, expected %v", tt.path, result, tt.expected)
			}
		})
	}
}

// TestReadDirWithIntermediateDirectories tests that ReadDir correctly lists
// intermediate directories along with repositories.
func TestReadDirWithIntermediateDirectories(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")
	server, err := NewServer("test-node", ":9000", dbPath, tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()

	// Register multiple repositories
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   "storage1",
		DisplayPath: "github.com/user1/repo1",
		Source:      "https://github.com/user1/repo1",
	})
	if err != nil {
		t.Fatalf("failed to register repo1: %v", err)
	}

	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   "storage2",
		DisplayPath: "github.com/user2/repo2",
		Source:      "https://github.com/user2/repo2",
	})
	if err != nil {
		t.Fatalf("failed to register repo2: %v", err)
	}

	// Test ReadDir at root - should show "github.com"
	t.Run("readdir root", func(t *testing.T) {
		stream := &testReadDirStream{}
		err := server.ReadDir(&pb.ReadDirRequest{Path: ""}, stream)
		if err != nil {
			t.Fatalf("ReadDir root failed: %v", err)
		}

		found := false
		for _, entry := range stream.entries {
			if entry.Name == "github.com" {
				found = true
				// Check if it's a directory by checking the mode
				if entry.Mode&0040000 == 0 {
					t.Error("github.com should be a directory")
				}
			}
		}
		if !found {
			t.Error("github.com not found in root ReadDir")
		}
	})

	// Test ReadDir at "github.com" - should show "user1" and "user2"
	t.Run("readdir github.com", func(t *testing.T) {
		stream := &testReadDirStream{}
		err := server.ReadDir(&pb.ReadDirRequest{Path: "github.com"}, stream)
		if err != nil {
			t.Fatalf("ReadDir github.com failed: %v", err)
		}

		foundUser1 := false
		foundUser2 := false
		for _, entry := range stream.entries {
			if entry.Name == "user1" {
				foundUser1 = true
			}
			if entry.Name == "user2" {
				foundUser2 = true
			}
		}
		if !foundUser1 {
			t.Error("user1 not found in github.com")
		}
		if !foundUser2 {
			t.Error("user2 not found in github.com")
		}
	})
}

// TestIntermediateDirCache tests that the intermediate directory cache works correctly.
func TestIntermediateDirCache(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "monofs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")
	server, err := NewServer("test-node", ":9000", dbPath, tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer server.Close()

	ctx := context.Background()

	// Register a repository
	_, err = server.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   "storage1",
		DisplayPath: "github.com/user/repo",
		Source:      "https://github.com/user/repo",
	})
	if err != nil {
		t.Fatalf("failed to register: %v", err)
	}

	// First call - should compute and cache
	result1 := server.isIntermediateDir("github.com")
	if !result1 {
		t.Error("github.com should be intermediate")
	}

	// Check cache was populated
	cached, ok := server.intermediateDirCache.Load("github.com")
	if !ok {
		t.Error("github.com should be in cache")
	}
	if !cached.(bool) {
		t.Error("cached value should be true")
	}

	// Second call - should use cache (would fail if cache lookup broken)
	result2 := server.isIntermediateDir("github.com")
	if !result2 {
		t.Error("github.com should still be intermediate (from cache)")
	}
}

// testReadDirStream implements grpc.ServerStreamingServer for testing ReadDir
type testReadDirStream struct {
	grpc.ServerStream
	entries []*pb.DirEntry
}

func (m *testReadDirStream) Send(entry *pb.DirEntry) error {
	m.entries = append(m.entries, entry)
	return nil
}

func (m *testReadDirStream) Context() context.Context {
	return context.Background()
}
