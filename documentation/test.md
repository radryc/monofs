# Test Package Documentation

Package `test` provides integration tests for MonoFS. This document covers all exported and unexported functions across all test files.

---

## File: test/client_integration_test.go

Tests for the sharded client, covering connectivity, lookups, attribute retrieval, directory listing, concurrent operations, reconnection, multi-repository access, file reading, and routing.

### Struct: `clientTestCluster`

```go
type clientTestCluster struct
```

Encapsulates a complete cluster for client testing. Fields:
- `t *testing.T` — the test instance
- `baseDir string` — temporary base directory
- `nodes []*nodeInfo` — backend node info
- `router *router.Router` — the router instance
- `routerGRPC *grpc.Server` — router gRPC server
- `routerLis net.Listener` — router listener
- `routerPort int` — router port (also serves as base port)
- `stopOnce sync.Once` — ensures cleanup runs once

### Struct: `nodeInfo`

```go
type nodeInfo struct
```

Holds info about a single backend node. Fields: `id`, `port`, `server *server.Server`, `grpcServer *grpc.Server`, `listener net.Listener`, `client pb.MonoFSClient`, `conn *grpc.ClientConn`.

### `newClientTestCluster`

```go
func newClientTestCluster(t *testing.T, numNodes int, basePort int) *clientTestCluster
```

Creates a fully functional cluster with `numNodes` backend server nodes, a router at `basePort`, and health checks enabled. Each node gets its own DB and git cache directories, a gRPC server, and a client connection. The router is configured with `300ms` health check intervals and `1s` unhealthy threshold. Returns the cluster after a `500ms` ready delay. Fails the test (`t.Fatalf`) if any node or router fails to start.

**Called from:** All test functions in this file that need a cluster.

**Key parameters:**
- `t` — test instance (used for `t.Helper()` and `t.Fatalf`)
- `numNodes` — number of backend MonoFS server nodes to create
- `basePort` — port for the router; nodes get `basePort+1` through `basePort+numNodes`

### `clientTestCluster.cleanup`

```go
func (c *clientTestCluster) cleanup()
```

Stops and closes all cluster resources: router gRPC server, router listener, router itself, then each node's gRPC connection, gRPC server, listener, and server. Called by `Close()` via `sync.Once`.

### `clientTestCluster.Close`

```go
func (c *clientTestCluster) Close()
```

Thread-safe shutdown of the cluster. Delegates to `cleanup()` through `sync.Once`.

### `clientTestCluster.ingestTestData`

```go
func (c *clientTestCluster) ingestTestData(ctx context.Context, storageID, displayPath, repoURL string, files []string) error
```

Registers a repository on all nodes, ingests the given file list using `nodes[0]`, and builds directory indexes. Used for test setup to populate data before running client operations.

**Called from:** `TestShardedClientLookup`, `TestShardedClientGetAttr`, `TestShardedClientReadDir`, `TestShardedClientConcurrentOperations`, `TestShardedClientReconnection`, `TestShardedClientMultipleRepositories`, `TestShardedClientRead`

**Key parameters:**
- `storageID` — unique storage identifier
- `displayPath` — virtual filesystem path (e.g., `"github_com/test/repo"`)
- `repoURL` — source repository URL
- `files` — list of file paths relative to repo root

**Returns:** error if registration, ingestion, or index building fails

**Implementation note:** Ingests all files through `nodes[0]` for simplicity; proper sharding is tested separately.

### `TestShardedClientConnection`

```go
func TestShardedClientConnection(t *testing.T)
```

Tests basic `ShardedClient` connectivity. Creates a 3-node cluster (base port 19400), then:
1. **CreateClient** — creates a `ShardedClient` with standard config and verifies it connects
2. **ClientWithExternalAddresses** — creates a `ShardedClient` with `UseExternalAddresses: true` to verify external address mode works

### `TestShardedClientLookup`

```go
func TestShardedClientLookup(t *testing.T)
```

