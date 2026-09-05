# Fetcher Package Documentation

Package `fetcher` (`internal/fetcher`) provides the blob-fetching and workspace-sync service for MonoFS. It defines a backend registry, a gRPC service (`Service`), a gRPC client with affinity routing (`Client`), Prometheus metrics, and workspace publish/source-push handlers.

---

## File: `backend.go`

### Types

#### `Backend` (type alias)
```go
type Backend = storage.FetchBackend
```
Canonical interface for fetch backends. Aliased from `storage.FetchBackend`. All fetch backends implement this unified interface.

#### `SourceType` (type alias)
```go
type SourceType = storage.FetchType
```
Identifies the data source backend type. Aliased from `storage.FetchType`.

#### `BackendConfig` (type alias)
```go
type BackendConfig = storage.BackendConfig
```
Common configuration for all backends. Aliased from `storage.BackendConfig`.

#### `FetchRequest` (type alias)
```go
type FetchRequest = storage.FetchRequest
```
Contains all information needed to fetch a blob. Aliased from `storage.FetchRequest`.

#### `FetchResult` (type alias)
```go
type FetchResult = storage.FetchResult
```
Contains the fetched blob data. Aliased from `storage.FetchResult`.

#### `BackendStats` (type alias)
```go
type BackendStats = storage.BackendStats
```
Holds statistics for a backend. Aliased from `storage.BackendStats`.

#### `Registry`
```go
type Registry struct {
    backends map[storage.FetchType]Backend
}
```
Manages available fetch backend singleton instances at runtime. Unlike `storage.BackendRegistry` (which stores factories), this stores initialized instances used by the fetcher `Service`.

---

### Constants

```go
const (
    SourceTypeUnknown SourceType = storage.FetchTypeUnknown // Unknown/unset
    SourceTypeGit     SourceType = storage.FetchTypeGit     // Git repository
    SourceTypeBlob    SourceType = storage.FetchTypeBlob    // Packager-based blob archive store (default)
)
```

---

### Functions

#### `ParseSourceType`
```go
func ParseSourceType(s string) SourceType
```
Converts a string to a `SourceType`. Delegates to `storage.ParseFetchType`. Called from code that needs to parse source type strings from configuration or API inputs.

**Parameters:**
- `s string` — The string representation of a source type.

**Returns:**
- `SourceType` — The parsed source type.

---

#### `NewRegistry`
```go
func NewRegistry() *Registry
```
Creates and returns a new, empty backend registry. Initializes the internal `backends` map.

**Called from:** `NewService` in `service.go:102`, which passes it to the fetcher `Service`.

**Returns:**
- `*Registry` — A new registry with an empty backend map.

---

#### `(*Registry) Register`
```go
func (r *Registry) Register(backend Backend)
```
Adds a backend instance to the registry, keyed by its `Type()` return value.

**Parameters:**
- `backend Backend` — The backend instance to register.

---

#### `(*Registry) Get`
```go
func (r *Registry) Get(sourceType SourceType) (Backend, bool)
```
Retrieves the backend for a given source type.

**Called from:** `Service.FetchBlob`, `Service.fetchSingleBlob`, `Service.PrefetchBlobs`, `Service.CheckCache`, `Service.GetStats`, `Service.StoreBlob`, `Service.StoreBlobBatchStream`, `Service.DeleteBlobs`, `Service.StoreArchive`, `Service.processPrefetchJob`.

**Parameters:**
- `sourceType SourceType` — The type of backend to look up.

**Returns:**
- `Backend` — The backend instance if found.
- `bool` — `true` if the backend exists, `false` otherwise.

---

#### `(*Registry) All`
```go
func (r *Registry) All() []Backend
```
Returns all registered backends as a slice.

**Called from:** `Service.GetStats` to iterate over all backends for statistics collection.

**Returns:**
- `[]Backend` — All registered backends.

---

#### `(*Registry) Close`
```go
func (r *Registry) Close() error
```
Shuts down all registered backends by calling `Close()` on each. Individual close errors are silently ignored (logged but not collected). Always returns `nil`.

**Returns:**
- `error` — Always `nil`.

---

## File: `client.go`

### Types

#### `Client`
```go
type Client struct {
    fetchers []*fetcherConn
    mu       sync.RWMutex
    affinity   map[string]int
    affinityMu sync.RWMutex
    logger *slog.Logger
    config ClientConfig
    totalRequests  atomic.Int64
    affinityHits   atomic.Int64
    affinityMisses atomic.Int64
}
```
Provides access to the fetcher pool with repo-affinity routing. Storage nodes use this to request blobs from fetcher instances.

- `fetchers` — The connected fetcher instances.
- `affinity` — Maps `sourceKey` → preferred fetcher index, used for routing.
- `affinityMu` — Protects the affinity map.
- `totalRequests`, `affinityHits`, `affinityMisses` — Atomic counters for statistics.

---

#### `fetcherConn` (unexported)
```go
type fetcherConn struct {
    address string
    conn    *grpc.ClientConn
    client  pb.BlobFetcherClient
    sync    pb.RepoSyncWorkerClient
    cachedSources    map[string]bool
    cachedSourcesMu  sync.RWMutex
    lastSourceUpdate time.Time
    healthy    atomic.Bool
    lastError  time.Time
    errorCount atomic.Int64
}
```
Represents a connection to a single fetcher instance over gRPC.

- `client` — The `BlobFetcher` gRPC client.
- `sync` — The `RepoSyncWorker` gRPC client (for workspace sync operations).
- `cachedSources` — Set of cached source keys, periodically updated.
- `healthy` — Atomic flag for health state.
- `errorCount` / `lastError` — Error tracking for health decisions.

---

#### `ClientConfig`
```go
type ClientConfig struct {
    FetcherAddresses    []string
    ConnectionTimeout   time.Duration
    RequestTimeout      time.Duration
    AffinityWeight      float64
    HealthCheckInterval time.Duration
    MaxRetries          int
}
```
Configuration for the fetcher client.

- `FetcherAddresses` — gRPC addresses of fetcher instances.
- `ConnectionTimeout` — Timeout for establishing gRPC connections (default: 5s).
- `RequestTimeout` — Timeout for individual fetch requests (default: 5 min).
- `AffinityWeight` — Controls affinity influence in routing. 0.0 = pure round-robin, 1.0 = strict affinity (default: 0.8).
- `HealthCheckInterval` — Period for health checks (default: 10s).
- `MaxRetries` — Max retries per request across fetchers (default: 3).

---

#### `ClientStats`
```go
type ClientStats struct {
    TotalRequests   int64
    AffinityHits    int64
    AffinityMisses  int64
    TotalFetchers   int
    HealthyFetchers int
    AffinityEntries int
}
```
Holds aggregated client-side statistics.

---

#### `FetcherStats`
```go
type FetcherStats struct {
    Address, FetcherID          string
    Healthy                     bool
    UptimeSeconds               int64
    TotalRequests, CacheHits    int64
    CacheMisses                 int64
    CacheHitRate                float64
    CacheSizeBytes, CacheEntries int64
    ActiveFetches, QueuedPrefetches int64
    BytesFetched, BytesServed   int64
    SyncWorker                  SyncWorkerStatsInfo
    SourceStats                 map[string]SourceStatsInfo
    ErrorCount                  int64
    LastError                   string
    StorageHealthy              bool
    StorageError                string
}
```
Contains statistics for a single fetcher instance, queried from the fetcher's gRPC `GetStats` endpoint and enriched with client-side health info.

---

#### `SyncWorkerStatsInfo`
```go
type SyncWorkerStatsInfo struct {
    TotalJobs, ActiveJobs, CompletedJobs, FailedJobs           int64
    RefreshProbes, RefreshProbeFailures                        int64
    GitCacheEntries                                            int64
    PublishJobs, PublishedRepositories                         int64
    StagedBundles, StagedBundleBytes                           int64
    WorktreeBytes                                              int64
    BundleStageFailures                                        int64
}
```
Statistics for the sync worker component (workspace refresh/publish/source push).

---

#### `SourceStatsInfo`
```go
type SourceStatsInfo struct {
    Requests, Errors, BytesFetched    int64
    AvgLatencyMs                      float64
    CachedItems, CacheBytes           int64
    ObjectsStored, ObjectsRetrieved   int64
}
```
Per-source-type statistics aggregated from a fetcher instance.

---

#### `ClusterStats`
```go
type ClusterStats struct {
    TotalFetchers, HealthyFetchers       int
    TotalRequests, TotalCacheHits        int64
    TotalCacheMisses                     int64
    AggregatedHitRate                    float64
    TotalCacheSizeBytes, TotalCacheEntries int64
    TotalActiveFetches, TotalQueuedPrefetch int64
    TotalBytesFetched, TotalBytesServed  int64
    SyncWorker                           SyncWorkerStatsInfo
    Fetchers                             []FetcherStats
    ClientAffinityHits, ClientAffinityMisses int64
    ClientTotalRequests                  int64
    BlobStats        map[string]BlobBackendSum
    StorageBlobs     map[string]BlobBackendSum
    TotalBlobs, TotalErrors              int64
    LastError, StorageError              string
    StorageHealthy                       bool
    CloudObjectsStored, CloudObjectsRetrieved int64
}
```
Contains aggregated statistics for the entire fetcher cluster, gathered by querying all connected fetchers in parallel.

---

#### `BlobBackendSum`
```go
type BlobBackendSum struct {
    BlobCount int64
    BlobBytes int64
}
```
Aggregates blob counts and sizes per backend type across all fetcher instances.

---

#### `PrefetchFile`
```go
type PrefetchFile struct {
    SourceURL  string
    BlobHash   string
    FilePath   string
    Branch     string
    SourceType SourceType
    Confidence float32
}
```
Contains information for prefetching a single file. Used with `PrefetchSimple`.

---

