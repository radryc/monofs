# Internal Package Documentation: `cache`, `client`, `diagnostics`

---

## `internal/cache/cache.go`

Package `cache` provides optional metadata caching using NutsDB (an embedded key-value store). It is used by the FUSE layer to avoid redundant gRPC calls for attributes and directory listings.

### Types

#### `AttrEntry`

```go
type AttrEntry struct {
    Ino   uint64 `json:"ino"`
    Mode  uint32 `json:"mode"`
    Size  uint64 `json:"size"`
    Mtime int64  `json:"mtime"`
    Atime int64  `json:"atime"`
    Ctime int64  `json:"ctime"`
    Nlink uint32 `json:"nlink"`
    Uid   uint32 `json:"uid"`
    Gid   uint32 `json:"gid"`
}
```

Represents cached file attributes. Mirrors standard inode stat fields. Stored as JSON in the `attr_cache` NutsDB bucket.

#### `DirEntry`

```go
type DirEntry struct {
    Name string `json:"name"`
    Mode uint32 `json:"mode"`
    Ino  uint64 `json:"ino"`
}
```

Represents a cached directory entry (name + mode + inode). Stored as JSON in the `dir_cache` NutsDB bucket. Converted to `fuse.DirEntry` when returned.

#### `Cache`

```go
type Cache struct {
    db     *nutsdb.DB
    logger *slog.Logger
}
```

Wraps a NutsDB instance for attribute and directory listing caching. Not safe for concurrent use from multiple goroutines without external synchronization (but NutsDB itself is thread-safe).

---

### Constants

| Constant | Value | Description |
|---|---|---|
| `attrBucket` | `"attr_cache"` | NutsDB bucket name for attribute cache |
| `dirBucket` | `"dir_cache"` | NutsDB bucket name for directory cache |
| `DefaultAttrTTL` | `30s` | Default TTL for cached attributes |
| `DefaultDirTTL` | `30s` | Default TTL for cached directory listings |

---

### Functions

#### `New(dir string, logger *slog.Logger) (*Cache, error)`

**What it does**: Creates a new `Cache` instance, opening a NutsDB database at the specified directory. Creates the `attr_cache` and `dir_cache` buckets if they don't exist.

**How it's called**: Called from the FUSE filesystem setup code during mount initialization. Passed the mount-specific cache directory (usually `<mountdir>/.monofs/cache`).

**Parameters**:
- `dir` — filesystem path where the NutsDB database files are stored.
- `logger` — structured logger. If `nil`, defaults to `slog.Default()`.

**Returns**: A `*Cache` instance or an error if the database could not be opened or buckets could not be created.

**Implementation details**:
- Opens NutsDB with 64 MB segments, `HintKeyAndRAMIdxMode` for fast startup (only keys in RAM), and `MMap` mode for fast reads.
- Attempts to create both `attrBucket` and `dirBucket` in a single transaction. Ignores `ErrBucketAlreadyExist` errors.
- On bucket creation failure, closes the database and returns an error.
- Logs `"cache initialized"` at info level on success.

---

#### `(c *Cache) cacheKey(path string) []byte`

**What it does**: Converts a filesystem path to a cache key byte slice. Returns `[]byte("/")` for an empty path (root).

**How it's called**: Internally by all cache read/write methods (`GetAttr`, `PutAttr`, `GetDir`, `PutDir`, `Invalidate`, `InvalidatePrefix`).

**Parameters**:
- `path` — the filesystem path string.

**Returns**: A `[]byte` representation of the path, used as the NutsDB key.

---

#### `(c *Cache) GetAttr(path string) (*AttrEntry, error)`

**What it does**: Retrieves cached file attributes for a given path from the `attr_cache` bucket.

**How it's called**: Called by the FUSE `GetAttr` implementation before making an RPC call to the backend. If a cache hit occurs, the cached entry is returned without a network round-trip.

**Parameters**:
- `path` — the filesystem path to look up.

**Returns**: A pointer to the cached `AttrEntry`, or an error (most commonly `nutsdb.ErrKeyNotFound` if the path is not cached or the TTL has expired).

**Implementation details**: Uses a read-only NutsDB `View` transaction. JSON-unmarshals the raw value into an `AttrEntry`. Logs `"cache hit"` at debug level on success.

---

#### `(c *Cache) PutAttr(path string, entry *AttrEntry) error`

**What it does**: Stores file attributes for a path in the cache with a TTL of `DefaultAttrTTL` (30 seconds).

**How it's called**: Called by the FUSE layer after successfully fetching attributes from the backend, to warm the cache.

**Parameters**:
- `path` — the filesystem path to cache.
- `entry` — the `AttrEntry` to store.

**Returns**: An error if the write transaction fails.

**Implementation details**: Uses a NutsDB `Update` transaction with `tx.Put()` specifying a TTL in seconds. Logs `"cached attr"` at debug level on success, `"failed to cache attr"` at warn level on failure. NutsDB automatically expires entries after the TTL.

---

#### `(c *Cache) GetDir(path string) ([]fuse.DirEntry, error)`

**What it does**: Retrieves cached directory entries for a path from the `dir_cache` bucket. Converts internal `DirEntry` structs to `fuse.DirEntry` before returning.

**How it's called**: Called by the FUSE `ReadDir` implementation to check the cache before querying backend nodes.

**Parameters**:
- `path` — the directory path to look up.

**Returns**: A slice of `fuse.DirEntry` on cache hit, or an error on cache miss.

**Implementation details**: Internally stores `[]DirEntry` as JSON. On retrieval, iterates and converts each `DirEntry` to `fuse.DirEntry{Name, Mode, Ino}`. Logs `"cache hit"` with entry count at debug level.

---

#### `(c *Cache) PutDir(path string, entries []DirEntry) error`

**What it does**: Stores directory entries for a path in the cache with a TTL of `DefaultDirTTL` (30 seconds).

**How it's called**: Called by the FUSE layer after a successful `ReadDir` operation across all nodes, to cache the merged result.

**Parameters**:
- `path` — the directory path to cache.
- `entries` — the `[]DirEntry` slice to store.

**Returns**: An error if the write transaction fails.

**Implementation details**: JSON-marshals the entries and stores them with `tx.Put()` and a TTL. Logs `"cached dir"` at debug level with entry count and TTL.

---

#### `(c *Cache) Invalidate(path string)`

**What it does**: Removes a specific path from both the `attr_cache` and `dir_cache` buckets.

**How it's called**: Called when a file/directory is modified or deleted, so stale cache entries are not served to FUSE clients.

**Parameters**:
- `path` — the path to invalidate in both cache buckets.

**Returns**: Nothing (void). The delete errors are silently ignored.

**Implementation details**: Uses a NutsDB `Update` transaction to delete the key from both buckets. Logs `"invalidated cache"` at debug level.

---

#### `(c *Cache) InvalidatePrefix(prefix string) int`

**What it does**: Removes all cached entries whose key starts with a given prefix from both cache buckets. Used after dependency push to evict all entries under a repository path.

**How it's called**: Called from the dependency push workflow (in `IngestBlobs`/`DeleteBlobs` aftermath) to ensure stale attributes and directory listings under a prefix are not served after new data is pushed.

