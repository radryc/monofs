package gazelle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoGeneratorCanGenerate(t *testing.T) {
	g := &GoGenerator{}

	// Dir with go.mod
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.21\n"), 0644)
	if !g.CanGenerate(context.Background(), dir) {
		t.Error("GoGenerator should detect go.mod")
	}

	// Dir without go.mod
	emptyDir := t.TempDir()
	if g.CanGenerate(context.Background(), emptyDir) {
		t.Error("GoGenerator should not claim empty dir")
	}
}

func TestGoGeneratorSimple(t *testing.T) {
	g := &GoGenerator{}
	dir := t.TempDir()

	// Create go.mod
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/example/app\n\ngo 1.21\n"), 0644)

	// Create a Go source file
	os.MkdirAll(filepath.Join(dir, "pkg", "auth"), 0755)
	os.WriteFile(filepath.Join(dir, "pkg", "auth", "auth.go"), []byte("package auth\n\nfunc Foo() {}\n"), 0644)

	// Create a .go file at repo root so it gets a BUILD.bazel too
	os.WriteFile(filepath.Join(dir, "root.go"), []byte("package app\n\nfunc Version() string { return \"1.0\" }\n"), 0644)

	// Create a main package
	os.MkdirAll(filepath.Join(dir, "cmd", "server"), 0755)
	os.WriteFile(filepath.Join(dir, "cmd", "server", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	// Create a test file
	os.WriteFile(filepath.Join(dir, "pkg", "auth", "auth_test.go"), []byte("package auth\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {}\n"), 0644)

	opts := GenerateOptions{
		RepoDir:    dir,
		ModulePath: "github.com/example/app",
		Force:      true,
	}

	result, err := g.Generate(context.Background(), opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Should have generated 3 BUILD files: root, pkg/auth, cmd/server
	if result.FilesGenerated < 3 {
		t.Errorf("expected at least 3 files generated, got %d", result.FilesGenerated)
	}

	// Verify generated BUILD file for the library package
	buildPath := filepath.Join(dir, "pkg", "auth", "BUILD.bazel")
	content, err := os.ReadFile(buildPath)
	if err != nil {
		t.Fatalf("read BUILD.bazel: %v", err)
	}
	c := string(content)

	if !strings.Contains(c, "go_library") {
		t.Error("BUILD.bazel missing go_library")
	}
	if !strings.Contains(c, `"auth.go"`) {
		t.Error("BUILD.bazel missing auth.go src")
	}
	if !strings.Contains(c, "go_test") {
		t.Error("BUILD.bazel missing go_test for auth_test.go")
	}
	if !strings.Contains(c, "embed") {
		t.Error("go_test missing embed")
	}
	if !strings.Contains(c, "DO NOT EDIT") {
		t.Error("BUILD.bazel missing DO NOT EDIT header")
	}
	if !strings.Contains(c, "importpath") {
		t.Error("go_library missing importpath")
	}

	// Verify main binary BUILD file
	mainBuildPath := filepath.Join(dir, "cmd", "server", "BUILD.bazel")
	mainContent, err := os.ReadFile(mainBuildPath)
	if err != nil {
		t.Fatalf("read cmd BUILD.bazel: %v", err)
	}
	mc := string(mainContent)
	if !strings.Contains(mc, "go_binary") {
		t.Error("cmd BUILD.bazel should use go_binary")
	}
	if strings.Contains(mc, "importpath") {
		t.Error("go_binary should NOT have importpath")
	}
}

func TestGoGeneratorDryRun(t *testing.T) {
	g := &GoGenerator{}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	opts := GenerateOptions{
		RepoDir:    dir,
		ModulePath: "example.com/test",
		DryRun:     true,
		Force:      true,
	}

	result, err := g.Generate(context.Background(), opts)
	if err != nil {
		t.Fatalf("Generate dry-run: %v", err)
	}

	if result.FilesGenerated == 0 {
		t.Error("dry-run should report FilesGenerated > 0")
	}

	// File should NOT exist on disk.
	if _, err := os.Stat(filepath.Join(dir, "BUILD.bazel")); err == nil {
		t.Error("dry-run should not write files")
	}
}

func TestGoGeneratorManualMarker(t *testing.T) {
	g := &GoGenerator{}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	// Pre-create a BUILD.bazel with manual marker.
	existingContent := "# monofs: manual\nload(\"@rules_go//go:def.bzl\", \"go_binary\")\ngo_binary(name=\"myserver\", srcs=[\"main.go\"])\n"
	os.WriteFile(filepath.Join(dir, "BUILD.bazel"), []byte(existingContent), 0644)

	opts := GenerateOptions{
		RepoDir:    dir,
		ModulePath: "example.com/test",
		Force:      false,
	}

	result, err := g.Generate(context.Background(), opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if result.FilesGenerated > 0 {
		t.Error("should skip file with manual marker")
	}
	if result.FilesSkipped == 0 {
		t.Error("should report skipped files")
	}

	// File should be unchanged.
	content, _ := os.ReadFile(filepath.Join(dir, "BUILD.bazel"))
	if string(content) != existingContent {
		t.Error("manual file was modified")
	}
}

func TestGoGeneratorForce(t *testing.T) {
	g := &GoGenerator{}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	// Pre-create a BUILD.bazel with manual marker.
	os.WriteFile(filepath.Join(dir, "BUILD.bazel"), []byte("# monofs: manual\n# old\n"), 0644)

	opts := GenerateOptions{
		RepoDir:    dir,
		ModulePath: "example.com/test",
		Force:      true,
	}

	_, err := g.Generate(context.Background(), opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// File should be overwritten with generated content.
	content, _ := os.ReadFile(filepath.Join(dir, "BUILD.bazel"))
	// The generated header mentions "monofs: manual" as a directive for users,
	// but the actual "# monofs: manual" marker line we wrote earlier should be gone.
	if !strings.Contains(string(content), "DO NOT EDIT") {
		t.Error("generated file missing DO NOT EDIT header")
	}
	if strings.Contains(string(content), "# old") {
		t.Error("force should overwrite old content")
	}
}

func TestDetectModulePath(t *testing.T) {
	dir := t.TempDir()

	// No go.mod
	if mp := DetectModulePath(dir); mp != "" {
		t.Errorf("empty dir: got %q, want empty", mp)
	}

	// With go.mod
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/org/repo\n\ngo 1.21\n"), 0644)
	if mp := DetectModulePath(dir); mp != "github.com/org/repo" {
		t.Errorf("got %q, want github.com/org/repo", mp)
	}

	// With multiline go.mod
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("// comment\nmodule  github.com/org/v2  \n\ngo 1.21\n"), 0644)
	if mp := DetectModulePath(dir); mp != "github.com/org/v2" {
		t.Errorf("got %q, want github.com/org/v2", mp)
	}
}

func TestDetectLanguage(t *testing.T) {
	dir := t.TempDir()

	// Unknown
	if lang := DetectLanguage(dir); lang != "unknown" {
		t.Errorf("empty dir: got %q, want unknown", lang)
	}

	// Go
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0644)
	if lang := DetectLanguage(dir); lang != "go" {
		t.Errorf("go.mod: got %q, want go", lang)
	}

	// TypeScript (go.mod removed, package.json added)
	os.Remove(filepath.Join(dir, "go.mod"))
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)
	if lang := DetectLanguage(dir); lang != "typescript" {
		t.Errorf("package.json: got %q, want typescript", lang)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()

	// Should find GoGenerator for a dir with go.mod.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0644)
	gen := r.FindGenerator(context.Background(), dir)
	if gen == nil {
		t.Fatal("expected GoGenerator for go.mod dir")
	}
	if gen.Name() != "go" {
		t.Errorf("generator name: got %q, want go", gen.Name())
	}

	// Should return nil for empty dir.
	emptyDir := t.TempDir()
	if gen := r.FindGenerator(context.Background(), emptyDir); gen != nil {
		t.Errorf("expected nil for empty dir, got %s", gen.Name())
	}
}

func TestGenerateRepo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package app\n\nfunc Foo() int { return 42 }\n"), 0644)

	reg := NewRegistry()
	opts := GenerateOptions{Force: true}
	result, err := GenerateRepo(context.Background(), reg, dir, opts)
	if err != nil {
		t.Fatalf("GenerateRepo: %v", err)
	}
	if result.FilesGenerated == 0 {
		t.Error("expected at least 1 file generated")
	}
}

func TestGoGeneratorEmptyRepo(t *testing.T) {
	g := &GoGenerator{}
	dir := t.TempDir()

	// Dir with go.mod but no Go files.
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/empty\n\ngo 1.21\n"), 0644)

	opts := GenerateOptions{
		RepoDir:    dir,
		ModulePath: "example.com/empty",
	}
	result, err := g.Generate(context.Background(), opts)
	if err != nil {
		t.Fatalf("Generate empty repo: %v", err)
	}
	if result.FilesGenerated > 0 {
		t.Error("should not generate any files for empty Go repo")
	}
}

func TestGoGeneratorVendorSkip(t *testing.T) {
	g := &GoGenerator{}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.21\n"), 0644)
	os.MkdirAll(filepath.Join(dir, "vendor", "some-lib"), 0755)
	os.WriteFile(filepath.Join(dir, "vendor", "some-lib", "lib.go"), []byte("package somelib\n"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644)

	opts := GenerateOptions{RepoDir: dir, ModulePath: "example.com/test", Force: true}
	result, err := g.Generate(context.Background(), opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Should only generate one BUILD.bazel (root), not vendor/
	if result.FilesGenerated != 1 {
		t.Errorf("expected 1 file generated (skipping vendor/), got %d", result.FilesGenerated)
	}
}
