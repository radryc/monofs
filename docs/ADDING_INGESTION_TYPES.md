# Adding New Ingestion Types

The ingestion system has been refactored to be modular and extensible. Adding a new package manager or source type is now straightforward.

## Overview

The ingestion system uses a **handler pattern** where each ingestion type has its own handler that implements the `IngestionHandler` interface.

## Architecture

- **`ingestion_handler.go`**: Interface definition and registry
- **`ingestion_git.go`**: Git repository handler
- **`ingestion_go.go`**: Go modules handler
- **`ingestion_npm.go`**: npm packages handler
- **`ingestion_cargo.go`**: Cargo crates handler
- **`ingestion_maven.go`**: Maven artifacts handler
- **`ingestion_s3.go`**: S3 buckets handler

Each handler is in its own file for easy maintenance and extension.

## Adding a New Ingestion Type

### Step 1: Define the Protobuf Type

Add your new type to `api/proto/monofs.proto`:

```protobuf
enum IngestionType {
  INGESTION_GIT = 0;
  INGESTION_GO = 1;
  INGESTION_NPM = 2;
  INGESTION_CARGO = 3;
  INGESTION_MAVEN = 4;
  INGESTION_S3 = 5;
  INGESTION_PYPI = 6;  // New type
}
```

### Step 2: Add Storage Types

Add corresponding types to `internal/storage/types.go`:

```go
const (
    IngestionTypePypi IngestionType = "pypi"
    FetchTypePypi     FetchType     = "fetch_pypi"
)
```

### Step 3: Create the Handler File

Create a new file `internal/router/ingestion_pypi.go`:

```go
package router

import (
    "fmt"
    "strings"
    
    pb "github.com/radryc/monofs/api/proto"
    "github.com/radryc/monofs/internal/storage"
)

// PypiIngestionHandler handles Python package ingestion from PyPI.
type PypiIngestionHandler struct{}

func (h *PypiIngestionHandler) Type() pb.IngestionType {
    return pb.IngestionType_INGESTION_PYPI
}

func (h *PypiIngestionHandler) NormalizeDisplayPath(source string, sourceID string) string {
    if sourceID != "" {
        return sourceID
    }
    // PyPI packages are in package@version or package==version format
    // Normalize to package@version
    return strings.ReplaceAll(source, "==", "@")
}

func (h *PypiIngestionHandler) AddDirectoryPrefix(displayPath string, source string) string {
    // Python packages go under .venv/lib/python3.x/site-packages/
    // For simplicity, we'll use a flatter structure
    return "site-packages/" + displayPath
}

func (h *PypiIngestionHandler) GetDefaultRef() string {
    return "latest"
}

func (h *PypiIngestionHandler) ValidateSource(source string, ref string) error {
    if source == "" {
        return fmt.Errorf("package name is required")
    }
    return nil
}

func (h *PypiIngestionHandler) GetStorageType() storage.IngestionType {
    return storage.IngestionTypePypi
}

func (h *PypiIngestionHandler) GetFetchType() storage.FetchType {
    return storage.FetchTypePypi
}
```

### Step 4: Register the Handler

Add your handler to the registry in `ingestion_handler.go` in `NewIngestionRegistry()`:

```go
func NewIngestionRegistry() *IngestionRegistry {
    r := &IngestionRegistry{
        handlers: make(map[pb.IngestionType]IngestionHandler),
    }

    // Register all handlers
    r.Register(&GitIngestionHandler{})
    r.Register(&GoModIngestionHandler{})
    r.Register(&NpmIngestionHandler{})
    r.Register(&CargoIngestionHandler{})
    r.Register(&MavenIngestionHandler{})
    r.Register(&S3IngestionHandler{})
    r.Register(&PypiIngestionHandler{})  // New handler

    return r
}
```

### Step 5: Implement the Fetcher Backend

Create `internal/fetcher/pypi_backend.go`:

```go
package fetcher

import (
    "context"
    // ... imports
)

type PypiBackend struct {
    config BackendConfig
    client *http.Client
    // ... fields
}

func (pb *PypiBackend) Type() SourceType {
    return SourceTypePypi
}

func (pb *PypiBackend) Initialize(ctx context.Context, config BackendConfig) error {
    // Setup logic
    return nil
}

func (pb *PypiBackend) FetchBlob(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
    // Fetch package from PyPI
    return &FetchResult{}, nil
}

// ... implement other required methods
```

### Step 6: Register the Fetcher Backend

Add to `internal/fetcher/service.go`:

```go
func (s *Service) Initialize(ctx context.Context) error {
    // ... existing code
    
    // Register PyPI backend
    pypiBackend := NewPypiBackend()
    if err := pypiBackend.Initialize(ctx, pypiConfig); err != nil {
        return fmt.Errorf("failed to initialize PyPI backend: %w", err)
    }
    s.backends[SourceTypePypi] = pypiBackend
    
    return nil
}
```

## That's It!

Your new ingestion type is now fully integrated. Users can ingest packages with:

```bash
monofs-router ingest requests@2.28.0 --type pypi
```

## Interface Reference

### IngestionHandler Interface

```go
type IngestionHandler interface {
    // Type returns the ingestion type this handler supports
    Type() pb.IngestionType

    // NormalizeDisplayPath converts the source to a display path
    // source: original source string (e.g., "package@1.0.0")
    // sourceID: custom ID provided by user (optional)
    NormalizeDisplayPath(source string, sourceID string) string

    // AddDirectoryPrefix adds the proper directory prefix for this type
    // displayPath: normalized package name
    // source: original source for reference
    AddDirectoryPrefix(displayPath string, source string) string

    // GetDefaultRef returns the default ref/version if not specified
    GetDefaultRef() string

    // ValidateSource validates the source format
    ValidateSource(source string, ref string) error

    // GetStorageType returns the storage ingestion type
    GetStorageType() storage.IngestionType

    // GetFetchType returns the storage fetch type
    GetFetchType() storage.FetchType
}
```

## Examples

### Existing Handlers

- **Git**: No prefix, normalizes URLs to `github.com/owner/repo`
- **npm**: Prefix `node_modules/`, keeps `package@version` format
- **Cargo**: Prefix `.cargo/registry/src/`, converts `@` to `-`
- **Maven**: Prefix `.m2/repository/`, converts `groupId:artifactId:version` to proper path
- **Go Modules**: No prefix, uses `module@version` format
- **S3**: No prefix, uses bucket name as-is

### Testing Your Handler

```bash
# Build
make build

# Ingest a package
./bin/monofs-router ingest <package-spec> --type <your-type>

# Verify in mounted filesystem
ls -la /mnt/monofs/<your-prefix>/
```
