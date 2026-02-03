package server

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

// TestDeeplyNestedDirectoryLookup tests that deeply nested paths like
// internal/cortex/tenant work correctly
func TestDeeplyNestedDirectoryLookup(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-storage-deep"
	displayPath := "thanos"

	// Ingest a file deep in the hierarchy
	req := &pb.IngestFileRequest{
		Metadata: &pb.FileMetadata{
			Path:        "internal/cortex/tenant/resolver.go",
			DisplayPath: displayPath,
			StorageId:   storageID,
			Mode:        0644,
			Size:        100,
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

	ctx := context.Background()

	// Test each level of the directory hierarchy
	testCases := []struct {
		path string
		name string
	}{
		{displayPath, "internal"},
		{displayPath + "/internal", "cortex"},
		{displayPath + "/internal/cortex", "tenant"},
	}

	for _, tc := range testCases {
		t.Run("Lookup_"+tc.path+"/"+tc.name, func(t *testing.T) {
			lookupResp, err := s.Lookup(ctx, &pb.LookupRequest{
				ParentPath: tc.path,
				Name:       tc.name,
			})
			if err != nil {
				t.Fatalf("Lookup failed: %v", err)
			}
			if !lookupResp.Found {
				t.Errorf("Lookup for %s/%s returned not found", tc.path, tc.name)
			}
			if lookupResp.Mode&uint32(syscall.S_IFDIR) == 0 {
				t.Errorf("Expected directory mode, got 0%o", lookupResp.Mode)
			}
		})
	}

	// Test GetAttr for full paths
	attrTestCases := []string{
		displayPath + "/internal",
		displayPath + "/internal/cortex",
		displayPath + "/internal/cortex/tenant",
	}

	for _, fullPath := range attrTestCases {
		t.Run("GetAttr_"+fullPath, func(t *testing.T) {
			attrResp, err := s.GetAttr(ctx, &pb.GetAttrRequest{
				Path: fullPath,
			})
			if err != nil {
				t.Fatalf("GetAttr failed: %v", err)
			}
			if !attrResp.Found {
				t.Errorf("GetAttr for %s returned not found", fullPath)
			}
			if attrResp.Mode&uint32(syscall.S_IFDIR) == 0 {
				t.Errorf("Expected directory mode for %s, got 0%o", fullPath, attrResp.Mode)
			}
		})
	}
}

// TestEmptyIntermediateDirectory tests directories that exist but have no direct files
func TestEmptyIntermediateDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	gitCache := filepath.Join(tmpDir, "git")

	s, err := NewServer("test-node", "localhost:9000", dbPath, gitCache, nil)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer s.Close()

	storageID := "test-storage-empty"
	displayPath := "myrepo"

	// Ingest files that create intermediate directories with no direct files
	files := []string{
		"hooks/pre-commit/check.sh",
		"internal/pkg/util/helper.go",
	}

	for _, filePath := range files {
		req := &pb.IngestFileRequest{
			Metadata: &pb.FileMetadata{
				Path:        filePath,
				DisplayPath: displayPath,
				StorageId:   storageID,
				Mode:        0644,
				Size:        100,
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

	ctx := context.Background()

	// Test that intermediate directories exist even though they have no direct files
	testCases := []struct {
		path        string
		description string
	}{
		{displayPath + "/hooks", "hooks directory (no direct files)"},
		{displayPath + "/hooks/pre-commit", "hooks/pre-commit directory"},
		{displayPath + "/internal", "internal directory (no direct files)"},
		{displayPath + "/internal/pkg", "internal/pkg directory (no direct files)"},
		{displayPath + "/internal/pkg/util", "internal/pkg/util directory"},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			attrResp, err := s.GetAttr(ctx, &pb.GetAttrRequest{
				Path: tc.path,
			})
			if err != nil {
				t.Fatalf("GetAttr failed for %s: %v", tc.description, err)
			}
			if !attrResp.Found {
				t.Errorf("GetAttr for %s returned not found", tc.description)
			}
			if attrResp.Mode&uint32(syscall.S_IFDIR) == 0 {
				t.Errorf("Expected directory mode for %s, got 0%o", tc.description, attrResp.Mode)
			} else {
				t.Logf("✓ Found %s", tc.description)
			}
		})
	}
}
