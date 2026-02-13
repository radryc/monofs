// Package golang implements the Go module layout mapper.
// It creates virtual entries under go-modules/pkg/mod/ matching GOMODCACHE layout.
package golang

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"unicode"

	"github.com/radryc/monofs/internal/buildcache"
	gocache "github.com/radryc/monofs/internal/buildcache/golang"
	"github.com/radryc/monofs/internal/buildlayout"
)

const (
	// GoModCachePrefix is the virtual mount prefix for Go module cache.
	// The full GOMODCACHE path will be: <mount>/go-modules/pkg/mod/
	GoModCachePrefix = "go-modules/pkg/mod"
)

// GoMapper implements LayoutMapper for Go modules.
type GoMapper struct{}

// NewGoMapper creates a new Go module layout mapper.
func NewGoMapper() *GoMapper {
	return &GoMapper{}
}

func (g *GoMapper) Type() string { return "go" }

// Matches returns true for Go module ingestions.
// Only matches when IngestionType is "go" — Git repos that happen to contain
// Go code are NOT matched (they don't need GOMODCACHE layout).
func (g *GoMapper) Matches(info buildlayout.RepoInfo) bool {
	return info.IngestionType == "go"
}

// MapPaths creates virtual entries under go-modules/pkg/mod/<module>@<version>/
//
// For a repo ingested as "github.com/google/uuid@v1.6.0" with files [uuid.go, go.mod]:
// Output:
//
//	VirtualEntry{
//	  VirtualDisplayPath: "go-modules/pkg/mod/github.com/google/uuid@v1.6.0",
//	  VirtualFilePath:    "uuid.go",
//	  OriginalFilePath:   "uuid.go",
//	}
//
// The Go module cache uses case-insensitive encoding:
//
//	github.com/Azure/... → github.com/!azure/...
func (g *GoMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
	if len(files) == 0 {
		return nil, nil
	}

	// Parse module path and version from DisplayPath.
	// DisplayPath format: "github.com/google/uuid@v1.6.0"
	// OR without version if version is in Ref.
	modulePath, version := parseModuleVersion(info.DisplayPath, info.Ref)
	if modulePath == "" {
		return nil, fmt.Errorf("cannot parse module path from display path %q", info.DisplayPath)
	}
	if version == "" {
		return nil, fmt.Errorf("no version found for module %q (display_path=%q, ref=%q)",
			modulePath, info.DisplayPath, info.Ref)
	}

	// Apply Go module cache case encoding to module path.
	encodedModule := EncodePath(modulePath)

	// Return just module@version without prefix
	// The prefix (go-modules/pkg/mod/) is added during ingestion by the handler
	virtualDisplayPath := encodedModule + "@" + version

	entries := make([]buildlayout.VirtualEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, buildlayout.VirtualEntry{
			VirtualDisplayPath: virtualDisplayPath,
			VirtualFilePath:    f.Path,
			OriginalFilePath:   f.Path,
		})
	}

	// Generate cache/download/ entries for offline builds.
	// These are synthetic entries (no OriginalFilePath) — the fetcher
	// generates content on-demand when FUSE reads these files.
	cacheEntries := g.generateCacheDownloadEntries(modulePath, version, info.Source, files)
	entries = append(entries, cacheEntries...)

	return entries, nil
}

