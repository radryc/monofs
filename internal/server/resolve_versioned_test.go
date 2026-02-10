package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

// testGenerateStorageID matches the router's testGenerateStorageID for test setup.
func testGenerateStorageID(displayPath string) string {
	hash := sha256.Sum256([]byte(displayPath))
	return hex.EncodeToString(hash[:])
}

// TestResolvePathToStorage_VersionedRepoDoesNotLeakIntoGitRepo tests the critical
// bug where a versioned npm/go display path like "github.com/webpack/webpack@5.89.0"
// would cause the Go module fallback in resolvePathToStorage to incorrectly match
// paths INSIDE the versioned repo against the unversioned git repo.
//
// BUG SCENARIO:
//  1. Git repo ingested at display path "github.com/webpack/webpack" (STORAGE_A)
//  2. npm package ingested at display path "github.com/webpack/webpack@5.89.0" (STORAGE_B)
//  3. ReadDir("github.com/webpack/webpack@5.89.0/compiler") should resolve to
//     (STORAGE_B, "compiler") — the npm package's compiler/ directory
//  4. WITHOUT THE FIX: the Go module fallback would strip "@5.89.0/compiler" to get
//     "github.com/webpack/webpack", match STORAGE_A, and return (STORAGE_A, "") —
//     the git repo ROOT, causing every subdirectory to show root entries (infinite loop)
func TestResolvePathToStorage_VersionedRepoDoesNotLeakIntoGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db")
	s, err := NewServer("test-node", ":9000", dbPath, tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Register git repo: github.com/webpack/webpack → STORAGE_A
	gitStorageID := testGenerateStorageID("github.com/webpack/webpack")
	_, err = s.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   gitStorageID,
		DisplayPath: "github.com/webpack/webpack",
		Source:      "https://github.com/webpack/webpack",
	})
	if err != nil {
		t.Fatalf("failed to register git repo: %v", err)
	}

	// Register npm versioned repo: github.com/webpack/webpack@5.89.0 → STORAGE_B
	npmStorageID := testGenerateStorageID("github.com/webpack/webpack@5.89.0")
	_, err = s.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   npmStorageID,
		DisplayPath: "github.com/webpack/webpack@5.89.0",
		Source:      "webpack@5.89.0",
	})
	if err != nil {
		t.Fatalf("failed to register npm repo: %v", err)
	}

	// Verify they have different storage IDs
	if gitStorageID == npmStorageID {
		t.Fatalf("storage IDs should be different: git=%s npm=%s", gitStorageID, npmStorageID)
	}

	tests := []struct {
		name            string
		path            string
		expectOK        bool
		expectStorageID string
		expectFilePath  string
		description     string
	}{
		{
			name:            "git repo root",
			path:            "github.com/webpack/webpack",
			expectOK:        true,
			expectStorageID: gitStorageID,
			expectFilePath:  "",
			description:     "should resolve to git repo root",
		},
		{
			name:            "git repo file",
			path:            "github.com/webpack/webpack/compiler/Compiler.js",
			expectOK:        true,
			expectStorageID: gitStorageID,
			expectFilePath:  "compiler/Compiler.js",
			description:     "should resolve file inside git repo",
		},
		{
			name:            "versioned repo root",
			path:            "github.com/webpack/webpack@5.89.0",
			expectOK:        true,
			expectStorageID: npmStorageID,
			expectFilePath:  "",
			description:     "should resolve to versioned npm repo root",
		},
		{
			name:            "versioned repo subdir",
			path:            "github.com/webpack/webpack@5.89.0/compiler",
			expectOK:        true,
			expectStorageID: npmStorageID,
			expectFilePath:  "compiler",
			description:     "CRITICAL: should resolve to npm repo's compiler/, NOT git repo root",
		},
		{
			name:            "versioned repo deeply nested",
			path:            "github.com/webpack/webpack@5.89.0/compiler/flow-typed/defs",
			expectOK:        true,
			expectStorageID: npmStorageID,
			expectFilePath:  "compiler/flow-typed/defs",
			description:     "CRITICAL: deep paths inside versioned repo must stay in npm storage",
		},
		{
			name:            "versioned repo file",
			path:            "github.com/webpack/webpack@5.89.0/package.json",
			expectOK:        true,
			expectStorageID: npmStorageID,
			expectFilePath:  "package.json",
			description:     "file in versioned repo root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storageID, filePath, ok := s.resolvePathToStorage(tt.path)
			if ok != tt.expectOK {
				t.Errorf("%s: resolvePathToStorage(%q) ok=%v, want %v",
					tt.description, tt.path, ok, tt.expectOK)
				return
			}
			if !tt.expectOK {
				return
			}
			if storageID != tt.expectStorageID {
				whichRepo := "unknown"
				if storageID == gitStorageID {
					whichRepo = "GIT REPO (WRONG!)"
				} else if storageID == npmStorageID {
					whichRepo = "npm repo"
				}
				t.Errorf("%s: resolvePathToStorage(%q) storageID=%s (%s), want %s",
					tt.description, tt.path, storageID[:12]+"...", whichRepo, tt.expectStorageID[:12]+"...")
			}
			if filePath != tt.expectFilePath {
				t.Errorf("%s: resolvePathToStorage(%q) filePath=%q, want %q",
					tt.description, tt.path, filePath, tt.expectFilePath)
			}
		})
	}
}

// TestResolvePathToStorage_GoModuleFallbackStillWorks ensures the Go module
// fallback (stripping @version) still works correctly for its intended purpose.
func TestResolvePathToStorage_GoModuleFallbackStillWorks(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db")
	s, err := NewServer("test-node", ":9000", dbPath, tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Register a Go module WITHOUT version suffix
	goStorageID := testGenerateStorageID("google.golang.org/grpc")
	_, err = s.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   goStorageID,
		DisplayPath: "google.golang.org/grpc",
		Source:      "google.golang.org/grpc",
	})
	if err != nil {
		t.Fatalf("failed to register go module: %v", err)
	}

	tests := []struct {
		name            string
		path            string
		expectOK        bool
		expectStorageID string
		expectFilePath  string
	}{
		{
			name:            "exact match",
			path:            "google.golang.org/grpc",
			expectOK:        true,
			expectStorageID: goStorageID,
			expectFilePath:  "",
		},
		{
			name:            "versioned path - fallback should strip @version",
			path:            "google.golang.org/grpc@v1.75.0",
			expectOK:        true,
			expectStorageID: goStorageID,
			expectFilePath:  "",
		},
		{
			name:            "versioned path with file",
			path:            "google.golang.org/grpc@v1.75.0/server.go",
			expectOK:        true,
			expectStorageID: goStorageID,
			expectFilePath:  "server.go",
		},
		{
			name:            "versioned path with nested file",
			path:            "google.golang.org/grpc@v1.75.0/internal/transport/handler.go",
			expectOK:        true,
			expectStorageID: goStorageID,
			expectFilePath:  "internal/transport/handler.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storageID, filePath, ok := s.resolvePathToStorage(tt.path)
			if ok != tt.expectOK {
				t.Errorf("resolvePathToStorage(%q) ok=%v, want %v", tt.path, ok, tt.expectOK)
				return
			}
			if storageID != tt.expectStorageID {
				t.Errorf("resolvePathToStorage(%q) storageID=%q, want %q", tt.path, storageID, tt.expectStorageID)
			}
			if filePath != tt.expectFilePath {
				t.Errorf("resolvePathToStorage(%q) filePath=%q, want %q", tt.path, filePath, tt.expectFilePath)
			}
		})
	}
}
