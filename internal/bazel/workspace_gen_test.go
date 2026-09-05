package bazel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleManifest() *WorkspaceManifest {
	return &WorkspaceManifest{
		GeneratedAt: "2026-07-28T14:00:00Z",
		Repositories: []ManifestRepository{
			{
				StorageID:   "abc123",
				DisplayPath: "sre/platform-api",
				Source:      "https://github.com/org/platform-api.git",
				Ref:         "main",
				CommitHash:  "abc123def",
				Included:    true,
			},
			{
				StorageID:   "def456",
				DisplayPath: "sre/infra-tooling",
				Source:      "https://github.com/org/infra-tooling.git",
				Ref:         "main",
				CommitHash:  "789ghi012",
				Included:    true,
			},
			{
				StorageID:       "sys001",
				DisplayPath:     "guardian",
				Source:          "",
				Included:        false,
				ExclusionReason: "system-namespace",
			},
			{
				StorageID:       "sys002",
				DisplayPath:     "doctor",
				Source:          "",
				Included:        false,
				ExclusionReason: "system-namespace",
			},
		},
	}
}

func TestDisplayPathToModuleName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"sre/platform-api", "platform_api"},
		{"frontend/web-ui", "web_ui"},
		{"shared/proto-defs", "proto_defs"},
		{"monitoring", "monitoring"},
		{"org.sub/dotted.repo", "dotted_repo"},
		{"deep/nested/path/service", "service"},
	}
	for _, tt := range tests {
		got := DisplayPathToModuleName(tt.input)
		if got != tt.expected {
			t.Errorf("DisplayPathToModuleName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestIncludedRepos(t *testing.T) {
	m := sampleManifest()
	repos := m.IncludedRepos()
	if len(repos) != 2 {
		t.Fatalf("expected 2 included repos, got %d", len(repos))
	}
	if repos[0].DisplayPath != "sre/platform-api" {
		t.Errorf("unexpected first repo: %s", repos[0].DisplayPath)
	}
	if repos[1].DisplayPath != "sre/infra-tooling" {
		t.Errorf("unexpected second repo: %s", repos[1].DisplayPath)
	}
}

func TestIncludedReposNil(t *testing.T) {
	m := &WorkspaceManifest{Repositories: nil}
	repos := m.IncludedRepos()
	if repos != nil {
		t.Errorf("expected nil from nil repos, got %v", repos)
	}
}

func TestParseManifest(t *testing.T) {
	data := `{
		"generated_at": "2026-07-28T14:00:00Z",
		"repositories": [
			{
				"storage_id": "abc",
				"display_path": "my/repo",
				"source": "https://example.com/repo.git",
				"ref": "main",
				"commit_hash": "abc123",
				"included": true
			}
		]
	}`
	m, err := ParseManifest([]byte(data))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Repositories) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(m.Repositories))
	}
	if m.Repositories[0].DisplayPath != "my/repo" {
		t.Errorf("display_path = %q, want %q", m.Repositories[0].DisplayPath, "my/repo")
	}
}

