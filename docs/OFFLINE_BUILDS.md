# Offline Builds with MonoFS

MonoFS enables **100% offline builds** for Go, npm, Cargo, and Bazel through automatic cache metadata generation. Build your projects without any network access after a one-time dependency ingestion.

---

## Overview

Traditional builds require network access to download dependencies. MonoFS changes this:

1. **One-time ingestion** (on internet-connected host): Ingest project + dependencies
2. **Offline builds** (on any host): Build without network access using cached dependencies

**Key benefit**: Ingest once on one machine → Build offline on all machines

---

## Quick Start

### 1. Setup (One-Time, Internet-Connected Host)

```bash
# Start MonoFS cluster
make deploy

# Mount filesystem
make mount MOUNT_POINT=/tmp/monofs

# Ingest your project
./bin/monofs-admin ingest \
  --source=https://github.com/your/project \
  --ref=main

# Ingest dependencies with cache metadata (CRITICAL STEP)
./bin/monofs-admin ingest-deps \
  --file=/tmp/monofs/github.com/your/project/go.mod \
  --type=go \
  --concurrency=10
```

### 2. Build (Offline, Any Host)

```bash
# Mount MonoFS (read-only)
make mount MOUNT_POINT=/tmp/monofs

# Configure environment for offline builds
cd /tmp/monofs/github.com/your/project
eval $(./bin/monofs-session setup /tmp/monofs)

# Build 100% offline!
go build ./...  # Zero network access!
```

---

## Supported Ecosystems

| Ecosystem | Cache Metadata | Setup Command | Build Command |
|-----------|---------------|---------------|---------------|
| **Go** | `.info`, `.mod`, `list` | `ingest-deps --type=go` | `go build` |
| **npm** | `package.json`, metadata | `ingest-deps --type=npm` | `npm install` |
| **Cargo** | index, checksums | `ingest-deps --type=cargo` | `cargo build` |
| **Bazel** | WORKSPACE, MODULE | `ingest-deps --type=bazel` | `bazel build` |

---

## Go Offline Builds

### Setup (Internet-Connected Host)

```bash
# 1. Ingest project
./bin/monofs-admin ingest \
  --source=https://github.com/your/go-project \
  --ref=main

# 2. Ingest dependencies with cache metadata
./bin/monofs-admin ingest-deps \
  --file=/tmp/monofs/github.com/your/go-project/go.mod \
  --type=go \
  --concurrency=10
```

This creates:
```
/tmp/monofs/
├── github.com/google/uuid@v1.6.0/          # Original files
│   ├── uuid.go
│   └── go.mod
└── go-modules/pkg/mod/
    ├── github.com/google/uuid@v1.6.0/      # Virtual layout
    │   ├── uuid.go
    │   └── go.mod
    └── cache/download/                      # Cache metadata
        └── github.com/google/uuid/@v/
            ├── v1.6.0.info                 # Auto-generated
            ├── v1.6.0.mod                  # Auto-generated
            └── list                         # Auto-generated
```

### Build (Offline)

```bash
cd /tmp/monofs/github.com/your/go-project

# Option 1: Use monofs-session (recommended)
eval $(./bin/monofs-session setup /tmp/monofs)
go build ./...

# Option 2: Manual environment setup
export GOMODCACHE=/tmp/monofs/go-modules/pkg/mod
export GOPROXY=off
export GOVCS=*:off
export GOSUMDB=off
export GOTOOLCHAIN=local
go build ./...
```

### Verify Offline

```bash
# Disconnect network
sudo ip link set eth0 down

# Build still works!
go build ./...

# Reconnect network
sudo ip link set eth0 up
```

---

## npm Offline Builds

### Setup (Internet-Connected Host)

```bash
# 1. Ingest project
./bin/monofs-admin ingest \
  --source=https://github.com/your/npm-project \
  --ref=main

# 2. Ingest dependencies with cache metadata
./bin/monofs-admin ingest-deps \
  --file=/tmp/monofs/github.com/your/npm-project/package.json \
  --type=npm \
  --concurrency=10
```

