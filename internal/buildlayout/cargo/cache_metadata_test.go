package cargo

import (
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

// TestCreateCargoCacheMetadata verifies Cargo cache metadata generation
func TestCreateCargoCacheMetadata(t *testing.T) {
	crateName := "serde"
	version := "1.0.152"

	files := []buildlayout.FileInfo{
		{
			Path: "Cargo.toml",
			Content: []byte(`[package]
name = "serde"
version = "1.0.152"
edition = "2021"

[dependencies]
serde_derive = "1.0"
`),
		},
		{Path: "src/lib.rs"},
		{Path: "src/de/mod.rs"},
	}

	info := buildlayout.RepoInfo{
		DisplayPath:   "cargo:serde@1.0.152",
		StorageID:     "test-serde-storage",
		CommitTime:    "2023-12-01T14:30:00Z",
		IngestionType: "cargo",
		Ref:           "v1.0.152",
	}

	entries := createCargoCacheMetadata(crateName, version, files, info)

	// Should generate: index entry + Cargo.toml + .cargo-checksum.json
	if len(entries) != 3 {
		t.Fatalf("Expected 3 cache metadata entries, got %d", len(entries))
	}

	// Verify paths
	foundIndex := false
	foundCargoToml := false
	foundChecksum := false

	for _, entry := range entries {
		if entry.VirtualDisplayPath == ".cargo/registry/index" && entry.VirtualFilePath == crateName {
			foundIndex = true
			// Verify index content
			indexStr := string(entry.SyntheticContent)
			if !strings.Contains(indexStr, `"name": "serde"`) {
				t.Error("Index entry missing crate name")
			}
			if !strings.Contains(indexStr, `"vers": "1.0.152"`) {
				t.Error("Index entry missing version")
			}
		}

		if entry.VirtualDisplayPath == ".cargo/registry/cache/serde-1.0.152" && entry.VirtualFilePath == "Cargo.toml" {
			foundCargoToml = true
			// Verify Cargo.toml content
			if !strings.Contains(string(entry.SyntheticContent), "serde") {
				t.Error("Cargo.toml missing crate name")
			}
		}

		if entry.VirtualDisplayPath == ".cargo/registry/cache/serde-1.0.152" && entry.VirtualFilePath == ".cargo-checksum.json" {
			foundChecksum = true
			// Verify checksum content
			checksumStr := string(entry.SyntheticContent)
			if !strings.Contains(checksumStr, `"package"`) {
				t.Error("Checksum file missing package field")
			}
			if !strings.Contains(checksumStr, "monofs-serde-1.0.152") {
				t.Error("Checksum file has wrong package identifier")
			}
		}
	}

	if !foundIndex {
		t.Error("Index entry not found")
	}
	if !foundCargoToml {
		t.Error("Cargo.toml cache entry not found")
	}
	if !foundChecksum {
		t.Error("Checksum file not found")
	}
}

// TestCreateCargoCacheMetadata_MissingCargoToml verifies fallback when Cargo.toml is missing
func TestCreateCargoCacheMetadata_MissingCargoToml(t *testing.T) {
	crateName := "tokio"
	version := "1.35.0"

	// Files without Cargo.toml
	files := []buildlayout.FileInfo{
		{Path: "src/lib.rs"},
		{Path: "src/runtime/mod.rs"},
	}

	info := buildlayout.RepoInfo{
		DisplayPath:   "cargo:tokio@1.35.0",
		StorageID:     "test-tokio-storage",
		CommitTime:    "2024-01-10T08:00:00Z",
		IngestionType: "cargo",
		Ref:           "v1.35.0",
	}

	entries := createCargoCacheMetadata(crateName, version, files, info)

	if len(entries) != 3 {
		t.Fatalf("Expected 3 cache metadata entries, got %d", len(entries))
	}

	// Find Cargo.toml
	var cargoToml []byte
	for _, entry := range entries {
		if entry.VirtualFilePath == "Cargo.toml" {
			cargoToml = entry.SyntheticContent
			break
		}
	}

	if cargoToml == nil {
		t.Fatal("Cargo.toml not generated")
	}

	// Verify generated Cargo.toml has correct name and version
	tomlStr := string(cargoToml)
	if !strings.Contains(tomlStr, `name = "tokio"`) {
		t.Error("Generated Cargo.toml missing name")
	}
	if !strings.Contains(tomlStr, `version = "1.35.0"`) {
		t.Error("Generated Cargo.toml missing version")
	}
	if !strings.Contains(tomlStr, `edition = "2021"`) {
		t.Error("Generated Cargo.toml missing edition")
	}
}

// TestMapPaths_WithCargoCacheMetadata verifies MapPaths includes cache metadata
func TestMapPaths_WithCargoCacheMetadata(t *testing.T) {
	mapper := NewCargoMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "cargo:regex@1.10.2",
		StorageID:     "test-regex-storage",
		CommitTime:    "2023-10-15T16:45:00Z",
		IngestionType: "cargo",
		Ref:           "v1.10.2",
	}

	files := []buildlayout.FileInfo{
		{
			Path:    "Cargo.toml",
			Content: []byte(`[package]\nname = "regex"\nversion = "1.10.2"\n`),
		},
		{Path: "src/lib.rs"},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths failed: %v", err)
	}

	// Should have original files (.cargo/registry/src/) + cache metadata
	if len(entries) < 3 {
		t.Fatalf("Expected at least 3 entries, got %d", len(entries))
	}

	// Check for cache metadata entries
	cacheEntriesFound := 0
	for _, entry := range entries {
		if strings.Contains(entry.VirtualDisplayPath, ".cargo/registry/cache/") ||
			strings.Contains(entry.VirtualDisplayPath, ".cargo/registry/index") {
			cacheEntriesFound++
		}
	}

	if cacheEntriesFound != 3 {
		t.Errorf("Expected 3 cache metadata entries, found %d", cacheEntriesFound)
	}

	// Verify all cache entries have synthetic content
	for _, entry := range entries {
		if strings.Contains(entry.VirtualDisplayPath, ".cargo/registry/cache/") ||
			strings.Contains(entry.VirtualDisplayPath, ".cargo/registry/index") {
			if len(entry.SyntheticContent) == 0 {
				t.Errorf("Cache entry %s has no synthetic content", entry.VirtualFilePath)
			}
		}
	}
}

