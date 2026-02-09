# Using MonoFS Build - Complete Guide

**monofs-build** is a universal wrapper that configures your build tools (Go, Bazel, npm, Maven, Cargo, pip) to use MonoFS as their module/package cache. This eliminates redundant downloads and enables zero-copy builds across your infrastructure.

## Table of Contents

- [What is monofs-build?](#what-is-monofs-build)
- [Quick Start](#quick-start)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Basic Usage](#basic-usage)
- [Build System Guides](#build-system-guides)
  - [Go](#go-golang)
  - [Bazel](#bazel)
  - [npm (Node.js)](#npm-nodejs)
  - [Maven (Java)](#maven-java)
  - [Cargo (Rust)](#cargo-rust)
  - [pip (Python)](#pip-python)
- [Advanced Usage](#advanced-usage)
- [Troubleshooting](#troubleshooting)
- [CI/CD Integration](#cicd-integration)

---

## What is monofs-build?

`monofs-build` is a convenience CLI that:
- **Auto-detects** your build system from project files
- **Sets environment variables** to point build tools at MonoFS cache
- **Validates** that MonoFS is mounted and dependencies are available
- **Passes through** all arguments to the underlying build tool

**You don't need to learn new commands** — it's just a wrapper around your existing tools.

---

## Quick Start

```bash
# 1. Mount MonoFS
monofs-client --mount=/mnt/monofs --router=localhost:9090

# 2. Ingest your project's dependencies
cd /path/to/your/project
monofs-admin ingest-deps --file=go.mod --type=go --router=localhost:9090

# 3. Build using MonoFS cache (auto-detects Go from go.mod)
monofs-build build ./...
```

**That's it!** Your build now uses MonoFS instead of downloading dependencies.

---

## Prerequisites

Before using `monofs-build`, ensure:

1. **MonoFS is running**: Router and server nodes are up
2. **MonoFS is mounted**: FUSE client is connected
3. **Dependencies are ingested**: Use `monofs-admin ingest-deps` or manually ingest modules

### Verify Setup

```bash
# Check if MonoFS is mounted
ls /mnt/monofs

# Check if cache directories exist
ls /mnt/monofs/go-modules/pkg/mod      # Go
ls /mnt/monofs/npm-cache                # npm
ls /mnt/monofs/cargo-home               # Cargo
ls /mnt/monofs/bazel-repos              # Bazel
ls /mnt/monofs/maven-repo               # Maven
ls /mnt/monofs/pip-cache                # pip
```

---

## Installation

### From Source

```bash
cd /path/to/monofs
make build
# Binary created at: bin/monofs-build
```

### Install System-Wide

```bash
sudo cp bin/monofs-build /usr/local/bin/
monofs-build --version
```

---

## Basic Usage

### Auto-Detection Mode

Run `monofs-build` from your project directory. It detects the build system automatically:

```bash
cd /path/to/go-project
monofs-build build ./...
# Auto-detected build system: go
```

Detection is based on files:
- **Go**: `go.mod`, `go.sum`
- **Bazel**: `MODULE.bazel`, `WORKSPACE`, `BUILD.bazel`
- **npm**: `package.json`, `package-lock.json`
- **Maven**: `pom.xml`
- **Cargo**: `Cargo.toml`, `Cargo.lock`
- **pip**: `requirements.txt`, `pyproject.toml`, `setup.py`

### Explicit Mode

Specify the build system explicitly:

```bash
monofs-build go -- build -v ./...
monofs-build npm -- install
monofs-build cargo -- build --release
```

The `--` separates monofs-build options from build command arguments.

### Options

```bash
--mount=PATH     # MonoFS mount point (default: /mnt/monofs)
--dry-run        # Show what would run without executing
--version        # Show version
--help           # Show help
```

### Dry Run (Debugging)

See what environment variables would be set:

```bash
monofs-build --dry-run go -- build ./...

# Output:
# === Dry Run Mode ===
# Build System: go
# Mount Point:  /mnt/monofs
# Cache Path:   /mnt/monofs/go-modules/pkg/mod
#
# Environment Variables:
#   GOMODCACHE=/mnt/monofs/go-modules/pkg/mod
#   GONOSUMDB=*
#   GONOSUMCHECK=*
#   GOFLAGS=-mod=mod
#
# Command: go build ./...
```

---

## Build System Guides

### Go (Golang)

**Environment Variables Set:**
- `GOMODCACHE=/mnt/monofs/go-modules/pkg/mod`
- `GONOSUMDB=*` — Skip checksum database
- `GONOSUMCHECK=*` — Skip checksum verification
- `GOFLAGS=-mod=mod` — Use module mode

#### Complete Workflow

```bash
# 1. Mount MonoFS
monofs-client --mount=/mnt/monofs --router=localhost:9090

# 2. Ingest dependencies from go.mod
cd /path/to/go-project
monofs-admin ingest-deps \
  --file=go.mod \
  --type=go \
  --router=localhost:9090

# 3. Build
monofs-build build ./...

# 4. Test
monofs-build test ./...

# 5. Run
monofs-build run ./cmd/myapp
```

#### Manual Alternative (without monofs-build)

```bash
GOMODCACHE=/mnt/monofs/go-modules/pkg/mod \
GONOSUMDB='*' \
GONOSUMCHECK='*' \
GOFLAGS='-mod=mod' \
  go build ./...
```

#### Verify Go Modules

```bash
# List ingested Go modules
ls /mnt/monofs/go-modules/pkg/mod

# Example structure:
# /mnt/monofs/go-modules/pkg/mod/
#   github.com/google/uuid@v1.6.0/
#   golang.org/x/sync@v0.6.0/
```

---

### Bazel

**Environment Variables Set:**
- `BAZEL_REPOSITORY_CACHE=/mnt/monofs/bazel-repos`

#### Complete Workflow

```bash
# 1. Ingest Bazel dependencies (if using MODULE.bazel)
monofs-admin ingest-deps \
  --file=MODULE.bazel \
  --type=bazel \
  --router=localhost:9090

# 2. Or manually ingest specific repos
monofs-admin ingest \
  --source=https://github.com/bazelbuild/rules_go \
  --ref=v0.50.0 \
  --ingestion-type=git

# 3. Build
monofs-build bazel -- build //...

# 4. Test
monofs-build bazel -- test //...
```

---

### npm (Node.js)

**Environment Variables Set:**
- `NPM_CONFIG_CACHE=/mnt/monofs/npm-cache`
- `NPM_CONFIG_PREFER_OFFLINE=true`

#### Complete Workflow

```bash
# 1. Ingest dependencies from package.json
cd /path/to/node-project
monofs-admin ingest-deps \
  --file=package.json \
  --type=npm \
  --router=localhost:9090

# 2. Install packages
monofs-build npm -- install

# 3. Run scripts
monofs-build npm -- run build
monofs-build npm -- test
```

---

### Maven (Java)

**Environment Variables Set:**
- `MAVEN_REPO=/mnt/monofs/maven-repo`

#### Complete Workflow

```bash
# 1. Ingest dependencies from pom.xml
cd /path/to/maven-project
monofs-admin ingest-deps \
  --file=pom.xml \
  --type=maven \
  --router=localhost:9090

# 2. Build
monofs-build maven -- clean install

# 3. Test
monofs-build maven -- test

# 4. Run with specific profiles
monofs-build maven -- clean package -P production
```

---

### Cargo (Rust)

**Environment Variables Set:**
- `CARGO_HOME=/mnt/monofs/cargo-home`
- `CARGO_NET_OFFLINE=true` — Use offline mode

#### Complete Workflow

```bash
# 1. Ingest dependencies from Cargo.toml
cd /path/to/rust-project
monofs-admin ingest-deps \
  --file=Cargo.toml \
  --type=cargo \
  --router=localhost:9090

# 2. Build
monofs-build cargo -- build

# 3. Build release
monofs-build cargo -- build --release

# 4. Test
monofs-build cargo -- test

# 5. Run
monofs-build cargo -- run
```

---

### pip (Python)

**Environment Variables Set:**
- `PIP_CACHE_DIR=/mnt/monofs/pip-cache`
- `PIP_NO_INDEX=true` — Don't use PyPI index

#### Complete Workflow

```bash
# 1. Ingest dependencies from requirements.txt
cd /path/to/python-project
monofs-admin ingest-deps \
  --file=requirements.txt \
  --type=pip \
  --router=localhost:9090

# 2. Install packages
monofs-build pip -- install -r requirements.txt

# 3. Install with constraints
monofs-build pip -- install -r requirements.txt -c constraints.txt
```

---

## Advanced Usage

### Custom Mount Point

If MonoFS is mounted at a non-default location:

```bash
monofs-build --mount=/custom/path go -- build ./...
```

### Combining with Build Flags

Pass any flags to the underlying tool:

```bash
# Go: verbose build with race detector
monofs-build go -- build -v -race ./...

# Cargo: release build with features
monofs-build cargo -- build --release --features=extra

# npm: production install
monofs-build npm -- install --production

# Maven: skip tests
monofs-build maven -- clean install -DskipTests
```

### Multiple Build Systems in One Project

Some projects use multiple build systems:

```bash
# Build Go backend
monofs-build go -- build ./backend/...

# Build npm frontend
cd frontend
monofs-build npm -- run build
```

### Using in Scripts

```bash
#!/bin/bash
set -e

# Build script using MonoFS
MONOFS_MOUNT=/mnt/monofs

# Check if MonoFS is available
if [ ! -d "$MONOFS_MOUNT" ]; then
    echo "Error: MonoFS not mounted"
    exit 1
fi

# Build with MonoFS
monofs-build --mount="$MONOFS_MOUNT" go -- build -o myapp ./cmd/myapp

# Run tests
monofs-build --mount="$MONOFS_MOUNT" go -- test -v ./...
```

---

## Troubleshooting

### Error: MonoFS not mounted

```
Error: MonoFS not mounted at /mnt/monofs
```

**Solution:** Start the FUSE client:

```bash
monofs-client --mount=/mnt/monofs --router=localhost:9090
```

### Error: Cache directory not found

```
Error: Cache directory not found at /mnt/monofs/go-modules/pkg/mod
```

**Solution:** Ingest dependencies first:

```bash
monofs-admin ingest-deps --file=go.mod --type=go --router=localhost:9090
```

### Error: Could not detect build system

```
Error: Could not detect build system in current directory.
```

**Solution:** Either:
1. Run from directory with build files (go.mod, package.json, etc.)
2. Specify build type explicitly: `monofs-build go -- build ./...`

### Build Tool Not Found

```
Build failed: exec: "cargo": executable file not found in $PATH
```

**Solution:** Install the build tool:

```bash
# Go
sudo apt install golang-go

# npm
sudo apt install nodejs npm

# Cargo
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh

# Maven
sudo apt install maven

# Bazel
# See https://bazel.build/install
```

### Dependencies Not Available

If builds fail with missing dependencies:

```bash
# Verify dependencies are ingested
ls /mnt/monofs/go-modules/pkg/mod/github.com/your/dependency@v1.0.0

# If missing, ingest manually
monofs-admin ingest \
  --source=github.com/your/dependency@v1.0.0 \
  --ingestion-type=go \
  --fetch-type=gomod
```

### Permission Issues

If you get permission errors:

```bash
# Check MonoFS mount permissions
ls -la /mnt/monofs

# Ensure you have read access to cache directories
chmod -R a+r /mnt/monofs/go-modules/pkg/mod
```

### Debugging with Dry Run

Always use `--dry-run` first to see what would happen:

```bash
monofs-build --dry-run go -- build ./...
```

This shows:
- Detected build system
- Mount point
- Cache path
- Environment variables
- Command that would be executed

---

## CI/CD Integration

### GitHub Actions

```yaml
name: Build with MonoFS

on: [push, pull_request]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      
      - name: Mount MonoFS
        run: |
          # Start MonoFS client in background
          monofs-client \
            --mount=/mnt/monofs \
            --router=${{ secrets.MONOFS_ROUTER }} &
          sleep 5
      
      - name: Ingest Dependencies
        run: |
          monofs-admin ingest-deps \
            --file=go.mod \
            --type=go \
            --router=${{ secrets.MONOFS_ROUTER }}
      
      - name: Build
        run: monofs-build --mount=/mnt/monofs build -v ./...
      
      - name: Test
        run: monofs-build --mount=/mnt/monofs test -v ./...
      
      - name: Cleanup
        if: always()
        run: fusermount -u /mnt/monofs || true
```

### GitLab CI

```yaml
build:
  stage: build
  script:
    # Mount MonoFS
    - monofs-client --mount=/mnt/monofs --router=$MONOFS_ROUTER &
    - sleep 5
    
    # Ingest dependencies
    - monofs-admin ingest-deps --file=go.mod --type=go --router=$MONOFS_ROUTER
    
    # Build
    - monofs-build --mount=/mnt/monofs build ./...
  after_script:
    - fusermount -u /mnt/monofs || true
```

### Jenkins

```groovy
pipeline {
    agent any
    
    environment {
        MONOFS_MOUNT = '/mnt/monofs'
        MONOFS_ROUTER = credentials('monofs-router-url')
    }
    
    stages {
        stage('Mount MonoFS') {
            steps {
                sh '''
                    monofs-client --mount=${MONOFS_MOUNT} --router=${MONOFS_ROUTER} &
                    sleep 5
                '''
            }
        }
        
        stage('Ingest Dependencies') {
            steps {
                sh '''
                    monofs-admin ingest-deps \
                        --file=go.mod \
                        --type=go \
                        --router=${MONOFS_ROUTER}
                '''
            }
        }
        
        stage('Build') {
            steps {
                sh 'monofs-build --mount=${MONOFS_MOUNT} build -v ./...'
            }
        }
        
        stage('Test') {
            steps {
                sh 'monofs-build --mount=${MONOFS_MOUNT} test -v ./...'
            }
        }
    }
    
    post {
        always {
            sh 'fusermount -u ${MONOFS_MOUNT} || true'
        }
    }
}
```

---

## Best Practices

### 1. Pre-Ingest Dependencies

Ingest all dependencies before starting parallel builds:

```bash
# Ingest once
monofs-admin ingest-deps --file=go.mod --type=go

# Many builds can now use the same cache
for dir in service1 service2 service3; do
    (cd $dir && monofs-build build ./...) &
done
wait
```

### 2. Use Dedicated MonoFS for Builds

For production, use a dedicated MonoFS cluster for build caches:

```bash
# Development cluster
monofs-build --mount=/mnt/monofs-dev go -- build ./...

# CI/CD cluster  
monofs-build --mount=/mnt/monofs-ci go -- build ./...
```

### 3. Verify Before Building

```bash
# Always check MonoFS is mounted
if [ ! -d /mnt/monofs ]; then
    echo "Mount MonoFS first"
    exit 1
fi

# Then build
monofs-build build ./...
```

### 4. Keep Dependencies Updated

Periodically re-ingest to get latest versions:

```bash
# Update Go modules
monofs-admin ingest-deps --file=go.mod --type=go

# Update npm packages
monofs-admin ingest-deps --file=package.json --type=npm
```

---

## Performance Benefits

### Without MonoFS (Traditional)

```bash
# 10 parallel builds
for i in {1..10}; do
    git clone repo && cd repo && go build  # Each downloads all deps
done
# Total: 10x network bandwidth, 10x disk space
```

### With MonoFS

```bash
# One-time setup
monofs-admin ingest-deps --file=go.mod --type=go

# 10 parallel builds
for i in {1..10}; do
    monofs-build build ./...  # All share same cache, zero downloads
done
# Total: 1x network bandwidth, 1x disk space
```

**Result:** 10x faster builds, 90% less network usage, 90% less disk space.

---

## Getting Help

```bash
# Show help
monofs-build --help

# Show version
monofs-build --version

# Debug with dry run
monofs-build --dry-run <command>
```

For more information:
- [BUILD_INTEGRATION.md](BUILD_INTEGRATION.md) - Architecture and implementation
- [ARCHITECTURE.md](ARCHITECTURE.md) - System design
- [QUICKSTART.md](QUICKSTART.md) - Getting started with MonoFS