#### `streamReader` (unexported)
```go
type streamReader struct {
    stream pb.BlobFetcher_FetchBlobClient
    cancel context.CancelFunc
    buf    []byte
    pos    int
}
```
Wraps a gRPC server-side streaming `FetchBlob` client as an `io.ReadCloser`. Buffers partial chunks between `Read` calls.

---

### Constants

```go
const maxUnaryBlobSize  = 64 * 1024 * 1024 // 64 MB
const maxStreamChunkSize = 64 * 1024 * 1024 // 64 MB
```

---

### Functions

#### `DefaultClientConfig`
```go
func DefaultClientConfig() ClientConfig
```
Returns a `ClientConfig` with sensible defaults:
- ConnectionTimeout: 5s
- RequestTimeout: 5 min
- AffinityWeight: 0.8
- HealthCheckInterval: 10s
- MaxRetries: 3

**Returns:**
- `ClientConfig` — Default configuration.

---

#### `NewClient`
```go
func NewClient(config ClientConfig, logger *slog.Logger) (*Client, error)
```
Creates a new fetcher client with repo-affinity routing. Connects to all configured fetcher addresses (skipping those that fail to connect). Starts two background goroutines: `healthCheckLoop` and `affinityUpdateLoop`.

**Parameters:**
- `config ClientConfig` — Client configuration.
- `logger *slog.Logger` — Structured logger.

**Returns:**
- `*Client` — The new client instance.
- `error` — Non-nil if no fetcher could be connected (i.e., zero fetcher connections established).

**Implementation details:**
- Connects to each address via `connectFetcher`.
- If zero connections succeed, returns an error.
- Background loops handle health checks and affinity map updates.

---

#### `(*Client) connectFetcher` (unexported)
```go
func (c *Client) connectFetcher(address string) (*fetcherConn, error)
```
Establishes a gRPC connection to a single fetcher at the given address. Uses insecure credentials and keepalive parameters (ping every 30s, 10s timeout). Immediately pings the fetcher via `GetStats` to verify responsiveness and set initial health state.

**Parameters:**
- `address string` — gRPC address (e.g., `"localhost:50051"`).

**Returns:**
- `*fetcherConn` — The connection wrapper with both `BlobFetcherClient` and `RepoSyncWorkerClient`.
- `error` — Non-nil if gRPC dial fails.

**Implementation details:**
- Initial health check: calls `GetStats` with a timeout equal to `ConnectionTimeout`.
- If the ping fails, marks the fetcher as unhealthy but still returns it for retry logic.

---

#### `(*Client) ProbeWorkspaceRefresh`
```go
func (c *Client) ProbeWorkspaceRefresh(ctx context.Context, req *pb.ProbeWorkspaceRefreshRequest) ([]*pb.RepoSyncProgress, error)
```
Asks one healthy fetcher's sync worker to probe remote heads for the provided repositories. Uses affinity-routed fetcher selection based on `req.GetWorkspaceId()`.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.ProbeWorkspaceRefreshRequest` — The probe request containing repositories to check.

**Returns:**
- `[]*pb.RepoSyncProgress` — Progress results for each probed repository.
- `error` — Non-nil if no healthy fetcher available or RPC fails.

**Implementation details:**
- Calls `fetcher.sync.ProbeWorkspaceRefresh`, which returns a server-side stream.
- Collects all `RepoSyncProgress` items from the stream.
- Records errors on failure, marks fetcher healthy on success.

---

#### `(*Client) StageWorkspaceBundle`
```go
func (c *Client) StageWorkspaceBundle(ctx context.Context, bundleID, workspaceID string, data []byte) (*pb.StageWorkspaceBundleResponse, error)
```
Pushes a workspace bundle into the selected fetcher sync worker's staging cache. Uses client-side streaming: sends data in 1 MB chunks, then calls `CloseAndRecv`.

**Parameters:**
- `ctx context.Context` — Request context.
- `bundleID string` — The bundle identifier.
- `workspaceID string` — The workspace identifier (used for affinity routing and sent with the bundle).
- `data []byte` — The serialized workspace bundle bytes.

**Returns:**
- `*pb.StageWorkspaceBundleResponse` — Response with bundle metadata.
- `error` — Non-nil if no healthy fetcher or RPC fails.

**Implementation details:**
- Chunk size: 1 MB (`1024 * 1024`).
- If `data` is empty, sends a single chunk with `IsLast: true`.
- Records errors on failure, marks fetcher healthy on success.

---

#### `(*Client) StageWorkspaceCommitBundle`
```go
func (c *Client) StageWorkspaceCommitBundle(ctx context.Context, bundleID, workspaceID string, data []byte) (*pb.StageWorkspaceBundleResponse, error)
```
Pushes a source commit bundle into the selected fetcher sync worker's staging cache. Identical streaming pattern to `StageWorkspaceBundle` but uses `fetcher.sync.StageWorkspaceCommitBundle`.

**Parameters:**
- `ctx context.Context` — Request context.
- `bundleID string` — The bundle identifier.
- `workspaceID string` — The workspace identifier.
- `data []byte` — The serialized source commit bundle bytes.

**Returns:**
- `*pb.StageWorkspaceBundleResponse` — Response with bundle metadata.
- `error` — Non-nil if no healthy fetcher or RPC fails.

---

#### `(*Client) StartWorkspacePublish`
```go
func (c *Client) StartWorkspacePublish(ctx context.Context, req *pb.StartWorkspacePublishRequest) ([]*pb.RepoSyncProgress, error)
```
Asks one healthy fetcher sync worker to publish the staged workspace bundle. Returns streaming progress results for each repository.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.StartWorkspacePublishRequest` — The publish request referencing the staged bundle.

**Returns:**
- `[]*pb.RepoSyncProgress` — Progress for each published repository.
- `error` — Non-nil if no healthy fetcher or RPC fails.

---

#### `(*Client) StartWorkspaceCommitPush`
```go
func (c *Client) StartWorkspaceCommitPush(ctx context.Context, req *pb.StartWorkspaceCommitPushRequest) ([]*pb.RepoSyncProgress, error)
```
Asks one healthy fetcher sync worker to push the staged source commit bundle. Returns streaming progress results.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.StartWorkspaceCommitPushRequest` — The push request.

**Returns:**
- `[]*pb.RepoSyncProgress` — Progress for each repository.
- `error` — Non-nil if no healthy fetcher or RPC fails.

---

#### `(*Client) DiscardWorkspaceBundle`
```go
func (c *Client) DiscardWorkspaceBundle(ctx context.Context, workspaceID, bundleID string) error
```
Removes a staged workspace bundle from the fetcher sync worker selected for the same workspace shard that staged it.

**Parameters:**
- `ctx context.Context` — Request context.
- `workspaceID string` — Used for affinity-based fetcher selection.
- `bundleID string` — The bundle to discard.

**Returns:**
- `error` — Non-nil if no healthy fetcher or RPC fails.

---

