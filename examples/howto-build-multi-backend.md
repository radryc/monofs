# How to Build Multi-Backend Projects Using MonoFS

This guide demonstrates building complex projects that use multiple build systems and package managers (Go + NPM, Bazel + Maven, etc.) using MonoFS.

## Overview

Many modern projects combine multiple languages and build systems. MonoFS excels at managing these complex dependency graphs by providing unified access to all package sources.

## Example 1: Build Kubernetes (Go + Bazel + Docker)

Kubernetes uses Go, Bazel, and Docker together. This is one of the most complex builds.

### Step 1: Ingest Kubernetes

```bash
# Ingest Kubernetes (large repo, ~700MB)
monofs-admin ingest \
  --url=https://github.com/kubernetes/kubernetes \
  --ref=master \
  --display-path=github.com/kubernetes/kubernetes

# Wait for ingestion
monofs-admin status --filter=kubernetes

# Verify source
ls /mnt/github.com/kubernetes/kubernetes/
# Should see: cmd/ pkg/ staging/ hack/ build/ Makefile
```

### Step 2: Ingest All Dependencies

```bash
# Ingest Go module dependencies
monofs-admin ingest-deps \
  --file=/mnt/github.com/kubernetes/kubernetes/go.mod \
  --type=go

# Ingest Bazel dependencies
monofs-admin ingest-deps \
  --file=/mnt/github.com/kubernetes/kubernetes/WORKSPACE \
  --type=bazel

# Wait for both to complete
monofs-admin status --filter="kubernetes"

# All dependencies now in /mnt/go-modules/ and /mnt/bazel-cache/
```

### Step 3: Build Kubernetes Components (100% Offline)

```bash
# Navigate to source in /mnt
cd /mnt/github.com/kubernetes/kubernetes/

# Start session
monofs-session start

# Verify versions
go version  # Should be 1.21+
bazel version
```

#### Build with Make (Go Backend)

```bash
# Build all Kubernetes binaries using monofs-build
monofs-build go -- make all

# Or build specific components
monofs-build go -- make WHAT=cmd/kubelet
monofs-build go -- make WHAT=cmd/kube-apiserver
monofs-build go -- make WHAT=cmd/kube-controller-manager
monofs-build go -- make WHAT=cmd/kubectl

# Binaries are in _output/bin/
ls -lh _output/bin/

# 🚀 100% offline! Uses /mnt/go-modules/ only
```

#### Build with Bazel (100% Offline)

```bash
# Build using monofs-build (uses /mnt/bazel-cache/)
monofs-build bazel -- build //cmd/kubelet
monofs-build bazel -- build //cmd/kube-apiserver
monofs-build bazel -- build //pkg/...

# Run tests offline
monofs-build bazel -- test //pkg/kubelet/...
monofs-build bazel -- test //pkg/scheduler/...

# 🚀 Zero network access - all deps from /mnt/bazel-cache/
```

### Step 4: Build Container Images (Offline)

```bash
# Build all images using monofs-build
monofs-build make -- release-images

# Or build specific images
monofs-build make -- image-kube-apiserver
monofs-build make -- image-kube-controller-manager

# Images are built and tagged
docker images | grep k8s

# All dependencies pulled from /mnt - no registry access needed!
```

### Step 5: Cross-Compile for Multiple Platforms (Offline)

```bash
# Build for Linux AMD64
monofs-build go -- make all KUBE_BUILD_PLATFORMS=linux/amd64

# Build for Linux ARM64
monofs-build go -- make all KUBE_BUILD_PLATFORMS=linux/arm64

# Build for multiple platforms
monofs-build go -- make all KUBE_BUILD_PLATFORMS=linux/amd64,linux/arm64,darwin/amd64

# Check outputs
ls -lh _output/dockerized/bin/

# All toolchains cached in /mnt - 100% offline!
```

### Step 6: Run E2E Tests (Offline)

