// Package server provides resilience tests for directory indexing.
// These tests ensure the filesystem remains consistent after various operations
// including updates, concurrent access, edge cases, and server restarts.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc/metadata"
)

// TestFileUpdateReplacesEntry verifies that updating a file properly replaces
// its directory index entry with new metadata.
func TestFileUpdateReplacesEntry(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-update-storage"
	displayPath := "update-repo"
	filePath := "src/main.go"

	// Initial ingestion
	req1 := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        filePath,
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0644,
			Size:        100,
			Mtime:       1000000000,
			BlobHash:    "hash-v1",
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req1)
	if err != nil {
		t.Fatalf("Initial IngestFile failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Update with new content
	req2 := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        filePath,
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0755, // Different mode
			Size:        200,  // Different size
			Mtime:       2000000000,
			BlobHash:    "hash-v2", // Different hash
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req2)
	if err != nil {
		t.Fatalf("Update IngestFile failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes after update failed: %v", err)
	}

	// Verify the file entry was updated, not duplicated
	srcKey := makeDirIndexKey(storageID, "src")
	var srcIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, srcKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &srcIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read src directory index: %v", err)
	}

	// Should have exactly one entry for main.go
	mainGoCount := 0
	var foundEntry *dirIndexEntry
	for i, entry := range srcIndex {
		if entry.Name == "main.go" {
			mainGoCount++
			foundEntry = &srcIndex[i]
		}
	}

	if mainGoCount != 1 {
		t.Errorf("Expected exactly 1 main.go entry, found %d. Entries: %+v", mainGoCount, srcIndex)
	}

	if foundEntry == nil {
		t.Fatal("main.go entry not found")
	}

	// Verify updated values
	if foundEntry.Size != 200 {
		t.Errorf("Expected size 200, got %d", foundEntry.Size)
	}
	if foundEntry.Mtime != 2000000000 {
		t.Errorf("Expected mtime 2000000000, got %d", foundEntry.Mtime)
	}

	t.Log("✓ File update correctly replaced entry without duplication")
}

// TestDirectoryMtimePropagation verifies that parent directory mtimes
// are updated when files within them change.
func TestDirectoryMtimePropagation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-mtime-storage"
	displayPath := "mtime-repo"

	// Ingest older file first
	req1 := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "dir/older.txt",
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0644,
			Size:        10,
			Mtime:       1000000000,
			BlobHash:    "hash1",
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req1)
	if err != nil {
		t.Fatalf("IngestFile failed: %v", err)
	}

	// Ingest newer file
	req2 := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "dir/newer.txt",
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0644,
			Size:        20,
			Mtime:       2000000000, // Newer mtime
			BlobHash:    "hash2",
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req2)
	if err != nil {
		t.Fatalf("IngestFile failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Check that root's "dir" entry has the newer mtime
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

	for _, entry := range rootIndex {
		if entry.Name == "dir" && entry.IsDir {
			if entry.Mtime != 2000000000 {
				t.Errorf("Expected directory mtime to be 2000000000 (newest file), got %d", entry.Mtime)
			} else {
				t.Log("✓ Directory mtime correctly propagated from newest file")
			}
			return
		}
	}

	t.Error("Directory 'dir' not found in root index")
}

// TestConcurrentDirectoryOperations tests race conditions in directory updates.
func TestConcurrentDirectoryOperations(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-concurrent-storage"
	displayPath := "concurrent-repo"

	const numGoroutines = 20
	const filesPerGoroutine = 5

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	// Ingest files concurrently into the same directory structure
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for f := 0; f < filesPerGoroutine; f++ {
				req := &pb.IngestFileRequest{
					Metadata: &pb.FileMetadata{
						Path:        fmt.Sprintf("pkg/module%d/file%d.go", goroutineID, f),
						DisplayPath: displayPath,
						StorageId:   storageID,
						Mode:        0644,
						Size:        uint64(100 + goroutineID*10 + f),
						Mtime:       time.Now().Unix(),
						BlobHash:    fmt.Sprintf("hash-%d-%d", goroutineID, f),
						Source:      "https://github.com/test/repo.git",
						Ref:         "main",
					},
				}

				_, err := s.IngestFile(context.Background(), req)
				if err != nil {
					errorCount.Add(1)
				} else {
					successCount.Add(1)
				}
			}
		}(g)
	}

	wg.Wait()

	if errorCount.Load() > 0 {
		t.Errorf("Had %d errors during concurrent ingestion", errorCount.Load())
	}

	// Build indexes
	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Verify all directories exist
	pkgKey := makeDirIndexKey(storageID, "pkg")
	var pkgIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, pkgKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &pkgIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read pkg directory index: %v", err)
	}

	// Should have numGoroutines module directories
	if len(pkgIndex) != numGoroutines {
		t.Errorf("Expected %d module directories, found %d", numGoroutines, len(pkgIndex))
	}

	// Verify each module directory has the right number of files
	for _, entry := range pkgIndex {
		if !entry.IsDir {
			t.Errorf("Expected %s to be a directory", entry.Name)
			continue
		}

		moduleKey := makeDirIndexKey(storageID, "pkg/"+entry.Name)
		var moduleIndex []dirIndexEntry

		err = s.db.View(func(tx *nutsdb.Tx) error {
			value, err := tx.Get(bucketDirIndex, moduleKey)
			if err != nil {
				return err
			}
			return json.Unmarshal(value, &moduleIndex)
		})

		if err != nil {
			t.Errorf("Failed to read %s directory index: %v", entry.Name, err)
			continue
		}

		if len(moduleIndex) != filesPerGoroutine {
			t.Errorf("Module %s: expected %d files, got %d", entry.Name, filesPerGoroutine, len(moduleIndex))
		}
	}

	t.Logf("✓ Concurrent operations: %d files ingested successfully", successCount.Load())
}

