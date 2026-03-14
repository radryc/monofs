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

// TestDirectoryIndexHierarchy verifies that all parent directories are updated
// when a file is ingested.
func TestDirectoryIndexHierarchy(t *testing.T) {
	// Create temp dir for test
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	// Create server
	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	// Ingest a deeply nested file
	storageID := "test-storage-123"
	displayPath := "test-repo"
	filePath := "cmd/thanos/main.go"

	req := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        filePath,
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0644,
			Size:        30,
			Mtime:       1234567890,
			BlobHash:    "abc123",
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req)
	if err != nil {
		t.Fatalf("IngestFile failed: %v", err)
	}

	// Build directory indexes (required after ingestion)
	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Verify root directory index contains "cmd" as directory
	rootKey := makeDirIndexKey(storageID, "")
	var rootIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, rootKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &rootIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read root directory index: %v", err)
	}

	// Check that "cmd" exists and is a directory
	found := false
	for _, entry := range rootIndex {
		if entry.Name == "cmd" {
			found = true
			if !entry.IsDir {
				t.Errorf("Expected 'cmd' to be a directory, but IsDir=false")
			}
			t.Logf("✓ Root directory contains 'cmd' directory")
			break
		}
	}

	if !found {
		t.Errorf("Root directory does not contain 'cmd' entry. Entries: %+v", rootIndex)
	}

	// Verify "cmd" directory index contains "thanos" as directory
	cmdKey := makeDirIndexKey(storageID, "cmd")
	var cmdIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, cmdKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &cmdIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read 'cmd' directory index: %v", err)
	}

	// Check that "thanos" exists and is a directory
	found = false
	for _, entry := range cmdIndex {
		if entry.Name == "thanos" {
			found = true
			if !entry.IsDir {
				t.Errorf("Expected 'thanos' to be a directory, but IsDir=false")
			}
			t.Logf("✓ 'cmd' directory contains 'thanos' directory")
			break
		}
	}

	if !found {
		t.Errorf("'cmd' directory does not contain 'thanos' entry. Entries: %+v", cmdIndex)
	}

	// Verify "cmd/thanos" directory index contains "main.go" as file
	thanosKey := makeDirIndexKey(storageID, "cmd/thanos")
	var thanosIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, thanosKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &thanosIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read 'cmd/thanos' directory index: %v", err)
	}

	// Check that "main.go" exists and is a file
	found = false
	for _, entry := range thanosIndex {
		if entry.Name == "main.go" {
			found = true
			if entry.IsDir {
				t.Errorf("Expected 'main.go' to be a file, but IsDir=true")
			}
			if entry.Size != 30 {
				t.Errorf("Expected size 30, got %d", entry.Size)
			}
			t.Logf("✓ 'cmd/thanos' directory contains 'main.go' file")
			break
		}
	}

	if !found {
		t.Errorf("'cmd/thanos' directory does not contain 'main.go' entry. Entries: %+v", thanosIndex)
	}
}