#### `(*Client) FetchBlob`
```go
func (c *Client) FetchBlob(ctx context.Context, req *FetchRequest, sourceType SourceType) ([]byte, error)
```
Fetches a blob using repo-affinity routing. On failure, retries up to `MaxRetries` times by cycling through healthy fetchers (excluding previously tried ones). Updates affinity on successful fetches.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *FetchRequest` — Fetch request with ContentID, SourceKey, SourceConfig, etc.
- `sourceType SourceType` — The backend source type.

**Returns:**
- `[]byte` — The fetched blob content.
- `error` — Non-nil if all fetch attempts fail.

**Implementation details:**
- Builds a `pb.FetchBlobRequest` proto from the request.
- Uses `selectFetcher` for initial selection (affinity-aware + hash-based fallback).
- Retry loop: tracks tried fetchers in a `map[*fetcherConn]bool` to avoid oscillating pairs.
- Calls `doFetch` for each attempt.
- Updates the affinity map on success.

---

#### `(*Client) doFetch` (unexported)
```go
func (c *Client) doFetch(ctx context.Context, fetcher *fetcherConn, req *pb.FetchBlobRequest) ([]byte, error)
```
Performs a single `FetchBlob` gRPC call to a specific fetcher. Collects all chunks from the server-side stream into a single byte slice.

**Parameters:**
- `ctx context.Context` — Context with timeout applied.
- `fetcher *fetcherConn` — The target fetcher.
- `req *pb.FetchBlobRequest` — The proto request.

**Returns:**
- `[]byte` — The complete blob data.
- `error` — Non-nil if the RPC or streaming fails.

**Implementation details:**
- Applies `RequestTimeout` via `context.WithTimeout`.
- Iterates `stream.Recv()` until `io.EOF`.
- Marks fetcher as healthy on success, records error on failure.

---

#### `(*Client) FetchBlobStream`
```go
func (c *Client) FetchBlobStream(ctx context.Context, req *FetchRequest, sourceType SourceType) (io.ReadCloser, error)
```
Fetches a blob and returns an `io.ReadCloser` for streaming consumption. Does NOT apply the request timeout to reads (caller controls lifecycle via `Close()`).

**Parameters:**
- `ctx context.Context` — Request context.
- `req *FetchRequest` — Fetch request.
- `sourceType SourceType` — The backend source type.

**Returns:**
- `io.ReadCloser` — A `streamReader` wrapping the gRPC stream.
- `error` — Non-nil if no healthy fetcher or RPC fails.

**Implementation details:**
- The `streamReader.Close()` method calls the context cancel function.
- Multiple `Read` calls drain the stream; buffering handles partial chunk copies.

---

#### `(*Client) Prefetch`
```go
func (c *Client) Prefetch(ctx context.Context, requests []*FetchRequest, sourceType SourceType) error
```
Queues blobs for background prefetching. Groups requests by their affinity-routed fetcher and sends `PrefetchBlobs` RPCs concurrently. Returns immediately without waiting for prefetch completion.

**Parameters:**
- `ctx context.Context` — Request context (unused for the prefetch RPCs themselves; a background context is used).
- `requests []*FetchRequest` — The blobs to prefetch.
- `sourceType SourceType` — The backend source type.

**Returns:**
- `error` — Always `nil`. Errors from individual prefetch RPCs are logged as warnings.

**Implementation details:**
- Groups requests by `selectFetcher(req.SourceKey)`.
- Each fetcher gets its own goroutine with a 30-second background context.
- Logs warnings on individual RPC failures.

---

#### `(*Client) selectFetcher` (unexported)
```go
func (c *Client) selectFetcher(sourceKey string) *fetcherConn
```
Chooses a fetcher using a two-tier strategy:
1. **Affinity check:** If a preferred index exists for `sourceKey` and that fetcher is healthy, return it (incrementing `affinityHits`).
2. **Hash-based fallback:** Uses FNV-32a hash of `sourceKey` modulo the number of healthy fetchers (incrementing `affinityMisses`).

If no healthy fetchers exist, falls back to all fetchers (even unhealthy ones).

**Parameters:**
- `sourceKey string` — The source key for affinity routing (typically a repo URL or storage ID).

**Returns:**
- `*fetcherConn` — The selected fetcher, or `nil` if none available.

---

#### `(*Client) selectFetcherExcluding` (unexported)
```go
func (c *Client) selectFetcherExcluding(sourceKey string, exclude *fetcherConn) *fetcherConn
```
Selects any healthy fetcher other than the excluded one. Simple linear scan. Used internally but largely superseded by `nextHealthyFetcherExcluding`.

**Parameters:**
- `sourceKey string` — Unused in the selection logic (declared for API consistency).
- `exclude *fetcherConn` — The fetcher to exclude.

**Returns:**
- `*fetcherConn` — The first healthy fetcher not equal to `exclude`, or `nil`.

---

#### `(*Client) nextHealthyFetcherExcluding` (unexported)
```go
func (c *Client) nextHealthyFetcherExcluding(sourceKey string, tried map[*fetcherConn]bool) *fetcherConn
```
Finds the next healthy fetcher that hasn't been tried yet, using hash-based start position and scanning forward. This avoids the oscillating-pair problem of `selectFetcherExcluding`.

**Parameters:**
- `sourceKey string` — Used for FNV-32a hash to determine start position.
- `tried map[*fetcherConn]bool` — Set of already-tried fetchers.

**Returns:**
- `*fetcherConn` — The next untried healthy fetcher, or `nil` if none remain.

**Implementation details:**
- Computes FNV-32a hash of `sourceKey` for consistent starting index.
- Scans all fetchers in order from start, skipping tried or unhealthy ones.

---

#### `(*Client) updateAffinity` (unexported)
```go
func (c *Client) updateAffinity(sourceKey string, fetcher *fetcherConn)
```
Records an affinity binding: maps `sourceKey` to the index of the given `fetcher` in the `c.fetchers` slice. Called after successful fetches to reinforce routing decisions.

**Parameters:**
- `sourceKey string` — The source key.
- `fetcher *fetcherConn` — The fetcher that successfully served the request.

**Implementation details:**
- Finds the index of `fetcher` in `c.fetchers` under read lock, then updates affinity map under write lock.

---

#### `(*Client) healthCheckLoop` (unexported)
```go
func (c *Client) healthCheckLoop()
```
Background goroutine started by `NewClient`. Every `HealthCheckInterval`, spawns a goroutine for each fetcher to check its health via `checkFetcherHealth`. Runs until the ticker is stopped (never, in practice, until process exit).

---

#### `(*Client) checkFetcherHealth` (unexported)
```go
func (c *Client) checkFetcherHealth(f *fetcherConn)
```
Pings a fetcher via `GetStats` with a 5-second timeout. Sets `healthy` to `true` on success, `false` on failure. Increments `errorCount` and records `lastError` on failures.

**Parameters:**
- `f *fetcherConn` — The fetcher to check.

---

#### `(*Client) affinityUpdateLoop` (unexported)
```go
func (c *Client) affinityUpdateLoop()
```
Background goroutine started by `NewClient`. Every 30 seconds, calls `updateCachedSources` to refresh the cached source lists from each fetcher and update affinity bindings.

---

#### `(*Client) updateCachedSources` (unexported)
```go
func (c *Client) updateCachedSources()
```
Collects cached source lists from all fetchers via `GetStats` (with `IncludeSourceStats: true`). Updates each fetcher's `cachedSources` map. If a source exists on only one fetcher, creates a strong affinity binding for it.

**Implementation details:**
- Queries all fetchers in sequence (not parallel) with a 5-second timeout each.
- Counts source occurrences across fetchers.
- Sources present on exactly one fetcher get strong affinity bindings.

---

#### `(*Client) GetStats`
```go
func (c *Client) GetStats() ClientStats
```
Returns client-side statistics: total requests, affinity hits/misses, fetcher counts, and affinity entries.

**Returns:**
- `ClientStats` — Aggregated client statistics.

---

#### `(*Client) Close`
```go
func (c *Client) Close() error
```
Closes all fetcher gRPC connections and clears the fetchers slice. Always returns `nil`.

**Returns:**
- `error` — Always `nil`.

---

#### `(*fetcherConn) recordError` (unexported)
```go
func (fc *fetcherConn) recordError()
```
Marks the fetcher connection as unhealthy, increments the error count, and records the current time as `lastError`. Called from various RPC methods on failure.

---

#### `(*streamReader) Read`
```go
func (r *streamReader) Read(p []byte) (int, error)
```
Reads blob data from the gRPC stream. First drains any buffered data, then receives the next chunk from the stream. If a chunk doesn't fully fit in `p`, the remainder is buffered for the next call.

**Parameters:**
- `p []byte` — Destination buffer.

**Returns:**
- `int` — Number of bytes read.
- `error` — `io.EOF` when stream ends, or the stream error.

---

#### `(*streamReader) Close`
```go
func (r *streamReader) Close() error
```
Cancels the underlying context, terminating the gRPC stream. Always returns `nil`.

---

#### `sourceTypeToProto` (unexported)
```go
func sourceTypeToProto(st SourceType) pb.SourceType
```
Converts an internal `SourceType` to the corresponding protobuf `SourceType` enum value. Returns `SOURCE_TYPE_UNKNOWN` for unrecognized types.

**Parameters:**
- `st SourceType` — The internal source type.

**Returns:**
- `pb.SourceType` — The protobuf source type enum.

---

#### `(*Client) HealthyFetchers`
```go
func (c *Client) HealthyFetchers() []string
```
Returns a sorted list of addresses of currently healthy fetchers.

**Returns:**
- `[]string` — Sorted healthy fetcher addresses.

---

#### `(*Client) AllFetchers`
```go
func (c *Client) AllFetchers() []string
```
Returns a sorted list of all configured fetcher addresses (regardless of health).

**Returns:**
- `[]string` — Sorted fetcher addresses.

---

#### `(*Client) FetchBlobSimple`
```go
func (c *Client) FetchBlobSimple(ctx context.Context, sourceURL, blobHash, filePath, branch string, sourceType SourceType) ([]byte, error)
```
Convenience wrapper around `FetchBlob`. Builds a `FetchRequest` from individual parameters and delegates to `FetchBlob`. Includes `[CONTENT_AUDIT]` logging for tracking content fetch requests, errors, and successes.

**Parameters:**
- `ctx context.Context` — Request context.
- `sourceURL string` — The repository URL.
- `blobHash string` — The blob/content hash to fetch.
- `filePath string` — The file display path.
- `branch string` — The git branch.
- `sourceType SourceType` — The source type.

**Returns:**
- `[]byte` — The fetched blob content.
- `error` — Non-nil if fetch fails.

---

#### `(*Client) CheckCacheSimple`
```go
func (c *Client) CheckCacheSimple(ctx context.Context, sourceURL, blobHash string) (bool, error)
```
Checks if a blob is in the prefetch cache of any healthy fetcher. Queries each healthy fetcher's `CheckCache` RPC until a cache hit is found.

**Parameters:**
- `ctx context.Context` — Request context.
- `sourceURL string` — Unused for the cache lookup itself.
- `blobHash string` — The blob hash to check.

**Returns:**
- `bool` — `true` if the blob is cached on any fetcher.
- `error` — Always `nil` (errors from individual fetchers are silently ignored).

---

#### `(*Client) PrefetchSimple`
```go
func (c *Client) PrefetchSimple(ctx context.Context, files []PrefetchFile) (int, error)
```
Sends prefetch requests using the simpler `PrefetchFile` struct. Groups files by their affinity-routed fetcher and sends `PrefetchBlobs` RPCs concurrently. Returns immediately.

**Parameters:**
- `ctx context.Context` — Request context (unused; background context is used for RPCs).
- `files []PrefetchFile` — Files to prefetch.

**Returns:**
- `int` — Number of files queued (sum of accepted counts from all fetcher RPCs).
- `error` — Always `nil`. Individual RPC errors are logged.

**Implementation details:**
- Priority is computed as `10 - int(Confidence * 10)` (higher confidence = lower priority number = higher actual priority).

---

#### `(*Client) GetClusterStats`
```go
func (c *Client) GetClusterStats(ctx context.Context, includeSourceStats bool) (*ClusterStats, error)
```
Retrieves statistics from all fetchers in the cluster by querying each in parallel via `GetStats`. Aggregates results into a `ClusterStats` struct.

**Parameters:**
- `ctx context.Context` — Request context.
- `includeSourceStats bool` — Whether to include per-source statistics.

**Returns:**
- `*ClusterStats` — Aggregated cluster statistics.
- `error` — Always `nil` (individual fetcher errors are reflected in per-fetcher stats, not as aggregate errors).

**Implementation details:**
- Queries all fetchers in parallel using goroutines and a buffered channel.
- Aggregates sync worker stats, blob stats, storage blobs, and cloud object counts.
- Computes `AggregatedHitRate` from total cache hits / (hits + misses).
- `StorageHealthy` is `true` only when all fetchers report `storage_healthy`.
- Sorts `Fetchers` by address for consistent output.

---

#### `(*Client) StoreBlob`
```go
func (c *Client) StoreBlob(ctx context.Context, blobHash string, content []byte) error
```
Stores a single blob on a fetcher instance via the unary `StoreBlob` RPC. Uses the `"blob"` key for affinity-based fetcher selection (which routes to the same fetcher chosen by hash of `"blob"`).

**Parameters:**
- `ctx context.Context` — Request context.
- `blobHash string` — The blob hash identifier.
- `content []byte` — The blob content.

**Returns:**
- `error` — Non-nil if no healthy fetcher, RPC fails, or the response indicates failure.

**Implementation details:**
- Checks `resp.Success` and returns `resp.ErrorMessage` on failure.

---

#### `(*Client) StoreBlobBatch`
```go
func (c *Client) StoreBlobBatch(ctx context.Context, blobs map[string][]byte) (stored int, failed int, err error)
```
Stores multiple blobs on a fetcher. For a single small blob (≤64 MB), uses the unary `StoreBlob` RPC. For multiple blobs or any oversized blob, uses the streaming `StoreBlobBatchStream` RPC with automatic chunking.

**Parameters:**
- `ctx context.Context` — Request context.
- `blobs map[string][]byte` — Map of blob hash → content.

**Returns:**
- `stored int` — Number of blobs successfully stored.
- `failed int` — Number of blobs that failed to store.
- `error` — Non-nil if the entire operation failed (with fallback to individual `StoreBlob` calls on streaming failure).

**Implementation details:**
- Streaming timeout: 30 minutes for large pushes.
- Fallback: If `StoreBlobBatchStream` is unavailable or fails, retries each blob individually via `StoreBlob`.
- Large blobs are chunked by `sendBlobChunked` (chunks up to 64 MB each).
- Logs info on successful batch storage with archive metadata.

---

#### `(*Client) sendBlobChunked` (unexported)
```go
func (c *Client) sendBlobChunked(stream pb.BlobFetcher_StoreBlobBatchStreamClient, hash string, content []byte) error
```
Sends a single blob over the streaming RPC, splitting it into multiple `StoreBlobEntry` messages if it exceeds `maxStreamChunkSize` (64 MB).

**Parameters:**
- `stream pb.BlobFetcher_StoreBlobBatchStreamClient` — The gRPC client stream.
- `hash string` — The blob hash.
- `content []byte` — The blob content.

**Returns:**
- `error` — Non-nil if a stream send fails.

**Implementation details:**
- Small blobs (≤64 MB): single message with `IsLast: true` and `TotalSize` set.
- Large blobs: multiple messages with incrementing `ChunkIndex`, `IsLast: true` on the final message, `TotalSize` set only on chunk 0.

---

#### `(*Client) DeleteBlobs`
```go
func (c *Client) DeleteBlobs(ctx context.Context, blobHashes []string, compact bool) (deleted int, notFound int, err error)
```
Asks a fetcher to remove blobs from its index. If `compact` is `true`, archive files that become empty are deleted from disk.

**Parameters:**
- `ctx context.Context` — Request context.
- `blobHashes []string` — The blob hashes to delete.
- `compact bool` — Whether to compact (delete empty archives).

**Returns:**
- `deleted int` — Number of blobs successfully deleted.
- `notFound int` — Number of blobs not found in the index.
- `error` — Non-nil if the RPC fails or the response indicates failure.

---

#### `(*Client) StoreArchive`
```go
func (c *Client) StoreArchive(ctx context.Context, storageID string, chunkIndex int, archiveData []byte) error
```
Streams a packager archive to a fetcher via the `StoreArchive` RPC. Sends data in 1 MB chunks using client-side streaming.

**Parameters:**
- `ctx context.Context` — Request context.
- `storageID string` — The storage identifier.
- `chunkIndex int` — The chunk index for the archive.
- `archiveData []byte` — The complete archive data.

**Returns:**
- `error` — Non-nil if no healthy fetcher, stream open/send/close fails, or response indicates failure.

**Implementation details:**
- Timeout: 10 minutes for large archives.
- Chunk size: 1 MB.
- Logs info on successful storage with fetcher address, storage ID, chunk index, total bytes, and files indexed.

---

## File: `metrics.go`

This file contains no functions — only package-level Prometheus metric variables registered via `promauto`. All metrics are in the `monofs_fetcher_*` namespace.

### Metric Variables

#### `fetcherBlobRequestsTotal`
```go
var fetcherBlobRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{...}, []string{"source_type"})
```
**Type:** `*prometheus.CounterVec`

Counts blob fetch requests handled by the fetcher service, labeled by `source_type` (`git`, `blob`, `s3`, `local`). Incremented in `Service.FetchBlob` and `Service.fetchSingleBlob`.

---

#### `fetcherBlobBytesTotal`
```go
var fetcherBlobBytesTotal = promauto.NewCounterVec(prometheus.CounterOpts{...}, []string{"source_type"})
```
**Type:** `*prometheus.CounterVec`

Counts bytes of blob content served, labeled by `source_type`. Incremented in `Service.FetchBlob` and `Service.fetchSingleBlob`.

---

#### `fetcherBlobErrorsTotal`
```go
var fetcherBlobErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{...}, []string{"source_type"})
```
**Type:** `*prometheus.CounterVec`

Counts fetch errors, labeled by `source_type`. Incremented in `Service.FetchBlob` and `Service.fetchSingleBlob`.

---

#### `fetcherPrefetchRequestsTotal`
```go
var fetcherPrefetchRequestsTotal = promauto.NewCounter(prometheus.CounterOpts{...})
```
**Type:** `prometheus.Counter`

Counts background prefetch requests enqueued. Incremented in `Service.PrefetchBlobs`.

---

#### `fetcherStoreArchiveBytesTotal`
```go
var fetcherStoreArchiveBytesTotal = promauto.NewCounter(prometheus.CounterOpts{...})
```
**Type:** `prometheus.Counter`

Counts bytes of packager archives stored on the fetcher. Incremented in `Service.StoreArchive`.

---

#### `fetcherStoreArchivesTotal`
```go
var fetcherStoreArchivesTotal = promauto.NewCounter(prometheus.CounterOpts{...})
```
**Type:** `prometheus.Counter`

Counts packager archive chunks stored on the fetcher. Incremented in `Service.StoreArchive`.

---

#### `fetcherStoreBlobBytesTotal`
```go
var fetcherStoreBlobBytesTotal = promauto.NewCounter(prometheus.CounterOpts{...})
```
**Type:** `prometheus.Counter`

Counts bytes from individual blob stores (non-archive). Incremented in `Service.StoreBlob`.

---

#### `fetcherGitSyncJobsTotal`
```go
var fetcherGitSyncJobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{...}, []string{"action", "result"})
```
**Type:** `*prometheus.CounterVec`

Counts Git sync jobs handled by the sync worker, labeled by `action` (`refresh`, `publish`, `source_push`) and `result` (`succeeded`, `failed`). Incremented in `ProbeWorkspaceRefresh`, `StartWorkspacePublish`, and `StartWorkspaceCommitPush`.

---

#### `fetcherGitSyncActiveJobs`
```go
var fetcherGitSyncActiveJobs = promauto.NewGauge(prometheus.GaugeOpts{...})
```
**Type:** `prometheus.Gauge`

Tracks currently active Git sync jobs. Incremented on job start, decremented on job completion (via `defer`).

---

#### `fetcherGitSyncDurationSeconds`
```go
var fetcherGitSyncDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{...}, []string{"action", "result"})
```
**Type:** `*prometheus.HistogramVec`

Records duration of Git sync jobs, labeled by `action` and `result`. Uses default Prometheus buckets. Observed in `ProbeWorkspaceRefresh`, `StartWorkspacePublish`, and `StartWorkspaceCommitPush`.

---

#### `fetcherGitSyncRemoteOpsTotal`
```go
var fetcherGitSyncRemoteOpsTotal = promauto.NewCounterVec(prometheus.CounterOpts{...}, []string{"op", "result"})
```
**Type:** `*prometheus.CounterVec`

Counts remote Git operations attempted by the sync worker, labeled by `op` (e.g., `probe_refresh`, `clone_publish`, `push_publish`, `stage_bundle`, `stage_commit_bundle`, `push_source_push`) and `result` (`started`, `succeeded`, `failed`).

---

#### `fetcherGitSyncConflictsTotal`
```go
var fetcherGitSyncConflictsTotal = promauto.NewCounterVec(prometheus.CounterOpts{...}, []string{"action", "reason"})
```
**Type:** `*prometheus.CounterVec`

Counts sync conflicts detected, labeled by `action` and `reason` (e.g., `non_fast_forward`, `base_commit_mismatch`).

---

#### `fetcherGitSyncBundleBytesTotal`
```go
var fetcherGitSyncBundleBytesTotal = promauto.NewCounter(prometheus.CounterOpts{...})
```
**Type:** `prometheus.Counter`

Counts total bytes staged into the sync worker bundle cache. Incremented in `StageWorkspaceBundle` and `StageWorkspaceCommitBundle`.

---

#### `fetcherGitSyncWorktreeBytes`
```go
var fetcherGitSyncWorktreeBytes = promauto.NewGauge(prometheus.GaugeOpts{...})
```
**Type:** `prometheus.Gauge`

Tracks current bytes consumed by active publish/source-push worktrees. Added to on worktree creation, subtracted on cleanup.

---

## File: `service.go`

### Types

#### `loggerAccessor` (unexported)
```go
type loggerAccessor interface {
    GetLogger() *slog.Logger
}
```
Interface for getting a logger from a writer. Not currently used in any implementation (declared for future use).

---

#### `Service`
```go
type Service struct {
    pb.UnimplementedBlobFetcherServer
    pb.UnimplementedRepoSyncWorkerServer

    fetcherID string
    registry  *Registry
    logger    *slog.Logger
    repoMgr   *monogit.RepoManager

    prefetchQueue chan *prefetchJob
    prefetchWg    sync.WaitGroup

    startTime             time.Time
    totalRequests         atomic.Int64
    bytesServed           atomic.Int64
    activeRequests        atomic.Int64
    syncTotalJobs         atomic.Int64
    syncActiveJobs        atomic.Int64
    syncDoneJobs          atomic.Int64
    syncFailedJobs        atomic.Int64
    syncProbes            atomic.Int64
    syncProbeFails        atomic.Int64
    syncPublishJobs       atomic.Int64
    syncPublishedRepos    atomic.Int64
    syncStagedBundles     atomic.Int64
    syncStagedBundleBytes atomic.Int64
    syncWorktreeBytes     atomic.Int64
    syncStageFails        atomic.Int64
    stagedBundlesMu       sync.RWMutex
    stagedBundles         map[string]*syncWorkerBundle

    config ServiceConfig
    ctx    context.Context
    cancel context.CancelFunc
}
```
Implements both `BlobFetcherServer` and `RepoSyncWorkerServer` gRPC service interfaces. The central server-side component for blob fetching and workspace sync operations.

- `repoMgr` — Git repository manager for sync operations; only created if `SyncRepoCacheDir` is configured.
- `prefetchQueue` — Buffered channel for background prefetch jobs.
- `sync*` atomics — Track various sync worker statistics.
- `stagedBundles` — In-memory cache of staged workspace/commit bundles, protected by `stagedBundlesMu`.

---

#### `ServiceConfig`
```go
type ServiceConfig struct {
    PrefetchWorkers      int
    PrefetchQueueSize    int
    MaxConcurrentFetches int
    StreamChunkSize      int
    SyncRepoCacheDir     string
}
```
Configuration for the fetcher service.

- `PrefetchWorkers` — Number of background prefetch workers (default: 4).
- `PrefetchQueueSize` — Max pending prefetch requests (default: 1000).
- `MaxConcurrentFetches` — Limits parallel fetch operations (default: 20).
- `StreamChunkSize` — Chunk size for streaming responses (default: 64 KB).
- `SyncRepoCacheDir` — Temp directory for Git mirrors used by refresh probes.

---

#### `prefetchJob` (unexported)
```go
type prefetchJob struct {
    req       *pb.FetchBlobRequest
    submitted time.Time
}
```
Represents a queued prefetch request with its submission timestamp.

---

### Functions

#### `DefaultServiceConfig`
```go
func DefaultServiceConfig() ServiceConfig
```
Returns a `ServiceConfig` with sensible defaults:
- PrefetchWorkers: 4
- PrefetchQueueSize: 1000
- MaxConcurrentFetches: 20
- StreamChunkSize: 64 KB
- SyncRepoCacheDir: empty (sync repo manager not created)

**Returns:**
- `ServiceConfig` — Default configuration.

---

#### `NewService`
```go
func NewService(fetcherID string, registry *Registry, config ServiceConfig, logger *slog.Logger) *Service
```
Creates a new fetcher service instance. If `config.SyncRepoCacheDir` is non-empty, creates a `monogit.RepoManager` in that directory. Starts `config.PrefetchWorkers` background goroutines (`prefetchWorker`).

**Parameters:**
- `fetcherID string` — Unique identifier for this fetcher instance.
- `registry *Registry` — The backend registry.
- `config ServiceConfig` — Service configuration.
- `logger *slog.Logger` — Structured logger.

**Returns:**
- `*Service` — The initialized service with background workers running.

**Implementation details:**
- Creates the `SyncRepoCacheDir` directory if it doesn't exist.
- Errors from `NewRepoManager` are silently ignored.

---

#### `(*Service) RegisterService`
```go
func (s *Service) RegisterService(server *grpc.Server)
```
Registers both `BlobFetcherServer` and `RepoSyncWorkerServer` with the given gRPC server.

**Parameters:**
- `server *grpc.Server` — The gRPC server to register on.

---

#### `(*Service) FetchBlob` (gRPC handler)
```go
func (s *Service) FetchBlob(req *pb.FetchBlobRequest, stream pb.BlobFetcher_FetchBlobServer) error
```
Handles synchronous blob fetch requests. Looks up the appropriate backend, calls `FetchBlobStream` on it, and streams the data back to the client in chunks of `StreamChunkSize` bytes.

**Parameters:**
- `req *pb.FetchBlobRequest` — The fetch request.
- `stream pb.BlobFetcher_FetchBlobServer` — The gRPC server stream to send chunks to.

**Returns:**
- `error` — Non-nil if the source type is unsupported or the fetch/stream fails.

**Implementation details:**
- Includes `[CONTENT_AUDIT]` debug logging at entry and completion.
- Increments `totalRequests`, `activeRequests`, `bytesServed`.
- Updates Prometheus metrics (`fetcherBlobRequestsTotal`, `fetcherBlobBytesTotal`, `fetcherBlobErrorsTotal`).

---

#### `(*Service) FetchBlobBatch` (gRPC handler)
```go
func (s *Service) FetchBlobBatch(req *pb.FetchBlobBatchRequest, stream pb.BlobFetcher_FetchBlobBatchServer) error
```
Handles batch fetch requests. Processes blobs concurrently with a semaphore-limited concurrency (capped at `MaxConcurrentFetches`). Streams results back as they complete.

**Parameters:**
- `req *pb.FetchBlobBatchRequest` — The batch request with multiple `Blobs` and optional `Concurrency` / `FailFast` settings.
- `stream pb.BlobFetcher_FetchBlobBatchServer` — The gRPC server stream.

**Returns:**
- `error` — Non-nil if streaming fails or `FailFast` is set and a blob fails.

**Implementation details:**
- Concurrency defaults to 4 if unspecified or zero.
- Results are streamed via a channel; the channel is closed after all goroutines finish.
- Sets `BatchComplete: true` on the last result.

---

#### `(*Service) fetchSingleBlob` (unexported)
```go
func (s *Service) fetchSingleBlob(ctx context.Context, req *pb.FetchBlobRequest) *pb.FetchBlobBatchResponse
```
Fetches a single blob for the batch handler. Uses `FetchBlob` (not streaming) on the backend to get the complete content. Records latency and updates metrics.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.FetchBlobRequest` — The fetch request.

