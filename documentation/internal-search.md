# internal/search — Documentation

Package `search` provides code search functionality built on Zoekt (from Sourcegraph). It exposes gRPC handlers for repository indexing and full-text code search, backed by a job-queue worker pool and NutsDB persistence.

---

## File: `service.go`

Core types and the `Service` struct that ties everything together.

### Types

#### `Config`
```go
type Config struct {
    IndexDir   string
    CacheDir   string
    Workers    int
    QueueSize  int
    RouterAddr string
    Logger     *slog.Logger
}
```
Service configuration. `IndexDir` is the Zoekt index directory; `CacheDir` is for temporary git clones during indexing. `RouterAddr` optionally connects to MonoFS storage nodes to read files directly. `Workers` controls concurrent indexing goroutines; `QueueSize` is the buffered channel capacity for jobs.

#### `Job`
```go
type Job struct {
    ID           string
    StorageID    string
    DisplayPath  string
    RepoURL      string
    Branch       string
    Status       pb.IndexStatus
    Progress     float32
    FilesCount   int64
    IndexSize    int64
    QueuedAt     time.Time
    StartedAt    time.Time
    CompletedAt  time.Time
    ErrorMessage string
}
```
Represents an indexing job. Persisted to NutsDB bucket `bucketJobs`. `Status` is a proto enum (`INDEX_STATUS_QUEUED`, `INDEX_STATUS_INDEXING`, `INDEX_STATUS_READY`, `INDEX_STATUS_ERROR`).

#### `RepoMeta`
```go
type RepoMeta struct {
    StorageID   string
    DisplayPath string
    RepoURL     string
    Branch      string
    FilesCount  int64
    IndexSize   int64
    LastIndexed time.Time
}
```
Repository metadata persisted to NutsDB bucket `bucketRepos`. Written after a successful indexing run. Used by `ListIndexes` and `GetIndexStatus` to report repository state.

#### `ServiceStats`
```go
type ServiceStats struct {
    SearchesTotal       int64
    SearchDurationTotal int64
    StartedAt           time.Time
    JobsQueued          int64
    JobsCompleted       int64
    JobsFailed          int64
    JobsRejected        int64
}
```
Aggregated statistics persisted to bucket `bucketStats`. Loaded on startup, saved on shutdown.

#### `Service`
```go
type Service struct {
    pb.UnimplementedMonoFSSearchServer
    mu         sync.RWMutex
    indexDir   string
    cacheDir   string
    db         *nutsdb.DB
    indexer    *Indexer
    logger     *slog.Logger
    jobQueue   chan *Job
    activeJobs sync.Map
    jobsWg     sync.WaitGroup
    stats      ServiceStats
    searchCount atomic.Int64
    stopChan   chan struct{}
    workers    int
}
```
The top-level gRPC service. Owns the `Indexer`, a NutsDB database for state persistence, a buffered job channel, and a set of indexing workers.

---

### Functions

#### `DefaultConfig()`
```go
func DefaultConfig() Config
```
**Signature:** `func DefaultConfig() Config`

**What it does:** Returns a `Config` with sensible defaults:
- `IndexDir`: `/data/index`
- `CacheDir`: `/data/cache`
- `Workers`: `2`
- `QueueSize`: `100`
- Logger: `slog.Default()`

**Callers:** Called by external code (likely `cmd/` or test harnesses) that want reasonable defaults before overriding.

**Implementation details:** Pure factory function; no side effects.

---

#### `NewService(cfg Config)`
```go
func NewService(cfg Config) (*Service, error)
```

**What it does:** Creates and initialises the full search service. Performs:
1. Creates `IndexDir` and `CacheDir` directories.
2. Opens a NutsDB database at `<IndexDir>/state.db` with 8 MB segments.
3. Creates three buckets: `jobs`, `repos`, `stats`.
4. Optionally connects to a MonoFS cluster via `client.NewShardedClient` (with 5 retries, 2s/4s/6s/8s/10s backoff, 10 s connect timeout per attempt). If connection fails, falls back to direct git clone / go mod download.
5. Creates the `Indexer` via `NewIndexer()`.
6. Loads stats from the DB.
7. Loads repo mappings (`DisplayPath` → `StorageID`) into the indexer so search results include StorageIDs.
8. Starts `cfg.Workers` goroutines running `worker()`.
9. Calls `restorePendingJobs()` to re-queue any jobs that were `QUEUED` or `INDEXING` at shutdown.

**Callers:** External startup code (e.g. `main.go`, test setup).

**Parameters:**
- `cfg Config`: Full service configuration.

**Returns:**
- `*Service`: Initialised service (workers already running).
- `error`: If directory creation, DB open, bucket creation, MonoFS connection, or indexer creation fails.

