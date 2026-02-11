# Offline Go Builds with MonoFS - Deep Dive

Go-specific deep dive for building Go projects 100% offline using MonoFS.

**Note**: For a multi-language overview covering Go, npm, Cargo, and Bazel, see [OFFLINE_BUILDS.md](OFFLINE_BUILDS.md).

## Overview

This document explains how MonoFS enables completely offline Go builds by automatically generating complete Go module cache metadata during ingestion.

## Architecture

When you ingest Go modules, MonoFS now creates **both**:

1. **Extracted module files**: `go-modules/pkg/mod/path@version/*.go`
2. **Cache metadata**: `go-modules/pkg/mod/cache/download/path/@v/version.{info,mod,list}`

This allows Go builds with `GOPROXY=off` to work completely offline.

## How It Works

### During Ingestion

When you run:
```bash
monofs-admin ingest-deps --file=go.mod --type=go
```

The ingestion process:
1. Fetches each module from the Go proxy
2. Stores module source files under `go-modules/pkg/mod/module@version/`
3. **Automatically generates** cache metadata files:
   - `version.info` - JSON with version and timestamp
   - `version.mod` - Copy of the module's go.mod
   - `list` - List of available versions

### Directory Structure Created

```
/mnt/go-modules/pkg/mod/
├── google.golang.org/
│   ├── grpc@v1.75.0/          # Extracted module files
│   │   ├── go.mod
│   │   ├── *.go
│   │   └── ...
│   └── protobuf@v1.36.8/
│       └── ...
└── cache/
    └── download/
        └── google.golang.org/
            ├── grpc/@v/
            │   ├── v1.75.0.info  # {"Version":"v1.75.0","Time":"..."}
            │   ├── v1.75.0.mod   # Copy of go.mod
            │   └── list          # v1.75.0\n
            └── protobuf/@v/
                ├── v1.36.8.info
                ├── v1.36.8.mod
                └── list
```

## Usage Workflow

### One-Time Setup (Internet Host)

On a host with internet access:

```bash
# 1. Start MonoFS cluster
make deploy

# 2. Ingest your project's repository
monofs-admin ingest \
  --source=https://github.com/your/project \
  --ref=main

# 3. Ingest ALL Go module dependencies
# This automatically creates cache metadata!
monofs-admin ingest-deps \
  --file=/mnt/github.com/your/project/go.mod \
  --type=go \
  --concurrency=10

# 4. Verify modules are ingested
ls -la /mnt/go-modules/pkg/mod/
ls -la /mnt/go-modules/pkg/mod/cache/download/google.golang.org/grpc/@v/
```

### Offline Builds (All Hosts)

On any host (even without internet):

```bash
# 1. Mount MonoFS (read-only is fine)
monofs-client \
  --router=router-host:9090 \
  --mount=/mnt

# 2. Set environment for offline builds
export GOMODCACHE="/mnt/go-modules/pkg/mod"
export GOPROXY="off"
export GOVCS="*:off"
export GOSUMDB="off"
export GOTOOLCHAIN="local"

# 3. Build your project - completely offline!
cd /mnt/github.com/your/project
go build -o bin/app ./cmd/app
```

Or use the helper:

```bash
cd /mnt/github.com/your/project
eval $(monofs-session setup .)
go build -o bin/app ./cmd/app
```

## Docker Workflow

### Build Image (Internet Host)

```dockerfile
FROM golang:1.24-alpine

# Mount MonoFS with pre-ingested modules
VOLUME /mnt

# Configure offline builds
ENV GOMODCACHE=/mnt/go-modules/pkg/mod
ENV GOPROXY=off
ENV GOVCS=*:off
ENV GOSUMDB=off
ENV GOTOOLCHAIN=local

WORKDIR /app
COPY . .

# Build completely offline using MonoFS
RUN go build -o /bin/app ./cmd/app

CMD ["/bin/app"]
```

### Run Container

```bash
# Start MonoFS client as sidecar or init container
monofs-client --mount=/mnt-shared &

# Run build container with MonoFS mount
docker run -v /mnt-shared:/mnt your-image
```

## Implementation Details

### Code Changes

