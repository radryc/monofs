package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateFallbackGenrule(t *testing.T) {
	spec := GenruleSpec{
		Name:        "build",
		Command:     "make build",
		DisplayPath: "sre/my-repo",
	}

	content := GenerateFallbackGenrule(spec)

	// Must contain the genrule.
	if !strings.Contains(content, "genrule(") {
		t.Error("missing genrule(")
	}
	if !strings.Contains(content, `name = "build"`) {
		t.Error("missing name")
	}
	if !strings.Contains(content, "native build fallback") {
		t.Error("missing fallback header")
	}
	if !strings.Contains(content, "sre/my-repo") {
		t.Error("missing display_path")
	}
	if !strings.Contains(content, "monofs-native-build") {
		t.Error("missing native-build tool reference")
	}
	if !strings.Contains(content, "no-remote-exec") {
		t.Error("missing no-remote-exec tag")
	}
}

func TestGenerateFallbackTestGenrule(t *testing.T) {
	spec := GenruleSpec{
		Name:        "test",
		Command:     "go test ./...",
		DisplayPath: "sre/go-lib",
	}

	content := GenerateFallbackGenrule(spec)

	if !strings.Contains(content, `name = "test"`) {
		t.Error("missing test name")
	}
	if !strings.Contains(content, `"test.done"`) {
		t.Error("missing test.done output")
	}
	if !strings.Contains(content, "test") {
		// the action should be "test"
	}
}

func TestWriteFallbackGenrules(t *testing.T) {
	dir := t.TempDir()

	// Setup: mount root with two repos.
	repoDir := filepath.Join(dir, "sre", "legacy")
	os.MkdirAll(repoDir, 0755)
	os.WriteFile(filepath.Join(repoDir, "Makefile"), []byte("all:\n"), 0644)

	repos := []RepoStatus{
		{DisplayPath: "sre/legacy", State: StateNative, BuildSystem: "make"},
		{DisplayPath: "sre/active", State: StateActive, BuildSystem: "go"},
	}

	written, err := WriteFallbackGenrules(dir, repos)
	if err != nil {
		t.Fatalf("WriteFallbackGenrules: %v", err)
	}

	// Only legacy should get a fallback (one BUILD.bazel with both build+test genrules).
	if len(written) != 1 {
		t.Fatalf("expected 1 genrule file written, got %d: %v", len(written), written)
	}

	// Active repo should NOT have a BUILD.bazel.
	activeBuild := filepath.Join(dir, "sre", "active", "BUILD.bazel")
	if _, err := os.Stat(activeBuild); err == nil {
		t.Error("active repo should not have fallback genrule")
	}

	// Legacy repo should have a BUILD.bazel with fallback.
	legacyBuild := filepath.Join(dir, "sre", "legacy", "BUILD.bazel")
	data, err := os.ReadFile(legacyBuild)
	if err != nil {
		t.Fatalf("read legacy BUILD.bazel: %v", err)
	}
	if !strings.Contains(string(data), "native build fallback") {
		t.Error("legacy BUILD.bazel missing fallback header")
	}
	if !strings.Contains(string(data), `name = "build"`) {
		t.Error("missing build genrule")
	}
	if !strings.Contains(string(data), `name = "test"`) {
		t.Error("missing test genrule")
	}
}

func TestRemoveFallbackGenrules(t *testing.T) {
	dir := t.TempDir()

	// Create a repo with a fallback genrule.
	repoDir := filepath.Join(dir, "sre", "legacy")
	os.MkdirAll(repoDir, 0755)
	spec := GenruleSpec{Name: "build", Command: "make build", DisplayPath: "sre/legacy"}
	os.WriteFile(filepath.Join(repoDir, "BUILD.bazel"), []byte(GenerateFallbackGenrule(spec)), 0644)

	repos := []RepoStatus{
		{DisplayPath: "sre/legacy", State: StateActive, BuildSystem: "go"},
	}

	removed, err := RemoveFallbackGenrules(dir, repos)
	if err != nil {
		t.Fatalf("RemoveFallbackGenrules: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed, got %d", len(removed))
	}

	if _, err := os.Stat(filepath.Join(repoDir, "BUILD.bazel")); err == nil {
		t.Error("fallback file should be removed")
	}
}

func TestRemoveFallbackDoesNotTouchRealBUILD(t *testing.T) {
	dir := t.TempDir()

	// Create a repo with a real BUILD.bazel (not fallback).
	repoDir := filepath.Join(dir, "sre", "real")
	os.MkdirAll(repoDir, 0755)
	realContent := `load("@rules_go//go:def.bzl", "go_library")
go_library(name = "lib", srcs = ["lib.go"], importpath = "example.com/lib")
`
	os.WriteFile(filepath.Join(repoDir, "BUILD.bazel"), []byte(realContent), 0644)

	repos := []RepoStatus{
		{DisplayPath: "sre/real", State: StateActive, BuildSystem: "go"},
	}

	removed, err := RemoveFallbackGenrules(dir, repos)
	if err != nil {
		t.Fatalf("RemoveFallbackGenrules: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("should not remove real BUILD.bazel, got %v", removed)
	}

	// Content should be intact.
	data, _ := os.ReadFile(filepath.Join(repoDir, "BUILD.bazel"))
	if string(data) != realContent {
		t.Error("real BUILD.bazel was modified")
	}
}

func TestExtractAction(t *testing.T) {
	tests := []struct {
		cmd      string
		expected string
	}{
		{"make build", "build"},
		{"make test", "test"},
		{"go build ./...", "build"},
		{"go test ./...", "test"},
		{"npm run build", "run"},
		{"build", "build"},
	}
	for _, tt := range tests {
		got := extractAction(tt.cmd)
		if got != tt.expected {
			t.Errorf("extractAction(%q) = %q, want %q", tt.cmd, got, tt.expected)
		}
	}
}
