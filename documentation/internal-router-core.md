# Router Package — Internal Documentation

Package: `github.com/radryc/monofs/internal/router`

The router package implements the `MonoFSRouter` gRPC service, which manages cluster topology, node health, ingestion orchestration, failover, rebalancing, and workspace sync. This is the central coordination component of the MonoFS distributed filesystem.

---

## File: `router.go`

Core file defining the `Router` struct, its configuration, lifecycle methods, cluster info, heartbeat, file routing, health check, failover, recovery, rebalancing, client management, and guardian authentication.

---

### Types

#### `RouterConfig`

Struct holding all configuration for the Router service. Fields:
- `ClusterID`, `RouterName` — identifiers
- `HealthCheckInterval`, `UnhealthyThreshold` — health check tuning
- `PeerRouters` — list of peer router addresses
- `FetcherAddresses`, `FetcherDiagnostics` — fetcher cluster config
- `SearchDiagnostics`, `ServerDiagnostics`, `RegistryDiagnostics` — diagnostic addresses
- `EncryptionKey` — 32-byte ChaCha20-Poly1305 key for packager archives
- `GuardianStateDir` — persistent state directory
- `WorkspaceStateDir` — workspace job persistence directory
- `SourcePushMode` — "squash" (default) or "preserve"
- `PolicyGateEnabled`, `PolicyConfigPath` — Phase 3 policy controls
- `AuthzEnforceIngest`, `AuthzGrantsPath`, `AuthzGrantsJSON` — partition authz for ingestion
- `AuthzEnforceRead` — read/mount access gating
- `AutoPushEnabled`, `AutoPushInterval` — Phase 3 auto-push
- `ReplicationFactor`, `RebalanceDelay`, `GracefulFailoverDelay`, `GuardianIngestTimeout` — replication/failover

#### `RouterPeer`

```go
type RouterPeer struct {
    Name string
    URL  string
}
```

#### `Router`

Main struct implementing `pb.MonoFSRouterServer`. Contains maps tracking nodes, ingested repos, clients, guardian clients, workspace sync jobs, failover state, policy config, UI channels, cache, and more. Full field list at `router.go:100-224`.

#### `clientState`

Tracks a connected FUSE client with performance metrics.

```go
type clientState struct {
    info            *pb.ClientInfo
    lastHeartbeat   time.Time
    operationsCount int64
    bytesRead       int64
    errorsCount     int64
    mu              sync.RWMutex
}
```

#### `guardianClientState`

Tracks a connected guardian-* client.

```go
type guardianClientState struct {
    clientID      string
    principalID   string
    role          string
    displayName   string
    baseURL       string
    authToken     string
    lastHeartbeat time.Time
}
```

#### `nodeState`

Tracks a backend node's state including gRPC connection, health, KVS status, disk usage, failover info.

```go
type nodeState struct {
    info                     *pb.NodeInfo
    externalAddress          string
    lastSeen                 time.Time
    conn                     *grpc.ClientConn
    client                   pb.MonoFSClient
    kvsStatus                *pb.KVSNodeStatus
    lastHealthCheckError     string
    healthCheckFailureLogged bool
    status                   NodeStatus
    syncProgress             float64
    ownedFilesCount          int64
    diskUsedBytes            int64
    diskTotalBytes           int64
    diskFreeBytes            int64
    backingUpNodes           []string
    onboardRequested         bool
    logEngineStats           *pb.LogEngineStats
}
```

#### `NodeStatus` (enum)

```go
type NodeStatus int
const (
    NodeStaging NodeStatus = iota
    NodeSyncing
    NodeActive
)
```

Has method `String() string` returning "Staging", "Syncing", or "Active".

#### `RebalanceState` (enum)

```go
type RebalanceState int
const (
    RebalanceStateStable      RebalanceState = iota
    RebalanceStateRebalancing
    RebalanceStateDualActive
)
```

Has method `String() string` returning "Stable", "Rebalancing", or "DualActive".

#### `ingestedRepo`

Tracks an ingested repository with topology version and rebalancing state.

```go
type ingestedRepo struct {
    repoID      string
    repoURL     string
    guardianURL string
    branch      string
    filesCount  int64
    ingestedAt  time.Time
    topologyVersion   int64
    targetTopology    int64
    rebalanceState    RebalanceState
    rebalanceProgress float64
    mu                sync.RWMutex
}
```

#### `inProgressIngestion`

Tracks an active ingestion with stage and progress.

```go
type inProgressIngestion struct {
    storageID      string
    repoID         string
    repoURL        string
    branch         string
    startedAt      time.Time
    stage          pb.IngestProgress_Stage
    message        string
    filesProcessed int64
    totalFiles     int64
    mu             sync.RWMutex
}
```

---

### Functions

#### `DefaultRouterConfig() RouterConfig`

- **Signature:** `func DefaultRouterConfig() RouterConfig`
- **What it does:** Returns default `RouterConfig` with sensible defaults (5s health check interval, 15s unhealthy threshold, replication factor 2, etc.).
- **Called from:** Tests, wiring code when constructing a Router with minimal config.
- **Details:** Sets `GuardianIngestTimeout` to 5 minutes for large guardian file batches.

---

#### `trackedRepoRegisterRequest(storageID string, repo *ingestedRepo) *pb.RegisterRepositoryRequest`

- **Signature:** `func trackedRepoRegisterRequest(storageID string, repo *ingestedRepo) *pb.RegisterRepositoryRequest`
- **What it does:** Builds a `RegisterRepositoryRequest` from a tracked ingested repo, copying display path, source URL, guardian URL. Nil `repo` is safe.
- **Called from:** `handleEarlyRecovery`, `recoverNode`, `onboardNewNode` when registering repos on nodes.
- **Details:** Applies `applyGuardianRepoStorageBackend` to set the backend type for guardian partitions.

---

#### `initGrantEvaluator(cfg RouterConfig, logger *slog.Logger) authz.GrantEvaluator`

- **Signature:** `func initGrantEvaluator(cfg RouterConfig, logger *slog.Logger) authz.GrantEvaluator`
- **What it does:** Builds the partition-authorization grant evaluator. Falls back to an empty (deny-by-default) in-memory store on error. Parses inline `AuthzGrantsJSON` grants and merges into the store.
- **Called from:** `NewRouter`.
- **Details:** Defaults grants path to `<GuardianStateDir>/authz_grants.json` when `AuthzGrantsPath` is empty.

---

#### `(r *Router) SetGrantEvaluator(eval authz.GrantEvaluator, enforceIngest bool)`

- **Signature:** `func (r *Router) SetGrantEvaluator(eval authz.GrantEvaluator, enforceIngest bool)`
- **What it does:** Overrides the grant evaluator and enforcement flag. Used in tests and wiring.
- **Called from:** Tests, server wiring code.

---

#### `(r *Router) AddBreakGlassAdmin(clientID string)`

- **Signature:** `func (r *Router) AddBreakGlassAdmin(clientID string)`
- **What it does:** Grants the given principal admin on all partitions (wildcard) for the `MONOFS_TOKEN` break-glass path (authz epic G2). No-op if the evaluator is not a mutable grant store.
- **Called from:** Server bootstrap / auth wiring.

---

#### `NewRouter(cfg RouterConfig, logger *slog.Logger) *Router`

- **Signature:** `func NewRouter(cfg RouterConfig, logger *slog.Logger) *Router`
- **What it does:** Creates and fully initializes a Router service. Sets up guardian principal/version stores, workspace job store (if configured), policy config, grant evaluator, all maps, channels, whitelist store, auto-push worker (if enabled), pipeline orchestrator, and starts the UI handler and client cleanup goroutines.
- **Called from:** Server main / wiring.
- **Details:** Initializes `version` to 1, `namespaceGeneration` to 1. Sets `authzEnforceIngest` and `authzEnforceRead` from config.

---

#### `(r *Router) SetVersion(version, commit, buildTime string)`

- **Signature:** `func (r *Router) SetVersion(version, commit, buildTime string)`
- **What it does:** Stores build version info on the router for diagnostics.
- **Called from:** Server main during startup.

---

#### `(r *Router) markForIndexRebuild(nodeID, storageID string)`

- **Signature:** `func (r *Router) markForIndexRebuild(nodeID, storageID string)`
- **What it does:** Marks a repository on a node for deferred directory index rebuild. Used after rebalancing or recovery.
- **Called from:** `recoverNode`, `rebalanceRepository` (phase 2/3).
- **Details:** Uses `pendingIndexRebuildsMu` for thread safety.

---

#### `(r *Router) triggerIndexRebuild(nodeID, storageID string) error`

- **Signature:** `func (r *Router) triggerIndexRebuild(nodeID, storageID string) error`
- **What it does:** Immediately triggers a `BuildDirectoryIndexes` RPC on the specified node for the given storage ID.
- **Called from:** `recoverNode`, `rebalanceRepository` (phase 5).
- **Details:** 5-minute timeout. Logs the number of directories indexed.