```bash
# Build test binaries
monofs-build go -- make WHAT=test/e2e/e2e.test
monofs-build go -- make WHAT=cmd/kubectl

# Run E2E tests (requires running cluster)
monofs-build go -- make test-e2e

# Or run specific tests
monofs-build go -- make test-e2e-node
```

### Commit Build Artifacts

```bash
# Commit all binaries to MonoFS
monofs-session commit \
  --message="Kubernetes $(git rev-parse --short HEAD) build"

# Binaries instantly available to all clients!
ls /mnt/github.com/kubernetes/kubernetes/_output/bin/kubectl
```

## Example 2: Build Grafana (Go + Node.js + Yarn)

Grafana combines Go backend with TypeScript/React frontend.

### Ingest Grafana

```bash
# Ingest Grafana
monofs-admin ingest \
  --url=https://github.com/grafana/grafana \
  --ref=main \
  --display-path=github.com/grafana/grafana

# Verify
ls /mnt/github.com/grafana/grafana/
```

### Build Grafana

```bash
# Ingest dependencies for both Go and npm
monofs-admin ingest-deps \
  --file=/mnt/github.com/grafana/grafana/go.mod \
  --type=go

monofs-admin ingest-deps \
  --file=/mnt/github.com/grafana/grafana/package.json \
  --type=npm

# Navigate to source
cd /mnt/github.com/grafana/grafana/
monofs-session start

# Install frontend dependencies (100% offline)
monofs-build npm -- install

# Build frontend (offline)
monofs-build npm -- run build

# Build Go backend (offline)
monofs-build go -- build -o bin/grafana-server ./pkg/cmd/grafana-server

# Or use Make
monofs-build make -- build

# Verify binaries
ls -lh bin/grafana-server
ls -lh public/build/

# 🚀 100% offline build! All from /mnt/go-modules/ and /mnt/npm-cache/

# Run Grafana
./bin/grafana-server \
  --homepath=. \
  --config=conf/defaults.ini

# Access at http://localhost:3000

# Commit build
monofs-session commit --message="Grafana $(git describe --tags)"
```

### Build Grafana Plugins

```bash
# Build all plugins
make build-go
make build-js

# Build specific plugin
cd public/app/plugins/datasource/prometheus
yarn build

# Package plugin
yarn plugin:build

# Commit plugins
cd /mnt/build/grafana-build
monofs-session commit \
  --message="Grafana build with plugins" \
  --files="bin/,public/build/,plugins/"
```

## Example 3: Build GitLab (Ruby + Go + Node.js)

GitLab uses Ruby (Rails), Go (Gitaly), and Node.js (frontend).

### Ingest GitLab

```bash
# Ingest GitLab CE (very large)
monofs-admin ingest \
  --url=https://gitlab.com/gitlab-org/gitlab-foss \
  --ref=master \
  --display-path=gitlab.com/gitlab-org/gitlab-foss

# Ingest Gitaly (Go component)
monofs-admin ingest \
  --url=https://gitlab.com/gitlab-org/gitaly \
  --ref=master \
  --display-path=gitlab.com/gitlab-org/gitaly
```

### Build GitLab Components

```bash
# Create workspace
mkdir -p /mnt/build/gitlab-build
cd /mnt/build/gitlab-build

monofs-session start

# Build Gitaly (Go backend)
mkdir -p gitaly
cp -r /mnt/gitlab.com/gitlab-org/gitaly/* gitaly/
cd gitaly

# Install Go dependencies
go mod download

# Build Gitaly
make build

# Binaries in _build/bin/
ls -lh _build/bin/

cd ..

# Build GitLab Rails (Ruby backend)
mkdir -p gitlab
cp -r /mnt/gitlab.com/gitlab-org/gitlab-foss/* gitlab/
cd gitlab

# Install Ruby gems (if Ruby/Bundler installed)
bundle install

# Install Node.js dependencies
yarn install

# Build frontend assets
yarn webpack

# Precompile Rails assets
bundle exec rake assets:precompile

# Commit all components
cd /mnt/build/gitlab-build
monofs-session commit \
  --message="GitLab full build" \
  --files="gitaly/_build/,gitlab/public/assets/"
```

