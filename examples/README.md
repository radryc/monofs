# MonoFS Build Examples

This directory contains comprehensive guides for building real-world projects using MonoFS.

## Quick Start

Choose a guide based on your project type:

### Single Backend Projects

- **[Build MonoFS using MonoFS](howto-build-monofs.md)** - Self-hosting example ✅
  - Language: Go
  - Build System: **monofs-build** + Make + Go modules
  - Complexity: ⭐⭐
  - Build Time: ~3 minutes (100% offline)

- **[Build Prometheus](howto-build-prometheus.md)** - Go monitoring system ✅
  - Language: Go + Node.js
  - Build System: **monofs-build** + Make + Go modules + npm
  - Complexity: ⭐⭐⭐
  - Build Time: ~3 minutes (100% offline)

- **[Build Bazel Projects](howto-bazel-build.md)** - TensorFlow, Envoy, etc. ✅
  - Languages: Various (C++, Java, Python, Go)
  - Build System: **monofs-build** + Bazel
  - Complexity: ⭐⭐⭐⭐
  - Build Time: 30-60 minutes (offline after deps ingested)

- **[Build React/NPM Projects](howto-build-react-app.md)** - Modern JavaScript apps ✅
  - Languages: JavaScript/TypeScript
  - Build System: **monofs-build** + npm/yarn + Webpack/Vite
  - Complexity: ⭐⭐
  - Build Time: ~2 minutes (100% offline)

### Multi-Backend Projects

- **[Build Multi-Backend Projects](howto-build-multi-backend.md)** - Kubernetes, GitLab, VS Code ✅
  - Languages: Go + Node.js + C++ + Ruby
  - Build Systems: **monofs-build** + Make + Bazel + npm + Bundler
  - Complexity: ⭐⭐⭐⭐⭐
  - Build Time: 10-30 minutes (100% offline)

## Core Concept: Work Directly in /mnt

**The MonoFS Way:** Work directly on ingested source, no copying needed!

```bash
# ❌ WRONG - Old pattern (don't do this)
mkdir /mnt/build/myproject
cp -r /mnt/github.com/org/project/* /mnt/build/myproject/
cd /mnt/build/myproject
go build  # Downloads from network!

# ✅ CORRECT - MonoFS pattern
# 1. Ingest source and ALL dependencies
monofs-admin ingest --url=<repo-url> --ref=main --display-path=<path>
monofs-admin ingest-deps --file=/mnt/<path>/go.mod --type=go

# 2. Work directly in /mnt (no copying!)
cd /mnt/github.com/org/project/
monofs-session start  # Only if you need to modify

# 3. Build 100% offline
monofs-build go -- build ./...  # Zero network access!
```

## Key Principles

1. **✅ Pre-ingest dependencies**: Use `monofs-admin ingest-deps`
2. **✅ Work in /mnt directly**: No copying source code
3. **✅ Use monofs-build**: Enforces offline mode
4. **✅ 100% offline builds**: Zero network during build
5. **✅ Shared everything**: Source + deps shared cluster-wide

## Performance Benefits

### Time Savings

| Project | Traditional Build | MonoFS (Offline) | Speedup |
|---------|------------------|------------------|---------|
| MonoFS | 4m 30s (clone+download+build) | 3m (instant+build) | 1.5x |
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
✅ **Work in place** - No copying, modify /mnt directly
✅ **Build artifact sharing** - Commit once, available everywhere

## Project Type Matrix

| Guide | Go | Node.js | Python | C++ | Java | Ruby | Bazel | Docker |
|-------|----|----|--------|-----|------|------|-------|--------|
| [MonoFS](howto-build-monofs.md) | ✓ | | | | | | | ✓ |
| [Prometheus](howto-build-prometheus.md) | ✓ | ✓ | | | | | | ✓ |
| [Bazel](howto-bazel-build.md) | ✓ | | ✓ | ✓ | ✓ | | ✓ | |
| [React/NPM](howto-build-react-app.md) | | ✓ | | | | | | |
| [Multi-Backend](howto-build-multi-backend.md) | ✓ | ✓ | ✓ | ✓ | | ✓ | ✓ | ✓ |

## Common Workflows

### Daily Development

```bash
# Morning: Start work
ssh monofs@client -p 2222
cd /mnt/build/my-feature-$(date +%Y%m%d)
monofs-session start

# Copy latest source
cp -r /mnt/github.com/myorg/myproject/* ./

# Make changes
vim src/main.go

# Build and test
make build
make test

# Commit work
monofs-session commit --message="Feature X implementation"

# Afternoon: Team member pulls your changes
# On another client
cd /mnt/build/review-feature
cp -r /mnt/build/my-feature-20260210/* ./
# Instant access to your changes + compiled artifacts!
```

### CI/CD Integration

```bash
# CI server pulls latest from MonoFS
cd /mnt/build/ci-$BUILD_ID
cp -r /mnt/github.com/myorg/myproject/* ./

# Cached dependencies = fast builds
npm config set cache /mnt/cache/npm
npm install  # Instant if cached!

# Build
npm run build

# Test
npm test

# Commit artifacts for deployment
monofs-session commit \
  --message="CI Build $BUILD_ID" \
  --files="dist/"
```

### Cross-Platform Builds

```bash
# Build for multiple platforms in parallel
mkdir -p /mnt/build/linux-amd64 /mnt/build/darwin-amd64

# Terminal 1: Linux build
cd /mnt/build/linux-amd64
GOOS=linux GOARCH=amd64 make build

# Terminal 2: macOS build
cd /mnt/build/darwin-amd64
GOOS=darwin GOARCH=amd64 make build

# Both use same source + dependencies from MonoFS!
```

## Troubleshooting

### Common Issues

**Dependencies not found:**
```bash
# Check fetcher service status
monofs-admin status

# Verify cache
ls /mnt/cache/npm/
ls /mnt/cache/bazel/

# Clear and rebuild cache
go clean -modcache
npm cache clean --force
```

**Permission errors:**
```bash
# Ensure you're in writable overlay
monofs-session start

# Check mount
mountpoint /mnt

# Verify permissions
ls -ld /mnt/build/
```

**Out of disk space:**
```bash
# Check overlay usage
du -sh /var/lib/monofs/overlay/

# Clean old sessions
monofs-session cleanup --older-than=7d

# Remove build artifacts
rm -rf /mnt/build/old-*
```

## Advanced Topics

### Custom Build Configurations

Each guide includes:
- Development builds (fast, debug symbols)
- Production builds (optimized, stripped)
- Cross-compilation examples
- Container image building
- CI/CD integration

### Performance Tuning

- Configure shared caches for all tools
- Use parallel builds with `-j` flags
- Leverage MonoFS for build artifact sharing
- Monitor cache hit rates
- Implement cache cleanup policies

### Team Workflows

- **Feature branches**: Create isolated build directories
- **Code review**: Share build artifacts via MonoFS
- **Release builds**: Commit versioned artifacts
- **A/B testing**: Compare builds side-by-side

## Contributing

To add a new build example:

1. Choose a popular open-source project
2. Document the full build process
3. Include performance comparisons
4. Add troubleshooting section
5. Submit PR with example

## Support

- Documentation: [../docs/](../docs/)
- Issues: GitHub Issues
- Community: Discussions

## License

These examples are provided under the same license as MonoFS.
