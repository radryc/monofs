# MonoFS Build Examples

Comprehensive guides for building real-world projects using MonoFS with 100% offline builds.

## Quick Start

Choose a guide based on your project type:

### Featured Examples

- **[Build MonoFS using MonoFS](howto-build-monofs.md)** - Self-hosting example ✅
  - Language: Go
  - Build System: Go modules + Make
  - Complexity: ⭐⭐
  - Build Time: ~2 minutes (100% offline)

- **[Build Prometheus](howto-build-prometheus.md)** - Go monitoring system ✅
  - Languages: Go + Node.js
  - Build Systems: Go modules + npm + Make
  - Complexity: ⭐⭐⭐
  - Build Time: ~3 minutes (100% offline)

- **[Build React/npm Projects](howto-build-react-app.md)** - Modern JavaScript apps ✅
  - Languages: JavaScript/TypeScript
  - Build Systems: npm + Webpack/Vite
  - Complexity: ⭐⭐
  - Build Time: ~1 minute (100% offline)

- **[Build Bazel Projects](howto-bazel-build.md)** - TensorFlow, Envoy, etc. ✅
  - Languages: Various (C++, Java, Python, Go)
  - Build System: Bazel
  - Complexity: ⭐⭐⭐⭐
  - Build Time: 30-60 minutes (offline after deps ingested)

- **[Build Multi-Backend Projects](howto-build-multi-backend.md)** - Kubernetes, GitLab ✅
  - Languages: Go + Node.js + C++ + Ruby
  - Build Systems: Go + npm + Bazel + Bundler
  - Complexity: ⭐⭐⭐⭐⭐
  - Build Time: 10-30 minutes (100% offline)

---

## Core Concept: Direct Work in /mnt

**The MonoFS Way:** Work directly on ingested source with automatic offline builds!

```bash
# ❌ WRONG - Old pattern (don't do this)
mkdir /tmp/mybuild
cp -r /mnt/github.com/org/project/* /tmp/mybuild/
cd /tmp/mybuild
go build  # Still downloads from network!

# ✅ CORRECT - MonoFS pattern
# 1. Ingest source and ALL dependencies
./bin/monofs-admin ingest \
  --source=https://github.com/org/project \
  --ref=main

./bin/monofs-admin ingest-deps \
  --file=/mnt/github.com/org/project/go.mod \
  --type=go

# 2. Work directly in /mnt (no copying!)
cd /mnt/github.com/org/project/

# 3. Configure offline environment
eval $(./bin/monofs-session setup /mnt)

# 4. Build 100% offline
go build ./...  # Zero network access!
```

---

## Key Principles

1. **✅ Pre-ingest dependencies**: Use `monofs-admin ingest-deps` to cache all dependencies
2. **✅ Work in /mnt directly**: No copying source code
3. **✅ Auto-configured offline mode**: `monofs-session setup` handles all env vars
4. **✅ 100% offline builds**: Zero network during build
5. **✅ Shared everything**: Source + deps shared cluster-wide

---

## Performance Benefits

### Time Savings

| Project | Traditional Build | MonoFS (Offline) | Speedup |
|---------|------------------|------------------|---------|
| MonoFS | 4m 30s (clone+download+build) | 2m (instant+build) | 2.25x |
| Prometheus | 8m 45s (clone+deps+build) | 3m (instant+build) | 2.9x |
| TensorFlow | 78m (clone+deps+build) | 57m (first) / 2m (incremental) | 1.4x / 39x |
| Kubernetes | 17m (clone+deps+build) | 10m (instant+build) | 1.7x |
| React App | 5m (clone+npm install+build) | 30s (instant+build) | 10x |

**All MonoFS builds are 100% offline - zero network access!**

### Disk Savings

- **Traditional**: Each developer needs full source + dependencies (5-10 GB per project)
- **MonoFS**: Shared source + dependencies (1-2 GB total, 80-90% savings)

### Key Advantages

✅ **Instant source access** - No git clone (0.01s vs 30-120s)
✅ **Pre-ingested dependencies** - Zero downloads during build
✅ **100% offline builds** - No network access needed
✅ **Shared caches** - Everyone uses same dependencies
✅ **Work in place** - Read-only or writable with sessions
✅ **Cluster-wide sharing** - Ingest once, build anywhere

---

## Project Type Matrix

| Guide | Go | Node.js | Python | C++ | Java | Ruby | Bazel | Docker |
|-------|----|----|--------|-----|------|------|-------|--------|
| [MonoFS](howto-build-monofs.md) | ✓ | | | | | | | ✓ |
| [Prometheus](howto-build-prometheus.md) | ✓ | ✓ | | | | | | ✓ |
| [Bazel](howto-bazel-build.md) | ✓ | | ✓ | ✓ | ✓ | | ✓ | |
| [React/npm](howto-build-react-app.md) | | ✓ | | | | | | |
| [Multi-Backend](howto-build-multi-backend.md) | ✓ | ✓ | ✓ | ✓ | | ✓ | ✓ | ✓ |

