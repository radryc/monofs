# internal/git — Package Documentation

Package `git` provides Git repository operations for MonoFS, including repository management,
blob caching with LRU eviction, and tree walking.

---

## File: `blob_cache.go`

### Types

#### `BlobCache`

```go
type BlobCache struct {
    mu             sync.RWMutex
    cacheDir       string
    repoMgr        *RepoManager
    accessTracker  map[string]time.Time
    blobMetadata   map[string]*blobMeta
    maxAge         time.Duration
    maxRetries     int
    retryDelay     time.Duration
    cleanupInterval time.Duration
    stopCleanup    chan struct{}
    cleanupWg      sync.WaitGroup
}
```

Resilient blob cache with LRU-based cleanup. Tracks file access times and removes unused
blobs to save space. When a cleaned blob is requested, it automatically restores from Git.
Runs a background cleanup goroutine on creation.

#### `blobMeta`

```go
type blobMeta struct {
    RepoURL     string
    DisplayPath string
    Branch      string
    BlobHash    string
    LastAccess  time.Time
}
```

Stores metadata needed to restore a blob from Git. Unused struct — `RegisterBlob` populates
these fields but `ReadBlobWithRestore` calls `CloneOrOpen` via `repoMgr` instead of using
this metadata directly for restoration.

#### `BlobCacheConfig`

```go
type BlobCacheConfig struct {
    CacheDir        string
    MaxAge          time.Duration
    MaxRetries      int
    RetryDelay      time.Duration
    CleanupInterval time.Duration
}
```

Configuration struct for `BlobCache` construction. All fields have defaults applied in
`DefaultBlobCacheConfig()`.

#### `accessEntry`

```go
type accessEntry struct {
    hash       string
    lastAccess time.Time
}
```

Unexported helper struct used by `CleanupByCount` to sort tracked blobs by access time.

#### `CleanupStats`

```go
type CleanupStats struct {
    TotalTracked    int
    CachedOnDisk    int
    ExpiredCount    int
    OldestAccess    time.Time
    NewestAccess    time.Time
    TotalCacheBytes int64
}
```

Statistics returned by `GetStats()` about the current state of the blob cache.

---

### Functions

#### `DefaultBlobCacheConfig()`

```go
func DefaultBlobCacheConfig() BlobCacheConfig
```

Returns a `BlobCacheConfig` with sensible defaults:
- `CacheDir`: `os.TempDir()/monofs-blobs`
- `MaxAge`: 1 hour
- `MaxRetries`: 3
- `RetryDelay`: 100ms
- `CleanupInterval`: 5 minutes

**Called from:** `internal/storage/git/fetch.go:49` — the Git fetcher backend uses this
to create a `BlobCache` for serving blob content.

---

#### `NewBlobCache(repoMgr, cfg)`

```go
func NewBlobCache(repoMgr *RepoManager, cfg BlobCacheConfig) (*BlobCache, error)
```

Creates a new `BlobCache`, creates the cache directory with `MkdirAll`, applies defaults
for any zero-value config fields, and starts a background cleanup goroutine via `cleanupLoop()`.

**Parameters:**
- `repoMgr` — `*RepoManager` used for blob restoration from Git.
- `cfg` — `BlobCacheConfig`; zero fields get filled with defaults.

**Returns:** `(*BlobCache, error)` — the cache instance or an error if directory creation fails.

**Called from:** `internal/storage/git/fetch.go:56` — the fetcher backend creates one
`BlobCache` per fetcher instance.

---

#### `(bc *BlobCache) Close()`

```go
func (bc *BlobCache) Close() error
```

Signals the background cleanup goroutine to stop via `stopCleanup` channel and waits
for it to finish via `cleanupWg`.

**Returns:** always `nil`. The error return is for interface compliance (no real failure path).

**Called from:** expected to be called during shutdown of the fetcher backend to stop
the cleanup goroutine.

---

#### `(bc *BlobCache) blobPath(blobHash)`

```go
func (bc *BlobCache) blobPath(blobHash string) string
```

