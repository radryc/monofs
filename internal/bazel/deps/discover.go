package deps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/radryc/monofs/internal/bazel"
)

// DiscoveredDep is a dependency found by scanning source files.
type DiscoveredDep struct {
	// DisplayPath is the target repo's display_path (e.g. "sre/infra-tooling").
	DisplayPath string

	// ModulePath is the Go module path (e.g. "github.com/org/infra-tooling").
	ModulePath string

	// Source is the import path that triggered this discovery (e.g.
	// "github.com/org/infra-tooling/pkg/auth").
	SourceImport string

	// DetectedBy indicates which scanner found this dependency.
	DetectedBy string
}

// DepDiscoverer finds cross-repo dependencies by scanning a repo's
// source files and build manifests.
type DepDiscoverer struct {
	// Manifest is the workspace manifest for matching discovered
	// module paths to ingested repos.
	Manifest *bazel.WorkspaceManifest
}

// NewDepDiscoverer creates a discoverer backed by a workspace manifest.
func NewDepDiscoverer(manifest *bazel.WorkspaceManifest) *DepDiscoverer {
	return &DepDiscoverer{Manifest: manifest}
}

// DiscoverAll returns all cross-repo dependencies for the repo at repoDir.
// It scans go.mod, package.json, etc. and matches discovered module
// paths against the workspace manifest.
func (d *DepDiscoverer) DiscoverAll(ctx context.Context, repoDir, displayPath string) ([]DiscoveredDep, error) {
	var all []DiscoveredDep

	// Go: parse go.mod
	if gomods, err := d.discoverGoMod(repoDir, displayPath); err == nil {
		all = append(all, gomods...)
	}

	// Future: TypeScript (package.json), Rust (Cargo.toml), Java (pom.xml)

	return all, nil
}

// discoverGoMod reads go.mod and matches require directives against the
// workspace manifest. Returns dependencies on other ingested repos.
func (d *DepDiscoverer) discoverGoMod(repoDir, fromDisplayPath string) ([]DiscoveredDep, error) {
	gomod := filepath.Join(repoDir, "go.mod")
	data, err := os.ReadFile(gomod)
	if err != nil {
		return nil, fmt.Errorf("read go.mod: %w", err)
	}

	var deps []DiscoveredDep
	inRequire := false

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)

		// Enter/exit require block.
		if strings.HasPrefix(line, "require (") {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}

		// Single-line require: "require module/path v1.2.3"
		if !inRequire && strings.HasPrefix(line, "require ") {
			line = strings.TrimPrefix(line, "require ")
		} else if !inRequire {
			continue
		}

		// Skip indirect comments.
		if strings.Contains(line, "// indirect") {
			continue
		}

		// Parse: "module/path v1.2.3" or "module/path // comment"
		parts := strings.Fields(line)
		if len(parts) < 1 {
			continue
		}
		modulePath := strings.TrimSpace(parts[0])
		if modulePath == "" || strings.HasPrefix(modulePath, "//") {
			continue
		}

		// Check if this module path matches any repo in the manifest.
		dep := d.matchModulePath(modulePath, fromDisplayPath)
		if dep != nil {
			dep.DetectedBy = "go.mod"
			deps = append(deps, *dep)
		}
	}

	return deps, nil
}

// matchModulePath checks if a Go module path belongs to one of the
// ingested repos in the workspace manifest. Returns nil if no match.
func (d *DepDiscoverer) matchModulePath(modulePath, fromDisplayPath string) *DiscoveredDep {
	if d.Manifest == nil {
		return nil
	}
	for _, repo := range d.Manifest.IncludedRepos() {
		if repo.DisplayPath == fromDisplayPath {
			continue // self-reference
		}
		// The repo's module path is derived from its source or display_path.
		// For now, we guess the module path from the source URL.
		candidateModule := inferModulePath(repo)
		if candidateModule == "" {
			continue
		}
		if modulePath == candidateModule || strings.HasPrefix(modulePath, candidateModule+"/") {
			return &DiscoveredDep{
				DisplayPath:  repo.DisplayPath,
				ModulePath:   candidateModule,
				SourceImport: modulePath,
			}
		}
	}
	return nil
}

// inferModulePath derives a Go module path from a ManifestRepository.
// It uses common patterns: github.com/org/repo from source URL.
func inferModulePath(repo bazel.ManifestRepository) string {
	source := repo.Source
	if source == "" {
		return ""
	}
	// github.com/org/repo.git → github.com/org/repo
	for _, prefix := range []string{"https://", "http://"} {
		source = strings.TrimPrefix(source, prefix)
	}
	// git@github.com:org/repo.git → github.com/org/repo
	if idx := strings.Index(source, "@"); idx >= 0 {
		source = source[idx+1:]
	}
	if idx := strings.Index(source, ":"); idx >= 0 && !strings.Contains(source[:idx], "/") {
		// SSH-style: github.com:org/repo
		source = source[:idx] + "/" + source[idx+1:]
	}
	source = strings.TrimSuffix(source, ".git")
	return source
}
