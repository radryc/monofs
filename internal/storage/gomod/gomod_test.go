package gomod

import (
	"context"
	"os"
	"testing"

	"github.com/radryc/monofs/internal/storage"
)

func TestGoModIngestionBackend(t *testing.T) {
	// Skip if in CI or no network
	if os.Getenv("CI") != "" || os.Getenv("SKIP_NETWORK_TESTS") != "" {
		t.Skip("Skipping network-dependent test")
	}

	backend := NewGoModIngestionBackend()

	// Test with a small, stable Go module
	sourceURL := "github.com/google/uuid@v1.3.0"
	config := map[string]string{
		"cache_dir": t.TempDir(),
	}

	ctx := context.Background()

	// Test Validate
	t.Run("Validate", func(t *testing.T) {
		err := backend.Validate(ctx, sourceURL, config)
		if err != nil {
			t.Fatalf("Validate failed: %v", err)
		}
	})

	// Test Initialize
	t.Run("Initialize", func(t *testing.T) {
		err := backend.Initialize(ctx, sourceURL, config)
		if err != nil {
			t.Fatalf("Initialize failed: %v", err)
		}
	})

	// Test WalkFiles
	t.Run("WalkFiles", func(t *testing.T) {
		fileCount := 0
		err := backend.WalkFiles(ctx, func(meta storage.FileMetadata) error {
			fileCount++

			// Verify metadata
			if meta.Path == "" {
				t.Error("Empty path in metadata")
			}
			if meta.Size == 0 {
				t.Logf("Warning: zero size for %s", meta.Path)
			}
			if meta.ContentHash == "" {
				t.Error("Empty content hash in metadata")
			}
			if meta.Metadata["module"] != "github.com/google/uuid" {
				t.Errorf("Expected module github.com/google/uuid, got %s", meta.Metadata["module"])
			}
			if meta.Metadata["version"] != "v1.3.0" {
				t.Errorf("Expected version v1.3.0, got %s", meta.Metadata["version"])
			}

			return nil
		})
		if err != nil {
			t.Fatalf("WalkFiles failed: %v", err)
		}
		if fileCount == 0 {
			t.Error("No files found")
		}
		t.Logf("Found %d files in module", fileCount)
	})

	// Test GetMetadata
	t.Run("GetMetadata", func(t *testing.T) {
		// Try to get metadata for a known file in the uuid module
		meta, err := backend.GetMetadata(ctx, "uuid.go")
		if err != nil {
			t.Fatalf("GetMetadata failed: %v", err)
		}
		if meta.Path != "uuid.go" {
			t.Errorf("Expected path uuid.go, got %s", meta.Path)
		}
		if meta.ContentHash == "" {
			t.Error("Empty content hash")
		}
	})

	// Test Cleanup
	t.Run("Cleanup", func(t *testing.T) {
		err := backend.Cleanup()
		if err != nil {
			t.Fatalf("Cleanup failed: %v", err)
		}
	})
}

func TestGoModBackendTypes(t *testing.T) {
	ingestion := NewGoModIngestionBackend()

	if ingestion.Type() != storage.IngestionTypeGo {
		t.Errorf("Expected ingestion type %s, got %s", storage.IngestionTypeGo, ingestion.Type())
	}
}

func TestInvalidModuleURL(t *testing.T) {
	backend := NewGoModIngestionBackend()
	ctx := context.Background()
	config := map[string]string{
		"cache_dir": t.TempDir(),
	}

	// Test with invalid URL format
	err := backend.Initialize(ctx, "invalid-format", config)
	if err == nil {
		t.Error("Expected error for invalid module URL format")
	}

	// Test with non-existent module
	err = backend.Validate(ctx, "github.com/nonexistent/module@v99.99.99", config)
	if err == nil {
		t.Error("Expected error for non-existent module")
	}
}
