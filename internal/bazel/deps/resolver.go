package deps

import (
	"context"
	"fmt"
	"strings"

	"github.com/radryc/monofs/internal/bazel"
)

// ResolvedDep describes how an import path resolves in the virtual monorepo.
type ResolvedDep struct {
	// Type is self, cross-repo, or external.
	Type ResolvedType

	// Target is the Bazel target label (e.g. "@infra_tooling//pkg/auth:auth").
	Target string

	// DisplayPath is the target repo's display_path (cross-repo only).
	DisplayPath string

	// ModulePath is the Go module path of the resolved repo.
	ModulePath string
}

// ResolvedType indicates the nature of a dependency.
type ResolvedType string

const (
	// SelfDep means the import belongs to the same repo.
	SelfDep ResolvedType = "self"

	// CrossRepoDep means the import is from another ingested repo.
	CrossRepoDep ResolvedType = "cross-repo"

	// ExternalDep means the import is from outside the virtual monorepo.
	ExternalDep ResolvedType = "external"
)

// ImportResolver resolves import paths (Go, TypeScript, etc.) to Bazel
// targets in the virtual monorepo.
type ImportResolver struct {
	// Manifest is the workspace manifest for matching imports to repos.
	Manifest *bazel.WorkspaceManifest

	// Lockfile maps repo display_paths to their module paths and commits.
	// Used to validate that the pinned version matches.
	Lockfile *WorkspaceLockfile
}

// NewImportResolver creates a resolver with the given manifest and lockfile.
func NewImportResolver(manifest *bazel.WorkspaceManifest, lockfile *WorkspaceLockfile) *ImportResolver {
	return &ImportResolver{
		Manifest: manifest,
		Lockfile: lockfile,
	}
}

// ResolveGoImport resolves a Go import path relative to a repo identified
// by its display_path. Returns the Bazel target label.
//
// Examples:
//
//	ResolveGoImport("github.com/org/platform-api/pkg/auth", "sre/platform-api")
//	  → {Type: SelfDep, Target: "//pkg/auth:auth"}
//
//	ResolveGoImport("github.com/org/infra-tooling/pkg/middleware", "sre/platform-api")
//	  → {Type: CrossRepoDep, Target: "@infra_tooling//pkg/middleware:middleware"}
//
//	ResolveGoImport("github.com/gorilla/mux", "sre/platform-api")
//	  → {Type: ExternalDep, Target: "@com_github_gorilla_mux//:mux"}
func (r *ImportResolver) ResolveGoImport(ctx context.Context, importPath, fromDisplayPath string) (*ResolvedDep, error) {
	// 1. Build a map of display_path → module_path from the manifest.
	repoModules := r.buildRepoModuleMap()

	// 2. Find the repo that owns this import path.
	fromModulePath := repoModules[fromDisplayPath]

	// 3. If the import starts with the current repo's module path, it's a self-dep.
	if fromModulePath != "" && (importPath == fromModulePath || strings.HasPrefix(importPath, fromModulePath+"/")) {
		suffix := strings.TrimPrefix(importPath, fromModulePath)
		suffix = strings.TrimPrefix(suffix, "/")
		return &ResolvedDep{
			Type:       SelfDep,
			Target:     goImportToBazelTarget("", suffix),
			ModulePath: fromModulePath,
		}, nil
	}

	// 4. Check if any other repo owns this import path.
	for displayPath, modulePath := range repoModules {
		if displayPath == fromDisplayPath {
			continue
		}
		if modulePath == "" {
			continue
		}
		if importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/") {
			suffix := strings.TrimPrefix(importPath, modulePath)
			suffix = strings.TrimPrefix(suffix, "/")
			moduleName := bazel.DisplayPathToModuleName(displayPath)
			return &ResolvedDep{
				Type:        CrossRepoDep,
				Target:      goImportToBazelTarget(moduleName, suffix),
				DisplayPath: displayPath,
				ModulePath:  modulePath,
			}, nil
		}
	}

	// 5. External dependency.
	return &ResolvedDep{
		Type:   ExternalDep,
		Target: goImportToGazelleTarget(importPath),
	}, nil
}