**Returns:**
- `*pb.FetchBlobBatchResponse` — The result with data, size, cache status, and latency.

---

#### `(*Service) PrefetchBlobs` (gRPC handler)
```go
func (s *Service) PrefetchBlobs(ctx context.Context, req *pb.PrefetchRequest) (*pb.PrefetchResponse, error)
```
Queues blobs for background prefetching. For each blob, checks if the source is already cached/warmed up (via `backend.CachedSources()`); if so, counts it as `AlreadyCached`. Otherwise, enqueues it into `prefetchQueue`.

**Parameters:**
- `ctx context.Context` — Request context (unused for queueing).
- `req *pb.PrefetchRequest` — Contains the blobs to prefetch.

**Returns:**
- `*pb.PrefetchResponse` — Counts of `Accepted`, `AlreadyCached`, `Rejected`, and a `JobId`.
- `error` — Always `nil`.

**Implementation details:**
- Queue is non-blocking: if the channel is full, the blob is rejected.
- Generates a `JobId` based on the current Unix nano timestamp.

---

#### `(*Service) CheckCache` (gRPC handler)
```go
func (s *Service) CheckCache(ctx context.Context, req *pb.CheckCacheRequest) (*pb.CheckCacheResponse, error)
```
Checks if blobs are in the fetcher's cache. Uses a simple heuristic: a blob is considered potentially cached if its `ContentID` exists in the set of cached source keys from the backend.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.CheckCacheRequest` — Contains `ContentIds` and `SourceType`.

