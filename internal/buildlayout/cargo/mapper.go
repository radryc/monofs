package cargo

import (
	"fmt"
	"strings"

	"github.com/radryc/monofs/internal/buildlayout"
)

const (
	// CargoRegistryPrefix is the virtual mount prefix for Cargo crates
	CargoRegistryPrefix = ".cargo/registry/src"
)

// CargoMapper implements LayoutMapper for Cargo crates.
type CargoMapper struct{}

// NewCargoMapper creates a new Cargo layout mapper.
func NewCargoMapper() *CargoMapper {
	return &CargoMapper{}
}

func (c *CargoMapper) Type() string { return "cargo" }

// Matches returns true for Cargo crate ingestions.
func (c *CargoMapper) Matches(info buildlayout.RepoInfo) bool {
	return info.IngestionType == "cargo"
}

// MapPaths creates virtual hard link entries for Cargo crates.
//
// For "serde@1.0.152" from "github.com/serde-rs/serde":
// 1. Files stored at canonical path: github.com/serde-rs/serde/ (primary storage)
// 2. Hard link created: .cargo/registry/src/serde-1.0.152/ -> github.com/serde-rs/serde/ (same storage_id)
//
// Output includes entries for BOTH paths, sharing the same storage_id.
func (c *CargoMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
	if len(files) == 0 {
		return nil, nil
	}

	// Check for Cargo.toml to confirm this is a Cargo crate
	hasCargoToml := false
	for _, f := range files {
		if f.Path == "Cargo.toml" {
			hasCargoToml = true
			break
		}
	}

	if !hasCargoToml {
		return nil, nil // Not a Cargo crate
	}

	// Extract crate name and version
	crateName := strings.TrimPrefix(info.DisplayPath, "cargo:")
	if strings.Contains(crateName, "@") {
		parts := strings.Split(crateName, "@")
		crateName = parts[0]
	}

	version := info.Ref
	if version == "" {
		version = "latest"
	}
	version = strings.TrimPrefix(version, "v")

	// Extract canonical GitHub repository path from metadata
	canonicalPath := ""
	if len(files) > 0 && files[0].BackendMetadata != nil {
		canonicalPath = normalizeGitURL(files[0].BackendMetadata["repository_url"])
	}
	if canonicalPath == "" {
		// Fallback: use crates.io path if no repository found
		canonicalPath = "crates.io/" + crateName + "@" + version
	}
	_ = canonicalPath // TODO: use canonical path for virtual entry deduplication

	// Initial ingestion already created files at the canonical GitHub path.
	// Now create virtual hard link entries at .cargo/registry/src/ for Cargo compatibility.
	// This matches the npm/Go module pattern: primary at canonical path, virtual link at tool-specific path.

	var entries []buildlayout.VirtualEntry

	// Create entries for .cargo/registry/src/ path (virtual hard link to canonical GitHub path)
	// Cargo uses hyphen between name and version
	cargoPath := fmt.Sprintf("%s/%s-%s", CargoRegistryPrefix, crateName, version)
	for _, f := range files {
		entries = append(entries, buildlayout.VirtualEntry{
			VirtualDisplayPath: cargoPath,
			VirtualFilePath:    f.Path,
			OriginalFilePath:   f.Path,
		})
	}

	return entries, nil
}

// normalizeGitURL converts a Git URL to canonical path format
func normalizeGitURL(url string) string {
	if url == "" {
		return ""
	}

	// Remove git+ prefix and .git suffix
	url = strings.TrimPrefix(url, "git+")
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")

	// Remove scheme
	for _, prefix := range []string{"https://", "http://", "git://", "ssh://"} {
		url = strings.TrimPrefix(url, prefix)
	}

	// Handle git@github.com:user/repo format
	if strings.HasPrefix(url, "git@") {
		url = strings.TrimPrefix(url, "git@")
		url = strings.Replace(url, ":", "/", 1)
	}

	// Validate it looks like a proper path
	if strings.Contains(url, "/") && !strings.Contains(url, " ") {
		return url
	}

	return ""
}

// ParseDependencyFile parses Cargo.toml and returns all dependencies.
// Note: This is a simplified parser. For production use, consider using
// a proper TOML library like github.com/pelletier/go-toml
func (c *CargoMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
	deps := make([]buildlayout.Dependency, 0)

	// Simple line-by-line parser for [dependencies] and [dev-dependencies] sections
	lines := strings.Split(string(content), "\n")
	inDeps := false

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Check if we're entering dependencies or dev-dependencies section
		if line == "[dependencies]" || line == "[dev-dependencies]" {
			inDeps = true
			continue
		}

		// Check if we're leaving dependencies section (new section starts)
		if inDeps && strings.HasPrefix(line, "[") {
			inDeps = false
			continue
		}

		// Parse dependency if in dependencies section
		if inDeps && line != "" && !strings.HasPrefix(line, "#") {
			// Parse lines like: serde = "1.0"  or  serde = { version = "1.0", features = [...] }
			if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
				name := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])

				// Extract version
				version := "latest"
				if strings.HasPrefix(value, "\"") {
					// Simple version: "1.0"
					version = strings.Trim(value, "\"' ")
				} else if strings.HasPrefix(value, "{") {
					// Complex format: { version = "1.0", ... }
					// Extract version field
					if idx := strings.Index(value, "version"); idx != -1 {
						rest := value[idx+7:] // Skip "version"
						rest = strings.TrimSpace(rest)
						if strings.HasPrefix(rest, "=") {
							rest = strings.TrimSpace(rest[1:])
							if endIdx := strings.IndexAny(rest, ",}"); endIdx != -1 {
								version = strings.Trim(rest[:endIdx], "\"' ")
							}
						}
					}
				}

				deps = append(deps, buildlayout.Dependency{
					Module:  name,
					Version: version,
					Source:  name + "@" + version,
				})
			}
		}
	}

	return deps, nil
}
