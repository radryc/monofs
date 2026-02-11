# Examples Documentation Status

## Summary

The example guides have been reviewed and updated to remove outdated patterns and align with current offline builds features.

---

## Issues Found and Fixed

### ❌ Outdated Patterns Removed

1. **`monofs-build` wrapper** - Deprecated tool, replaced with `monofs-session setup`
2. **`/mnt/build/` copying pattern** - Outdated workflow, replaced with direct work in `/mnt/github.com/`
3. **`--url` flag** - Old ingestion flag, replaced with `--source`
4. **Missing `./bin/` prefix** - Commands should use `./bin/monofs-admin` not bare `monofs-admin`
5. **Go 1.21+** - Updated to Go 1.24+
6. **Missing `--router` flag** - All commands need router address
7. **Missing `--concurrency` flag** - ingest-deps should use concurrency for speed

---

## Files Updated

### ✅ howto-build-monofs.md

**Changes**:
- Updated all commands to use `./bin/` prefix
- Added `--router` and `--concurrency` flags
- Removed `monofs-build` wrapper references
- Replaced with `monofs-session setup` pattern
- Updated to Go 1.24+
- Simplified writable session comments

**Before**:
```bash
monofs-admin ingest --source=...
monofs-build go -- build ./cmd/monofs-server
```

**After**:
```bash
./bin/monofs-admin ingest --router=localhost:9090 --source=...
eval $(./bin/monofs-session setup /mnt)
go build ./cmd/monofs-server  # 100% offline!
```

### ✅ howto-build-prometheus.md

**Changes**:
- Updated Go version from 1.21+ to 1.24+
- Changed `--url` to `--source`
- Added `./bin/` prefix to all commands
- Added `--router` and `--concurrency` flags
- Removed ALL `monofs-build` wrapper references (40+ instances)
- Replaced with `monofs-session setup` pattern
- Updated verification section to show cache metadata

**Before**:
```bash
monofs-admin ingest --url=... --display-path=...
monofs-build go -- build ./cmd/prometheus
monofs-build npm -- ci
```

**After**:
```bash
./bin/monofs-admin ingest --router=localhost:9090 --source=...
eval $(./bin/monofs-session setup /mnt)
go build ./cmd/prometheus  # 100% offline!
npm ci  # 100% offline!
```

### ✅ howto-build-react-app.md (Partially Fixed)

**Changes Made**:
- Updated Step 1-4 to remove `/mnt/build/` pattern
- Changed to work directly in `/mnt/github.com/`
- Added `./bin/` prefix
- Added `--router` and `--concurrency` flags
- Removed `monofs-build` wrapper in main sections

**Remaining Issues**:
- Steps 5-8 (Next.js, Vue, Nx Workspace) still use `/mnt/build/` pattern
- These sections need similar updates

---

## Recommended Pattern

All example guides should follow this consistent pattern:

### 1. Ingest Project

```bash
# Ingest source repository
./bin/monofs-admin ingest \
  --router=localhost:9090 \
  --source=https://github.com/org/project \
  --ref=main

# Verify ingestion
ls /mnt/github.com/org/project/
```

### 2. Ingest Dependencies with Cache Metadata

```bash
# For Go projects
./bin/monofs-admin ingest-deps \
  --router=localhost:9090 \
  --file=/mnt/github.com/org/project/go.mod \
  --type=go \
  --concurrency=10

# For npm projects
./bin/monofs-admin ingest-deps \
  --router=localhost:9090 \
  --file=/mnt/github.com/org/project/package.json \
  --type=npm \
  --concurrency=10

# Verify cache metadata
ls /mnt/go-modules/pkg/mod/cache/download/
ls /mnt/npm-cache/
```

### 3. Work Directly in /mnt

```bash
# Navigate to project (NO COPYING!)
cd /mnt/github.com/org/project/

# For modifications, start session
./bin/monofs-session start

# Configure environment for offline builds
eval $(./bin/monofs-session setup /mnt)
```

### 4. Build 100% Offline

```bash
# Use standard build tools (no wrapper!)
go build ./...           # Go
npm install && npm build # npm
cargo build             # Cargo
bazel build //...       # Bazel

# ALL builds are 100% offline - zero network access!
```

---

## What NOT to Do

### ❌ Don't Copy to /mnt/build/

```bash
# WRONG - outdated pattern
mkdir -p /mnt/build/myproject
cp -r /mnt/github.com/org/project/* /mnt/build/myproject/
cd /mnt/build/myproject
```

### ❌ Don't Use monofs-build Wrapper

```bash
# WRONG - deprecated tool
monofs-build go -- build ./...
monofs-build npm -- install
```

### ❌ Don't Forget Router Flag

```bash
# WRONG - missing router address
monofs-admin ingest --source=...

# CORRECT
./bin/monofs-admin ingest --router=localhost:9090 --source=...
```

### ❌ Don't Use Old Ingestion Flags

```bash
# WRONG - old flags
monofs-admin ingest --url=... --display-path=...

# CORRECT
./bin/monofs-admin ingest --source=...
```

---

## Verification Checklist

For each example guide, verify:

- [ ] Uses `./bin/` prefix for all MonoFS commands
- [ ] Uses `--router=localhost:9090` flag
- [ ] Uses `--source` not `--url` for ingestion
- [ ] Uses `ingest-deps` with `--concurrency` flag
- [ ] Uses `monofs-session setup` pattern (NOT `monofs-build`)
- [ ] Works directly in `/mnt/github.com/` (NOT `/mnt/build/`)
- [ ] Shows cache metadata verification
- [ ] Mentions "100% offline" builds
- [ ] Uses standard build tools (go, npm, cargo, bazel)
- [ ] Shows Go 1.24+ requirement

---

## Remaining Work

### howto-build-react-app.md

Needs updates in:
- **Example 2: Next.js** (lines ~109-147)
- **Example 3: Vue.js** (lines ~148-220)
- **Example 4: Nx Workspace** (lines ~221-290)
- **CI/CD Examples** (lines ~380-450)

All need to:
1. Remove `/mnt/build/` pattern
2. Work directly in ingested repos
3. Use `monofs-session setup` instead of `monofs-build`

### Other Guides

- **howto-bazel-build.md** - Needs verification
- **howto-build-multi-backend.md** - Needs verification

---

## Quick Fix Template

For any section using `/mnt/build/`:

**Replace**:
```bash
mkdir -p /mnt/build/myproject
cd /mnt/build/myproject
cp -r /mnt/github.com/org/repo/* ./
monofs-build npm -- install
```

**With**:
```bash
cd /mnt/github.com/org/repo/
./bin/monofs-session start  # If modifications needed
eval $(./bin/monofs-session setup /mnt)
npm install  # 100% offline!
```

---

## Testing

To verify examples are current:

```bash
# Check for outdated patterns
grep -r "monofs-build" examples/*.md
grep -r "/mnt/build/" examples/*.md
grep -r "url=" examples/*.md
grep -r "display-path" examples/*.md

# All should return empty or minimal results
```

---

## Status

- ✅ **howto-build-monofs.md** - COMPLETE
- ✅ **howto-build-prometheus.md** - COMPLETE
- ⚠️ **howto-build-react-app.md** - PARTIAL (first 4 steps updated)
- ❓ **howto-bazel-build.md** - NEEDS REVIEW
- ❓ **howto-build-multi-backend.md** - NEEDS REVIEW

---

**Next Action**: Complete updating remaining examples with consistent pattern.