// ParseDependencyFile parses a go.mod file and returns all required dependencies.
//
// Handles:
//   - require ( ... ) blocks
//   - Single-line: require github.com/foo/bar v1.2.3
//   - Skips comments and "// indirect" markers (includes indirect deps)
//   - Skips replace/exclude/retract directives
func (g *GoMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
	var deps []buildlayout.Dependency
	scanner := bufio.NewScanner(bytes.NewReader(content))

	inRequireBlock := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		// Handle require block start
		if strings.HasPrefix(line, "require (") || strings.HasPrefix(line, "require(") {
			inRequireBlock = true
			continue
		}

		// Handle block end
		if line == ")" {
			inRequireBlock = false
			continue
		}

		// Single-line require
		if strings.HasPrefix(line, "require ") && !inRequireBlock {
			dep := parseRequireLine(strings.TrimPrefix(line, "require "))
			if dep != nil {
				deps = append(deps, *dep)
			}
			continue
		}

		// Inside require block
		if inRequireBlock {
			dep := parseRequireLine(line)
			if dep != nil {
				deps = append(deps, *dep)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading go.mod: %w", err)
	}

	return deps, nil
}

// parseRequireLine parses a single require line like:
//
//	github.com/google/uuid v1.6.0
//	github.com/google/uuid v1.6.0 // indirect
func parseRequireLine(line string) *buildlayout.Dependency {
	// Remove inline comments
	if idx := strings.Index(line, "//"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	if line == "" {
		return nil
	}

	parts := strings.Fields(line)
	if len(parts) < 2 {
		return nil
	}

	module := parts[0]
	version := parts[1]

	return &buildlayout.Dependency{
		Module:  module,
		Version: version,
		Source:  module + "@" + version,
	}
}

// parseModuleVersion extracts module path and version from a display path.
//
// Patterns:
//
//	"github.com/google/uuid@v1.6.0" → ("github.com/google/uuid", "v1.6.0")
//	"github.com/google/uuid" with ref="v1.6.0" → ("github.com/google/uuid", "v1.6.0")
func parseModuleVersion(displayPath, ref string) (modulePath, version string) {
	if idx := strings.LastIndex(displayPath, "@"); idx >= 0 {
		return displayPath[:idx], displayPath[idx+1:]
	}
	// No @ in displayPath — use ref as version
	return displayPath, ref
}

// EncodePath applies Go module cache case encoding.
// Uppercase letters are replaced with '!' + lowercase.
// Example: "github.com/Azure/go-autorest" → "github.com/!azure/go-autorest"
func EncodePath(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	for _, r := range s {
		if unicode.IsUpper(r) {
			buf.WriteRune('!')
			buf.WriteRune(unicode.ToLower(r))
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

// DecodePath reverses EncodePath.
// Example: "github.com/!azure/go-autorest" → "github.com/Azure/go-autorest"
func DecodePath(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	escape := false
	for _, r := range s {
		if escape {
			buf.WriteRune(unicode.ToUpper(r))
			escape = false
		} else if r == '!' {
			escape = true
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

// generateCacheDownloadEntries creates synthetic VirtualEntry items for the
// Go module download cache (cache/download/<escaped_module>/@v/*).
//
// These entries have NO OriginalFilePath — they are fully synthetic.
// The router uses their BlobHash, BackendMetadata etc. directly.
// Fetchers generate actual content on-demand when FUSE reads occur.
//
// Cache entries use a SEPARATE VirtualDisplayPath from source files so they
// appear at the correct location in the FUSE mount:
//
//	<mount>/go-modules/pkg/mod/cache/download/<escaped_module>/@v/<file>
//
// NOT inside the module source directory.
func (g *GoMapper) generateCacheDownloadEntries(modulePath, version, source string, files []buildlayout.FileInfo) []buildlayout.VirtualEntry {
	// Strip the go-modules prefix if present — in production the display path
	// includes "go-modules/pkg/mod/" but the Go module path must not contain it.
	realModulePath := modulePath
	if pfx := GoModCachePrefix + "/"; strings.HasPrefix(modulePath, pfx) {
		realModulePath = strings.TrimPrefix(modulePath, pfx)
	}

	// Find go.mod size from the ingested files so the cache .mod entry
	// reports the correct size upfront (same content, served from proxy/zip).
	var goModSize uint64
	for _, f := range files {
		if f.Path == "go.mod" {
			goModSize = f.Size
			break
		}
	}

	gen := gocache.NewGenerator()
	artifactEntries, err := gen.GenerateEntries(buildcache.ArtifactParams{
		ModulePath:    realModulePath,
		Version:       version,
		Source:        source,
		GoModFileSize: goModSize,
	})
	if err != nil {
		// Non-fatal: return empty list, source files still work
		return nil
	}

	// Cache entries go under a SEPARATE virtual display path so the directory
	// tree matches what `go build` expects in $GOMODCACHE:
	//   go-modules/pkg/mod/cache/download/<escaped_module>/@v/
	escapedRealModule := EncodePath(realModulePath)
	cacheDisplayPath := GoModCachePrefix + "/cache/download/" + escapedRealModule + "/@v"

	entries := make([]buildlayout.VirtualEntry, 0, len(artifactEntries))
	for _, ae := range artifactEntries {
		// Extract just the filename from the generator's full relative path.
		// e.g. "cache/download/github.com/google/uuid/@v/v1.6.0.info" → "v1.6.0.info"
		fileName := ae.RelativePath
		if idx := strings.LastIndex(ae.RelativePath, "/"); idx >= 0 {
			fileName = ae.RelativePath[idx+1:]
		}

		entries = append(entries, buildlayout.VirtualEntry{
			VirtualDisplayPath: cacheDisplayPath,
			VirtualFilePath:    fileName,
			OriginalFilePath:   "", // Synthetic: no original file
			BlobHash:           gocache.DeterministicHash(realModulePath, version, ae.Type),
			Size:               ae.Size, // Pre-computed where possible, 0 for on-demand
			Mode:               0444,    // Read-only
			Source:             source,
			BackendMetadata:    ae.Metadata,
		})
	}

	return entries
}