// TestSingleComponentPath tests files at the root of a repository (single path component).
func TestSingleComponentPath(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-single-storage"
	displayPath := "single-repo"

	// Ingest root-level files
	files := []string{"README.md", "LICENSE", "Makefile", ".gitignore"}

	for _, file := range files {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        file,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        100,
				Mtime:       1234567890,
				BlobHash:    "hash-" + file,
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}

		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Fatalf("IngestFile for %s failed: %v", file, err)
		}
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Verify root index contains all files
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

	if len(rootIndex) != len(files) {
		t.Errorf("Expected %d root entries, got %d", len(files), len(rootIndex))
	}

	// Verify each file is marked as a file, not a directory
	for _, entry := range rootIndex {
		if entry.IsDir {
			t.Errorf("Expected %s to be a file, not a directory", entry.Name)
		}
	}

	// Verify Lookup works for root-level files
	ctx := context.Background()
	for _, file := range files {
		resp, err := s.Lookup(ctx, &pb.LookupRequest{
			ParentPath: displayPath,
			Name:       file,
		})
		if err != nil {
			t.Errorf("Lookup for %s failed: %v", file, err)
			continue
		}
		if !resp.Found {
			t.Errorf("Lookup for %s returned not found", file)
		}
		if resp.Mode&uint32(syscall.S_IFREG) == 0 {
			t.Errorf("Expected file mode for %s, got 0%o", file, resp.Mode)
		}
	}

	t.Log("✓ Single component paths handled correctly")
}

// TestLargeDirectory tests directory indexing with many files.
func TestLargeDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large directory test in short mode")
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-large-storage"
	displayPath := "large-repo"

	const numFiles = 1000

	// Ingest many files into a single directory
	for i := 0; i < numFiles; i++ {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        fmt.Sprintf("bigdir/file%04d.txt", i),
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        uint64(i * 10),
				Mtime:       int64(1000000000 + i),
				BlobHash:    fmt.Sprintf("hash-%d", i),
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}

		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Fatalf("IngestFile for file%04d.txt failed: %v", i, err)
		}
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Verify all files are in the directory
	bigdirKey := makeDirIndexKey(storageID, "bigdir")
	var bigdirIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, bigdirKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &bigdirIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read bigdir directory index: %v", err)
	}

	if len(bigdirIndex) != numFiles {
		t.Errorf("Expected %d files in bigdir, got %d", numFiles, len(bigdirIndex))
	}

	t.Logf("✓ Large directory with %d files indexed correctly", numFiles)
}