**Parameters**:
- `prefix` — the byte prefix to match for removal.

**Returns**: The total number of entries removed across both buckets.

**Implementation details**: Uses `tx.PrefixScanEntries()` with `offset=0, limit=-1` to find all matching keys, then deletes each one. Runs separate transactions per bucket. Errors from `PrefixScanEntries` (e.g., bucket empty) are silently ignored. Logs `"invalidated cache prefix"` at info level if any entries were removed.

---

#### `(c *Cache) Close() error`

**What it does**: Closes the underlying NutsDB database.

**How it's called**: Called during FUSE filesystem shutdown/unmount.

**Returns**: An error from `nutsdb.DB.Close()`.

**Implementation details**: Logs `"closing cache"` at info level before closing.

---

## `internal/client/client.go`

Package `client` provides a gRPC client wrapper for MonoFS backend communication.

### Types

#### `MonoFSClient` (interface)

```go
type MonoFSClient interface {
    Lookup(ctx context.Context, path string) (*pb.LookupResponse, error)
    GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error)
    ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error)
    Read(ctx context.Context, path string, offset, size int64) ([]byte, error)
    StatFS(ctx context.Context) (fsstat.Snapshot, error)
    QueryLogs(ctx context.Context, query string) ([]byte, error)
    WriteQueryLogs(ctx context.Context, query string, writer io.Writer) error
    Close() error
    RecordOperation()
    RecordBytesRead(n int64)
    RecordError()
    IsGuardianVisible() bool
}
```

The interface for all MonoFS client operations. Both a simple `Client` and `ShardedClient` implement this interface. It covers:
- **Path operations**: `Lookup`, `GetAttr`, `ReadDir`, `Read`
- **FS stats**: `StatFS`
- **Log queries**: `QueryLogs`, `WriteQueryLogs` (streaming variant)
- **Lifecycle**: `Close`
- **Metrics**: `RecordOperation`, `RecordBytesRead`, `RecordError`
- **Guardian visibility**: `IsGuardianVisible`

No functions are defined in this file; it only declares the interface.

---

## `internal/client/identity.go`

Provides persistent client identity management. Each MonoFS mount gets a stable UUID stored on disk so the router can track the same client across restarts.

### Constants

| Constant | Value | Description |
|---|---|---|
| `clientIDFileName` | `"client-id"` | Filename stored in `~/.monofs/` |

---

### Functions

#### `LoadOrCreateClientID() (string, error)`

**What it does**: Returns a persistent client identifier. Reads the ID from `~/.monofs/client-id` if the file exists and is non-empty. Otherwise generates a new UUID (v4), creates the config directory, writes the UUID to the file, and returns it.

**How it's called**: Called during `NewShardedClient` or `NewDisconnectedClient` construction if `cfg.ClientID` is empty. The caller passes the result as the `ClientID` field.

**Returns**: A UUID string (e.g., `"550e8400-e29b-41d4-a716-446655440000"`), or an error if the home directory cannot be determined or the file cannot be written.

**Implementation details**:
- Reads the ID file; trims whitespace.
- If the file is missing or empty, generates a new UUID using `github.com/google/uuid`.
- Creates `~/.monofs/` with mode `0700`.
- Writes the ID file with a trailing newline and mode `0600`.

---

#### `clientIDDir() (string, error)`

**What it does**: Returns the path `$HOME/.monofs` by calling `os.UserHomeDir()`.

**How it's called**: Internally by `LoadOrCreateClientID()`.

**Returns**: The absolute path to the config directory, or an error if the home directory cannot be determined.

---

## `internal/client/sharded.go`

The core of the client package. Provides `ShardedClient`, a multi-node gRPC client that routes requests using HRW (Rendezvous Hashing). Maintains connections to all backend nodes, refreshes topology from the router, and handles heartbeat/registration.

### Types

#### `ShardedClient`

```go
type ShardedClient struct {
    mu                   sync.RWMutex
    hrw                  *sharding.HRW
    conns                map[string]*grpc.ClientConn
    clients              map[string]pb.MonoFSClient
    routerAddr           string
    routerConn           *grpc.ClientConn
    routerClient         pb.MonoFSRouterClient
    clientID             string
    logger               *slog.Logger
    useExternalAddresses bool
    clusterVersion       int64
    refreshTicker        *time.Ticker
    stopRefresh          chan struct{}
    connected            bool
    lastError            error
    rpcTimeout           time.Duration
    hostname             string
    mountPoint           string
    writable             bool
    version              string
    registered           bool
    heartbeatInterval    time.Duration
    stopHeartbeat        chan struct{}
    operationsCount      int64
    bytesRead            int64
    errorsCount          int64
    guardianVisible      bool
    topologyChangeHook   func()
}
```

A multi-node client with HRW-based routing. It maintains persistent gRPC connections to all backend nodes and routes requests based on Rendezvous hashing. Fields:

- `hrw` — the currently-active HRW hash ring (nil until first topology load).
- `conns` / `clients` — maps from node ID to gRPC connection and protobuf client stub.
- `routerAddr` / `routerConn` / `routerClient` — connection to the MonoFS router service.
- `useExternalAddresses` — whether to prefer external node addresses (for host-mount clients).
- `clusterVersion` — monotonic version from the router; used to detect topology changes.
- `refreshTicker` / `stopRefresh` — periodic topology refresh control.
- `connected` / `lastError` — connection state.
- `registered` — whether the client has registered with the router.
- `heartbeatInterval` / `stopHeartbeat` — heartbeat loop control.
- `operationsCount` / `bytesRead` / `errorsCount` — atomic metrics sent via heartbeat.
- `guardianVisible` — set to `true` after first successful cluster info fetch.
- `topologyChangeHook` — optional callback for topology change notification.

#### `ShardedClientConfig`

```go
type ShardedClientConfig struct {
    RouterAddr           string
    ClientID             string
    RefreshInterval      time.Duration
    RPCTimeout           time.Duration
    UseExternalAddresses bool
    Logger               *slog.Logger
    Hostname             string
    MountPoint           string
    Writable             bool
    Version              string
}
```

Configuration for `NewShardedClient` and `NewDisconnectedClient`:
- `RouterAddr` — router service address (`host:port`).
- `ClientID` — unique client identifier. Auto-generated if empty.
- `RefreshInterval` — how often to refresh cluster topology (default: 30s).
- `RPCTimeout` — timeout for individual RPC calls (default: 10s).
- `UseExternalAddresses` — use external node addresses (for host-based clients).
- `Hostname`, `MountPoint`, `Writable`, `Version` — client identification for router registration.

#### `BlobFileType` and constants

```go
type BlobFileType = uint8
const (
    BlobFileRegular BlobFileType = 0
    BlobFileDir     BlobFileType = 1
    BlobFileSymlink BlobFileType = 2
)
```

Mirrors packager file types for blob ingestion: regular file (0), directory (1), symlink resolved to content (2).

#### `BlobFile`

```go
type BlobFile struct {
    Path     string
    Content  []byte
    Mode     uint32
    FileType BlobFileType
}
```

Describes a single file to be ingested into the cluster. `Path` is relative to the blob root (e.g., `"go/mod/cache/download/..."`).