Tests file lookup operations through the sharded client. Creates a 3-node cluster (base port 19410), ingests 4 test files, creates a `ShardedClient`, then:
1. **LookupFiles** — looks up each ingested file and verifies `Found=true`, logs inode number
2. **LookupNonExistent** — looks up a missing file and verifies it's not found

### `TestShardedClientGetAttr`

```go
func TestShardedClientGetAttr(t *testing.T)
```

Tests attribute retrieval through the sharded client. Creates a 3-node cluster (base port 19420), ingests 3 files, creates a client, then verifies `GetAttr` returns correct `Size` and non-zero `Mode` for each file.

### `TestShardedClientReadDir`

```go
func TestShardedClientReadDir(t *testing.T)
```

Tests directory listing through the sharded client. Creates a 3-node cluster (base port 19430), ingests 6 files with nested directory structure, then:
1. **ReadDirRoot** — lists root and verifies expected entries (`README.md`, `go.mod`, `main.go`, `internal`, `pkg`)
2. **ReadDirSubdirectory** — lists `internal/` and verifies `server` and `client` subdirs

### `TestShardedClientConcurrentOperations`

```go
func TestShardedClientConcurrentOperations(t *testing.T)
```

Tests concurrent client operations. Creates a 3-node cluster (base port 19440), ingests 100 files, creates a client, then:
1. **ConcurrentLookups** — 10 workers × 50 lookups each, verifying success count > 50% of total
2. **ConcurrentMixedOperations** — 5 workers × 20 operations each, cycling through `Lookup`, `GetAttr`, and `ReadDir`

Uses `atomic.Int64` for thread-safe counters and `sync.WaitGroup` for coordination.

### `TestShardedClientReconnection`

```go
func TestShardedClientReconnection(t *testing.T)
```

Tests client behavior on connection loss. Skipped in short mode. Creates a 3-node cluster (base port 19450), ingests 3 files, creates a client with `1s` refresh interval, then runs initial operations to verify basic connectivity. Full reconnection testing (stop/restart nodes) is noted as complex for unit tests.

### `TestShardedClientMultipleRepositories`

```go
func TestShardedClientMultipleRepositories(t *testing.T)
```

Tests accessing multiple repositories through the same client. Creates a 3-node cluster (base port 19460), ingests 3 repositories with different display paths and file sets, then verifies all files in all repos are accessible via `Lookup`.

### `TestShardedClientRead`

```go
func TestShardedClientRead(t *testing.T)
```

Tests file reading through the client's `Read` method. Creates a 3-node cluster (base port 19470), ingests 2 files, creates a client. **Read** attempts to read `0..1024` bytes of a file. Since no actual git repo/blob content exists, the test gracefully handles the expected error case.

### `TestClientNodeSelection`

```go
func TestClientNodeSelection(t *testing.T)
```

Tests that the client correctly interacts with files distributed across nodes. Creates a 3-node cluster (base port 19480), ingests different files on each node (5 per node), then verifies that `ReadDir` returns entries from the combined directory listing.

---

## File: test/consistency_test.go

Tests data integrity, persistence, directory index correctness, metadata accuracy, repository isolation, concurrent read/write safety, and cluster data distribution.

### `TestDataPersistence`

```go
func TestDataPersistence(t *testing.T)
```

Verifies data survives server restart. Skipped in short mode. Two phases:
1. **Phase1_IngestData** — creates a standalone server (port 19700), registers repository, ingests 10 files, builds indexes, then cleanly shuts down (stops gRPC, closes listener, closes server, `500ms` delay)
2. **Phase2_VerifyPersistence** — restarts server on a different port (port+1) using the same `dbPath`, verifies all 10 files via `GetAttr`, and checks `ListRepositories` is non-empty

### `TestDirectoryIndexConsistency`

```go
func TestDirectoryIndexConsistency(t *testing.T)
```