**Implementation details:** The MonoFS connection uses `client.ShardedClientConfig` with `ClientID: "search-indexer"`, refresh interval 60 s, RPC timeout 5 min, internal addresses only. Resources are cleaned up on failure (MonoFS client closed, DB closed).

---

#### `(*Service) Close()`
```go
func (s *Service) Close() error
```

**What it does:** Gracefully shuts down the service:
1. Closes `stopChan`, signalling all workers to stop.
2. Waits for all in-flight jobs to finish (`jobsWg.Wait()`).
3. Saves current stats to DB.
4. Closes the indexer.
5. Closes the NutsDB database.

**Callers:** External shutdown code.

**Returns:** `error` from `db.Close()` (or nil).

**Implementation details:** Does not drain the job queue — jobs already queued are abandoned (channel closed, workers exit). Only in-flight jobs complete.

---

#### `(*Service) worker(id int)`
```go
func (s *Service) worker(id int)
```

**What it does:** Long-running goroutine that dequeues jobs from `s.jobQueue` and processes them. Blocks on `select` for either `stopChan` (exit) or a job from the channel (calls `processJob`).

**Callers:** Started by `NewService` in a `go` statement for each of `cfg.Workers` goroutines.

**Parameters:**
- `id int`: Worker identifier for logging.

**Implementation details:** Runs in an infinite loop. Exits only when `stopChan` is closed. No return value.

---

#### `(*Service) processJob(job *Job)`
```go
func (s *Service) processJob(job *Job)
```

**What it does:** Executes a single indexing job:
1. Adds to `jobsWg` (deferred Done).
2. Sets job status to `INDEX_STATUS_INDEXING`, records `StartedAt`, stores in `activeJobs` sync.Map, persists via `saveJob`.
3. Creates a 30-minute context timeout.
4. Calls `s.indexer.IndexRepository()` with the job's `StorageID`, `DisplayPath`, `RepoURL`, and `Branch`.
5. On error: sets status to `INDEX_STATUS_ERROR`, stores error message, increments `JobsFailed`.
6. On success: sets status to `INDEX_STATUS_READY`, records file count and index size, sets progress to 1.0, increments `JobsCompleted`, saves `RepoMeta` via `saveRepoMeta`.
7. Marks `CompletedAt`, removes from `activeJobs`, persists final state via `saveJob`.

**Callers:** Called exclusively by `worker()` when a job is dequeued.

**Parameters:**
- `job *Job`: The job to process.

**Implementation details:** Locks `s.mu` only when updating stats counters. Uses `defer s.jobsWg.Done()` to ensure wait-group tracking even on panic.

---

#### `(*Service) saveJob(job *Job)`
```go
func (s *Service) saveJob(job *Job)
```

**What it does:** Marshals a `Job` to JSON and writes it to the `bucketJobs` NutsDB bucket, keyed by `job.StorageID`.

**Callers:** `IndexRepository` handler (on queue and on rejection), `processJob` (start, completion, error), `RebuildIndex`, `RebuildAllIndexes`, `restorePendingJobs`.

**Parameters:**
- `job *Job`: Job to persist.

**Implementation details:** Uses `json.Marshal`. Logs errors but does not return them (fire-and-forget persistence).

---

#### `(*Service) loadJob(storageID string)`
```go
func (s *Service) loadJob(storageID string) (*Job, error)
```

**What it does:** Reads a `Job` from the `bucketJobs` NutsDB bucket by `StorageID` and unmarshals from JSON.

**Callers:** `GetIndexStatus`, `RebuildIndex`, `ListIndexes`.

**Parameters:**
- `storageID string`: The storage ID key.

**Returns:**
- `*Job`: The deserialised job, or nil.
- `error`: If the key is not found or JSON unmarshal fails.

---

#### `(*Service) saveRepoMeta(meta *RepoMeta)`
```go
func (s *Service) saveRepoMeta(meta *RepoMeta)
```

**What it does:** Marshals `RepoMeta` to JSON and writes to `bucketRepos`, keyed by `meta.StorageID`.

**Callers:** `processJob` (on successful indexing).

**Parameters:**
- `meta *RepoMeta`: Repository metadata to persist.

**Implementation details:** Fire-and-forget; logs errors but does not return them.

---

#### `(*Service) loadRepoMeta(storageID string)`
```go
func (s *Service) loadRepoMeta(storageID string) (*RepoMeta, error)
```

**What it does:** Reads `RepoMeta` from `bucketRepos` by `StorageID`, unmarshals from JSON.

**Callers:** `GetIndexStatus`, `RebuildIndex`.

**Parameters:**
- `storageID string`: Lookup key.

