package golang

import (
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

func TestCreateCacheMetadata(t *testing.T) {
	modulePath := "github.com/google/uuid"
	encodedModule := "github.com/google/uuid" // No uppercase, no encoding needed
	version := "v1.6.0"

	// Sample files including go.mod
	files := []buildlayout.FileInfo{
		{
			Path:     "go.mod",
			BlobHash: "hash1",
			Size:     100,
			Content:  []byte("module github.com/google/uuid\n\ngo 1.21\n"),
		},
		{
			Path:     "uuid.go",
			BlobHash: "hash2",
			Size:     1000,
		},
	}

	info := buildlayout.RepoInfo{
		DisplayPath: "github.com/google/uuid@v1.6.0",
		CommitTime:  "2024-01-15T10:30:00Z",
	}

	entries := createCacheMetadata(modulePath, encodedModule, version, files, info)

	// Should create 3 cache metadata files
	if len(entries) != 3 {
		t.Fatalf("expected 3 cache metadata entries, got %d", len(entries))
	}

	// Verify cache directory path
	expectedCachePath := "cache/download/github.com/google/uuid/@v"
	for i, entry := range entries {
		if entry.VirtualDisplayPath != expectedCachePath {
			t.Errorf("entry %d: expected cache path %q, got %q",
				i, expectedCachePath, entry.VirtualDisplayPath)
		}
	}

	// Verify .info file
	infoEntry := entries[0]
	if infoEntry.VirtualFilePath != "v1.6.0.info" {
		t.Errorf("expected .info file, got %q", infoEntry.VirtualFilePath)
	}
	if !strings.Contains(string(infoEntry.SyntheticContent), `"Version":"v1.6.0"`) {
		t.Errorf(".info content missing version: %s", infoEntry.SyntheticContent)
	}
	if !strings.Contains(string(infoEntry.SyntheticContent), `"Time":"2024-01-15T10:30:00Z"`) {
		t.Errorf(".info content missing time: %s", infoEntry.SyntheticContent)
	}

	// Verify .mod file
	modEntry := entries[1]
	if modEntry.VirtualFilePath != "v1.6.0.mod" {
		t.Errorf("expected .mod file, got %q", modEntry.VirtualFilePath)
	}
	if string(modEntry.SyntheticContent) != string(files[0].Content) {
		t.Errorf(".mod content doesn't match go.mod:\nexpected: %s\ngot: %s",
			files[0].Content, modEntry.SyntheticContent)
	}

	// Verify list file
	listEntry := entries[2]
	if listEntry.VirtualFilePath != "list" {
		t.Errorf("expected list file, got %q", listEntry.VirtualFilePath)
	}
	if string(listEntry.SyntheticContent) != "v1.6.0\n" {
		t.Errorf("list content incorrect: %q", listEntry.SyntheticContent)
	}
}

func TestCreateCacheMetadata_MissingGoMod(t *testing.T) {
	modulePath := "github.com/example/nogomod"
	encodedModule := "github.com/example/nogomod"
	version := "v1.0.0"

	// No go.mod file
	files := []buildlayout.FileInfo{
		{Path: "main.go", BlobHash: "hash1", Size: 100},
	}

	info := buildlayout.RepoInfo{
		DisplayPath: "github.com/example/nogomod@v1.0.0",
		CommitTime:  "2024-01-01T00:00:00Z",
	}

	entries := createCacheMetadata(modulePath, encodedModule, version, files, info)

	// Should still create 3 files
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// .mod should have minimal go.mod
	modEntry := entries[1]
	modContent := string(modEntry.SyntheticContent)
	if !strings.Contains(modContent, "module github.com/example/nogomod") {
		t.Errorf("minimal go.mod missing module directive: %s", modContent)
	}
	if !strings.Contains(modContent, "go 1.24") {
		t.Errorf("minimal go.mod missing go version: %s", modContent)
	}
}

func TestCreateCacheMetadata_CaseEncoding(t *testing.T) {
	modulePath := "github.com/Azure/go-autorest"
	encodedModule := EncodePath(modulePath) // Should be "github.com/!azure/go-autorest"
	version := "v0.11.29"

	files := []buildlayout.FileInfo{
		{
			Path:    "go.mod",
			Content: []byte("module github.com/Azure/go-autorest\n"),
		},
	}

	info := buildlayout.RepoInfo{
		DisplayPath: "github.com/Azure/go-autorest@v0.11.29",
		CommitTime:  "2024-01-01T00:00:00Z",
	}

	entries := createCacheMetadata(modulePath, encodedModule, version, files, info)

	// Cache path should use encoded module path
	expectedCachePath := "cache/download/github.com/!azure/go-autorest/@v"
	if entries[0].VirtualDisplayPath != expectedCachePath {
		t.Errorf("expected encoded cache path %q, got %q",
			expectedCachePath, entries[0].VirtualDisplayPath)
	}
}

func TestMapPaths_WithCacheMetadata(t *testing.T) {
	mapper := NewGoMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/google/uuid@v1.6.0",
		StorageID:     "storage123",
		Source:        "github.com/google/uuid@v1.6.0",
		Ref:           "v1.6.0",
		CommitTime:    "2024-01-15T10:30:00Z",
		IngestionType: "go",
		FetchType:     "gomod",
	}

	files := []buildlayout.FileInfo{
		{
			Path:     "go.mod",
			BlobHash: "hash1",
			Size:     100,
			Content:  []byte("module github.com/google/uuid\n\ngo 1.21\n"),
		},
		{
			Path:     "uuid.go",
			BlobHash: "hash2",
			Size:     5000,
		},
		{
			Path:     "version.go",
			BlobHash: "hash3",
			Size:     200,
		},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths failed: %v", err)
	}

	// Should create:
	// - 3 regular file entries (go.mod, uuid.go, version.go)
	// - 3 cache metadata entries (.info, .mod, list)
	// Total: 6 entries
	if len(entries) != 6 {
		t.Fatalf("expected 6 entries (3 files + 3 cache metadata), got %d", len(entries))
	}

	// Count entries by type
	regularCount := 0
	cacheCount := 0

	for _, entry := range entries {
		if strings.HasPrefix(entry.VirtualDisplayPath, "cache/download/") {
			cacheCount++
			// Cache metadata should have synthetic content
			if len(entry.SyntheticContent) == 0 {
				t.Errorf("cache metadata entry %q missing synthetic content",
					entry.VirtualFilePath)
			}
		} else {
			regularCount++
			// Regular files should reference original
			if entry.OriginalFilePath == "" {
				t.Errorf("regular file entry %q missing original path",
					entry.VirtualFilePath)
			}
		}
	}

	if regularCount != 3 {
		t.Errorf("expected 3 regular file entries, got %d", regularCount)
	}
	if cacheCount != 3 {
		t.Errorf("expected 3 cache metadata entries, got %d", cacheCount)
	}
}