---

#### `(r *Router) SetSearchClient(addr string) error`

- **Signature:** `func (r *Router) SetSearchClient(addr string) error`
- **What it does:** Configures the search service gRPC client. If initial connection fails, starts background retry loop.
- **Called from:** Server wiring.
- **Details:** Uses insecure credentials and 256MB max recv message size. Empty addr is a no-op.

---

#### `(r *Router) retrySearchConnection(addr string)`

- **Signature:** `func (r *Router) retrySearchConnection(addr string)`
- **What it does:** Retries connecting to the search service every 5 seconds in a background goroutine until successful.
- **Called from:** `SetSearchClient` on initial connection failure.

---

#### `(r *Router) getSearchAddress() string`

- **Signature:** `func (r *Router) getSearchAddress() string`
- **What it does:** Returns the configured search service address under read lock.
- **Called from:** UI/diagnostics code.

---

#### `(r *Router) SetFetcherClient(addrs []string) error`

- **Signature:** `func (r *Router) SetFetcherClient(addrs []string) error`
- **What it does:** Configures the fetcher cluster client for monitoring. Stops any existing reconnect loop, creates a new client, and starts a reconnection loop if it fails.
- **Called from:** Server wiring.
- **Details:** Checks for at least one healthy fetcher. Empty addrs clears the client.

---

#### `(r *Router) GetFetcherClusterStats(ctx context.Context, includeSourceStats bool) (*fetcher.ClusterStats, error)`

- **Signature:** `func (r *Router) GetFetcherClusterStats(ctx context.Context, includeSourceStats bool) (*fetcher.ClusterStats, error)`
- **What it does:** Returns cluster-wide statistics from all fetchers via the fetcher client.
- **Called from:** UI/diagnostics.

---

#### `(r *Router) getFetcherClient() *fetcher.Client`

- **Signature:** `func (r *Router) getFetcherClient() *fetcher.Client`
- **What it does:** Returns the current fetcher client under read lock.
- **Called from:** `GetFetcherClusterStats`, `IngestRepository` (archive building), `tryPush`.

---

#### `(r *Router) swapFetcherClient(client *fetcher.Client)`

- **Signature:** `func (r *Router) swapFetcherClient(client *fetcher.Client)`
- **What it does:** Atomically swaps the fetcher client and closes the old one (if different).
- **Called from:** `SetFetcherClient`, fetcher reconnect loop, `Close`.

---

#### `(r *Router) SetRegistryAddr(addr string)`

- **Signature:** `func (r *Router) SetRegistryAddr(addr string)`
- **What it does:** Stores the registry API address for integration.
- **Called from:** Server wiring.

---

#### `(r *Router) startFetcherReconnectLoop(addrs []string)`

- **Signature:** `func (r *Router) startFetcherReconnectLoop(addrs []string)`
- **What it does:** Starts a background goroutine that periodically retries connecting to fetcher addresses. Stops when connection succeeds or is cancelled. Idempotent if already running.
- **Called from:** `SetFetcherClient`.

---

#### `(r *Router) stopFetcherReconnectLoop()`

- **Signature:** `func (r *Router) stopFetcherReconnectLoop()`
- **What it does:** Stops the background fetcher reconnection loop.
- **Called from:** `SetFetcherClient`, `Close`.

---

#### `(r *Router) finishFetcherReconnectLoop(stopCh chan struct{})`

- **Signature:** `func (r *Router) finishFetcherReconnectLoop(stopCh chan struct{})`
- **What it does:** Cleans up the reconnection loop state atomically after a successful connection.
- **Called from:** the fetcher reconnect goroutine.

---

#### `(r *Router) RegisterNode(nodeID, address string, weight uint32) error`

- **Signature:** `func (r *Router) RegisterNode(nodeID, address string, weight uint32) error`
- **What it does:** Adds a backend node to the cluster with health verification. Connects via gRPC, verifies with `GetNodeInfo`, and starts onboarding in the background. New nodes start in `NodeStaging` status.
- **Called from:** gRPC `RegisterNode` handler, admin CLI.

---

#### `(r *Router) RegisterNodeStatic(nodeID, address string, weight uint32)`

- **Signature:** `func (r *Router) RegisterNodeStatic(nodeID, address string, weight uint32)`
- **What it does:** Registers a node without health checking (for initial setup). Delegates to `RegisterNodeWithExternalAddr` with empty external address.
- **Called from:** Cluster initialization code.

---

#### `(r *Router) RegisterNodeWithExternalAddr(nodeID, internalAddr, externalAddr string, weight uint32)`

- **Signature:** `func (r *Router) RegisterNodeWithExternalAddr(nodeID, internalAddr, externalAddr string, weight uint32)`
- **What it does:** Registers a node with separate internal (Docker network) and external (host) addresses. The node is immediately `NodeActive` and increments the cluster version.
- **Called from:** `RegisterNodeStatic`, cluster initialization.

---

#### `(r *Router) UnregisterNode(nodeID string)`

- **Signature:** `func (r *Router) UnregisterNode(nodeID string)`
- **What it does:** Removes a backend node from the cluster, closes its connection, and increments the cluster version.
- **Called from:** gRPC unregister handler, admin CLI.

---

#### `(r *Router) GetClusterInfo(ctx context.Context, req *pb.ClusterInfoRequest) (*pb.ClusterInfoResponse, error)`

- **Signature:** `func (r *Router) GetClusterInfo(ctx context.Context, req *pb.ClusterInfoRequest) (*pb.ClusterInfoResponse, error)`
- **What it does:** Implements `pb.MonoFSRouterServer.GetClusterInfo`. Returns all nodes with their status, KVS metadata, disk usage, and addresses (external vs internal based on request flag).
- **Called from:** gRPC clients, FUSE mount process.

---

#### `(r *Router) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error)`

- **Signature:** `func (r *Router) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error)`
- **What it does:** Implements `pb.MonoFSRouterServer.Heartbeat`. Updates the node's `lastSeen` timestamp. If the node was unhealthy, marks it healthy and increments version for recovery.
- **Called from:** Nodes periodically.

---

#### `(r *Router) GetNodeForFile(ctx context.Context, req *pb.GetNodeForFileRequest) (*pb.GetNodeForFileResponse, error)`

- **Signature:** `func (r *Router) GetNodeForFile(ctx context.Context, req *pb.GetNodeForFileRequest) (*pb.GetNodeForFileResponse, error)`
- **What it does:** Determines which node owns a given file path, handling three rebalancing states (Stable, Rebalancing, DualActive) and failover. Returns primary node ID, fallback node IDs, and cache TTL.
- **Called from:** gRPC clients (FUSE filesystem), client routing layer.
- **Details:** The TTL varies: 300s for stable, 10s for rebalancing, 60s for dual-active, 30s during failover. Uses `getHRWFallbacks` and `getNodeForTopologyVersion` internally.

---

#### `(r *Router) QueryLogs(ctx context.Context, req *pb.QueryLogsRequest) (*pb.QueryLogsResponse, error)`

- **Signature:** `func (r *Router) QueryLogs(ctx context.Context, req *pb.QueryLogsRequest) (*pb.QueryLogsResponse, error)`
- **What it does:** Fans out a log query to all healthy nodes, merges JSON results, and returns them as a single JSON array.
- **Called from:** gRPC query clients.

---

#### `(r *Router) StreamQueryLogs(req *pb.QueryLogsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error`

- **Signature:** `func (r *Router) StreamQueryLogs(req *pb.QueryLogsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error`
- **What it does:** Fans out a log query to all healthy nodes and streams individual results to the client.
- **Called from:** gRPC streaming query clients.

---

#### `(r *Router) QueryMetrics(ctx context.Context, req *pb.QueryMetricsRequest) (*pb.QueryMetricsResponse, error)`

- **Signature:** `func (r *Router) QueryMetrics(ctx context.Context, req *pb.QueryMetricsRequest) (*pb.QueryMetricsResponse, error)`
- **What it does:** Fans out a metrics query to all healthy nodes and returns merged JSON.
- **Called from:** gRPC query clients.

---

#### `(r *Router) StreamQueryMetrics(req *pb.QueryMetricsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error`

- **Signature:** `func (r *Router) StreamQueryMetrics(req *pb.QueryMetricsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error`
- **What it does:** Streams metric query results from all healthy nodes.
- **Called from:** gRPC streaming query clients.

---

#### `(r *Router) QueryTraces(ctx context.Context, req *pb.QueryTracesRequest) (*pb.QueryTracesResponse, error)`

- **Signature:** `func (r *Router) QueryTraces(ctx context.Context, req *pb.QueryTracesRequest) (*pb.QueryTracesResponse, error)`
- **What it does:** Fans out a trace query to all healthy nodes and merges results. Ensures traces distributed across shards are always returned.
- **Called from:** gRPC query clients.

---

#### `(r *Router) StreamQueryTraces(req *pb.QueryTracesRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error`