#### `IngestBlobsResult`

```go
type IngestBlobsResult struct {
    FilesIngested int
    FilesFailed   int
    FailedFiles   []FailedBlobFile
}
```

Summarises a blob ingestion operation.

#### `FailedBlobFile`

```go
type FailedBlobFile struct {
    Path   string
    Reason string
}
```

Describes a single file that failed ingestion and why.

#### `DeleteBlobsResult`

```go
type DeleteBlobsResult struct {
    FilesDeleted int
    FilesFailed  int
}
```

Summarises a blob deletion operation.

---

### Functions

#### `NewShardedClient(ctx context.Context, cfg ShardedClientConfig) (*ShardedClient, error)`

**What it does**: Creates a new sharded client connected to the router. Fetches the initial cluster topology, establishes connections to all nodes, registers with the router, and starts background refresh and heartbeat loops.

**How it's called**: Called from the CLI/FUSE mounting code when starting a MonoFS mount.

**Parameters**:
- `ctx` — context for the initial topology fetch and registration.
- `cfg` — client configuration. Fields that are empty/zero get defaults filled in.

**Returns**: A fully connected `*ShardedClient`, or an error if the router connection fails or the initial cluster info fetch fails.

**Implementation details**:
- Defaults: `RefreshInterval` = 30s, `RPCTimeout` = 10s, `ClientID` auto-generated, `Hostname` from `os.Hostname()`, `Version` = `"unknown"`.
- Connects to the router via insecure gRPC.
- Calls `refreshClusterInfo()` to fetch the initial topology and establish node connections.
- Calls `registerWithRouter()` (non-fatal if it fails).
- Starts `refreshLoop()` and `heartbeatLoop()` as background goroutines.

---

#### `NewDisconnectedClient(cfg ShardedClientConfig) *ShardedClient`

**What it does**: Creates a `ShardedClient` that starts in a disconnected state and attempts to connect in the background via `reconnectLoop()`.

**How it's called**: Used when the router is not immediately available at startup (e.g., lazy mount scenarios). The client will keep retrying in the background.

**Parameters**:
- `cfg` — same as `ShardedClientConfig` but `Logger` defaults to `slog.Default()` if nil, and `RPCTimeout` defaults to 3s (shorter than the connected variant).

**Returns**: An unconnected `*ShardedClient` with the background reconnection loop already running.

**Implementation details**: Sets `connected = false` and `lastError = fmt.Errorf("not connected")`. Starts `reconnectLoop()` via `go sc.reconnectLoop()`.

---

#### `(sc *ShardedClient) reconnectLoop()`

**What it does**: Background goroutine that repeatedly attempts to connect to the router when not connected. On each tick, if not connected, calls `attemptConnection()`. If already connected, tries to refresh topology.

**How it's called**: Started automatically by `NewDisconnectedClient()`.

**Implementation details**: Runs an immediate connection attempt, then loops on `sc.refreshTicker.C` ticks. Exits when `sc.stopRefresh` is closed.

---

#### `(sc *ShardedClient) attemptConnection()`

**What it does**: Attempts to establish a router connection and fetch cluster topology. Called by `reconnectLoop()`. If already connected, returns immediately.

**How it's called**: From `reconnectLoop()`.

**Implementation details**:
- Checks-and-returns if already connected.
- Creates an insecure gRPC connection to `sc.routerAddr`.
- Calls `refreshClusterInfo()` with a 5-second timeout.
- On success, sets `connected = true`, clears `lastError`.
- Registers with the router and starts the heartbeat loop if not already running.
- Logs success/failure at debug/info level.

---

#### `(sc *ShardedClient) isConnected() bool`

**What it does**: Returns the current connection state under a read lock.

**How it's called**: By `reconnectLoop()` to check if a connection attempt is needed.

**Returns**: `true` if connected to the cluster, `false` otherwise.

---

#### `(sc *ShardedClient) refreshLoop()`

**What it does**: Background goroutine that periodically calls `refreshClusterInfo()` to sync topology and node health from the router.

**How it's called**: Started by `NewShardedClient()`.

**Implementation details**: Ticks on `sc.refreshTicker.C` with a 5-second context timeout. Exits when `sc.stopRefresh` is closed.

---

#### `(sc *ShardedClient) refreshClusterInfo(ctx context.Context) error`

**What it does**: Fetches the current cluster topology from the router via `GetClusterInfo` RPC, updates the HRW ring, manages gRPC connections to new/removed nodes, and logs topology changes.

**How it's called**: By `refreshLoop()`, `attemptConnection()`, `RefreshWorkspaceRepositories()`, and `reconnectLoop()`.

**Parameters**:
- `ctx` — context for the RPC call.

**Returns**: An error if the RPC fails, or `nil` on success. Errors from `refreshClusterInfo` do **not** set `connected = false` — the router may be temporarily unavailable while individual node connections still work.

**Implementation details**:
- Calls `routerClient.GetClusterInfo()` with `ClientId` and `UseExternalAddresses` flags.
- On success: sets `connected = true`, `guardianVisible = true`.
- On first load (`sc.hrw == nil`): creates a new `HRW` ring via `sharding.NewHRWFromProto()`.
- On subsequent calls: updates existing ring health via `sc.hrw.UpdateNodeHealthFromProto()`.
- Detects topology changes by comparing `resp.Version > sc.clusterVersion`.
- When topology changes: establishes new gRPC connections to nodes not yet in `sc.conns`, removes connections for nodes no longer in the cluster.
- Logs at INFO level on topology changes, DEBUG level on health-only syncs.
- Invokes `topologyChangeHook` on first load or when topology version changes.

---

#### `(sc *ShardedClient) SetTopologyChangeHook(hook func())`

**What it does**: Installs a callback that runs after the client loads its first topology and whenever the router reports a newer cluster version.

**How it's called**: Called by the FUSE mount code to be notified when topology changes, so it can rebuild the synthetic directory tree.

**Parameters**:
- `hook` — a niladic function to call on topology changes.

---

#### `splitDisplayPath(fullPath string) (displayPath, filePath string, ok bool)`

**What it does**: Splits a full MonoFS path into display path and file path components using `monopath.SplitDisplayPath()`. For example, `"github.com/owner/repo/README.md"` becomes `("github.com/owner/repo", "README.md", true)`.

**How it's called**: Called by `getNodeForFileFromRouter()`.

**Returns**: `(displayPath, filePath, ok)` where `ok` is `false` if the path cannot be split.

---

#### `buildShardKey(fullPath string) string`

**What it does**: Builds a sharding key from a full path using `monopath.BuildShardKey()`. The format is `"storageID:filePath"`, matching the router's sharding algorithm.

**How it's called**: By `Lookup()`, `GetAttr()`, and `Read()` to determine the routing key.

**Returns**: The shard key string, or `""` if the path cannot be parsed.

---

#### `(sc *ShardedClient) getNodeForFileFromRouter(ctx context.Context, fullPath string) (string, []string, error)`

**What it does**: Queries the router's `GetNodeForFile` RPC to determine which node should serve a file. Used during failover when HRW-based routing fails.

**How it's called**: By `Lookup()`, `GetAttr()`, and `Read()` as a fallback routing strategy.