// TestMixedBatchAndSingleOperations tests consistency when mixing
// batch ingestion with single file operations.
func TestMixedBatchAndSingleOperations(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-mixed-storage"
	displayPath := "mixed-repo"

	// First batch of files
	batch1Files := []string{"src/a.go", "src/b.go", "src/c.go"}
	for _, file := range batch1Files {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        file,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        100,
				Mtime:       1000000000,
				BlobHash:    "hash-" + file,
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}
		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Fatalf("IngestFile failed: %v", err)
		}
	}

	// Build indexes after first batch
	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Add more files after initial indexing
	additionalFiles := []string{"src/d.go", "src/subdir/e.go"}
	for _, file := range additionalFiles {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        file,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        200,
				Mtime:       2000000000,
				BlobHash:    "hash-" + file,
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}
		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Fatalf("IngestFile failed: %v", err)
		}
	}

	// Rebuild indexes
	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Verify all files are present
	srcKey := makeDirIndexKey(storageID, "src")
	var srcIndex []dirIndexEntry

	err = s.db.View(func(tx *nutsdb.Tx) error {
		value, err := tx.Get(bucketDirIndex, srcKey)
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &srcIndex)
	})

	if err != nil {
		t.Fatalf("Failed to read src directory index: %v", err)
	}

	// Should have 4 files + 1 subdir = 5 entries
	expectedEntries := 5
	if len(srcIndex) != expectedEntries {
		t.Errorf("Expected %d entries in src, got %d: %+v", expectedEntries, len(srcIndex), srcIndex)
	}

	// Verify subdir exists
	subdirFound := false
	for _, entry := range srcIndex {
		if entry.Name == "subdir" && entry.IsDir {
			subdirFound = true
			break
		}
	}
	if !subdirFound {
		t.Error("Subdir not found in src directory")
	}

	t.Log("✓ Mixed batch and single operations handled correctly")
}

// TestReadDirAfterIngestion verifies ReadDir returns correct entries.
func TestReadDirAfterIngestion(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-readdir-storage"
	displayPath := "readdir-repo"

	// Ingest files with a mix of directories and files
	files := map[string]bool{
		"README.md":        false, // file
		"src/main.go":      false, // file in dir
		"src/util.go":      false, // file in dir
		"docs/README.md":   false, // file in dir
		"docs/api/spec.md": false, // file in nested dir
	}

	for file := range files {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        file,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        100,
				Mtime:       1234567890,
				BlobHash:    "hash-" + file,
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}
		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Fatalf("IngestFile failed: %v", err)
		}
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Test ReadDir for root
	rootEntries := make(map[string]bool)
	stream := newMockDirEntryStream()

	err = s.ReadDir(&pb.ReadDirRequest{Path: displayPath}, stream)
	if err != nil {
		t.Fatalf("ReadDir for root failed: %v", err)
	}

	for _, entry := range stream.entries {
		rootEntries[entry.Name] = entry.Mode&uint32(syscall.S_IFDIR) != 0
	}

	// Root should have: README.md (file), src (dir), docs (dir)
	expectedRoot := map[string]bool{
		"README.md": false, // file
		"src":       true,  // dir
		"docs":      true,  // dir
	}

	for name, isDir := range expectedRoot {
		if gotIsDir, found := rootEntries[name]; !found {
			t.Errorf("Missing entry in root: %s", name)
		} else if gotIsDir != isDir {
			t.Errorf("Entry %s: expected isDir=%v, got %v", name, isDir, gotIsDir)
		}
	}

	t.Log("✓ ReadDir returns correct entries after ingestion")
}

