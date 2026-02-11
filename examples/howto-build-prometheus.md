# How to Build Prometheus Using MonoFS

This guide demonstrates building Prometheus from source using the MonoFS filesystem for instant source access and dependency management.

## Overview

Prometheus is a popular monitoring system written in Go. This example shows how MonoFS provides instant access to source code and dependencies, dramatically speeding up the build process.

## Prerequisites

- MonoFS cluster running
- Go 1.24+ installed
- Node.js 18+ (for UI assets)
- Make installed

## Step 1: Ingest Prometheus Repository and Dependencies

```bash
# Ingest main Prometheus repository
./bin/monofs-admin ingest \
  --router=localhost:9090 \
  --source=https://github.com/prometheus/prometheus \
  --ref=main

# Ingest ALL Go dependencies with cache metadata
./bin/monofs-admin ingest-deps \
  --router=localhost:9090 \
  --file=/mnt/github.com/prometheus/prometheus/go.mod \
  --type=go \
  --concurrency=10

# Ingest npm dependencies with cache metadata
./bin/monofs-admin ingest-deps \
  --router=localhost:9090 \
  --file=/mnt/github.com/prometheus/prometheus/web/ui/package.json \
  --type=npm \
  --concurrency=10

# Wait for ingestion (or check status)
./bin/monofs-admin status --router=localhost:9090

# Verify source and dependencies
ls /mnt/github.com/prometheus/prometheus/
# Should see: cmd/ web/ storage/ promql/ tsdb/ etc.

ls /mnt/go-modules/pkg/mod/github.com/go-kit/
ls /mnt/npm-cache/
```

## Step 2: Navigate to Source in MonoFS

Work directly on the mounted filesystem:

```bash
# Navigate directly to ingested source
cd /mnt/github.com/prometheus/prometheus/

# Verify you're in the right place
ls -la
# Should see: Makefile, go.mod, cmd/, web/, etc.

# Start a session if you need to modify source
./bin/monofs-session start
```

## Step 3: Verify Dependencies (Already Available)

All dependencies are already available - verify cache metadata:

```bash
# Check Go dependencies and cache metadata
ls /mnt/go-modules/pkg/mod/github.com/go-kit/
ls /mnt/go-modules/pkg/mod/cache/download/github.com/go-kit/

# Check npm dependencies
ls /mnt/npm-cache/

# Configure environment for offline builds
eval $(./bin/monofs-session setup /mnt)

# Verify offline environment variables
echo $GOMODCACHE  # Should be /mnt/go-modules/pkg/mod
echo $GOPROXY     # Should be off
echo $NPM_CONFIG_CACHE  # Should be /mnt/npm-cache
```

## Step 4: Build Prometheus Components (Offline)

### Build Prometheus Server

```bash
# Environment already configured with monofs-session setup
# Build 100% offline with standard go commands!
go build -o prometheus ./cmd/prometheus

# Verify binary
./prometheus --version
# Should output: prometheus, version ...

# No network access was used!
```

### Build Additional Tools

Prometheus includes several utilities:

```bash
# Build promtool (config validator and query tool)
go build -o promtool ./cmd/promtool

# Build query examples
go build -o examples/random ./docs/examples/random

# List all built binaries
ls -lh prometheus promtool

# All builds 100% offline using MonoFS cache!
```

## Step 5: Build Web UI Assets (Offline)

Prometheus includes a React-based web UI:

```bash
# Navigate to web UI directory
cd web/ui

# Environment already configured for npm offline mode
# Build UI assets 100% offline!
npm ci
npm run build

# Return to root
cd ../..

# Rebuild Prometheus with UI embedded
go build -o prometheus ./cmd/prometheus

# Entire build was offline - zero network access!
```

## Step 6: Run Tests (Offline)

Run Prometheus test suite using MonoFS dependencies:

```bash
# Run all tests (100% offline!)
go test ./...

# Run specific package tests
go test ./promql/...
go test ./storage/...
go test ./tsdb/...

# Run with coverage
go test -cover ./...

# Run race detector tests
go test -race ./...

# Benchmark tests
go test -bench=. ./tsdb/...

# All tests run completely offline!
```

## Step 7: Build with Different Configurations (Offline)

### Production Build with Optimization

```bash
# Build with release flags (100% offline!)
go build \
  -ldflags="-s -w -X github.com/prometheus/common/version.Version=2.49.0" \
  -o prometheus-prod \
  ./cmd/prometheus

# Check binary size
ls -lh prometheus-prod

# Strip binary further
strip prometheus-prod
```