Verifies directory index correctness for a complex directory structure. Uses `newServerTestEnv` (port 19710), ingests 16 files across multiple nested directories (`internal/server/`, `internal/client/`, `pkg/utils/`, `pkg/config/`, `cmd/app/`, `cmd/tool/`, `docs/api/`). Tests 6 directory paths against expected entry lists, sorting both expected and actual before comparison.

### `TestMetadataIntegrity`

```go
func TestMetadataIntegrity(t *testing.T)
```

Verifies file metadata is stored and retrieved correctly. Uses `newServerTestEnv` (port 19720), ingests 5 files with specific `Size`, `Mode`, `Mtime`, and `BlobHash` values. Then verifies `GetAttr` returns matching metadata. Compares permission bits only (masked with `0777`).

### `TestRepositoryIsolation`

```go
func TestRepositoryIsolation(t *testing.T)
```

Ensures complete isolation between repositories when they have files with the same names but different sizes. Uses `newServerTestEnv` (port 19730), creates 2 repos with identical filenames (`README.md`, `main.go`, `config.yaml`) but different sizes, verifies each repo's `GetAttr` returns the correct size for that repo.

### `TestConcurrentReadWrite`

```go
func TestConcurrentReadWrite(t *testing.T)
```

Tests consistency under concurrent read/write operations. Skipped in short mode. Uses `newServerTestEnv` (port 19740). Pre-populates 50 files, then runs 10 readers and 5 writers concurrently (20 iterations each). Readers call `GetAttr`, writers call `IngestFile`. Errors are collected in a buffered channel (size 1000) and reported.

### `TestClusterDataDistribution`

```go
func TestClusterDataDistribution(t *testing.T)
```

Verifies data is properly distributed across nodes. Skipped in short mode. Creates 3 independent server nodes (ports 19751-19753), a router with `500ms` health checks, registers repository on all nodes, ingests 10 unique files per node, then verifies each node's `GetRepositoryFiles` returns exactly 10 files.

---

## File: test/dual_addressing_test.go

Tests the router's dual-addressing capability (internal vs external addresses).

### `TestDualAddressing`

```go
func TestDualAddressing(t *testing.T)
```

Integration test requiring a running router at `localhost:9090`. Skipped in short mode. Connects to the router, then:
1. Requests **internal addresses** (`UseExternalAddresses: false`) — verifies no node address starts with `"localhost"` (should be internal hostnames like `node1:9000`)
2. Requests **external addresses** (`UseExternalAddresses: true`) — counts how many addresses start with `"localhost"` (expected for host-based clients)

If the router is unavailable, the test is skipped.

---

## File: test/embedded_kvs_integration_test.go

Tests the MonoFS server integrated with an embedded KVS (Key-Value Store) using Raft, with Guardian-like repository isolation over gRPC.

### Struct: `embeddedKVSServerTestEnv`

```go
type embeddedKVSServerTestEnv struct
```

Encapsulates a test environment with an embedded KVS store. Fields: `t`, `server *server.Server`, `grpcServer`, `listener`, `conn`, `client pb.MonoFSClient`, `kvsClient kvsv1.KVStoreClient`, `baseDir`, `stopOnce`.

### `newEmbeddedKVSServerTestEnv`

```go
func newEmbeddedKVSServerTestEnv(t *testing.T, nodeID string) *embeddedKVSServerTestEnv
```

Creates a test server with an embedded Raft-based KVS store. Steps:
1. Creates temp dirs for `db`, `git`, and `kvs`
2. Allocates a free TCP port via `net.Listen("tcp", "127.0.0.1:0")`
3. Creates a `server.NewServer`
4. Opens a `raftstore` with bootstrap mode and API address matching the listener
5. Calls `srv.SetKVSStore(store)` to inject the KVS
6. Registers both the KVS gRPC service and MonoFS service on the same gRPC server
7. Creates client connections for both `MonoFSClient` and `KVStoreClient`

