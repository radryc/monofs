package npm

import (
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

func TestNpmMapper_Type(t *testing.T) {
	mapper := NewNpmMapper()
	if got := mapper.Type(); got != "npm" {
		t.Errorf("Type() = %v, want %v", got, "npm")
	}
}

func TestNpmMapper_Matches(t *testing.T) {
	mapper := NewNpmMapper()

	tests := []struct {
		name        string
		info        buildlayout.RepoInfo
		wantMatches bool
	}{
		{
			name: "matches npm ingestion type",
			info: buildlayout.RepoInfo{
				IngestionType: "npm",
			},
			wantMatches: true,
		},
		{
			name: "does not match git ingestion type",
			info: buildlayout.RepoInfo{
				IngestionType: "git",
			},
			wantMatches: false,
		},
		{
			name: "does not match maven ingestion type",
			info: buildlayout.RepoInfo{
				IngestionType: "maven",
			},
			wantMatches: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapper.Matches(tt.info); got != tt.wantMatches {
				t.Errorf("Matches() = %v, want %v", got, tt.wantMatches)
			}
		})
	}
}

func TestNpmMapper_MapPaths_RealLodashStructure(t *testing.T) {
	mapper := NewNpmMapper()

	// Simulate real lodash@4.17.21 file structure
	info := buildlayout.RepoInfo{
		DisplayPath:   "lodash@4.17.21",
		StorageID:     "test-storage-id",
		Source:        "lodash@4.17.21",
		Ref:           "4.17.21",
		IngestionType: "npm",
		FetchType:     "npm",
	}

	files := []buildlayout.FileInfo{
		{Path: "package.json", BlobHash: "hash1", Size: 5000, Mode: 0644},
		{Path: "index.js", BlobHash: "hash2", Size: 100, Mode: 0644},
		{Path: "lodash.js", BlobHash: "hash3", Size: 544320, Mode: 0644},
		{Path: "lodash.min.js", BlobHash: "hash4", Size: 73638, Mode: 0644},
		{Path: "array.js", BlobHash: "hash5", Size: 500, Mode: 0644},
		{Path: "collection.js", BlobHash: "hash6", Size: 600, Mode: 0644},
		{Path: "README.md", BlobHash: "hash7", Size: 2000, Mode: 0644},
		{Path: "LICENSE", BlobHash: "hash8", Size: 1500, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// With hard links, we get 2x entries: canonical path + node_modules/ path
	expectedEntries := len(files) * 2
	if len(entries) != expectedEntries {
		t.Errorf("MapPaths() returned %d entries, want %d (2x for hard links)", len(entries), expectedEntries)
	}

	// Verify we have both canonical and node_modules paths
	hasCanonical := false
	hasNodeModules := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.VirtualDisplayPath, "registry.npmjs.org/") {
			hasCanonical = true
		}
		if strings.HasPrefix(entry.VirtualDisplayPath, "node_modules/") {
			hasNodeModules = true
		}
	}
	if !hasCanonical {
		t.Error("Missing canonical path entries (registry.npmjs.org/)")
	}
	if !hasNodeModules {
		t.Error("Missing node_modules/ hard link entries")
	}

	// Build a map for efficient lookup - expect 2x entries (hard links)
	fileMap := make(map[string]buildlayout.VirtualEntry)
	for _, entry := range entries {
		fileMap[entry.OriginalFilePath] = entry
	}

	// Verify each file has entries for both paths
	for _, file := range files {
		entry, exists := fileMap[file.Path]
		if !exists {
			t.Errorf("Missing virtual entry for file: %s", file.Path)
			continue
		}

		if entry.VirtualFilePath != file.Path {
			t.Errorf("VirtualFilePath = %v, want %v", entry.VirtualFilePath, file.Path)
		}
		if entry.OriginalFilePath != file.Path {
			t.Errorf("OriginalFilePath = %v, want %v", entry.OriginalFilePath, file.Path)
		}
	}
}