**Returns:**
- `*pb.CheckCacheResponse` — Map of `ContentID → bool` for cache status.
- `error` — Non-nil if source type is unsupported.

---

#### `(*Service) GetStats` (gRPC handler)
```go
func (s *Service) GetStats(ctx context.Context, req *pb.FetcherStatsRequest) (*pb.FetcherStatsResponse, error)
```
Returns comprehensive fetcher statistics including uptime, request counts, cache stats, source stats, and sync worker stats. If `IncludeSourceStats` is requested, collects per-backend and per-storage-ID statistics.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.FetcherStatsRequest` — Stats request with `IncludeSourceStats` and `IncludeCacheStats` flags.

**Returns:**
- `*pb.FetcherStatsResponse` — Detailed statistics.
- `error` — Always `nil`.

**Implementation details:**
- Iterates all backends to collect cache hits, misses, entries, and bytes.
- For `BlobBackend`, extracts per-storage-ID blob counts via `StorageStats()`.
- Computes `CacheHitRate` from hits / (hits + misses).
- Populates `SyncWorker` stats via `syncWorkerStats()`.
- Checks storage backend health via `BlobBackend.StorageHealth()`.

---

#### `(*Service) ProbeWorkspaceRefresh` (gRPC handler)
```go
func (s *Service) ProbeWorkspaceRefresh(req *pb.ProbeWorkspaceRefreshRequest, stream pb.RepoSyncWorker_ProbeWorkspaceRefreshServer) error
```
Probes remote heads for the provided repositories to detect if upstream branches have advanced. For each repository in the request, clones/opens the repo and compares the remote commit against the base commit.

**Parameters:**
- `req *pb.ProbeWorkspaceRefreshRequest` — Contains the list of repositories to probe.
- `stream pb.RepoSyncWorker_ProbeWorkspaceRefreshServer` — Server stream for sending progress updates.

**Returns:**
- `error` — Non-nil if streaming fails.

**Implementation details:**
- Tracks `syncTotalJobs`, `syncActiveJobs`, `syncProbes`, `syncProbeFails`.
- Updates Prometheus metrics for duration, remote ops, conflicts, and job results.
- Each repository is probed via `probeWorkspaceRepository`.

---

#### `(*Service) GetSyncWorkerStats` (gRPC handler)
```go
func (s *Service) GetSyncWorkerStats(ctx context.Context, req *pb.SyncWorkerStatsRequest) (*pb.SyncWorkerStatsResponse, error)
```
Returns sync worker statistics via `syncWorkerStats()`.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.SyncWorkerStatsRequest` — Unused.

