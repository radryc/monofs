# Adding Cache Metadata Support for New Build Systems

This guide shows how to add offline build support (cache metadata generation) for new package managers like npm, pip, Maven, etc.

## Overview

To enable offline builds, a build system needs **two** things:
1. **Extracted package files** - the actual source code
2. **Cache metadata** - manifest/lock files that build tools need to recognize packages

MonoFS automates this through the `LayoutMapper` interface.

## Architecture

```
LayoutMapper.MapPaths(info, files) returns:
  ├── Regular VirtualEntry (references original blobs)
  │   - OriginalFilePath: "index.js"
  │   - SyntheticContent: nil
  │
  └── Cache Metadata VirtualEntry (new synthetic files)
      - OriginalFilePath: ""
      - SyntheticContent: []byte("{...}")  <-- NEW!
```

### Key Concepts

1. **Synthetic Content**: Files that don't exist in the original package but are needed by build tools
2. **Cache Structure**: Each package manager has a specific cache directory layout
3. **Metadata Files**: Manifest files (package.json, .info, pom.xml, etc.) stored in cache

## Implementation Pattern

### Step 1: Identify Cache Requirements

Research what the build tool needs for offline operation:

**Go (`GOMODCACHE`):**
```
$GOMODCACHE/
├── module@version/          # Extracted files
└── cache/download/
    └── module/@v/
        ├── version.info     # {"Version":"...","Time":"..."}
        ├── version.mod      # go.mod content
        └── list             # List of versions
```

**npm (`npm_config_cache`):**
```
~/.npm/
├── _cacache/               # Content-addressed storage
│   └── content-v2/
│       └── sha512/
│           └── [hash]      # Package tarball
└── _locks/
```

**pip (`PIP_CACHE_DIR`):**
```
~/.cache/pip/
├── http/                   # HTTP cache
└── wheels/                 # Pre-built wheels
    └── package-version-py3-none-any.whl
```

**Maven (`MAVEN_REPO`):**
```
~/.m2/repository/
└── group/artifact/version/
    ├── artifact-version.pom        # POM file
    ├── artifact-version.jar        # JAR file
    └── _remote.repositories        # Metadata
```

### Step 2: Create Mapper Implementation

Create `internal/buildlayout/[ecosystem]/mapper.go`:

```go
package npm

import (
	"encoding/json"
	"fmt"
	"github.com/radryc/monofs/internal/buildlayout"
)

type NpmMapper struct{}

func NewNpmMapper() *NpmMapper {
	return &NpmMapper{}
}

func (n *NpmMapper) Type() string { return "npm" }

func (n *NpmMapper) Matches(info buildlayout.RepoInfo) bool {
	return info.IngestionType == "npm"
}

func (n *NpmMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
	// Parse package@version
	packageName, version := parseNpmPackage(info.DisplayPath)

	var entries []buildlayout.VirtualEntry

	// 1. Add regular package files
	for _, f := range files {
		entries = append(entries, buildlayout.VirtualEntry{
			VirtualDisplayPath: fmt.Sprintf("node_modules/%s", packageName),
			VirtualFilePath:    f.Path,
			OriginalFilePath:   f.Path,
		})
	}

	// 2. Generate cache metadata for offline builds
	cacheEntries := n.createCacheMetadata(packageName, version, files, info)
	entries = append(entries, cacheEntries...)

	return entries, nil
}

// createCacheMetadata generates npm cache files for offline operation
func (n *NpmMapper) createCacheMetadata(packageName, version string, files []buildlayout.FileInfo, info buildlayout.RepoInfo) []buildlayout.VirtualEntry {
	var entries []buildlayout.VirtualEntry

	// Find package.json
	var pkgJSON []byte
	for _, f := range files {
		if f.Path == "package.json" {
			pkgJSON = f.Content
			break
		}
	}

	if pkgJSON == nil {
		// Create minimal package.json
		pkgJSON = []byte(fmt.Sprintf(`{"name":"%s","version":"%s"}`, packageName, version))
	}

	// npm cache structure: _cacache/index-v5/[hash]
	// Simplified: store package.json in predictable location
	entries = append(entries, buildlayout.VirtualEntry{
		VirtualDisplayPath: fmt.Sprintf("npm-cache/%s/%s", packageName, version),
		VirtualFilePath:    "package.json",
		OriginalFilePath:   "",
		SyntheticContent:   pkgJSON,
	})

	return entries
}

func (n *NpmMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}

	if err := json.Unmarshal(content, &pkg); err != nil {
		return nil, err
	}

	var deps []buildlayout.Dependency
	for name, version := range pkg.Dependencies {
		deps = append(deps, buildlayout.Dependency{
			Module:  name,
			Version: version,
			Source:  fmt.Sprintf("%s@%s", name, version),
		})
	}

	return deps, nil
}
```