**Parameters**:
- `ctx` — context for the RPC call.
- `fullPath` — the full MonoFS path.

**Returns**: The primary node ID, a list of fallback node IDs, and any error.

**Implementation details**: Splits the path into display path + file path, generates the storage ID, and calls `routerClient.GetNodeForFile()`. Logs the response (primary, fallbacks, rebalance state, cache TTL) at debug level.

---

#### `(sc *ShardedClient) withClientID(ctx context.Context) context.Context`

**What it does**: Attaches the client ID as gRPC metadata (`x-client-id`) to the outgoing context. This allows backend servers to identify the client for access pattern analysis.

**How it's called**: Called by `Read()` before making RPC calls.

**Returns**: A new context with the `x-client-id` metadata pair set.

---

#### `(sc *ShardedClient) Lookup(ctx context.Context, path string) (*pb.LookupResponse, error)`

**What it does**: Performs a lookup operation routed via HRW. Returns file/directory metadata from the correct shard. Implements multi-layer fallback: HRW primary → HRW fallbacks → other healthy nodes → router-based failover.

**How it's called**: By the FUSE layer when the kernel issues a `LOOKUP` operation (e.g., `stat()` on a path, or walking the directory tree).

**Parameters**:
- `ctx` — context for the RPC calls.
- `path` — the full MonoFS path to look up.

**Returns**: A `*pb.LookupResponse` with `Found`, `Ino`, `Attr`, etc., or an error.

**Implementation details**:
- Builds a shard key and gets the top 3 nodes from HRW.
- Tries up to 3 HRW-ranked nodes sequentially with per-node timeouts.
- If the primary returns `Found=false` (not an RPC error), broadens to check all other healthy nodes (handles directories where files are sharded across nodes).
- If all HRW attempts fail, queries the router via `getNodeForFileFromRouter()` for failover routing.
- Returns `&pb.LookupResponse{Found: false}` if the file truly does not exist anywhere.

---

#### `(sc *ShardedClient) GetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, error)`

**What it does**: Performs a `getattr` operation routed via HRW with the same multi-layer fallback strategy as `Lookup()`.

**How it's called**: By the FUSE `GetAttr` implementation (kernel `GETATTR` / `stat()` calls).

**Parameters**:
- `ctx` — context for RPC calls.
- `path` — the full MonoFS path.

**Returns**: A `*pb.GetAttrResponse` with `Found` and attribute fields, or an error.

**Implementation details**: Identical routing strategy to `Lookup()`: HRW primary → HRW fallbacks → all healthy nodes → router-based failover. Returns `&pb.GetAttrResponse{Found: false}` if not found.

---

#### `(sc *ShardedClient) ReadDir(ctx context.Context, path string) ([]*pb.DirEntry, error)`

**What it does**: Performs a `readdir` operation across **all healthy nodes in parallel**. Since files are sharded across nodes, every healthy node must be queried and results merged to produce a complete directory listing.

**How it's called**: By the FUSE `ReadDir` implementation.

**Parameters**:
- `ctx` — context for RPC calls.
- `path` — the directory path.

**Returns**: A sorted slice of `[]*pb.DirEntry`, or an error.

**Implementation details**:
- **Correctness guarantee**: Never returns partial results. If any node fails to respond fully, it retries that node once. If any node still fails after retry, returns an error so callers see an error rather than an incomplete file list.
- Queries all healthy nodes in parallel via goroutines, each calling `readDirFromNode()`.
- Merges results using a map keyed by entry name (first-write-wins for deduplication).
- Sorts entries by name for deterministic ordering (required by `go mod verify`).
- Retries failed nodes once. If still failing, returns `"readdir incomplete: node %s failed: %w"`.

---

#### `(sc *ShardedClient) readDirFromNode(ctx context.Context, client pb.MonoFSClient, nodeID, path string) ([]*pb.DirEntry, error)`

**What it does**: Queries a single node for directory entries via a gRPC server-side streaming RPC (`ReadDir`). Applies a per-node timeout.

**How it's called**: Internally by `ReadDir()` in parallel goroutines, once per healthy node.

**Parameters**:
- `ctx` — parent context.
- `client` — the gRPC client stub for the target node.
- `nodeID` — the node's identifier (for logging).
- `path` — the directory path.

**Returns**: `([]*pb.DirEntry, nil)` on clean EOF, or `(partialEntries, error)` on stream error.

**Implementation details**: Creates a node-specific context with `sc.rpcTimeout`. Reads entries from the stream via `stream.Recv()` until `io.EOF`. On stream error, returns whatever entries were received along with the error.

---

#### `(sc *ShardedClient) Read(ctx context.Context, path string, offset, size int64) ([]byte, error)`

**What it does**: Performs a read operation routed via HRW. Reads `size` bytes starting at `offset` from the file at `path`. Uses gRPC server-side streaming to receive chunks. Implements HRW + router-based failover.

**How it's called**: By the FUSE `Read` implementation (kernel `READ` calls).

**Parameters**:
- `ctx` — context for RPC calls.
- `path` — the full MonoFS path to the file.
- `offset` — byte offset to start reading from.
- `size` — number of bytes to read.

**Returns**: A byte slice containing the read data, or an error. Returns an empty non-nil slice (`[]byte{}`) on successful zero-byte reads (distinguishable from nil/error).

**Implementation details**:
- Tries up to 3 HRW-ranked nodes, then falls back to router-based routing.
- Reads via a streaming RPC (`client.Read()`), accumulating chunks into a buffer.
- Tracks operations count and bytes read via `atomic.AddInt64()` for heartbeat metrics.
- On stream error during HRW attempt, tries the next node (partial reads are discarded).

---

#### `(sc *ShardedClient) Close() error`

**What it does**: Gracefully shuts down the client: stops heartbeat, unregisters from router, stops refresh loop, closes all backend gRPC connections, and closes the router connection.

**How it's called**: During FUSE unmount, when the filesystem is being shut down.

**Returns**: Always `nil` (individual close errors are swallowed).

**Implementation details**:
- Closes `stopHeartbeat` channel (if not already closed) to stop heartbeat goroutine.
- Calls `unregisterFromRouter("client shutdown")` if registered.
- Closes `stopRefresh` channel and stops ticker.
- Under write lock: iterates all `sc.conns`, calls `conn.Close()` on each, then nils out the maps.
- Closes `sc.routerConn`.

---

#### `(sc *ShardedClient) GetClusterInfo() []sharding.Node`

**What it does**: Returns all nodes in the current cluster topology (including unhealthy ones).

**Returns**: A slice of `sharding.Node`, or `nil` if no topology has been loaded.

---

#### `(sc *ShardedClient) GetHealthyNodes() []sharding.Node`

**What it does**: Returns only the healthy nodes in the current cluster topology.

**Returns**: A slice of `sharding.Node`, or `nil` if no topology has been loaded.

---

#### `(sc *ShardedClient) registerWithRouter(ctx context.Context) error`

**What it does**: Registers this client with the router via `RegisterClient` RPC, providing client ID, mount point, hostname, writable flag, and version.

**How it's called**: By `NewShardedClient()`, `attemptConnection()`, and `sendHeartbeat()` (re-registration).