**Called from:** `TestEmbeddedKVSServerSupportsGuardianIsolationOverGRPC`

### `embeddedKVSServerTestEnv.Close`

```go
func (env *embeddedKVSServerTestEnv) Close()
```

Thread-safe shutdown. Closes connection, stops gRPC server, closes listener, closes server. Uses `sync.Once`.

### `allocateFreeTCPAddr`

```go
func allocateFreeTCPAddr(t *testing.T) string
```

Allocates a free TCP address by listening on `127.0.0.1:0`, reading the assigned address, closing the listener, and returning the address string. Used for the Raft address.

### `ingestKVSBatchEventually`

```go
func ingestKVSBatchEventually(t *testing.T, client pb.MonoFSClient, req *pb.IngestFileBatchRequest) *pb.IngestFileBatchResponse
```

Retries `IngestFileBatch` until it succeeds or a `10s` deadline expires. Retries every `100ms`. Treats "not leader" errors as transient (calls `looksLikeTransientLeaderError`). Fails the test permanently if the error doesn't look transient.

**Called from:** `TestEmbeddedKVSServerSupportsGuardianIsolationOverGRPC`

### `looksLikeTransientLeaderError`

```go
func looksLikeTransientLeaderError(message string) bool
```

Returns `true` if the error message contains `"not leader"`, `"is not the leader"`, or `"leadership lost"` (case-insensitive). Used to determine if a batch ingestion failure is a transient Raft leader election issue.

**Called from:** `ingestKVSBatchEventually`

### `readAllFromMonoFSStream`

```go
func readAllFromMonoFSStream(t *testing.T, client pb.MonoFSClient, fullPath string) []byte
```

Reads an entire file from MonoFS via the streaming `Read` RPC. Opens a stream with a `5s` timeout, accumulates all chunks, returns the combined byte slice. Fails the test on any error.

**Called from:** `TestEmbeddedKVSServerSupportsGuardianIsolationOverGRPC`

### `collectDirEntries`

```go
func collectDirEntries(t *testing.T, client pb.MonoFSClient, path string) map[string]*pb.DirEntry
```

Collects all directory entries from `ReadDir` into a map keyed by entry name. Opens a stream with a `5s` timeout. Fails the test on any error.

**Called from:** `TestEmbeddedKVSServerSupportsGuardianIsolationOverGRPC`

### `TestEmbeddedKVSServerSupportsGuardianIsolationOverGRPC`

```go
func TestEmbeddedKVSServerSupportsGuardianIsolationOverGRPC(t *testing.T)
```

End-to-end test for Guardian-style repository isolation with the embedded KVS backend. Steps:
1. Creates an `embeddedKVSServerTestEnv` with `"embedded-kvs-node"`
2. Registers 3 repos (`guardian/genomics`, `guardian/payments`, `guardian-system`) with `storage_backend: kvs` config
3. Ingests one file per repo via `IngestFileBatch` with inline content and KVS backend metadata
4. Verifies directory structure: `guardian` root contains `genomics` and `payments`, `guardian-system/.queues/local/tasks` contains `task-42.json`
5. Verifies `GetAttr` returns correct file sizes
6. Verifies file content via both MonoFS `Read` stream and KVS `ReadFile` (dual-path verification)
7. Deletes the `genomics` repo and verifies:
   - The file is gone from `GetAttr`
   - The KVS path returns `codes.NotFound`
   - The `guardian` root no longer contains `genomics`
   - `payments` repo is unaffected (both MonoFS and KVS paths)
   - `guardian-system` repo is unaffected

---

## File: test/failover_integration_test.go

Tests end-to-end failover scenarios, node onboarding, multi-node failure recovery, data integrity during failover, and failover cache clearing.

### `TestFailoverE2EScenario`

```go
func TestFailoverE2EScenario(t *testing.T)
```