**Returns:**
- `*RepoMeta`: On success.
- `error`: On not found or unmarshal failure.

---

#### `(*Service) loadStats()`
```go
func (s *Service) loadStats()
```

**What it does:** Reads the `"stats"` key from `bucketStats`, unmarshals into `s.stats`. If not found (fresh DB), resets to default `ServiceStats` with `StartedAt: time.Now()`.

**Callers:** `NewService` during initialisation.

**Implementation details:** No error returned; on failure silently resets stats.

---

#### `(*Service) loadRepoMappings()`
```go
func (s *Service) loadRepoMappings()
```

**What it does:** Iterates over all entries in `bucketRepos`, unmarshals each `RepoMeta`, and calls `s.indexer.RegisterStorageMapping(meta.DisplayPath, meta.StorageID)` for each. This ensures the indexer can map display paths back to storage IDs in search results.

**Callers:** `NewService` during initialisation.

**Implementation details:** Skips entries that fail to unmarshal. Logs the count of loaded mappings if > 0.

---

#### `(*Service) saveStats()`
```go
func (s *Service) saveStats()
```

**What it does:** Marshals `s.stats` to JSON and writes to `bucketStats` under the key `"stats"`.

**Callers:** `Close()` during shutdown.

**Implementation details:** Silently ignores marshal or DB write errors.

---

#### `(*Service) restorePendingJobs()`
```go
func (s *Service) restorePendingJobs()
```

**What it does:** On service startup, reads all jobs from `bucketJobs`. For any job with status `QUEUED` or `INDEXING` (meaning it was interrupted by a previous shutdown), resets the status to `QUEUED` and re-enqueues on `s.jobQueue`. If the queue is full, it skips with a warning log.

**Callers:** `NewService` during initialisation.

**Implementation details:** Uses non-blocking `select` with default to avoid blocking on a full queue. Creates a new `Job` value (not pointer to loop variable) for each enqueue.

---

#### `generateJobID(storageID string)`
```go
func generateJobID(storageID string) string
```

**What it does:** Creates a unique job ID by taking the first 16 characters of the `storageID` (truncated) and appending the current Unix nano timestamp: `"<prefix16>-<nanos>"`.

**Callers:** `IndexRepository`, `RebuildIndex`, `RebuildAllIndexes` handlers.

**Parameters:**
- `storageID string`: Repository storage ID used as prefix.

**Returns:**
- `string`: Unique job ID.

**Implementation details:** Not guaranteed globally unique across restarts or clock changes, but practically unique within a process lifetime due to nanosecond monotonic clock.

---

## File: `handlers.go`

gRPC service handlers implementing the `MonoFSSearchServer` interface. All are methods on `*Service`.

### Functions

#### `(*Service) IndexRepository(ctx, req)`
```go
func (s *Service) IndexRepository(ctx context.Context, req *pb.IndexRequest) (*pb.IndexResponse, error)
```

**What it does:** Handles the `IndexRepository` RPC. Creates a `Job` from the request, attempts to enqueue it on `s.jobQueue`:
- **Success:** returns `Queued: true` with the job ID.
- **Queue full:** marks the job as `INDEX_STATUS_ERROR` with message "Job queue is full - please retry later", saves it, returns `Queued: false`.

Updates `s.stats.JobsQueued` or `s.stats.JobsRejected`.

**Callers:** gRPC server dispatch.

**Parameters:**
- `ctx context.Context`: gRPC context (unused beyond logging).
- `req *pb.IndexRequest`: Contains `StorageId`, `DisplayPath`, `Source` (repo URL), `Ref` (branch).

**Returns:**
- `*pb.IndexResponse`: `Queued` bool, `Message` string, `JobId` string.
- `error`: nil (this handler never returns gRPC errors — rejection is communicated in the response).

**Implementation details:** Uses a `select` with `default` for non-blocking send to the channel. Always saves the job (even rejected ones) so the UI can display failure state.

---

#### `(*Service) Search(ctx, req)`
```go
func (s *Service) Search(ctx context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error)
```

**What it does:** Handles the `Search` RPC. Delegates to `s.indexer.Search()`, then:
- Increments `searchCount` atomic counter and updates `s.stats.SearchesTotal` / `SearchDurationTotal`.
- Converts `SearchResults` to protobuf `pb.SearchResult` slices, including `MatchRange` start/end offsets.
- Returns `TotalMatches`, `FilesSearched`, `DurationMs`, `Truncated`.

**Callers:** gRPC server dispatch.

**Parameters:**
- `ctx context.Context`: Passed to `indexer.Search`.
- `req *pb.SearchRequest`: `Query`, `StorageId` (optional filter), `MaxResults`, `CaseSensitive`, `Regex`, `FilePatterns`.

