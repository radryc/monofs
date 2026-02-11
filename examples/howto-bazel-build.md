# How to Build Projects with Bazel Using MonoFS

This guide demonstrates building large Bazel-based projects (like TensorFlow, Envoy, or Bazel itself) using MonoFS for source and dependency management.

## Overview

Bazel is a build system developed by Google for monorepo-style development. MonoFS can dramatically speed up Bazel builds by providing instant access to source code and build dependencies.

## Prerequisites

- MonoFS cluster running
- Bazel 6.0+ installed
- Appropriate language toolchains (JDK, Python, etc.)

## Example 1: Build Bazel Itself

### Step 1: Ingest Bazel Repository

```bash
# Ingest Bazel source
monofs-admin ingest \
  --url=https://github.com/bazelbuild/bazel \
  --ref=master \
  --display-path=github.com/bazelbuild/bazel

# Verify ingestion
ls /mnt/github.com/bazelbuild/bazel/
# Should see: src/ scripts/ third_party/ BUILD WORKSPACE
```

### Step 2: Ingest Dependencies

```bash
# Ingest all Bazel external dependencies
monofs-admin ingest-deps \
  --file=/mnt/github.com/bazelbuild/bazel/WORKSPACE \
  --type=bazel

# Wait for dependency ingestion
monofs-admin status --filter="bazel"

# All external repositories now cached in /mnt/bazel-cache/
```

### Step 3: Start Session and Work In Place

```bash
# Navigate to source in /mnt
cd /mnt/github.com/bazelbuild/bazel/

# Start session for this directory
monofs-session start

# monofs-build will automatically set:
# - BAZEL_REPOSITORY_CACHE=/mnt/bazel-cache/
# - Offline build flags
# - Shared disk cache
```

### Step 4: Build Bazel (100% Offline)

```bash
# Build using monofs-build (no network access)
monofs-build bazel -- build //src:bazel

# Or bootstrap compile
monofs-build bash -- ./compile.sh

# The compiled binary will be at:
ls -lh bazel-bin/src/bazel

# Test the built binary
./bazel-bin/src/bazel version

# 🚀 Build uses only /mnt/bazel-cache/ - zero downloads!
```

### Step 5: Run Tests (Offline)

```bash
# Run Bazel tests through monofs-build
monofs-build bazel -- test //src/test/...

# Run specific test suite
monofs-build bazel -- test //src/test/shell/bazel:bazel_test

# Run with test output
monofs-build bazel -- test --test_output=all //src/test/java/...

# Commit changes if tests pass
monofs-session commit --message="Bazel build and tests pass"
```

## Example 2: Build Envoy Proxy

Envoy is a complex C++ project with many dependencies:

### Ingest Envoy

```bash
# Ingest Envoy and its dependencies
monofs-admin ingest \
  --url=https://github.com/envoyproxy/envoy \
  --ref=main \
  --display-path=github.com/envoyproxy/envoy

# Verify
ls /mnt/github.com/envoyproxy/envoy/
```

### Build Envoy

```bash
# Ingest Envoy's Bazel dependencies
monofs-admin ingest-deps \
  --file=/mnt/github.com/envoyproxy/envoy/WORKSPACE \
  --type=bazel

# Navigate to source
cd /mnt/github.com/envoyproxy/envoy/
monofs-session start

# Build Envoy binary (100% offline)
monofs-build bazel -- build //source/exe:envoy-static

# Build uses only /mnt/bazel-cache/ - no network!
# First build: ~45min, subsequent builds: seconds

# Test the binary
./bazel-bin/source/exe/envoy-static --version

# Commit if needed
monofs-session commit --message="Envoy $(git rev-parse --short HEAD)"
```

### Build Envoy Tools

```bash
# Build all Envoy tools
bazel build //tools/...

# Build specific tools
bazel build //tools/router_check:router_check
bazel build //tools/config_validation:validate_config

# List built tools
find bazel-bin/tools -type f -executable
```