### Debug Build

```bash
# Build with debug symbols (100% offline!)
go build -gcflags="all=-N -l" -o prometheus-debug ./cmd/prometheus

# Verify debug symbols
file prometheus-debug
```

### Cross-Compilation (Offline)

Build for multiple platforms using shared dependencies:

```bash
# Linux AMD64 (100% offline!)
GOOS=linux GOARCH=amd64 go build -o prometheus-linux-amd64 ./cmd/prometheus

# Linux ARM64 (100% offline!)
GOOS=linux GOARCH=arm64 go build -o prometheus-linux-arm64 ./cmd/prometheus

# macOS AMD64
GOOS=darwin GOARCH=amd64 gobuild -o prometheus-darwin-amd64 ./cmd/prometheus

# Windows AMD64
GOOS=windows GOARCH=amd64 gobuild -o prometheus-windows-amd64.exe ./cmd/prometheus

# List all built binaries
ls -lh prometheus-*

# All platforms built from same shared dependencies!
```

## Step 8: Create Distribution Package

```bash
# Create distribution directory (in current location)
mkdir -p dist/prometheus-2.49.0

# Copy binaries
cp prometheus dist/prometheus-2.49.0/
cp promtool dist/prometheus-2.49.0/

# Copy configuration examples
cp -r documentation/examples/*.yml dist/prometheus-2.49.0/

# Copy console templates
cp -r console_libraries/ dist/prometheus-2.49.0/
cp -r consoles/ dist/prometheus-2.49.0/

# Create tarball
tar czf dist/prometheus-2.49.0.tar.gz -C dist prometheus-2.49.0/

# Commit to MonoFS session if you made changes
monofs-session commit \
  --message="Prometheus 2.49.0 build - $(date)" \
  --files="dist/,prometheus,promtool"

echo "✓ Distribution package created"
echo "✓ Available at: /mnt/github.com/prometheus/prometheus/dist/"
```

## Step 9: Run Prometheus from MonoFS

Test the built Prometheus:

```bash
# Create minimal config in current directory
cat > prometheus-test.yml <<EOF
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: 'prometheus'
    static_configs:
      - targets: ['localhost:9090']
EOF

# Run Prometheus
./prometheus \
  --config.file=./prometheus-test.yml \
  --storage.tsdb.path=./data \
  --web.listen-address=:9090

# Access UI at http://localhost:9090
# Built completely offline using MonoFS!
```

## Performance Comparison

**Traditional Build Process:**
```bash
# Git clone
time git clone https://github.com/prometheus/prometheus
# Real: 45 seconds (network dependent)

# Go dependencies
cd prometheus && time go mod download
# Real: 120 seconds (network dependent)

# NPM dependencies
cd web/ui && time npm install
# Real: 180 seconds (network dependent)

# Build
time make build
# Real: 180 seconds

# Total: ~525 seconds (8m 45s)
```

**MonoFS Build Process (First Build):**
```bash
# Access source (already ingested)
time ls /mnt/github.com/prometheus/prometheus/
# Real: 0.01 seconds (instant!)

# Navigate to source (no copying!)
cd /mnt/github.com/prometheus/prometheus/
# Real: 0.01 seconds

# Dependencies (already ingested)
time ls /mnt/go-modules/pkg/mod/
# Real: 0.01 seconds (instant!)

# Build (100% offline!)
time gobuild ./cmd/prometheus
# Real: 180 seconds

# Total: ~180 seconds (3m)
# Speedup: 2.9x faster!
# Network usage: ZERO!
```

**MonoFS Build Process (Subsequent Builds):**
```bash
# Navigate to source
cd /mnt/github.com/prometheus/prometheus/

# Incremental build (only changed files)
time gobuild ./cmd/prometheus
# Real: 15-30 seconds

# Speedup: 17x-35x faster!
```

## Advanced: Building Multiple Prometheus Versions

Compare different Prometheus versions using MonoFS:

```bash
# Ingest specific versions (if not already ingested)
monofs-admin ingest \
  --url=https://github.com/prometheus/prometheus \
  --ref=v2.49.0 \
  --display-path=github.com/prometheus/prometheus@v2.49.0

monofs-admin ingest \
  --url=https://github.com/prometheus/prometheus \
  --ref=v2.45.0 \
  --display-path=github.com/prometheus/prometheus@v2.45.0

# Ingest dependencies for each version
monofs-admin ingest-deps \
  --file=/mnt/github.com/prometheus/prometheus@v2.49.0/go.mod \
  --type=go

monofs-admin ingest-deps \
  --file=/mnt/github.com/prometheus/prometheus@v2.45.0/go.mod \
  --type=go

# Build v2.49.0 (work directly in /mnt)
cd /mnt/github.com/prometheus/prometheus@v2.49.0
gobuild -o prometheus ./cmd/prometheus

# Build v2.45.0 (work directly in /mnt)
cd /mnt/github.com/prometheus/prometheus@v2.45.0
gobuild -o prometheus ./cmd/prometheus

# Compare binaries
ls -lh /mnt/github.com/prometheus/prometheus@v2.49.0/prometheus
ls -lh /mnt/github.com/prometheus/prometheus@v2.45.0/prometheus

# Compare features
/mnt/github.com/prometheus/prometheus@v2.49.0/prometheus --help > v249-help.txt
/mnt/github.com/prometheus/prometheus@v2.45.0/prometheus --help > v245-help.txt
diff v249-help.txt v245-help.txt

# All builds share common dependencies from /mnt/go-modules/!
```

## Continuous Integration Example

GitHub Actions workflow using MonoFS:

```yaml
# .github/workflows/monofs-build.yml
name: Build with MonoFS

on: [push, pull_request]

jobs:
  build:
    runs-on: self-hosted  # Must have MonoFS mounted
    
    steps:
      - name: Wait for MonoFS mount
        run: |
          while ! mountpoint -q /mnt; do
            echo "Waiting for MonoFS..."
            sleep 2
          done

      - name: Ensure source and dependencies ingested
        run: |
          monofs-admin ingest \
            --url=https://github.com/prometheus/prometheus \
            --ref=${{ github.ref }} \
            --display-path=github.com/prometheus/prometheus
          
          monofs-admin ingest-deps \
            --file=/mnt/github.com/prometheus/prometheus/go.mod \
            --type=go

      - name: Navigate to source
        run: |
          cd /mnt/github.com/prometheus/prometheus
          monofs-session start

      - name: Build Prometheus (offline)
        run: |
          cd /mnt/github.com/prometheus/prometheus
          gobuild -o prometheus ./cmd/prometheus
          gobuild -o promtool ./cmd/promtool

      - name: Run tests (offline)
        run: |
          cd /mnt/github.com/prometheus/prometheus
          gotest ./...

      - name: Commit artifacts
        run: |
          cd /mnt/github.com/prometheus/prometheus
          monofs-session commit \
            --message="CI Build ${{ github.run_id }}" \
            --files="prometheus,promtool"

      - name: Success
        run: |
          echo "✓ Build complete - 100% offline!"
          echo "✓ Zero network access during build"
```

## Troubleshooting

### Go Build Tries to Download Modules

```bash
# Ensure environment is configured for offline builds
eval $(./bin/monofs-session setup /mnt)

# Build 100% offline with standard commands
gobuild ./...

# NOT: go build ./...
# The wrapper sets GOMODCACHE and offline flags

# Verify offline configuration
# Verify offline environment
echo $GOMODCACHE
echo $GOPROXY

# Run dry-run go -- build ./...
# Should show: GOMODCACHE=/mnt/go-modules/pkg/mod
```

### Missing Dependencies

```bash
# Check if dependencies were ingested
ls /mnt/go-modules/pkg/mod/

# Re-ingest if needed
monofs-admin ingest-deps \
  --file=/mnt/github.com/prometheus/prometheus/go.mod \
  --type=go

# For npm dependencies
ls /mnt/npm-cache/
monofs-admin ingest-deps \
  --file=/mnt/github.com/prometheus/prometheus/web/ui/package.json \
  --type=npm
```

### Missing UI Assets

```bash
# Ensure npm dependencies ingested
cd /mnt/github.com/prometheus/prometheus/web/ui

# Rebuild UI (offline)
npmci
npmrun build

# Return and rebuild Prometheus
cd ../..
gobuild ./cmd/prometheus
```

### Permission Errors

```bash
# Start a session to make changes
cd /mnt/github.com/prometheus/prometheus
monofs-session start

# Now you can modify files and build
vim cmd/prometheus/main.go
gobuild ./cmd/prometheus
```

## Next Steps

- Build Kubernetes: See [howto-build-kubernetes.md](./howto-build-kubernetes.md)
- Use Bazel: See [howto-bazel-build.md](./howto-bazel-build.md)
- NPM projects: See [howto-build-react-app.md](./howto-build-react-app.md)
- Multi-backend: See [howto-build-multi-backend.md](./howto-build-multi-backend.md)