## Example 4: Build Envoy + gRPC (C++ + Bazel + Go)

Build Envoy proxy with gRPC support.

### Ingest Repositories

```bash
# Ingest Envoy
monofs-admin ingest \
  --url=https://github.com/envoyproxy/envoy \
  --ref=main \
  --display-path=github.com/envoyproxy/envoy

# Ingest gRPC
monofs-admin ingest \
  --url=https://github.com/grpc/grpc \
  --ref=master \
  --display-path=github.com/grpc/grpc

# Ingest gRPC-Go
monofs-admin ingest \
  --url=https://github.com/grpc/grpc-go \
  --ref=master \
  --display-path=github.com/grpc/grpc-go
```

### Build Everything

```bash
# Build workspace
mkdir -p /mnt/build/envoy-grpc-build
cd /mnt/build/envoy-grpc-build

monofs-session start

# Build Envoy (C++ with Bazel)
mkdir -p envoy
cp -r /mnt/github.com/envoyproxy/envoy/* envoy/
cd envoy

# Configure Bazel cache
cat > .bazelrc.user <<EOF
build --repository_cache=/mnt/cache/bazel/envoy-repos
build --disk_cache=/mnt/cache/bazel/envoy-disk
build --jobs=16
EOF

# Build Envoy
bazel build //source/exe:envoy-static

cd ..

# Build gRPC (C++ core)
mkdir -p grpc
cp -r /mnt/github.com/grpc/grpc/* grpc/
cd grpc

# Build gRPC C++ libraries
mkdir -p cmake/build
cd cmake/build
cmake ../..
make -j16

cd ../../..

# Build gRPC-Go examples
mkdir -p grpc-go
cp -r /mnt/github.com/grpc/grpc-go/* grpc-go/
cd grpc-go/examples/helloworld

# Build Go examples
go build -o greeter_server ./greeter_server
go build -o greeter_client ./greeter_client

# Test gRPC with Envoy
cd /mnt/build/envoy-grpc-build

# Start Envoy with gRPC config
./envoy/bazel-bin/source/exe/envoy-static -c envoy-grpc.yaml &

# Start gRPC server
./grpc-go/examples/helloworld/greeter_server &

# Test client
./grpc-go/examples/helloworld/greeter_client

# Commit everything
monofs-session commit \
  --message="Envoy + gRPC full build" \
  --files="envoy/bazel-bin/,grpc/cmake/build/,grpc-go/examples/"
```

## Example 5: Build VS Code (TypeScript + Electron + Node.js)

VS Code is a complex Electron app with TypeScript.

### Ingest VS Code

```bash
# Ingest VS Code
monofs-admin ingest \
  --url=https://github.com/microsoft/vscode \
  --ref=main \
  --display-path=github.com/microsoft/vscode
```

### Build VS Code

```bash
# Create workspace
mkdir -p /mnt/build/vscode-build
cd /mnt/build/vscode-build

monofs-session start
cp -r /mnt/github.com/microsoft/vscode/* ./

# Configure Yarn cache
yarn config set cache-folder /mnt/cache/yarn

# Install dependencies (many!)
yarn install

# Build VS Code
yarn run gulp vscode-linux-x64

# Or build for other platforms
yarn run gulp vscode-darwin-x64
yarn run gulp vscode-win32-x64

# Package the build
yarn run gulp vscode-linux-x64-min

# Built package is in ../VSCode-linux-x64/
ls -lh ../VSCode-linux-x64/

# Commit build
monofs-session commit \
  --message="VS Code build" \
  --files="../VSCode-linux-x64/"
```

## Performance Comparison: Kubernetes Build

### Traditional Build

```bash
# Clone Kubernetes
time git clone --depth 1 https://github.com/kubernetes/kubernetes
# Real: 120 seconds (large repo)

cd kubernetes

# Build (first time)
time make all
# Real: 900 seconds (15 minutes)

# Total: ~1020 seconds (17 minutes)
```