**1. Enhanced VirtualEntry Structure** (`internal/buildlayout/types.go`):
```go
type VirtualEntry struct {
    VirtualDisplayPath string
    VirtualFilePath    string
    OriginalFilePath   string
    SyntheticContent   []byte  // NEW: For cache metadata files
}
```

**2. Go Mapper Creates Cache Metadata** (`internal/buildlayout/golang/mapper.go`):
- `MapPaths()` now calls `createCacheMetadata()`
- Generates `.info`, `.mod`, and `list` files as synthetic entries

**3. Router Handles Synthetic Files** (`internal/router/layout.go`):
- Detects `VirtualEntry.SyntheticContent`
- Computes blob hash
- Stores content in `BackendMetadata` for retrieval

**4. Dockerfile Fixed** (`Dockerfile`):
- Changed `GOVCS=off` → `GOVCS=*:off`
- Updated environment scripts

**5. monofs-session Fixed** (`cmd/monofs-session/main.go`):
- Changed `GOPROXY=file://...,direct` → `GOPROXY=off`
- Added `GOVCS=*:off`

## Benefits

1. **True Offline Builds**: No network access needed after initial ingestion
2. **One-Time Setup**: Ingest once, build everywhere
3. **Consistent Environments**: All hosts use identical module versions
4. **Fast Builds**: No proxy downloads during build
5. **Air-Gapped Support**: Works in isolated networks

## Troubleshooting

### Go Still Tries to Download

**Problem**: `go: downloading module@version`

**Solution**: Check environment variables:
```bash
go env | grep -E "GOMODCACHE|GOPROXY|GOVCS"
```

Should show:
```
GOMODCACHE=/mnt/go-modules/pkg/mod
GOPROXY=off
GOVCS=*:off
```

### Module Not Found

**Problem**: `module lookup disabled by GOPROXY=off`

**Solution**: Verify cache metadata exists:
```bash
ls /mnt/go-modules/pkg/mod/cache/download/path/to/module/@v/
```

If missing, re-ingest with the updated code:
```bash
# Delete old ingestion
monofs-admin delete --name=module@version

# Re-ingest (will create metadata)
monofs-admin ingest --source=module@version --ingestion-type=go
```

### go.mod Toolchain Mismatch

**Problem**: `go: downloading go1.24.3 for linux/amd64: toolchain not available`

**Solution**: Use local toolchain:
```bash
export GOTOOLCHAIN=local
```

Or update go.mod:
```bash
# Remove toolchain directive
sed -i '/^toolchain/d' go.mod
```

## Testing

### Verify Cache Metadata Generation

```bash
# Ingest a single module
monofs-admin ingest \
  --source=github.com/google/uuid@v1.6.0 \
  --ingestion-type=go \
  --fetch-type=gomod

# Check extracted files
ls -la /mnt/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/

# Check cache metadata (NEW!)
ls -la /mnt/go-modules/pkg/mod/cache/download/github.com/google/uuid/@v/

# Should see:
# v1.6.0.info
# v1.6.0.mod
# list
```

### Test Offline Build

```bash
# Create test project
mkdir -p /tmp/test-offline
cd /tmp/test-offline

cat > go.mod << 'EOF'
module example.com/test

go 1.24

require github.com/google/uuid v1.6.0
EOF

cat > main.go << 'EOF'
package main
import (
    "fmt"
    "github.com/google/uuid"
)
func main() {
    fmt.Println("ID:", uuid.New())
}
EOF

# Set offline environment
export GOMODCACHE=/mnt/go-modules/pkg/mod
export GOPROXY=off
export GOVCS=*:off
export GOSUMDB=off
export GOTOOLCHAIN=local

# Build (should work without internet!)
go build -o test ./

# Run
./test
```

## Future Enhancements

1. **ZIP Files**: Generate `.zip` files for full compatibility
2. **sum.golang.org**: Generate local checksum database
3. **Proxy Protocol**: Implement Go proxy protocol for transparent integration
4. **Automatic Updates**: Detect go.mod changes and auto-ingest new deps

## References

- Go Module Cache: https://go.dev/ref/mod#module-cache
- GOMODCACHE: https://go.dev/doc/go1.15#go-command
- Offline Builds: https://go.dev/ref/mod#offline
