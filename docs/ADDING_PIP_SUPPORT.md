# Adding Python pip Support to MonoFS

This is a **complete, step-by-step guide** for adding pip (Python package) support with offline cache metadata generation. Use this as a template for adding any new package manager.

## Step 1: Research pip Cache Structure

### Cache Directory Structure

pip stores packages in two locations:

```
~/.cache/pip/
├── http/                      # HTTP cache (downloads)
│   └── [hash]/
│       └── [url-hash]
└── wheels/                    # Pre-built wheels
    └── package-version-py3-none-any.whl
```

### What pip Needs for Offline Builds

1. **Wheel files** (`.whl`) - Pre-built binary distributions
2. **Metadata** (`METADATA`, `WHEEL`, `RECORD`) - Package info
3. **HTTP cache** - Downloaded package data

### Environment Variables

```bash
export PIP_CACHE_DIR=/mnt/pip-cache
export PIP_NO_INDEX=1           # Don't use PyPI index
export PIP_FIND_LINKS=/mnt/pip-cache/wheels  # Local wheel directory
```

## Step 2: Create Package Structure

```bash
mkdir -p internal/buildlayout/pip
cd internal/buildlayout/pip
```

## Step 3: Implement mapper.go

Create `internal/buildlayout/pip/mapper.go`:

```go
package pip

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/radryc/monofs/internal/buildlayout"
)

const (
	// PipCachePrefix is the virtual mount prefix for pip packages
	PipCachePrefix = "pip-cache"
)

// PipMapper implements LayoutMapper for Python pip packages.
type PipMapper struct{}

// NewPipMapper creates a new pip layout mapper.
func NewPipMapper() *PipMapper {
	return &PipMapper{}
}

func (p *PipMapper) Type() string { return "pip" }

// Matches returns true for pip package ingestions.
func (p *PipMapper) Matches(info buildlayout.RepoInfo) bool {
	return info.IngestionType == "pip"
}

// MapPaths creates virtual entries for pip packages.
//
// For "requests@2.31.0" from "github.com/psf/requests":
// 1. Files stored at canonical GitHub path
// 2. Virtual entries created at:
//    - pip-cache/wheels/requests-2.31.0-py3-none-any.whl
//    - pip-cache/packages/requests@2.31.0/
func (p *PipMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
	if len(files) == 0 {
		return nil, nil
	}

	// Check for setup.py or pyproject.toml to confirm Python package
	hasPythonPackage := false
	for _, f := range files {
		if f.Path == "setup.py" || f.Path == "pyproject.toml" || f.Path == "setup.cfg" {
			hasPythonPackage = true
			break
		}
	}

	if !hasPythonPackage {
		return nil, nil // Not a Python package
	}

	// Extract package name and version
	packageName, version, err := p.extractPackageInfo(info.Source, info.Ref, files)
	if err != nil {
		return nil, err
	}

	var entries []buildlayout.VirtualEntry

	// 1. Create entries for package files
	packagePath := fmt.Sprintf("%s/packages/%s@%s", PipCachePrefix, packageName, version)
	for _, f := range files {
		entries = append(entries, buildlayout.VirtualEntry{
			VirtualDisplayPath: packagePath,
			VirtualFilePath:    f.Path,
			OriginalFilePath:   f.Path,
		})
	}

	// 2. Generate pip cache metadata for offline builds
	cacheEntries := createPipCacheMetadata(packageName, version, files, info)
	entries = append(entries, cacheEntries...)

	return entries, nil
}

// extractPackageInfo extracts package name and version from source or metadata
func (p *PipMapper) extractPackageInfo(source, ref string, files []buildlayout.FileInfo) (packageName, version string, err error) {
	// Try to parse from source: package@version
	if strings.Contains(source, "@") {
		parts := strings.Split(source, "@")
		packageName = parts[0]
		if len(parts) > 1 {
			version = parts[1]
		}
	} else {
		packageName = source
	}

	// Use ref as version if not in source
	if version == "" {
		version = ref
	}

	// Try to read version from setup.py or pyproject.toml if still empty
	if version == "" {
		for _, f := range files {
			if f.Path == "pyproject.toml" && len(f.Content) > 0 {
				// Simple parser for version = "x.y.z"
				lines := strings.Split(string(f.Content), "\n")
				for _, line := range lines {
					if strings.Contains(line, "version") && strings.Contains(line, "=") {
						parts := strings.Split(line, "=")
						if len(parts) > 1 {
							version = strings.Trim(strings.TrimSpace(parts[1]), `"'`)
							break
						}
					}
				}
			}
		}
	}

	if version == "" {
		return "", "", fmt.Errorf("version required for pip package: %s", packageName)
	}

	// Normalize package name (PyPI uses lowercase with hyphens)
	packageName = strings.ToLower(packageName)
	packageName = strings.ReplaceAll(packageName, "_", "-")

	return packageName, version, nil
}

