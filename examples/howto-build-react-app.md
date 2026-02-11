# How to Build React/NPM Projects Using MonoFS

This guide demonstrates building React applications and NPM-based projects using MonoFS for dependency management and build artifacts.

## Overview

Modern JavaScript projects rely heavily on NPM dependencies. MonoFS can cache and share these dependencies across the cluster, dramatically reducing install times and disk usage.

## Prerequisites

- MonoFS cluster running
- Node.js 18+ installed
- npm or yarn installed

## Example 1: Build Create React App

### Step 1: Ingest React Repository

```bash
# Ingest Create React App
monofs-admin ingest \
  --url=https://github.com/facebook/create-react-app \
  --ref=main \
  --display-path=github.com/facebook/create-react-app

# Verify
ls /mnt/github.com/facebook/create-react-app/
```

### Step 2: Create New React App in /mnt

```bash
# Create project directory in /mnt
mkdir -p /mnt/build/my-react-app
cd /mnt/build/my-react-app

# Start session
monofs-session start

# Initialize React app (using create-react-app template)
monofs-build npm -- create-react-app@latest . --use-npm

# Or manually copy template structure from ingested repo
cp /mnt/github.com/facebook/create-react-app/packages/cra-template/template.json .
cp -r /mnt/github.com/facebook/create-react-app/packages/cra-template/template/* .
```

### Step 3: Ingest Dependencies (One-Time Setup)

```bash
# Ingest all npm dependencies from package.json
monofs-admin ingest-deps \
  --file=/mnt/build/my-react-app/package.json \
  --type=npm

# Wait for ingestion
monofs-admin status --filter="npm"

# All packages now cached in /mnt/npm-cache/

# Install dependencies (100% offline, uses MonoFS cache)
monofs-build npm -- install

# 🚀 Zero downloads! All packages from /mnt/npm-cache/
```

### Step 4: Build the Application (Offline)

```bash
# Development build
monofs-build npm -- run start  # Runs on localhost:3000

# Production build (100% offline)
monofs-build npm -- run build

# Verify build output
ls -lh build/
# Should see: static/ index.html manifest.json etc.

# Commit build artifacts to MonoFS
monofs-session commit \
  --message="React app production build $(date +%Y-%m-%d)"

# Build artifacts now accessible from /mnt to all clients!
```

## Example 2: Build Next.js Application

### Ingest Next.js

```bash
# Ingest Next.js repository with examples
monofs-admin ingest \
  --url=https://github.com/vercel/next.js \
  --ref=canary \
  --display-path=github.com/vercel/next.js

# Browse examples
ls /mnt/github.com/vercel/next.js/examples/
```

### Create Next.js App

```bash
# Create workspace in /mnt
mkdir -p /mnt/build/my-nextjs-app
cd /mnt/build/my-nextjs-app

monofs-session start

# Copy example template
cp -r /mnt/github.com/vercel/next.js/examples/blog-starter/* ./

# Ingest npm dependencies
monofs-admin ingest-deps \
  --file=/mnt/build/my-nextjs-app/package.json \
  --type=npm

# Install dependencies (100% offline)
monofs-build npm -- install

# Verify node_modules
du -sh node_modules/
# 🚀 All from /mnt/npm-cache/ - zero downloads!
```

### Build and Export (Offline)

```bash
# Development server
monofs-build npm -- run dev

# Production build (100% offline)
monofs-build npm -- run build

# Start production server
monofs-build npm -- run start

# Check build size
du -sh .next/

# Commit artifacts
monofs-session commit \
  --message="Next.js build $(date +%Y-%m-%d)"

# Build artifacts accessible from /mnt instantly!
```

## Example 3: Build Vue.js Application

### Ingest Vue.js

```bash
# Ingest Vue.js core
monofs-admin ingest \
  --url=https://github.com/vuejs/core \
  --ref=main \
  --display-path=github.com/vuejs/core

# Ingest Vue CLI
monofs-admin ingest \
  --url=https://github.com/vuejs/vue-cli \
  --ref=dev \
  --display-path=github.com/vuejs/vue-cli
```