// TestGetAttrConsistency verifies GetAttr returns consistent metadata.
func TestGetAttrConsistency(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-getattr-storage"
	displayPath := "getattr-repo"
	filePath := "src/main.go"
	expectedSize := uint64(12345)
	expectedMtime := int64(1609459200)
	expectedMode := uint32(0755)

	req := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        filePath,
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        expectedMode,
			Size:        expectedSize,
			Mtime:       expectedMtime,
			BlobHash:    "somehash",
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req)
	if err != nil {
		t.Fatalf("IngestFile failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	ctx := context.Background()

	// GetAttr for file
	fileAttr, err := s.GetAttr(ctx, &pb.GetAttrRequest{
		Path: displayPath + "/" + filePath,
	})
	if err != nil {
		t.Fatalf("GetAttr for file failed: %v", err)
	}

	if !fileAttr.Found {
		t.Fatal("GetAttr returned not found for file")
	}
	if fileAttr.Size != expectedSize {
		t.Errorf("GetAttr size: expected %d, got %d", expectedSize, fileAttr.Size)
	}
	if fileAttr.Mtime != expectedMtime {
		t.Errorf("GetAttr mtime: expected %d, got %d", expectedMtime, fileAttr.Mtime)
	}

	// GetAttr for directory
	dirAttr, err := s.GetAttr(ctx, &pb.GetAttrRequest{
		Path: displayPath + "/src",
	})
	if err != nil {
		t.Fatalf("GetAttr for directory failed: %v", err)
	}

	if !dirAttr.Found {
		t.Fatal("GetAttr returned not found for directory")
	}
	if dirAttr.Mode&uint32(syscall.S_IFDIR) == 0 {
		t.Error("GetAttr for directory should have directory mode set")
	}

	t.Log("✓ GetAttr returns consistent metadata")
}

// TestSpecialCharactersInPath tests handling of various path patterns.
func TestSpecialCharactersInPath(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-special-storage"
	displayPath := "special-repo"

	// Test various path patterns (excluding truly problematic characters)
	testPaths := []string{
		"file-with-dash.txt",
		"file_with_underscore.txt",
		"file.multiple.dots.txt",
		"CamelCaseFile.go",
		"UPPERCASE.MD",
		"123numeric.txt",
		"dir-name/file.txt",
		"nested/deep/path/file.txt",
	}

	for _, path := range testPaths {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        path,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        100,
				Mtime:       1234567890,
				BlobHash:    "hash",
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}

		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Errorf("IngestFile for path %q failed: %v", path, err)
		}
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Verify files can be looked up
	ctx := context.Background()
	for _, path := range testPaths {
		fullPath := displayPath + "/" + path
		attr, err := s.GetAttr(ctx, &pb.GetAttrRequest{Path: fullPath})
		if err != nil {
			t.Errorf("GetAttr for %q failed: %v", path, err)
			continue
		}
		if !attr.Found {
			t.Errorf("GetAttr for %q returned not found", path)
		}
	}

	t.Log("✓ Special characters in paths handled correctly")
}

// mockDirEntryStream implements the streaming interface for testing.
type mockDirEntryStream struct {
	entries []*pb.DirEntry
	ctx     context.Context
}

func newMockDirEntryStream() *mockDirEntryStream {
	return &mockDirEntryStream{
		entries: []*pb.DirEntry{},
		ctx:     context.Background(),
	}
}

func (m *mockDirEntryStream) Send(entry *pb.DirEntry) error {
	m.entries = append(m.entries, entry)
	return nil
}

func (m *mockDirEntryStream) SetHeader(md metadata.MD) error {
	return nil
}

func (m *mockDirEntryStream) SendHeader(md metadata.MD) error {
	return nil
}

func (m *mockDirEntryStream) SetTrailer(md metadata.MD) {
}

func (m *mockDirEntryStream) Context() context.Context {
	return m.ctx
}

func (m *mockDirEntryStream) SendMsg(msg interface{}) error {
	return nil
}

func (m *mockDirEntryStream) RecvMsg(msg interface{}) error {
	return nil
}