Computes the on-disk filesystem path for a cached blob. Uses the first 2 characters of
the hash as a subdirectory to avoid having too many files in a single directory.
If the hash is shorter than 2 characters, it stores the blob directly in the cache root.

**Called from:** `writeToCache`, `readFromCache`, `cleanupExpiredBlobs`, `ForceCleanup`,
`CleanupByCount` — all blob I/O operations use this to resolve file paths.

---

#### `(bc *BlobCache) trackAccess(blobHash)`

```go
func (bc *BlobCache) trackAccess(blobHash string)
```

Updates the access time for a blob in both `accessTracker` and `blobMetadata` maps.
Called under a write lock (`mu.Lock`). Used to refresh an entry's last-access timestamp
so it is not evicted prematurely.

**Called from:** `ReadBlob`, `ReadBlobWithRestore` — after a successful read from either
cache or Git.

---

#### `(bc *BlobCache) RegisterBlob(blobHash, repoURL, displayPath, branch)`

```go
func (bc *BlobCache) RegisterBlob(blobHash, repoURL, displayPath, branch string)
```

Registers a blob with restoration metadata. Stores the metadata in `blobMetadata` and
sets the initial access time in `accessTracker`. Should be called when blob metadata
becomes available (e.g., during ingestion).

**Parameters:**
- `blobHash` — Git blob hash string.
- `repoURL` — URL of the source repository.
- `displayPath` — display/identifier path for the repo.
- `branch` — branch name the blob belongs to.

**Called from:** `ReadBlobWithRestore` (which calls it immediately before attempting
to read), and potentially from ingestion code.

---

#### `(bc *BlobCache) ReadBlob(ctx, repo, blobHash)`

```go
func (bc *BlobCache) ReadBlob(ctx context.Context, repo *git.Repository, blobHash string) ([]byte, error)
```

Reads a blob with automatic retry. Attempts to read from the filesystem cache first
(via `readFromCache`), then falls back to reading from the provided `git.Repository`
(via `readFromGit`). On a successful Git read, the data is written back to the cache
for future reads. Uses exponential backoff between retries. Honors `ctx.Done()` for
cancellation.

**Parameters:**
- `ctx` — for deadline/cancellation.
- `repo` — an already-opened `*git.Repository` to read the blob from if not cached.
- `blobHash` — the Git blob hash to read.

**Returns:** `([]byte, error)` — the blob content or a wrapped last error after all retries.

**Called from:** `internal/storage/git/fetch.go:70` — the Git fetcher backend reads
blob content using this method, passing `nil` for the repo parameter (which will
always fail the `readFromGit` path, so this path relies on cache hits).

**Note:** The `repo` parameter is nullable. When `nil`, the `readFromGit` call will
panic since it dereferences `repo`. This is a bug — `readFromGit` should check for
a nil repo.

---

#### `(bc *BlobCache) ReadBlobWithRestore(ctx, blobHash, repoURL, displayPath, branch)`

```go
func (bc *BlobCache) ReadBlobWithRestore(ctx context.Context, blobHash, repoURL, displayPath, branch string) ([]byte, error)
```

The primary resilient read method. Registers blob metadata, then attempts to read with
automatic restoration: tries cache first, then uses `repoMgr.CloneOrOpen` to open/clone
the repository and `readFromGit` to retrieve the blob data. Caches the result on success.

**Parameters:**
- `ctx` — for deadline/cancellation.
- `blobHash` — Git blob hash.
- `repoURL` — remote repository URL for cloning if needed.
- `displayPath` — identifier/display path.
- `branch` — branch name.

**Returns:** `([]byte, error)` — blob content or wrapped error.

**Called from:** expected to be the primary method for resilient blob reads that need
auto-restoration capability.

---

#### `(bc *BlobCache) readFromCache(blobHash)`

```go
func (bc *BlobCache) readFromCache(blobHash string) ([]byte, error)
```

Reads a blob from the filesystem cache at the path computed by `blobPath`. Uses `os.ReadFile`.