// ResolveAllGoImports resolves a batch of Go imports for a single repo.
// Returns resolved deps indexed by import path.
func (r *ImportResolver) ResolveAllGoImports(ctx context.Context, importPaths []string, fromDisplayPath string) (map[string]*ResolvedDep, error) {
	result := make(map[string]*ResolvedDep, len(importPaths))
	for _, ip := range importPaths {
		dep, err := r.ResolveGoImport(ctx, ip, fromDisplayPath)
		if err != nil {
			return result, fmt.Errorf("resolve %q: %w", ip, err)
		}
		result[ip] = dep
	}
	return result, nil
}

// buildRepoModuleMap builds a display_path → module_path map from the manifest.
func (r *ImportResolver) buildRepoModuleMap() map[string]string {
	m := make(map[string]string)
	for _, repo := range r.Manifest.IncludedRepos() {
		mp := inferModulePath(repo)
		if mp != "" {
			m[repo.DisplayPath] = mp
		}
	}
	return m
}

// goImportToBazelTarget converts a Go import path suffix to a Bazel label.
//
//	repo=""  (self), suffix="pkg/auth" → "//pkg/auth:auth"
//	repo="infra_tooling", suffix="pkg/auth" → "@infra_tooling//pkg/auth:auth"
func goImportToBazelTarget(repo, suffix string) string {
	parts := strings.Split(suffix, "/")
	target := parts[len(parts)-1]
	if target == "" && repo != "" {
		// Root package (import is the module path itself).
		// Derive target name from the repo's module name.
		target = repo
	}
	dir := suffix
	if target == dir {
		// Single-segment path (root package): "mypkg"
		dir = ""
	} else if target == "" {
		dir = ""
	}
	prefix := "//"
	if repo != "" {
		prefix = "@" + repo + "//"
	}
	if dir == "" {
		return fmt.Sprintf("%s:%s", prefix, target)
	}
	return fmt.Sprintf("%s%s:%s", prefix, dir, target)
}

// goImportToGazelleTarget converts an external import path to a Gazelle-style label.
func goImportToGazelleTarget(importPath string) string {
	parts := strings.Split(importPath, "/")
	target := parts[len(parts)-1]
	gazelleName := strings.NewReplacer(
		".", "_",
		"-", "_",
		"@", "_",
	).Replace(importPath)
	gazelleName = strings.ReplaceAll(gazelleName, "/", "_")
	return fmt.Sprintf("@%s//:%s", gazelleName, target)
}

// Cycle represents a dependency cycle between repos.
type Cycle []string

// DetectCycles finds cycles in the dependency graph.
func (r *ImportResolver) DetectCycles() []Cycle {
	if r.Lockfile == nil {
		return nil
	}

	graph := make(map[string][]string)
	for _, repo := range r.Lockfile.Repositories {
		var deps []string
		for depPath := range repo.Dependencies {
			deps = append(deps, depPath)
		}
		graph[repo.DisplayPath] = deps
	}

	return findCycles(graph)
}

// findCycles uses DFS to detect cycles in a directed graph.
func findCycles(graph map[string][]string) []Cycle {
	const (
		white = 0 // unvisited
		gray  = 1 // in current DFS path
		black = 2 // fully explored
	)

	color := make(map[string]int)
	parent := make(map[string]string)
	var cycles []Cycle

	var dfs func(node string)
	dfs = func(node string) {
		color[node] = gray
		for _, neighbor := range graph[node] {
			if color[neighbor] == white {
				parent[neighbor] = node
				dfs(neighbor)
			} else if color[neighbor] == gray {
				// Found a cycle. Reconstruct it.
				cycle := Cycle{neighbor}
				cur := node
				for cur != neighbor && cur != "" {
					cycle = append(Cycle{cur}, cycle...)
					cur = parent[cur]
				}
				cycles = append(cycles, cycle)
			}
		}
		color[node] = black
	}

	for node := range graph {
		if color[node] == white {
			dfs(node)
		}
	}

	return cycles
}