**Returns:**
- `*pb.SearchResponse`: Search results with before/after context, match ranges, and aggregates.
- `error`: From indexer on query parse or search failure.

**Implementation details:** Type-converts `int` ↔ `int32` between domain types and protobuf. `Duration` in response is `time.Since(start)` — total handler time, not just Zoekt search time.

---

#### `(*Service) GetIndexStatus(ctx, req)`
```go
func (s *Service) GetIndexStatus(ctx context.Context, req *pb.IndexStatusRequest) (*pb.IndexStatusResponse, error)
```

**What it does:** Returns the indexing status for a given `StorageId`. Checks three sources in order:
1. `activeJobs` sync.Map — if an active job exists, returns its live status and progress.
2. Persisted `Job` via `loadJob` — if found, returns the persisted state.
3. `s.indexer.IndexExists()` — if an index shard exists on disk but no job record, loads `RepoMeta` and reports `INDEX_STATUS_READY`.
4. Otherwise returns `INDEX_STATUS_NOT_FOUND`.

**Callers:** gRPC server dispatch.

**Parameters:**
- `ctx context.Context`: Unused.
- `req *pb.IndexStatusRequest`: Contains `StorageId`.

**Returns:**
- `*pb.IndexStatusResponse`: Status, progress, file count, index size, timestamps.
- `error`: nil.

**Implementation details:** Time fields formatted as RFC 3339 strings. The `sync.Map.Range` is used with early-return (`return false`) pattern for active job lookup.

---

#### `(*Service) ListIndexes(ctx, req)`
```go
func (s *Service) ListIndexes(ctx context.Context, req *pb.ListIndexesRequest) (*pb.ListIndexesResponse, error)
```

**What it does:** Returns all indexed repositories and their status. Merges data from three sources:
1. **`bucketRepos`** — all successfully indexed repositories (from `RepoMeta`), cross-referenced with `activeJobs` for live status/progress and `bucketJobs` for ERROR state.
2. **`activeJobs`** — queued/active jobs not yet in `bucketRepos`.
3. **`bucketJobs`** — ERROR jobs that never had successful metadata.

Deduplicates by `StorageID` across the three sources.

**Callers:** gRPC server dispatch.

**Parameters:**
- `ctx context.Context`: Unused.
- `req *pb.ListIndexesRequest`: Unused (empty message in current proto).

**Returns:**
- `*pb.ListIndexesResponse`: Slice of `IndexStatusResponse`.
- `error`: nil.

**Implementation details:** Three sequential passes: repos view, activeJobs scan, jobs view. Each pass checks for duplicates via linear scan of the in-progress `indexes` slice.

---

#### `(*Service) RebuildIndex(ctx, req)`
```go
func (s *Service) RebuildIndex(ctx context.Context, req *pb.RebuildIndexRequest) (*pb.RebuildIndexResponse, error)
```

**What it does:** Re-indexes a repository. Looks up existing metadata from `loadRepoMeta` first, falls back to `loadJob` (for repos that failed and have no metadata). If `req.Force` is true, deletes the existing index via `s.indexer.DeleteIndex()`. Creates a new job and enqueues it.

**Callers:** gRPC server dispatch.

**Parameters:**
- `ctx context.Context`: Unused.
- `req *pb.RebuildIndexRequest`: `StorageId` (required), `Force` (delete existing index first).

**Returns:**
- `*pb.RebuildIndexResponse`: `Queued`, `Message`, `JobId`.
- `error`: nil (not-found returns `Queued: false` in response).

**Implementation details:** On metadata miss followed by job miss, returns `Queued: false, Message: "Repository not found"`.

---

#### `(*Service) RebuildAllIndexes(ctx, req)`
```go
func (s *Service) RebuildAllIndexes(ctx context.Context, req *pb.RebuildAllIndexesRequest) (*pb.RebuildAllIndexesResponse, error)

```

**What it does:** Re-indexes all repositories in `bucketRepos`. Iterates over all `RepoMeta` entries, optionally deletes existing indexes if `req.Force`, creates jobs, and enqueues them. Stops if the queue fills up (non-blocking send with default).

**Callers:** gRPC server dispatch.

**Parameters:**
- `ctx context.Context`: Unused.
- `req *pb.RebuildAllIndexesRequest`: `Force` flag.

**Returns:**
- `*pb.RebuildAllIndexesResponse`: `QueuedCount` (how many actually enqueued), `Message`.
- `error`: nil.

**Implementation details:** Operates inside a NutsDB view transaction. Early-returns from the transaction if the queue is full (no more jobs added).

---

#### `(*Service) DeleteIndex(ctx, req)`
```go
func (s *Service) DeleteIndex(ctx context.Context, req *pb.DeleteIndexRequest) (*pb.DeleteIndexResponse, error)
```