**Returns:**
- `*pb.SyncWorkerStatsResponse` — Sync worker statistics.

---

#### `(*Service) syncWorkerStats` (unexported)
```go
func (s *Service) syncWorkerStats() *pb.SyncWorkerStats
```
Builds a `SyncWorkerStats` proto from the service's atomic counters. If `repoMgr` is configured, reads the sync repo cache directory to count git cache entries.

**Returns:**
- `*pb.SyncWorkerStats` — Current sync worker statistics.

---

#### `(*Service) probeWorkspaceRepository` (unexported)
```go
func (s *Service) probeWorkspaceRepository(ctx context.Context, repo *pb.WorkspaceRepositoryRef) *pb.RepoSyncProgress
```
Probes a single repository: clones or opens it via `repoMgr.CloneOrOpen`, resolves the remote branch commit, and compares it against the base commit.

**Parameters:**
- `ctx context.Context` — Request context.
- `repo *pb.WorkspaceRepositoryRef` — The repository reference.

**Returns:**
- `*pb.RepoSyncProgress` — The probe result with status and remote commit.

**Implementation details:**
- Status `UNCHANGED` if remote commit equals base commit.
- Status `ADVANCED` if remote has new commits.
- Returns `TRANSIENT_ERROR` if `BaseCommit` is empty.
- Requires `repoMgr` to be configured.

---

#### `resolveRepoBranchCommit` (unexported)
```go
func resolveRepoBranchCommit(repo *gogit.Repository, branch string) (string, error)
```
Resolves the commit hash for a branch in a Git repository. First tries the local branch reference, then falls back to the `origin/<branch>` remote reference.

**Parameters:**
- `repo *gogit.Repository` — The Git repository.
- `branch string` — The branch name.

**Returns:**
- `string` — The commit hash string.
- `error` — Non-nil if the reference cannot be resolved.

---

#### `mapRepoSyncError` (unexported)
```go
func mapRepoSyncError(err error) pb.RepoSyncStatus
```
Maps a Go error to the corresponding `RepoSyncStatus` enum value. Uses error message pattern matching:
- `ErrReferenceNotFound` → `MISSING_BRANCH`
- "authentication"/"authorization"/"access denied" → `AUTH_FAILED`
- "not found" + "reference" → `MISSING_BRANCH`
- "timeout"/"temporary"/"connection" → `TRANSIENT_ERROR`
- Everything else → `FAILED`

**Parameters:**
- `err error` — The error to map.

**Returns:**
- `pb.RepoSyncStatus` — The mapped status.

---

#### `(*Service) prefetchWorker` (unexported)
```go
func (s *Service) prefetchWorker(id int)
```
Background goroutine that continuously reads from `prefetchQueue` and processes jobs via `processPrefetchJob`. Exits when the service context is cancelled.

**Parameters:**
- `id int` — Worker identifier (for logging).

---

#### `(*Service) processPrefetchJob` (unexported)
```go
func (s *Service) processPrefetchJob(job *prefetchJob)
```
Processes a single prefetch job: looks up the backend, warms up the source (clone repo, download module, etc.), then fetches the blob to ensure it is cached. Each step has a 5-minute timeout.

**Parameters:**
- `job *prefetchJob` — The prefetch job to process.

**Implementation details:**
- Calls `backend.Warmup()` to prepare the source.
- Then calls `backend.FetchBlob()` to cache the content.
- Logs warnings on failures, debug on success.

---

#### `(*Service) Close`
```go
func (s *Service) Close() error
```
Shuts down the service: cancels the context, closes the prefetch queue, and waits for all prefetch workers to finish. Always returns `nil`.

**Returns:**
- `error` — Always `nil`.

---

#### `protoToSourceType` (unexported)
```go
func protoToSourceType(pt pb.SourceType) SourceType
```
Converts a protobuf `SourceType` enum to the internal `SourceType` type. Returns `SourceTypeUnknown` for unrecognized values.

**Parameters:**
- `pt pb.SourceType` — The protobuf source type.

**Returns:**
- `SourceType` — The internal source type.

---

#### `getSourceKey` (unexported)
```go
func getSourceKey(req *pb.FetchBlobRequest) string
```
Extracts a source key from a fetch request for affinity routing and caching. Uses `StorageId` if provided; otherwise falls back to `repo_url` for Git sources, `storage_id` for blob sources, or `ContentId` as a last resort.

**Parameters:**
- `req *pb.FetchBlobRequest` — The fetch request.

**Returns:**
- `string` — The source key.

---

#### `(*Service) StoreBlob` (gRPC handler)
```go
func (s *Service) StoreBlob(ctx context.Context, req *pb.StoreBlobRequest) (*pb.StoreBlobResponse, error)
```
Stores a single blob on the fetcher. Validates that `blob_hash` and `content` are non-empty, then delegates to `BlobBackend.StoreBlob()`. Updates `fetcherStoreBlobBytesTotal` metric.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.StoreBlobRequest` — Contains `BlobHash` and `Content`.

**Returns:**
- `*pb.StoreBlobResponse` — Success/failure with size on success.

**Implementation details:**
- Requires the `SourceTypeBlob` backend to be registered.
- Type-asserts the backend to `*blob.BlobBackend`.

---

#### `(*Service) StoreBlobBatchStream` (gRPC handler)
```go
func (s *Service) StoreBlobBatchStream(stream pb.BlobFetcher_StoreBlobBatchStreamServer) error
```
Receives a stream of `StoreBlobEntry` messages and packs them into archive(s) via `BlobBackend.NewStoreBlobBatchWriter()`. Supports chunked blobs (multiple messages sharing the same `blob_hash` with incrementing `chunk_index`) that are reassembled before archiving.

**Parameters:**
- `stream pb.BlobFetcher_StoreBlobBatchStreamServer` — The gRPC server stream.

**Returns:**
- `error` — Sent via `SendAndClose` with the batch result.

**Implementation details:**
- Creates a `StoreBlobBatchWriter` for archive packing.
- Handles two code paths: single-message blobs (common case) and multi-chunk blobs.
- On stream receive error, seals the writer and reports partial results.
- Flushes any incomplete chunked blob at the end of the stream.
- Calls `writer.Finish()` to finalize archives.
- Returns success/failure counts, archive bytes, and archives created.

---

#### `(*Service) DeleteBlobs` (gRPC handler)
```go
func (s *Service) DeleteBlobs(ctx context.Context, req *pb.DeleteBlobsRequest) (*pb.DeleteBlobsResponse, error)
```
Removes blobs from the fetcher's index and optionally cleans up empty archive files. Delegates to `BlobBackend.DeleteBlobs()`.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.DeleteBlobsRequest` — Contains `BlobHashes` and `Compact` flag.

**Returns:**
- `*pb.DeleteBlobsResponse` — Counts of deleted, not found, and archives removed.

---

#### `(*Service) StoreArchive` (gRPC handler)
```go
func (s *Service) StoreArchive(stream pb.BlobFetcher_StoreArchiveServer) error
```
Receives a packager archive stream, collects all chunks into a buffer, then stores the complete archive via `BlobBackend.StoreArchive()`. Updates `fetcherStoreArchiveBytesTotal` and `fetcherStoreArchivesTotal` metrics.

**Parameters:**
- `stream pb.BlobFetcher_StoreArchiveServer` — The gRPC server stream.

**Returns:**
- `error` — Sent via `SendAndClose` with `TotalBytes` and `FilesIndexed` on success.

**Implementation details:**
- Reads all chunks until `IsLast` or `io.EOF`.
- Extracts `StorageId` and `ChunkIndex` from the first chunk.
- Returns an error if no `storage_id` is provided.

---

## File: `workspace_publish.go`

### Types

#### `syncWorkerBundle` (unexported)
```go
type syncWorkerBundle struct {
    bundleID     string
    workspaceID  string
    data         []byte
    bundle       *workspacebundle.Bundle
    commitBundle *workspacebundle.SourceCommitBundle
    createdAt    time.Time
    expiresAt    time.Time
}
```
Represents a staged workspace or commit bundle in the fetcher's in-memory cache. Contains:
- The parsed `Bundle` (for workspace publishes) or `SourceCommitBundle` (for source pushes).
- The raw binary data.
- Creation and expiration timestamps.

---

### Constants

```go
const stagedWorkspaceBundleTTL = 30 * time.Minute
```
Time-to-live for staged bundles. Bundles older than this are silently discarded on access.

---

### Functions

#### `(*Service) StageWorkspaceBundle` (gRPC handler)
```go
func (s *Service) StageWorkspaceBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.StageWorkspaceBundleResponse]) error
```
Receives a workspace bundle via client-side streaming. Validates bundle/workspace ID consistency, parses the bundle with `workspacebundle.Parse()`, and stores it as a `syncWorkerBundle` in the service's staged bundles map.

**Parameters:**
- `stream grpc.ClientStreamingServer[...]` — The gRPC client stream.

**Returns:**
- `error` — Sent via `SendAndClose` with bundle metadata on success.

**Implementation details:**
- Enforces that `workspace_id` and `bundle_id` do not change mid-stream.
- Validates that the parsed bundle's `WorkspaceID` matches the stream's `WorkspaceID`.
- Updates metrics: `fetcherGitSyncBundleBytesTotal`, `fetcherGitSyncRemoteOpsTotal`.
- Sets TTL expiration 30 minutes from creation.

---

