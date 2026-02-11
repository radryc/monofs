package npm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/radryc/monofs/internal/buildlayout"
)

const (
	// NodeModulesPrefix is the virtual mount prefix for npm packages
	NodeModulesPrefix = "node_modules"
)

// NpmMapper implements LayoutMapper for npm packages.
type NpmMapper struct{}

// NewNpmMapper creates a new npm layout mapper.
func NewNpmMapper() *NpmMapper {
	return &NpmMapper{}
}

func (n *NpmMapper) Type() string { return "npm" }

// Matches returns true for npm package ingestions.
func (n *NpmMapper) Matches(info buildlayout.RepoInfo) bool {
	return info.IngestionType == "npm"
}

// ExtractPackageInfo extracts the package name and version from npm source.
// Handles both scoped (@scope/package@version) and regular (package@version) formats.
func (n *NpmMapper) ExtractPackageInfo(source string, ref string) (packageName string, version string, err error) {
	packageName = source
	version = ref

	// Handle scoped packages: @scope/package@version
	if strings.HasPrefix(packageName, "@") {
		// Split on last @ to get version
		lastAt := strings.LastIndex(packageName, "@")
		if lastAt > 0 { // Must be after the initial @
			// Extract version from source if not provided in Ref
			if version == "" {
				version = packageName[lastAt+1:]
			}
			packageName = packageName[:lastAt]
		}
	} else if strings.Contains(packageName, "@") {
		// Regular package with version: lodash@4.17.21
		parts := strings.Split(packageName, "@")
		packageName = parts[0]
		if version == "" && len(parts) > 1 {
			version = parts[1]
		}
	}

	// Use actual version, don't default to "latest"
	if version == "" {
		return "", "", fmt.Errorf("version required for npm package: %s", packageName)
	}
	version = strings.TrimPrefix(version, "v")

	return packageName, version, nil
}

// MapPaths creates virtual hard link entries for npm packages.
//
// For "lodash@4.17.21" from "github.com/lodash/lodash":
// 1. Files stored at canonical path: github.com/lodash/lodash/ (primary storage)
// 2. Hard link created: node_modules/lodash@4.17.21/ -> github.com/lodash/lodash/ (same storage_id)
//
// Output includes entries for BOTH paths, sharing the same storage_id.
func (n *NpmMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
	if len(files) == 0 {
		return nil, nil
	}

	// Check for package.json to confirm this is an npm package
	hasPackageJSON := false
	for _, f := range files {
		if f.Path == "package.json" {
			hasPackageJSON = true
			break
		}
	}

	if !hasPackageJSON {
		return nil, nil // Not an npm package
	}

	// Extract package name and version from SOURCE
	packageName, version, err := n.ExtractPackageInfo(info.Source, info.Ref)
	if err != nil {
		return nil, err
	}

	// Extract canonical GitHub repository path from metadata
	canonicalPath := ""
	if len(files) > 0 && files[0].BackendMetadata != nil {
		canonicalPath = normalizeGitURL(files[0].BackendMetadata["repository_url"])
	}
	if canonicalPath == "" {
		// Fallback: use registry path if no repository found
		canonicalPath = "registry.npmjs.org/" + packageName
	}

	// Add version to canonical path
	canonicalPath = canonicalPath + "@" + version
	_ = canonicalPath // TODO: use canonical path for virtual entry deduplication

	// Initial ingestion already created files at the canonical GitHub path.
	// Now create virtual hard link entries at node_modules/ for npm compatibility.
	// This matches the Go module pattern: primary at canonical path, virtual link at tool-specific path.

	var entries []buildlayout.VirtualEntry

	// Create entries for node_modules path (virtual hard link to canonical GitHub path)
	nodeModulesPath := fmt.Sprintf("%s/%s@%s", NodeModulesPrefix, packageName, version)
	for _, f := range files {
		entries = append(entries, buildlayout.VirtualEntry{
			VirtualDisplayPath: nodeModulesPath,
			VirtualFilePath:    f.Path,
			OriginalFilePath:   f.Path,
		})
	}

	// Generate npm cache metadata for offline builds
	cacheEntries := createNpmCacheMetadata(packageName, version, files, info)
	entries = append(entries, cacheEntries...)

	return entries, nil
}

