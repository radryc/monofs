# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

---

## Overview

MonoFS is a distributed FUSE filesystem that mounts Git repositories, Go modules, npm packages, Maven artifacts, and Cargo crates as a unified, mountable filesystem. The system uses HRW (Highest Random Weight) consistent hashing to distribute file metadata across backend nodes with automatic failover.

**Primary Language:** Go 1.24+
**Key Dependencies:** go-fuse (FUSE), NutsDB (metadata storage), Zoekt (search), gRPC (inter-service communication)

---

## Build & Development Commands

### Building Binaries

```bash
# Build all binaries (outputs to bin/)
make build

# Build specific components
make build-server    # Backend node
make build-router    # Cluster coordinator
make build-client    # FUSE client
make build-admin     # Admin CLI
make build-session   # Write session CLI
make build-search    # Search service
make build-fetcher   # Blob fetcher service
```

### Testing

```bash
# Run all unit tests
make test
make test-unit  # Same as 'make test'

# Run specific test suites
make test-deadlock  # Deadlock detection tests
make test-stress    # Stress and edge case tests (requires FUSE)
make test-e2e       # End-to-end integration tests (requires FUSE)
make test-e2e-sudo  # E2E tests with sudo (for FUSE mount permissions)

# Run tests with race detector or coverage
make test-race
make test-coverage  # Generates coverage.html
```

**Important:** E2E tests require FUSE support and may need sudo for mounting. They clean up test mounts in `/tmp/monofs-e2e-test` automatically.

### Running a Local Cluster

```bash
# Option 1: Docker cluster (recommended)
make deploy         # Starts HAProxy + 2 routers + 5 backend nodes + 2 fetchers + search
make deploy-stop    # Stop cluster
make deploy-clean   # Stop and remove all data

# Option 2: Local dev cluster (no Docker)
make deploy-local           # Starts 3 backend nodes + 1 router
make deploy-local-stop      # Stop nodes
make deploy-local-clean     # Stop and remove data

# View logs
make docker-logs    # For Docker cluster
tail -f /tmp/monofs-dev/*.log  # For local cluster
```

### Mounting the Filesystem

```bash
# Mount as read-only
make mount MOUNT_POINT=/tmp/monofs

# Mount with write support
make mount-writable MOUNT_POINT=/tmp/monofs

# Unmount
make unmount MOUNT_POINT=/tmp/monofs
```

### Code Quality

```bash
make fmt        # Format code with gofmt
make fmt-check  # Check if code is formatted
make vet        # Run go vet
make tidy       # Tidy go.mod
```

### Protocol Buffers

```bash
# Regenerate protobuf files (after editing .proto files)
make proto

# Install protobuf tools
make install-tools
```

---

## Architecture Overview

MonoFS uses a **distributed metadata + external fetcher** architecture:

### Component Architecture

```
Client (FUSE)
  ↓ [gRPC connections to all nodes]
Router (topology coordinator)
  ↓ [provides cluster info via GetClusterInfo RPC]
Backend Nodes (metadata storage via NutsDB)
  ↓ [FetchBlob RPC]
Fetcher Services (external blob retrieval from Git/Go/npm/Maven/Cargo)
  ↓ [streams file content]
External Sources (GitHub, Go proxy, npm registry, etc.)
```

### Key Architectural Patterns

1. **Client-Side Routing**: Clients compute file-to-node mapping locally using HRW (no router in data path). Router only provides topology updates every 30s.

2. **Stateless vs. Stateful Split**:
   - **Stateless**: Client (local cache only), Router (in-memory cluster state), Fetcher (evictable cache)
   - **Stateful**: Backend Nodes (NutsDB with persistent metadata)

3. **Sharding via HRW**: Uses Highest Random Weight consistent hashing. For each file path, compute `score = hash(path || nodeID) * weight` and select node with highest score. Adding/removing nodes only reshuffles ~1/n of keys.