- **Signature:** `func (r *Router) StreamQueryTraces(req *pb.QueryTracesRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error`
- **What it does:** Streams trace query results from all healthy nodes.
- **Called from:** gRPC streaming query clients.

---

#### `(r *Router) IngestLogs(ctx context.Context, req *pb.IngestLogsRequest) (*pb.IngestLogsResponse, error)`

- **Signature:** `func (r *Router) IngestLogs(ctx context.Context, req *pb.IngestLogsRequest) (*pb.IngestLogsResponse, error)`
- **What it does:** Routes log ingestion to the appropriate node determined by HRW sharding on chunk ID. Uses `telemetryNodeClient` for shard-aware routing.
- **Called from:** Telemetry ingestion clients.

---

#### `(r *Router) IngestMetrics(ctx context.Context, req *pb.IngestMetricsRequest) (*pb.IngestMetricsResponse, error)`

- **Signature:** `func (r *Router) IngestMetrics(ctx context.Context, req *pb.IngestMetricsRequest) (*pb.IngestMetricsResponse, error)`
- **What it does:** Routes metric ingestion to the appropriate node by HRW sharding.
- **Called from:** Telemetry ingestion clients.

---

#### `(r *Router) IngestTraces(ctx context.Context, req *pb.IngestTracesRequest) (*pb.IngestTracesResponse, error)`

- **Signature:** `func (r *Router) IngestTraces(ctx context.Context, req *pb.IngestTracesRequest) (*pb.IngestTracesResponse, error)`
- **What it does:** Routes trace ingestion to the appropriate node by HRW sharding.
- **Called from:** Telemetry ingestion clients.

---

#### `(r *Router) fanoutJSONQueryStream(ctx context.Context, limit int, mergeErrPrefix string, query func(...)) ([]byte, error)`

- **Signature:** `func (r *Router) fanoutJSONQueryStream(ctx context.Context, limit int, mergeErrPrefix string, query func(context.Context, pb.MonoFSClient) (grpc.ServerStreamingClient[pb.QueryResultItem], error)) ([]byte, error)`
- **What it does:** Fans out a streaming query to all healthy nodes, collects results, and concatenates them into a single JSON array (`[...]`). Validates each item as valid JSON.
- **Called from:** `QueryLogs`, `QueryMetrics`, `QueryTraces`.

---

#### `(r *Router) fanoutQueryItems(ctx context.Context, limit int, mergeErrPrefix string, query func(...), handle func([]byte) error) error`

- **Signature:** `func (r *Router) fanoutQueryItems(ctx context.Context, limit int, mergeErrPrefix string, query func(context.Context, pb.MonoFSClient) (grpc.ServerStreamingClient[pb.QueryResultItem], error), handle func([]byte) error) error`
- **What it does:** Generic fan-out for streaming queries. Launches goroutines per node, streams results via a channel, applies the limit, and passes each valid JSON item to the `handle` callback. Cancels context when limit is reached.
- **Called from:** All `StreamQuery*` methods, `fanoutJSONQueryStream`.

---

#### `streamRepositoryFiles(ctx context.Context, client pb.MonoFSClient, storageID string) ([]string, error)`

- **Signature:** `func streamRepositoryFiles(ctx context.Context, client pb.MonoFSClient, storageID string) ([]string, error)`
- **What it does:** Streams all file paths for a repository from a node via `StreamRepositoryFiles` RPC.
- **Called from:** `syncFailoverCache`, `recoverNode`, `onboardNewNode`, `rebalanceRepository`.

---

#### `telemetryShardKey(signal, chunkID string) string`

- **Signature:** `func telemetryShardKey(signal, chunkID string) string`
- **What it does:** Constructs a shard key for telemetry data routing. Uses format: `telemetry:tenant=default:signal=<signal>:chunk=<chunkID>`.
- **Called from:** `telemetryNodeID`.

---

#### `(r *Router) telemetryNodeID(signal, chunkID string) (string, error)`

- **Signature:** `func (r *Router) telemetryNodeID(signal, chunkID string) (string, error)`
- **What it does:** Determines the target node ID for telemetry data using HRW sharding on the shard key.
- **Called from:** `telemetryNodeClient`.

---

#### `(r *Router) telemetryNodeClient(signal, chunkID string) (pb.MonoFSClient, error)`

- **Signature:** `func (r *Router) telemetryNodeClient(signal, chunkID string) (pb.MonoFSClient, error)`
- **What it does:** Returns the gRPC client for the node that should handle a telemetry signal/chunk.
- **Called from:** `IngestLogs`, `IngestMetrics`, `IngestTraces`.

---

#### `(r *Router) anyHealthyNodeClient() (pb.MonoFSClient, error)`

- **Signature:** `func (r *Router) anyHealthyNodeClient() (pb.MonoFSClient, error)`
- **What it does:** Returns the gRPC client for any healthy active node.
- **Called from:** Various admin operations.

---

#### `(r *Router) allHealthyNodeClients() []pb.MonoFSClient`

- **Signature:** `func (r *Router) allHealthyNodeClients() []pb.MonoFSClient`
- **What it does:** Returns gRPC clients for all healthy active nodes.
- **Called from:** `fanoutQueryItems`, `fanoutJSONQueryStream`.

---

#### `(r *Router) getHRWFallbacks(key string, excludeNodeID string, count int) []string`

- **Signature:** `func (r *Router) getHRWFallbacks(key string, excludeNodeID string, count int) []string`
- **What it does:** Returns fallback node IDs in HRW order, excluding a specified node. Must be called with `r.mu` held.
- **Called from:** `GetNodeForFile` (the `checkNodeWithFailover` closure).
- **Details:** Requests `count+1` nodes from HRW to account for the excluded node.

---

#### `(r *Router) getNodeForTopologyVersion(key string, version int64) string`

- **Signature:** `func (r *Router) getNodeForTopologyVersion(key string, version int64) string`
- **What it does:** Calculates the owning node for a key using a specific topology version snapshot. Falls back to current topology if no snapshot exists. Must be called with `r.mu` held for reading.
- **Called from:** `GetNodeForFile`.
- **Details:** Uses `topologySnapshots` map for consistent routing during rebalancing.

---

#### `(r *Router) StartHealthCheck()`

- **Signature:** `func (r *Router) StartHealthCheck()`
- **What it does:** Launches the background health check loop and a deferred repository discovery goroutine (runs after one health check interval).
- **Called from:** Server main after nodes are configured.

---

#### `(r *Router) discoverClusterRepositories()`

- **Signature:** `func (r *Router) discoverClusterRepositories()`
- **What it does:** Queries all connected nodes to discover existing repositories, building the router's view of cluster state on startup. Enables recovery/rebalancing after restart.
- **Called from:** `StartHealthCheck` (deferred goroutine).
- **Details:** Runs in parallel across nodes with 10s timeout. Does not overwrite already-tracked repos. Bumps namespace generation if new repos are found.

---

#### `(r *Router) StopHealthCheck()`

- **Signature:** `func (r *Router) StopHealthCheck()`
- **What it does:** Closes the `stopHealth` channel, terminating the health check loop.
- **Called from:** `Close`.

---

#### `(r *Router) healthCheckLoop()`

- **Signature:** `func (r *Router) healthCheckLoop()`
- **What it does:** Periodically calls `checkAllNodes()` at the configured interval.
- **Called from:** `StartHealthCheck` (as goroutine).

---

#### `(r *Router) checkAllNodes()`

- **Signature:** `func (r *Router) checkAllNodes()`
- **What it does:** Iterates all registered nodes, checking health via `GetNodeInfo` RPC. Handles:
  - **Unhealthy detection:** If `lastSeen` exceeds threshold, marks unhealthy, closes connection, assigns failover node (if not in drain mode).
  - **Connection establishment:** Creates gRPC connections for statically registered nodes.
  - **Active health check:** Calls `GetNodeInfo` with 500ms timeout, updates file counts, disk usage, KVS status, log engine stats.
  - **Recovery detection:** When a previously unhealthy node becomes healthy, cancels pending failover timers, clears failover mappings, and triggers early recovery.
  - **Onboarding check:** Verifies nodes are onboarded and triggers `checkAndRecoverNode` if needed.
- **Called from:** `healthCheckLoop`.

---

#### `(r *Router) evalPolicy(req *workspacepolicy.EvaluationRequest) (*workspacepolicy.EvaluationResult, error)`

- **Signature:** `func (r *Router) evalPolicy(req *workspacepolicy.EvaluationRequest) (*workspacepolicy.EvaluationResult, error)`
- **What it does:** Evaluates a workspace policy. If policy gate is disabled, allows by default. If config is nil with gate enabled, denies. Otherwise delegates to `workspacepolicy.Evaluate`.
- **Called from:** `tryPush` (auto-push), workspace sync handler.

---

#### `(r *Router) Close() error`