// TestVirtualDirectoryMtime verifies that virtual directories have proper mtime.
func TestVirtualDirectoryMtime(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-vdir-mtime-storage"
	displayPath := "vdir-mtime-repo"
	expectedMtime := int64(1609459200)

	req := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "deep/nested/file.txt",
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0644,
			Size:        100,
			Mtime:       expectedMtime,
			BlobHash:    "hash",
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req)
	if err != nil {
		t.Fatalf("IngestFile failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	ctx := context.Background()

	// Check virtual directory has correct mtime
	resp, err := s.GetAttr(ctx, &pb.GetAttrRequest{
		Path: displayPath + "/deep",
	})
	if err != nil {
		t.Fatalf("GetAttr failed: %v", err)
	}

	if !resp.Found {
		t.Fatal("Virtual directory not found")
	}

	if resp.Mode&uint32(syscall.S_IFDIR) == 0 {
		t.Error("Expected directory mode")
	}

	// Mtime should be propagated from file
	if resp.Mtime != expectedMtime {
		t.Errorf("Virtual directory mtime: expected %d, got %d", expectedMtime, resp.Mtime)
	}

	t.Log("✓ Virtual directory mtime correctly set")
}

// TestEmptyRepositoryReadDir tests ReadDir on an empty repository.
func TestEmptyRepositoryReadDir(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	// Register repository but don't add any files
	ctx := context.Background()
	storageID := "empty-repo-storage"
	displayPath := "empty-repo"

	_, err = s.RegisterRepository(ctx, &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "https://github.com/test/empty-repo",
	})
	if err != nil {
		t.Fatalf("RegisterRepository failed: %v", err)
	}

	// ReadDir should return empty
	stream := newMockDirEntryStream()
	err = s.ReadDir(&pb.ReadDirRequest{Path: displayPath}, stream)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	if len(stream.entries) != 0 {
		t.Errorf("Expected empty directory, got %d entries: %+v", len(stream.entries), stream.entries)
	}

	t.Log("✓ Empty repository ReadDir returns no entries")
}

// TestNonExistentPathLookup tests looking up paths that don't exist.
func TestNonExistentPathLookup(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-nonexistent-storage"
	displayPath := "nonexistent-repo"

	// Ingest a file
	req := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "existing/file.txt",
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0644,
			Size:        100,
			Mtime:       1234567890,
			BlobHash:    "hash",
			Source:      "https://github.com/test/repo.git",
			Ref:         "main",
		},
	}

	_, err = s.IngestFile(context.Background(), req)
	if err != nil {
		t.Fatalf("IngestFile failed: %v", err)
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	ctx := context.Background()

	// Lookup non-existent file
	resp, err := s.Lookup(ctx, &pb.LookupRequest{
		ParentPath: displayPath + "/existing",
		Name:       "nonexistent.txt",
	})
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if resp.Found {
		t.Error("Expected non-existent file to not be found")
	}

	// Lookup non-existent directory
	resp, err = s.Lookup(ctx, &pb.LookupRequest{
		ParentPath: displayPath,
		Name:       "nonexistent-dir",
	})
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if resp.Found {
		t.Error("Expected non-existent directory to not be found")
	}

	// GetAttr for non-existent path
	attrResp, err := s.GetAttr(ctx, &pb.GetAttrRequest{
		Path: displayPath + "/nonexistent/path/file.txt",
	})
	if err != nil {
		t.Fatalf("GetAttr failed: %v", err)
	}
	if attrResp.Found {
		t.Error("Expected non-existent path to not be found")
	}

	t.Log("✓ Non-existent paths correctly return not found")
}