---

## Common Workflows

### Daily Development

```bash
# Work directly in /mnt (read-only)
cd /mnt/github.com/myorg/myproject

# Configure environment
eval $(./bin/monofs-session setup /mnt)

# Build and test (100% offline)
go build ./...
go test ./...

# For modifications, start a writable session
./bin/monofs-session start

# Make changes
vim src/main.go

# Test changes
go build ./...

# Commit work
./bin/monofs-session commit
```

### CI/CD Integration

```bash
# CI server mounts MonoFS (read-only)
./bin/monofs-client --mount=/mnt --router=<router>

# Configure environment
eval $(./bin/monofs-session setup /mnt)

# Build (instant - dependencies already cached)
cd /mnt/github.com/myorg/myproject
go build ./...

# Test
go test ./...

# No network access needed!
```

### Cross-Platform Builds

```bash
# Build for multiple platforms using same source+deps
cd /mnt/github.com/myorg/myproject
eval $(./bin/monofs-session setup /mnt)

# Linux build
GOOS=linux GOARCH=amd64 go build -o bin/app-linux ./...

# macOS build
GOOS=darwin GOARCH=amd64 go build -o bin/app-darwin ./...

# Windows build
GOOS=windows GOARCH=amd64 go build -o bin/app-windows.exe ./...

# All use same cached dependencies!
```

---

## Troubleshooting

### Dependencies not found

```bash
# Check if dependencies were ingested
./bin/monofs-admin repos | grep <package>

# Re-ingest dependencies
./bin/monofs-admin ingest-deps --file=/mnt/.../go.mod --type=go

# Verify cache structure
ls -la /mnt/go-modules/pkg/mod/cache/download/
```

### Permission errors

```bash
# For read-only: no session needed
cd /mnt/github.com/org/project
go build ./...

# For modifications: start session
./bin/monofs-session start
vim src/main.go
```

### Environment not configured

```bash
# Always run setup for offline builds
eval $(./bin/monofs-session setup /mnt)

# Verify environment
echo $GOMODCACHE
echo $GOPROXY
echo $NPM_CONFIG_CACHE
```

---

## Advanced Topics

### Custom Build Configurations

Each guide includes:
- Development builds (fast, debug symbols)
- Production builds (optimized, stripped)
- Cross-compilation examples
- Container image building
- CI/CD integration

### Performance Tuning

- **Parallel builds**: Use `-j` flags with make/bazel
- **Cache hits**: Monitor cache effectiveness
- **Incremental builds**: Leverage unchanged dependencies
- **Shared artifacts**: Commit builds back to MonoFS

### Team Workflows

- **Feature branches**: Use writable sessions for changes
- **Code review**: Share modified source via session commits
- **Release builds**: Tag and commit versioned artifacts
- **A/B testing**: Compare builds side-by-side

---

## Environment Setup

MonoFS automatically configures build environments:

### Go
```bash
eval $(./bin/monofs-session setup /mnt)
# Sets: GOMODCACHE, GOPROXY=off, GOVCS=*:off, GOSUMDB=off
```

### npm
```bash
eval $(./bin/monofs-session setup /mnt)
# Sets: NPM_CONFIG_CACHE, NPM_CONFIG_PREFER_OFFLINE=true
```

### Cargo
```bash
eval $(./bin/monofs-session setup /mnt)
# Sets: CARGO_HOME, CARGO_NET_OFFLINE=true
```

### Bazel
```bash
eval $(./bin/monofs-session setup /mnt)
# Sets: BAZEL_REPOSITORY_CACHE
```

---

## Contributing

To add a new build example:

1. Choose a popular open-source project
2. Document the full ingestion + build process
3. Include performance comparisons
4. Add troubleshooting section
5. Test completely offline builds
6. Submit PR with example

---

## See Also

- [OFFLINE_BUILDS.md](../docs/OFFLINE_BUILDS.md) - Complete offline builds guide (all ecosystems)
- [BUILD_INTEGRATION.md](../docs/BUILD_INTEGRATION.md) - Build tool integration details
- [TESTING_OFFLINE_BUILDS.md](../docs/TESTING_OFFLINE_BUILDS.md) - Testing guide
- [CACHE_METADATA_SUMMARY.md](../docs/CACHE_METADATA_SUMMARY.md) - Implementation details

---

## Support

- **Documentation**: [../docs/](../docs/)
- **Issues**: GitHub Issues
- **Community**: Discussions

## License

These examples are provided under the same license as MonoFS (Apache 2.0).