### Step 3: Add Tests

Create `internal/buildlayout/[ecosystem]/cache_metadata_test.go`:

```go
package npm

import (
	"testing"
	"github.com/radryc/monofs/internal/buildlayout"
)

func TestCreateCacheMetadata(t *testing.T) {
	files := []buildlayout.FileInfo{
		{
			Path:    "package.json",
			Content: []byte(`{"name":"express","version":"4.18.0"}`),
		},
		{
			Path:     "index.js",
			BlobHash: "hash1",
		},
	}

	info := buildlayout.RepoInfo{
		DisplayPath: "express@4.18.0",
		CommitTime:  "2024-01-01T00:00:00Z",
	}

	mapper := NewNpmMapper()
	entries := mapper.createCacheMetadata("express", "4.18.0", files, info)

	if len(entries) == 0 {
		t.Fatal("expected cache metadata entries")
	}

	// Verify synthetic content
	for _, entry := range entries {
		if len(entry.SyntheticContent) == 0 {
			t.Errorf("cache entry %q missing synthetic content", entry.VirtualFilePath)
		}
	}
}
```

### Step 4: Register Mapper

Update `cmd/monofs-router/main.go`:

```go
import (
	"github.com/radryc/monofs/internal/buildlayout"
	"github.com/radryc/monofs/internal/buildlayout/golang"
	"github.com/radryc/monofs/internal/buildlayout/npm"     // NEW
	"github.com/radryc/monofs/internal/buildlayout/bazel"
	"github.com/radryc/monofs/internal/buildlayout/maven"
	"github.com/radryc/monofs/internal/buildlayout/cargo"
)

func initializeBuildLayoutRegistry() *buildlayout.LayoutMapperRegistry {
	registry := buildlayout.NewRegistry()

	// Register all mappers
	registry.Register(golang.NewGoMapper())
	registry.Register(npm.NewNpmMapper())        // NEW
	registry.Register(bazel.NewBazelMapper())
	registry.Register(maven.NewMavenMapper())
	registry.Register(cargo.NewCargoMapper())

	return registry
}
```

## Real-World Examples

### npm/yarn (Node.js)

**Cache Structure:**
```
npm-cache/
└── [scope/]package@version/
    ├── package.json           # Manifest
    ├── package-lock.json      # Lock file
    └── .npm-metadata.json     # npm metadata
```

**Key Metadata Files:**
- `package.json` - Package manifest
- `.npm-metadata.json` - Distribution info (tarball URL, shasum)

**Build Tool Settings:**
```bash
export NPM_CONFIG_CACHE=/mnt/npm-cache
export NPM_CONFIG_PREFER_OFFLINE=true
npm install  # Uses cache
```

### pip (Python)

**Cache Structure:**
```
pip-cache/
├── http/                      # HTTP cache
└── wheels/
    └── package-version-py3-none-any.whl
```

**Key Metadata Files:**
- `.whl` files - Pre-built wheels
- `METADATA` - Package metadata
- `RECORD` - File inventory

**Build Tool Settings:**
```bash
export PIP_CACHE_DIR=/mnt/pip-cache
export PIP_NO_INDEX=1
pip install -r requirements.txt
```

### Maven (Java)

**Cache Structure:**
```
maven-repo/
└── com/example/artifact/1.0.0/
    ├── artifact-1.0.0.pom     # Project Object Model
    ├── artifact-1.0.0.jar     # JAR file
    ├── artifact-1.0.0.pom.sha1
    └── _remote.repositories   # Repository metadata
```

**Key Metadata Files:**
- `.pom` - Maven POM file
- `.sha1`/`.md5` - Checksums
- `_remote.repositories` - Repository info

**Build Tool Settings:**
```bash
export MAVEN_REPO=/mnt/maven-repo
mvn -o package  # Offline build
```

### Cargo (Rust)