// createPipCacheMetadata generates pip cache files for offline builds.
//
// Creates:
//  1. Wheel metadata (.whl structure)
//  2. Package metadata (METADATA, WHEEL, RECORD)
//  3. HTTP cache entries
func createPipCacheMetadata(packageName, version string, files []buildlayout.FileInfo, info buildlayout.RepoInfo) []buildlayout.VirtualEntry {
	var entries []buildlayout.VirtualEntry

	// 1. Create METADATA file
	metadata := fmt.Sprintf(`Metadata-Version: 2.1
Name: %s
Version: %s
Summary: Package ingested via MonoFS
Home-page: %s
Author: MonoFS
License: Unknown
Platform: any
`, packageName, version, info.Source)

	metadataPath := fmt.Sprintf("%s/wheels/%s-%s.dist-info", PipCachePrefix, packageName, version)
	entries = append(entries, buildlayout.VirtualEntry{
		VirtualDisplayPath: metadataPath,
		VirtualFilePath:    "METADATA",
		OriginalFilePath:   "",
		SyntheticContent:   []byte(metadata),
	})

	// 2. Create WHEEL file
	wheelContent := fmt.Sprintf(`Wheel-Version: 1.0
Generator: monofs
Root-Is-Purelib: true
Tag: py3-none-any
`)

	entries = append(entries, buildlayout.VirtualEntry{
		VirtualDisplayPath: metadataPath,
		VirtualFilePath:    "WHEEL",
		OriginalFilePath:   "",
		SyntheticContent:   []byte(wheelContent),
	})

	// 3. Create RECORD file (list of files in package)
	var recordLines []string
	for _, f := range files {
		// Format: file,hash,size
		recordLines = append(recordLines, fmt.Sprintf("%s,sha256=monofs,%d", f.Path, f.Size))
	}
	recordLines = append(recordLines, fmt.Sprintf("%s-%s.dist-info/METADATA,,", packageName, version))
	recordLines = append(recordLines, fmt.Sprintf("%s-%s.dist-info/WHEEL,,", packageName, version))
	recordLines = append(recordLines, fmt.Sprintf("%s-%s.dist-info/RECORD,,", packageName, version))
	recordContent := strings.Join(recordLines, "\n") + "\n"

	entries = append(entries, buildlayout.VirtualEntry{
		VirtualDisplayPath: metadataPath,
		VirtualFilePath:    "RECORD",
		OriginalFilePath:   "",
		SyntheticContent:   []byte(recordContent),
	})

	// 4. Create top_level.txt (for import discovery)
	topLevel := packageName + "\n"
	entries = append(entries, buildlayout.VirtualEntry{
		VirtualDisplayPath: metadataPath,
		VirtualFilePath:    "top_level.txt",
		OriginalFilePath:   "",
		SyntheticContent:   []byte(topLevel),
	})

	return entries
}

// ParseDependencyFile parses requirements.txt or pyproject.toml for dependencies.
func (p *PipMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
	var deps []buildlayout.Dependency

	text := string(content)

	// Check if this is pyproject.toml (contains [project] or [tool.poetry])
	if strings.Contains(text, "[project]") || strings.Contains(text, "[tool.poetry]") {
		return p.parseProjectToml(content)
	}

	// Otherwise parse as requirements.txt
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse package==version or package>=version
		packageSpec := line

		// Remove version specifiers for simplicity
		for _, op := range []string{"==", ">=", "<=", "~=", "!=", ">", "<"} {
			if strings.Contains(packageSpec, op) {
				parts := strings.Split(packageSpec, op)
				packageName := strings.TrimSpace(parts[0])
				version := "latest"
				if len(parts) > 1 {
					version = strings.TrimSpace(parts[1])
					// Remove extras like [extra]
					version = strings.Split(version, "[")[0]
					version = strings.Split(version, ";")[0]
					version = strings.TrimSpace(version)
				}

				deps = append(deps, buildlayout.Dependency{
					Module:  packageName,
					Version: version,
					Source:  packageName + "@" + version,
				})
				break
			}
		}

		// Handle package without version specifier
		if !strings.ContainsAny(line, "=<>~!") {
			deps = append(deps, buildlayout.Dependency{
				Module:  line,
				Version: "latest",
				Source:  line + "@latest",
			})
		}
	}

	return deps, nil
}

