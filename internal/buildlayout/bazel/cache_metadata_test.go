package bazel

import (
	"strings"
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

// TestCreateBazelCacheMetadata verifies Bazel cache metadata generation
func TestCreateBazelCacheMetadata(t *testing.T) {
	repoName := "rules_go"

	files := []buildlayout.FileInfo{
		{
			Path: "MODULE.bazel",
			Content: []byte(`module(
    name = "rules_go",
    version = "0.44.0",
)
`),
		},
		{
			Path: "WORKSPACE",
			Content: []byte(`workspace(name = "rules_go")

# Workspace content
`),
		},
		{Path: "go/def.bzl"},
		{Path: "go/private/rules/binary.bzl"},
	}

	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/bazelbuild/rules_go",
		StorageID:     "test-rules-go-storage",
		CommitTime:    "2024-01-05T11:20:00Z",
		IngestionType: "git",
		Ref:           "v0.44.0",
	}

	entries := createBazelCacheMetadata(repoName, files, info)

	// Should generate: WORKSPACE + MODULE.bazel + repo.bzl + BUILD.monofs
	if len(entries) != 4 {
		t.Fatalf("Expected 4 cache metadata entries, got %d", len(entries))
	}

	expectedCachePath := "bazel-cache/repos/rules_go"

	// Verify each file
	fileMap := make(map[string][]byte)
	for _, entry := range entries {
		if entry.VirtualDisplayPath != expectedCachePath {
			t.Errorf("Expected cache path %s, got %s", expectedCachePath, entry.VirtualDisplayPath)
		}
		fileMap[entry.VirtualFilePath] = entry.SyntheticContent
	}

	// 1. WORKSPACE should exist
	if workspace, exists := fileMap["WORKSPACE"]; !exists {
		t.Error("WORKSPACE not found in cache metadata")
	} else if !strings.Contains(string(workspace), "rules_go") {
		t.Error("WORKSPACE does not contain repo name")
	}

	// 2. MODULE.bazel should exist
	if moduleBazel, exists := fileMap["MODULE.bazel"]; !exists {
		t.Error("MODULE.bazel not found in cache metadata")
	} else if !strings.Contains(string(moduleBazel), "rules_go") {
		t.Error("MODULE.bazel does not contain repo name")
	}

	// 3. repo.bzl should exist with repository marker
	if repoBzl, exists := fileMap["repo.bzl"]; !exists {
		t.Error("repo.bzl not found in cache metadata")
	} else {
		bzlStr := string(repoBzl)
		if !strings.Contains(bzlStr, "monofs_repo") {
			t.Error("repo.bzl missing repository rule")
		}
		if !strings.Contains(bzlStr, "rules_go") {
			t.Error("repo.bzl does not reference repo name")
		}
	}

	// 4. BUILD.monofs should exist
	if buildFile, exists := fileMap["BUILD.monofs"]; !exists {
		t.Error("BUILD.monofs not found in cache metadata")
	} else if !strings.Contains(string(buildFile), "exports_files") {
		t.Error("BUILD.monofs missing exports_files")
	}
}

// TestCreateBazelCacheMetadata_MissingFiles verifies fallback when WORKSPACE/MODULE.bazel missing
func TestCreateBazelCacheMetadata_MissingFiles(t *testing.T) {
	repoName := "rules_python"

	// Files without WORKSPACE or MODULE.bazel
	files := []buildlayout.FileInfo{
		{Path: "python/defs.bzl"},
		{Path: "python/pip.bzl"},
	}

	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/bazelbuild/rules_python",
		StorageID:     "test-rules-python-storage",
		CommitTime:    "2024-02-10T09:00:00Z",
		IngestionType: "git",
		Ref:           "v0.29.0",
	}

	entries := createBazelCacheMetadata(repoName, files, info)

	if len(entries) != 4 {
		t.Fatalf("Expected 4 cache metadata entries, got %d", len(entries))
	}

	// Find WORKSPACE and MODULE.bazel
	var workspace []byte
	var moduleBazel []byte

	for _, entry := range entries {
		if entry.VirtualFilePath == "WORKSPACE" {
			workspace = entry.SyntheticContent
		} else if entry.VirtualFilePath == "MODULE.bazel" {
			moduleBazel = entry.SyntheticContent
		}
	}

	if workspace == nil {
		t.Fatal("WORKSPACE not generated")
	}
	if moduleBazel == nil {
		t.Fatal("MODULE.bazel not generated")
	}

	// Verify generated files have correct content
	wsStr := string(workspace)
	if !strings.Contains(wsStr, `workspace(name = "rules_python")`) {
		t.Error("Generated WORKSPACE missing workspace declaration")
	}

	modStr := string(moduleBazel)
	if !strings.Contains(modStr, `name = "rules_python"`) {
		t.Error("Generated MODULE.bazel missing module name")
	}
	if !strings.Contains(modStr, `version = "v0.29.0"`) {
		t.Error("Generated MODULE.bazel missing version")
	}
}