### Create Vue App

```bash
# Create workspace
mkdir -p /mnt/build/my-vue-app
cd /mnt/build/my-vue-app

monofs-session start

# Create Vue app
npm config set cache /mnt/cache/npm
npx @vue/cli create my-app

# Select options:
# - Vue 3
# - TypeScript: Yes
# - Router: Yes
# - Vuex: Yes
# - ESLint: Yes

cd my-app
```

### Build Vue Application

```bash
# Install dependencies (cached)
npm install

# Development server
npm run serve

# Production build
npm run build

# Check dist folder
ls -lh dist/

# Commit build
monofs-session commit \
  --message="Vue app production build" \
  --files="dist/"
```

## Example 4: Build Complex Monorepo (Nx/Turborepo)

### Build Nx Monorepo

```bash
# Ingest Nx repository
monofs-admin ingest \
  --url=https://github.com/nrwl/nx \
  --ref=master \
  --display-path=github.com/nrwl/nx

# Create workspace
mkdir -p /mnt/build/my-nx-workspace
cd /mnt/build/my-nx-workspace

monofs-session start

# Create Nx workspace
npm config set cache /mnt/cache/npm
npx create-nx-workspace my-workspace

# Navigate to workspace
cd my-workspace

# Add React app
npx nx g @nrwl/react:app my-frontend

# Add Express backend
npx nx g @nrwl/express:app my-backend

# Add shared library
npx nx g @nrwl/react:lib shared-ui
```

### Build All Projects

```bash
# Build all apps
npx nx run-many --target=build --all

# Build specific app
npx nx build my-frontend

# Build with cache
npx nx build my-backend --with-deps

# Check Nx cache (stored in MonoFS!)
ls -lh .nx/cache/

# Run tests
npx nx test my-frontend
npx nx test my-backend

# Commit entire workspace
monofs-session commit \
  --message="Nx workspace build" \
  --files="dist/"
```

## Advanced: Shared NPM Cache Strategy

### Configure Global NPM Cache

On each MonoFS client:

```bash
# Create shared cache directories
mkdir -p /mnt/cache/npm
mkdir -p /mnt/cache/yarn
mkdir -p /mnt/cache/pnpm

# Set global npm config
npm config set cache /mnt/cache/npm --global

# Set yarn config
yarn config set cache-folder /mnt/cache/yarn --global

# Set pnpm config
pnpm config set store-dir /mnt/cache/pnpm --global
```

### Performance Benefits

**Traditional npm install:**
```bash
# First install
time npm install
# Real: 120 seconds (download all packages)

# Second install (different directory)
cd ../another-project
time npm install
# Real: 115 seconds (download again!)
```

**MonoFS npm install:**
```bash
# First install (Client A)
time npm install
# Real: 120 seconds (cache populated)

# Second install (Client B, same packages)
cd /mnt/build/another-project
time npm install
# Real: 5 seconds (cache hit!)

# Disk savings: 90%+ (shared node_modules)
```

## Example 5: Build TypeScript Library

### Create TypeScript Library

```bash
# Create library workspace
mkdir -p /mnt/build/my-ts-library
cd /mnt/build/my-ts-library

monofs-session start

# Initialize package
npm init -y

# Install TypeScript
npm config set cache /mnt/cache/npm
npm install --save-dev typescript @types/node

# Create tsconfig.json
cat > tsconfig.json <<EOF
{
  "compilerOptions": {
    "target": "ES2020",
    "module": "commonjs",
    "declaration": true,
    "outDir": "./dist",
    "rootDir": "./src",
    "strict": true,
    "esModuleInterop": true
  },
  "include": ["src/**/*"],
  "exclude": ["node_modules", "dist"]
}
EOF

# Create source
mkdir -p src
cat > src/index.ts <<EOF
export function hello(name: string): string {
  return \`Hello, \${name}!\`;
}
EOF
```

### Build and Package