## Example 3: Build TensorFlow

TensorFlow is one of the largest Bazel projects:

### Ingest TensorFlow

```bash
# Warning: TensorFlow is very large (~2GB source)
monofs-admin ingest \
  --url=https://github.com/tensorflow/tensorflow \
  --ref=master \
  --display-path=github.com/tensorflow/tensorflow

# This may take time, but only needs to be done once
# All clients get instant access afterwards!
```

### Configure TensorFlow Build

```bash
# Set up workspace
mkdir -p /mnt/build/tensorflow-build
cd /mnt/build/tensorflow-build

monofs-session start
cp -r /mnt/github.com/tensorflow/tensorflow/* ./

# Configure TensorFlow
./configure

# Example configuration:
# - Python location: /usr/bin/python3
# - CUDA support: N (or Y if you have GPUs)
# - Optimization flags: Use defaults
```

### Build TensorFlow

```bash
# Build TensorFlow pip package
bazel build --config=opt //tensorflow/tools/pip_package:build_pip_package

# Create the package
./bazel-bin/tensorflow/tools/pip_package/build_pip_package /tmp/tensorflow_pkg

# Build TensorFlow C library
bazel build --config=opt //tensorflow:libtensorflow.so

# Build TensorFlow Lite
bazel build --config=opt //tensorflow/lite:libtensorflowlite.so

# Commit built artifacts
monofs-session commit \
  --message="TensorFlow build $(date)" \
  --files="bazel-bin/"
```

## Advanced: Shared Bazel Cache Across Cluster

MonoFS enables sharing Bazel's build cache across all clients:

### Set Up Shared Cache

```bash
# Create shared cache directories in MonoFS
mkdir -p /mnt/cache/bazel/shared-repos
mkdir -p /mnt/cache/bazel/shared-disk
mkdir -p /mnt/cache/bazel/shared-output

# Set permissions
chmod 777 /mnt/cache/bazel/shared-*
```

### Configure All Clients

Add to each client's `~/.bazelrc`:

```bash
# Shared cache configuration for MonoFS cluster
build --repository_cache=/mnt/cache/bazel/shared-repos
build --disk_cache=/mnt/cache/bazel/shared-disk
build --experimental_remote_cache_compression

# Optional: Use action cache
build --remote_cache=grpc://localhost:9092
```

### Benefits

- **First build**: Client A builds project, cache populated
- **Second build**: Client B gets instant build (cache hit!)
- **Incremental builds**: Only changed files recompiled
- **Disk savings**: 90%+ reduction vs per-client caches

## Example 4: Multi-Language Bazel Project

Build a project using multiple languages (Go, Java, Python):

### Project Structure

```bash
# Ingest example multi-language project
monofs-admin ingest \
  --url=https://github.com/bazelbuild/examples \
  --ref=main \
  --display-path=github.com/bazelbuild/examples

# Navigate to examples
cd /mnt/build/bazel-examples
monofs-session start
cp -r /mnt/github.com/bazelbuild/examples/* ./
```

### Build All Languages

```bash
# Build Go targets
bazel build //go/...

# Build Java targets
bazel build //java-tutorial/...

# Build Python targets
bazel build //python/...

# Build C++ targets
bazel build //cpp-tutorial/...

# Build everything
bazel build //...

# Run tests for all languages
bazel test //...
```

## Performance Metrics

### TensorFlow Build Comparison

**Traditional Git Clone + Build:**
```bash
# Clone repository
time git clone --depth 1 https://github.com/tensorflow/tensorflow
# Real: 180 seconds (large repo)

# Download Bazel dependencies
time bazel build --nobuild //tensorflow/tools/pip_package:build_pip_package
# Real: 900 seconds (many dependencies)

# Actual build
time bazel build --config=opt //tensorflow/tools/pip_package:build_pip_package
# Real: 3600 seconds (1 hour)

# Total first build: ~4680 seconds (~78 minutes)
```