4. **Failover Model**: When primary node fails, router assigns backup node (via HRW), syncs temporary metadata to `bucketFailover` with 10-minute TTL. Backup serves failed node's files until primary recovers.

5. **Write Sessions**: Client-side overlay layering at `~/.monofs/overlay/`. All modifications stored locally until explicitly committed. Session manager tracks changes in JSON with O(1) lookup indexes.

6. **Virtual Path Mapping**: Build layout system (`internal/buildlayout/`) creates virtual paths for package managers. Example: `github.com/google/uuid@v1.6.0/uuid.go` also accessible via `go-modules/pkg/mod/github.com/google/uuid@v1.6.0/uuid.go`. Same blob, multiple paths.

### Data Flow for File Read

1. Client calls `ShardedClient.Read(path)` → computes HRW node locally
2. Client sends `Read RPC` to computed node
3. Node performs:
   - `resolvePathToStorage(path)` → `(storageID, filePath)`
   - `getHashFromPath(storageID, filePath)` → blob hash (cached in memory)
   - NutsDB lookup in `bucketMetadata` → `storedMetadata{BlobHash, Size, Mode, Mtime}`
4. If not found locally, checks `bucketFailover` (for failed nodes)
5. Node calls `Fetcher.FetchBlob()` with blob hash + source config
6. Fetcher streams content back in 64KB chunks
7. Client returns content to FUSE

### NutsDB Bucket Structure

Backend nodes use NutsDB with the following buckets:

- `bucketMetadata`: `"hash_key"` → `storedMetadata` (file metadata + blob hash)
- `bucketPathIndex`: `"storageID:filePath"` → `"hash_key"` (path-to-metadata lookup)
- `bucketDirIndex`: `"storageID:sha256(dirPath)"` → `[]dirIndexEntry` (directory listings)
- `bucketRepoLookup`: `"displayPath"` → `"storageID"` (repo ingestion lookup)
- `bucketOwnedFiles`: `"storageID:filePath"` → `"1"` (node ownership tracking)
- `bucketReplicaFiles`: `"storageID:filePath:primary:primaryNodeID"` → metadata (for failover)
- `bucketFailover`: `"failedNodeID:storageID:filePath"` → metadata (temporary TTL cache during failover)

---

## Important Implementation Details

### Path Resolution

**Critical:** Path resolution happens in two stages:
1. `resolvePathToStorage()` converts display path → `(storageID, internalPath)`
2. `getHashFromPath()` converts `(storageID, internalPath)` → `hash_key`

The `storageID` is the repository's unique identifier (UUID generated on ingestion). Multiple display paths can map to the same `storageID` (e.g., Git repo vs. Go module virtual path).

### Sharding & Node Health

The router maintains node health via periodic heartbeats (every 5 seconds). Nodes marked unhealthy after 3 missed checks (~15 seconds). Client HRW instances filter out unhealthy nodes automatically when computing file ownership.

**When modifying sharding logic:**
- Preserve hash stability: node health updates shouldn't trigger rebalancing
- Use `UpdateNodeHealthFromProto()` to update health without reordering
- Rebalancing triggered only after `rebalance-delay` (default: 10 minutes)

### Failover Implementation

Failover is **temporary** and **metadata-only**:
- Backup node doesn't replicate blob data, only metadata
- `SyncMetadataFromNode()` copies from `bucketReplicaFiles` → `bucketFailover`
- TTL ensures failover cache expires (10 minutes default)
- On primary recovery, router calls `ClearFailoverCache()` on backup

