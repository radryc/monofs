# How to Build MonoFS Using MonoFS

This guide demonstrates building MonoFS from its own source using the MonoFS filesystem - a practical example of self-hosting.

## Prerequisites

- MonoFS cluster running (see [QUICKSTART.md](../docs/QUICKSTART.md))
- Access to a MonoFS client with write support
- Go 1.24+ installed on the client

## Step 1: Ingest MonoFS Repository and Dependencies

First, ingest the MonoFS repository and ALL its dependencies:

```bash
# Ingest MonoFS source
monofs-admin ingest \
  --source=https://github.com/radryc/monofs \
  --ref=main

# Ingest ALL Go module dependencies automatically
monofs-admin ingest-deps \
  --file=/mnt/github.com/radryc/monofs/go.mod \
  --type=go

# Wait for ingestion to complete
monofs-admin status --filter=monofs

# Verify dependencies are available
ls /mnt/go-modules/pkg/mod/github.com/nutsdb/
ls /mnt/go-modules/pkg/mod/google.golang.org/grpc@*/
```

## Step 2: Navigate to Source in MonoFS

Work directly on the mounted filesystem - no copying needed:

```bash
# SSH into a MonoFS client
ssh monofs@localhost -p 2222

# Navigate directly to the ingested source
cd /mnt/github.com/radryc/monofs/

# Verify you're in the right place
ls -la
# Should see: cmd/ internal/ api/ go.mod go.sum Makefile etc.

# Start a writable session if you need to make changes
# Note: monofs-client must have been started with --writable flag.
# In Docker, ensure MONOFS_OVERLAY_DIR is set to match --overlay path:
#   export MONOFS_OVERLAY_DIR=/var/lib/monofs/overlay
monofs-session start
```

## Step 3: Build with MonoFS (Offline Mode)

Build using ONLY dependencies from MonoFS - no external network access.

**Option A (recommended): One-time setup, then use native commands:**

```bash
# Auto-detects go.mod in current directory and sets up the environment
eval $(monofs-session setup .)

# Now use standard go commands — no wrapper needed!
go build -o bin/monofs-server ./cmd/monofs-server
go build -o bin/monofs-router ./cmd/monofs-router
go build -o bin/monofs-client ./cmd/monofs-client
go build -o bin/monofs-admin ./cmd/monofs-admin
go build -o bin/monofs-search ./cmd/monofs-search
go build -o bin/monofs-fetcher ./cmd/monofs-fetcher
go build -o bin/monofs-session ./cmd/monofs-session

# Or just use make
make build
```

**Option B: Use the monofs-build wrapper for each command:**

```bash
monofs-build go -- build -o bin/monofs-server ./cmd/monofs-server
monofs-build go -- build -o bin/monofs-router ./cmd/monofs-router
# ... etc
```

```bash
# Verify binaries
ls -lh bin/
./bin/monofs-server --version

# Check that NO network access was used (offline build!)
# All dependencies came from /mnt/go-modules/
```

## Step 4: Run Tests (Optional)

Run the test suite using MonoFS dependencies (offline):

```bash
# If you haven't run setup yet:
# eval $(monofs-session setup go)

# Run all tests (native go command)
go test ./...

# Run specific package tests
go test ./internal/client/...
go test ./internal/storage/...

# Run with verbose output
go test -v ./internal/server/...

# Run integration tests
go test -v ./test/...

# All tests run completely offline!
```

## Step 5: Make Changes (Optional)

If you need to modify the source:

```bash
# Make your changes in place
vim internal/server/server.go

# Rebuild only changed components
go build -o bin/monofs-server ./cmd/monofs-server

# Test your changes
go test ./internal/server/...
```

## Step 6: Commit Build Artifacts

If you made changes, commit them to your session:

```bash
# Commit modified source files
monofs-session commit \
  --message="Built MonoFS binaries with my changes" \
  --files="bin/*,internal/server/server.go"

# Verify commit
monofs-session status
```

## Step 7: Access Built Binaries from Other Clients

The built binaries are immediately available across the cluster:

```bash
# From another client
ssh monofs@client2 -p 2223

# Access the binaries directly (instant!)
ls /mnt/github.com/radryc/monofs/bin/

# Run the built binary
/mnt/github.com/radryc/monofs/bin/monofs-server --help

# Copy to local system if needed
cp /mnt/github.com/radryc/monofs/bin/* ~/local-bin/
```

## Advanced: Build with Different Go Versions

Build MonoFS with multiple Go versions:

```bash
# Navigate to source
cd /mnt/github.com/radryc/monofs/

# Build with Go 1.24 (using monofs-build)
GO=go1.24 monofs-build go -- build -o bin/monofs-server-go124 ./cmd/monofs-server

# Build with Go 1.23
GO=go1.23 monofs-build go -- build -o bin/monofs-server-go123 ./cmd/monofs-server

# Compare binary sizes
ls -lh bin/monofs-server-*

# All builds use same shared dependencies from /mnt/go-modules/!
```

## Performance Notes

**Build Time Comparison:**

- **Traditional**: ~30s (git clone) + ~60s (go mod download) + ~180s (build) = **270 seconds**
- **MonoFS (first client)**: 0s (instant access) + 0s (deps pre-ingested) + ~180s (build) = **180 seconds**
- **MonoFS (subsequent builds)**: 0s + 0s + ~30s (incremental) = **30 seconds**
- **Speedup**: Up to **9x faster** for incremental builds!

**Network Usage:**

- **Traditional**: Downloads all dependencies on every `go mod download`
- **MonoFS**: **Zero network access** during build - completely offline!
- **Dependency ingestion**: Done once, shared across all clients

**Disk Usage:**

- **Traditional**: Full source + dependencies per developer (~500MB each)
- **MonoFS**: Shared source + shared deps (~500MB total for all developers)
- **Savings**: ~90% disk savings with 10+ developers

## Troubleshooting

### Missing Dependencies

If Go modules are not found:

```bash
# Check if dependencies were ingested
ls /mnt/go-modules/pkg/mod/

# Re-ingest dependencies if needed
monofs-admin ingest-deps \
  --file=/mnt/github.com/radryc/monofs/go.mod \
  --type=go

# Verify GOMODCACHE is set correctly
monofs-build --dry-run go -- build ./...
# Should show: GOMODCACHE=/mnt/go-modules/pkg/mod
```

### Build Tries to Download from Network

If build attempts network access:

```bash
# Setup offline environment first
eval $(monofs-session setup go)

# Then use native go commands — they'll use the MonoFS cache
go build ./...

# Verify what environment variables are set
monofs-build --dry-run go -- build ./...
# Should show: GOMODCACHE=/mnt/go-modules/pkg/mod
```

### Build Permission Errors

If you need to modify source:

```bash
# Start a writable session (monofs-client must be running with --writable)
# If socket not found, set MONOFS_OVERLAY_DIR to match monofs-client's --overlay path:
export MONOFS_OVERLAY_DIR=/var/lib/monofs/overlay
monofs-session start

# Make changes
vim internal/server/server.go

# Build will work on modified files
monofs-build go -- build ./...
```

### Out of Disk Space

Check overlay disk usage:

```bash
# Check overlay size (only stores your changes)
du -sh /var/lib/monofs/overlay/

# Clean up old sessions
monofs-session cleanup --older-than=7d

# Remove build artifacts if needed
rm -rf /mnt/github.com/radryc/monofs/bin/*
```

## Continuous Integration Example

Automate MonoFS builds in CI:

```bash
#!/bin/bash
# ci-build.sh - Build MonoFS in CI environment

set -e

# Wait for MonoFS mount
while ! mountpoint -q /mnt; do
  echo "Waiting for MonoFS..."
  sleep 2
done

# Ensure dependencies are ingested (idempotent)
monofs-admin ingest \
  --source=https://github.com/radryc/monofs \
  --ref=$GIT_BRANCH

monofs-admin ingest-deps \
  --file=/mnt/github.com/radryc/monofs/go.mod \
  --type=go

# Navigate to source (no copying!)
cd /mnt/github.com/radryc/monofs/

# Start session for any changes
monofs-session start

# Setup Go environment (one-time per shell)
eval $(monofs-session setup go)

# Build (completely offline, native go commands!)
go build -o bin/monofs-server ./cmd/monofs-server
go build -o bin/monofs-router ./cmd/monofs-router
go build -o bin/monofs-client ./cmd/monofs-client

# Run tests (offline!)
go test ./...

# Commit artifacts if needed
monofs-session commit \
  --message="CI Build $CI_BUILD_ID" \
  --files="bin/*"

echo "✓ Build complete: /mnt/github.com/radryc/monofs/bin/"
echo "✓ Zero network access during build - fully offline!"
```

## Next Steps

- Build other Go projects: See [howto-build-prometheus.md](./howto-build-prometheus.md)
- Use Bazel builds: See [howto-bazel-build.md](./howto-bazel-build.md)
- NPM projects: See [howto-build-react-app.md](./howto-build-react-app.md)
- Multi-backend projects: See [howto-build-multi-backend.md](./howto-build-multi-backend.md)