**Parameters**:
- `ctx` — context for the RPC call.

**Returns**: An error if the RPC fails or the router rejects registration.

**Implementation details**: On success, sets `sc.registered = true` and updates `sc.heartbeatInterval` from the router's response (`HeartbeatIntervalMs`).

---

#### `(sc *ShardedClient) unregisterFromRouter(reason string)`

**What it does**: Unregisters this client from the router via `UnregisterClient` RPC with a reason string.

**How it's called**: By `Close()` during shutdown.

**Parameters**:
- `reason` — a human-readable reason for disconnection (e.g., `"client shutdown"`).

**Implementation details**: If not registered or no router connection, returns immediately. Uses a 5-second timeout. Sets `sc.registered = false` on success.

---

#### `(sc *ShardedClient) heartbeatLoop()`

**What it does**: Background goroutine that sends periodic heartbeats to the router at the configured interval.

**How it's called**: Started by `NewShardedClient()` or `attemptConnection()` after successful registration.

**Implementation details**: Runs a ticker at `sc.heartbeatInterval`. Exits when `sc.stopHeartbeat` is closed.

---

#### `(sc *ShardedClient) sendHeartbeat()`

**What it does**: Sends a single heartbeat to the router via `ClientHeartbeat` RPC, reporting current operations count, bytes read, and errors count. If the router responds with `ShouldRegister=true`, triggers re-registration.

**How it's called**: Periodically by `heartbeatLoop()`.

**Implementation details**: Uses a 5-second timeout. If `ShouldRegister` is true, calls `registerWithRouter()`.

---

#### `(sc *ShardedClient) RecordOperation()`

**What it does**: Atomically increments the operations counter. Used for heartbeat metrics.

**How it's called**: By the FUSE layer on every filesystem operation.

---

#### `(sc *ShardedClient) RecordBytesRead(n int64)`

**What it does**: Atomically adds `n` to the bytes-read counter. Used for heartbeat metrics.

**How it's called**: By the FUSE layer after successful read operations.

---

#### `(sc *ShardedClient) RecordError()`

**What it does**: Atomically increments the error counter. Used for heartbeat metrics.

**How it's called**: By the FUSE layer when an operation fails.

---

#### `(sc *ShardedClient) IsGuardianVisible() bool`

**What it does**: Reports whether Guardian namespaces should be exposed. Returns `true` after the first successful `refreshClusterInfo()` call.

**How it's called**: By the FUSE layer to decide whether to show guardian-related paths in the synthetic directory tree.

---

#### `(sc *ShardedClient) StatFS(ctx context.Context) (fsstat.Snapshot, error)`

**What it does**: Returns namespace-wide filesystem statistics derived from router cluster info and current logical usage counters. Aggregates disk usage and file counts across all active nodes.

**How it's called**: By the FUSE `StatFS` implementation (kernel `STATFS` / `df` calls).

**Parameters**:
- `ctx` — context for the RPC call.

**Returns**: A `fsstat.Snapshot` with total/used/available bytes and file count, or an error.

**Implementation details**: Calls `routerClient.GetClusterInfo()`, then iterates nodes that have `status == "Active"` (filtering out nodes with other statuses or nil nodes). Sums `DiskUsedBytes` and `TotalFiles`. Returns `fsstat.FromUsage(usedBytes, totalFiles)`.

---

#### `(sc *ShardedClient) GetClientID() string`

**What it does**: Returns the unique client identifier.

---

#### `(sc *ShardedClient) SetMountPoint(mountPoint string)`

**What it does**: Sets the mount point for registration. Should be called before the first connection.

---

#### `(sc *ShardedClient) IngestBlobs(ctx context.Context, files []BlobFile) (*IngestBlobsResult, error)`

**What it does**: Ingests blob files (e.g., Go module cache dependency files) into the backend cluster. Files are sharded via HRW across healthy nodes and become visible under `/mnt/monofs/dependency/...` on all clients.

**How it's called**: From the dependency push workflow (e.g., `monofs-session push`).

**Parameters**:
- `ctx` — context for RPC calls.
- `files` — slice of `BlobFile` to ingest.

**Returns**: An `*IngestBlobsResult` with counts and per-file failure details, or an error if no nodes are available.

**Implementation details**: A 5-step process:
1. **Register repository**: Calls `RegisterRepository` on all healthy nodes with `displayPath = "dependency"`.
2. **Build metadata and shard**: For each file, computes SHA-256 content hash, builds `FileMetadata`, and determines the primary HRW node. File types: regular (default mode `0444`), directory (mode `0555`, no content/hash), symlink (with `file_type` backend metadata).
3. **Send batches**: Groups files by target node. Splits into sub-batches respecting `maxBatchBytes` (64 MB) and `maxBatchFiles` (2000) limits per gRPC call. Uses 120-second timeouts.
4. **Send dir-hints**: Sends lightweight `dir_hint` metadata for ALL files to EVERY healthy node. This ensures each node has a complete directory index even though it only owns a subset of files via HRW sharding. Uses 60-second timeouts.
5. **Build indexes**: Calls `BuildDirectoryIndexes` on all nodes (30-second timeouts).
6. **Mark onboarded**: Calls `MarkRepositoryOnboarded` on all nodes (5-second timeouts, errors silently ignored).

---

#### `(sc *ShardedClient) DeleteBlobs(ctx context.Context, paths []string) (*DeleteBlobsResult, error)`

**What it does**: Removes previously-ingested dependency files from the cluster backend. Routes per-file deletions via HRW to the correct primary node, then rebuilds directory indexes.

**How it's called**: From the dependency cleanup workflow.

**Parameters**:
- `ctx` — context for RPC calls.
- `paths` — relative paths under the dependency root to delete.

**Returns**: `*DeleteBlobsResult` with deleted/failed counts, or an error.

**Implementation details**:
- Groups paths by HRW target node.
- For each path, calls `client.DeleteFile()` with a 10-second timeout.
- Rebuilds directory indexes on all healthy nodes after deletion.
- Logs warnings for individual deletion failures but continues to attempt remaining files.

---

#### `(sc *ShardedClient) QueryLogs(ctx context.Context, query string) ([]byte, error)`

**What it does**: Delegates log queries directly to the MonoFS router. Returns the query result as a byte slice.

**How it's called**: From administrative/debugging CLI commands.

**Parameters**:
- `ctx` — context for the RPC call.
- `query` — the log query string.

**Returns**: The raw response bytes, or an error.

**Implementation details**: Simply delegates to `WriteQueryLogs()` into a `bytes.Buffer` and returns the buffer's bytes.

---

#### `(sc *ShardedClient) WriteQueryLogs(ctx context.Context, query string, writer io.Writer) error`

**What it does**: Streams log query results from the router via `StreamQueryLogs` RPC and writes a JSON array to the provided writer.

**How it's called**: By `QueryLogs()` and potentially by streaming CLI output.

**Parameters**:
- `ctx` — context for the RPC call.
- `query` — the log query string.
- `writer` — the `io.Writer` to write the JSON array to.

**Returns**: An error if the RPC fails or writing fails.

