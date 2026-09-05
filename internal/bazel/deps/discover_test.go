package deps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/radryc/monofs/internal/bazel"
)

func manifestWithGoRepos() *bazel.WorkspaceManifest {
	return &bazel.WorkspaceManifest{
		Repositories: []bazel.ManifestRepository{
			{
				DisplayPath: "sre/platform-api",
				Source:      "https://github.com/org/platform-api.git",
				Included:    true,
			},
			{
				DisplayPath: "sre/infra-tooling",
				Source:      "https://github.com/org/infra-tooling.git",
				Included:    true,
			},
			{
				DisplayPath: "shared/proto-defs",
				Source:      "https://github.com/org/proto-defs.git",
				Included:    true,
			},
		},
	}
}

func TestDiscoverGoMod(t *testing.T) {
	dir := t.TempDir()
	gomod := `module github.com/org/platform-api

go 1.21

require (
	github.com/org/infra-tooling v1.4.0
	github.com/gorilla/mux v1.8.0
	github.com/org/proto-defs v2.0.0 // indirect
)
`
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644)

	d := NewDepDiscoverer(manifestWithGoRepos())
	deps, err := d.discoverGoMod(dir, "sre/platform-api")
	if err != nil {
		t.Fatalf("discoverGoMod: %v", err)
	}

	// Should find infra-tooling (cross-repo), but NOT gorilla/mux (external)
	// and NOT proto-defs (indirect).
	if len(deps) != 1 {
		t.Fatalf("expected 1 cross-repo dep, got %d: %v", len(deps), deps)
	}
	if deps[0].DisplayPath != "sre/infra-tooling" {
		t.Errorf("dep display_path: got %q, want sre/infra-tooling", deps[0].DisplayPath)
	}
	if deps[0].DetectedBy != "go.mod" {
		t.Errorf("detected_by: got %q", deps[0].DetectedBy)
	}
}

func TestDiscoverGoModSingleRequire(t *testing.T) {
	dir := t.TempDir()
	// Single-line require (not in a block).
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n\nrequire github.com/org/infra-tooling v1.0.0\n"), 0644)

	d := NewDepDiscoverer(manifestWithGoRepos())
	deps, err := d.discoverGoMod(dir, "sre/some-other")
	if err != nil {
		t.Fatalf("discoverGoMod: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
}

func TestDiscoverGoModSelfReference(t *testing.T) {
	dir := t.TempDir()
	// This repo requires itself (should not appear as cross-repo dep).
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/org/platform-api\n\ngo 1.21\n\nrequire github.com/org/platform-api v0.0.0\n"), 0644)

	d := NewDepDiscoverer(manifestWithGoRepos())
	deps, err := d.discoverGoMod(dir, "sre/platform-api")
	if err != nil {
		t.Fatalf("discoverGoMod: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps (self-ref), got %d", len(deps))
	}
}

func TestInferModulePath(t *testing.T) {
	tests := []struct {
		source   string
		expected string
	}{
		{"https://github.com/org/repo.git", "github.com/org/repo"},
		{"https://gitlab.com/org/repo.git", "gitlab.com/org/repo"},
		{"https://github.com/org/repo", "github.com/org/repo"},
		{"git@github.com:org/repo.git", "github.com/org/repo"},
		{"", ""},
	}
	for _, tt := range tests {
		got := inferModulePath(bazel.ManifestRepository{Source: tt.source})
		if got != tt.expected {
			t.Errorf("inferModulePath(%q) = %q, want %q", tt.source, got, tt.expected)
		}
	}
}

func TestDiscoverAllEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	d := NewDepDiscoverer(manifestWithGoRepos())
	deps, err := d.DiscoverAll(context.Background(), dir, "sre/nonexistent")
	// Should not error, just return empty.
	if err != nil {
		t.Fatalf("DiscoverAll: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps for empty dir, got %d", len(deps))
	}
}

func TestDiscoverGoModNoMatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module private.local/repo\n\ngo 1.21\n\nrequire external.com/lib v1.0.0\n"), 0644)

	d := NewDepDiscoverer(manifestWithGoRepos())
	deps, err := d.discoverGoMod(dir, "sre/private-repo")
	if err != nil {
		t.Fatalf("discoverGoMod: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 cross-repo deps (no match), got %d", len(deps))
	}
}
