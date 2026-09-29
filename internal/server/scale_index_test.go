package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/nutsdb/nutsdb"
	pb "github.com/radryc/monofs/api/proto"
)

// TestBatchIngestMaintainsDirectoryIndexWithoutRebuild verifies that batch
// ingestion updates the directory index for every touched directory, so the
// router no longer needs a repository-wide BuildDirectoryIndexes rebuild.
func TestBatchIngestMaintainsDirectoryIndexWithoutRebuild(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := NewServer("test-node", "localhost:9000",
		filepath.Join(tmpDir, "test.db"), filepath.Join(tmpDir, "git"), false, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()

	storageID := "scale-storage"
	displayPath := "scale-repo"
	if _, err := s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "https://example.com/repo.git",
	}); err != nil {
		t.Fatalf("RegisterRepository: %v", err)
	}

	files := []*pb.FileMetadata{
		{Path: "cmd/app/main.go", Mode: 0644, Size: 10, Mtime: 100, BlobHash: "h1"},
		{Path: "cmd/app/util.go", Mode: 0644, Size: 20, Mtime: 101, BlobHash: "h2"},
		{Path: "pkg/lib/lib.go", Mode: 0644, Size: 30, Mtime: 102, BlobHash: "h3"},
	}
	if _, err := s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: displayPath,
		Source:      "https://example.com/repo.git",
		Files:       files,
	}); err != nil {
		t.Fatalf("IngestFileBatch: %v", err)
	}

	// Intentionally no BuildDirectoryIndexes call here.
	assertDirIndexContains(t, s, storageID, "", []string{"cmd", "pkg"})
	assertDirIndexContains(t, s, storageID, "cmd", []string{"app"})
	assertDirIndexContains(t, s, storageID, "cmd/app", []string{"main.go", "util.go"})
	assertDirIndexContains(t, s, storageID, "pkg/lib", []string{"lib.go"})
}

func assertDirIndexContains(t *testing.T, s *Server, storageID, dirPath string, want []string) {
	t.Helper()
	var index []dirIndexEntry
	if err := s.db.View(func(tx *nutsdb.Tx) error {
		val, err := tx.Get(bucketDirIndex, makeDirIndexKey(storageID, dirPath))
		if err != nil {
			return err
		}
		return json.Unmarshal(val, &index)
	}); err != nil {
		t.Fatalf("dir index %q: %v", dirPath, err)
	}
	names := make(map[string]bool, len(index))
	for _, e := range index {
		names[e.Name] = true
	}
	for _, w := range want {
		if !names[w] {
			t.Fatalf("dir index %q missing %q; have %v", dirPath, w, names)
		}
	}
}

// TestUsageCountersPersistAcrossRestart verifies the O(1) startup path: usage
// counters are persisted after mutations and reloaded on the next start without
// rescanning the owned-files bucket.
func TestUsageCountersPersistAcrossRestart(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, false, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	storageID := "counter-storage"
	if _, err := s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId:   storageID,
		DisplayPath: "counter-repo",
		Source:      "src",
	}); err != nil {
		t.Fatalf("RegisterRepository: %v", err)
	}
	if _, err := s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId:   storageID,
		DisplayPath: "counter-repo",
		Source:      "src",
		Files: []*pb.FileMetadata{
			{Path: "a/1.go", Mode: 0644, Size: 11, Mtime: 1, BlobHash: "x"},
			{Path: "a/2.go", Mode: 0644, Size: 22, Mtime: 2, BlobHash: "y"},
		},
	}); err != nil {
		t.Fatalf("IngestFileBatch: %v", err)
	}

	wantFiles := s.totalFiles.Load()
	wantBytes := s.ownedBytes.Load()
	if wantFiles != 2 {
		t.Fatalf("totalFiles = %d, want 2", wantFiles)
	}
	if wantBytes != 33 {
		t.Fatalf("ownedBytes = %d, want 33", wantBytes)
	}

	// The counter record must be persisted, and the startup loader must return
	// it (found=true) so NewServer can skip the O(total files) scan.
	if err := s.db.View(func(tx *nutsdb.Tx) error {
		count, bytes_, found, err := loadUsageCounters(tx)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("persisted usage counter record not found")
		}
		if count != wantFiles || bytes_ != wantBytes {
			t.Fatalf("persisted counters = (files=%d, bytes=%d), want (%d, %d)",
				count, bytes_, wantFiles, wantBytes)
		}
		return nil
	}); err != nil {
		t.Fatalf("loadUsageCounters: %v", err)
	}
}