Tests complete failover workflow. Skipped in short mode. Creates 3 server nodes (ports 19001-19003) via `createTestServer`, a router with `100ms` health checks and `200ms` unhealthy threshold, registers all nodes, verifies 3 healthy nodes, registers a repository on all nodes, ingests 3 files on node1, verifies node1 owns the files. Checks that each node can report its repository files.

### `TestNodeOnboardingE2E`

```go
func TestNodeOnboardingE2E(t *testing.T)
```

Tests a new node joining an existing cluster. Skipped in short mode. Creates 2 initial nodes (ports 19011-19012), registers them with the router, verifies `NodeCount() == 2`, ingests data, then creates a third node (port 19013) and registers it. Verifies `NodeCount() == 3`.

### `TestMultiNodeFailureRecovery`

```go
func TestMultiNodeFailureRecovery(t *testing.T)
```

Tests recovery from multiple node failures. Skipped in short mode. Creates 5 nodes (ports 19020-19024), a router with `200ms` unhealthy threshold, verifies 5 healthy nodes, ingests 10 files on node0, verifies file tracking via `GetRepositoryFiles`.

### `TestFailoverDataIntegrity`

```go
func TestFailoverDataIntegrity(t *testing.T)
```

Tests data remains accessible during failover. Skipped in short mode. Creates a primary (port 19031) and backup (port 19032) node via `createTestServer`, registers the repo on both, ingests 3 files (`README.md`, `src/main.go`, `docs/guide.md`) with specific sizes on the primary, verifies all files via `Lookup` on primary with correct sizes.

### `TestClearFailoverCacheE2E`

```go
func TestClearFailoverCacheE2E(t *testing.T)
```

Tests clearing failover cache after recovery. Skipped in short mode. Creates a backup node (port 19041), registers a repo, ingests 5 files (simulating failover state), then calls `ClearFailoverCache` with a `RecoveredNodeId`. Verifies `Success=true` and `EntriesCleared` is logged.

### `createTestServer`

```go
func createTestServer(t *testing.T, tmpDir, nodeID, address string, logger *slog.Logger) (*server.Server, error)
```

Helper to create test server instances. Creates `db-{nodeID}` and `git-{nodeID}` directories under `tmpDir`, then calls `server.NewServer`.

**Called from:** All test functions in this file that need standalone server instances.

**Key parameters:**
- `tmpDir` — parent temp directory
- `nodeID` — unique node identifier
- `address` — bind address (e.g., `"localhost:19001"`)
- `logger` — structured logger instance

**Returns:** A `*server.Server` and any creation error.

---

## File: test/integration_full_test.go

Contains two skipped tests that were removed due to architectural issues. Both now unconditionally call `t.Skip`.

### `TestFullStackIngestAndAccess`

```go
func TestFullStackIngestAndAccess(t *testing.T)
```

**SKIPPED.** Previously tested full-stack ingestion and access but had fundamental issues: ingested files directly to `nodes[0]`, bypassing router HRW sharding, causing the sharded client to miss files when HRW routed to other nodes. Replaced by `router_integration_test.go`, `server_integration_test.go`, and `client_integration_test.go`.

### `TestNodeFailoverRecovery`

```go
func TestNodeFailoverRecovery(t *testing.T)
```

**SKIPPED.** Previously tested node failover recovery but bypassed proper ingestion workflow and router HRW sharding. Replaced by `internal/router/failover_test.go`, `internal/server/failover_test.go`, `internal/client/failover_test.go`, and `test/failover_integration_test.go`.

---

## File: test/router_integration_test.go

Integration tests for the MonoFS router, covering cluster info, heartbeats, health checks, node registration, HRW routing, multi-node ingestion, failover scenarios, and cluster versioning.

### Struct: `clusterTestEnv`

```go
type clusterTestEnv struct
```

