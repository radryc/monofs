package server

import (
	"context"
	"path/filepath"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/metastore"
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
	index := readDirEntries(t, s, storageID, dirPath)
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

// TestDeleteRepositoryScopedToRepo verifies repository deletion uses
// prefix-scoped scans and leaves other repositories' keys untouched.
func TestDeleteRepositoryScopedToRepo(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := NewServer("test-node", "localhost:9000",
		filepath.Join(tmpDir, "test.db"), filepath.Join(tmpDir, "git"), false, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()

	ingest := func(storageID, display, file string) {
		t.Helper()
		if _, err := s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
			StorageId: storageID, DisplayPath: display, Source: "src",
		}); err != nil {
			t.Fatalf("RegisterRepository(%s): %v", storageID, err)
		}
		if _, err := s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
			StorageId: storageID, DisplayPath: display, Source: "src",
			Files: []*pb.FileMetadata{{Path: file, Mode: 0644, Size: 5, Mtime: 1, BlobHash: "h"}},
		}); err != nil {
			t.Fatalf("IngestFileBatch(%s): %v", storageID, err)
		}
	}
	ingest("repo-a", "repo-a", "a/x.go")
	ingest("repo-b", "repo-b", "b/y.go")

	if _, err := s.DeleteRepository(context.Background(), &pb.DeleteRepositoryOnNodeRequest{
		StorageId: "repo-a",
	}); err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}

	countPrefix := func(bucket, prefix string) int {
		t.Helper()
		n := 0
		if err := s.db.View(func(tx metastore.Tx) error {
			keys, err := prefixScanKeys(tx, bucket, []byte(prefix))
			n = len(keys)
			return err
		}); err != nil {
			t.Fatalf("scan %s/%s: %v", bucket, prefix, err)
		}
		return n
	}

	if got := countPrefix(bucketOwnedFiles, "repo-a:"); got != 0 {
		t.Fatalf("repo-a owned files remain after delete: %d", got)
	}
	if got := countDirEntries(t, s, "repo-a", ""); got != 0 {
		t.Fatalf("repo-a root dir entries remain after delete: %d", got)
	}
	if got := countDirEntries(t, s, "repo-a", "a"); got != 0 {
		t.Fatalf("repo-a/a dir entries remain after delete: %d", got)
	}
	if got := countPrefix(bucketOwnedFiles, "repo-b:"); got != 1 {
		t.Fatalf("repo-b owned files = %d, want 1", got)
	}
	if got := countDirEntries(t, s, "repo-b", "b"); got == 0 {
		t.Fatal("repo-b dir entries were removed by repo-a delete")
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
	if err := s.db.View(func(tx metastore.Tx) error {
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

// TestReadDirPagination verifies resumable name-ordered pages over the
// per-entry directory index.
func TestReadDirPagination(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := NewServer("test-node", "localhost:9000",
		filepath.Join(tmpDir, "test.db"), filepath.Join(tmpDir, "git"), false, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()

	storageID := "page-storage"
	displayPath := "page-repo"
	if _, err := s.RegisterRepository(context.Background(), &pb.RegisterRepositoryRequest{
		StorageId: storageID, DisplayPath: displayPath, Source: "src",
	}); err != nil {
		t.Fatalf("RegisterRepository: %v", err)
	}

	names := []string{"a.go", "b.go", "c.go", "d.go", "e.go"}
	files := make([]*pb.FileMetadata, 0, len(names))
	for _, n := range names {
		files = append(files, &pb.FileMetadata{Path: "dir/" + n, Mode: 0644, Size: 1, Mtime: 1, BlobHash: n})
	}
	if _, err := s.IngestFileBatch(context.Background(), &pb.IngestFileBatchRequest{
		StorageId: storageID, DisplayPath: displayPath, Source: "src", Files: files,
	}); err != nil {
		t.Fatalf("IngestFileBatch: %v", err)
	}

	readPage := func(startAfter string, limit int) []string {
		t.Helper()
		stream := &mockReadDirStream{}
		if err := s.ReadDir(&pb.ReadDirRequest{
			Path:       displayPath + "/dir",
			StartAfter: startAfter,
			Limit:      int32(limit),
		}, stream); err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		out := make([]string, 0, len(stream.entries))
		for _, e := range stream.entries {
			out = append(out, e.Name)
		}
		return out
	}

	var got []string
	page := readPage("", 2)
	got = append(got, page...)
	for len(page) == 2 {
		page = readPage(page[len(page)-1], 2)
		got = append(got, page...)
	}

	if len(got) != len(names) {
		t.Fatalf("paged entries = %v, want %v", got, names)
	}
	for i := range names {
		if got[i] != names[i] {
			t.Fatalf("paged entries = %v, want %v", got, names)
		}
	}
}