**Cache Structure:**
```
cargo-home/
├── registry/
│   ├── index/                 # Crate index
│   └── cache/
│       └── crate-version.crate
└── git/
    └── db/                    # Git dependencies
```

**Key Metadata Files:**
- `.crate` files - Compressed source archives
- `config.json` - Crate metadata
- Index files - Crate versions

**Build Tool Settings:**
```bash
export CARGO_HOME=/mnt/cargo-home
export CARGO_NET_OFFLINE=true
cargo build
```

### Bazel

**Cache Structure:**
```
bazel-cache/
└── repos/
    └── rule_name/
        ├── WORKSPACE          # Workspace file
        ├── BUILD              # Build file
        └── repo.bzl           # Repository rule
```

**Key Metadata Files:**
- `WORKSPACE` - External dependencies
- `MODULE.bazel` - Module definition (Bzlmod)
- `*.bzl` - Repository rules

**Build Tool Settings:**
```bash
export BAZEL_REPOSITORY_CACHE=/mnt/bazel-cache
bazel build --repository_cache=/mnt/bazel-cache //...
```

## Testing Offline Builds

### Test Script Template

```bash
#!/bin/bash
# test_offline_[ecosystem].sh

set -e

echo "=== Testing Offline [Ecosystem] Builds ==="

# 1. Ingest dependencies
monofs-admin ingest-deps \
  --file=[manifest-file] \
  --type=[ecosystem]

# 2. Verify cache metadata exists
echo "Checking cache metadata..."
ls -la /mnt/[cache-dir]/@v/ || {
  echo "ERROR: Cache metadata not found!"
  exit 1
}

# 3. Configure offline environment
export [CACHE_VAR]=/mnt/[cache-dir]
export [OFFLINE_VAR]=true

# 4. Attempt offline build
echo "Building offline..."
[build-command] || {
  echo "ERROR: Offline build failed!"
  exit 1
}

echo "✅ Offline build succeeded!"
```

### Example: Testing npm

```bash
#!/bin/bash
set -e

# Ingest npm dependencies
monofs-admin ingest-deps --file=package.json --type=npm

# Verify cache
ls -la /mnt/npm-cache/express/4.18.0/package.json

# Configure offline
export NPM_CONFIG_CACHE=/mnt/npm-cache
export NPM_CONFIG_PREFER_OFFLINE=true
export NPM_CONFIG_OFFLINE=true

# Test build
cd /tmp/test-project
npm install  # Should use cache only

echo "✅ npm offline build works!"
```

## Checklist for New Backend

- [ ] Research cache directory structure
- [ ] Identify required metadata files
- [ ] Implement `LayoutMapper` interface
- [ ] Add `createCacheMetadata()` function
- [ ] Write unit tests for cache generation
- [ ] Add integration test for offline builds
- [ ] Register mapper in router
- [ ] Document environment variables
- [ ] Add to OFFLINE_BUILDS.md
- [ ] Update ADDING_INGESTION_TYPES.md

## Common Pitfalls

1. **Forgetting Content Field**: Set `FileInfo.Content` for manifest files in ingestion
2. **Wrong Cache Path**: Research exact cache structure expected by tool
3. **Missing Checksums**: Some tools require SHA checksums in metadata
4. **Version Format**: Respect semantic versioning or custom version schemes
5. **Symbolic Links**: Some caches use symlinks (npm), handle appropriately

## Advanced: Content-Addressed Storage

Some package managers (npm, Bazel) use content-addressed storage where files are stored by hash:

```go
func (n *NpmMapper) createCacheMetadata(...) []buildlayout.VirtualEntry {
	// Compute content hash
	contentHash := sha512.Sum512(packageContent)
	hashHex := hex.EncodeToString(contentHash[:])

	// Store in content-addressed location
	cachePath := fmt.Sprintf("_cacache/content-v2/sha512/%s/%s",
		hashHex[:2], hashHex[2:4])

	return []buildlayout.VirtualEntry{{
		VirtualDisplayPath: cachePath,
		VirtualFilePath:    hashHex,
		SyntheticContent:   packageContent,
	}}
}
```

## Getting Help

- Example: `internal/buildlayout/golang/mapper.go`
- Tests: `internal/buildlayout/golang/cache_metadata_test.go`
- Docs: `docs/OFFLINE_GO_BUILDS.md`
- Issues: https://github.com/anthropics/monofs/issues
