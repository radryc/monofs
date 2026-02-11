package npm

import (
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

// TestCreateNpmCacheMetadata verifies npm cache metadata generation
func TestCreateNpmCacheMetadata(t *testing.T) {
	packageName := "express"
	version := "4.18.0"

	files := []buildlayout.FileInfo{
		{
			Path:    "package.json",
			Content: []byte(`{"name":"express","version":"4.18.0","description":"Fast, unopinionated, minimalist web framework"}`),
		},
		{Path: "index.js"},
		{Path: "lib/application.js"},
	}

	info := buildlayout.RepoInfo{
		DisplayPath:   "npm:express@4.18.0",
		StorageID:     "test-storage-id",
		CommitTime:    "2024-01-15T10:30:00Z",
		IngestionType: "npm",
	}

	entries := createNpmCacheMetadata(packageName, version, files, info)

	// Should generate 3 files: package.json, .npm-metadata.json, .package-lock.json
	if len(entries) != 3 {
		t.Fatalf("Expected 3 cache metadata entries, got %d", len(entries))
	}

	// Check cache path
	expectedCachePath := "npm-cache/express@4.18.0"
	for _, entry := range entries {
		if entry.VirtualDisplayPath != expectedCachePath {
			t.Errorf("Expected cache path %s, got %s", expectedCachePath, entry.VirtualDisplayPath)
		}
	}

	// Verify each file
	fileMap := make(map[string][]byte)
	for _, entry := range entries {
		fileMap[entry.VirtualFilePath] = entry.SyntheticContent
	}

	// 1. package.json should exist
	if pkgJSON, exists := fileMap["package.json"]; !exists {
		t.Error("package.json not found in cache metadata")
	} else if len(pkgJSON) == 0 {
		t.Error("package.json is empty")
	} else if !strings.Contains(string(pkgJSON), "express") {
		t.Error("package.json does not contain package name")
	}

	// 2. .npm-metadata.json should exist with proper structure
	if npmMeta, exists := fileMap[".npm-metadata.json"]; !exists {
		t.Error(".npm-metadata.json not found in cache metadata")
	} else if !strings.Contains(string(npmMeta), `"name": "express"`) {
		t.Error(".npm-metadata.json missing package name")
	} else if !strings.Contains(string(npmMeta), `"version": "4.18.0"`) {
		t.Error(".npm-metadata.json missing version")
	} else if !strings.Contains(string(npmMeta), `"time": "2024-01-15T10:30:00Z"`) {
		t.Error(".npm-metadata.json missing timestamp")
	}

	// 3. .package-lock.json should exist
	if lockFile, exists := fileMap[".package-lock.json"]; !exists {
		t.Error(".package-lock.json not found in cache metadata")
	} else if !strings.Contains(string(lockFile), `"lockfileVersion": 3`) {
		t.Error(".package-lock.json missing lockfileVersion")
	}
}

// TestCreateNpmCacheMetadata_MissingPackageJSON verifies fallback when package.json is missing
func TestCreateNpmCacheMetadata_MissingPackageJSON(t *testing.T) {
	packageName := "lodash"
	version := "4.17.21"

	// Files without package.json
	files := []buildlayout.FileInfo{
		{Path: "lodash.js"},
		{Path: "package/core.js"},
	}

	info := buildlayout.RepoInfo{
		DisplayPath:   "npm:lodash@4.17.21",
		StorageID:     "test-storage-id-2",
		CommitTime:    "2024-02-01T12:00:00Z",
		IngestionType: "npm",
	}

	entries := createNpmCacheMetadata(packageName, version, files, info)

	if len(entries) != 3 {
		t.Fatalf("Expected 3 cache metadata entries, got %d", len(entries))
	}

	// Find package.json
	var packageJSON []byte
	for _, entry := range entries {
		if entry.VirtualFilePath == "package.json" {
			packageJSON = entry.SyntheticContent
			break
		}
	}

	if packageJSON == nil {
		t.Fatal("package.json not generated")
	}

	// Verify generated package.json has correct name and version
	pkgStr := string(packageJSON)
	if !strings.Contains(pkgStr, `"name": "lodash"`) {
		t.Error("Generated package.json missing name")
	}
	if !strings.Contains(pkgStr, `"version": "4.17.21"`) {
		t.Error("Generated package.json missing version")
	}
}

// TestMapPaths_WithNpmCacheMetadata verifies MapPaths includes cache metadata
func TestMapPaths_WithNpmCacheMetadata(t *testing.T) {
	mapper := NewNpmMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "npm:react@18.2.0",
		StorageID:     "test-react-storage",
		CommitTime:    "2023-11-01T09:00:00Z",
		IngestionType: "npm",
		Ref:           "v18.2.0",
	}

	files := []buildlayout.FileInfo{
		{
			Path:    "package.json",
			Content: []byte(`{"name":"react","version":"18.2.0"}`),
		},
		{Path: "index.js"},
		{Path: "cjs/react.production.min.js"},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths failed: %v", err)
	}

	// Should have original files + cache metadata files
	if len(entries) < 3 {
		t.Fatalf("Expected at least 3 entries (original files), got %d", len(entries))
	}

	// Check for cache metadata entries
	cacheEntriesFound := 0
	for _, entry := range entries {
		if strings.Contains(entry.VirtualDisplayPath, "npm-cache/") {
			cacheEntriesFound++
		}
	}

	if cacheEntriesFound != 3 {
		t.Errorf("Expected 3 cache metadata entries, found %d", cacheEntriesFound)
	}

	// Verify cache metadata has synthetic content
	for _, entry := range entries {
		if strings.Contains(entry.VirtualDisplayPath, "npm-cache/") {
			if len(entry.SyntheticContent) == 0 {
				t.Errorf("Cache entry %s has no synthetic content", entry.VirtualFilePath)
			}
		}
	}
}

// TestScopedPackageName verifies scoped package names (e.g., @types/node)
func TestScopedPackageName(t *testing.T) {
	packageName := "@types/node"
	version := "20.10.0"

	files := []buildlayout.FileInfo{
		{
			Path:    "package.json",
			Content: []byte(`{"name":"@types/node","version":"20.10.0"}`),
		},
	}

	info := buildlayout.RepoInfo{
		DisplayPath:   "npm:@types/node@20.10.0",
		StorageID:     "test-types-node",
		CommitTime:    "2024-01-01T00:00:00Z",
		IngestionType: "npm",
	}

	entries := createNpmCacheMetadata(packageName, version, files, info)

	// Verify cache path handles scoped packages
	expectedCachePath := "npm-cache/@types/node@20.10.0"
	found := false
	for _, entry := range entries {
		if entry.VirtualDisplayPath == expectedCachePath {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Expected cache path %s not found", expectedCachePath)
		for _, entry := range entries {
			t.Logf("Found: %s", entry.VirtualDisplayPath)
		}
	}
}
