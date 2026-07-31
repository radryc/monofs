// Package gazelle provides BUILD file generation for MonoFS ingested
// repositories. It wraps Bazel Gazelle with multi-repo awareness so
// that cross-repo Go imports resolve to @repo_name//... targets instead
// of external module references.
package gazelle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/radryc/monofs/internal/bazel"
	"github.com/radryc/monofs/internal/bazel/deps"
)

// GenerateOptions controls BUILD file generation for a single repo.
type GenerateOptions struct {
	// RepoDir is the absolute or workspace-relative path to the repo root.
	RepoDir string

	// ModulePath is the Go module path detected from go.mod (e.g. "github.com/org/repo").
	ModulePath string

	// WorkspaceManifest is the full workspace manifest for cross-repo resolution.
	// When nil, cross-repo imports fall back to external Gazelle resolution.
	WorkspaceManifest *bazel.WorkspaceManifest

	// Resolver is an optional cross-repo import resolver. When set, it is
	// passed to generators for Go import resolution.
	Resolver *deps.ImportResolver

	// Force overwrites existing BUILD files even if they have a manual marker.
	Force bool

	// DryRun generates content but does not write files.
	DryRun bool

	// Verbose enables detailed logging.
	Verbose bool
}

// GenerateResult reports what happened during generation.
type GenerateResult struct {
	// FilesGenerated is the number of BUILD.bazel files written.
	FilesGenerated int

	// FilesSkipped is the number of packages that already had manual BUILD files.
	FilesSkipped int

	// Errors is a list of non-fatal errors encountered (e.g. Gazelle parse
	// warnings on individual packages).
	Errors []string

	// WrittenFiles lists the paths of files that were written (relative to RepoDir).
	WrittenFiles []string
}

// Generator can generate BUILD files for a particular language.
type Generator interface {
	// Name returns a human-readable generator identifier (e.g. "go", "typescript").
	Name() string

	// CanGenerate reports whether this generator can handle the repo at repoDir.
	CanGenerate(ctx context.Context, repoDir string) bool

	// Generate runs BUILD file generation for the repo.
	Generate(ctx context.Context, opts GenerateOptions) (*GenerateResult, error)
}

// Registry holds the set of registered generators.
type Registry struct {
	generators []Generator
}

// NewRegistry creates a Registry with the built-in generators registered.
func NewRegistry() *Registry {
	r := &Registry{}
	r.Register(&GoGenerator{})
	return r
}

// Register adds a generator. Generators registered later take priority
// for CanGenerate checks (LIFO order for detection).
func (r *Registry) Register(g Generator) {
	r.generators = append(r.generators, g)
}

// FindGenerator returns the first registered generator that can handle
// the repo at repoDir, or nil if none can.
func (r *Registry) FindGenerator(ctx context.Context, repoDir string) Generator {
	for i := len(r.generators) - 1; i >= 0; i-- {
		if r.generators[i].CanGenerate(ctx, repoDir) {
			return r.generators[i]
		}
	}
	return nil
}

// DetectModulePath attempts to read the Go module path from go.mod
// in the given directory. Returns "module/path" or empty string.
func DetectModulePath(repoDir string) string {
	gomod := filepath.Join(repoDir, "go.mod")
	data, err := os.ReadFile(gomod)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// DetectLanguage reports the primary language of a repo directory by
// probing for known marker files.
func DetectLanguage(repoDir string) string {
	markers := []struct {
		file     string
		language string
	}{
		{"go.mod", "go"},
		{"package.json", "typescript"},
		{"Cargo.toml", "rust"},
		{"pom.xml", "java"},
		{"build.gradle", "java"},
		{"CMakeLists.txt", "cpp"},
		{"Makefile", "generic"},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(repoDir, m.file)); err == nil {
			return m.language
		}
	}
	return "unknown"
}

// GenerateRepo detects the language, picks a generator, and runs it.
// This is the convenience entry point for "monofs-bazelctl generate".
func GenerateRepo(ctx context.Context, reg *Registry, repoDir string, opts GenerateOptions) (*GenerateResult, error) {
	opts.RepoDir = repoDir

	// Auto-detect module path for Go repos.
	if opts.ModulePath == "" {
		opts.ModulePath = DetectModulePath(repoDir)
	}

	gen := reg.FindGenerator(ctx, repoDir)
	if gen == nil {
		return nil, fmt.Errorf("no generator found for repo at %s (detected language: %s)", repoDir, DetectLanguage(repoDir))
	}

	return gen.Generate(ctx, opts)
}