**Returns:** `([]byte, error)` — blob bytes, or an error indicating the blob is not cached
or the read failed.

**Called from:** `ReadBlob`, `ReadBlobWithRestore` — as the first attempt before falling
back to Git.

---

#### `(bc *BlobCache) writeToCache(blobHash, data)`

```go
func (bc *BlobCache) writeToCache(blobHash string, data []byte) error
```

Writes a blob to the filesystem cache atomically. Creates the parent directory if needed,
writes to a temporary `.tmp` file first, then renames it to the final path. If the rename
fails, the temp file is cleaned up.

**Called from:** `ReadBlob`, `ReadBlobWithRestore` — after successfully reading from Git
to populate the cache for future reads.

---

#### `(bc *BlobCache) readFromGit(repo, blobHash)`

```go
func (bc *BlobCache) readFromGit(repo *git.Repository, blobHash string) ([]byte, error)
```

Reads a blob directly from a go-git `*git.Repository` by hash. Calls `repo.BlobObject(hash)`,
gets a reader, and reads all data with `io.ReadAll`.

**Parameters:**
- `repo` — an open `*git.Repository`. Must not be nil or it will panic.
- `blobHash` — Git blob hash.

**Returns:** `([]byte, error)` — blob bytes or an error from the Git object layer.

**Called from:** `ReadBlob`, `ReadBlobWithRestore` — as the Git fallback read path.

---

#### `(bc *BlobCache) cleanupLoop()`

```go
func (bc *BlobCache) cleanupLoop()
```

Background goroutine that runs `cleanupExpiredBlobs()` at `cleanupInterval` intervals.
Stops when `stopCleanup` is signaled. Decrements `cleanupWg` on exit.

**Called from:** `NewBlobCache` — launched as a goroutine during cache construction.

---

#### `(bc *BlobCache) cleanupExpiredBlobs()`

```go
func (bc *BlobCache) cleanupExpiredBlobs()
```

Iterates over all tracked blobs and removes from disk any whose last access time is
before `now - maxAge`. Only deletes the `accessTracker` entry; **preserves `blobMetadata`**
entries so that blobs can be restored later. Removes the filesystem file via `os.Remove`.

**Called from:** `cleanupLoop` — periodically on the background goroutine ticker.

---

#### `(bc *BlobCache) GetStats()`

```go
func (bc *BlobCache) GetStats() CleanupStats
```

Returns current cache statistics. Computes:
- `TotalTracked` — entries in the access tracker
- `ExpiredCount` — entries whose last access predates the cutoff
- `OldestAccess` / `NewestAccess` — time bounds
- `CachedOnDisk` / `TotalCacheBytes` — walked from the filesystem via `filepath.Walk`

**Returns:** `CleanupStats` struct populated with live data.

**Called from:** monitoring/debugging UI or API endpoints to report cache health.

---

#### `(bc *BlobCache) ForceCleanup()`

```go
func (bc *BlobCache) ForceCleanup() int
```

Immediately removes all expired blobs from disk, deleting both the file and the
`accessTracker` entry (unlike `cleanupExpiredBlobs`, which preserves metadata for
restoration, `ForceCleanup` deletes the tracker entry too).

**Returns:** `int` — number of blobs removed.

**Called from:** on-demand cleanup triggers, e.g., when disk space is critically low.

---

#### `(bc *BlobCache) CleanupByCount(count)`

```go
func (bc *BlobCache) CleanupByCount(count int) int
```

Removes the oldest `count` blobs from disk, sorted by last access time (oldest first).
Deletes both the file and the `accessTracker` entry. Returns early if `count <= 0`
or there are no tracked blobs.

**Returns:** `int` — number of blobs actually removed (may be less than `count` if
fewer entries exist).

**Called from:** space-pressure scenarios where a fixed number of blobs need to be freed.

---

## File: `repo.go`

### Types

#### `RepoManager`

```go
type RepoManager struct {
    cacheDir string
}
```