### MonoFS Build (First Client)

```bash
# Access source (instant)
time ls /mnt/github.com/kubernetes/kubernetes
# Real: 0.01 seconds

# Copy source
time cp -r /mnt/github.com/kubernetes/kubernetes/* ./
# Real: 8 seconds

# Build (dependencies cached by fetcher)
time make all
# Real: 720 seconds (12 minutes, fetcher caches help)

# Total: ~728 seconds (12 minutes)
# Speedup: 1.4x faster
```

### MonoFS Build (Second Client, Same Day)

```bash
# Access source
time cp -r /mnt/github.com/kubernetes/kubernetes/* ./
# Real: 8 seconds

# Build (all dependencies cached!)
time make all
# Real: 600 seconds (10 minutes, Go build cache + Bazel cache)

# Total: ~608 seconds (10 minutes)
# Speedup: 1.68x faster
```

## Advanced: Multi-Backend CI Pipeline

```yaml
# .github/workflows/multi-backend.yml
name: Multi-Backend Build

on: [push, pull_request]

jobs:
  build-go:
    runs-on: self-hosted
    steps:
      - name: Build Go components
        run: |
          cd /mnt/build/ci-go-${{ github.run_id }}
          cp -r /mnt/github.com/myorg/myproject/* ./
          make build-go
          monofs-session commit --message="Go build" --files="bin/"

  build-node:
    runs-on: self-hosted
    steps:
      - name: Build Node components
        run: |
          cd /mnt/build/ci-node-${{ github.run_id }}
          cp -r /mnt/github.com/myorg/myproject/* ./
          npm config set cache /mnt/cache/npm
          npm install
          npm run build
          monofs-session commit --message="Node build" --files="dist/"

  build-bazel:
    runs-on: self-hosted
    steps:
      - name: Build Bazel targets
        run: |
          cd /mnt/build/ci-bazel-${{ github.run_id }}
          cp -r /mnt/github.com/myorg/myproject/* ./
          bazel build --repository_cache=/mnt/cache/bazel/repos //...
          monofs-session commit --message="Bazel build" --files="bazel-bin/"

  integration-test:
    needs: [build-go, build-node, build-bazel]
    runs-on: self-hosted
    steps:
      - name: Run integration tests
        run: |
          cd /mnt/build/ci-integration-${{ github.run_id }}
          # All artifacts available from previous jobs!
          ./bin/backend &
          npm run test:e2e
```

## Best Practices

1. **Separate Build Directories**: One per backend/language
2. **Use Shared Caches**: Configure all tools to use MonoFS cache
3. **Commit Incrementally**: Commit after each backend builds
4. **Parallel Builds**: Different backends can build simultaneously
5. **Version Lock**: Pin all dependency versions for reproducibility

## Troubleshooting

### Conflicting Dependencies

```bash
# Go and Node.js both need different protobuf versions
# Solution: Use separate directories
mkdir -p go-build node-build
cp -r /mnt/source/* go-build/
cp -r /mnt/source/* node-build/

# Build separately
cd go-build && make build-go
cd ../node-build && npm run build
```

### Cache Corruption

```bash
# Clear all caches
rm -rf /mnt/cache/npm/*
rm -rf /mnt/cache/yarn/*
rm -rf /mnt/cache/bazel/*
go clean -modcache
```

### Out of Disk Space

```bash
# Check overlay usage
du -sh /var/lib/monofs/overlay/

# Clean old builds
monofs-session cleanup --older-than=7d

# Remove build artifacts
cd /mnt/build
find . -name "node_modules" -type d -exec rm -rf {} +
find . -name "_output" -type d -exec rm -rf {} +
```

## Next Steps

- Self-hosting: See [howto-build-monofs.md](./howto-build-monofs.md)
- Go projects: See [howto-build-prometheus.md](./howto-build-prometheus.md)
- Bazel: See [howto-bazel-build.md](./howto-bazel-build.md)
- NPM: See [howto-build-react-app.md](./howto-build-react-app.md)