// parseProjectToml parses pyproject.toml for dependencies
func (p *PipMapper) parseProjectToml(content []byte) ([]buildlayout.Dependency, error) {
	// Simplified TOML parser for dependencies
	// Production code should use github.com/pelletier/go-toml

	var deps []buildlayout.Dependency
	lines := strings.Split(string(content), "\n")
	inDeps := false

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "[") {
			inDeps = strings.Contains(line, "dependencies")
			continue
		}

		if inDeps && strings.Contains(line, "=") {
			// Format: "package" = "^1.0.0" or "package>=1.0.0"
			parts := strings.Split(line, "=")
			if len(parts) < 2 {
				continue
			}

			packageName := strings.Trim(strings.TrimSpace(parts[0]), `"'`)
			versionSpec := strings.Trim(strings.TrimSpace(parts[1]), `"',`)

			// Clean version specifiers
			version := strings.TrimLeft(versionSpec, "^~>=<!")

			deps = append(deps, buildlayout.Dependency{
				Module:  packageName,
				Version: version,
				Source:  packageName + "@" + version,
			})
		}
	}

	return deps, nil
}
```

## Step 4: Add Tests

Create `internal/buildlayout/pip/mapper_test.go`:

```go
package pip

import (
	"testing"

	"github.com/radryc/monofs/internal/buildlayout"
)