Handles Git repository operations: cloning, opening, fetching, tree walking, blob
reading, and cleanup. All repositories are stored under `cacheDir`.

#### `FileMetadata`

```go
type FileMetadata struct {
    Path     string
    Size     uint64
    Mode     uint32
    BlobHash string
    Mtime    int64
}
```

Represents file metadata extracted from a Git tree object:
- `Path` — file path within the repository.
- `Size` — file size in bytes.
- `Mode` — OS file mode (converted from Git file mode).
- `BlobHash` — Git blob hash for the file content.
- `Mtime` — modification time derived from the commit's committer time.

#### `ErrRepoNotCloned`

```go
var ErrRepoNotCloned = fmt.Errorf("repository not cloned locally")
```

Sentinel error returned by `OpenOnly` when the repository directory does not exist.

---

### Functions

#### `NewRepoManager(cacheDir)`

```go
func NewRepoManager(cacheDir string) (*RepoManager, error)
```

Creates a new `RepoManager`, ensuring the cache directory exists with `os.MkdirAll`.

**Parameters:**
- `cacheDir` — filesystem path where cloned repositories will be stored.

**Returns:** `(*RepoManager, error)` — the manager or a directory creation error.

**Called from:**
- `internal/storage/git/ingestion.go:47` — Git ingestion backend.
- `internal/storage/git/fetch.go:42` — Git fetcher backend.
- `internal/storage/file/ingestion.go:82` — File ingestion backend.
- `internal/fetcher/service.go:107` — Fetcher service.

---

#### `(rm *RepoManager) OpenOnly(repoID)`

```go
func (rm *RepoManager) OpenOnly(repoID string) (*git.Repository, error)
```

Opens an existing repository without cloning. Returns `ErrRepoNotCloned` if the
repository directory does not exist. Used for fast-path lookups where network
operations should be avoided.

**Parameters:**
- `repoID` — directory name under `cacheDir` where the repo is stored.

**Returns:** `(*git.Repository, error)` — the open repository or `ErrRepoNotCloned`.

**Called from:** fast-path checks where the caller wants to avoid blocking on
network clone operations.

---

#### `(rm *RepoManager) CloneOrOpen(ctx, repoURL, repoID, branch)`

```go
func (rm *RepoManager) CloneOrOpen(ctx context.Context, repoURL, repoID, branch string) (*git.Repository, error)
```

Clones a repository or opens an existing one. If the repo already exists on disk,
it opens it and performs a shallow fetch (`Depth: 1`) to get latest changes
(non-fatal if fetch fails). If the repo does not exist, it performs a shallow
clone with `SingleBranch: true`, `NoTags`, `NoCheckout: true`, and
`ShallowSubmodules: true` — these options are critical for performance, avoiding
tag downloads and working directory checkout.

**Parameters:**
- `ctx` — for deadline/cancellation.
- `repoURL` — remote repository URL.
- `repoID` — directory name under `cacheDir`.
- `branch` — branch name; must be non-empty for clones.

**Returns:** `(*git.Repository, error)` — the open repository or a clone error.

**Requires:** `branch` must be non-empty when the repo needs to be cloned.

**Called from:**
- `internal/storage/git/ingestion.go:54` — ingestion backend.
- `internal/fetcher/service.go:567` — fetcher service.
- `(*BlobCache).ReadBlobWithRestore` in `blob_cache.go:250` — blob restoration.

---

#### `(rm *RepoManager) GetDefaultBranch(ctx, repoURL)`

```go
func (rm *RepoManager) GetDefaultBranch(ctx context.Context, repoURL string) (string, error)
```

Detects the default branch of a remote repository by listing remote references.
Looks for the symbolic `HEAD` reference and resolves its target (stripping the
`refs/heads/` prefix). Falls back to checking for `main` or `master` branches
if the symbolic HEAD is not found.

**Parameters:**
- `ctx` — for deadline/cancellation.
- `repoURL` — remote URL.

**Returns:** `(string, error)` — branch name (e.g. `"main"`, `"master"`) or error.

