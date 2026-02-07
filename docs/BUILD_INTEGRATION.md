# MonoFS Build Integration

MonoFS now supports automatic generation of build tool layouts, enabling build systems (Go, Bazel, npm, Maven, Cargo) to read modules/packages directly from the FUSE mount with zero network transfer and zero cache duplication.

## Features

✅ **Zero-Copy Virtual Layouts**: Virtual layout files reference the same blob hashes as original files - no data duplication  
✅ **Backend-Side Generation**: All virtual paths exist in server NutsDB databases, not in FUSE  
✅ **Modular Plugin Architecture**: Add new build systems by implementing one interface  
✅ **Automatic Deduplication**: Same blob content served regardless of how many virtual paths reference it  
✅ **Hot Reload**: Re-ingestion updates both original and virtual layouts  

## Quick Start

### 1. Ingest a Go Module

```bash
# Ingest with automatic virtual layout generation
./bin/monofs-admin ingest \
  --router=localhost:9090 \
  --source=github.com/google/uuid@v1.6.0 \
  --ingestion-type=go \
  --fetch-type=gomod
```

This creates:
- Original: `/mnt/monofs/github.com/google/uuid@v1.6.0/`
- Virtual:  `/mnt/monofs/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/`

### 2. Bulk Ingest Dependencies

```bash
# Ingest all dependencies from go.mod
./bin/monofs-admin ingest-deps \
  --file=go.mod \
  --type=go \
  --concurrency=10 \
  --skip-existing=true
```

### 3. Use with Go Build

```bash
# Mount MonoFS
mkdir -p /tmp/monofs
./bin/monofs-client --mount=/tmp/monofs --router=localhost:9090

# Use Go module cache from MonoFS
export GOMODCACHE=/tmp/monofs/go-modules/pkg/mod
cd your-project
go build ./...
```

## Architecture

```
INGESTION FLOW:

1. Admin CLI → Router.IngestRepository()
   ├─> Storage backend downloads/clones repo
   ├─> WalkFiles() yields file metadata
   ├─> Router distributes files to server nodes via HRW sharding
   ├─> Server stores in NutsDB
   └─> **NEW**: Router calls LayoutMapper.MapPaths()
       ├─> Input: RepoInfo + file list
       ├─> Output: VirtualEntry[] with new display paths
       └─> Router ingests virtual entries using existing IngestFileBatch RPC
           (same blob_hash, different displayPath + storageID)

RESULT:

/mnt/monofs/
├── github.com/google/uuid@v1.6.0/       ← original (from ingestion)
│   ├── uuid.go
│   └── go.mod
└── go-modules/pkg/mod/                   ← virtual (from LayoutMapper)
    └── github.com/google/uuid@v1.6.0/
        ├── uuid.go                       ← same blob_hash as original
        └── go.mod                        ← same blob_hash as original
```

## Implementation Details

### Key Components

1. **`internal/buildlayout/types.go`**: Core interfaces and registry
2. **`internal/buildlayout/golang/mapper.go`**: Go module layout mapper
3. **`internal/router/layout.go`**: Post-ingestion layout generation
4. **`internal/router/ingest.go`**: Integration with ingestion pipeline

### Go Module Layout Mapper

The Go mapper creates virtual entries under `go-modules/pkg/mod/` matching `$GOMODCACHE` layout:

- **Case Encoding**: `github.com/Azure/sdk` → `github.com/!azure/sdk`
- **Version Handling**: Supports both `module@version` and separate version in ref
- **Dependency Parsing**: Reads `go.mod` to extract all required dependencies

### Adding New Build Systems

To add support for npm, Maven, Cargo, etc.:

1. Create `internal/buildlayout/<system>/mapper.go`
2. Implement the `LayoutMapper` interface:
   ```go
   type LayoutMapper interface {
       Type() string
       Matches(info RepoInfo) bool
       MapPaths(info RepoInfo, files []FileInfo) ([]VirtualEntry, error)
       ParseDependencyFile(content []byte) ([]Dependency, error)
   }
   ```
3. Register in `cmd/monofs-router/main.go`:
   ```go
   layoutRegistry.Register(npmMapper.NewNPMMapper())
   ```

## CLI Reference

### `monofs-admin ingest`

Ingest a repository or module:

```bash
monofs-admin ingest \
  --router=localhost:9090 \
  --source=<source> \
  --ingestion-type=<type> \
  --fetch-type=<type> \
  [--ref=<version>]
```

**Ingestion types**: `git`, `go`, `s3`, `file`  
**Fetch types**: `git`, `gomod`, `s3`, `local`

### `monofs-admin ingest-deps`

Bulk ingest all dependencies from a manifest:

```bash
monofs-admin ingest-deps \
  --file=<manifest> \
  --type=<type> \
  [--concurrency=<N>] \
  [--skip-existing=true]
```

**Supported types**: `go` (more coming soon: `npm`, `maven`, `cargo`)

Options:
- `--file`: Path to manifest file (e.g., `go.mod`, `package.json`)
- `--type`: Dependency type
- `--concurrency`: Max parallel ingestions (default: 5)
- `--skip-existing`: Skip already-ingested dependencies (default: true)

## Design Principles

1. **Backend-side generation**: Virtual paths exist in server NutsDB, not FUSE
2. **Zero FUSE changes**: FUSE sees virtual repos as regular repos
3. **Blob deduplication**: Same `blob_hash` → fetcher serves content once
4. **Modular architecture**: One interface implementation per build system
5. **Fast re-ingestion**: Metadata-only updates, blob hashes unchanged

## Testing

```bash
# Run unit tests
go test -v ./internal/buildlayout/...
go test -v ./internal/buildlayout/golang/...
go test -v ./internal/router/...

# Build all components
make build

# Integration test (requires running cluster)
./bin/monofs-admin ingest --source=github.com/google/uuid@v1.6.0 --ingestion-type=go --fetch-type=gomod
ls /mnt/monofs/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/
```

## Performance Characteristics

- **Ingestion overhead**: ~5-10% (virtual entry generation)
- **Storage overhead**: Only metadata (path mappings), zero blob duplication
- **Read performance**: Identical to original files (same blob hash lookups)
- **Network usage**: Zero (fetcher cache serves all blob requests)

## Roadmap

- [ ] npm/yarn support (`node_modules/` layout)
- [ ] Maven support (`.m2/repository/` layout)
- [ ] Cargo support (`.cargo/registry/` layout)
- [ ] Bazel support (external workspace layout)
- [ ] Python pip support (site-packages layout)
- [ ] Version conflict resolution strategies
- [ ] Nested dependency resolution
- [ ] Lock file support for reproducible builds

## Limitations

1. Currently only supports Go modules
2. No version conflict resolution (uses first ingested version)
3. No support for replace/exclude directives in go.mod
4. Virtual layouts are generated at ingestion time only

## Contributing

To add support for a new build system:

1. Study the build system's cache layout (e.g., `$NPM_CONFIG_CACHE`, `.m2/repository/`)
2. Create mapper in `internal/buildlayout/<system>/`
3. Implement `LayoutMapper` interface
4. Add tests covering typical use cases
5. Register mapper in router initialization
6. Update documentation with examples

## References

- [BUILD_INTEGRATION_TODO.md](BUILD_INTEGRATION_TODO.md) - Complete implementation guide
- [Go module cache layout](https://go.dev/ref/mod#module-cache) - Official Go documentation
- [npm cache layout](https://docs.npmjs.com/cli/v9/configuring-npm/folders) - npm documentation

---

**Status**: Phase 1-3 Complete ✅  
**Next**: Add npm, Maven, and Cargo support