- **Signature:** `func (r *Router) Close() error`
- **What it does:** Gracefully shuts down the router: stops health check, UI handler, fetcher reconnect loop, guardian version store, auto-push worker, workspace job store, search connection, and all node connections.
- **Called from:** Server shutdown.

---

#### `(r *Router) NodeCount() int`

- **Signature:** `func (r *Router) NodeCount() int`
- **What it does:** Returns the number of registered nodes.
- **Called from:** UI/diagnostics.

---

#### `(r *Router) HealthyNodeCount() int`

- **Signature:** `func (r *Router) HealthyNodeCount() int`
- **What it does:** Returns the count of healthy nodes.
- **Called from:** UI/diagnostics.

---

#### `(r *Router) assignFailoverNode(failedNodeID string)`

- **Signature:** `func (r *Router) assignFailoverNode(failedNodeID string)`
- **What it does:** Public wrapper for `assignFailoverNodeLocked` that acquires the write lock.
- **Called from:** External code, tests.

---

#### `(r *Router) assignFailoverNodeLocked(failedNodeID string)`

- **Signature:** `func (r *Router) assignFailoverNodeLocked(failedNodeID string)`
- **What it does:** Selects the healthiest active node with the fewest backups to cover a failed node. Stores the failover mapping, records the failover start time, and starts a delayed rebalance timer. Asynchronously triggers `syncFailoverCache`.
- **Called from:** `checkAllNodes` (health check), `assignFailoverNode`.
- **Details:** The backup node already has replica data from ingestion, so failover is instant. The rebalance timer fires after `RebalanceDelay` (default 10 min) to permanently redistribute data.

---

#### `(r *Router) syncFailoverCache(failedNodeID, backupNodeID string, backupClient pb.MonoFSClient, allRepos []string, activeNodes []sharding.Node, sourceClients map[string]pb.MonoFSClient)`

- **Signature:** as above
- **What it does:** Builds an HRW sharder including the failed node, then for each repository queries healthy source nodes for file lists, determines which files were owned by the failed node, and calls `SyncMetadataFromNode` on the backup to populate its failover cache.
- **Called from:** `assignFailoverNodeLocked` (as goroutine).

---

#### `(r *Router) triggerDelayedRebalance(failedNodeID string)`

- **Signature:** `func (r *Router) triggerDelayedRebalance(failedNodeID string)`
- **What it does:** Called when the rebalance timer fires after `RebalanceDelay`. Triggers permanent rebalancing for all repositories by launching `rebalanceRepository` goroutines.
- **Called from:** `time.AfterFunc` callback in `assignFailoverNodeLocked`.

---

#### `(r *Router) cancelFailoverTimer(nodeID string) bool`

- **Signature:** `func (r *Router) cancelFailoverTimer(nodeID string) bool`
- **What it does:** Cancels the pending rebalance timer for a recovering node. Returns true if a timer was found and stopped.
- **Called from:** `checkAllNodes` when a node becomes healthy again.

---

#### `(r *Router) getReposIngestedDuringOutage(nodeID string) []string`

- **Signature:** `func (r *Router) getReposIngestedDuringOutage(nodeID string) []string`
- **What it does:** Returns the list of repository storage IDs that were ingested while a node was down.
- **Called from:** `handleEarlyRecovery`.

---

#### `(r *Router) clearFailoverState(nodeID string)`

- **Signature:** `func (r *Router) clearFailoverState(nodeID string)`
- **What it does:** Cleans up all failover tracking for a recovered node: removes failover mapping, clears start times, stops timers, and notifies backup nodes to clear their failover cache via `ClearFailoverCache` RPC.
- **Called from:** `handleEarlyRecovery`.

---

#### `(r *Router) handleEarlyRecovery(nodeID string)`

- **Signature:** `func (r *Router) handleEarlyRecovery(nodeID string)`
- **What it does:** Handles a node that recovered before the rebalance timer fired. For repos ingested during the outage, registers them on the returning node and triggers `checkAndRecoverNode` to sync files.
- **Called from:** `checkAllNodes` when a node transitions from unhealthy to healthy while in `NodeActive` state.

---

#### `(r *Router) checkAndRecoverNode(nodeID string, state *nodeState)`

- **Signature:** `func (r *Router) checkAndRecoverNode(nodeID string, state *nodeState)`
- **What it does:** Verifies a node's onboarding status by querying `GetOnboardingStatus` and comparing against the router's tracked repos. If repos are missing or incomplete, marks the node as syncing and launches `recoverNode`.
- **Called from:** `checkAllNodes`, `handleEarlyRecovery`, `triggerRebalanceOnRecovery`.

---

#### `(r *Router) recoverNode(nodeID string, missingRepos, incompleteRepos []string)`

- **Signature:** `func (r *Router) recoverNode(nodeID string, missingRepos, incompleteRepos []string)`
- **What it does:** Primary recovery mechanism for nodes with missing data. For each missing/incomplete repo: (1) registers the repo on the target node, (2) builds HRW sharder, (3) collects files from source nodes, (4) determines which files belong to the recovering node, (5) syncs files via `SyncMetadataFromNode`, (6) marks repo as onboarded, (7) marks for index rebuild. Finally marks node as `NodeActive`, increments version, and triggers rebalancing for all repos.
- **Called from:** `checkAndRecoverNode`.

---

#### `(r *Router) onboardNewNode(nodeID string)`

- **Signature:** `func (r *Router) onboardNewNode(nodeID string)`
- **What it does:** Brings a new node into active rotation. Uses majority consensus (>= 50% of existing nodes) to determine which repositories exist. For each consensus repo, registers it on the new node, then redistributes files using HRW sharding with the new topology, syncing files from source nodes. Finally marks the node as `NodeActive`, increments version, and triggers full rebalancing.
- **Called from:** `RegisterNode` (as goroutine).

---

#### `(r *Router) triggerRebalanceOnRecovery(nodeID string)`

- **Signature:** `func (r *Router) triggerRebalanceOnRecovery(nodeID string)`
- **What it does:** After a 2-second settle delay, checks if the node still needs onboarding and triggers rebalancing for all repositories.
- **Called from:** `checkAllNodes` recovery path.

---

#### `(r *Router) rebalanceRepository(storageID string)`

- **Signature:** `func (r *Router) rebalanceRepository(storageID string)`
- **What it does:** Atomically redistributes files for a repository across all active nodes. Phases:
  1. Collect all file paths from all nodes.
  2. Identify files that need to move (HRW mismatch between current and target topology).
  3. Copy files to new locations via `SyncMetadataFromNode` (dual-active — both old and new locations are valid).
  4. 30-second grace period for clients to refresh routing.
  5. Atomic switchover: update `topologyVersion`.
  6. Async cleanup of old file locations after a 5-minute grace period.
- **Called from:** `recoverNode`, `onboardNewNode`, `triggerDelayedRebalance`, `triggerRebalanceOnRecovery`, `IngestRepository` (re-ingestion), `checkAllNodes`.

---

#### `(r *Router) cleanupOldFileLocations(storageID string, filesToMove map[string]struct{from, to string})`

- **Signature:** as above
- **What it does:** After a 5-minute grace period (ensuring all clients have refreshed routing), deletes files from their old locations on source nodes. Best-effort: failures are logged but don't fail rebalancing.
- **Called from:** `rebalanceRepository` (as goroutine, phase 6).

---

#### `(r *Router) RegisterClient(ctx context.Context, req *pb.RegisterClientRequest) (*pb.RegisterClientResponse, error)`

- **Signature:** `func (r *Router) RegisterClient(ctx context.Context, req *pb.RegisterClientRequest) (*pb.RegisterClientResponse, error)`
- **What it does:** Registers a FUSE client. Handles guardian clients separately (storing auth tokens, principal IDs, roles, base URLs). Supports reconnection by updating existing client state. Persists guardian principals.
- **Called from:** gRPC `RegisterClient` handler.

---

#### `(r *Router) UnregisterClient(ctx context.Context, req *pb.UnregisterClientRequest) (*pb.UnregisterClientResponse, error)`

- **Signature:** `func (r *Router) UnregisterClient(ctx context.Context, req *pb.UnregisterClientRequest) (*pb.UnregisterClientResponse, error)`
- **What it does:** Removes a FUSE client and logs session statistics. Also removes from guardian clients if applicable.
- **Called from:** gRPC `UnregisterClient` handler.

---

#### `(r *Router) ClientHeartbeat(ctx context.Context, req *pb.ClientHeartbeatRequest) (*pb.ClientHeartbeatResponse, error)`

- **Signature:** `func (r *Router) ClientHeartbeat(ctx context.Context, req *pb.ClientHeartbeatRequest) (*pb.ClientHeartbeatResponse, error)`
- **What it does:** Updates client metrics (operations count, bytes read, errors), heartbeat timestamp, and guardian client heartbeat if applicable. If client is not registered, instructs it to re-register.
- **Called from:** gRPC `ClientHeartbeat` handler (FUSE clients heartbeat periodically).