```bash
# Build TypeScript
npx tsc

# Check output
ls -la dist/

# Add build script to package.json
npm pkg set scripts.build="tsc"
npm pkg set scripts.test="jest"
npm pkg set scripts.prepublish="npm run build"

# Build
npm run build

# Run tests
npm test

# Package for npm
npm pack

# Commit library
monofs-session commit \
  --message="TypeScript library v1.0.0" \
  --files="dist/,*.tgz"
```

## CI/CD Integration

### GitHub Actions with MonoFS

```yaml
# .github/workflows/build.yml
name: Build React App

on: [push, pull_request]

jobs:
  build:
    runs-on: self-hosted  # Must have MonoFS mounted
    
    steps:
      - name: Setup workspace
        run: |
          BUILD_DIR="/mnt/build/ci-${{ github.run_id }}"
          mkdir -p "$BUILD_DIR"
          echo "BUILD_DIR=$BUILD_DIR" >> $GITHUB_ENV
      
      - name: Copy source from MonoFS
        run: |
          cp -r /mnt/github.com/myorg/myapp/* "$BUILD_DIR/"
      
      - name: Configure npm cache
        run: |
          npm config set cache /mnt/cache/npm
      
      - name: Install dependencies
        run: |
          cd "$BUILD_DIR"
          npm ci
      
      - name: Build
        run: |
          cd "$BUILD_DIR"
          npm run build
      
      - name: Test
        run: |
          cd "$BUILD_DIR"
          npm test
      
      - name: Commit artifacts
        run: |
          cd "$BUILD_DIR"
          monofs-session start
          monofs-session commit \
            --message="Build ${{ github.run_id }}" \
            --files="build/"
```

## Performance Comparison

### Large React App Build

**Traditional Build:**
```bash
# Clone repo
git clone https://github.com/myorg/large-app
cd large-app

# Install 1500+ packages
time npm install
# Real: 180 seconds

# Build
time npm run build
# Real: 120 seconds

# Total: 300 seconds
```

**MonoFS Build (First Client):**
```bash
# Access source (instant)
ls /mnt/github.com/myorg/large-app

# Copy source
time cp -r /mnt/github.com/myorg/large-app/* ./
# Real: 2 seconds

# Install with MonoFS cache
time npm install
# Real: 180 seconds (first time)

# Build
time npm run build
# Real: 120 seconds

# Total: 302 seconds (similar to traditional)
```

**MonoFS Build (Second Client, Same Day):**
```bash
# Access source
cp -r /mnt/github.com/myorg/large-app/* ./
# Real: 2 seconds

# Install (cache hit!)
time npm install
# Real: 8 seconds (all packages cached!)

# Build
time npm run build
# Real: 120 seconds

# Total: 130 seconds
# Speedup: 2.3x faster!
```

## Troubleshooting

### NPM Cache Corruption

```bash
# Clear MonoFS npm cache
rm -rf /mnt/cache/npm/*

# Verify cache
npm cache verify
```

### Package Lock Issues

```bash
# Remove lock file
rm package-lock.json

# Clean install
rm -rf node_modules
npm install
```

### Build Failures

```bash
# Clear build cache
rm -rf .next/ build/ dist/

# Clean node_modules
rm -rf node_modules
npm ci

# Rebuild
npm run build
```

## Best Practices

1. **Always Configure NPM Cache**: Point to `/mnt/cache/npm`
2. **Use CI for Reproducibility**: Lock dependencies with package-lock.json
3. **Monitor Cache Size**: Set up cleanup for old packages
4. **Version Dependencies**: Avoid `^` or `~` in production
5. **Commit Build Artifacts**: Use MonoFS sessions for deployable builds

## Next Steps

- Go projects: See [howto-build-prometheus.md](./howto-build-prometheus.md)
- Bazel projects: See [howto-bazel-build.md](./howto-bazel-build.md)
- Multi-backend: See [howto-build-multi-backend.md](./howto-build-multi-backend.md)
- Kubernetes: See [howto-build-kubernetes.md](./howto-build-kubernetes.md)