**What it does:** Removes an index. Calls `s.indexer.DeleteIndex()` to remove Zoekt shard files, then deletes the corresponding entries from `bucketRepos` and `bucketJobs` in NutsDB.

**Callers:** gRPC server dispatch.

**Parameters:**
- `ctx context.Context`: Unused.
- `req *pb.DeleteIndexRequest`: `StorageId`.

**Returns:**
- `*pb.DeleteIndexResponse`: `Success` bool, `Message` string.
- `error`: nil (errors are reflected in response).

**Implementation details:** Even if `deleteIndex` fails, it still attempts to clean up NutsDB entries. The DB update is non-transactional (two separate `Delete` calls in one `Update`).

---

#### `(*Service) GetStats(ctx, req)`
```go
func (s *Service) GetStats(ctx context.Context, req *pb.StatsRequest) (*pb.StatsResponse, error)
```

**What it does:** Returns aggregate service statistics. Reads `bucketRepos` to compute `totalIndexes`, `totalFiles`, `totalSize`. Reads `bucketJobs` to count ERROR jobs. Counts `activeJobs`. Computes average search duration. Computes uptime as `time.Since(stats.StartedAt)`.

**Callers:** gRPC server dispatch.

**Parameters:**
- `ctx context.Context`: Unused.
- `req *pb.StatsRequest`: Unused (empty message).

**Returns:**
- `*pb.StatsResponse`: Counts, queue length, active jobs, search metrics, uptime.
- `error`: nil.

**Implementation details:** Uses `RLock`/`RUnlock` for reading `s.stats`. `JobsFailed` is the sum of `stats.JobsFailed` (runtime) + `errorCount` (persisted ERROR jobs).

---

## File: `indexer.go`

Type definitions for search/index types and the `Indexer` struct implementing Zoekt-based indexing and searching.

### Types

#### `Indexer`
```go
type Indexer struct {
    mu              sync.RWMutex
    indexDir        string
    cacheDir        string
    searcher        zoekt.Streamer
    logger          *slog.Logger
    pathToStorageID map[string]string
    monofsClient    client.MonoFSClient
}
```
Manages Zoekt index creation and search. `searcher` is an in-process Zoekt searcher backed by on-disk shards in `indexDir`. `pathToStorageID` maps `DisplayPath` → `StorageID` for enriching search results. `monofsClient` is an optional client for fetching files from MonoFS storage nodes.

#### `IndexRequest`
```go
type IndexRequest struct {
    StorageID   string
    DisplayPath string
    RepoURL     string
    Ref         string
}
```
Parameters passed to `IndexRepository`.

#### `IndexResult`
```go
type IndexResult struct {
    FilesIndexed   int64
    IndexSizeBytes int64
    Duration       time.Duration
}
```
Returned by indexing operations.

#### `SearchRequest`
```go
type SearchRequest struct {
    Query         string
    StorageID     string
    MaxResults    int
    CaseSensitive bool
    Regex         bool
    FilePatterns  []string
}
```
Domain-level search parameters. `StorageID` is optional; when set, search is scoped to one repository.

#### `SearchResult`
```go
type SearchResult struct {
    StorageID     string
    DisplayPath   string
    FilePath      string
    LineNumber    int
    LineContent   string
    Matches       []MatchRange
    BeforeContext string
    AfterContext  string
}
```
A single match within a file.

#### `MatchRange`
```go
type MatchRange struct {
    Start int
    End   int
}
```
Byte offsets of a match within `LineContent`.

#### `SearchResults`
```go
type SearchResults struct {
    Results       []SearchResult
    TotalMatches  int64
    FilesSearched int64
    Duration      time.Duration
    Truncated     bool
}
```
Aggregate search result. `Truncated` is true when results hit `MaxResults` limit.

#### `IndexLocalRequest`
```go
type IndexLocalRequest struct {
    StorageID   string
    DisplayPath string
    SourceDir   string
    Ref         string
}
```
Parameters for `IndexLocalDir` — indexes a local filesystem directory instead of fetching remotely.

---

### Functions

#### `globToRegex(glob string)`
```go
func globToRegex(glob string) string
```