---

#### `(r *Router) ListClients(ctx context.Context, req *pb.ListClientsRequest) (*pb.ListClientsResponse, error)`

- **Signature:** `func (r *Router) ListClients(ctx context.Context, req *pb.ListClientsRequest) (*pb.ListClientsResponse, error)`
- **What it does:** Returns snapshot of all connected clients with their states. Marks clients stale if no heartbeat for >60s.
- **Called from:** gRPC `ListClients` handler, UI dashboard.

---

#### `(r *Router) cleanupStaleClients()`

- **Signature:** `func (r *Router) cleanupStaleClients()`
- **What it does:** Periodic goroutine (every 15s) that marks clients stale after 60s and removes them after 5 minutes of no heartbeat. Also cleans up guardian clients.
- **Called from:** `NewRouter` (as goroutine).

---

#### `(r *Router) cleanupStaleClientsOnce(staleThreshold, removeThreshold time.Duration)`

- **Signature:** `func (r *Router) cleanupStaleClientsOnce(staleThreshold, removeThreshold time.Duration)`
- **What it does:** Single iteration of stale client cleanup.
- **Called from:** `cleanupStaleClients`.

---

#### `(r *Router) GetClientCount() int`

- **Signature:** `func (r *Router) GetClientCount() int`
- **What it does:** Returns the number of connected clients.
- **Called from:** Dashboard UI.

---

#### `(r *Router) isGuardianVisible() bool`

- **Signature:** `func (r *Router) isGuardianVisible() bool`
- **What it does:** Reports whether Guardian namespaces should be exposed to clients. Always returns true (visibility is no longer gated on connected guardian UI clients).
- **Called from:** `GetClusterInfo`.

---

#### `(r *Router) validateGuardianToken(token string) (string, bool)`

- **Signature:** `func (r *Router) validateGuardianToken(token string) (string, bool)`
- **What it does:** Validates a guardian auth token against persistent principals and connected clients. Returns the matching principal ID.
- **Called from:** `IngestRepository` (guardian ingestion validation).

---

#### `(r *Router) authenticateGuardianToken(token string) (*guardianPrincipal, bool)`

- **Signature:** `func (r *Router) authenticateGuardianToken(token string) (*guardianPrincipal, bool)`
- **What it does:** Authenticates a guardian token. First checks persistent store (`guardianPrincipals`), then falls back to connected guardian clients.
- **Called from:** `validateGuardianToken`, `authenticateGuardianMutation`.

---

#### `(r *Router) authenticateGuardianMutation(token string, mutationCtx *pb.GuardianMutationContext) (*guardianPrincipal, bool)`

- **Signature:** `func (r *Router) authenticateGuardianMutation(token string, mutationCtx *pb.GuardianMutationContext) (*guardianPrincipal, bool)`
- **What it does:** Authenticates a guardian mutation request. If `mutationCtx` specifies a `PrincipalId`, validates that the token belongs to that specific principal. Otherwise falls back to basic token authentication.
- **Called from:** Guardian mutation RPC handlers (upsert, delete).

---

#### `(r *Router) getGuardianBaseURL(clientID string) string`

- **Signature:** `func (r *Router) getGuardianBaseURL(clientID string) string`
- **What it does:** Returns the base URL for a guardian client by ID.
- **Called from:** `IngestRepository` (guardian ingestion).

---

#### `(r *Router) isGuardianRepo(displayPath string) bool`

- **Signature:** `func (r *Router) isGuardianRepo(displayPath string) bool`
- **What it does:** Returns true if the display path is a guardian partition (starts with `guardian/` or is `guardian-system`).
- **Called from:** `DeleteRepository`, `DeleteGuardianFile`, `DeleteGuardianDirectory`.

---

#### `(r *Router) GetClientStats() (total, connected, stale int, totalOps, totalBytes int64)`

- **Signature:** `func (r *Router) GetClientStats() (total int, connected int, stale int, totalOps int64, totalBytes int64)`
- **What it does:** Returns aggregated client statistics for the performance page.
- **Called from:** UI/diagnostics.

---

#### `(r *Router) RequestFailover(ctx context.Context, req *pb.FailoverRequest) (*pb.FailoverResponse, error)`

- **Signature:** `func (r *Router) RequestFailover(ctx context.Context, req *pb.FailoverRequest) (*pb.FailoverResponse, error)`
- **What it does:** Handles graceful node shutdown by selecting the least-loaded healthy node as failover target, marking the source node unhealthy, storing the failover mapping, and starting a rebalance timer with the shorter `GracefulFailoverDelay` (default 60s).
- **Called from:** gRPC `RequestFailover` handler, nodes during graceful shutdown.
- **Details:** Ignores the request if the cluster is in drain mode.

---

## File: `metrics.go`

Defines Prometheus metrics for the router component.

---

### Variables (package-level metrics)

| Variable | Type | Description |
|---|---|---|
| `routerGuardianUpsertBatchesTotal` | `Counter` | Total UpsertGuardianPaths RPC batches processed |
| `routerGuardianUpsertFilesTotal` | `Counter` | Individual file upserts forwarded to nodes |
| `routerGuardianUpsertBytesTotal` | `Counter` | Raw bytes of upserted file content |
| `routerGuardianDeleteBatchesTotal` | `Counter` | Total DeleteGuardianPaths RPC batches |
| `routerGuardianDeleteFilesTotal` | `Counter` | Individual file deletes processed |
| `routerIngestRepositoriesTotal` | `Counter` | Total IngestRepository RPC calls |
| `routerIngestFilesTotal` | `Counter` | Files ingested through the router |
| `routerIngestBytesTotal` | `Counter` | Bytes of file content ingested |
| `routerNativeReadOpsTotal` | `Counter` | NativeRead (FUSE-path) read operations |
| `routerNativeReadBytesTotal` | `Counter` | Bytes returned via NativeRead |
| `routerGuardianVersionStoreWriteBytesTotal` | `Counter` | Bytes written to guardian_versions.json |
| `routerGuardianVersionStoreFileBytes` | `Gauge` | Current on-disk size of guardian_versions.json |
| `routerWorkspaceSyncJobsTotal` | `CounterVec` | Workspace sync jobs by action and result |
| `routerWorkspaceSyncActiveJobs` | `GaugeVec` | Currently active sync jobs by action |
| `routerWorkspaceSyncDurationSeconds` | `HistogramVec` | Duration of sync jobs by action/result |
| `routerWorkspaceSyncRepositoriesTotal` | `CounterVec` | Repository outcomes during sync |
| `routerWorkspaceSyncBundleBytesTotal` | `Counter` | Bytes received through bundle uploads |
| `routerWorkspaceSyncConflictsTotal` | `CounterVec` | Sync conflicts by action/reason |
| `routerWorkspaceSyncReingestTotal` | `CounterVec` | Re-ingest attempts triggered by refresh |
| `routerClusterNodes` | `GaugeVec` | Node counts by state |
| `routerClusterFilesTotal` | `Gauge` | Total files attributed to known nodes |
| `routerClusterFailoversTotal` | `Gauge` | Active failover mappings |
| `routerClusterDiskBytes` | `GaugeVec` | Aggregated disk bytes by kind |
| `routerAuthOutcomesTotal` | `CounterVec` | Auth outcomes by result and protocol |

---

### Functions

#### `RecordAuthOutcome(outcome, protocol string)`

- **Signature:** `func RecordAuthOutcome(outcome, protocol string)`
- **What it does:** Records an authentication result in the `routerAuthOutcomesTotal` counter. `outcome` must be one of: `authenticated`, `anonymous`, `rejected`. `protocol` must be `http` or `grpc`.
- **Called from:** HTTP and gRPC authenticator middleware.

---

## File: `stats.go`

Cluster and node statistics RPC handlers.

---

### Functions

#### `(r *Router) GetClusterStats(ctx context.Context, req *pb.ClusterStatsRequest) (*pb.ClusterStatsResponse, error)`

- **Signature:** `func (r *Router) GetClusterStats(ctx context.Context, req *pb.ClusterStatsRequest) (*pb.ClusterStatsResponse, error)`
- **What it does:** Returns cluster-wide statistics: total/healthy/unhealthy/syncing node counts, total repositories, total files, total size, failover mappings, and cluster version.
- **Called from:** gRPC `GetClusterStats` handler, UI dashboard.
- **Details:** A node is considered healthy if it has a non-zero `lastSeen` timestamp.

---

#### `(r *Router) GetNodeStats(ctx context.Context, req *pb.NodeStatsRequest) (*pb.NodeStatsResponse, error)`

- **Signature:** `func (r *Router) GetNodeStats(ctx context.Context, req *pb.NodeStatsRequest) (*pb.NodeStatsResponse, error)`
- **What it does:** Returns per-node statistics including status, health, file count, disk usage, backup nodes, sync progress, heartbeat timestamp, KVS status, and log engine stats.
- **Called from:** gRPC `GetNodeStats` handler, UI dashboard.

