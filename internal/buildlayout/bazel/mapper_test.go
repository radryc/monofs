package bazel

import (
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

func TestBazelMapper_Type(t *testing.T) {
	mapper := NewBazelMapper()
	if got := mapper.Type(); got != "bazel" {
		t.Errorf("Type() = %v, want %v", got, "bazel")
	}
}

func TestBazelMapper_Matches(t *testing.T) {
	mapper := NewBazelMapper()

	tests := []struct {
		name        string
		info        buildlayout.RepoInfo
		wantMatches bool
	}{
		{
			name: "matches git ingestion type (Bazel repos are git)",
			info: buildlayout.RepoInfo{
				IngestionType: "git",
			},
			wantMatches: true,
		},
		{
			name: "does not match npm ingestion type",
			info: buildlayout.RepoInfo{
				IngestionType: "npm",
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

func TestBazelMapper_MapPaths_RulesGoModule(t *testing.T) {
	mapper := NewBazelMapper()

	// Simulate real rules_go@0.42.0 Bazel module structure
	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/bazelbuild/rules_go",
		StorageID:     "test-storage-id",
		Source:        "https://github.com/bazelbuild/rules_go",
		Ref:           "0.42.0",
		IngestionType: "git",
		FetchType:     "git",
	}

	files := []buildlayout.FileInfo{
		{Path: "MODULE.bazel", BlobHash: "hash1", Size: 500, Mode: 0644},
		{Path: "BUILD.bazel", BlobHash: "hash2", Size: 1000, Mode: 0644},
		{Path: "go/def.bzl", BlobHash: "hash3", Size: 5000, Mode: 0644},
		{Path: "go/private/rules/library.bzl", BlobHash: "hash4", Size: 8000, Mode: 0644},
		{Path: "go/private/rules/binary.bzl", BlobHash: "hash5", Size: 7000, Mode: 0644},
		{Path: "README.rst", BlobHash: "hash6", Size: 3000, Mode: 0644},
		{Path: "LICENSE", BlobHash: "hash7", Size: 2000, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	if len(entries) != len(files) {
		t.Errorf("MapPaths() returned %d entries, want %d", len(entries), len(files))
	}

	// Verify Bazel repository path format: bazel-repos/rules_go@0.42.0
	expectedVirtualPath := "bazel-repos/rules_go@0.42.0"
	if len(entries) > 0 && entries[0].VirtualDisplayPath != expectedVirtualPath {
		t.Errorf("VirtualDisplayPath = %v, want %v", entries[0].VirtualDisplayPath, expectedVirtualPath)
	}
}

func TestBazelMapper_MapPaths_GazelleModule(t *testing.T) {
	mapper := NewBazelMapper()

	// Simulate real gazelle@0.33.0 Bazel module
	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/bazelbuild/bazel-gazelle",
		StorageID:     "test-storage-id",
		Source:        "https://github.com/bazelbuild/bazel-gazelle",
		Ref:           "0.33.0",
		IngestionType: "git",
		FetchType:     "git",
	}

	files := []buildlayout.FileInfo{
		{Path: "MODULE.bazel", BlobHash: "hash1", Size: 400, Mode: 0644},
		{Path: "BUILD.bazel", BlobHash: "hash2", Size: 800, Mode: 0644},
		{Path: "def.bzl", BlobHash: "hash3", Size: 3000, Mode: 0644},
		{Path: "internal/gazelle/generator.go", BlobHash: "hash4", Size: 10000, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	expectedVirtualPath := "bazel-repos/bazel-gazelle@0.33.0"
	if len(entries) > 0 && entries[0].VirtualDisplayPath != expectedVirtualPath {
		t.Errorf("VirtualDisplayPath = %v, want %v", entries[0].VirtualDisplayPath, expectedVirtualPath)
	}
}

func TestBazelMapper_MapPaths_WorkspaceFile(t *testing.T) {
	mapper := NewBazelMapper()

	// Simulate legacy Bazel repository with WORKSPACE file (no MODULE.bazel)
	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/example/legacy",
		StorageID:     "test-storage-id",
		Source:        "https://github.com/example/legacy",
		Ref:           "main",
		IngestionType: "git",
		FetchType:     "git",
	}

	files := []buildlayout.FileInfo{
		{Path: "WORKSPACE", BlobHash: "hash1", Size: 5000, Mode: 0644},
		{Path: "BUILD", BlobHash: "hash2", Size: 1000, Mode: 0644},
		{Path: "src/main/java/BUILD", BlobHash: "hash3", Size: 500, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	if len(entries) != len(files) {
		t.Errorf("MapPaths() returned %d entries, want %d", len(entries), len(files))
	}

	// Should still create virtual path even with WORKSPACE instead of MODULE.bazel
	if len(entries) > 0 {
		if !containsPrefix(entries[0].VirtualDisplayPath, "bazel-repos/") {
			t.Errorf("VirtualDisplayPath = %v, expected to start with bazel-repos/", entries[0].VirtualDisplayPath)
		}
	}
}

func TestBazelMapper_MapPaths_NoBazelFiles(t *testing.T) {
	mapper := NewBazelMapper()

	info := buildlayout.RepoInfo{
		DisplayPath:   "github.com/example/not-bazel",
		IngestionType: "git",
	}

	// Files without MODULE.bazel or WORKSPACE
	files := []buildlayout.FileInfo{
		{Path: "README.md", BlobHash: "hash1", Size: 100, Mode: 0644},
		{Path: "src/main.go", BlobHash: "hash2", Size: 500, Mode: 0644},
	}

	entries, err := mapper.MapPaths(info, files)
	if err != nil {
		t.Fatalf("MapPaths() error = %v", err)
	}

	// Should return nil when no Bazel files are found
	if entries != nil {
		t.Errorf("MapPaths() returned entries for non-Bazel repository, want nil")
	}
}

func TestBazelMapper_ParseDependencyFile_ModuleBazel(t *testing.T) {
	mapper := NewBazelMapper()

	// Real MODULE.bazel from a Bazel project
	content := []byte(`module(
    name = "my-project",
    version = "1.0.0",
)

bazel_dep(name = "rules_go", version = "0.42.0")
bazel_dep(name = "gazelle", version = "0.33.0")
bazel_dep(name = "protobuf", version = "21.7")
bazel_dep(name = "rules_proto", version = "5.3.0-21.7")

# Development only
bazel_dep(name = "buildifier_prebuilt", version = "6.3.3", dev_dependency = True)`)

	deps, err := mapper.ParseDependencyFile(content)
	if err != nil {
		t.Fatalf("ParseDependencyFile() error = %v", err)
	}

	// Should parse all bazel_dep declarations (including dev dependencies)
	if len(deps) < 4 {
		t.Errorf("ParseDependencyFile() returned %d dependencies, want at least 4", len(deps))
	}

	// Verify dependency parsing
	depMap := make(map[string]buildlayout.Dependency)
	for _, dep := range deps {
		depMap[dep.Module] = dep
	}

	// Check rules_go
	if dep, ok := depMap["rules_go"]; ok {
		if dep.Version != "0.42.0" {
			t.Errorf("rules_go version = %v, want 0.42.0", dep.Version)
		}
		if dep.Source != "rules_go@0.42.0" {
			t.Errorf("rules_go source = %v, want rules_go@0.42.0", dep.Source)
		}
	} else {
		t.Error("rules_go dependency not found")
	}

	// Check gazelle
	if dep, ok := depMap["gazelle"]; ok {
		if dep.Version != "0.33.0" {
			t.Errorf("gazelle version = %v, want 0.33.0", dep.Version)
		}
	} else {
		t.Error("gazelle dependency not found")
	}
}

// Helper function
func containsPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