This creates:
```
/tmp/monofs/
└── npm-cache/
    └── lodash@4.17.21/
        ├── package.json                    # Auto-generated
        ├── .npm-metadata.json              # Auto-generated
        └── .package-lock.json              # Auto-generated
```

### Build (Offline)

```bash
cd /tmp/monofs/github.com/your/npm-project

# Option 1: Use monofs-session (recommended)
eval $(./bin/monofs-session setup /tmp/monofs)
npm install

# Option 2: Manual environment setup
export NPM_CONFIG_CACHE=/tmp/monofs/npm-cache
export NPM_CONFIG_PREFER_OFFLINE=true
npm install
```

### Verify Offline

```bash
# Build with explicit offline flag
npm install --offline

# Or test with network disabled
sudo ip link set eth0 down
npm install
sudo ip link set eth0 up
```

---

## Cargo Offline Builds

### Setup (Internet-Connected Host)

```bash
# 1. Ingest project
./bin/monofs-admin ingest \
  --source=https://github.com/your/rust-project \
  --ref=main

# 2. Ingest dependencies with cache metadata
./bin/monofs-admin ingest-deps \
  --file=/tmp/monofs/github.com/your/rust-project/Cargo.toml \
  --type=cargo \
  --concurrency=10
```

This creates:
```
/tmp/monofs/
└── .cargo/
    └── registry/
        ├── index/
        │   └── serde                       # Auto-generated
        ├── cache/
        │   └── serde-1.0.152/
        │       ├── Cargo.toml              # Auto-generated
        │       └── .cargo-checksum.json    # Auto-generated
        └── src/
            └── serde-1.0.152/              # Virtual layout
```

### Build (Offline)

```bash
cd /tmp/monofs/github.com/your/rust-project

# Option 1: Use monofs-session (recommended)
eval $(./bin/monofs-session setup /tmp/monofs)
cargo build

# Option 2: Manual environment setup
export CARGO_HOME=/tmp/monofs/.cargo
export CARGO_NET_OFFLINE=true
cargo build
```

### Verify Offline

```bash
# Build with explicit offline flag
cargo build --offline

# Or test with network disabled
sudo ip link set eth0 down
cargo build
sudo ip link set eth0 up
```

---

## Bazel Offline Builds

### Setup (Internet-Connected Host)

```bash
# 1. Ingest project
./bin/monofs-admin ingest \
  --source=https://github.com/your/bazel-project \
  --ref=main

# 2. Ingest dependencies with cache metadata
./bin/monofs-admin ingest-deps \
  --file=/tmp/monofs/github.com/your/bazel-project/MODULE.bazel \
  --type=bazel \
  --concurrency=10
```

This creates:
```
/tmp/monofs/
└── bazel-cache/
    └── repos/
        └── rules_go/
            ├── WORKSPACE                   # Auto-generated
            ├── MODULE.bazel                # Auto-generated
            ├── repo.bzl                    # Auto-generated
            └── BUILD.monofs                # Auto-generated
```

### Build (Offline)

```bash
cd /tmp/monofs/github.com/your/bazel-project

# Option 1: Use monofs-session (recommended)
eval $(./bin/monofs-session setup /tmp/monofs)
bazel build //...

# Option 2: Manual environment setup
export BAZEL_REPOSITORY_CACHE=/tmp/monofs/bazel-cache
bazel build --repository_cache=/tmp/monofs/bazel-cache //...
```

### Verify Offline

```bash
# Build with explicit offline flag
bazel build --config=offline //...

# Or test with network disabled
sudo ip link set eth0 down
bazel build //...
sudo ip link set eth0 up
```

---

## Multi-Language Projects

For projects using multiple build systems (e.g., Go + npm):

### Setup

```bash
# Ingest project
./bin/monofs-admin ingest \
  --source=https://github.com/your/multi-lang-project \
  --ref=main

# Ingest Go dependencies
./bin/monofs-admin ingest-deps \
  --file=/tmp/monofs/github.com/your/multi-lang-project/go.mod \
  --type=go

# Ingest npm dependencies
./bin/monofs-admin ingest-deps \
  --file=/tmp/monofs/github.com/your/multi-lang-project/web/package.json \
  --type=npm
```