---

## File: `ingest.go`

Repository ingestion logic including file distribution, archive building, replication, and node lifecycle integration.

---

### Functions

#### `min(a, b int) int`

- **Signature:** `func min(a, b int) int`
- **What it does:** Returns the smaller of two integers.
- **Called from:** `autoPushWorker.recordFailure` (exponential backoff calculation).

---

#### `normalizeRepoID(repoURL string) string`

- **Signature:** `func normalizeRepoID(repoURL string) string`
- **What it does:** Converts a repository URL to a normalized path format. Strips URL scheme, removes `.git` suffix, and combines host/path. Go modules with `@version` are kept as-is.
- **Called from:** `IngestRepository` (when `SourceId` is empty).

---

#### `reservedManagedDisplayPathConflict(displayPath string, ingestionType pb.IngestionType) error`

- **Signature:** `func reservedManagedDisplayPathConflict(displayPath string, ingestionType pb.IngestionType) error`
- **What it does:** Rejects display paths that conflict with reserved managed namespaces (`guardian`, `guardian-system`, `doctor`). Guardian ingestion type is exempt.
- **Called from:** `IngestRepository`.

---

#### `(r *Router) authorizeIngest(ctx context.Context, req *pb.IngestRequest, displayPath string) error`

- **Signature:** `func (r *Router) authorizeIngest(ctx context.Context, req *pb.IngestRequest, displayPath string) error`
- **What it does:** Enforces partition-scoped ingest authorization. Extracts the identity from context, resolves the partition name, and checks if the principal has permission to ingest. Returns `PermissionDenied` error if denied.
- **Called from:** `IngestRepository`.

---

#### `(r *Router) SetAuthzEnforceIngest(enforce bool)`

- **Signature:** `func (r *Router) SetAuthzEnforceIngest(enforce bool)`
- **What it does:** Toggles partition-scoped ingest enforcement at runtime.
- **Called from:** Admin API, tests.

---

#### `(r *Router) AuthzEnforceIngest() bool`

- **Signature:** `func (r *Router) AuthzEnforceIngest() bool`
- **What it does:** Returns the current ingest enforcement state.
- **Called from:** UI, tests.

---

#### `ingestPartitionForGrant(req *pb.IngestRequest, displayPath string) string`

- **Signature:** `func ingestPartitionForGrant(req *pb.IngestRequest, displayPath string) string`
- **What it does:** Resolves the partition name for authorization. Guardian ingestion uses `SourceId` directly; other ingestion derives the top-level namespace from the display path.
- **Called from:** `authorizeIngest`.

---

#### `(r *Router) IngestRepository(req *pb.IngestRequest, stream pb.MonoFSRouter_IngestRepositoryServer) error`

- **Signature:** `func (r *Router) IngestRepository(req *pb.IngestRequest, stream pb.MonoFSRouter_IngestRepositoryServer) error`
- **What it does:** Primary ingestion RPC with streaming progress. Full pipeline:
  1. **Whitelist check** — denies if whitelist is enabled and client is not listed.
  2. **Validation** — source required; guardian-specific validation (partition name, token).
  3. **Display path resolution** — from `SourceId` or normalized source URL.
  4. **Partition authz check** — via `authorizeIngest`.
  5. **Storage ID generation** — SHA-256 hash of display path.
  6. **Backend initialization** — with keepalive progress updates every 10s.
  7. **Repository registration on all nodes.**
  8. **File distribution** — walks files via backend, groups by HRW sharding with replication factor, sends primary batches (concurrently, max 10 workers, 1000 files/batch), then replica batches (async, best-effort).
  9. **Archive building** — deduplicates files by content hash, builds packager archives (2GB split threshold), streams to fetcher.
  10. **Directory index building** on all active nodes.
  11. **Onboarding marking** on all active nodes.
  12. **Re-ingestion detection** — if the repo already existed, triggers `rebalanceRepository`.
  13. **Search indexing** — async if search client is configured.
  14. **Progress streaming** throughout.
- **Called from:** gRPC `IngestRepository` handler.

---

## File: `delete.go`

Repository, file, and directory deletion logic across the cluster.

---

### Functions

#### `(r *Router) DeleteRepository(ctx context.Context, req *pb.DeleteRepositoryRequest) (*pb.DeleteRepositoryResponse, error)`

- **Signature:** `func (r *Router) DeleteRepository(ctx context.Context, req *pb.DeleteRepositoryRequest) (*pb.DeleteRepositoryResponse, error)`
- **What it does:** Deletes a repository from the entire cluster: removes from router memory, deletes from all backend nodes (or KVS node for guardian), deletes search index, bumps namespace generation, and publishes guardian change event if applicable.
- **Called from:** gRPC `DeleteRepository` handler, HTTP guardian API.

---

#### `(r *Router) deleteRepositoryFromAllNodes(ctx context.Context, storageID string) (int64, int64, int)`

- **Signature:** `func (r *Router) deleteRepositoryFromAllNodes(ctx context.Context, storageID string) (int64, int64, int)`
- **What it does:** Calls `DeleteRepository` on every node in parallel (via goroutines). Returns total files deleted, dirs deleted, and error count.
- **Called from:** `DeleteRepository`, `deleteRepositoryFromNodes`.

---

#### `(r *Router) deleteRepositoryFromNodes(storageID string, filesDeletedPtr *int64)`

- **Signature:** `func (r *Router) deleteRepositoryFromNodes(storageID string, filesDeletedPtr *int64)`
- **What it does:** Compatibility wrapper for `cleanupStalePartialRepos`. Calls `deleteRepositoryFromAllNodes` with a 5-minute timeout and writes the file count to the pointer.
- **Called from:** cleanup logic.

---

#### `(r *Router) deleteRepositoryFromKVSNode(ctx context.Context, storageID, displayPath string) (int64, int64, int, bool)`

- **Signature:** `func (r *Router) deleteRepositoryFromKVSNode(ctx context.Context, storageID, displayPath string) (int64, int64, int, bool)`
- **What it does:** Deletes a KVS-backed guardian repository from the specific KVS mutation target node. Returns `(filesDeleted, dirsDeleted, errors, handled)`.
- **Called from:** `DeleteRepository` (when repo is guardian KVS).

---

#### `(r *Router) deleteSearchIndex(ctx context.Context, storageID string)`

- **Signature:** `func (r *Router) deleteSearchIndex(ctx context.Context, storageID string)`
- **What it does:** Removes the search index for a repository via the search client.
- **Called from:** `DeleteRepository`.

---

#### `(r *Router) deleteRepositoryInternal(ctx context.Context, storageID string) (*pb.DeleteRepositoryResponse, error)`

- **Signature:** `func (r *Router) deleteRepositoryInternal(ctx context.Context, storageID string) (*pb.DeleteRepositoryResponse, error)`
- **What it does:** Convenience wrapper that builds a `DeleteRepositoryRequest` from just a storage ID and calls `DeleteRepository`.
- **Called from:** HTTP guardian API delete endpoint.

---

#### `(r *Router) deleteGuardianFileFromAllNodes(storageID, filePath string) (map[string]interface{}, error)`

- **Signature:** `func (r *Router) deleteGuardianFileFromAllNodes(storageID, filePath string) (map[string]interface{}, error)`
- **What it does:** Deletes a single file from a guardian partition. Tries KVS mutation target first, then falls back to fan-out on all nodes. Returns a map with success counts.
- **Called from:** Guardian file delete HTTP handler.

---

#### `(r *Router) deleteGuardianDirFromAllNodes(storageID, dirPath string) (map[string]interface{}, error)`

- **Signature:** `func (r *Router) deleteGuardianDirFromAllNodes(storageID, dirPath string) (map[string]interface{}, error)`
- **What it does:** Recursively deletes a directory from a guardian partition. Tries KVS mutation target first, then fan-out. Returns file/dir deletion counts.
- **Called from:** Guardian directory delete HTTP handler.

---

#### `(r *Router) DeleteGuardianFile(ctx context.Context, req *pb.DeleteGuardianFileRequest) (*pb.DeleteGuardianFileResponse, error)`

- **Signature:** `func (r *Router) DeleteGuardianFile(ctx context.Context, req *pb.DeleteGuardianFileRequest) (*pb.DeleteGuardianFileResponse, error)`
- **What it does:** gRPC handler for deleting a file from a guardian partition. Validates the storage ID is a guardian repo, converts physical path to logical path, and delegates to `DeleteGuardianPaths`.
- **Called from:** gRPC `DeleteGuardianFile` handler.

---

#### `(r *Router) DeleteGuardianDirectory(ctx context.Context, req *pb.DeleteGuardianDirectoryRequest) (*pb.DeleteGuardianDirectoryResponse, error)`