// TestMapPaths_WithBazelCacheMetadata verifies MapPaths includes cache metadata
func TestMapPaths_WithBazelCacheMetadata(t *testing.T) {
	mapper := NewBazelMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/bazelbuild/bazel-skylib",
		StorageID:     "test-skylib-storage",
		CommitTime:    "2023-11-20T14:00:00Z",
		IngestionType: "git",
		Ref:           "1.5.0",
	}

	files := []buildlayout.FileInfo{
		{
			Path:    "MODULE.bazel",
			Content: []byte(`module(name = "bazel-skylib", version = "1.5.0")`),
		},
		{Path: "lib/paths.bzl"},
		{Path: "lib/types.bzl"},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths failed: %v", err)
	}

	// Should have original files + cache metadata
	if len(entries) < 4 {
		t.Fatalf("Expected at least 4 entries (original + metadata), got %d", len(entries))
	}

	// Check for cache metadata entries
	cacheEntriesFound := 0
	for _, entry := range entries {
		if strings.Contains(entry.VirtualDisplayPath, "bazel-cache/repos/") {
			cacheEntriesFound++
		}
	}

	if cacheEntriesFound != 4 {
		t.Errorf("Expected 4 cache metadata entries, found %d", cacheEntriesFound)
	}

	// Verify all cache entries have synthetic content
	for _, entry := range entries {
		if strings.Contains(entry.VirtualDisplayPath, "bazel-cache/repos/") {
			if len(entry.SyntheticContent) == 0 {
				t.Errorf("Cache entry %s has no synthetic content", entry.VirtualFilePath)
			}
		}
	}
}

// TestMapPaths_NonBazelRepo verifies repos without Bazel files return nil
func TestMapPaths_NonBazelRepo(t *testing.T) {
	mapper := NewBazelMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/user/regular-repo",
		StorageID:     "test-regular-repo",
		IngestionType: "git",
	}

	// Files without any Bazel build files
	files := []buildlayout.FileInfo{
		{Path: "README.md"},
		{Path: "src/main.go"},
		{Path: "go.mod"},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths failed: %v", err)
	}

	// Should return nil for non-Bazel repos
	if entries != nil {
		t.Errorf("Expected nil entries for non-Bazel repo, got %d entries", len(entries))
	}
}

// TestParseDependencyFile verifies MODULE.bazel parsing
func TestParseDependencyFile(t *testing.T) {
	mapper := NewBazelMapper()

	moduleBazel := `module(
    name = "my_project",
    version = "1.0.0",
)

bazel_dep(name = "rules_go", version = "0.44.0")
bazel_dep(name = "gazelle", version = "0.35.0")
bazel_dep(name = "rules_python", version = "0.29.0")

# Other declarations
go_deps = use_extension("@rules_go//extensions:go_deps.bzl", "go_deps")
`

	deps, err := mapper.ParseDependencyFile([]byte(moduleBazel))
	if err != nil {
		t.Fatalf("ParseDependencyFile failed: %v", err)
	}

	if len(deps) != 3 {
		t.Fatalf("Expected 3 dependencies, got %d", len(deps))
	}

	// Verify each dependency
	expectedDeps := map[string]string{
		"rules_go":     "0.44.0",
		"gazelle":      "0.35.0",
		"rules_python": "0.29.0",
	}

	for _, dep := range deps {
		expectedVersion, exists := expectedDeps[dep.Module]
		if !exists {
			t.Errorf("Unexpected dependency: %s", dep.Module)
		}
		if dep.Version != expectedVersion {
			t.Errorf("Dependency %s version = %s, want %s", dep.Module, dep.Version, expectedVersion)
		}
		if dep.Source != dep.Module+"@"+dep.Version {
			t.Errorf("Dependency %s has incorrect Source: %s", dep.Module, dep.Source)
		}
	}
}