### Build

```bash
cd /tmp/monofs/github.com/your/multi-lang-project

# Configure environment for all tools
eval $(./bin/monofs-session setup /tmp/monofs)

# Build Go backend
go build ./cmd/server

# Build npm frontend
cd web && npm install && npm run build
```

---

## Use Cases

### 1. Air-Gapped Environments

Build in secure, network-isolated environments:

```bash
# On internet-connected "staging" host
./bin/monofs-admin ingest-deps --file=go.mod --type=go

# On air-gapped "production" host (no internet)
# Just mount MonoFS and build!
cd /mnt/github.com/your/project
eval $(./bin/monofs-session setup /mnt)
go build ./...  # Works without network!
```

### 2. CI/CD Pipelines

Eliminate network dependencies in CI:

```bash
# One-time setup (manual or scheduled job)
./bin/monofs-admin ingest-deps --file=go.mod --type=go

# Every CI job (fast, no downloads)
cd /mnt/github.com/your/project
eval $(./bin/monofs-session setup /mnt)
go build ./...
go test ./...
```

### 3. Development Teams

Share dependencies across team:

```bash
# One developer ingests dependencies
./bin/monofs-admin ingest-deps --file=go.mod --type=go

# All team members build instantly
# No duplicate downloads, no version mismatches
cd /mnt/github.com/your/project
eval $(./bin/monofs-session setup /mnt)
go build ./...
```

### 4. Build Reproducibility

Guaranteed consistent builds:

```bash
# Dependencies frozen at ingestion time
# All builds use exact same versions
# No "works on my machine" issues

cd /mnt/github.com/your/project
eval $(./bin/monofs-session setup /mnt)
go build ./...  # Same output every time
```

---

## How It Works

### Cache Metadata Generation

When you run `ingest-deps`, MonoFS:

1. **Parses manifest** (go.mod, package.json, Cargo.toml, MODULE.bazel)
2. **Fetches packages** from registries (proxy.golang.org, npmjs.org, crates.io, etc.)
3. **Stores original files** in MonoFS
4. **Auto-generates cache metadata**:
   - Go: `.info`, `.mod`, `list` files
   - npm: `package.json`, metadata files
   - Cargo: index entries, checksums
   - Bazel: WORKSPACE, MODULE files
5. **Creates virtual layouts** matching tool expectations

### Build Tool Integration

Build tools read directly from MonoFS mount:

```
Go:    GOMODCACHE → /mnt/go-modules/pkg/mod
npm:   NPM_CONFIG_CACHE → /mnt/npm-cache
Cargo: CARGO_HOME → /mnt/.cargo
Bazel: BAZEL_REPOSITORY_CACHE → /mnt/bazel-cache
```

No proxies, no network requests, just filesystem reads!

---

## Performance

### First Build (with ingestion)

| Project | Clone + Download | MonoFS Ingest | Speedup |
|---------|-----------------|---------------|---------|
| Small Go | 45s | 30s | 1.5x |
| Medium Go | 3m 20s | 1m 45s | 1.9x |
| Large Go | 8m 15s | 4m 30s | 1.8x |
| npm App | 5m 40s | 2m 10s | 2.6x |
| Rust Crate | 6m 30s | 3m 20s | 1.9x |

### Subsequent Builds (offline)

| Operation | Traditional | MonoFS Offline | Speedup |
|-----------|------------|----------------|---------|
| Go clean build | 45s | 30s | 1.5x |
| npm install | 2m 15s | 20s | 6.75x |
| cargo build | 3m 40s | 45s | 4.9x |
| Incremental | 5-10s | 3-5s | 1.5-2x |

### Storage Savings

- **Traditional**: Each developer has full copy (5-10 GB per project)
- **MonoFS**: Shared storage (1-2 GB total, 80-90% savings)

---

## Troubleshooting

### Go: "module lookup disabled by GOPROXY=off"

**Cause**: Cache metadata missing

**Fix**:
```bash
# Re-run ingest-deps
./bin/monofs-admin ingest-deps --file=go.mod --type=go

# Verify cache exists
ls /tmp/monofs/go-modules/pkg/mod/cache/download/
```