- **Signature:** `func (r *Router) DeleteGuardianDirectory(ctx context.Context, req *pb.DeleteGuardianDirectoryRequest) (*pb.DeleteGuardianDirectoryResponse, error)`
- **What it does:** gRPC handler for deleting a directory from a guardian partition. Validates, converts to logical path, delegates to `DeleteGuardianPaths`.
- **Called from:** gRPC `DeleteGuardianDirectory` handler.

---

## File: `drain.go`

Cluster drain/undrain functionality for planned maintenance.

---

### Functions

#### `(r *Router) DrainCluster(ctx context.Context, req *pb.DrainClusterRequest) (*pb.DrainClusterResponse, error)`

- **Signature:** `func (r *Router) DrainCluster(ctx context.Context, req *pb.DrainClusterRequest) (*pb.DrainClusterResponse, error)`
- **What it does:** Puts the cluster in maintenance mode, disabling failover via `drainMode` atomic bool. Records the drain start time and reason. Returns error if already drained.
- **Called from:** gRPC `DrainCluster` handler, admin operations.

---

#### `(r *Router) UndrainCluster(ctx context.Context, req *pb.UndrainClusterRequest) (*pb.UndrainClusterResponse, error)`

- **Signature:** `func (r *Router) UndrainCluster(ctx context.Context, req *pb.UndrainClusterRequest) (*pb.UndrainClusterResponse, error)`
- **What it does:** Exits maintenance mode, re-enabling failover. Logs the drain duration and clears the reason.
- **Called from:** gRPC `UndrainCluster` handler.

---

#### `(r *Router) IsDrained() bool`

- **Signature:** `func (r *Router) IsDrained() bool`
- **What it does:** Returns whether the cluster is currently in drain mode.
- **Called from:** `checkAllNodes` (health check, to skip failover assignment), `RequestFailover`.

---

## File: `autopush.go`

Phase 3 auto-push worker for workspace source push operations.

---

### Types

#### `autoPushWorker`

```go
type autoPushWorker struct {
    router       *Router
    logger       *slog.Logger
    interval     time.Duration
    concurrency  int
    stop         chan struct{}
    mu           sync.Mutex
    active       map[string]bool
    failureCount map[string]int
    backoffUntil map[string]time.Time
}
```

### Constants

| Constant | Value | Description |
|---|---|---|
| `defaultAutoPushInterval` | `60s` | Default scan interval |
| `defaultConcurrencyCap` | `20` | Max concurrent push operations |
| `minAutoPushInterval` | `30s` | Minimum allowed interval |
| `maxBackoffInterval` | `30m` | Maximum backoff for failed pushes |
| `deadLetterAfterFailures` | `10` | Max failures before dead-lettering |

---

### Functions

#### `newAutoPushWorker(r *Router, interval time.Duration, concurrency int, logger *slog.Logger) *autoPushWorker`

- **Signature:** `func newAutoPushWorker(r *Router, interval time.Duration, concurrency int, logger *slog.Logger) *autoPushWorker`
- **What it does:** Creates a new auto-push worker, enforcing minimum interval and default concurrency.
- **Called from:** `NewRouter`.

---

#### `(w *autoPushWorker) Start()`

- **Signature:** `func (w *autoPushWorker) Start()`
- **What it does:** Logs startup info and launches the `loop()` goroutine.
- **Called from:** `NewRouter`.

---

#### `(w *autoPushWorker) Stop()`

- **Signature:** `func (w *autoPushWorker) Stop()`
- **What it does:** Closes the stop channel, terminating the loop.
- **Called from:** `Router.Close()`.

---

#### `(w *autoPushWorker) loop()`

- **Signature:** `func (w *autoPushWorker) loop()`
- **What it does:** Main loop that calls `scan()` at every ticker interval.
- **Called from:** `Start` (as goroutine).

---

#### `(w *autoPushWorker) scan()`

- **Signature:** `func (w *autoPushWorker) scan()`
- **What it does:** Gets candidate workspaces, then launches concurrent `tryPush` goroutines bounded by `concurrency`.
- **Called from:** `loop`.

---

#### `(w *autoPushWorker) getCandidates() []string`

- **Signature:** `func (w *autoPushWorker) getCandidates() []string`
- **What it does:** Scans `workspaceSyncJobs` for queued source-push jobs. Skips workspaces that are currently active, in backoff, or dead-lettered (>=10 failures).
- **Called from:** `scan`.

---

#### `(w *autoPushWorker) tryPush(workspaceID string)`

- **Signature:** `func (w *autoPushWorker) tryPush(workspaceID string)`
- **What it does:** Attempts to push a workspace. Steps:
  1. Checks if already active; marks as active.
  2. Looks up the bundle ID and logical branch from the sync job.
  3. Validates the bundle exists.
  4. Evaluates policy gate via `evalPolicy`.
  5. Stages the commit bundle on the fetcher.
  6. Calls `StartWorkspaceCommitPush` on the fetcher.
  7. Records success (resets backoff) or failure (exponential backoff up to 30 min, dead-letter after 10 failures).
- **Called from:** `scan` (per candidate).

---

#### `(w *autoPushWorker) recordFailure(workspaceID string)`

- **Signature:** `func (w *autoPushWorker) recordFailure(workspaceID string)`
- **What it does:** Increments failure count and sets exponential backoff (`2^(count-1)` minutes, capped at 30 min). Logs warning when dead-letter threshold is reached.
- **Called from:** `tryPush`.

---

#### `(w *autoPushWorker) resetBackoff(workspaceID string)`

- **Signature:** `func (w *autoPushWorker) resetBackoff(workspaceID string)`
- **What it does:** Clears failure count and backoff for a workspace after successful push.
- **Called from:** `tryPush`.

---

## File: `subscribe.go`

Guardian change subscription system for reactive file system updates.

---

### Types

#### `guardianChangeSubscriber`

```go
type guardianChangeSubscriber struct {
    id           uint64
    storageID    string
    pathPrefixes []string
    events       chan *pb.ChangeEvent
}
```

Buffered with `guardianChangeBufferSize` (128).

---

### Functions

#### `(r *Router) SubscribeToChanges(req *pb.SubscribeChangesRequest, stream grpc.ServerStreamingServer[pb.ChangeEvent]) error`

- **Signature:** `func (r *Router) SubscribeToChanges(req *pb.SubscribeChangesRequest, stream grpc.ServerStreamingServer[pb.ChangeEvent]) error`
- **What it does:** Registers a server-streaming subscription for guardian file changes. Validates `storageID` and checks the client is a known connected client (`isKnownChangeClient`). Filters events by path prefixes. Streams events until the client disconnects.
- **Called from:** gRPC `SubscribeToChanges` handler.

---

#### `(r *Router) isKnownChangeClient(clientID string) bool`

- **Signature:** `func (r *Router) isKnownChangeClient(clientID string) bool`
- **What it does:** Checks if a client ID belongs to a connected guardian client or regular client.
- **Called from:** `SubscribeToChanges`.

---

#### `(r *Router) publishGuardianChange(event *pb.ChangeEvent)`

- **Signature:** `func (r *Router) publishGuardianChange(event *pb.ChangeEvent)`
- **What it does:** Publishes a change event to all subscribers matching the storage ID and path prefixes. Clones the event via protobuf. Non-blocking: drops events for slow subscribers with a warning log.
- **Called from:** Delete operations, upsert operations (when guardian repos change).

---

#### `normalizeGuardianPrefixes(prefixes []string) []string`

- **Signature:** `func normalizeGuardianPrefixes(prefixes []string) []string`
- **What it does:** Normalizes path prefixes for subscription filtering. Empty or invalid prefixes result in nil (match-all).
- **Called from:** `SubscribeToChanges`.

---

#### `matchesGuardianPrefixes(filePath string, prefixes []string) bool`

- **Signature:** `func matchesGuardianPrefixes(filePath string, prefixes []string) bool`
- **What it does:** Checks if a file path matches any of the normalized prefixes. Returns true if prefixes are nil/empty (match-all). Matches exact, prefix-based, and reverse-prefix patterns.
- **Called from:** `publishGuardianChange`.

---

#### `cloneGuardianChangeEvent(event *pb.ChangeEvent) *pb.ChangeEvent`

- **Signature:** `func cloneGuardianChangeEvent(event *pb.ChangeEvent) *pb.ChangeEvent`
- **What it does:** Deep-copies a `ChangeEvent` using `proto.Clone` so each subscriber gets its own copy.
- **Called from:** `publishGuardianChange`.

---

## File: `kvs_status.go`

KVS (Key-Value Store) node status normalization.

---

### Functions

#### `normalizedKVSNodeStatus(status *pb.KVSNodeStatus) *pb.KVSNodeStatus`

- **Signature:** `func normalizedKVSNodeStatus(status *pb.KVSNodeStatus) *pb.KVSNodeStatus`
- **What it does:** Normalizes a `KVSNodeStatus` by ensuring mode and role are never empty (default to "disabled" for nil or empty status). Returns a new `KVSNodeStatus` with all fields copied.
- **Called from:** `GetClusterInfo`, `GetNodeStats`, `checkAllNodes`.