**Called from:** `internal/storage/git/ingestion.go:82` — to detect the default
branch when not explicitly specified.

---

#### `resolveReference(repo, branch)`

```go
func resolveReference(repo *git.Repository, branch string) (*plumbing.Reference, error)
```

Resolves a Git reference using multiple strategies, attempted in order:
1. Local branch name (`refs/heads/<branch>`)
2. Remote-tracking branch (`refs/remotes/origin/<branch>`)
3. `HEAD`

Stops at the first successful resolution.

**Parameters:**
- `repo` — open repository.
- `branch` — branch name to resolve.

**Returns:** `(*plumbing.Reference, error)` — the resolved reference or an error
indicating all strategies failed.

**Called from:**
- `resolveReferenceWithFallback` — as the inner resolution step.
- `WalkTree` — to resolve the branch for tree walking.
- `GetFileMetadata` — to resolve the branch for single-file lookup.

---

#### `resolveReferenceWithFallback(repo, branch)`

```go
func resolveReferenceWithFallback(repo *git.Repository, branch string) (*plumbing.Reference, error)
```

Tries to resolve `branch`, then falls back to `"main"`, then `"master"` if the
given branch cannot be resolved. Uses `resolveReference` for each attempt.

**Parameters:**
- `repo` — open repository.
- `branch` — preferred branch name (may be empty to skip directly to fallbacks).

**Returns:** `(*plumbing.Reference, error)` — the first successfully resolved
reference, or an error if all branches fail.

**Called from:** (currently defined but not called outside its own file; may be
used by callers that need multi-branch fallback).

---

#### `(rm *RepoManager) WalkTree(repo, branch, fn)`

```go
func (rm *RepoManager) WalkTree(repo *git.Repository, branch string, fn func(FileMetadata) error) error
```

Walks the Git tree of the specified branch and invokes `fn` for each file.
Resolves the branch reference, gets the commit object, extracts the commit
timestamp as `Mtime`, retrieves the commit's tree, and iterates over all files
with `tree.Files().ForEach()`. Works with `NoCheckout` clones since it reads
Git objects directly.

**Parameters:**
- `repo` — open repository.
- `branch` — branch name (defaults to `"main"` if empty).
- `fn` — callback receiving `FileMetadata` for each file. If it returns an error,
  the walk stops.

**Returns:** `error` — reference resolution error, commit error, tree error,
or error from the callback.

**Called from:**
- `internal/storage/git/ingestion.go:113` — to enumerate files for ingestion.
- `internal/storage/file/ingestion.go:132` — to enumerate files for file ingestion.

---

#### `(rm *RepoManager) ReadBlob(repo, blobHash)`

```go
func (rm *RepoManager) ReadBlob(repo *git.Repository, blobHash string) ([]byte, error)
```

Reads a blob from the repository by hash. Uses `repo.BlobObject(hash)` and
`io.ReadAll`.

**Parameters:**
- `repo` — open repository.
- `blobHash` — Git blob hash.

**Returns:** `([]byte, error)` — blob content or error.

**Called from:**
- `internal/storage/git/ingestion.go:115` — to read file content during ingestion.
- `internal/storage/file/ingestion.go:133` — to read file content during file ingestion.

---

#### `(rm *RepoManager) CleanupRepo(repoID)`

```go
func (rm *RepoManager) CleanupRepo(repoID string) error
```

Removes a cloned repository from the cache to free disk space. Uses `os.RemoveAll`
on the repository directory.

**Parameters:**
- `repoID` — directory name under `cacheDir`.

**Returns:** `error` — from `os.RemoveAll`.

**Called from:**
- `internal/storage/git/ingestion.go:80,164` — cleanup after validation or ingestion.
- `internal/storage/file/ingestion.go:255` — cleanup after file ingestion.

---

#### `(rm *RepoManager) OpenOrClone(repoURL, branch)`

```go
func (rm *RepoManager) OpenOrClone(repoURL, branch string) (*git.Repository, error)
```