**Implementation details**: Opens a streaming RPC, writes `[`, iterates received items writing each `ItemJson` with commas between them, writes `]`. Handles nil/empty items gracefully.

---

## `internal/client/repository_changes.go`

Handles batch apply of repository changes (upserts and deletes) after a workspace refresh has pulled new content. Used by `ShardedClient.ApplyRepositoryChanges()`.

### Types

#### `RepositoryChangeKind`

```go
type RepositoryChangeKind string
```

Enum for repository change types. Values:
- `RepositoryChangeUpsert` (`"upsert"`) — create or update a file.
- `RepositoryChangeDelete` (`"delete"`) — remove a file.

#### `RepositoryChange`

```go
type RepositoryChange struct {
    Kind    RepositoryChangeKind
    Path    string
    Content []byte
    Mode    uint32
    Mtime   int64
}
```

Describes a single file change to apply to the cluster.

#### `ApplyRepositoryChangesResult`

```go
type ApplyRepositoryChangesResult struct {
    FilesUpserted int
    FilesDeleted  int
    FilesFailed   int
}
```

Summary counts from a batch apply operation.

---

### Functions

#### `(sc *ShardedClient) ApplyRepositoryChanges(ctx context.Context, repo WorkspaceRepository, changes []RepositoryChange) (*ApplyRepositoryChangesResult, error)`

**What it does**: Applies a batch of file upserts and deletes to the cluster for a given repository. Handles HRW sharding, batches ingest calls, sends directory hints, and triggers index rebuilds.

**How it's called**: Called after a workspace refresh, when the client needs to push new file content to the cluster nodes.

**Parameters**:
- `ctx` — context for RPC calls.
- `repo` — the repository metadata (storage ID, display path, source, ref).
- `changes` — slice of `RepositoryChange` to apply.

**Returns**: `*ApplyRepositoryChangesResult` with success/failure counts, or an error.

**Implementation details**:
- Derives `storageID` from `repo.StorageID` or generates one via `sharding.GenerateStorageID()` from `repo.DisplayPath`.
- For each change, determines the HRW primary node and batches into per-node upsert and delete maps.
- Upsert batches: sends `IngestFileBatch` RPC with full file metadata, SHA-256 blob hash, inline content, and source/ref info. Default mode `0644`, default mtime `now`.
- Sends `dir_hint` metadata for ALL changed files to ALL healthy nodes (same pattern as `IngestBlobs`).
- Delete batches: iterates paths per node and calls `DeleteFile` RPC for each.
- After all mutations, calls `BuildDirectoryIndexes` on all healthy nodes with a 3x RPC timeout.
- Returns an error if `FilesFailed > 0`.

---

## `internal/client/workspace.go`

Provides workspace repository discovery and path resolution for the synthetic monorepo view.

### Types

#### `WorkspaceRepository`

```go
type WorkspaceRepository struct {
    StorageID     string
    DisplayPath   string
    Source        string
    Ref           string
    CommitHash    string
    CommitTime    int64
    CommitMessage string
}
```

Describes a repository visible in the mounted workspace. Fields:
- `StorageID` — the repository's storage identifier in the cluster.
- `DisplayPath` — the user-visible path (e.g., `"github.com/owner/repo"`).
- `Source` — the git remote URL.
- `Ref` — the git branch/tag name.
- `CommitHash` — the current HEAD commit hash.
- `CommitTime` — the timestamp of the commit.
- `CommitMessage` — the commit message.

#### `WorkspaceMetadataProvider` (interface)

```go
type WorkspaceMetadataProvider interface {
    ListWorkspaceRepositories(ctx context.Context) ([]WorkspaceRepository, error)
    ResolveWorkspacePath(ctx context.Context, path string) (*WorkspaceRepository, error)
}
```

Exposes repository discovery and path resolution for clients that support a synthetic monorepo view.

#### `ErrWorkspacePathNotFound`

```go
var ErrWorkspacePathNotFound = errors.New("workspace path not found")
```

Sentinel error indicating a path does not belong to any known repository in the current workspace view.

---

### Functions

#### `(sc *ShardedClient) ListWorkspaceRepositories(ctx context.Context) ([]WorkspaceRepository, error)`

**What it does**: Discovers repositories currently visible through the cluster by querying all backend nodes in parallel for their registered repository metadata. Deduplicates by storage ID.

**How it's called**: Called by the FUSE layer to build the synthetic monorepo directory tree, and by `ResolveWorkspacePath()`.

**Parameters**:
- `ctx` — context for RPC calls.

**Returns**: A sorted slice of `WorkspaceRepository`, or an error if no healthy responses are received.

**Implementation details**:
- Queries all connected nodes concurrently via goroutines.
- For each node, calls `ListRepositories()` to get storage IDs, then `GetRepositoryInfo()` for each storage ID.
- Deduplicates using a map keyed by `storageID` under a mutex.
- If all nodes fail, returns the first error encountered.
- Sorts results by `DisplayPath`, then by `StorageID`.

---

#### `(sc *ShardedClient) ResolveWorkspacePath(ctx context.Context, path string) (*WorkspaceRepository, error)`

**What it does**: Resolves a user-visible path to the repository that owns it by longest-prefix matching against discovered display paths.

**How it's called**: Called by the FUSE layer when the kernel accesses a path, to determine which repository backs it.

**Parameters**:
- `ctx` — context for the repository listing call.
- `path` — the full path to resolve (e.g., `"github.com/owner/repo/subdir/file.go"`).

**Returns**: A `*WorkspaceRepository` if matched, or `ErrWorkspacePathNotFound` if no repository owns the path.

**Implementation details**: Calls `ListWorkspaceRepositories()` internally, then scans for the longest-prefix match where the path starts with `repo.DisplayPath + "/"`. Returns the repository with the longest matching display path.

---

## `internal/client/workspace_publish.go`

Handles publishing workspace bundles: uploading a workspace bundle to the router, streaming publish events, and triggering workspace refresh after successful publish.

### Types

#### `WorkspacePublishOptions`

```go
type WorkspacePublishOptions struct {
    WorkspaceID             string
    LogicalCommitMessage    string
    AuthorName              string
    AuthorEmail             string
    RequestedBranchStrategy string
}
```

Options for `PublishWorkspaceBundle()`:
- `WorkspaceID` — identifier for the workspace (fallback if bundle has none).
- `LogicalCommitMessage` — commit message for the publish.
- `AuthorName`, `AuthorEmail` — author attribution.
- `RequestedBranchStrategy` — branching strategy (empty or `"direct"` triggers post-publish refresh).

#### `WorkspacePublishResult`

```go
type WorkspacePublishResult struct {
    BundleID              string
    Job                   *pb.WorkspaceSyncJob
    Events                []*pb.WorkspaceSyncEvent
    PublishedRepositories []WorkspaceRepository
    RefreshedRepositories []WorkspaceRepository
    Warning               string
}
```

Result of a workspace publish operation.

---

### Functions

#### `(sc *ShardedClient) PublishWorkspaceBundle(ctx context.Context, bundle *workspacebundle.Bundle, opts WorkspacePublishOptions) (*WorkspacePublishResult, error)`