// TestNormalizeGitURL verifies Git URL normalization
func TestNormalizeGitURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://github.com/serde-rs/serde", "github.com/serde-rs/serde"},
		{"https://github.com/serde-rs/serde.git", "github.com/serde-rs/serde"},
		{"git+https://github.com/tokio-rs/tokio", "github.com/tokio-rs/tokio"},
		{"git@github.com:rust-lang/regex.git", "github.com/rust-lang/regex"},
		{"ssh://git@gitlab.com/group/project", "gitlab.com/group/project"},
		{"", ""},
		{"invalid url", ""},
	}

	for _, tt := range tests {
		result := normalizeGitURL(tt.input)
		if result != tt.expected {
			t.Errorf("normalizeGitURL(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// TestParseDependencyFile verifies Cargo.toml dependency parsing
func TestParseDependencyFile(t *testing.T) {
	mapper := NewCargoMapper()

	cargoToml := `[package]
name = "my-project"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = "1.0"
tokio = { version = "1.35", features = ["full"] }
regex = "1.10.2"

[dev-dependencies]
criterion = "0.5"
`

	deps, err := mapper.ParseDependencyFile([]byte(cargoToml))
	if err != nil {
		t.Fatalf("ParseDependencyFile failed: %v", err)
	}

	if len(deps) != 4 {
		t.Fatalf("Expected 4 dependencies, got %d", len(deps))
	}

	// Verify each dependency
	expectedDeps := map[string]string{
		"serde":     "1.0",
		"tokio":     "1.35",
		"regex":     "1.10.2",
		"criterion": "0.5",
	}

	for _, dep := range deps {
		expectedVersion, exists := expectedDeps[dep.Module]
		if !exists {
			t.Errorf("Unexpected dependency: %s", dep.Module)
		}
		if dep.Version != expectedVersion {
			t.Errorf("Dependency %s version = %s, want %s", dep.Module, dep.Version, expectedVersion)
		}
	}
}