Encapsulates a test cluster with a router and multiple server nodes. Fields:
- `t *testing.T`
- `router *router.Router`
- `routerGRPC *grpc.Server`
- `routerLis net.Listener`
- `routerCli pb.MonoFSRouterClient` — router gRPC client
- `routerConn *grpc.ClientConn`
- `routerPort int` — base port
- `nodes []*serverTestEnv` — uses `serverTestEnv` for each backend node
- `stopOnce sync.Once`

### `newClusterTestEnv`

```go
func newClusterTestEnv(t *testing.T, numNodes int, basePort int) *clusterTestEnv
```

Creates a test cluster with `numNodes` nodes (using `newServerTestEnv`, ports `basePort+1` through `basePort+numNodes`), a router at `basePort` with `500ms` health checks and `2s` unhealthy threshold, and a router gRPC client connection. Registers all nodes with `100` weight. Waits `200ms` for router readiness, then `1s` for node health.

**Called from:** `TestRouterClusterInfo`, `TestRouterHeartbeat`, `TestMultiNodeIngestion`

### `clusterTestEnv.cleanup`

```go
func (env *clusterTestEnv) cleanup()
```

Stops all resources: router connection, router gRPC server, router listener, router itself, then each node via `Close()`. Called by `Close()` through `sync.Once`.

### `clusterTestEnv.Close`

```go
func (env *clusterTestEnv) Close()
```

Thread-safe shutdown; delegates to `cleanup()` via `sync.Once`.

### `TestRouterClusterInfo`

```go
func TestRouterClusterInfo(t *testing.T)
```

Tests cluster topology retrieval. Creates a 3-node cluster (base port 19200), then:
1. **GetClusterInfo** — verifies 3 nodes returned, all healthy, logs version
2. **GetClusterInfoWithExternalAddresses** — verifies 3 nodes with `UseExternalAddresses: true`

### `TestRouterHeartbeat`

```go
func TestRouterHeartbeat(t *testing.T)
```

Tests node heartbeat handling. Creates a 3-node cluster (base port 19210), sends a heartbeat for `"cluster-node-1"`, verifies `Acknowledged=true` and gets `ClusterVersion`.

### `TestRouterHealthChecks`

```go
func TestRouterHealthChecks(t *testing.T)
```

Tests automatic health checking. Skipped in short mode. Creates a standalone server node (port 19220), a router with `200ms` health checks and `500ms` unhealthy threshold, then:
1. **NodeBecomesHealthy** — after `500ms`, verifies `HealthyNodeCount() == 1`
2. **NodeBecomesUnhealthy** — stops the node's gRPC server, after `1s` verifies `HealthyNodeCount() == 0`

### `TestRouterNodeRegistration`

```go
func TestRouterNodeRegistration(t *testing.T)
```

Tests dynamic node registration. Creates a router with `100ms` health checks, registers 5 nodes (ports 19301-19305) with increasing weights (`100*i`). Notes that nodes won't be healthy without actual servers.

### `TestRouterGetNodeForFile`

```go
func TestRouterGetNodeForFile(t *testing.T)
```

Tests HRW routing logic. Creates a router, registers 3 nodes, then:
1. **HRWDistribution** — logs how 6 files would be routed across nodes
2. **ConsistentHashing** — verifies deterministic nature of HRW hashing

### `TestMultiNodeIngestion`