**What it does**: Publishes a workspace bundle to the cluster. Normalizes the bundle, uploads it in 1 MB chunks via streaming RPC, starts publish via `PublishWorkspace` streaming RPC, collects events, and optionally triggers a workspace refresh on success.

**How it's called**: From the workspace publish CLI workflow (`monofs-session publish`).

**Parameters**:
- `ctx` — context for RPC calls.
- `bundle` — the workspace bundle to publish.
- `opts` — publish options.

**Returns**: `*WorkspacePublishResult` with the job, events, and repository lists, or an error.

**Implementation details**:
- Normalizes the bundle via `normalizeWorkspaceBundleForPublish()`.
- Marshals the bundle to JSON and uploads via `UploadWorkspaceBundle` streaming RPC in 1 MB chunks with a timeout of `12 * rpcTimeout`.
- After upload, starts `PublishWorkspace` streaming RPC with a timeout of `30 * rpcTimeout`.
- Collects all events from the stream; extracts the final `WorkspaceSyncJob`.
- If branch strategy is empty or `"direct"`, calls `RefreshWorkspaceRepositories()` after publish. On refresh failure, sets a `Warning` field instead of returning an error.
- If the job state is not `WORKSPACE_SYNC_STATE_SUCCEEDED`, returns a formatted error.

---

#### `normalizeWorkspaceBundleForPublish(bundle *workspacebundle.Bundle, requestedWorkspaceID, clientID string) (*workspacebundle.Bundle, error)`

**What it does**: Normalizes a workspace bundle for publishing. Ensures the workspace ID is set (using the requested ID, client ID, or an auto-generated one, in that order of preference). Validates the normalized bundle.

**How it's called**: Internally by `PublishWorkspaceBundle()`.

**Parameters**:
- `bundle` — the original bundle.
- `requestedWorkspaceID` — the workspace ID from options.
- `clientID` — the client's identifier as a fallback.

**Returns**: A validated normalized bundle copy, or an error.

**Implementation details**: Clones the bundle (copies `Repositories` slice). Falls back: `bundle.WorkspaceID` → `requestedWorkspaceID` → `clientID` → `"workspace-<unix_nano>"`. Calls `normalized.Validate()`.

---

#### `publishedWorkspaceRepositoriesFromJob(job *pb.WorkspaceSyncJob, bundle *workspacebundle.Bundle) []WorkspaceRepository`

**What it does**: Extracts a list of successfully published `WorkspaceRepository` entries from a sync job and bundle. Only includes repositories with status `WORKSPACE_SYNC_REPOSITORY_STATUS_PUBLISHED`. Enriches with source URL and branch from the bundle when available.

**How it's called**: Internally by `PublishWorkspaceBundle()`.

**Parameters**:
- `job` — the completed sync job from the router.
- `bundle` — the original workspace bundle for enrichment.

**Returns**: A slice of `WorkspaceRepository` for successfully published repos.

**Implementation details**: Deduplicates by storage ID. Falls back to `RepoUrl` and `Branch` from the job result if bundle lookup fails.

---

#### `workspacePublishJobError(job *pb.WorkspaceSyncJob) error`

**What it does**: Formats a human-readable error from a failed workspace sync job. Extracts the error message or lists repositories with conflict/failed/cancelled statuses.

**How it's called**: Internally by `PublishWorkspaceBundle()` when the job state is not `SUCCEEDED`.

**Parameters**:
- `job` — the failed sync job.

**Returns**: A formatted error describing the failure.

**Implementation details**: Checks `job.GetErrorMessage()` first. Then iterates repositories looking for `CONFLICT`, `FAILED`, or `CANCELLED` statuses. Truncates at 3 issues + `"and N more"`. Falls back to `"ended in state <state>"`.

---

## `internal/client/workspace_refresh.go`

Handles refreshing workspace repositories: requesting the router to pull latest content from git remotes into the cluster.

### Types

#### `WorkspaceRefreshResult`

```go
type WorkspaceRefreshResult struct {
    Requested int
    Refreshed int
    Failed    int
    Results   []WorkspaceRepositoryRefresh
}
```

Summary of a workspace refresh operation.

#### `WorkspaceRepositoryRefresh`

```go
type WorkspaceRepositoryRefresh struct {
    StorageID   string
    DisplayPath string
    Refreshed   bool
    Message     string
    Error       string
}
```

Per-repository result of a refresh operation.

---

### Functions

#### `refreshRequestForRepository(repo WorkspaceRepository) (*pb.WorkspaceRepositoryRef, error)`

**What it does**: Validates and converts a `WorkspaceRepository` into a protobuf `WorkspaceRepositoryRef` suitable for the refresh RPC. Requires `DisplayPath`, `Source`, `Ref`, and `CommitHash` to be non-empty.

**How it's called**: Internally by `RefreshWorkspaceRepositories()` for each repository to refresh.

**Parameters**:
- `repo` — the workspace repository metadata.

**Returns**: A `*pb.WorkspaceRepositoryRef`, or an error if validation fails.

---

#### `(sc *ShardedClient) RefreshWorkspaceRepositories(ctx context.Context, repos []WorkspaceRepository) (*WorkspaceRefreshResult, error)`

**What it does**: Requests the router to refresh (pull latest git content) for a set of workspace repositories. Starts a `RefreshWorkspace` streaming RPC, consumes events, and after completion refreshes the local cluster topology.

**How it's called**: From the workspace refresh CLI workflow (`monofs-session pull`), and automatically after `PublishWorkspaceBundle()`.

**Parameters**:
- `ctx` — context for RPC calls.
- `repos` — the repositories to refresh.

**Returns**: `*WorkspaceRefreshResult` with per-repo results, or an error.

**Implementation details**:
- Deduplicates repositories via `dedupeWorkspaceReposForRefresh()`.
- Validates each repo via `refreshRequestForRepository()`; records failures for invalid repos.
- Opens `RefreshWorkspace` streaming RPC with timeout `30 * rpcTimeout`.
- Consumes the stream via `consumeWorkspaceRefreshStream()`.
- After completion, calls `refreshClusterInfo()` (errors logged as warnings).
- Returns aggregate error if any failures occurred.

---

#### `dedupeWorkspaceReposForRefresh(repos []WorkspaceRepository) []WorkspaceRepository`

**What it does**: Removes duplicate repositories from a slice, using `StorageID` (falling back to `DisplayPath`) as the deduplication key. Sorts by `DisplayPath`.

**How it's called**: Internally by `RefreshWorkspaceRepositories()`.

**Returns**: A deduplicated, sorted slice.

---

#### `consumeWorkspaceRefreshStream(stream pb.MonoFSRouter_RefreshWorkspaceClient) (*pb.WorkspaceSyncJob, error)`

**What it does**: Consumes all events from a `RefreshWorkspace` streaming RPC. Tracks the last `WorkspaceSyncJob` from events. Returns the final job and any error.

**How it's called**: Internally by `RefreshWorkspaceRepositories()`.

**Parameters**:
- `stream` — the gRPC server-side streaming client.

**Returns**: The last `*pb.WorkspaceSyncJob` seen, or an error including a formatted job error.

**Implementation details**: Loops on `stream.Recv()` until `io.EOF`. On EOF, if no job was received, returns `"stream ended before completion"`. If the job state is not `SUCCEEDED`, returns `workspaceRefreshJobError(job)`.