func TestNpmMapper_MapPaths_ScopedPackage(t *testing.T) {
	mapper := NewNpmMapper()

	// Simulate real @types/node package structure
	info := buildlayout.RepoInfo{
		DisplayPath:   "@types/node@18.0.0",
		StorageID:     "test-storage-id",
		Source:        "@types/node@18.0.0",
		Ref:           "18.0.0",
		IngestionType: "npm",
		FetchType:     "npm",
	}

	files := []buildlayout.FileInfo{
		{Path: "package.json", BlobHash: "hash1", Size: 3000, Mode: 0644},
		{Path: "index.d.ts", BlobHash: "hash2", Size: 50000, Mode: 0644},
		{Path: "fs.d.ts", BlobHash: "hash3", Size: 20000, Mode: 0644},
		{Path: "http.d.ts", BlobHash: "hash4", Size: 15000, Mode: 0644},
		{Path: "README.md", BlobHash: "hash5", Size: 1000, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// With hard links, we get 2x entries: canonical path + node_modules/ path
	expectedEntries := len(files) * 2
	if len(entries) != expectedEntries {
		t.Errorf("MapPaths() returned %d entries, want %d (2x for hard links)", len(entries), expectedEntries)
	}

	// Verify we have both canonical and node_modules paths with correct format
	hasCanonical := false
	hasNodeModules := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.VirtualDisplayPath, "registry.npmjs.org/") {
			hasCanonical = true
		}
		if entry.VirtualDisplayPath == "node_modules/@types/node@18.0.0" {
			hasNodeModules = true
		}
	}
	if !hasCanonical {
		t.Error("Missing canonical path entries")
	}
	if !hasNodeModules {
		t.Error("Missing or incorrect node_modules/ path for scoped package")
	}
}

func TestNpmMapper_MapPaths_NoPackageJson(t *testing.T) {
	mapper := NewNpmMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "not-npm@1.0.0",
		IngestionType: "npm",
	}

	// Files without package.json
	files := []buildlayout.FileInfo{
		{Path: "README.md", BlobHash: "hash1", Size: 100, Mode: 0644},
		{Path: "index.js", BlobHash: "hash2", Size: 100, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// Should return nil when no package.json is found
	if entries != nil {
		t.Errorf("MapPaths() returned entries for non-npm package, want nil")
	}
}

func TestNpmMapper_ParseDependencyFile_RealPackageJson(t *testing.T) {
	mapper := NewNpmMapper()

	// Real package.json content from a typical Express.js app
	content := []byte(`{
  "name": "my-express-app",
  "version": "1.0.0",
  "description": "Express API server",
  "main": "index.js",
  "scripts": {
    "start": "node index.js",
    "test": "jest"
  },
  "dependencies": {
    "express": "^4.18.2",
    "body-parser": "^1.20.0",
    "cors": "~2.8.5",
    "dotenv": ">=16.0.0",
    "mongoose": "7.0.3",
    "jsonwebtoken": "*"
  },
  "devDependencies": {
    "jest": "^29.5.0",
    "nodemon": "~2.0.20",
    "eslint": ">=8.0.0"
  }
}`)

	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		t.Fatalf("ParseDependencyFile() error = %v", err)
	}

	// Should parse all 9 dependencies (6 regular + 3 dev)
	expectedCount := 9
	if len(deps) != expectedCount {
		t.Errorf("ParseDependencyFile() returned %d dependencies, want %d", len(deps), expectedCount)
	}

	// Verify version cleaning
	depMap := make(map[string]buildlayout.Dependency)
	for _, dep := range deps {
		depMap[dep.Module] = dep
	}

	// Check that version specifiers are cleaned
	if dep, ok := depMap["express"]; ok {
		if dep.Version != "4.18.2" {
			t.Errorf("express version = %v, want 4.18.2 (^ should be stripped)", dep.Version)
		}
		if dep.Source != "express@4.18.2" {
			t.Errorf("express source = %v, want express@4.18.2", dep.Source)
		}
	} else {
		t.Error("express dependency not found")
	}

	if dep, ok := depMap["mongoose"]; ok {
		if dep.Version != "7.0.3" {
			t.Errorf("mongoose version = %v, want 7.0.3", dep.Version)
		}
	} else {
		t.Error("mongoose dependency not found")
	}

	// Check wildcard handling
	if dep, ok := depMap["jsonwebtoken"]; ok {
		if dep.Version != "latest" {
			t.Errorf("jsonwebtoken version = %v, want latest (* should become latest)", dep.Version)
		}
	}
}