**What it does:** Converts a glob pattern to an anchored regex for filename matching.
- Simple extension filter like `.go` → `.*\.go$`.
- `*` → `[^/]*` (matches non-slash chars).
- `**` → `.*` (matches everything including slashes).
- `?` → `[^/]`.
- Regex metacharacters (`.`, `+`, `^`, `$`, `(`, `)`, `[`, `]`, `{`, `}`, `|`, `\`) are escaped.
- Always appends `$` to anchor to end of filename.

**Callers:** `Indexer.Search()` — used to build file pattern filters from user-supplied globs.

**Parameters:**
- `glob string`: Glob pattern (e.g. `"*.go"`, `"test_*.py"`).

**Returns:**
- `string`: Regex pattern string.

**Implementation details:** Does NOT compile the regex — returns a raw pattern string. Caller must pass to `syntax.Parse`. Handles `**` by detecting two consecutive asterisks.

---

#### `NewIndexer(indexDir, cacheDir string, monofsClient client.MonoFSClient, logger *slog.Logger)`
```go
func NewIndexer(indexDir, cacheDir string, monofsClient client.MonoFSClient, logger *slog.Logger) (*Indexer, error)
```

**What it does:** Creates a new `Indexer`. Creates `indexDir` and `cacheDir` directories if they don't exist. Creates a Zoekt `DirectorySearcher` that reads from `indexDir`.

**Callers:** `NewService` in `service.go`.

**Parameters:**
- `indexDir string`: Where Zoekt index shards are stored/read.
- `cacheDir string`: Where git clones are cached during external indexing.
- `monofsClient client.MonoFSClient`: Optional MonoFS client (nil if not using MonoFS cluster).
- `logger *slog.Logger`: Structured logger.

**Returns:**
- `*Indexer`: Initialised indexer.
- `error`: If directory creation or searcher creation fails.

**Implementation details:** The `searcher` is an in-process `search.DirectorySearcher` that watches `indexDir` for new shard files.

---

#### `(*Indexer) RegisterStorageMapping(displayPath, storageID string)`
```go
func (i *Indexer) RegisterStorageMapping(displayPath, storageID string)
```

**What it does:** Stores a `DisplayPath` → `StorageID` mapping in the indexer's internal map. Used so that search results (which report `DisplayPath` from Zoekt) can be enriched with the corresponding `StorageID`.

**Callers:** `loadRepoMappings()` in `service.go` (on startup), `IndexRepository` and `IndexLocalDir` in `indexer.go` (when indexing a new repo).

**Parameters:**
- `displayPath string`: The display/repository name.
- `storageID string`: The storage node ID.

**Implementation details:** Thread-safe (uses `mu.Lock()`).

---

#### `(*Indexer) Close()`
```go
func (i *Indexer) Close() error
```

**What it does:** Closes the underlying Zoekt searcher.

**Callers:** `Service.Close()` in `service.go`.

**Returns:** `nil` (always).

**Implementation details:** Thread-safe. If `searcher` is nil (somehow), no-op.

---

#### `(*Indexer) IndexRepository(ctx context.Context, req IndexRequest)`
```go
func (i *Indexer) IndexRepository(ctx context.Context, req IndexRequest) (*IndexResult, error)
```

**What it does:** Indexes a repository. Stores the `DisplayPath` → `StorageID` mapping. If `monofsClient` is configured, delegates to `indexFromMonoFS`. Otherwise delegates to `indexFromExternal` (git clone + index).

**Callers:** `processJob` in `service.go`.

**Parameters:**
- `ctx context.Context`: Cancellation context.
- `req IndexRequest`: Contains `StorageID`, `DisplayPath`, `RepoURL`, `Ref`.

**Returns:**
- `*IndexResult`: Files indexed, index size on disk, duration.
- `error`: On any failure.

**Implementation details:** The branching on `monofsClient != nil` is the key design decision — preferred path uses the internal MonoFS cluster; fallback uses git clone over the network.

---

#### `(*Indexer) indexFromMonoFS(ctx context.Context, req IndexRequest, start time.Time)`
```go
func (i *Indexer) indexFromMonoFS(ctx context.Context, req IndexRequest, start time.Time) (*IndexResult, error)
```

**What it does:** Indexes a repository by fetching all files from MonoFS storage nodes. Creates a Zoekt `index.Builder` with repository metadata, walks the tree via `walkMonoFSTree`, then calls `builder.Finish()`. After completion, walks `indexDir` to calculate the total size of shard files matching the escaped display path.

**Callers:** `IndexRepository` when `monofsClient` is configured.

**Parameters:**
- `ctx context.Context`: Passed to `walkMonoFSTree`.
- `req IndexRequest`: Indexing parameters.
- `start time.Time`: Timestamp for duration calculation (passed from `IndexRepository`).

**Returns:**
- `*IndexResult`: Indexing results.
- `error`: On builder creation or walk failure.

**Implementation details:** Uses `url.QueryEscape` to escape the display path for shard filename matching. Calls `builder.Finish()` on error path too (cleanup). The Zoekt `Repository` has `Branches` set to the requested ref with version `"HEAD"`.

---

#### `(*Indexer) walkMonoFSTree(ctx context.Context, repoPath, subPath, branch string, builder *index.Builder, filesIndexed *int64)`
```go
func (i *Indexer) walkMonoFSTree(ctx context.Context, repoPath, subPath, branch string, builder *index.Builder, filesIndexed *int64) error
```

**What it does:** Recursively walks a repository tree via the MonoFS client. For each directory entry:
- **Directories:** Skips `.git`, recurses into others. Subdirectory walk errors are logged and skipped (does not abort the whole index).
- **Files:** Skips binary files (by extension via `isBinaryFile`), reads content (up to 1 MB via `monofsClient.Read`), skips files ≥ 1 MB (truncated), skips binary content (null bytes via `isBinaryContent`), then adds to the Zoekt builder as an `index.Document`.

**Callers:** `indexFromMonoFS`.

**Parameters:**
- `ctx context.Context`: Passed to MonoFS client calls.
- `repoPath string`: Root path in MonoFS (the display path).
- `subPath string`: Relative subdirectory, empty for root.
- `branch string`: Branch name for the Zoekt document.
- `builder *index.Builder`: Zoekt index builder to add files to.
- `filesIndexed *int64`: Pointer to counter, incremented for each indexed file.

**Returns:**
- `error`: On directory read failure; sub-path errors are handled gracefully.

**Implementation details:** Directory mode bit check: `(entry.Mode & 0040000) != 0`. File read limit is 1 MB. Files that fail to read or exceed the limit are silently skipped. Binary content detection scans the first 8 KB for null bytes.

---

#### `(*Indexer) indexFromExternal(ctx context.Context, req IndexRequest, start time.Time)`
```go
func (i *Indexer) indexFromExternal(ctx context.Context, req IndexRequest, start time.Time) (*IndexResult, error)
```

**What it does:** Fallback indexing method. Clones a git repository (shallow, single-branch, depth 1) into `cacheDir/<StorageID>`, walks the clone directory with `filepath.WalkDir`, adds files to a Zoekt builder, builds the index, then removes the clone. Calculates index shard size after completion.

**Callers:** `IndexRepository` when `monofsClient` is nil.

**Parameters:**
- `ctx context.Context`: Passed to `exec.CommandContext` for git clone cancellation.
- `req IndexRequest`: Indexing parameters.
- `start time.Time`: For duration calculation.

**Returns:**
- `*IndexResult`: Indexing results.
- `error`: On clone failure, builder failure, or walk error.

**Implementation details:** Uses `exec.CommandContext` to run `git clone --depth=1 --single-branch --branch <ref> <url> <dir>`. Sets `GIT_TERMINAL_PROMPT=0` to suppress interactive prompts. Clone directory is cleaned up with `defer os.RemoveAll`. Skips `.git`, binary files, files > 1 MB, and binary content. Zoekt `Repository` fields: `Name=DisplayPath`, `Source=RepoURL`, `Branches=[{Name: Ref, Version: "HEAD"}]`.

---

#### `(*Indexer) Search(ctx context.Context, req SearchRequest)`
```go
func (i *Indexer) Search(ctx context.Context, req SearchRequest) (*SearchResults, error)
```

**What it does:** Performs a Zoekt code search. Builds the query depending on input:
1. If the query contains Zoekt query syntax hints (`lang:`, `file:`, `repo:`, `case:`, `sym:`) or `req.Regex` is true, parses with `query.Parse`.
2. If `req.Regex` is true but query lacks syntax, compiles as a `query.Regexp`.
3. Otherwise, uses `query.Substring` (literal text search with `Content: true`).

Then:
- Applies file pattern filters (glob → regex conversion via `globToRegex`, OR'ed together, AND'ed with the main query).
- Applies repository filter: looks up `DisplayPath` from `StorageID` mapping and wraps query with `query.NewRepoSet`. If the StorageID is not mapped, returns empty results.
- Executes search via `i.searcher.Search` with `NumContextLines: 1` and `ChunkMatches: true`.
- Converts Zoekt `FileMatch`/`ChunkMatch` results to `SearchResult` slices, enriching with `StorageID`.
- Truncates results to `maxResults` (default 100).

**Callers:** `Service.Search` handler in `handlers.go`.

**Parameters:**
- `ctx context.Context`: Passed to Zoekt searcher.
- `req SearchRequest`: `Query`, `StorageID`, `MaxResults`, `CaseSensitive`, `Regex`, `FilePatterns`.

**Returns:**
- `*SearchResults`: Results with matches, counts, duration.
- `error`: On query parse or search failure.

**Implementation details:** The Zoekt query syntax detection is a heuristic: checks for `:` in query AND at least one known keyword. File pattern filter fallback: if `syntax.Parse` fails on the glob regex, uses `query.Substring{FileName: true}`. Repository filter uses exact match via `query.NewRepoSet`. Nested loop break pattern for result truncation.

---

#### `(*Indexer) DeleteIndex(displayPath string)`
```go
func (i *Indexer) DeleteIndex(displayPath string) error
```

**What it does:** Removes Zoekt index shard files for a repository. Walks `indexDir`, finds files whose basename contains the URL-escaped display path and end with `.zoekt`, and deletes them.

**Callers:** `Service.DeleteIndex`, `Service.RebuildIndex` (force), `Service.RebuildAllIndexes` (force).

**Parameters:**
- `displayPath string`: The display path (repository name).

**Returns:**
- `error`: From `filepath.WalkDir`, or nil.

**Implementation details:** Thread-safe (mutex lock). Ignores individual file errors during walk. Matches files containing the escaped name (e.g. `"github.com%2Ffoo%2Fbar"`).

---

#### `(*Indexer) GetIndexSize(displayPath string)`
```go
func (i *Indexer) GetIndexSize(displayPath string) (int64, error)
```

**What it does:** Returns the total on-disk size of Zoekt shard files for a repository. Walks `indexDir`, sums `info.Size()` for files matching the URL-escaped display path.

**Callers:** `Service.GetIndexStatus` handler.

**Parameters:**
- `displayPath string`: Display path to look up.

**Returns:**
- `int64`: Total bytes.
- `error`: From walk.

---

#### `(*Indexer) IndexExists(displayPath string)`
```go
func (i *Indexer) IndexExists(displayPath string) bool
```

**What it does:** Checks whether any Zoekt shard file exists for a repository. Walks `indexDir`, looks for files whose basename contains the URL-escaped display path. Returns true on first match (short-circuits via `filepath.SkipAll`).

**Callers:** `Service.GetIndexStatus` handler.

**Parameters:**
- `displayPath string`: Display path.

**Returns:**
- `bool`: True if at least one matching shard file exists.

---

#### `(*Indexer) IndexLocalDir(ctx context.Context, req IndexLocalRequest)`
```go
func (i *Indexer) IndexLocalDir(ctx context.Context, req IndexLocalRequest) (*IndexResult, error)
```

**What it does:** Indexes a local filesystem directory directly (primarily for testing). Similar to `indexFromExternal` but reads from `req.SourceDir` instead of cloning a repo. Stores the `DisplayPath` → `StorageID` mapping, creates a Zoekt builder, walks the directory, adds files, finishes the index, calculates size.

**Callers:** Test code.

**Parameters:**
- `ctx context.Context`: Context (unused beyond interface compliance).
- `req IndexLocalRequest`: `StorageID`, `DisplayPath`, `SourceDir`, `Ref`.

**Returns:**
- `*IndexResult`: Indexing results.
- `error`: On failure.

**Implementation details:** Skips `.git`, binary files, files > 1 MB, binary content. Uses `filepath.Rel` for relative paths in Zoekt documents. Cleans up builder on error.

---

#### `(*Indexer) ReloadSearcher()`
```go
func (i *Indexer) ReloadSearcher() error
```

**What it does:** Closes the current Zoekt searcher and creates a new `search.DirectorySearcher` from the same `indexDir`. This picks up any newly created index shard files without restarting the process.

**Callers:** Not called within the package. Intended for external use after manual index modifications (e.g. after a batch reindex). Currently unused in the codebase.

**Returns:**
- `error`: If the new searcher cannot be created.

**Implementation details:** Thread-safe (full lock during swap). If the old searcher is nil, skips close.

---

#### `isBinaryFile(path string)`
```go
func isBinaryFile(path string) bool
```

**What it does:** Checks whether a file is likely binary based on its extension. Contains a hardcoded map of ~30 binary extensions covering executables, archives, images, audio/video, documents, fonts, bytecode, and object files.

**Callers:** `walkMonoFSTree`, `indexFromExternal`, `IndexLocalDir`.

**Parameters:**
- `path string`: File path (only extension is examined via `filepath.Ext`).

**Returns:**
- `bool`: True if the extension is in the binary extension set.

---

#### `isBinaryContent(content []byte)`
```go
func isBinaryContent(content []byte) bool
```

**What it does:** Checks whether byte content is likely binary by scanning for null bytes (0x00) in the first 8 KB. If any null byte is found, the content is considered binary.

**Callers:** `walkMonoFSTree`, `indexFromExternal`, `IndexLocalDir`.

**Parameters:**
- `content []byte`: Raw file content.

**Returns:**
- `bool`: True if null bytes detected in the first 8192 bytes.

**Implementation details:** Scans at most `min(len(content), 8192)` bytes.