---

#### `appendWorkspaceRefreshResults(result *WorkspaceRefreshResult, job *pb.WorkspaceSyncJob)`

**What it does**: Populates a `WorkspaceRefreshResult` from a completed sync job's summary and per-repository results. Updates `Refreshed`/`Failed` counts and appends per-repo entries.

**How it's called**: Internally by `RefreshWorkspaceRepositories()`.

**Parameters**:
- `result` — the result struct to populate.
- `job` — the completed sync job.

**Implementation details**: Reads `job.GetSummary()` for counts. Iterates `job.GetRepositories()`, mapping `FAILED`/`CONFLICT`/`CANCELLED` statuses to `entry.Error` and all others to `entry.Refreshed = true`.

---

#### `workspaceRefreshJobError(job *pb.WorkspaceSyncJob) error`

**What it does**: Formats a human-readable error from a failed workspace refresh job. Mirrors `workspacePublishJobError()` but with `"workspace refresh failed"` prefix.

**How it's called**: Internally by `consumeWorkspaceRefreshStream()`.

**Parameters**:
- `job` — the failed sync job.

**Returns**: A formatted error describing the failure.

**Implementation details**: Same logic as `workspacePublishJobError()` but with different message prefix and no 3-issue truncation.

---

## `internal/client/workspace_source_push.go`

Handles pushing source commit bundles to the cluster: uploading a source commit bundle and streaming push events from the router.

### Types

#### `WorkspaceSourcePushResult`

```go
type WorkspaceSourcePushResult struct {
    BundleID string
    Job      *pb.WorkspaceSyncJob
    Events   []*pb.WorkspaceSyncEvent
}
```

Result of a workspace source push operation.

---

### Functions

#### `(sc *ShardedClient) PushWorkspaceCommitBundle(ctx context.Context, bundle *workspacebundle.SourceCommitBundle) (*WorkspaceSourcePushResult, error)`

**What it does**: Pushes a source commit bundle to the cluster. Normalizes the bundle, uploads it in 1 MB chunks via streaming RPC, and streams push events from the router.

**How it's called**: From the workspace source push CLI workflow.

**Parameters**:
- `ctx` — context for RPC calls.
- `bundle` — the source commit bundle to push.

**Returns**: `*WorkspaceSourcePushResult` with the job and events, or an error.

**Implementation details**:
- Normalizes the bundle via `normalizeSourceCommitBundleForPush()`.
- Marshals the bundle to JSON and uploads via `UploadWorkspaceCommitBundle` streaming RPC in 1 MB chunks with timeout `12 * rpcTimeout`.
- After upload, starts `PushWorkspaceCommits` streaming RPC with timeout `30 * rpcTimeout`.
- Collects all events from the stream; extracts the final `WorkspaceSyncJob`.
- If the job state is not `SUCCEEDED`, returns `workspaceSourcePushJobError(job)`.

---

#### `normalizeSourceCommitBundleForPush(bundle *workspacebundle.SourceCommitBundle, clientID string) (*workspacebundle.SourceCommitBundle, error)`

**What it does**: Normalizes a source commit bundle for push. Ensures `WorkspaceID` is set (falling back to `clientID` then an auto-generated ID). Validates the normalized bundle.

**How it's called**: Internally by `PushWorkspaceCommitBundle()`.

**Parameters**:
- `bundle` — the original bundle.
- `clientID` — the client's identifier as a fallback for workspace ID.

**Returns**: A validated normalized bundle copy, or an error.

**Implementation details**: Clones the bundle. Falls back: `bundle.WorkspaceID` → `clientID` → `"workspace-<unix_nano>"`. Calls `normalized.Validate()`.

---

#### `workspaceSourcePushJobError(job *pb.WorkspaceSyncJob) error`

**What it does**: Formats a human-readable error from a failed workspace source push job. Mirrors `workspacePublishJobError()` but with `"workspace source push failed"` prefix.

**How it's called**: Internally by `PushWorkspaceCommitBundle()`.

**Parameters**:
- `job` — the failed sync job.

**Returns**: A formatted error describing the failure.

**Implementation details**: Checks `job.GetErrorMessage()` first, then iterates repositories for `CONFLICT`/`FAILED`/`CANCELLED` statuses. Falls back to a generic `"workspace source push failed"`.

---

## `internal/diagnostics/http.go`

Package `diagnostics` provides a diagnostics HTTP server exposing Prometheus `/metrics` and Go pprof profiling endpoints.

---

### Functions

#### `NewHandler() *http.ServeMux`

**What it does**: Returns a configured `http.ServeMux` with routes for `/metrics` (Prometheus handler) and `/debug/pprof/` (Go pprof endpoints: index, cmdline, profile, symbol, trace, allocs, block, goroutine, heap, mutex, threadcreate).

**How it's called**: By `StartServer()` (as the default handler) and by `StartServerWithMux()`.

**Returns**: A fully configured `*http.ServeMux`.

**Implementation details**: Uses `promhttp.Handler()` from `github.com/prometheus/client_golang` for metrics. Uses `net/http/pprof` standard library handlers for profiling.

---

#### `StartServer(logger *slog.Logger, component, addr string) *http.Server`

**What it does**: Starts a diagnostics HTTP server on the given address with the default handler from `NewHandler()`. Returns `nil` when `addr` is empty (diagnostics disabled).

**How it's called**: From main server startup code to optionally enable a diagnostics port.

**Parameters**:
- `logger` — structured logger for lifecycle messages.
- `component` — human-readable component name for log context (e.g., `"monofs-router"`).
- `addr` — listen address (`host:port`). If empty, diagnostics are disabled and `nil` is returned.

**Returns**: The `*http.Server` instance (for later shutdown), or `nil` if disabled.

**Implementation details**: Delegates to `StartServerWithMux()` with `NewHandler()`.

---

#### `StartServerWithMux(logger *slog.Logger, component, addr string, mux *http.ServeMux) *http.Server`

**What it does**: Starts a diagnostics HTTP server using the provided mux. Callers can add custom routes to the mux before starting. Returns `nil` when `addr` is empty.

**How it's called**: By `StartServer()`, and potentially by other components that need custom diagnostic routes.

**Parameters**:
- `logger` — structured logger.
- `component` — component name for log context.
- `addr` — listen address. Empty means disabled.
- `mux` — the `*http.ServeMux` to use.

**Returns**: The `*http.Server` instance, or `nil` if disabled.

**Implementation details**: Starts `server.ListenAndServe()` in a background goroutine. Logs the listening address and pprof path at info level. Logs errors at error level (except `http.ErrServerClosed` which is expected during shutdown).

---

#### `ShutdownServer(logger *slog.Logger, component string, server *http.Server)`

**What it does**: Gracefully shuts down a diagnostics server started with `StartServer()` or `StartServerWithMux()`.

**How it's called**: During application shutdown.

**Parameters**:
- `logger` — structured logger.
- `component` — component name for log context.
- `server` — the `*http.Server` to shut down.

**Implementation details**: Uses a 5-second shutdown context. If `server` is `nil`, returns immediately. Logs shutdown errors at warn level.
