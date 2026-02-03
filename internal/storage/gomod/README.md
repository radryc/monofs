# Go Module Storage Backend

This package implements storage backends for Go modules, following the same pattern as the Git storage backends.

## Overview

The Go module storage backend enables MonoFS to ingest and serve files from Go modules published to Go module proxies (like proxy.golang.org). This allows you to mount Go modules as filesystems.

## Components

### 1. GoModIngestionBackend (`ingestion.go`)

Implements the `storage.IngestionBackend` interface for Go modules.

**Features:**
- Downloads module ZIP files from Go module proxies (GOPROXY)
- Extracts and indexes all files in the module
- Computes SHA256 hashes for content addressing
- Validates module existence before ingestion
- Caches downloaded modules for reuse

**Usage:**
```go
backend := gomod.NewGoModIngestionBackend()

config := map[string]string{
    "cache_dir": "/path/to/cache",
    "proxy": "https://proxy.golang.org", // optional
}

// Initialize with module@version format
err := backend.Initialize(ctx, "github.com/google/uuid@v1.3.0", config)

// Walk all files
backend.WalkFiles(ctx, func(meta storage.FileMetadata) error {
    fmt.Printf("File: %s (size: %d, hash: %s)\n", 
        meta.Path, meta.Size, meta.ContentHash)
    return nil
})
```

### 2. GoModFetchBackend (`fetch.go`)

Implements the `storage.FetchBackend` interface for Go modules.

**Features:**
- Stores and retrieves blob content by SHA256 hash
- Local disk caching for blob data
- Support for both in-memory and streaming operations
- Configurable cache directory

**Usage:**
```go
backend := gomod.NewGoModFetchBackend()

config := map[string]string{
    "cache_dir": "/path/to/cache",
}

err := backend.Initialize(ctx, config)

// Store a blob
blobID, err := backend.StoreBlob(ctx, []byte("content"))

// Fetch it back
data, err := backend.FetchBlob(ctx, blobID)
```

## Configuration

### Environment Variables

- `GOPROXY`: Go module proxy URL (defaults to https://proxy.golang.org)

### Config Parameters

#### Ingestion Backend
- `cache_dir`: Directory to cache downloaded module ZIPs (default: `/tmp/monofs-gomod-cache`)
- `proxy`: Override GOPROXY setting (optional)

#### Fetch Backend
- `cache_dir`: Directory to store blob cache (required)
- `proxy`: Go module proxy URL (optional)

## Module URL Format

Go modules are specified in the format: `module@version`

Examples:
- `github.com/google/uuid@v1.3.0`
- `golang.org/x/crypto@v0.17.0`
- `github.com/gin-gonic/gin@v1.9.1`

## Implementation Details

### Module ZIP Structure

Go module proxies serve modules as ZIP files with the structure:
```
module@version/
  ├── file1.go
  ├── file2.go
  └── subdir/
      └── file3.go
```

The ingestion backend:
1. Downloads the ZIP from `{proxy}/{module}/@v/{version}.zip`
2. Extracts metadata for all files
3. Strips the `module@version/` prefix from paths
4. Computes SHA256 hashes for content addressing

### Content Addressing

Unlike Git which uses Git blob hashes, the Go module backend uses SHA256 hashes of file content for blob IDs. This provides:
- Standard cryptographic hashing
- Cross-platform compatibility
- Easy verification

### Validation

The `Validate` method checks module availability by querying the proxy's info endpoint:
```
{proxy}/{module}/@v/{version}.info
```

This returns JSON with version information:
```json
{
  "Version": "v1.3.0",
  "Time": "2022-08-31T15:04:05Z"
}
```

## Testing

Run tests with:
```bash
go test ./internal/storage/gomod -v
```

Tests include:
- Module validation
- Initialization and download
- File walking and metadata extraction
- Blob storage and retrieval
- Error handling for invalid modules

**Note:** Tests require network access to Go module proxies. Set `SKIP_NETWORK_TESTS=1` to skip network-dependent tests.

## Integration

The backends are registered in `cmd/monofs-router/main.go`:

```go
import gomodstorage "github.com/radryc/monofs/internal/storage/gomod"

func init() {
    storage.DefaultRegistry.RegisterIngestionBackend(
        storage.IngestionTypeGo,
        gomodstorage.NewGoModIngestionBackend,
    )
    storage.DefaultRegistry.RegisterFetchBackend(
        storage.FetchTypeGoMod,
        gomodstorage.NewGoModFetchBackend,
    )
}
```

## Future Improvements

1. **Streaming ZIP Extraction**: Currently loads entire file content for hashing; could stream for large files
2. **Proxy Failover**: Support multiple proxy URLs with automatic failover
3. **Module Resolution**: Support version ranges and semantic version resolution
4. **Direct Repository Fetch**: Fall back to git clone if module not in proxy
5. **Incremental Updates**: Support fetching newer versions efficiently
6. **Go Directive Parsing**: Parse go.mod file to understand module structure
7. **Private Module Support**: Handle authentication for private Go modules

## Comparison with Git Backend

| Feature | Git Backend | Go Module Backend |
|---------|-------------|-------------------|
| Source | Git repositories | Go module proxies |
| Content Hash | Git blob SHA1 | SHA256 |
| Cloning | Full git clone | Download ZIP |
| Updates | Git fetch/pull | Download new version |
| History | Full commit history | Single version snapshot |
| Branches | Multi-branch support | Version-based |
| Private Access | SSH keys, credentials | GOPROXY auth |

## Examples

### Ingesting a Go Module

```go
// Create ingestion backend
ingestion := gomod.NewGoModIngestionBackend()

// Initialize with module
err := ingestion.Initialize(ctx, "github.com/gin-gonic/gin@v1.9.1", map[string]string{
    "cache_dir": "/var/cache/monofs/gomod",
})

// Walk files and index them
err = ingestion.WalkFiles(ctx, func(meta storage.FileMetadata) error {
    // Index file metadata in your storage system
    return indexFile(meta)
})
```

### Fetching Module Content

```go
// Create fetch backend
fetch := gomod.NewGoModFetchBackend()

// Initialize
err := fetch.Initialize(ctx, map[string]string{
    "cache_dir": "/var/cache/monofs/blobs",
})

// Fetch blob by hash
content, err := fetch.FetchBlob(ctx, "7146f812d758...")
```

## Architecture

```
┌─────────────────────────────────────────┐
│         MonoFS Router/Server            │
├─────────────────────────────────────────┤
│     Storage Abstraction Layer           │
│  ┌──────────────┐  ┌─────────────────┐  │
│  │  Ingestion   │  │  Fetch          │  │
│  │  Backend     │  │  Backend        │  │
│  └──────────────┘  └─────────────────┘  │
└─────────────────────────────────────────┘
              │              │
    ┌─────────┴──────┐  ┌───┴──────────┐
    │                │  │              │
┌───▼────────┐  ┌────▼──▼───┐  ┌──────▼──────┐
│ Go Module  │  │   Local    │  │   GOPROXY   │
│   Proxy    │  │   Cache    │  │  (network)  │
└────────────┘  └────────────┘  └─────────────┘
```

## License

Same as MonoFS project.