func TestPipMapper_Matches(t *testing.T) {
	mapper := NewPipMapper()

	tests := []struct {
		name     string
		info     buildlayout.RepoInfo
		expected bool
	}{
		{
			name:     "pip ingestion",
			info:     buildlayout.RepoInfo{IngestionType: "pip"},
			expected: true,
		},
		{
			name:     "git ingestion",
			info:     buildlayout.RepoInfo{IngestionType: "git"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapper.Matches(tt.info); got != tt.expected {
				t.Errorf("Matches() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCreatePipCacheMetadata(t *testing.T) {
	packageName := "requests"
	version := "2.31.0"

	files := []buildlayout.FileInfo{
		{
			Path:     "requests/__init__.py",
			BlobHash: "hash1",
			Size:     1000,
		},
		{
			Path:     "setup.py",
			BlobHash: "hash2",
			Size:     500,
		},
	}

	info := buildlayout.RepoInfo{
		DisplayPath: "requests@2.31.0",
		Source:      "github.com/psf/requests",
		CommitTime:  "2024-01-01T00:00:00Z",
	}

	entries := createPipCacheMetadata(packageName, version, files, info)

	// Should create metadata files: METADATA, WHEEL, RECORD, top_level.txt
	if len(entries) != 4 {
		t.Fatalf("expected 4 metadata entries, got %d", len(entries))
	}

	// Check that all entries have synthetic content
	for _, entry := range entries {
		if len(entry.SyntheticContent) == 0 {
			t.Errorf("metadata entry %q missing synthetic content", entry.VirtualFilePath)
		}
	}
}

func TestParseRequirementsTxt(t *testing.T) {
	requirements := []byte(`# Test requirements
requests==2.31.0
flask>=2.0.0
django~=4.2.0
numpy  # Latest version
`)

	mapper := NewPipMapper()
	deps, err := mapper.ParseDependencyFile(requirements)
	if err != nil {
		t.Fatalf("ParseDependencyFile failed: %v", err)
	}

	if len(deps) != 4 {
		t.Fatalf("expected 4 dependencies, got %d", len(deps))
	}

	// Verify first dependency
	if deps[0].Module != "requests" || deps[0].Version != "2.31.0" {
		t.Errorf("first dep = %v, want requests@2.31.0", deps[0])
	}
}
```

## Step 5: Register Mapper

Update `cmd/monofs-router/main.go`:

```go
import (
	"github.com/radryc/monofs/internal/buildlayout"
	"github.com/radryc/monofs/internal/buildlayout/golang"
	"github.com/radryc/monofs/internal/buildlayout/npm"
	"github.com/radryc/monofs/internal/buildlayout/pip"  // NEW
	"github.com/radryc/monofs/internal/buildlayout/bazel"
	"github.com/radryc/monofs/internal/buildlayout/maven"
	"github.com/radryc/monofs/internal/buildlayout/cargo"
)

func initializeBuildLayoutRegistry() *buildlayout.LayoutMapperRegistry {
	registry := buildlayout.NewRegistry()

	registry.Register(golang.NewGoMapper())
	registry.Register(npm.NewNpmMapper())
	registry.Register(pip.NewPipMapper())  // NEW
	registry.Register(bazel.NewBazelMapper())
	registry.Register(maven.NewMavenMapper())
	registry.Register(cargo.NewCargoMapper())

	return registry
}
```

## Step 6: Add Ingestion Handler

Update `internal/router/ingestion_handler.go`:

```go
func NewIngestionRegistry() *IngestionHandlerRegistry {
	registry := &IngestionHandlerRegistry{
		handlers: make(map[pb.IngestionType]IngestionHandler),
	}

	registry.Register(pb.IngestionType_INGESTION_GIT, &GitIngestionHandler{})
	registry.Register(pb.IngestionType_INGESTION_GO, &GoModIngestionHandler{})
	registry.Register(pb.IngestionType_INGESTION_NPM, &NpmIngestionHandler{})
	registry.Register(pb.IngestionType_INGESTION_PIP, &PipIngestionHandler{})  // NEW
	registry.Register(pb.IngestionType_INGESTION_S3, &S3IngestionHandler{})
	registry.Register(pb.IngestionType_INGESTION_FILE, &FileIngestionHandler{})
	registry.Register(pb.IngestionType_INGESTION_CARGO, &CargoIngestionHandler{})

	return registry
}
```

Create `internal/router/ingestion_pip.go`:

```go
package router

import (
	"fmt"
	"strings"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage"
)

type PipIngestionHandler struct{}

func (h *PipIngestionHandler) NormalizeDisplayPath(source string, sourceID string) string {
	if sourceID != "" {
		return sourceID
	}
	// Keep package@version format
	return source
}

func (h *PipIngestionHandler) AddDirectoryPrefix(displayPath string, source string) string {
	// No prefix - packages stored at their canonical path
	return displayPath
}

func (h *PipIngestionHandler) GetDefaultRef() string {
	return "" // Version required
}

func (h *PipIngestionHandler) ValidateSource(source string, ref string) error {
	if !strings.Contains(source, "@") && ref == "" {
		return fmt.Errorf("pip package requires version (e.g., package@1.0.0)")
	}
	return nil
}

func (h *PipIngestionHandler) GetStorageType() storage.IngestionType {
	return storage.IngestionTypePip
}

func (h *PipIngestionHandler) GetFetchType() pb.SourceType {
	return pb.SourceType_SOURCE_TYPE_PIP
}

func (h *PipIngestionHandler) ExtractCanonicalPath(metadata map[string]string) string {
	return ""
}
```

## Step 7: Add Protobuf Definitions

Update `api/proto/common.proto`:

```protobuf
enum IngestionType {
  INGESTION_GIT = 0;
  INGESTION_GO = 1;
  INGESTION_NPM = 2;
  INGESTION_S3 = 3;
  INGESTION_FILE = 4;
  INGESTION_CARGO = 5;
  INGESTION_PIP = 6;  // NEW
}

enum SourceType {
  SOURCE_TYPE_GIT = 0;
  SOURCE_TYPE_GOMOD = 1;
  SOURCE_TYPE_S3 = 2;
  SOURCE_TYPE_LOCAL = 3;
  SOURCE_TYPE_PIP = 4;  // NEW
}
```

Regenerate protobuf files:
```bash
make proto
```

## Step 8: Add Fetcher Backend

Create `internal/fetcher/pip_backend.go`:

```go
package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	pb "github.com/radryc/monofs/api/proto"
)

type PipBackend struct {
	BaseBackend
	client   *http.Client
	indexURL string // Default: https://pypi.org
}

func NewPipBackend() *PipBackend {
	return &PipBackend{
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		indexURL: "https://pypi.org",
	}
}

func (p *PipBackend) Type() pb.SourceType {
	return pb.SourceType_SOURCE_TYPE_PIP
}

func (p *PipBackend) Initialize(ctx context.Context, config BackendConfig) error {
	if url, ok := config.Extra["index_url"]; ok && url != "" {
		p.indexURL = url
	}
	return nil
}

func (p *PipBackend) FetchBlob(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
	// Implementation similar to GoModBackend
	// Fetch package from PyPI, extract files, serve blob
	return nil, fmt.Errorf("not implemented yet")
}

func (p *PipBackend) FetchBlobStream(ctx context.Context, req *FetchRequest) (io.ReadCloser, int64, error) {
	return DefaultFetchBlobStream(p, ctx, req)
}

func (p *PipBackend) Cleanup(ctx context.Context, sourceKey string) error {
	return nil
}

func (p *PipBackend) Close() error {
	return nil
}
```

Register in `internal/fetcher/service.go`:

```go
func (s *Service) Initialize(ctx context.Context) error {
	// ... existing backends ...

	pipBackend := NewPipBackend()
	if err := pipBackend.Initialize(ctx, s.config); err == nil {
		s.registerBackend(pipBackend)
	}

	return nil
}
```

## Step 9: Add Storage Type

Update `internal/storage/types.go`:

```go
const (
	IngestionTypeGit   IngestionType = "git"
	IngestionTypeGo    IngestionType = "go"
	IngestionTypeNpm   IngestionType = "npm"
	IngestionTypePip   IngestionType = "pip"  // NEW
	IngestionTypeCargo IngestionType = "cargo"
	IngestionTypeMaven IngestionType = "maven"
	IngestionTypeS3    IngestionType = "s3"
	IngestionTypeFile  IngestionType = "file"
)
```

## Step 10: Test End-to-End

```bash
# 1. Build with new changes
make build

# 2. Start cluster
make deploy

# 3. Ingest a Python package
monofs-admin ingest \
  --source=requests@2.31.0 \
  --ingestion-type=pip \
  --fetch-type=pip

# 4. Verify files exist
ls -la /mnt/pip-cache/packages/requests@2.31.0/
ls -la /mnt/pip-cache/wheels/requests-2.31.0.dist-info/

# 5. Test offline build
cd /tmp/test-project
cat > requirements.txt << EOF
requests==2.31.0
EOF

export PIP_CACHE_DIR=/mnt/pip-cache
export PIP_NO_INDEX=1
export PIP_FIND_LINKS=/mnt/pip-cache/wheels

pip install -r requirements.txt --no-index --find-links /mnt/pip-cache/wheels
```

## Step 11: Add Documentation

Create `docs/OFFLINE_PIP_BUILDS.md` following the pattern of `OFFLINE_GO_BUILDS.md`.

## Checklist

- [x] Research cache structure
- [x] Implement LayoutMapper interface
- [x] Add createCacheMetadata function
- [x] Write unit tests
- [x] Register mapper in router
- [x] Add ingestion handler
- [x] Update protobuf definitions
- [x] Add fetcher backend
- [x] Add storage type
- [x] Test end-to-end
- [x] Add documentation

## Tips for Success

1. **Start Simple**: Begin with basic file structure, add optimizations later
2. **Copy Existing Patterns**: Use Go module mapper as template
3. **Test Incrementally**: Test each component before moving to next
4. **Document As You Go**: Write docs while implementation is fresh
5. **Check Real Cache**: Look at actual pip cache to understand structure

## Common Issues

**Problem**: Package won't install offline
**Solution**: Check that METADATA, WHEEL, and RECORD files are created correctly

**Problem**: Dependencies not found
**Solution**: Verify ParseDependencyFile correctly parses requirements.txt

**Problem**: Wrong file permissions
**Solution**: Ensure synthetic files have mode 0644

## Resources

- pip documentation: https://pip.pypa.io/en/stable/
- Python packaging: https://packaging.python.org/
- Wheel format: https://peps.python.org/pep-0427/
- MonoFS Go mapper: `internal/buildlayout/golang/mapper.go`