**MonoFS Build (First Client):**
```bash
# Access source (instant)
time ls /mnt/github.com/tensorflow/tensorflow
# Real: 0.01 seconds

# Copy to workspace
time cp -r /mnt/github.com/tensorflow/tensorflow/* ./
# Real: 15 seconds

# Build (dependencies cached by fetcher)
time bazel build --config=opt //tensorflow/tools/pip_package:build_pip_package
# Real: 3400 seconds (~57 minutes)

# Total: ~3415 seconds (~57 minutes)
# Speedup: 1.37x faster
```

**MonoFS Build (Second Client, Shared Cache):**
```bash
# Access source (instant)
time ls /mnt/github.com/tensorflow/tensorflow
# Real: 0.01 seconds

# Copy to workspace
time cp -r /mnt/github.com/tensorflow/tensorflow/* ./
# Real: 15 seconds

# Build (cache hits from first client!)
time bazel build --config=opt //tensorflow/tools/pip_package:build_pip_package
# Real: 120 seconds (mostly cache hits!)

# Total: ~135 seconds (~2 minutes)
# Speedup: 34x faster than traditional!
```

## CI/CD Integration

### Jenkins Pipeline

```groovy
// Jenkinsfile
pipeline {
    agent { label 'monofs-enabled' }
    
    stages {
        stage('Checkout from MonoFS') {
            steps {
                sh '''
                    BUILD_DIR="/mnt/build/jenkins-${BUILD_ID}"
                    mkdir -p "$BUILD_DIR"
                    cd "$BUILD_DIR"
                    cp -r /mnt/github.com/myorg/myproject/* ./
                '''
            }
        }
        
        stage('Build with Bazel') {
            steps {
                sh '''
                    cd "/mnt/build/jenkins-${BUILD_ID}"
                    bazel build \
                        --repository_cache=/mnt/cache/bazel/repos \
                        --disk_cache=/mnt/cache/bazel/disk \
                        --jobs=16 \
                        //...
                '''
            }
        }
        
        stage('Test') {
            steps {
                sh '''
                    cd "/mnt/build/jenkins-${BUILD_ID}"
                    bazel test //...
                '''
            }
        }
        
        stage('Package') {
            steps {
                sh '''
                    cd "/mnt/build/jenkins-${BUILD_ID}"
                    bazel build //:package
                    monofs-session commit \
                        --message="Build ${BUILD_ID}" \
                        --files="bazel-bin/"
                '''
            }
        }
    }
}
```

## Troubleshooting

### Bazel Can't Find Dependencies

```bash
# Clear Bazel cache
bazel clean --expunge

# Verify repository cache
ls -la /mnt/cache/bazel/repos/

# Rebuild with verbose output
bazel build --verbose_failures --explain=build_explain.txt //...
```

### Permission Issues with Shared Cache

```bash
# Fix cache permissions
sudo chown -R monofs:monofs /mnt/cache/bazel/
chmod -R 777 /mnt/cache/bazel/
```

### Slow External Repository Fetches

```bash
# Check fetcher service
monofs-admin status

# Pre-fetch known dependencies
bazel fetch //...

# Use local mirror
bazel build --repository_cache=/mnt/cache/bazel/repos //...
```

## Best Practices

1. **Use Shared Caches**: Configure all clients to use MonoFS shared cache
2. **Pre-fetch Large Projects**: Ingest during off-peak hours
3. **Version Pin**: Use specific refs for reproducible builds
4. **Monitor Cache Size**: Set up cache cleanup policies
5. **Parallel Builds**: Leverage MonoFS to run parallel builds safely

## Next Steps

- Go projects: See [howto-build-prometheus.md](./howto-build-prometheus.md)
- NPM projects: See [howto-build-react-app.md](./howto-build-react-app.md)
- Multi-backend: See [howto-build-multi-backend.md](./howto-build-multi-backend.md)