```go
func TestMultiNodeIngestion(t *testing.T)

Tests file ingestion across multiple nodes. Creates a 3-node cluster (base port 19330), registers a repository on all nodes, ingests 10 files per node (different files per node), builds indexes per node, then verifies the total file count across all nodes matches `3 × 10 = 30`.

### `TestNodeFailoverScenario`

```go
func TestNodeFailoverScenario(t *testing.T)
```

Tests failover when a node goes down. Skipped in short mode. Creates 3 independent nodes (ports 19351-19353), a router with `200ms` health checks and `600ms` unhealthy threshold, then:
1. **InitialHealthy** — verifies 3 healthy nodes after `1s`
2. **NodeFailure** — stops node 2's gRPC server, after `1s` verifies `HealthyNodeCount() == 2`

### `TestRouterClusterVersioning`

```go
func TestRouterClusterVersioning(t *testing.T)
```

Tests cluster version increments on node registration changes. Creates a router with its own gRPC server (port 19380) and client. **VersionIncrementsOnNodeAdd** — gets initial version, adds a node, verifies the new version is greater than the initial.

---

## File: test/server_integration_test.go

Integration tests for the MonoFS server components with actual NutsDB storage and gRPC communication.

### Struct: `serverTestEnv`

```go
type serverTestEnv struct
```

Encapsulates a test server environment. Fields: `t`, `server *server.Server`, `grpcServer *grpc.Server`, `listener net.Listener`, `client pb.MonoFSClient`, `conn *grpc.ClientConn`, `baseDir string`, `stopOnce sync.Once`.

**Used by:** All server tests and as building block for `clusterTestEnv` in `router_integration_test.go`, `consistency_test.go`, and `stress_test.go`.

### `newServerTestEnv`

```go
func newServerTestEnv(t *testing.T, nodeID string, port int) *serverTestEnv
```

Creates a new server test environment. Steps:
1. Creates temp dirs for `db` and `git`
2. Creates a `server.NewServer` with `localhost:{port}`
3. Starts a gRPC server serving the MonoFS service
4. Waits `100ms` for readiness
5. Creates a gRPC client connection

**Called from:** Nearly all test functions across the test package that need a standalone server.

**Key parameters:**
- `nodeID` — unique node identifier
- `port` — explicit port number for deterministic addressing

### `serverTestEnv.Close`

```go
func (env *serverTestEnv) Close()
```

Thread-safe shutdown. Closes connection, stops gRPC server, closes listener, closes server. Uses `sync.Once`.

### `TestServerIngestAndLookup`

```go
func TestServerIngestAndLookup(t *testing.T)
```

Tests basic file ingestion and lookup. Uses `newServerTestEnv` (port 19100), registers a repository (`github_com/test/lookup`), ingests 5 files with `IngestFile`, builds directory indexes, then tests:
1. **RegisterRepository** — verifies `Success=true`
2. **IngestFiles** — verifies each file is ingested successfully
3. **BuildDirectoryIndexes** — verifies index building succeeds with correct count
4. **LookupFiles** — extracts parent dir and name from each path, calls `Lookup`, verifies `Found=true` and correct `Size`
5. **GetAttrFiles** — calls `GetAttr` for each file, verifies `Found` and `Size`

### `TestServerReadDir`

```go
func TestServerReadDir(t *testing.T)
```

Tests directory listing operations. Uses `newServerTestEnv` (port 19101), ingests 8 files with a nested directory structure, then:
1. **ReadDirRoot** — lists root, expects `README.md`, `go.mod`, `main.go`, `internal`, `cmd`, `pkg`
2. **ReadDirSubdir** — lists `internal/`, expects `server` and `client`
3. **ReadDirNestedSubdir** — lists `internal/server/`, expects `server.go` and `handler.go`

Streams `ReadDir` responses and accumulates entries into maps.

### `TestServerBatchIngest`

```go
func TestServerBatchIngest(t *testing.T)
```

Tests batch file ingestion. Uses `newServerTestEnv` (port 19102), creates 100 file metadata entries, calls `IngestFileBatch`, verifies:
- `Success=true`
- `FilesIngested == 100`
- A sample of 10 files (every 10th) is retrievable via `GetAttr`

### `TestServerConcurrentOperations`

```go
func TestServerConcurrentOperations(t *testing.T)
```

Tests thread safety of server operations. Uses `newServerTestEnv` (port 19103), 20 workers × 50 files each:
1. **ConcurrentIngest** — workers ingest files concurrently, verifies `1000` successes, reports throughput
2. **ConcurrentLookup** — workers look up all ingested files via `GetAttr`, verifies all `1000` files found

Uses `atomic.Int64` and `sync.WaitGroup`.

### `TestServerMultipleRepositories`

```go
func TestServerMultipleRepositories(t *testing.T)
```

Tests isolation between repositories. Uses `newServerTestEnv` (port 19104), creates 3 repos with different display paths, then:
1. **VerifyIsolation** — each repo's files are accessible via `GetAttr`
2. **VerifyCrossRepoIsolation** — a file from repo1 is not found under repo2's namespace
3. **ListRepositories** — verifies count equals 3

### `TestServerNodeInfo`

```go
func TestServerNodeInfo(t *testing.T)
```

Tests node information retrieval. Uses `newServerTestEnv` (port 19105), calls `GetNodeInfo`, verifies `NodeId` matches and `UptimeSeconds >= 0`.

### `TestServerOnboardingStatus`

```go
func TestServerOnboardingStatus(t *testing.T)
```

Tests onboarding status tracking. Uses `newServerTestEnv` (port 19106), registers a repository, then:
1. **InitialStatus** — verifies repository is not yet onboarded
2. **MarkOnboarded** — marks the repo as onboarded via `MarkRepositoryOnboarded`, verifies `Success=true`
3. **VerifyOnboarded** — calls `GetOnboardingStatus`, verifies the repo status is `true`

### `TestServerFailoverMetadata`

```go
func TestServerFailoverMetadata(t *testing.T)
```

Tests failover metadata operations. Uses `newServerTestEnv` (port 19107), ingests 5 files, then:
1. **GetRepositoryFiles** — verifies 5 files returned
2. **ClearFailoverCache** — clears cache for `"failed-node-test"`, verifies `Success=true`

---

## File: test/stress_test.go

Stress and concurrency tests for MonoFS, covering high concurrency ingestion, batch performance, mixed workloads, cluster concurrent access, and rapid topology changes.

### `TestHighConcurrencyIngestion`

```go
func TestHighConcurrencyIngestion(t *testing.T)
```

Tests server under moderate concurrent load. Skipped in short mode. Uses `newServerTestEnv` (port 19500), 10 workers × 20 files each:
1. **ConcurrentIngestion** — concurrent `IngestFile` calls, verifies success rate ≥ 95%, logs throughput
2. **ConcurrentLookup** — concurrent `GetAttr` lookups after building indexes, reports throughput

Uses `atomic.Int64` for success/error counters.

### `TestBatchIngestionPerformance`

```go
func TestBatchIngestionPerformance(t *testing.T)
```

Tests batch ingestion performance with varying batch sizes. Skipped in short mode. Uses `newServerTestEnv` (port 19510), tests batch sizes `[10, 50, 100]`. Each sub-test creates batch metadata, calls `IngestFileBatch`, verifies success, logs `files/sec`.

### `TestMixedWorkload`

```go
func TestMixedWorkload(t *testing.T)
```

Tests realistic mixed read/write workload (70% reads, 30% writes). Skipped in short mode. Uses `newServerTestEnv` (port 19520), pre-populates 100 files via `IngestFileBatch`, then runs 20 workers × 100 operations each. Verifies error rate ≤ 5%. Reports read count, write count, and throughput.

### `TestClusterConcurrentOperations`

```go
func TestClusterConcurrentOperations(t *testing.T)
```

Tests a full cluster under concurrent load. Skipped in short mode. Creates 3 backend nodes (ports 19551-19553), a router (port 19550) with `500ms` health checks, a sharded client with `2s` refresh interval, ingests 100 files per node, then runs 10 virtual clients × 50 operations each, accessing files across all nodes via the sharded client's `GetAttr`.

### `TestRapidClusterUpdates`

```go
func TestRapidClusterUpdates(t *testing.T)
```

Tests cluster behavior with rapid topology changes. Skipped in short mode. Creates a router with `100ms` health checks, quickly registers 50 nodes (ports 19600-19649), measures and logs registration time.
