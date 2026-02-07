package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
)

// TestHooksDirectorySharding tests whether hooks directory gets properly indexed
// when files under it are distributed across different nodes
func TestHooksDirectorySharding(t *testing.T) {
	// Create temp dir for test
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	// Create server
	server, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer server.Close()

	storageID := "test_storage_hooks"
	displayPath := "github.com/test/repo"

	// Register repository first
	_, err = server.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "https://github.com/test/repo",
	})
	if err != nil {
		t.Fatalf("Failed to register repository: %v", err)
	}

	// Simulate ingesting files from hooks directory that might be sharded to this node
	testFiles := []struct {
		path string
		size uint64
		mode uint32
	}{
		// Files that would be in hooks/ subdirectory
		{"hooks/pre-commit/check.sh", 100, 0755},
		{"hooks/pre-push/validate.sh", 200, 0755},
		{"hooks/commit-msg/format.sh", 150, 0755},
		// Add some other files to root
		{"Makefile", 1000, 0644},
		{"README.md", 500, 0644},
	}

	for _, tf := range testFiles {
		_, err := server.IngestFile(context.Background(), &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        tf.path,
				StorageId:   storageID,
				DisplayPath: displayPath,
				Ref:         "main",
				Size:        tf.size,
				Mtime:       1234567890,
				Mode:        tf.mode,
				BlobHash:    "dummy_" + tf.path,
				Source:      "https://github.com/test/repo",
			},
		})
		if err != nil {
			t.Fatalf("Failed to ingest file %s: %v", tf.path, err)
		}
	}

	// Build directory indexes (required after ingestion)
	_, err = server.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Now check root directory index in database - should contain "hooks", "Makefile", "README.md"
	rootKey := makeDirIndexKey(storageID, "")
	var rootIndex []dirIndexEntry

	err = server.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, rootKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &rootIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read root directory index: %v", err)
	}

	t.Logf("Root directory has %d entries", len(rootIndex))
	for _, entry := range rootIndex {
		t.Logf("  - %s (IsDir=%v)", entry.Name, entry.IsDir)
	}

	// Verify "hooks" directory exists in root
	hooksFound := false
	makefileFound := false
	readmeFound := false

	for _, entry := range rootIndex {
		if entry.Name == "hooks" {
			hooksFound = true
			if !entry.IsDir {
				t.Errorf("hooks entry is not marked as directory!")
			}
		}
		if entry.Name == "Makefile" {
			makefileFound = true
		}
		if entry.Name == "README.md" {
			readmeFound = true
		}
	}

	if !hooksFound {
		t.Errorf("hooks directory not found in root directory index!")
	}
	if !makefileFound {
		t.Errorf("Makefile not found in root directory index!")
	}
	if !readmeFound {
		t.Errorf("README.md not found in root directory index!")
	}

	// Now try to lookup hooks directory
	lookupResp, err := server.Lookup(context.Background(), &pb.LookupRequest{
		ParentPath: displayPath,
		Name:       "hooks",
	})
	if err != nil {
		t.Fatalf("Lookup hooks failed: %v", err)
	}
	if !lookupResp.Found {
		t.Errorf("Lookup for hooks directory returned not found")
	}
	if lookupResp.Mode&uint32(syscall.S_IFDIR) == 0 {
		t.Errorf("Lookup for hooks returned non-directory mode: 0%o", lookupResp.Mode)
	}
}