// TestDirectoryIndexMultipleFiles verifies that multiple files in the same
// directory are all indexed correctly.
func TestDirectoryIndexMultipleFiles(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-storage-456"
	displayPath := "test-repo"

	// Ingest multiple files
	files := []string{
		"README.md",
		"LICENSE",
		"go.mod",
		"cmd/main.go",
		"cmd/version.go",
		"pkg/util.go",
	}

	for _, filePath := range files {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        filePath,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        12,
				Mtime:       1234567890,
				BlobHash:    "xyz789",
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}

		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Fatalf("IngestFile failed for %s: %v", filePath, err)
		}
	}

	// Build directory indexes (required after ingestion)
	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Verify root directory contains files and subdirectories
	rootKey := makeDirIndexKey(storageID, "")
	var rootIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, rootKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &rootIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read root directory index: %v", err)
	}

	// Check expected entries
	expected := map[string]bool{
		"README.md": false, // file
		"LICENSE":   false, // file
		"go.mod":    false, // file
		"cmd":       true,  // directory
		"pkg":       true,  // directory
	}

	for _, entry := range rootIndex {
		expectedIsDir, found := expected[entry.Name]
		if !found {
			t.Errorf("Unexpected entry in root: %s", entry.Name)
			continue
		}

		if entry.IsDir != expectedIsDir {
			t.Errorf("Entry %s: expected IsDir=%v, got %v", entry.Name, expectedIsDir, entry.IsDir)
		}

		delete(expected, entry.Name)
	}

	if len(expected) > 0 {
		t.Errorf("Missing entries in root directory: %v", expected)
	} else {
		t.Logf("✓ Root directory contains all expected entries")
	}

	// Verify cmd directory contains both files
	cmdKey := makeDirIndexKey(storageID, "cmd")
	var cmdIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, cmdKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &cmdIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read 'cmd' directory index: %v", err)
	}

	if len(cmdIndex) != 2 {
		t.Errorf("Expected 2 files in 'cmd', got %d: %+v", len(cmdIndex), cmdIndex)
	}

	cmdExpected := map[string]bool{
		"main.go":    false,
		"version.go": false,
	}

	for _, entry := range cmdIndex {
		if _, found := cmdExpected[entry.Name]; !found {
			t.Errorf("Unexpected entry in cmd: %s", entry.Name)
		}
		if entry.IsDir {
			t.Errorf("Entry %s in cmd should be a file, not directory", entry.Name)
		}
		delete(cmdExpected, entry.Name)
	}

	if len(cmdExpected) == 0 {
		t.Logf("✓ 'cmd' directory contains both expected files")
	}
}

// TestVirtualDirectoryLookup verifies that Lookup works for virtual directories
// created by the directory index hierarchy.
func TestVirtualDirectoryLookup(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-storage-789"
	displayPath := "myrepo"

	// Ingest a deeply nested file
	req := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "cmd/server/main.go",
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0644,
			Size:        100,
			Mtime:       1234567890,
			BlobHash:    "def456",
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req)
	if err != nil {
		t.Fatalf("IngestFile failed: %v", err)
	}

	// Build directory indexes (required after ingestion)
	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	ctx := context.Background()

	// Test Lookup for "cmd" directory
	lookupResp, err := s.Lookup(ctx, &pb.LookupRequest{
		ParentPath: displayPath,
		Name:       "cmd",
	})
	if err != nil {
		t.Fatalf("Lookup for 'cmd' failed: %v", err)
	}
	if !lookupResp.Found {
		t.Errorf("Lookup for 'cmd' directory returned not found")
	}
	if lookupResp.Mode&uint32(syscall.S_IFDIR) == 0 {
		t.Errorf("Lookup for 'cmd' should return directory mode, got 0%o", lookupResp.Mode)
	} else {
		t.Logf("✓ Lookup found 'cmd' as directory")
	}

	// Test Lookup for "cmd/server" directory
	lookupResp, err = s.Lookup(ctx, &pb.LookupRequest{
		ParentPath: displayPath + "/cmd",
		Name:       "server",
	})
	if err != nil {
		t.Fatalf("Lookup for 'server' failed: %v", err)
	}
	if !lookupResp.Found {
		t.Errorf("Lookup for 'server' directory returned not found")
	}
	if lookupResp.Mode&uint32(syscall.S_IFDIR) == 0 {
		t.Errorf("Lookup for 'server' should return directory mode, got 0%o", lookupResp.Mode)
	} else {
		t.Logf("✓ Lookup found 'cmd/server' as directory")
	}

	// Test GetAttr for "cmd" directory
	attrResp, err := s.GetAttr(ctx, &pb.GetAttrRequest{
		Path: displayPath + "/cmd",
	})
	if err != nil {
		t.Fatalf("GetAttr for 'cmd' failed: %v", err)
	}
	if !attrResp.Found {
		t.Errorf("GetAttr for 'cmd' directory returned not found")
	}
	if attrResp.Mode&uint32(syscall.S_IFDIR) == 0 {
		t.Errorf("GetAttr for 'cmd' should return directory mode, got 0%o", attrResp.Mode)
	} else {
		t.Logf("✓ GetAttr found 'cmd' as directory")
	}

	// Test GetAttr for "cmd/server" directory
	attrResp, err = s.GetAttr(ctx, &pb.GetAttrRequest{
		Path: displayPath + "/cmd/server",
	})
	if err != nil {
		t.Fatalf("GetAttr for 'cmd/server' failed: %v", err)
	}
	if !attrResp.Found {
		t.Errorf("GetAttr for 'cmd/server' directory returned not found")
	}
	if attrResp.Mode&uint32(syscall.S_IFDIR) == 0 {
		t.Errorf("GetAttr for 'cmd/server' should return directory mode, got 0%o", attrResp.Mode)
	} else {
		t.Logf("✓ GetAttr found 'cmd/server' as directory")
	}
}