// normalizeGitURL converts a Git URL to canonical path format
// Examples:
//   - https://github.com/webpack/webpack -> github.com/webpack/webpack
//   - git+https://github.com/lodash/lodash.git -> github.com/lodash/lodash
//   - github:user/repo -> github.com/user/repo
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

	// Handle shorthand formats
	if strings.HasPrefix(url, "github:") {
		return "github.com/" + strings.TrimPrefix(url, "github:")
	}
	if strings.HasPrefix(url, "gitlab:") {
		return "gitlab.com/" + strings.TrimPrefix(url, "gitlab:")
	}

	// Validate it looks like a proper path
	if strings.Contains(url, "/") && !strings.Contains(url, " ") {
		return url
	}

	return ""
}

// packageJSON represents the structure of package.json
type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// ParseDependencyFile parses package.json and returns all dependencies.
func (n *NpmMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
	var pkg packageJSON
	if err := json.Unmarshal(content, &pkg); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}

	deps := make([]buildlayout.Dependency, 0)

	// Parse regular dependencies
	for name, version := range pkg.Dependencies {
		// Clean version specifiers (^, ~, >=, etc.)
		cleanVersion := cleanNpmVersion(version)
		deps = append(deps, buildlayout.Dependency{
			Module:  name,
			Version: cleanVersion,
			Source:  name + "@" + cleanVersion,
		})
	}

	// Parse dev dependencies (optional, can be skipped for production builds)
	for name, version := range pkg.DevDependencies {
		cleanVersion := cleanNpmVersion(version)
		deps = append(deps, buildlayout.Dependency{
			Module:  name,
			Version: cleanVersion,
			Source:  name + "@" + cleanVersion,
		})
	}

	return deps, nil
}

// cleanNpmVersion removes version specifiers and returns clean version
func cleanNpmVersion(version string) string {
	// Remove common prefixes
	version = strings.TrimPrefix(version, "^")
	version = strings.TrimPrefix(version, "~")
	version = strings.TrimPrefix(version, ">=")
	version = strings.TrimPrefix(version, "<=")
	version = strings.TrimPrefix(version, ">")
	version = strings.TrimPrefix(version, "<")
	version = strings.TrimSpace(version)

	// If version is a range or wildcard, default to "latest"
	if strings.Contains(version, " ") || version == "*" || version == "x" {
		return "latest"
	}

	return version
}

// createNpmCacheMetadata generates npm cache files for offline builds.
// Creates cache structure compatible with npm's _cacache directory.
//
// npm cache structure:
//
//	npm-cache/
//	├── _cacache/
//	│   └── content-v2/sha512/[hash]  # Package tarballs
//	└── [package@version]/
//	    ├── package.json               # Package manifest
//	    └── .package-lock.json         # Lock metadata
func createNpmCacheMetadata(packageName, version string, files []buildlayout.FileInfo, info buildlayout.RepoInfo) []buildlayout.VirtualEntry {
	var entries []buildlayout.VirtualEntry

	// Cache path for this package
	cachePath := fmt.Sprintf("npm-cache/%s@%s", packageName, version)

	// 1. Find and store package.json
	var pkgJSON []byte
	for _, f := range files {
		if f.Path == "package.json" {
			if len(f.Content) > 0 {
				pkgJSON = f.Content
			}
			break
		}
	}

	if pkgJSON == nil {
		// Create minimal package.json
		pkgJSON = []byte(fmt.Sprintf(`{
  "name": "%s",
  "version": "%s",
  "description": "Package ingested via MonoFS"
}`, packageName, version))
	}

	entries = append(entries, buildlayout.VirtualEntry{
		VirtualDisplayPath: cachePath,
		VirtualFilePath:    "package.json",
		OriginalFilePath:   "",
		SyntheticContent:   pkgJSON,
	})

	// 2. Create .npm-metadata.json for npm cache
	metadata := fmt.Sprintf(`{
  "name": "%s",
  "version": "%s",
  "time": "%s",
  "dist": {
    "integrity": "sha512-monofs",
    "tarball": "file://%s"
  }
}`, packageName, version, info.CommitTime, cachePath)

	entries = append(entries, buildlayout.VirtualEntry{
		VirtualDisplayPath: cachePath,
		VirtualFilePath:    ".npm-metadata.json",
		OriginalFilePath:   "",
		SyntheticContent:   []byte(metadata),
	})

	// 3. Create .package-lock.json entry
	lockData := fmt.Sprintf(`{
  "name": "%s",
  "version": "%s",
  "lockfileVersion": 3,
  "requires": true
}`, packageName, version)

	entries = append(entries, buildlayout.VirtualEntry{
		VirtualDisplayPath: cachePath,
		VirtualFilePath:    ".package-lock.json",
		OriginalFilePath:   "",
		SyntheticContent:   []byte(lockData),
	})

	return entries
}