#### `(*Service) StartWorkspacePublish` (gRPC handler)
```go
func (s *Service) StartWorkspacePublish(req *pb.StartWorkspacePublishRequest, stream pb.RepoSyncWorker_StartWorkspacePublishServer) error
```
Publishes a staged workspace bundle: iterates over each repository in the bundle, clones a worktree, applies file operations, stages, commits, and pushes to the target branch. Streams progress for each repository.

**Parameters:**
- `req *pb.StartWorkspacePublishRequest` — References the staged bundle and provides author/commit details.
- `stream pb.RepoSyncWorker_StartWorkspacePublishServer` — Server stream for progress updates.

**Returns:**
- `error` — Non-nil if streaming fails.

**Implementation details:**
- Retrieves the staged bundle; fails if not found or workspace ID mismatch.
- For each repository, calls `publishBundleRepository`.
- Returns `nil` even on partial failures (failure is reflected in progress statuses).
- Tracks published repositories count, conflict reasons, and job status.

---

#### `(*Service) DiscardWorkspaceBundle` (gRPC handler)
```go
func (s *Service) DiscardWorkspaceBundle(ctx context.Context, req *pb.DiscardWorkspaceBundleRequest) (*pb.DiscardWorkspaceBundleResponse, error)
```
Removes a staged workspace bundle from the cache.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.DiscardWorkspaceBundleRequest` — Contains the `BundleId` to discard.

**Returns:**
- `*pb.DiscardWorkspaceBundleResponse` — Success/failure with message.

---

#### `(*Service) publishBundleRepository` (unexported)
```go
func (s *Service) publishBundleRepository(ctx context.Context, req *pb.StartWorkspacePublishRequest, repo workspacebundle.RepositoryBundle) *pb.RepoSyncProgress
```
Publishes a single repository from a workspace bundle. The full pipeline:
1. Clone the repository into a temp worktree.
2. Verify the remote HEAD matches the expected base commit (conflict detection).
3. Checkout the target branch.
4. Apply all bundle operations (upsert, delete, mkdir, etc.) to the worktree.
5. Stage and commit changes.
6. Push to the target branch.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.StartWorkspacePublishRequest` — The publish request with author/commit info.
- `repo workspacebundle.RepositoryBundle` — The repository bundle to publish.

**Returns:**
- `*pb.RepoSyncProgress` — Progress with status, remote commit, pushed commit, and conflict info.

**Implementation details:**
- Cleanup: `defer os.RemoveAll(worktreeRoot)` and `defer` tracking of worktree bytes.
- Target branch determined by `choosePublishTargetBranch`.
- Uses `stageWorktreeChanges` to detect and stage modifications.
- Conflict detection: compares remote HEAD against `repo.BaseCommit`.

---

#### `(*Service) clonePublishWorktree` (unexported)
```go
func (s *Service) clonePublishWorktree(ctx context.Context, repo workspacebundle.RepositoryBundle) (string, error)
```
Clones a repository for publishing into a temporary directory. Uses `gogit.PlainCloneContext` with `SingleBranch: true` on the repository's branch. If `SyncRepoCacheDir` is configured, creates the temp dir there.

**Parameters:**
- `ctx context.Context` — Request context.
- `repo workspacebundle.RepositoryBundle` — The repository info.

**Returns:**
- `string` — Path to the cloned worktree directory.
- `error` — Non-nil if clone fails.

---

#### `checkoutPublishBranch` (unexported)
```go
func checkoutPublishBranch(wt *gogit.Worktree, targetBranch string) error
```
Checks out the target branch in a worktree. If the branch doesn't exist, creates it (`Create: true`). If `targetBranch` is empty, returns `nil` (no-op).

**Parameters:**
- `wt *gogit.Worktree` — The git worktree.
- `targetBranch string` — The branch name to check out.

**Returns:**
- `error` — Non-nil if checkout fails.

---

#### `applyRepositoryOperations` (unexported)
```go
func applyRepositoryOperations(root string, operations []workspacebundle.Operation) error
```
Applies a list of file-system operations to a worktree root directory. Supports:
- `OperationUpsert` — Write file (creates parent dirs if needed, default mode 0644).
- `OperationDelete` — Remove file/directory.
- `OperationMkdir` — Create directory (default mode 0755).
- `OperationRmdir` — Remove directory (ignores if not exists).
- `OperationSymlink` — Create symlink (removes target first if exists).
- `OperationRename` — Rename/move file.
- `OperationChmod` — Change file mode (default 0644).

**Parameters:**
- `root string` — The worktree root directory.
- `operations []workspacebundle.Operation` — The operations to apply.

**Returns:**
- `error` — Non-nil if any operation fails.

**Implementation details:**
- All paths are cleaned and joined to `root` via `filepath.Join`/`filepath.Clean`.

---

#### `stageWorktreeChanges` (unexported)
```go
func stageWorktreeChanges(wt *gogit.Worktree) (bool, error)
```
Examines the worktree status and stages all changes. Handles both additions and deletions. Returns whether any changes were found.

**Parameters:**
- `wt *gogit.Worktree` — The git worktree.

**Returns:**
- `bool` — `true` if there were any changes to stage.
- `error` — Non-nil if status check or staging fails.

**Implementation details:**
- Deleted files are staged via `wt.Remove()`.
- All other modified/new files are staged via `wt.Add()`.
- Skips `Unmodified` files in both `Worktree` and `Staging` fields.

---

#### `choosePublishTargetBranch` (unexported)
```go
func choosePublishTargetBranch(strategy, workspaceID, jobID string, repo workspacebundle.RepositoryBundle) string
```
Determines the target branch name for publishing based on the strategy:
- `""` or `"direct"` → Uses the repository's branch directly.
- `"workspace_branch"` → `monofs/<workspaceID>/<shortJobID>` (sanitized).
- `"per_repo_branch"` → `monofs/<workspaceID>/<storageID>` (sanitized).

**Parameters:**
- `strategy string` — The branch naming strategy.
- `workspaceID string` — The workspace identifier.
- `jobID string` — The job identifier.
- `repo workspacebundle.RepositoryBundle` — The repository.

**Returns:**
- `string` — The target branch name.

---