---

## File: `whitelist.go`

Ingestion whitelist management for controlling which clients can ingest data.

---

### Types

#### `whitelistEntry`

```go
type whitelistEntry struct {
    clientID string
    label    string
    addedAt  time.Time
}
```

#### `whitelistStore`

```go
type whitelistStore struct {
    mu      sync.RWMutex
    entries map[string]*whitelistEntry
    enabled bool
}
```

---

### Functions

#### `newWhitelistStore() *whitelistStore`

- **Signature:** `func newWhitelistStore() *whitelistStore`
- **What it does:** Creates an empty whitelist store with whitelist disabled.
- **Called from:** `NewRouter`.

---

#### `(ws *whitelistStore) IsAllowed(clientID string) bool`

- **Signature:** `func (ws *whitelistStore) IsAllowed(clientID string) bool`
- **What it does:** Returns true if the client is whitelisted or the whitelist is disabled.
- **Called from:** `IngestRepository`.

---

#### `(ws *whitelistStore) Add(clientID, label string)`

- **Signature:** `func (ws *whitelistStore) Add(clientID, label string)`
- **What it does:** Adds a client to the whitelist with a timestamp.
- **Called from:** gRPC `AddWhitelistedClient`, HTTP whitelist API.

---

#### `(ws *whitelistStore) Remove(clientID string) bool`

- **Signature:** `func (ws *whitelistStore) Remove(clientID string) bool`
- **What it does:** Removes a client; returns false if not found.
- **Called from:** gRPC `RemoveWhitelistedClient`, HTTP whitelist API.

---

#### `(ws *whitelistStore) SetEnabled(enabled bool)`

- **Signature:** `func (ws *whitelistStore) SetEnabled(enabled bool)`
- **What it does:** Enables or disables whitelist enforcement.
- **Called from:** gRPC `SetWhitelistEnabled`, HTTP toggle API.

---

#### `(ws *whitelistStore) Enabled() bool`

- **Signature:** `func (ws *whitelistStore) Enabled() bool`
- **What it does:** Returns whether whitelist enforcement is active.
- **Called from:** `IngestRepository`, HTTP whitelist status API.

---

#### `(ws *whitelistStore) List() []*whitelistEntry`

- **Signature:** `func (ws *whitelistStore) List() []*whitelistEntry`
- **What it does:** Returns all whitelisted entries.
- **Called from:** gRPC `ListWhitelistedClients`, `GetWhitelistStatus`, HTTP whitelist API.

---

#### `(r *Router) AddWhitelistedClient(_ context.Context, req *pb.AddWhitelistedClientRequest) (*pb.AddWhitelistedClientResponse, error)`

- **Signature:** as above
- **What it does:** gRPC handler to add a client to the whitelist. Validates `client_id` is non-empty.
- **Called from:** gRPC `AddWhitelistedClient` handler.

---

#### `(r *Router) RemoveWhitelistedClient(_ context.Context, req *pb.RemoveWhitelistedClientRequest) (*pb.RemoveWhitelistedClientResponse, error)`

- **Signature:** as above
- **What it does:** gRPC handler to remove a client. Returns error if client not found.
- **Called from:** gRPC `RemoveWhitelistedClient` handler.

---

#### `(r *Router) ListWhitelistedClients(_ context.Context, _ *pb.ListWhitelistedClientsRequest) (*pb.ListWhitelistedClientsResponse, error)`

- **Signature:** as above
- **What it does:** gRPC handler listing all whitelisted clients with their labels and timestamps.
- **Called from:** gRPC `ListWhitelistedClients` handler.

---

#### `(r *Router) SetWhitelistEnabled(_ context.Context, req *pb.SetWhitelistEnabledRequest) (*pb.SetWhitelistEnabledResponse, error)`

- **Signature:** as above
- **What it does:** gRPC handler to toggle whitelist enforcement.
- **Called from:** gRPC `SetWhitelistEnabled` handler.

---

#### `(r *Router) GetWhitelistStatus(_ context.Context, _ *pb.GetWhitelistStatusRequest) (*pb.GetWhitelistStatusResponse, error)`

- **Signature:** as above
- **What it does:** gRPC handler returning whitelist status (enabled, count, clients).
- **Called from:** gRPC `GetWhitelistStatus` handler.

---

#### `extractClientID(ctx context.Context) string`

- **Signature:** `func extractClientID(ctx context.Context) string`
- **What it does:** Reads the `x-client-id` value from gRPC incoming metadata.
- **Called from:** `IngestRepository` (whitelist enforcement).

---

#### `(r *Router) handleWhitelistAPI(w http.ResponseWriter, req *http.Request)`

- **Signature:** `func (r *Router) handleWhitelistAPI(w http.ResponseWriter, req *http.Request)`
- **What it does:** HTTP API handler for whitelist CRUD: GET lists clients, POST adds a client, DELETE removes a client. Returns JSON responses.
- **Called from:** Router HTTP mux (web UI route).

---

#### `(r *Router) handleWhitelistToggleAPI(w http.ResponseWriter, req *http.Request)`

- **Signature:** `func (r *Router) handleWhitelistToggleAPI(w http.ResponseWriter, req *http.Request)`
- **What it does:** HTTP API handler to enable/disable the whitelist via POST.
- **Called from:** Router HTTP mux (web UI route).

---

## File: `repository_links.go`

Builds product-aware deep-links for the web UI, connecting repositories to their Guardian or Doctor interfaces.

---

### Types

#### `repositoryUIBases`

```go
type repositoryUIBases struct {
    Guardian string
    Doctor   string
}
```

#### `repositoryProductLink`

```go
type repositoryProductLink struct {
    Kind  string
    Label string
    URL   string
}
```

---

### Functions

#### `repositoryProductStoredURL(displayPath, guardianURL, sourceURL string) string`

- **Signature:** `func repositoryProductStoredURL(displayPath, guardianURL, sourceURL string) string`
- **What it does:** Resolves the stored product URL for a repository. Prefers `guardianURL` if set, falls back to `sourceURL` for guardian-kind repos if it's an HTTP URL.
- **Called from:** UI data assembly.

---

#### `(r *Router) repositoryUIBases() repositoryUIBases`

- **Signature:** `func (r *Router) repositoryUIBases() repositoryUIBases`
- **What it does:** Collects base URLs from connected guardian clients (live) and persisted guardian principals for building product links.
- **Called from:** UI data assembly for repository cards.

---

#### `recordRepositoryUIBase(bases *repositoryUIBases, role, rawBase string)`

- **Signature:** `func recordRepositoryUIBase(bases *repositoryUIBases, role, rawBase string)`
- **What it does:** Records the first valid HTTP base URL for a given role (`doctor` or default/guardian). Skips non-HTTP URLs.
- **Called from:** `repositoryUIBases`.

---

#### `buildRepositoryProductLink(displayPath, storedURL string, bases repositoryUIBases) repositoryProductLink`

- **Signature:** `func buildRepositoryProductLink(displayPath, storedURL string, bases repositoryUIBases) repositoryProductLink`
- **What it does:** Builds a product link with kind, label, and URL. For guardian partitions, generates a deep-link with partition query parameter. Falls back to base URL without deep-link if partition name resolution fails.
- **Called from:** UI data assembly.

---

#### `repositoryProductKind(displayPath string) string`

- **Signature:** `func repositoryProductKind(displayPath string) string`
- **What it does:** Classifies a display path as `"guardian"` (starts with `guardian/` or is `guardian-system`), `"doctor"` (starts with `doctor/`), or `""` (other).
- **Called from:** `repositoryProductStoredURL`, `buildRepositoryProductLink`.

---

#### `repositoryProductLabel(kind string) string`

- **Signature:** `func repositoryProductLabel(kind string) string`
- **What it does:** Returns a human-readable label: `"Guardian UI"` or `"Doctor UI"`.
- **Called from:** `buildRepositoryProductLink`.

---

#### `repositoryPartitionName(displayPath string) string`

- **Signature:** `func repositoryPartitionName(displayPath string) string`
- **What it does:** Extracts the partition name from a guardian display path (the segment after `guardian/`).
- **Called from:** `buildRepositoryProductLink`.

---

#### `guardianPartitionDeepLink(rawBase, partition string) string`

- **Signature:** `func guardianPartitionDeepLink(rawBase, partition string) string`
- **What it does:** Constructs a deep-link URL to the Guardian UI for a specific partition. Adds `?partition=<name>` query parameter. If the URL path already ends with the partition name, it strips that segment first.
- **Called from:** `buildRepositoryProductLink`.

---

#### `isHTTPURL(raw string) bool`

- **Signature:** `func isHTTPURL(raw string) bool`
- **What it does:** Validates that a string is an HTTP/HTTPS URL with a non-empty host.
- **Called from:** `repositoryProductStoredURL`, `recordRepositoryUIBase`, `buildRepositoryProductLink`.
