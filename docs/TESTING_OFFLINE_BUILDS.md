# Testing Offline Builds

This document describes how to test the offline build functionality in MonoFS.

## Overview

MonoFS now automatically generates cache metadata for Go, npm, Cargo, and Bazel during dependency ingestion, enabling completely offline builds across all supported ecosystems.

## Running Tests

### Unit Tests

Test cache metadata generation for all backends:

```bash
# Test all cache metadata implementations
make test-unit

# Test specific backends
go test -v ./internal/buildlayout/golang/ -run CacheMetadata
go test -v ./internal/buildlayout/npm/ -run CacheMetadata
go test -v ./internal/buildlayout/cargo/ -run CacheMetadata
go test -v ./internal/buildlayout/bazel/ -run CacheMetadata
```

### Integration Tests

End-to-end offline build tests (requires FUSE):

```bash
# Run all offline build integration tests
make test-offline

# Run specific ecosystem tests
go test -v ./test/ -run TestOfflineBuilds_Go
go test -v ./test/ -run TestOfflineBuilds_Npm
go test -v ./test/ -run TestOfflineBuilds_Cargo
```

### Full Test Suite

```bash
# Run all tests (unit + integration + offline)
make test-all
```

## Manual Testing

### Setup

1. **Build binaries:**
   ```bash
   make build
   ```

2. **Start MonoFS cluster:**
   ```bash
   make deploy  # Docker cluster
   # OR
   make deploy-local  # Local dev cluster
   ```

3. **Mount filesystem:**
   ```bash
   make mount MOUNT_POINT=/tmp/monofs
   ```

### Testing Go Offline Builds

1. **Create test project:**
   ```bash
   mkdir -p /tmp/go-test
   cd /tmp/go-test

   cat > go.mod <<'EOF'
   module example.com/test
   go 1.24
   require github.com/google/uuid v1.6.0
   EOF

   cat > main.go <<'EOF'
   package main
   import (
       "fmt"
       "github.com/google/uuid"
   )
   func main() {
       fmt.Println("UUID:", uuid.New())
   }
   EOF

   git init && git add . && git commit -m "init"
   ```

2. **Ingest project and dependencies:**
   ```bash
   # Ingest project
   ./bin/monofs-admin ingest \
     --source=/tmp/go-test \
     --ingestion-type=git

   # Find project in mount
   PROJECT=$(find /tmp/monofs -name "go-test" -type d | head -1)

   # Ingest dependencies with cache metadata
   ./bin/monofs-admin ingest-deps \
     --file=$PROJECT/go.mod \
     --type=go \
     --concurrency=10
   ```

3. **Verify cache metadata:**
   ```bash
   # Check cache structure
   ls -la /tmp/monofs/go-modules/pkg/mod/cache/download/github.com/google/uuid/@v/

   # Should show:
   # - v1.6.0.info
   # - v1.6.0.mod
   # - list

   # Verify .info content
   cat /tmp/monofs/go-modules/pkg/mod/cache/download/github.com/google/uuid/@v/v1.6.0.info
   # Should contain: {"Version":"v1.6.0","Time":"..."}
   ```

4. **Test offline build:**
   ```bash
   cd /tmp/go-test

   # Set offline environment
   export GOMODCACHE=/tmp/monofs/go-modules/pkg/mod
   export GOPROXY=off
   export GOVCS=*:off
   export GOSUMDB=off
   export GOTOOLCHAIN=local

   # Build completely offline
   go build -v ./...

   # Should succeed without network access!
   ```

### Testing npm Offline Builds

1. **Create test project:**
   ```bash
   mkdir -p /tmp/npm-test
   cd /tmp/npm-test

   cat > package.json <<'EOF'
   {
     "name": "test-project",
     "version": "1.0.0",
     "dependencies": {
       "lodash": "^4.17.21"
     }
   }
   EOF

   git init && git add . && git commit -m "init"
   ```

2. **Ingest and verify:**
   ```bash
   # Ingest project
   ./bin/monofs-admin ingest \
     --source=/tmp/npm-test \
     --ingestion-type=git

   # Find project
   PROJECT=$(find /tmp/monofs -name "npm-test" -type d | head -1)

   # Ingest dependencies
   ./bin/monofs-admin ingest-deps \
     --file=$PROJECT/package.json \
     --type=npm

   # Verify cache
   ls -la /tmp/monofs/npm-cache/lodash@4.17.21/
   # Should show:
   # - package.json
   # - .npm-metadata.json
   # - .package-lock.json
   ```

3. **Test offline install:**
   ```bash
   cd /tmp/npm-test

   export NPM_CONFIG_CACHE=/tmp/monofs/npm-cache
   export NPM_CONFIG_PREFER_OFFLINE=true

   npm install --prefer-offline
   ```

### Testing Cargo Offline Builds

1. **Create test project:**
   ```bash
   mkdir -p /tmp/cargo-test/src
   cd /tmp/cargo-test

   cat > Cargo.toml <<'EOF'
   [package]
   name = "test-project"
   version = "0.1.0"
   edition = "2021"

   [dependencies]
   serde = "1.0"
   EOF

   cat > src/main.rs <<'EOF'
   fn main() {
       println!("Hello from Rust!");
   }
   EOF

   git init && git add . && git commit -m "init"
   ```