// TestDirHintPopulatesIndex verifies that dir-hint entries in IngestFileBatch
// update the directory index without storing metadata or ownership.
func TestDirHintPopulatesIndex(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-storage-dirhint"
	displayPath := "dependency"

	// Register repo first.
	_, err = s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "test",
	})
	if err != nil {
		t.Fatalf("RegisterRepository failed: %v", err)
	}

	// Ingest one real file that this node "owns".
	_, err = s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "test",
		Files: []*pb.FileMetadata{
			{Path: "pkg/enry.go", Mode: 0444, Size: 100, Mtime: 1000, BlobHash: "aaa"},
		},
	})
	if err != nil {
		t.Fatalf("IngestFileBatch (real file) failed: %v", err)
	}

	// Send dir-hint entries for files that OTHER nodes own (no content/blob).
	_, err = s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "dir-hint",
		Files: []*pb.FileMetadata{
			{Path: "pkg/classifier.go", Mode: 0444, Size: 2777, Mtime: 1000,
				BackendMetadata: map[string]string{"dir_hint": "true"}},
			{Path: "pkg/common.go", Mode: 0444, Size: 500, Mtime: 1000,
				BackendMetadata: map[string]string{"dir_hint": "true"}},
		},
	})
	if err != nil {
		t.Fatalf("IngestFileBatch (dir hints) failed: %v", err)
	}

	// Now build directory indexes (from locally-owned files only).
	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// The dir index for "pkg" should have ALL 3 entries:
	// enry.go (local), classifier.go (dir-hint), common.go (dir-hint).
	var dirIndex []dirIndexEntry
	s.db.View(func(tx *nutsdb.Tx) error {
		val, err := tx.Get(bucketDirIndex, makeDirIndexKey(storageID, "pkg"))
		if err != nil {
			t.Fatalf("dir index for 'pkg' not found: %v", err)
		}
		return json.Unmarshal(val, &dirIndex)
	})

	names := make(map[string]bool)
	for _, e := range dirIndex {
		names[e.Name] = true
	}

	if len(dirIndex) != 3 {
		t.Errorf("expected 3 entries in pkg dir index, got %d: %v", len(dirIndex), names)
	}
	for _, want := range []string{"enry.go", "classifier.go", "common.go"} {
		if !names[want] {
			t.Errorf("dir index missing %q, got entries: %v", want, names)
		}
	}

	// Dir-hint entries should NOT have created metadata or ownership.
	key := makeStorageKey(storageID, "pkg/classifier.go")
	var metadataExists bool
	s.db.View(func(tx *nutsdb.Tx) error {
		_, err := tx.Get(bucketMetadata, key)
		metadataExists = (err == nil)
		return nil
	})
	if metadataExists {
		t.Error("dir-hint file should NOT have metadata stored")
	}

	var ownershipExists bool
	s.db.View(func(tx *nutsdb.Tx) error {
		_, err := tx.Get(bucketOwnedFiles, []byte(storageID+":pkg/classifier.go"))
		ownershipExists = (err == nil)
		return nil
	})
	if ownershipExists {
		t.Error("dir-hint file should NOT have ownership stored")
	}
}