Tries to open an existing repository or clones it if not present. Derives the
repo ID from `filepath.Base(repoURL)`. For clones, uses a shallow, single-branch
clone. Defaults to `"main"` if branch is empty.

**Parameters:**
- `repoURL` — remote repository URL.
- `branch` — branch name (defaults to `"main"`).

**Returns:** `(*git.Repository, error)` — the repository or error.

**Note:** This is a simpler, non-context-aware version of `CloneOrOpen`. It does
not perform fetches on already-existing repos. Unlike `CloneOrOpen`, it also
uses `PlainClone` (synchronous), which blocks without context support.

---

#### `(rm *RepoManager) GetFileMetadata(repo, branch, filePath)`

```go
func (rm *RepoManager) GetFileMetadata(repo *git.Repository, branch, filePath string) (FileMetadata, error)
```

Retrieves metadata for a specific file in the repository tree at the given branch.
Resolves the branch reference, gets the commit and tree, then looks up the specific
file by path. Extracts file mode, size, blob hash, and commit timestamp.

**Parameters:**
- `repo` — open repository.
- `branch` — branch name (defaults to `"main"` if empty).
- `filePath` — path within the repository.

**Returns:** `(FileMetadata, error)` — metadata or error.

**Called from:**
- `internal/storage/git/ingestion.go:144` — to get metadata for individual files.
- `internal/storage/file/ingestion.go:218` — to get metadata for individual files.

---

## Call Graph Summary

### `NewRepoManager` callers
- `internal/storage/git/ingestion.go:47` — `NewGitStorage`
- `internal/storage/git/fetch.go:42` — `NewGitFetcherBackend`
- `internal/storage/file/ingestion.go:82` — file ingestion init
- `internal/fetcher/service.go:107` — `NewService`

### `CloneOrOpen` callers
- `internal/storage/git/ingestion.go:54` — repo open during ingestion
- `internal/fetcher/service.go:567` — repo open during fetcher sync
- `blob_cache.go:250` — `ReadBlobWithRestore` auto-restoration

### `WalkTree` callers
- `internal/storage/git/ingestion.go:113` — batch file ingestion
- `internal/storage/file/ingestion.go:132` — batch file ingestion

### `ReadBlob` (RepoManager) callers
- `internal/storage/git/ingestion.go:115`
- `internal/storage/file/ingestion.go:133`

### `CleanupRepo` callers
- `internal/storage/git/ingestion.go:80,164`
- `internal/storage/file/ingestion.go:255`

### `GetFileMetadata` callers
- `internal/storage/git/ingestion.go:144`
- `internal/storage/file/ingestion.go:218`

### `GetDefaultBranch` callers
- `internal/storage/git/ingestion.go:82`

### `NewBlobCache` callers
- `internal/storage/git/fetch.go:56`

### `ReadBlob` (BlobCache) callers
- `internal/storage/git/fetch.go:70` — passes `nil` repo (potential issue)

### `DefaultBlobCacheConfig` callers
- `internal/storage/git/fetch.go:49`

---

## Known Issues

1. **`ReadBlob` (BlobCache) nil repo panic**: In `internal/storage/git/fetch.go:70`,
   `bc.ReadBlob(ctx, nil, req.ContentID)` passes `nil` for the repo parameter.
   If the cache misses and the retry loop reaches `readFromGit`, it will panic
   dereferencing the nil `*git.Repository`. The method should either check for
   nil repo and skip the Git path, or the caller should use `ReadBlobWithRestore`.

2. **`blobMeta` struct is populated but not used for restoration**: `RegisterBlob`
   stores metadata in `blobMetadata`, but `ReadBlobWithRestore` calls
   `repoMgr.CloneOrOpen(ctx, repoURL, displayPath, branch)` using the method
   parameters directly rather than reading from the stored metadata. The
   `blobMetadata` map is effectively dead data for restoration purposes.

3. **`resolveReferenceWithFallback` has no external callers** within the codebase
   (only defined and self-referenced). It may be intended for future use or
   for external callers outside this package.