// TestMultipleRepositoriesIsolation tests that directory indexes are isolated between repos.
func TestMultipleRepositoriesIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	// Create two repositories with similar file structures
	repos := []struct {
		storageID   string
		displayPath string
		files       []string
	}{
		{
			storageID:   "repo1-storage",
			displayPath: "repo1",
			files:       []string{"src/main.go", "src/util.go"},
		},
		{
			storageID:   "repo2-storage",
			displayPath: "repo2",
			files:       []string{"src/app.go", "lib/helper.go"},
		},
	}

	for _, repo := range repos {
		for _, file := range repo.files {
			req := &pb.IngestFileRequest{
				Metadata: &pb.FileMetadata{
					Path:        file,
					DisplayPath: repo.displayPath,
					StorageId:   repo.storageID,
					Mode:        0644,
					Size:        100,
					Mtime:       1234567890,
					BlobHash:    "hash-" + file,
					Source:      "https://github.com/test/" + repo.displayPath,
					Ref:         "main",
				},
			}
			_, err = s.IngestFile(context.Background(), req)
			if err != nil {
				t.Fatalf("IngestFile failed: %v", err)
			}
		}

		_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
			StorageId: repo.storageID,
		})
		if err != nil {
			t.Fatalf("BuildDirectoryIndexes failed: %v", err)
		}
	}

	// Verify each repo has its own files
	ctx := context.Background()

	// Repo1 should have main.go and util.go in src
	stream1 := newMockDirEntryStream()
	err = s.ReadDir(&pb.ReadDirRequest{Path: "repo1/src"}, stream1)
	if err != nil {
		t.Fatalf("ReadDir for repo1/src failed: %v", err)
	}

	repo1Files := make(map[string]bool)
	for _, entry := range stream1.entries {
		repo1Files[entry.Name] = true
	}

	if !repo1Files["main.go"] || !repo1Files["util.go"] {
		t.Errorf("Repo1 src should have main.go and util.go, got: %v", repo1Files)
	}
	if repo1Files["app.go"] {
		t.Error("Repo1 src should not have app.go from repo2")
	}

	// Repo2 should have app.go in src
	stream2 := newMockDirEntryStream()
	err = s.ReadDir(&pb.ReadDirRequest{Path: "repo2/src"}, stream2)
	if err != nil {
		t.Fatalf("ReadDir for repo2/src failed: %v", err)
	}

	repo2Files := make(map[string]bool)
	for _, entry := range stream2.entries {
		repo2Files[entry.Name] = true
	}

	if !repo2Files["app.go"] {
		t.Errorf("Repo2 src should have app.go, got: %v", repo2Files)
	}
	if repo2Files["main.go"] || repo2Files["util.go"] {
		t.Error("Repo2 src should not have files from repo1")
	}

	// Repo2 should have lib directory
	resp, err := s.Lookup(ctx, &pb.LookupRequest{
		ParentPath: "repo2",
		Name:       "lib",
	})
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if !resp.Found {
		t.Error("Repo2 should have lib directory")
	}

	// Repo1 should NOT have lib directory
	resp, err = s.Lookup(ctx, &pb.LookupRequest{
		ParentPath: "repo1",
		Name:       "lib",
	})
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if resp.Found {
		t.Error("Repo1 should NOT have lib directory")
	}

	t.Log("✓ Multiple repositories are properly isolated")
}

// TestIncrementalIndexing tests that files added after initial indexing are still accessible.
func TestIncrementalIndexing(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "incremental-storage"
	displayPath := "incremental-repo"

	// Phase 1: Add initial files
	initialFiles := []string{"src/a.go", "src/b.go"}
	for _, file := range initialFiles {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        file,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        100,
				Mtime:       1000000000,
				BlobHash:    "hash-" + file,
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}
		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Fatalf("IngestFile failed: %v", err)
		}
	}

	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Phase 2: Add more files
	newFiles := []string{"src/c.go", "pkg/util.go"}
	for _, file := range newFiles {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        file,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        200,
				Mtime:       2000000000,
				BlobHash:    "hash-" + file,
				Source:      "https://github.com/test/repo.git",
				Ref:         "main",
			},
		}
		_, err = s.IngestFile(context.Background(), req)
		if err != nil {
			t.Fatalf("IngestFile failed: %v", err)
		}
	}

	// Rebuild indexes
	_, err = s.BuildDirectoryIndexes(context.Background(), &pb.BuildDirectoryIndexesRequest{
		StorageId: storageID,
	})
	if err != nil {
		t.Fatalf("BuildDirectoryIndexes failed: %v", err)
	}

	// Verify all files are accessible
	ctx := context.Background()
	allFiles := append(initialFiles, newFiles...)

	for _, file := range allFiles {
		fullPath := displayPath + "/" + file
		resp, err := s.GetAttr(ctx, &pb.GetAttrRequest{Path: fullPath})
		if err != nil {
			t.Errorf("GetAttr for %s failed: %v", file, err)
			continue
		}
		if !resp.Found {
			t.Errorf("File %s not found after incremental indexing", file)
		}
	}

	// Verify new directory (pkg) exists
	resp, err := s.Lookup(ctx, &pb.LookupRequest{
		ParentPath: displayPath,
		Name:       "pkg",
	})
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if !resp.Found {
		t.Error("pkg directory not found after incremental indexing")
	}

	t.Log("✓ Incremental indexing works correctly")
}