2. **Ingest and verify:**
   ```bash
   # Ingest project
   ./bin/monofs-admin ingest \
     --source=/tmp/cargo-test \
     --ingestion-type=git

   # Find project
   PROJECT=$(find /tmp/monofs -name "cargo-test" -type d | head -1)

   # Ingest dependencies
   ./bin/monofs-admin ingest-deps \
     --file=$PROJECT/Cargo.toml \
     --type=cargo

   # Verify cache
   ls -la /tmp/monofs/.cargo/registry/cache/serde-*/
   ls -la /tmp/monofs/.cargo/registry/index/
   ```

3. **Test offline build:**
   ```bash
   cd /tmp/cargo-test

   export CARGO_HOME=/tmp/monofs/.cargo
   export CARGO_NET_OFFLINE=true

   cargo build
   ```

## Verification Checklist

After ingesting dependencies, verify the following:

### Go Modules
- [ ] Cache directory exists: `go-modules/pkg/mod/cache/download/`
- [ ] For each module, `@v/` directory contains:
  - [ ] `version.info` - JSON with version and timestamp
  - [ ] `version.mod` - Copy of go.mod
  - [ ] `list` - Version list file
- [ ] Extracted modules in `go-modules/pkg/mod/path@version/`
- [ ] Offline build succeeds with `GOPROXY=off`

### npm Packages
- [ ] Cache directory exists: `npm-cache/`
- [ ] For each package, `package@version/` directory contains:
  - [ ] `package.json` - Package manifest
  - [ ] `.npm-metadata.json` - npm cache metadata
  - [ ] `.package-lock.json` - Lock file
- [ ] Offline install succeeds with `NPM_CONFIG_PREFER_OFFLINE=true`

### Cargo Crates
- [ ] Cache directories exist:
  - [ ] `.cargo/registry/index/` - Crate index
  - [ ] `.cargo/registry/cache/` - Crate metadata
  - [ ] `.cargo/registry/src/` - Extracted sources
- [ ] For each crate:
  - [ ] Index entry in `index/crate-name`
  - [ ] `Cargo.toml` in `cache/crate-name-version/`
  - [ ] `.cargo-checksum.json` in `cache/crate-name-version/`
- [ ] Offline build succeeds with `CARGO_NET_OFFLINE=true`

### Bazel Repositories
- [ ] Cache directory exists: `bazel-cache/repos/`
- [ ] For each repo, `bazel-cache/repos/name/` contains:
  - [ ] `WORKSPACE` - Workspace definition
  - [ ] `MODULE.bazel` - Bzlmod module
  - [ ] `repo.bzl` - Repository marker
  - [ ] `BUILD.monofs` - Build marker
- [ ] Bazel can find repositories offline

## Troubleshooting

### Go: "module lookup disabled by GOPROXY=off"
**Cause:** Cache metadata files missing
**Fix:** Re-run `ingest-deps` with updated MonoFS code

### npm: "ENOENT: no such file or directory"
**Cause:** npm cache structure incorrect
**Fix:** Verify cache path and file names match npm expectations

### Cargo: "failed to load source for dependency"
**Cause:** Index or checksum file missing/invalid
**Fix:** Check `.cargo/registry/index/` and verify JSON format

### Tests Fail with "permission denied"
**Cause:** FUSE mount requires special permissions
**Solution:** Run tests with `sudo -E` or use `make test-e2e-sudo`

### Tests Timeout
**Cause:** Cluster didn't start or mount failed
**Solution:**
- Check if ports 19001-19002, 19090-19092 are available
- Manually unmount: `fusermount -u /tmp/monofs-test-*`
- Clean up: `rm -rf /tmp/monofs-test-*`

## Performance Testing

Test large-scale ingestion:

```bash
# Ingest project with many dependencies
time ./bin/monofs-admin ingest-deps \
  --file=/mnt/large-project/go.mod \
  --type=go \
  --concurrency=20

# Monitor cluster stats
./bin/monofs-admin stats --type=all
```

Expected performance:
- Go module ingestion: ~2-5ms per module
- npm package ingestion: ~3-8ms per package
- Cargo crate ingestion: ~4-10ms per crate
- Cache metadata adds < 1ms overhead

## CI/CD Integration

Example GitHub Actions workflow:

```yaml
name: Test Offline Builds

on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Install dependencies
        run: |
          sudo apt-get update
          sudo apt-get install -y fuse libfuse-dev

      - name: Build MonoFS
        run: make build

      - name: Run offline build tests
        run: sudo -E make test-offline
```

## See Also

- [CACHE_METADATA_SUMMARY.md](./CACHE_METADATA_SUMMARY.md) - Implementation overview
- [OFFLINE_GO_BUILDS.md](./OFFLINE_GO_BUILDS.md) - Go-specific guide
- [ADDING_CACHE_METADATA.md](./ADDING_CACHE_METADATA.md) - Adding new backends