#### `sanitizeBranchName` (unexported)
```go
func sanitizeBranchName(name string) string
```
Sanitizes a string for use as a Git branch name: replaces special characters (` `, `..`, `~`, `^`, `:`, `?`, `*`, `[`, `\`) with `-`, trims leading/trailing `/`, `.`, `-`, and falls back to `"monofs/publish"` if empty.

**Parameters:**
- `name string` — The raw branch name candidate.

**Returns:**
- `string` — The sanitized branch name.

---

#### `shortJobID` (unexported)
```go
func shortJobID(jobID string) string
```
Truncates a job ID to its last 12 characters for use in branch names. Returns the full ID if it's 12 characters or shorter.

**Parameters:**
- `jobID string` — The full job ID.

**Returns:**
- `string` — The truncated job ID.

---

#### `defaultFileMode` (unexported)
```go
func defaultFileMode(mode int64) int64
```
Returns `mode` if non-zero, otherwise returns `0644`.

**Parameters:**
- `mode int64` — The requested file mode.

**Returns:**
- `int64` — The effective file mode.

---

#### `defaultDirMode` (unexported)
```go
func defaultDirMode(mode int64) int64
```
Returns `mode` if non-zero, otherwise returns `0755`.

**Parameters:**
- `mode int64` — The requested directory mode.

**Returns:**
- `int64` — The effective directory mode.

---

#### `directorySize` (unexported)
```go
func directorySize(root string) (int64, error)
```
Calculates the total size of all files in a directory tree (non-recursive from directory perspective; walks all entries but only counts file sizes).

**Parameters:**
- `root string` — The directory path.

**Returns:**
- `int64` — Total size in bytes.
- `error` — Non-nil if the walk encounters an error.

---

#### `(*Service) putStagedBundle` (unexported)
```go
func (s *Service) putStagedBundle(entry *syncWorkerBundle)
```
Stores a bundle in the staged bundles map. If a bundle with the same ID already exists, adjusts the `syncStagedBundleBytes` stat accordingly.

**Parameters:**
- `entry *syncWorkerBundle` — The bundle to store.

---

#### `(*Service) getStagedBundle` (unexported)
```go
func (s *Service) getStagedBundle(bundleID string) *syncWorkerBundle
```
Retrieves a staged bundle by ID. Returns `nil` if not found or if the bundle has expired (in which case it is also removed).

**Parameters:**
- `bundleID string` — The bundle identifier.

**Returns:**
- `*syncWorkerBundle` — The bundle, or `nil`.

---

#### `(*Service) removeStagedBundle` (unexported)
```go
func (s *Service) removeStagedBundle(bundleID string) bool
```
Removes a staged bundle by ID. Updates `syncStagedBundles` and `syncStagedBundleBytes` stats.

**Parameters:**
- `bundleID string` — The bundle identifier.

**Returns:**
- `bool` — `true` if the bundle existed and was removed, `false` otherwise.

---

#### `mapPublishError` (unexported)
```go
func mapPublishError(err error) (pb.RepoSyncStatus, string)
```
Maps a Go error to a `RepoSyncStatus` and conflict reason string. Pattern matching:
- "authentication"/"authorization"/"access denied" → `AUTH_FAILED`, `"auth_failed"`
- "non-fast-forward"/"already exists"/"rejected"/"base commit" → `CONFLICT`, `"non_fast_forward"`
- "timeout"/"connection"/"transport" → `TRANSIENT_ERROR`, `"transport_error"`
- Everything else → `FAILED`, `""`

**Parameters:**
- `err error` — The error to map.

**Returns:**
- `pb.RepoSyncStatus` — The mapped status.
- `string` — The conflict reason (empty if not applicable).

---

## File: `workspace_source_push.go`

### Types

#### `sourcePushRepositoryPlan` (unexported)
```go
type sourcePushRepositoryPlan struct {
    repo                workspacebundle.SourceCommitRepository
    operations          []workspacebundle.Operation
    commitIDs           []string
    commitCount         int
    latestCommitMessage string
    latestAuthorName    string
    latestAuthorEmail   string
}
```
Aggregates all operations from multiple local commits targeting the same repository into a single plan for squashed (non-preserve mode) source push. Contains the repository reference, merged operations, commit IDs, count, and the latest commit's message/author.

---

#### `repoWorktree` (unexported)
```go
type repoWorktree struct {
    root       string
    repoHandle *gogit.Repository
    wt         *gogit.Worktree
}
```
Holds state for a repository worktree during preserve-mode source push. The worktree is opened once and reused across multiple commits for the same repository.

---

### Constants

```go
const sourcePushModePreserve = "preserve"
```
Push mode that preserves individual local commits as separate remote commits.

---

### Functions

#### `(*Service) StageWorkspaceCommitBundle` (gRPC handler)
```go
func (s *Service) StageWorkspaceCommitBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.StageWorkspaceBundleResponse]) error
```
Receives a source commit bundle via client-side streaming. Parses the bundle with `workspacebundle.ParseSourceCommitBundle()`, validates workspace ID consistency, and stores it as a `syncWorkerBundle` in the staged bundles map.

**Parameters:**
- `stream grpc.ClientStreamingServer[...]` — The gRPC client stream.

**Returns:**
- `error` — Sent via `SendAndClose` with bundle metadata on success.

**Implementation details:**
- Same validation pattern as `StageWorkspaceBundle` (ID consistency, non-empty, parse).
- Populates `commitBundle` field of `syncWorkerBundle` instead of `bundle`.
- Reports `RepositoryCount` from `bundle.RepositoryRefs()`.

---

#### `(*Service) StartWorkspaceCommitPush` (gRPC handler)
```go
func (s *Service) StartWorkspaceCommitPush(req *pb.StartWorkspaceCommitPushRequest, stream pb.RepoSyncWorker_StartWorkspaceCommitPushServer) error
```
Pushes a staged source commit bundle. Two modes based on `SourcePushMode`:
- **Squash mode** (default): Aggregates all commits into per-repository plans via `sourcePushRepositoryPlans`, then pushes each plan as a single commit.
- **Preserve mode** (`"preserve"`): Iterates over commits in order, pushing each as a separate commit via `pushSourceCommitsPreserve`.

**Parameters:**
- `req *pb.StartWorkspaceCommitPushRequest` — References the staged bundle and provides push configuration.
- `stream pb.RepoSyncWorker_StartWorkspaceCommitPushServer` — Server stream for progress updates.

**Returns:**
- `error` — Non-nil if streaming fails.

**Implementation details:**
- Fails if the staged bundle is not found or mis-matched workspace ID.
- Updates Prometheus metrics for duration, conflicts, and job results.
- Returns `nil` even on partial failures.

---

#### `sourcePushRepositoryPlans` (unexported)
```go
func sourcePushRepositoryPlans(bundle *workspacebundle.SourceCommitBundle) []sourcePushRepositoryPlan
```
Creates per-repository aggregation plans from a source commit bundle. Groups all commits by repository (keyed by `StorageID`, falling back to `DisplayPath`), merges operations, and collects commit metadata. Returns plans sorted by `DisplayPath`.

**Parameters:**
- `bundle *workspacebundle.SourceCommitBundle` — The parsed source commit bundle.

**Returns:**
- `[]sourcePushRepositoryPlan` — Sorted plans, one per unique repository.

---

#### `(*Service) pushSourceCommitRepository` (unexported)
```go
func (s *Service) pushSourceCommitRepository(ctx context.Context, req *pb.StartWorkspaceCommitPushRequest, plan sourcePushRepositoryPlan) *pb.RepoSyncProgress
```
Pushes a single repository from a squash plan. The pipeline mirrors `publishBundleRepository`:
1. Clone the repository.
2. Verify base commit match.
3. Checkout target branch.
4. Apply merged operations.
5. Stage, commit (with squashed commit message), and push.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.StartWorkspaceCommitPushRequest` — The push request.
- `plan sourcePushRepositoryPlan` — The aggregated repository plan.

**Returns:**
- `*pb.RepoSyncProgress` — Progress with status and pushed commit.

**Implementation details:**
- Target branch: `chooseSourcePushTargetBranch` from `req.LogicalBranch`.
- Commit message: `sourcePushCommitMessage(plan)` (squashed format).
- Uses `plan.latestAuthorName`/`plan.latestAuthorEmail` for commit author.

---

#### `cloneSourcePushWorktree` (unexported)
```go
func cloneSourcePushWorktree(ctx context.Context, repo workspacebundle.SourceCommitRepository) (string, error)
```
Clones a repository for source push into a temporary directory. Uses `gogit.PlainCloneContext` with `SingleBranch: true`. Unlike `clonePublishWorktree`, defaults to the system temp directory.

**Parameters:**
- `ctx context.Context` — Request context.
- `repo workspacebundle.SourceCommitRepository` — The repository info.

**Returns:**
- `string` — Path to the cloned worktree.
- `error` — Non-nil if clone fails.

---

#### `chooseSourcePushTargetBranch` (unexported)
```go
func chooseSourcePushTargetBranch(logicalBranch string, repo workspacebundle.SourceCommitRepository) string
```
Determines the target branch for source push. If `logicalBranch` is non-empty, sanitizes it; otherwise uses the repository's default branch.

**Parameters:**
- `logicalBranch string` — The requested branch name.
- `repo workspacebundle.SourceCommitRepository` — The repository.

**Returns:**
- `string` — The target branch name.

---

#### `sourcePushCommitMessage` (unexported)
```go
func sourcePushCommitMessage(plan sourcePushRepositoryPlan) string
```
Builds the commit message for a squashed source push:
- Single commit with a message → uses that message.
- Single commit without a message → `"MonoFS source push <DisplayPath>"`.
- Multiple commits with a message → `"<message>\n\nMonoFS source push squashed N local commits for <DisplayPath>"`.
- Multiple commits without a message → `"MonoFS source push <DisplayPath> (N local commits)"`.

**Parameters:**
- `plan sourcePushRepositoryPlan` — The aggregated plan.

**Returns:**
- `string` — The commit message.

---

#### `(*Service) pushSourceCommitsPreserve` (unexported)
```go
func (s *Service) pushSourceCommitsPreserve(ctx context.Context, req *pb.StartWorkspaceCommitPushRequest, bundleEntry *syncWorkerBundle, stream pb.RepoSyncWorker_StartWorkspaceCommitPushServer) error
```
Handles preserve-mode source push: sorts commits by `CreatedAtUnix` (falling back to `ID` for ties), then iterates over each commit, pushing its repository operations as individual commits. Reuses worktrees for the same repository across commits.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.StartWorkspaceCommitPushRequest` — The push request.
- `bundleEntry *syncWorkerBundle` — The staged bundle.
- `stream pb.RepoSyncWorker_StartWorkspaceCommitPushServer` — Progress stream.

**Returns:**
- `error` — Non-nil if streaming fails, or `nil` on completion.

**Implementation details:**
- Maintains a `repoWorktrees` map to reuse worktrees per repository.
- Cleans up all worktrees on exit via `defer`.
- Stops on conflicts, divergences, or failures (does not continue to next commit).
- Each commit is processed by `pushSinglePreserveCommit`.

---

#### `(*Service) pushSinglePreserveCommit` (unexported)
```go
func (s *Service) pushSinglePreserveCommit(ctx context.Context, req *pb.StartWorkspaceCommitPushRequest, rw *repoWorktree, commitIdx int, commit workspacebundle.SourceCommit, repo workspacebundle.SourceCommitRepository, workspaceID string, totalCommits int) *pb.RepoSyncProgress
```
Pushes a single local commit in preserve mode. On first call for a worktree, opens the repository, verifies the base commit, and checks out the target branch. Applies the commit's operations, stages, commits (with metadata footer), and pushes.

**Parameters:**
- `ctx context.Context` — Request context.
- `req *pb.StartWorkspaceCommitPushRequest` — The push request.
- `rw *repoWorktree` — The reusable worktree state.
- `commitIdx int` — Index of this commit in the sorted list.
- `commit workspacebundle.SourceCommit` — The commit to push.
- `repo workspacebundle.SourceCommitRepository` — The repository target.
- `workspaceID string` — The workspace identifier.
- `totalCommits int` — Total number of commits in the bundle.

**Returns:**
- `*pb.RepoSyncProgress` — Progress with local commit ID and index.

**Implementation details:**
- Lazy initialization: only opens the repo and worktree on the first call for a given `repoWorktree`.
- Commit message includes metadata footer: `MonoFS-Local-Commit`, `MonoFS-Workspace`, `MonoFS-Job`.
- Reuses the same worktree and pushes after each commit (incremental).

---

#### `buildPreserveCommitMessage` (unexported)
```go
func buildPreserveCommitMessage(commit workspacebundle.SourceCommit, jobID, workspaceID string) string
```
Builds a commit message for preserve mode. Appends metadata footer to the original commit message (or a generated one if the original is empty):
```
<original message>

MonoFS-Local-Commit: <commit.ID>
MonoFS-Workspace: <workspaceID>
MonoFS-Job: <jobID>
```

**Parameters:**
- `commit workspacebundle.SourceCommit` — The source commit.
- `jobID string` — The job identifier.
- `workspaceID string` — The workspace identifier.

**Returns:**
- `string` — The augmented commit message.

---

#### `repoRefFromSourceCommitRepo` (unexported)
```go
func repoRefFromSourceCommitRepo(repo workspacebundle.SourceCommitRepository) *pb.WorkspaceRepositoryRef
```
Converts a `workspacebundle.SourceCommitRepository` to the protobuf `WorkspaceRepositoryRef` type. Used when building progress messages for preserve-mode pushes.

**Parameters:**
- `repo workspacebundle.SourceCommitRepository` — The source commit repository.

**Returns:**
- `*pb.WorkspaceRepositoryRef` — The protobuf reference.