**When implementing failover changes:**
- Never permanently move metadata during failover (use TTL cache)
- Always check both `bucketMetadata` and `bucketFailover` during lookups
- Coordinate failover assignments via router (don't let nodes self-assign)

### Write Session State Management

Session state persisted to `~/.monofs/overlay/sessions/<mount-hash>/session.json`. Structure:

```go
type Session struct {
    ID        string
    MountPath string
    Changes   []Change
    Created   time.Time

    // O(1) lookup indexes (rebuilt on load)
    deletedPaths map[string]bool
    symlinks     map[string]string
    userDirs     map[string]bool
}
```

**When modifying session logic:**
- Always rebuild indexes after loading session JSON
- Update indexes on every `AddChange()` call
- Use two-phase commit: changes → committed/ directory

### Search Indexing

Search service uses persistent job queue backed by NutsDB:
- In-memory queue: channel with 100 capacity
- Persistent queue: `bucketQueue` overflow storage (10,000 capacity)
- Jobs survive restarts (loaded from `bucketJobs` on startup)

**When modifying search:**
- Job states: `QUEUED` → `RUNNING` → `COMPLETED`/`FAILED`
- Always persist job state before transitioning
- Index files via `ShardedClient.ReadDir/Read` (not direct Git access)
- Zoekt indexes stored at `indexDir/<storageID>/`

### Build Layout System

Build layout mappers are **additive**: they create new virtual path entries without modifying originals.

**Mapper Registration Flow:**
1. Router initializes `buildlayout.Registry`
2. Registers mappers: `registry.Register(golang.NewMapper())`, etc.
3. On ingestion, router calls `registry.MapAll(repoInfo, files)`
4. Matching mappers create `VirtualEntry[]` with display paths
5. Router creates new `bucketRepoLookup` entries for virtual paths
6. Both original and virtual paths point to same blob hashes

**When adding new build layout support:**
- Implement `LayoutMapper` interface in `internal/buildlayout/<ecosystem>/`
- Add registration in `cmd/monofs-router/main.go`
- Ensure `Matches()` checks `RepoInfo.IngestionType`
- Virtual paths must be deterministic (same input → same output)

---

## Testing Patterns

### Unit Tests

Most internal packages have `*_test.go` files. Key test patterns:

- **Server tests** (`internal/server/*_test.go`): Test metadata storage, replication, failover
- **Router tests** (`internal/router/*_test.go`): Test sharding, health checks, failover assignment
- **Deadlock tests** (`internal/server/deadlock_test.go`): Use `-timeout=5m` to catch lock contention
- **Integration tests** (`test/*_test.go`): Require FUSE, spin up full cluster

### Running a Single Test

```bash
# Run specific test
go test -v -run TestName ./internal/package

# Run with race detector
go test -race -v -run TestName ./internal/package

# Run with timeout
go test -v -timeout=5m -run TestDeadlock ./internal/server
```

### E2E Test Structure

E2E tests in `test/` follow this pattern:

1. Start local router + backend nodes in goroutines
2. Wait for health checks to pass
3. Mount FUSE client at `/tmp/monofs-e2e-test`
4. Run file operations
5. Verify results
6. Unmount and cleanup

**Important:** E2E tests clean up mounts automatically, but if interrupted, manually run:
```bash
fusermount -u /tmp/monofs-e2e-test || fusermount3 -u /tmp/monofs-e2e-test
```

---

## Common Development Tasks

### Adding a New Ingestion Backend

1. Create new backend in `internal/fetcher/backends/<ecosystem>/`
2. Implement `Backend` interface:
   ```go
   type Backend interface {
       Name() string
       Fetch(ctx context.Context, config BackendConfig) (FetchResult, error)
       Supports(config BackendConfig) bool
   }
   ```
3. Register in `fetcher/service.go`: `registerBackend(myBackend{}))`
4. Add build layout mapper in `internal/buildlayout/<ecosystem>/`
5. Register mapper in `cmd/monofs-router/main.go`

See `docs/ADDING_INGESTION_TYPES.md` for detailed guide.

### Modifying the gRPC API

1. Edit `.proto` files in `api/proto/`
2. Regenerate Go code: `make proto`
3. Update implementations in `internal/server/server.go` or `internal/router/router.go`
4. Update clients in `internal/client/sharded.go`

### Adding a New Web UI Page

1. Create HTML template in `internal/router/templates/<page>.html`
2. Add handler in `internal/router/http_handlers.go`
3. Register route in `router.go`: `http.HandleFunc("/page", r.handlePage)`
4. Update navigation in `templates/index.html`

### Debugging Cluster Issues

```bash
# Check cluster status
./bin/monofs-admin status --router=localhost:9090

# Check node health and failover state
./bin/monofs-admin failover --router=localhost:9090

# View logs
# Docker: make docker-logs
# Local: tail -f /tmp/monofs-dev/*.log

# Check HAProxy stats (Docker only)
open http://localhost:8404/stats
```

---

## Environment Variables

- `MONOFS_ROUTER`: Default router address (default: `localhost:9090`)
- `MONOFS_OVERLAY_DIR`: Write session storage directory (default: `~/.monofs/overlay`)
- `MONOFS_MOUNT`: Default mount point for `monofs-build` CLI

---

## Deployment Architecture

### Docker Compose Setup

The `docker-compose.yml` defines a full cluster:

- **HAProxy**: Load balancer at `localhost:9090` (gRPC) and `localhost:8080` (HTTP)
- **router1, router2**: Redundant routers with peer awareness
- **node1-5**: Backend nodes with persistent volumes
- **fetcher1-2**: External blob fetchers in DMZ network
- **search**: Zoekt-based search service
- **client, client2, client3**: Interactive SSH clients with auto-mounted FUSE at `/mnt`

### Network Topology

- `monofs-net`: Internal network (router, nodes, search, clients)
- `monofs-external`: DMZ network (fetchers only, for external access)

### Accessing Docker Cluster

```bash
# Web UI
open http://localhost:8080

# SSH to client
ssh monofs@localhost -p 2222  # client1
ssh monofs@localhost -p 2223  # client2
ssh monofs@localhost -p 2224  # client3
# Password: monofs

# Inside client container
ls /mnt                          # Browse filesystem
monofs-admin status              # Check cluster
monofs-session status            # Check write session
tail -f /var/log/monofs-client.log  # View client logs
```

---

## Critical Files Reference

- `cmd/monofs-router/main.go`: Router entry point, build layout registration
- `internal/router/router.go`: Cluster coordination, health checks, failover
- `internal/server/server.go`: Backend node gRPC implementation
- `internal/client/sharded.go`: Client-side HRW routing
- `internal/sharding/hrw.go`: Consistent hashing implementation
- `internal/server/failover.go`: Failover metadata synchronization
- `internal/fuse/session.go`: Write session management
- `internal/fuse/overlay.go`: Overlay filesystem merging
- `internal/search/service.go`: Search indexing and querying
- `internal/buildlayout/types.go`: Build layout mapper interface

---

## Performance Considerations

- **Metadata Caching**: Clients cache metadata locally; stale cache can cause `ENOENT` errors
- **Directory Listing**: Directory entries batched from `bucketDirIndex` (O(1) lookup by path hash)
- **Blob Streaming**: Files streamed in 64KB chunks; no size limit
- **Prefetching**: Nodes track access patterns and prefetch predicted files via fetchers
- **Search Indexing**: Files >1MB truncated for indexing (full content still accessible)

---

## Known Limitations

- **Single-Writer Sessions**: Only one write session active per mount at a time
- **No Authentication**: Current version lacks user authentication (roadmap item)
- **Commit Required**: Write changes local until explicitly committed (not auto-sync)
- **Search Index Limit**: Files >1MB truncated for search indexing
- **Concurrent Writes**: Multiple clients can mount, but write sessions are local to each client

---

## Additional Documentation

- `README.md`: Quick start and feature overview
- `docs/ARCHITECTURE.md`: Detailed system architecture diagrams
- `docs/QUICKSTART.md`: 5-minute getting started guide
- `docs/DEPLOYMENT.md`: Production deployment guide
- `docs/BUILD_INTEGRATION.md`: Using MonoFS with Go/npm/Maven/Bazel
- `docs/ADDING_INGESTION_TYPES.md`: Guide for adding new package manager support