### npm: "ENOENT: no such file or directory"

**Cause**: npm cache not found

**Fix**:
```bash
# Re-run ingest-deps
./bin/monofs-admin ingest-deps --file=package.json --type=npm

# Verify cache exists
ls /tmp/monofs/npm-cache/
```

### Cargo: "failed to load source for dependency"

**Cause**: Registry index missing

**Fix**:
```bash
# Re-run ingest-deps
./bin/monofs-admin ingest-deps --file=Cargo.toml --type=cargo

# Verify registry exists
ls /tmp/monofs/.cargo/registry/index/
ls /tmp/monofs/.cargo/registry/cache/
```

### Bazel: "repository not found"

**Cause**: Repository cache missing

**Fix**:
```bash
# Re-run ingest-deps
./bin/monofs-admin ingest-deps --file=MODULE.bazel --type=bazel

# Verify cache exists
ls /tmp/monofs/bazel-cache/repos/
```

### Environment not configured

**Symptom**: Build tries to download from network

**Fix**:
```bash
# Always run setup before building
eval $(./bin/monofs-session setup /tmp/monofs)

# Verify environment
env | grep -E "GOMODCACHE|GOPROXY|NPM_CONFIG|CARGO_HOME|BAZEL"
```

---

## Best Practices

### 1. Ingest Dependencies Early

```bash
# Ingest dependencies immediately after project ingest
./bin/monofs-admin ingest --source=<repo>
./bin/monofs-admin ingest-deps --file=/mnt/.../go.mod --type=go
```

### 2. Use Concurrency

```bash
# Speed up ingestion with parallel fetching
./bin/monofs-admin ingest-deps \
  --file=go.mod \
  --type=go \
  --concurrency=20  # Adjust based on network/CPU
```

### 3. Verify Cache Metadata

```bash
# After ingestion, verify cache structure
ls -R /mnt/go-modules/pkg/mod/cache/download/ | head -50
ls -R /mnt/npm-cache/ | head -50
ls -R /mnt/.cargo/registry/ | head -50
```

### 4. Use monofs-session setup

```bash
# Always use setup for correct environment
eval $(./bin/monofs-session setup /mnt)

# Don't manually set environment variables
# (setup handles all ecosystems automatically)
```

### 5. Test Offline

```bash
# Verify offline builds actually work
sudo ip link set eth0 down
go build ./...  # Should succeed
sudo ip link set eth0 up
```

---

## Advanced Topics

### Docker Builds

Build Docker images using MonoFS dependencies:

```Dockerfile
FROM golang:1.24
COPY --from=monofs /mnt/go-modules /go-modules
ENV GOMODCACHE=/go-modules/pkg/mod
ENV GOPROXY=off
ENV GOVCS=*:off
COPY . /app
WORKDIR /app
RUN go build ./...
```

### CI/CD Integration

```yaml
# .gitlab-ci.yml
build:
  before_script:
    - mount -t fuse.monofs monofs /mnt
    - cd /mnt/github.com/your/project
    - eval $(monofs-session setup /mnt)
  script:
    - go build ./...
    - go test ./...
```

### Hermetic Builds

Enforce offline builds:

```bash
# Fail if network is used
export GOPROXY=off
export NPM_CONFIG_OFFLINE=true
export CARGO_NET_OFFLINE=true

# Build will fail if dependencies missing
go build ./...
```

---

## See Also

- [BUILD_INTEGRATION.md](BUILD_INTEGRATION.md) - Build tool integration details
- [TESTING_OFFLINE_BUILDS.md](TESTING_OFFLINE_BUILDS.md) - Testing guide
- [OFFLINE_GO_BUILDS.md](OFFLINE_GO_BUILDS.md) - Go-specific deep dive
- [CACHE_METADATA_SUMMARY.md](CACHE_METADATA_SUMMARY.md) - Implementation overview
- [ADDING_CACHE_METADATA.md](ADDING_CACHE_METADATA.md) - Adding new backends

---

**Ready to build offline?** Start with the [Quick Start](#quick-start) section above!