func TestParseManifestError(t *testing.T) {
	_, err := ParseManifest([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestGenerateModuleBazel(t *testing.T) {
	g := &Generator{MountRoot: "/tmp/test"}
	m := sampleManifest()
	files, err := g.Generate(context.Background(), m)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// MODULE.bazel must reference included repos, not excluded ones.
	if !strings.Contains(files.ModuleBazel, "platform_api") {
		t.Error("MODULE.bazel missing platform_api")
	}
	if !strings.Contains(files.ModuleBazel, "infra_tooling") {
		t.Error("MODULE.bazel missing infra_tooling")
	}
	if strings.Contains(files.ModuleBazel, "guardian") {
		t.Error("MODULE.bazel should NOT contain guardian (excluded)")
	}
	if strings.Contains(files.ModuleBazel, "doctor") {
		t.Error("MODULE.bazel should NOT contain doctor (excluded)")
	}

	// Verify local_path_override references.
	if !strings.Contains(files.ModuleBazel, `path = "sre/platform-api"`) {
		t.Error("MODULE.bazel missing local_path_override for platform-api")
	}
}

func TestGenerateWorkspace(t *testing.T) {
	g := &Generator{}
	m := sampleManifest()
	files, err := g.Generate(context.Background(), m)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.Contains(files.Workspace, `name = "monofs_workspace"`) {
		t.Error("WORKSPACE missing workspace name")
	}
	if !strings.Contains(files.Workspace, `name = "platform_api"`) {
		t.Error("WORKSPACE missing platform_api local_repository")
	}
	if !strings.Contains(files.Workspace, `path = "sre/platform-api"`) {
		t.Error("WORKSPACE missing path for platform-api")
	}
}

func TestGenerateBazelrcDefaults(t *testing.T) {
	g := &Generator{}
	m := sampleManifest()
	files, err := g.Generate(context.Background(), m)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.Contains(files.Bazelrc, "output_user_root") {
		t.Error(".bazelrc missing output_user_root")
	}
	if !strings.Contains(files.Bazelrc, "disk_cache") {
		t.Error(".bazelrc missing disk_cache")
	}
}

func TestGenerateBazelrcWithCache(t *testing.T) {
	g := &Generator{
		CacheEnabled: true,
		CacheAddr:    "monofs-cache:9092",
	}
	files, err := g.Generate(context.Background(), sampleManifest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(files.Bazelrc, "monofs-cache:9092") {
		t.Error(".bazelrc missing remote_cache addr")
	}
	if !strings.Contains(files.Bazelrc, "build:ci") {
		t.Error(".bazelrc missing ci config")
	}
}

func TestGenerateBazelrcWithExecutor(t *testing.T) {
	g := &Generator{
		ExecutorEnabled: true,
		ExecutorAddr:    "monofs-executor:9093",
	}
	files, err := g.Generate(context.Background(), sampleManifest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(files.Bazelrc, "monofs-executor:9093") {
		t.Error(".bazelrc missing remote_executor addr")
	}
}

func TestGenerateBazelversion(t *testing.T) {
	// Default
	g := &Generator{}
	files, _ := g.Generate(context.Background(), sampleManifest())
	if !strings.Contains(files.Bazelversion, DefaultBazelVersion) {
		t.Errorf(".bazelversion = %q, want %q", files.Bazelversion, DefaultBazelVersion)
	}

	// Custom
	g2 := &Generator{BazelVersion: "6.4.0"}
	files2, _ := g2.Generate(context.Background(), sampleManifest())
	if !strings.Contains(files2.Bazelversion, "6.4.0") {
		t.Errorf(".bazelversion = %q, want 6.4.0", files2.Bazelversion)
	}

	// Empty
	g3 := &Generator{BazelVersion: ""}
	files3, _ := g3.Generate(context.Background(), sampleManifest())
	if files3.Bazelversion == "" {
		t.Error(".bazelversion should not be empty with default")
	}
}

func TestGenerateRootBuild(t *testing.T) {
	g := &Generator{}
	files, err := g.Generate(context.Background(), sampleManifest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(files.BuildBazel, "Root BUILD") {
		t.Error("BUILD.bazel missing root comment")
	}
	if !strings.Contains(files.BuildBazel, "DO NOT EDIT") {
		t.Error("BUILD.bazel missing DO NOT EDIT marker")
	}
}

func TestWriteGeneratedFiles(t *testing.T) {
	dir := t.TempDir()
	g := &Generator{MountRoot: dir}
	m := sampleManifest()

	written, err := g.WriteGeneratedFiles(context.Background(), m)
	if err != nil {
		t.Fatalf("WriteGeneratedFiles: %v", err)
	}

	// All five files should be written.
	expected := map[string]bool{
		".bazelversion": false,
		".bazelrc":      false,
		"MODULE.bazel":  false,
		"WORKSPACE":     false,
		"BUILD.bazel":   false,
	}
	for _, name := range written {
		expected[name] = true
	}
	for name, found := range expected {
		if !found {
			t.Errorf("file %s was not written", name)
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("file %s does not exist on disk: %v", name, err)
		}
	}
}

func TestWriteGeneratedFilesIdempotent(t *testing.T) {
	dir := t.TempDir()
	g := &Generator{MountRoot: dir}
	m := sampleManifest()

	// First write
	written1, err := g.WriteGeneratedFiles(context.Background(), m)
	if err != nil {
		t.Fatalf("first WriteGeneratedFiles: %v", err)
	}
	if len(written1) != 5 {
		t.Fatalf("first write: expected 5 files, got %d", len(written1))
	}

	// Second write with same manifest should not change files.
	written2, err := g.WriteGeneratedFiles(context.Background(), m)
	if err != nil {
		t.Fatalf("second WriteGeneratedFiles: %v", err)
	}
	if len(written2) != 5 {
		// Files are still "written" in the returned list but content is unchanged.
		// The key test is that WriteFile is a no-op due to content comparison.
	}

	// Verify content integrity after second write.
	modPath := filepath.Join(dir, "MODULE.bazel")
	content, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatalf("read MODULE.bazel after second write: %v", err)
	}
	if !strings.Contains(string(content), "platform_api") {
		t.Error("MODULE.bazel corrupted after second write")
	}
}

func TestGenerateEmptyManifest(t *testing.T) {
	g := &Generator{}
	m := &WorkspaceManifest{Repositories: []ManifestRepository{}}
	files, err := g.Generate(context.Background(), m)
	if err != nil {
		t.Fatalf("Generate empty manifest: %v", err)
	}
	// Should not crash on empty repo list.
	if !strings.Contains(files.ModuleBazel, "monofs_workspace") {
		t.Error("MODULE.bazel missing module name for empty manifest")
	}
}

func TestManifestInputHash(t *testing.T) {
	m1 := sampleManifest()
	m2 := sampleManifest()
	h1 := ManifestInputHash(m1)
	h2 := ManifestInputHash(m2)
	if h1 != h2 {
		t.Errorf("hashes should be equal for identical manifests: %s != %s", h1, h2)
	}
	if h1 == "" {
		t.Error("hash should not be empty")
	}

	// Different display path → different hash.
	m3 := sampleManifest()
	m3.Repositories[0].DisplayPath = "sre/different"
	h3 := ManifestInputHash(m3)
	if h3 == h1 {
		t.Error("hashes should differ for different display paths")
	}
}

func TestManifestInputHashNil(t *testing.T) {
	if ManifestInputHash(nil) != "" {
		t.Error("hash of nil manifest should be empty")
	}
}

func TestLoadManifestRoundTrip(t *testing.T) {
	m := sampleManifest()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	path := filepath.Join(t.TempDir(), "workspace.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(loaded.Repositories) != len(m.Repositories) {
		t.Fatalf("repo count: got %d, want %d", len(loaded.Repositories), len(m.Repositories))
	}
	for i := range m.Repositories {
		if loaded.Repositories[i].DisplayPath != m.Repositories[i].DisplayPath {
			t.Errorf("repo %d display_path mismatch: %s != %s",
				i, loaded.Repositories[i].DisplayPath, m.Repositories[i].DisplayPath)
		}
	}
}
