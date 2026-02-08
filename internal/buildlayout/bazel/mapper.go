package bazel

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/radryc/monofs/internal/buildlayout"
)

const (
	// BazelReposPrefix is the virtual mount prefix for Bazel repos
	BazelReposPrefix = "bazel-repos"
)

// BazelMapper implements LayoutMapper for Bazel repositories.
type BazelMapper struct{}

// NewBazelMapper creates a new Bazel layout mapper.
func NewBazelMapper() *BazelMapper {
	return &BazelMapper{}
}

func (b *BazelMapper) Type() string { return "bazel" }

// Matches returns true for git repos (Bazel repos are git repos).
// The actual check for Bazel files happens in MapPaths (returns empty if not Bazel).
func (b *BazelMapper) Matches(info buildlayout.RepoInfo) bool {
	return info.IngestionType == "git"
}

// MapPaths creates entries under bazel-repos/<module_name>@<version>/
//
// Only generates entries if the repo contains MODULE.bazel, WORKSPACE, or WORKSPACE.bazel.
func (b *BazelMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
	if len(files) == 0 {
		return nil, nil
	}

	// Check if this repo has Bazel build files.
	hasBazel := false
	for _, f := range files {
		base := path.Base(f.Path)
		if base == "MODULE.bazel" || base == "WORKSPACE" || base == "WORKSPACE.bazel" {
			if f.Path == base { // Only root-level files count
				hasBazel = true
				break
			}
		}
	}

	if !hasBazel {
		return nil, nil // Not a Bazel repo, skip silently
	}

	// Extract module name from display path.
	// "github.com/bazelbuild/rules_go" → "rules_go"
	moduleName := path.Base(strings.TrimSuffix(info.DisplayPath, "/"))

	// Version from ref, default to "latest"
	version := info.Ref
	if version == "" || version == "main" || version == "master" {
		version = "latest"
	}

	virtualDisplayPath := fmt.Sprintf("%s/%s@%s", BazelReposPrefix, moduleName, version)

	entries := make([]buildlayout.VirtualEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, buildlayout.VirtualEntry{
			VirtualDisplayPath: virtualDisplayPath,
			VirtualFilePath:    f.Path,
			OriginalFilePath:   f.Path,
		})
	}

	return entries, nil
}

// bazelDepRegex matches bazel_dep(name = "...", version = "...") in MODULE.bazel.
var bazelDepRegex = regexp.MustCompile(
	`bazel_dep\s*\(\s*name\s*=\s*"([^"]+)"\s*,\s*version\s*=\s*"([^"]+)"\s*\)`,
)

// ParseDependencyFile parses MODULE.bazel for bazel_dep declarations.
func (b *BazelMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
	matches := bazelDepRegex.FindAllStringSubmatch(string(content), -1)

	deps := make([]buildlayout.Dependency, 0, len(matches))
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		deps = append(deps, buildlayout.Dependency{
			Module:  m[1],
			Version: m[2],
			Source:  m[1] + "@" + m[2],
		})
	}

	return deps, nil
}
