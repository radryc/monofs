# Internal Registry Documentation

Package `registry` (`internal/registry`) implements an OCI-compatible Docker/OCI registry backed by MonoFS (a distributed file system). It layers on top of a gRPC client to MonoFS nodes routed through a MonoFSRouter.

---

## blobs.go — Blob storage layer

This file provides a chunked blob store on top of the MonoFS `Client`. Blobs larger than 512 MiB are split into numbered chunks (`_blobs/<algo>/<hash>/0000`, `0001`, …).

### Constants

| Name | Value | Description |
|------|-------|-------------|
| `chunkSize` | `512 * 1024 * 1024` (512 MiB) | Maximum size of a single blob chunk, chosen to stay well under gRPC's 1 GiB message limit. |

### Types

#### `BlobStore`
```go
type BlobStore struct {
    client *Client
}
```
Thin wrapper around a `*Client` that provides chunked blob operations.

#### `multiReadCloser`
```go
type multiReadCloser struct {
    readers []io.ReadCloser
    current int
}
```
Implements `io.ReadCloser` by reading sequentially through multiple readers until each returns `io.EOF`. Used to stitch together chunked blob reads into a single stream.

#### `byteReader`
```go
type byteReader struct {
    data   []byte
    offset int
}
```
Implements `io.Reader` over an in-memory byte slice.

---

### Function: `NewBlobStore`
```go
func NewBlobStore(client *Client) *BlobStore
```
**What it does:** Constructs a `BlobStore` wrapping the given MonoFS client.

**Called from:** `server.go:NewServer` (line 33); also created inline in `cmd/monofs-registry/main.go:118`.

**Key parameters:** `client` — the MonoFS gRPC client used for all I/O.

---

### Function: `BlobPath`
```go
func BlobPath(digest string) string
```
**What it does:** Converts a content-addressable digest string (e.g. `sha256:abc123…`) into a file path `_blobs/sha256/abc123…`. Used as the canonical storage path for a non-chunked (single-file) blob.

**Called from:** `BlobStore.Get`, `BlobStore.GetReader`, `BlobStore.Exists`, `BlobStore.Size`, `BlobStore.Delete`, `putChunkedFromReaderWithData`, `chunkPath`.

---

### Function: `DigestKey`
```go
func DigestKey(digest string) string
```
**What it does:** Produces a flat key `_blobs/<digest>` (keeping the colon) instead of replacing `:` with `/`. This function is defined but **not called** anywhere in the current codebase; likely reserved for alternate key schemes.

---

### Method: `(*BlobStore).Get`
```go
func (b *BlobStore) Get(ctx context.Context, digest string) ([]byte, error)
```
**What it does:** Reads the full content of a blob. First tries to read via `BlobPath` (single-file path). If that fails, falls back to `readChunked`, which concatenates all numbered chunk files in order.

**Called from:** `TagStore.GetManifest` (manifests.go:273, 281).

**Returns:** Full blob bytes, or error (wraps `os.ErrNotExist` if none found).

---

### Method: `(*BlobStore).GetReader`
```go
func (b *BlobStore) GetReader(ctx context.Context, digest string) (io.ReadCloser, error)
```
**What it does:** Returns a streaming reader for a blob's content. Prefers a single-stream read via `ReadStream`; if that fails, falls back to `readChunkedReader` which produces a `multiReadCloser` across all chunks. The caller must close the returned reader to release underlying gRPC streams.

**Called from:** `Server.handleGetBlob` (server.go:276, 328), `BlobStore.Size` (self, as a fallback when Stat fails, line 196).

---

### Method: `(*BlobStore).readChunkedReader`
```go
func (b *BlobStore) readChunkedReader(ctx context.Context, digest string) (io.ReadCloser, error)
```
**What it does:** Iterates chunk indices `0, 1, 2, …`, opening a streaming read for each `chunkPath(digest, i)`. Returns a `multiReadCloser` that transparently stitches chunks together. If the first chunk (`i == 0`) fails, propagates the error. If subsequent chunks are absent, stops iteration.

**Implementation note:** Only called as a fallback from `GetReader`. Chunks are opened lazily — all `io.ReadCloser` handles are collected upfront, and the `multiReadCloser` sequences through them.

---

### Method: `(*BlobStore).readChunked`
```go
func (b *BlobStore) readChunked(ctx context.Context, digest string) ([]byte, error)
```
**What it does:** The non-streaming equivalent of `readChunkedReader`. Reads each chunk fully into memory via `client.Read` and appends to a single `[]byte`. Returns all content or an error (first chunk error is propagated directly).

**Called from:** `BlobStore.Get` (fallback path).

---

### Method: `(*BlobStore).Exists`
```go
func (b *BlobStore) Exists(ctx context.Context, digest string) (bool, error)
```
**What it does:** Checks whether a blob exists by testing the single-file path via `client.Exists`. Does **not** check for chunked blobs; relies on the fact that even chunked uploads should have the path resolvable, or on callers handling the false-negative gracefully.

**Called from:** `Proxy.FetchBlob` (proxy.go:120).

---

### Method: `(*BlobStore).Put`
```go
func (b *BlobStore) Put(ctx context.Context, digest string, content []byte) error
```
**What it does:** Stores blob content **with digest verification**. Computes `sha256` of `content`, asserts it matches the expected `digest`, then delegates to `putChunkedFromReader`. Returns an error on digest mismatch.

**Called from:** Not called in the current codebase (public API for external consumers).

---

### Method: `(*BlobStore).PutUnchecked`
```go
func (b *BlobStore) PutUnchecked(ctx context.Context, digest string, content []byte) error
```
**What it does:** Stores blob content **without** digest verification. Wraps the content in a `byteReader` and calls `putChunkedFromReader`.

**Called from:** `TagStore.PutManifest` (manifests.go:287).

---

### Method: `(*BlobStore).PutUncheckedFromReader`
```go
func (b *BlobStore) PutUncheckedFromReader(ctx context.Context, digest string, r io.Reader, size int64) error
```
**What it does:** Stores blob content from an `io.Reader` without digest verification. Discards the returned data.

**Called from:** `Server.handleUploadComplete` (server.go:447).

---

### Method: `(*BlobStore).PutUncheckedFromReaderWithData`
```go
func (b *BlobStore) PutUncheckedFromReaderWithData(ctx context.Context, digest string, r io.Reader, size int64) ([]byte, error)
```
**What it does:** Stores blob content from an `io.Reader` and **returns** the read data. This is a convenience wrapper so the caller (the proxy) can serve blob content directly from memory without a second MonoFS read. Delegates to `putChunkedFromReaderWithData`.

**Called from:** `Proxy.fetchBlobFromUpstream` (proxy.go:287).

---

### Method: `(*BlobStore).putChunkedFromReader`
```go
func (b *BlobStore) putChunkedFromReader(ctx context.Context, digest string, r io.Reader, size int64) error
```
**What it does:** Thin wrapper around `putChunkedFromReaderWithData` that discards the returned byte slice.

**Called from:** `Put`, `PutUnchecked`, `PutUncheckedFromReader` (private dispatch).

---

### Method: `(*BlobStore).putChunkedFromReaderWithData`
```go
func (b *BlobStore) putChunkedFromReaderWithData(ctx context.Context, digest string, r io.Reader, size int64) ([]byte, error)
```
**What it does:** Core write logic. Three paths:
1. **`size <= 0`**: Writes an empty file at the blob path (used for zero-byte blobs).
2. **`size <= chunkSize` (512 MiB)**: Reads all data into a buffer, writes it as a single file at `BlobPath(digest)`, and returns the buffer.
3. **`size > chunkSize`**: Splits the data into `chunkSize`-sized pieces, writing each as `chunkPath(digest, i)`. Returns `nil, nil` for the data (too large to return in memory).

Every write path first ensures the parent directory exists via `client.CreateDir`.

**Called from:** `putChunkedFromReader`, `PutUncheckedFromReaderWithData` (private dispatch + public entry point).

---

### Method: `(*BlobStore).writeSingle`
```go
func (b *BlobStore) writeSingle(ctx context.Context, path string, data []byte) error
```
**What it does:** Atomically writes a single blob file (or chunk file). First ensures the parent directory exists via `client.CreateDir`, then writes the data via `client.Write`.

**Called from:** `putChunkedFromReaderWithData` (called for each chunk and for the single-file path).

---

### Method: `(*BlobStore).Delete`
```go
func (b *BlobStore) Delete(ctx context.Context, digest string) error
```
**What it does:** Deletes a blob and all its chunks. First deletes the main path (`BlobPath`), then iterates chunk indices `0, 1, 2, …` until a chunk is not found (`Exists` returns false), deleting each one.

**Called from:** `TagStore.DeleteManifest` (manifests.go:306), `Server.handleDeleteBlob` (server.go:363).

---

### Method: `(*BlobStore).Size`
```go
func (b *BlobStore) Size(ctx context.Context, digest string) (int64, error)
```
**What it does:** Determines the total size of a blob. Three strategies:
1. Try `client.Stat` on the single-file path — if successful, return the reported size.
2. Iterate chunk paths, summing sizes from `Stat` responses.
3. **Fallback:** If Stat fails on a non-chunked blob (e.g. NutsDB metadata lookup fails due to conflicting index keys from chunked uploads), reads the entire blob content via `GetReader` and returns `len(data)`.

**Called from:** `Proxy.FetchBlob` (proxy.go:122), `Server.handleGetBlob` (server.go:283, 335), `Server.handleDeleteBlob` (server.go:362).

---

### Function: `chunkPath`
```go
func chunkPath(digest string, index int) string
```
**What it does:** Generates the path for a single chunk: `BlobPath(digest) + "/" + fmt.Sprintf("%04d", index)`. For digest `sha256:abc`, chunk 3 → `_blobs/sha256/abc/0003`.

**Called from:** `readChunkedReader`, `readChunked`, `putChunkedFromReaderWithData`, `Delete`, `Size`.

---

### Function: `sha256Hash`
```go
func sha256Hash(data []byte) []byte
```
**What it does:** Returns the raw 32-byte SHA-256 hash of `data` (not hex-encoded).

**Called from:** `BlobStore.Put` (digest verification), `TagStore.PutManifest` (manifests.go:286, via `sha256Hash` directly — actually `manifests.go` uses `sha256Hash` defined in `blobs.go`).

---

### Method: `(*multiReadCloser).Read`
```go
func (m *multiReadCloser) Read(p []byte) (int, error)
```
**What it does:** Reads from the current reader in `readers[current]`. When it returns `io.EOF`, closes that reader, advances `current`, and continues to the next reader. Returns `io.EOF` only after the last reader is exhausted. If a reader returns `io.EOF` with `n > 0`, returns that data and closes/advances on the next call.

---

### Method: `(*multiReadCloser).Close`
```go
func (m *multiReadCloser) Close() error
```
**What it does:** Closes all remaining unclosed readers (from `current` to the end). Returns the last error encountered.

---

### Method: `(*byteReader).Read`
```go
func (r *byteReader) Read(p []byte) (int, error)
```
**What it does:** Standard reader over a `[]byte`. Copies as much as fits into `p` and returns `io.EOF` when exhausted.

---

### Function: `bytesReader`
```go
func bytesReader(data []byte) io.Reader
```
**What it does:** Wraps a `[]byte` in a `byteReader`, returning an `io.Reader`.

**Called from:** `Put`, `PutUnchecked`.

---

## manifests.go — Tag and manifest management

This file provides a `TagStore` that persists Docker/OCI tags and manifests in MonoFS, with an in-memory cache to work around MonoFS directory-listing inconsistency.

### Constants

| Name | Value | Description |
|------|-------|-------------|
| `cacheTTL` | `30 * time.Minute` | How long cache entries live before eviction. |
| `catalogPath` | `"_catalog"` | File path for the repository catalog (JSON). |

### Types

#### `TagStore`
```go
type TagStore struct {
    client      *Client
    blobs       *BlobStore

    repoCache   map[string]bool
    tagCache    map[string][]string
    cacheExpiry map[string]time.Time
    mu          sync.RWMutex
    stopCleanup chan struct{}
}
```
Manages tags and repositories. Uses `client` for metadata I/O (tags, catalog) and `blobs` for manifest content. Maintains in-memory caches with TTL-based expiry.

#### `catalogEntry`
```go
type catalogEntry struct {
    Repos []string `json:"repos"`
}
```
JSON structure for the `_catalog` file.

---

### Function: `NewTagStore`
```go
func NewTagStore(client *Client, blobs *BlobStore) *TagStore
```
**What it does:** Creates a `TagStore`, initializes maps and channels, and starts a background goroutine (`cacheCleanupLoop`) that periodically evicts expired cache entries.

**Called from:** `Server.NewServer` (server.go:34); also created inline in `cmd/monofs-registry/main.go:118`.

---

### Method: `(*TagStore).cacheCleanupLoop`
```go
func (t *TagStore) cacheCleanupLoop()
```
**What it does:** Background goroutine. Every 10 minutes (or on `stopCleanup` channel close), calls `evictExpired` to remove stale cache entries.

---

### Method: `(*TagStore).evictExpired`
```go
func (t *TagStore) evictExpired()
```
**What it does:** Iterates `cacheExpiry` under write-lock and removes any entry whose expiry time is before `time.Now()`. Cleans up `repoCache`, `tagCache`, and `cacheExpiry` for each expired repo.

---

### Method: `(*TagStore).touchRepo`
```go
func (t *TagStore) touchRepo(repo string)
```
**What it does:** Refreshes the cache expiry for a given repo to `now + cacheTTL` (30 minutes). Used on cache hits to extend TTL.

**Called from:** `ListTags` (on cache hit, via `touchRepo`).

---

### Method: `(*TagStore).Close`
```go
func (t *TagStore) Close() error
```
**What it does:** Closes the `stopCleanup` channel, which signals `cacheCleanupLoop` to exit its goroutine.

---

### Function: `tagPath`
```go
func tagPath(repo, tag string) string
```
**What it does:** Generates the MonoFS file path for a tag reference: `<repo>/_tags/<tag>`.

**Called from:** `GetTag`, `PutTag`, `DeleteTag`.

---

### Method: `(*TagStore).GetTag`
```go
func (t *TagStore) GetTag(ctx context.Context, repo, tag string) (string, error)
```
**What it does:** Reads the tag file from MonoFS and returns the manifest digest it points to. The content is trimmed of whitespace.

**Called from:** `GetManifest` (line 277), `Server.handleRepoDetail` (server.go:563).

---

### Method: `(*TagStore).PutTag`
```go
func (t *TagStore) PutTag(ctx context.Context, repo, tag, manifestDigest string) error
```
**What it does:** Writes the tag → digest mapping to MonoFS. Ensures the `_tags` directory exists. Updates the in-memory tag cache (avoids duplicates). Also calls `addToCatalog` to register the repo in the global catalog.

**Called from:** `PutManifest` (line 297).

---

### Method: `(*TagStore).DeleteTag`
```go
func (t *TagStore) DeleteTag(ctx context.Context, repo, tag string) error
```
**What it does:** Deletes a tag file from MonoFS via `client.Delete`. Does **not** update in-memory caches or catalog.

**Called from:** `DeleteManifest` (line 308).

---

### Method: `(*TagStore).ListTags`
```go
func (t *TagStore) ListTags(ctx context.Context, repo string) ([]string, error)
```
**What it does:** Returns sorted tag names for a repo. First checks the in-memory cache; on hit, extends TTL and returns. On miss, lists the `_tags` directory via `client.ListDir`, populates the cache, and returns. Returns `nil, nil` for unknown repos.

**Called from:** `Server.handleListTags` (server.go:473), `Server.handleRepoDetail` (server.go:548).

---

### Method: `(*TagStore).ListRepos`
```go
func (t *TagStore) ListRepos(ctx context.Context) ([]string, error)
```
**What it does:** Returns sorted repository names. Serves from the in-memory `repoCache` if populated. Otherwise acquires the write-lock and calls `listReposLocked` to rebuild the cache from storage.

**Called from:** `Server.handleRepos` (server.go:509).

---

### Method: `(*TagStore).listReposLocked`
```go
func (t *TagStore) listReposLocked(ctx context.Context) ([]string, error)
```
**What it does:** Rebuilds `repoCache` from two sources:
1. Root directory listing via `client.ListDir("")` — any entry not starting with `_` is a repo.
2. The `_catalog` file (JSON) — persists repos that may not appear in root directory listing due to MonoFS listing inconsistency.

MUST be called with `t.mu` held (write-lock).

**Called from:** `ListRepos`, `addToCatalog` (to check if a repo already exists).

---

### Method: `(*TagStore).ListReposAll`
```go
func (t *TagStore) ListReposAll(ctx context.Context) ([]string, error)
```
**What it does:** Similar to `ListRepos` but **always** re-scan from storage (bypasses cache). Overwrites `repoCache` with fresh data from both directory listing and `_catalog` file.

**Called from:** `Server.handleCatalog` (server.go:526).

---

### Method: `(*TagStore).addToCatalog`
```go
func (t *TagStore) addToCatalog(ctx context.Context, repo string) error
```
**What it does:** Ensures a repo is in the `_catalog` file. Reloads the full list via `listReposLocked`, checks for duplicate, appends the new repo if needed, sorts, and writes the JSON back to MonoFS. Also updates in-memory cache.

**Called from:** `PutTag` (line 114, after tag write).

---

### Method: `(*TagStore).GetManifest`
```go
func (t *TagStore) GetManifest(ctx context.Context, repo, ref string) ([]byte, string, error)
```
**What it does:** Resolves a manifest reference. Two strategies:
1. If `ref` is a valid digest (sha256:…), reads the blob directly via `blobs.Get`.
2. Otherwise treats `ref` as a tag, resolves it via `GetTag`, then reads the resulting digest blob.

Returns the manifest bytes, the resolved digest, and any error.

**Called from:** `Server.handleGetManifest` (server.go:202), `Proxy.FetchManifest` (proxy.go:150).

---

### Method: `(*TagStore).PutManifest`
```go
func (t *TagStore) PutManifest(ctx context.Context, repo, ref string, content []byte) (string, error)
```
**What it does:** Stores a manifest. Computes the SHA-256 digest of `content`, stores it as a blob via `blobs.PutUnchecked`. If `ref` is a digest, returns that digest without setting a tag. If `ref` is a tag name, calls `PutTag` to map the tag to the digest.

Returns the digest string.

**Called from:** `Server.handlePutManifest` (server.go:250), `Proxy.FetchManifest` (proxy.go:167).

---

### Method: `(*TagStore).DeleteManifest`
```go
func (t *TagStore) DeleteManifest(ctx context.Context, repo, ref string) error
```
**What it does:** Deletes a manifest. If `ref` is a digest, deletes the blob. Otherwise deletes the tag reference.

**Called from:** `Server.handleDeleteManifest` (server.go:265).

---

## monofs_client.go — MonoFS gRPC client

This file provides a gRPC-based client that connects to a MonoFS router, discovers healthy storage nodes, and performs file I/O operations (Read, Write, Stream, Delete, Stat, ListDir) against those nodes.

### Constants

| Name | Value | Description |
|------|-------|-------------|
| `defaultClientHeartbeat` | `30s` | Default heartbeat interval. |
| `defaultTopologyRefresh` | `10s` | Time between topology refreshes. |
| `defaultRPCTimeout` | `30s` | RPC call timeout. |

### Types

#### `ClientConfig`
```go
type ClientConfig struct {
    RouterAddr string
    Token      string
    ClientID   string
    DataNS     string
    Logger     *slog.Logger
}
```
Configuration for constructing a `Client`. `RouterAddr` is required; `ClientID` auto-generates if empty; `DataNS` defaults to `"docker-registry"`.

#### `Client`
```go
type Client struct {
    routerAddr string
    clientID   string
    token      string
    dataNS     string
    logger     *slog.Logger
    rpcTimeout time.Duration

    mu          sync.Mutex
    routerConn  *grpc.ClientConn
    router      pb.MonoFSRouterClient
    nodeConns   map[string]*grpc.ClientConn
    nodeClients map[string]pb.MonoFSClient
    nodeAddrs   map[string]string
    lastRefresh time.Time
    refreshTTL  time.Duration

    stopHeartbeat chan struct{}
    stopOnce      sync.Once
    heartbeatWG   sync.WaitGroup
}
```
Main client struct. Maintains gRPC connections to the MonoFS router and all discovered storage nodes.

#### `nodeTarget`
```go
type nodeTarget struct {
    id     string
    client pb.MonoFSClient
}
```
Internal pairing of a node ID and its gRPC client, used for iteration during read/write retry loops.

#### `grpcReadCloser`
```go
type grpcReadCloser struct {
    stream pb.MonoFS_ReadClient
    cancel context.CancelFunc
    buf    []byte
    offset int
    eof    bool
}
```
Implements `io.ReadCloser` over a server-side streaming gRPC `Read` call. Buffers the most recent chunk and drains it before requesting the next.

---

### Function: `NewClient`
```go
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error)
```
**What it does:** Validates config (requires `RouterAddr`; auto-generates `ClientID` if empty; defaults `DataNS` to `"docker-registry"`; defaults logger). Dials the router via gRPC (insecure, 1 GiB message limits). Refreshes the node topology via `refreshNodes`. Registers with the router via `register`, and starts a background heartbeat loop.

**Called from:** `cmd/monofs-registry/main.go:93`.

---

### Method: `(*Client).Close`
```go
func (c *Client) Close() error
```
**What it does:** Stops the heartbeat loop, sends an unregister RPC to the router (best-effort, 5s timeout), closes all node connections, and closes the router connection.

---

### Method: `(*Client).dataPath`
```go
func (c *Client) dataPath(elem ...string) string
```
**What it does:** Prepends the `dataNS` prefix to a path, joining segments with `/`. E.g., `dataPath("_blobs", "sha256", "abc")` → `"docker-registry/_blobs/sha256/abc"`.

**Called from:** Every I/O method (`Read`, `ReadStream`, `Write` via `resolveWritePaths`, `Exists`, `Stat`, `Delete`, `ListDir`).

---

### Method: `(*Client).Read`
```go
func (c *Client) Read(ctx context.Context, path string) ([]byte, error)
```
**What it does:** Reads a file's entire contents. Iterates healthy nodes (from `healthyNodes`), opens a gRPC `Read` streaming call on each, and concatenates all received chunks into a `[]byte`. Falls through to the next node on error. Converts `codes.NotFound` to `os.ErrNotExist`.

**Called from:** `BlobStore.Get` (via `readChunked` calling `client.Read`), `TagStore.GetTag`, `TagStore.listReposLocked`, `TagStore.ListReposAll`.

---

### Method: `(*Client).ReadStream`
```go
func (c *Client) ReadStream(ctx context.Context, path string) (io.ReadCloser, error)
```
**What it does:** Returns a lazy `io.ReadCloser` backed by a gRPC streaming `Read` call. Unlike `Read`, it eagerly receives the first chunk during setup to verify the file exists. Returns a `grpcReadCloser` that the caller **must close** to release the stream.

**Called from:** `BlobStore.GetReader`, `BlobStore.readChunkedReader`.

---

### Method: `(*grpcReadCloser).Read`
```go
func (r *grpcReadCloser) Read(p []byte) (int, error)
```
**What it does:** Serves buffered data from the current chunk. When the buffer is exhausted, calls `stream.Recv()` for the next chunk. Returns `io.EOF` when the stream ends (including after receiving a zero-length chunk, which signals end-of-stream without an explicit error). Fills `p` as much as possible.

---

### Method: `(*grpcReadCloser).Close`
```go
func (r *grpcReadCloser) Close() error
```
**What it does:** Cancels the gRPC context, releasing the stream.

---

### Method: `(*Client).Write`
```go
func (c *Client) Write(ctx context.Context, path string, data []byte) error
```
**What it does:** Writes a file to MonoFS. For repo-scoped paths (first segment not starting with `_`), each repo gets its own storage ID for HRW routing. Strategy:
1. Try the HRW-assigned target node via `targetNode`.
2. Fall back: iterate all healthy nodes.
3. Calls `tryIngest` on the selected node, which sends an `IngestFileBatch` RPC with inline content.

**Called from:** `BlobStore.writeSingle` (ultimately from `putChunkedFromReaderWithData`), `TagStore.PutTag`, `TagStore.addToCatalog`.

---

### Method: `(*Client).resolveWritePaths`
```go
func (c *Client) resolveWritePaths(path string) (displayPath, storageID, filePath string)
```
**What it does:** Splits a logical path for MonoFS ingestion routing. For repo-scoped paths like `"doctor-query/_tags/latest"`, sets `displayPath` to `"<dataNS>/doctor-query"` (so each repo independently HRW-shards). For namespace-scoped paths like `"_blobs/..."` or `"_catalog"`, uses `dataNS` directly. Generates a `storageID` via `sharding.GenerateStorageID(displayPath)`.

**Called from:** `Write`.

---

### Function: `writeMetadata`
```go
func writeMetadata(filePath, storageID, displayPath string, data []byte) *pb.FileMetadata
```
**What it does:** Constructs a `FileMetadata` protobuf for ingestion. Includes path, storage ID, display path, size, mtime, mode `0644`, SHA-256 blob hash, source tag `"monofs-registry"`, inline content, and ingestion type flags.

**Called from:** `Write`.

---

### Method: `(*Client).targetNode`
```go
func (c *Client) targetNode(ctx context.Context, storageID, _ string) pb.MonoFSClient
```
**What it does:** Queries the router for the HRW-assigned node for the given `storageID`. Returns the corresponding gRPC client. Returns `nil` if there is only 1 node (skip HRW routing) or if the node is not in the client's map.

**Called from:** `Write`.

---

### Method: `(*Client).tryIngest`
```go
func (c *Client) tryIngest(ctx context.Context, node pb.MonoFSClient, meta *pb.FileMetadata, path string) error
```
**What it does:** Sends an `IngestFileBatch` RPC to the given node with a single file's metadata. Returns `nil` if `resp.GetSuccess()` is true, otherwise returns an error with the response message.

**Called from:** `Write`.

---

### Method: `(*Client).Exists`
```go
func (c *Client) Exists(ctx context.Context, path string) (bool, error)
```
**What it does:** Checks for file existence by calling `GetAttr` RPC on healthy nodes. Returns `true` if any node responds successfully, `false` if any node responds with `NotFound`, otherwise `false, nil` (no definitive answer).

**Called from:** `BlobStore.Exists`, `BlobStore.Delete` (chunk iteration check).

---

### Method: `(*Client).Stat`
```go
func (c *Client) Stat(ctx context.Context, path string) (*pb.GetAttrResponse, error)
```
**What it does:** Returns file attributes via `GetAttr` RPC. Iterates healthy nodes until one responds with `Found == true`. Returns `os.ErrNotExist` if none found.

**Called from:** `BlobStore.Size`.

---

### Method: `(*Client).Delete`
```go
func (c *Client) Delete(ctx context.Context, path string) error
```
**What it does:** Deletes a file via `DeleteFile` RPC. Sends to the first healthy node. Ignores `NotFound` errors (treats as success).

**Called from:** `BlobStore.Delete`, `TagStore.DeleteTag`.

---

### Method: `(*Client).ListDir`
```go
func (c *Client) ListDir(ctx context.Context, path string) ([]string, error)
```
**What it does:** Lists directory entries via streaming `ReadDir` RPC. Iterates entries from the stream, collecting their names. Returns empty slice on `NotFound`. Returns whatever entries were received if the stream ends prematurely with an error.

**Called from:** `TagStore.ListTags`, `TagStore.listReposLocked`, `TagStore.ListReposAll`.

---

### Method: `(*Client).CreateDir`
```go
func (c *Client) CreateDir(ctx context.Context, path string) error
```
**What it does:** **No-op.** Directory creation is handled implicitly by MonoFS's `IngestFileBatch` RPC during writes. Retained for API compatibility.

**Called from:** `BlobStore.writeSingle` (which writes files — the directory creation is implicitly handled and this call is harmless).

---

### Method: `(*Client).register`
```go
func (c *Client) register(ctx context.Context) (time.Duration, error)
```
**What it does:** Registers this registry client with the MonoFS router via `RegisterClient` RPC. Sends `clientID`, hostname `"monofs-registry"`, version `"registry"`, and guardian config with role `"registry"`. Returns the heartbeat interval from the server response, or `defaultClientHeartbeat` if not specified.

**Called from:** `NewClient` (initial registration), `startHeartbeatLoop` (re-registration when router requests it).

---

### Method: `(*Client).refreshNodes`
```go
func (c *Client) refreshNodes(ctx context.Context) error`
```
**What it does:** Fetches cluster topology from the router via `GetClusterInfo` RPC. Filters to healthy nodes (non-nil, healthy=true, non-empty node ID and address). Under the write-lock:
- Dials new gRPC connections for any node address that changed.
- Removes connections for nodes no longer present in the cluster.
- Updates `nodeConns`, `nodeClients`, `nodeAddrs`, and `lastRefresh`.

Returns error if zero healthy nodes found.

**Called from:** `NewClient` (initial), `healthyNodes` (periodic refresh when TTL expired).

---

### Method: `(*Client).startHeartbeatLoop`
```go
func (c *Client) startHeartbeatLoop(interval time.Duration)
```
**What it does:** Launches a background goroutine that sends `ClientHeartbeat` RPCs at the given interval. If the response indicates `ShouldRegister`, calls `register` again (for re-authentication or recovery). Runs until `stopHeartbeat` is closed.

**Called from:** `NewClient`.

---

### Method: `(*Client).stopHeartbeatLoop`
```go
func (c *Client) stopHeartbeatLoop()
```
**What it does:** Closes `stopHeartbeat` channel (idempotent via `sync.Once`) and waits for the heartbeat goroutine to finish via `WaitGroup`.

**Called from:** `Close`.

---

### Method: `(*Client).healthyNodes`
```go
func (c *Client) healthyNodes(ctx context.Context) ([]nodeTarget, error)
```
**What it does:** Returns a snapshot of healthy node targets. If the topology has not been refreshed within `refreshTTL` (10s), calls `refreshNodes` first. If refresh fails but stale nodes exist, returns the stale snapshot. Returns error if zero nodes.

**Called from:** `Read`, `ReadStream`, `Write`, `Exists`, `Stat`, `Delete`, `ListDir`.

---

### Method: `(*Client).snapshotNodeTargets`
```go
func (c *Client) snapshotNodeTargets() []nodeTarget
```
**What it does:** Converts the current `nodeClients` map into a `[]nodeTarget` slice under the caller's lock. NOTE: does **not** itself hold the lock — caller must hold `c.mu`.

**Called from:** `healthyNodes`.

---

### Function: `randomHex`
```go
func randomHex(n int) string
```
**What it does:** Generates `n` random bytes via `crypto/rand` and returns their hex encoding.

**Called from:** `NewClient` (generating a unique `ClientID` suffix).

---

### Function: `sha256Hex`
```go
func sha256Hex(data []byte) string
```
**What it does:** Returns `"sha256:" + hex_encoded_sha256(data)`.

**Called from:** `writeMetadata` (populating `BlobHash` field).

---

### Function: `storageIDFromNS`
```go
func storageIDFromNS(ns string) string
```
**What it does:** Returns `"registry-" + ns`. Defined but **not called** in the current codebase.

---

## proxy.go — Upstream registry proxy

This file provides a pull-through proxy that fetches blobs, manifests, and tags from upstream Docker/OCI registries when they are not found in the local MonoFS store.

### Types

#### `UpstreamConfig`
```go
type UpstreamConfig struct {
    Default  string
    Mappings map[string]string
    Username string
    Password string
    Cooldown time.Duration
}
```
Configures upstream resolution. `Mappings` maps repo prefixes to upstream registry URLs. `Default` is the fallback (defaults to `https://registry-1.docker.io`). `Cooldown` is declared but **not used** in the current code.

#### `Proxy`
```go
type Proxy struct {
    config UpstreamConfig
    blobs  *BlobStore
    tags   *TagStore
    stats  *Stats
    logger *slog.Logger
    client *http.Client
}
```
Pull-through proxy. Uses `blobs` and `tags` for local cache lookups/writes, `stats` for counters, and an HTTP client (10-minute timeout) for upstream requests.

#### `tokenResponse`
```go
type tokenResponse struct {
    Token       string `json:"token"`
    AccessToken string `json:"access_token"`
}
```
JSON structure for Docker registry token responses.

---

### Function: `NewProxy`
```go
func NewProxy(config UpstreamConfig, blobs *BlobStore, tags *TagStore, stats *Stats, logger *slog.Logger) *Proxy
```
**What it does:** Constructs a `Proxy` with an HTTP client configured with a 10-minute timeout.

**Called from:** `cmd/monofs-registry/main.go:118` and `Server.NewServer` (server.go:39, when creating a default proxy if none provided).

---

### Method: `(*Proxy).getToken`
```go
func (p *Proxy) getToken(ctx context.Context, upstream, repo string) string
```
**What it does:** Performs the OAuth token flow for Docker registries:
1. Sends a `GET /v2/` to the upstream; expects `401 Unauthorized`.
2. Parses the `Www-Authenticate` header for `realm`, `service`, `scope`.
3. If no scope, constructs `repository:<repo>:pull`.
4. Fetches the token URL and parses the JSON response, preferring `token` over `access_token`.

Returns empty string on any failure.

**Called from:** `doAuth`.

---

### Function: `parseAuthHeader`
```go
func parseAuthHeader(header string) (realm, service, scope string)
```
**What it does:** Parses a `Bearer realm="...",service="...",scope="..."` WWW-Authenticate header into its components. Strips the `Bearer ` prefix, splits on commas, and extracts key=value pairs.

**Called from:** `getToken`.

---

### Method: `(*Proxy).FetchBlob`
```go
func (p *Proxy) FetchBlob(ctx context.Context, repo, digest string) (int64, []byte, error)
```
**What it does:** Fetches a blob, with local-cache semantics:
1. Checks `blobs.Exists` — on hit, gets size, increments cache hits, returns size with nil data.
2. On miss, increments cache misses, resolves upstream, calls `fetchBlobFromUpstream`.
3. Updates `BytesFetched`, `BytesStored`, `BlobCount` stats.
4. Returns the blob data (so the caller can serve it from memory without re-reading from MonoFS) and size.

**Called from:** `Server.handleGetBlob` (server.go:304).

---

### Method: `(*Proxy).FetchManifest`
```go
func (p *Proxy) FetchManifest(ctx context.Context, repo, ref string) ([]byte, string, error)
```
**What it does:** Fetches a manifest:
1. Tries `tags.GetManifest` for local cache.
2. On cache hit: increments `CacheHits` and `BytesServed`, returns.
3. On cache miss: increments `CacheMisses`, resolves upstream, calls `fetchManifestFromUpstream`.
4. Caches the fetched manifest locally via `tags.PutManifest` (best-effort; logs warning on failure).
5. Updates `BytesFetched` stat.
6. Returns manifest data and digest.

**Called from:** `Server.handleGetManifest` (server.go:220).

---

### Method: `(*Proxy).FetchTags`
```go
func (p *Proxy) FetchTags(ctx context.Context, repo string) ([]string, error)
```
**What it does:** Fetches tag list from upstream via `GET /v2/<repo>/tags/list`. Handles `401` by retrying with auth token. Returns `os.ErrNotExist` on `404`, error on other non-200 statuses. Does **not** cache tags in the local `TagStore`.

**Called from:** `Server.handleListTags` (server.go:476).

---

### Method: `(*Proxy).resolveUpstream`
```go
func (p *Proxy) resolveUpstream(repo string) string
```
**What it does:** Determines the upstream registry URL for a repo. Iterates `config.Mappings`, checking if the repo starts with a configured prefix. Falls back to `config.Default`, then to `"https://registry-1.docker.io"`.

**Called from:** `FetchBlob`, `FetchManifest`, `FetchTags`, `doAuth` (indirectly via `getToken`).

---

### Method: `(*Proxy).doAuth`
```go
func (p *Proxy) doAuth(ctx context.Context, upstream, repo string) string
```
**What it does:** Thin wrapper around `getToken`. Returns the bearer token or empty string.

**Called from:** `fetchBlobFromUpstream`, `fetchManifestFromUpstream`, `FetchTags` (on 401).

---

### Method: `(*Proxy).fetchBlobFromUpstream`
```go
func (p *Proxy) fetchBlobFromUpstream(ctx context.Context, upstream, repo, digest string) (int64, []byte, error)
```
**What it does:** Internal method that performs the actual HTTP blob fetch:
1. `GET /v2/<repo>/blobs/<digest>`.
2. On 401, retries with auth token.
3. On 404, returns `os.ErrNotExist`.
4. On 200, **streams directly into MonoFS** via `blobs.PutUncheckedFromReaderWithData` (so blob is both stored and returned in memory).
5. Returns `resp.ContentLength`, the data, and any error.

**Called from:** `FetchBlob`.

---

### Method: `(*Proxy).fetchManifestFromUpstream`
```go
func (p *Proxy) fetchManifestFromUpstream(ctx context.Context, upstream, repo, ref string) ([]byte, string, error)
```
**What it does:** Internal method for manifest HTTP fetch:
1. `GET /v2/<repo>/manifests/<ref>` with Accept headers for Docker V2 and OCI manifest types.
2. On 401, retries with auth token.
3. On 404, returns `os.ErrNotExist`.
4. On 200, reads the full body and returns the data and `Content-Type` header.
5. Does **not** store the manifest in MonoFS (that's done by `FetchManifest`).

**Called from:** `FetchManifest`.

---

## server.go — HTTP server and OCI API handler

Implements the Docker Registry HTTP API v2, plus management endpoints for stats, repos, and health.

### Types

#### `Server`
```go
type Server struct {
    blobs   *BlobStore
    tags    *TagStore
    uploads *UploadManager
    proxy   *Proxy
    stats   *Stats
    client  *Client
    logger  *slog.Logger
    dataNS  string

    blobDownloadCount atomic.Int64
    blobUploadCount   atomic.Int64
}
```
Top-level registry server. Orchestrates blob storage, tag management, upload sessions, proxy, and stats.

#### `manifestHeader`
```go
type manifestHeader struct {
    MediaType string `json:"mediaType"`
}
```
Used to peek at the `mediaType` field of a manifest JSON for content-type detection.

---

### Variables

```go
var (
    ociIndexMediaType    = "application/vnd.oci.image.index.v1+json"
    ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
    dockerManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
    dockerManifestList   = "application/vnd.docker.distribution.manifest.list.v2+json"
)
```
Manifest media type constants.

---

### Function: `NewServer`
```go
func NewServer(client *Client, nextProxy *Proxy, logger *slog.Logger, dataNS string) *Server
```
**What it does:** Constructs the registry server. Creates sub-components (`BlobStore`, `TagStore`, `UploadManager`, `Stats`). If `nextProxy` is nil, creates a default proxy targeting Docker Hub. Starts the `uploadCleanupLoop` background goroutine.

**Called from:** `cmd/monofs-registry/main.go:119`.

---

### Method: `(*Server).uploadCleanupLoop`
```go
func (s *Server) uploadCleanupLoop()
```
**What it does:** Background goroutine. Every 10 minutes, calls `uploads.Cleanup(30 * time.Minute)` to remove stale upload sessions.

---

### Method: `(*Server).Handler`
```go
func (s *Server) Handler() http.Handler
```
**What it does:** Constructs the HTTP handler with route registration:
- `/v2/` → `handleV2` (Docker Registry API)
- `/api/v1/stats` → `handleStats`
- `/api/v1/repos` → `handleRepos`
- `/api/v1/repos/` → `handleRepoDetail`
- `/health` → `handleHealth`
- `/` → root (returns Docker-Distribution-API-Version header)
- All other paths → 404

Wraps the mux with `middleware`.

---

### Method: `(*Server).middleware`
```go
func (s *Server) middleware(next http.Handler) http.Handler
```
**What it does:** HTTP middleware that logs every request (method + path) and adds the `Docker-Distribution-API-Version: registry/2.0` header to all responses.

---

### Method: `(*Server).handleV2`
```go
func (s *Server) handleV2(w http.ResponseWriter, r *http.Request)
```
**What it does:** Main V2 API request router. Parses the URL path (stripped of `/v2/`) into segments, identifies the boundary between repo name and action (keywords: `manifests`, `blobs`, `tags`, `referrers`). Dispatches to specific handlers:
- `_catalog` → `handleCatalog`
- `manifests` → `handleGetManifest` / `handlePutManifest` / `handleDeleteManifest`
- `blobs/<digest>` → `handleGetBlob` / `handleDeleteBlob`
- `blobs/uploads` → `handleStartUpload` (POST)
- `blobs/uploads/<uuid>` → `handleUploadChunk` (PATCH) / `handleUploadComplete` (PUT) / `handleUploadCancel` (DELETE)
- `tags/list` → `handleListTags`
- `referrers/<digest>` → `handleReferrers`

Handles multi-segment repo names like `library/alpine` or `grafana/grafana`.

---

### Method: `(*Server).handleGetManifest`
```go
func (s *Server) handleGetManifest(w http.ResponseWriter, r *http.Request, repo, ref string)
```
**What it does:** Handles `GET /v2/<repo>/manifests/<ref>`:
1. Tries local `tags.GetManifest`. On hit: returns manifest with detected content-type, digest header, and content-length.
2. On miss: tries proxy `proxy.FetchManifest` (pull-through cache to upstream).
3. On failure: returns OCI `MANIFEST_UNKNOWN` error.
4. On HEAD: returns headers only (200 with content-length but no body).
5. Increments `Pulls` and `BytesServed` stats.

---

### Method: `(*Server).handlePutManifest`
```go
func (s *Server) handlePutManifest(w http.ResponseWriter, r *http.Request, repo, ref string)
```
**What it does:** Handles `PUT /v2/<repo>/manifests/<ref>`:
1. Reads the request body.
2. Calls `tags.PutManifest` to store the manifest and update the tag.
3. Returns `201 Created` with `Docker-Content-Digest` and `Location` headers.
4. Increments `Pushes` and `BytesServed` stats.

---

### Method: `(*Server).handleDeleteManifest`
```go
func (s *Server) handleDeleteManifest(w http.ResponseWriter, r *http.Request, repo, ref string)
```
**What it does:** Handles `DELETE /v2/<repo>/manifests/<ref>`:
1. Calls `tags.DeleteManifest`.
2. Returns `202 Accepted` on success, `MANIFEST_UNKNOWN` error on failure.

---

### Method: `(*Server).handleGetBlob`
```go
func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request, repo, digestStr string)
```
**What it does:** Handles `GET /v2/<repo>/blobs/<digest>`:
1. Tries `blobs.GetReader` for local blob.
2. On hit (HEAD): returns 200 with `Content-Length` from `blobs.Size`. Returns 404 if size metadata is corrupt.
3. On hit (GET): streams the blob via `io.Copy`; increments `BytesServed`.
4. On miss: tries proxy `proxy.FetchBlob`.
   - If proxy returned data in memory (common for blobs ≤512 MiB): serves directly.
   - If proxy stored but didn't return data (cache hit or >512 MiB): reads back via `blobs.GetReader`.
5. Both paths increment `Pulls` stat.
6. Returns `BLOB_UNKNOWN` OCI error on total failure.

---

### Method: `(*Server).handleDeleteBlob`
```go
func (s *Server) handleDeleteBlob(w http.ResponseWriter, r *http.Request, repo, digestStr string)
```
**What it does:** Handles `DELETE /v2/<repo>/blobs/<digest>`:
1. Retrieves current size (for stats adjustment).
2. Calls `blobs.Delete`.
3. Decrements `BytesStored` stat by the deleted size.
4. Always returns `202 Accepted` (best-effort delete).

---

### Method: `(*Server).handleStartUpload`
```go
func (s *Server) handleStartUpload(w http.ResponseWriter, r *http.Request, repo string)
```
**What it does:** Handles `POST /v2/<repo>/blobs/uploads/`:
1. Creates a new `UploadSession` via `uploads.Start`.
2. Returns `202 Accepted` with `Docker-Upload-UUID`, `Location`, and `Range: 0-0` headers.

---

### Method: `(*Server).handleUploadChunk`
```go
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request, repo, uuid string)
```
**What it does:** Handles `PATCH /v2/<repo>/blobs/uploads/<uuid>`:
1. Validates the upload session exists and repo matches.
2. Appends request body to the session via `uploads.Append`.
3. Returns `202 Accepted` with updated `Range` header.

---

### Method: `(*Server).handleUploadComplete`
```go
func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request, repo, uuid string)
```
**What it does:** Handles `PUT /v2/<repo>/blobs/uploads/<uuid>?digest=<digest>`:
1. Validates session exists and repo matches.
2. Validates the `?digest=` query parameter is a valid digest.
3. If `ContentLength > 0`, appends final chunk from the request body.
4. Calls `session.Digest()` to compute the SHA-256 of all uploaded data.
5. **Validates** the computed digest matches the query parameter — if not, removes session and returns `DIGEST_INVALID`.
6. Stores the blob via `blobs.PutUncheckedFromReader`, reading from the temp file.
7. Removes the upload session, updates stats (`Pushes`, `BytesServed`, `BytesStored`, `BlobCount`).
8. Returns `201 Created` with `Docker-Content-Digest` and `Location` headers.

---

### Method: `(*Server).handleUploadCancel`
```go
func (s *Server) handleUploadCancel(w http.ResponseWriter, r *http.Request, repo, uuid string)
```
**What it does:** Handles `DELETE /v2/<repo>/blobs/uploads/<uuid>`:
1. Removes the upload session via `uploads.Remove` (closes and deletes the temp file).
2. Returns `204 No Content`.

---

### Method: `(*Server).handleListTags`
```go
func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request, _ string)
```
**What it does:** Handles `GET /v2/<repo>/tags/list`:
1. Extracts repo name from the URL path.
2. Calls `tags.ListTags`.
3. On local miss and proxy configured: falls back to `proxy.FetchTags`.
4. Returns JSON `{"name": "<repo>", "tags": [...]}`.

---

### Method: `(*Server).handleStats`
```go
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request)
```
**What it does:** Handles `GET /api/v1/stats`: Returns a JSON snapshot of all atomic stats counters.

---

### Method: `(*Server).handleRepos`
```go
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request)
```
**What it does:** Handles `GET /api/v1/repos`: Returns `{"repositories": [...]}` using `tags.ListRepos`.

---

### Method: `(*Server).handleCatalog`
```go
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request)
```
**What it does:** Handles `GET /v2/_catalog`: Returns `{"repositories": [...]}` using `tags.ListReposAll` (always rescans from storage).

---

### Method: `(*Server).handleRepoDetail`
```go
func (s *Server) handleRepoDetail(w http.ResponseWriter, r *http.Request)
```
**What it does:** Handles `GET /api/v1/repos/<name>`: Returns repo name and tags with their digest values. Uses `tags.ListTags` and `tags.GetTag` to build a detailed tag list.

---

### Method: `(*Server).handleHealth`
```go
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request)
```
**What it does:** Handles `GET /health`: Returns `{"status":"ok"}` with status 200.

---

### Function: `writeOCIError`
```go
func writeOCIError(w http.ResponseWriter, code, message string)
```
**What it does:** Writes an OCI-compliant error JSON: `{"errors": [{"code": "...", "message": "..."}]}`. Maps OCI error codes to HTTP status codes:
- `BLOB_UNKNOWN`, `MANIFEST_UNKNOWN` → 404
- `NAME_INVALID`, `DIGEST_INVALID`, `MANIFEST_INVALID`, `BLOB_UPLOAD_INVALID` → 400
- `BLOB_UPLOAD_UNKNOWN` → 404
- `UNAUTHORIZED` → 401
- Everything else → 500

**Called from:** All server handler methods on error.

---

### Method: `(*Server).handleReferrers`
```go
func (s *Server) handleReferrers(w http.ResponseWriter, r *http.Request, repo, ref string)
```
**What it does:** Handles `GET /v2/<repo>/referrers/<digest>`: Returns an empty OCI image index (`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`). This is a stub — the registry does not support referrers API and always returns an empty list.

---

### Function: `detectManifestContentType`
```go
func detectManifestContentType(data []byte) string
```
**What it does:** Peek at the `mediaType` field in a manifest JSON to return the correct `Content-Type` header. Falls back to `dockerManifestV2` (`application/vnd.docker.distribution.manifest.v2+json`) if parsing fails or mediaType is unrecognized.

**Called from:** `handleGetManifest` (both local and proxy paths).

---

## stats.go — Statistics tracking

Provides atomic counters for the registry's operational metrics.

### Types

#### `Stats`
```go
type Stats struct {
    Pulls        atomic.Int64
    Pushes       atomic.Int64
    CacheHits    atomic.Int64
    CacheMisses  atomic.Int64
    BytesServed  atomic.Int64
    BytesFetched atomic.Int64
    BytesStored  atomic.Int64
    BlobCount    atomic.Int64
}
```
Thread-safe counters using `sync/atomic`. Fields are public to allow direct `Add`/`Load` calls from any goroutine.

#### `StatsSnapshot`
```go
type StatsSnapshot struct {
    Pulls        int64 `json:"pulls"`
    Pushes       int64 `json:"pushes"`
    CacheHits    int64 `json:"cache_hits"`
    CacheMisses  int64 `json:"cache_misses"`
    BytesServed  int64 `json:"bytes_served"`
    BytesFetched int64 `json:"bytes_fetched"`
    BytesStored  int64 `json:"bytes_stored"`
    BlobCount    int64 `json:"blob_count"`
}
```
Immutable snapshot of `Stats`, used for JSON serialization.

#### `RepoStat`
```go
type RepoStat struct {
    Name     string   `json:"name"`
    Tags     []string `json:"tags"`
    BlobSize int64    `json:"blob_size_bytes"`
}
```
Represents a single repository's statistics. Defined but **not used** in the current codebase; likely intended for future use.

---

### Method: `(*Stats).Snapshot`
```go
func (s *Stats) Snapshot() StatsSnapshot
```
**What it does:** Atomically loads all counters and returns an immutable `StatsSnapshot`.

**Called from:** `Server.handleStats` (server.go:502).

---

## uploads.go — Upload session management

Manages chunked blob uploads as specified by the Docker Registry API. Each upload session uses a temp file on disk.

### Types

#### `UploadSession`
```go
type UploadSession struct {
    ID        string    `json:"id"`
    Repo      string    `json:"repo"`
    File      *os.File  `json:"-"`
    Size      int64     `json:"size"`
    StartedAt time.Time `json:"started_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```
Represents an in-progress chunked blob upload. Data is accumulated in a temporary file (`File`) created by `os.CreateTemp`. `Size` tracks the total bytes appended.

#### `UploadManager`
```go
type UploadManager struct {
    mu       sync.RWMutex
    sessions map[string]*UploadSession
}
```
Thread-safe manager for all active upload sessions.

#### `uploadStateJSON`
```go
type uploadStateJSON struct {
    ID        string `json:"id"`
    Repo      string `json:"repo"`
    Size      int64  `json:"size"`
    StartedAt int64  `json:"started_at"`
}
```
Serializable representation of upload state (excluding the `*os.File` handle).

---

### Function: `NewUploadManager`
```go
func NewUploadManager() *UploadManager
```
**What it does:** Constructs an empty `UploadManager`.

**Called from:** `Server.NewServer` (server.go:35).

---

### Method: `(*UploadManager).Start`
```go
func (um *UploadManager) Start(repo string) (*UploadSession, error)
```
**What it does:** Creates a new upload session:
1. Creates a temp file with prefix `monofs-registry-upload-*` in the OS default temp directory.
2. Generates a UUID session ID.
3. Registers the session in the internal map.
4. Returns the session.

**Called from:** `Server.handleStartUpload` (server.go:373).

---

### Method: `(*UploadManager).Get`
```go
func (um *UploadManager) Get(id string) (*UploadSession, bool)
```
**What it does:** Looks up an upload session by ID. Returns the session and a boolean indicating existence. Uses `RLock` for concurrent reads.

**Called from:** `Server.handleUploadChunk`, `Server.handleUploadComplete` (server.go:386, 407).

---

### Method: `(*UploadManager).Append`
```go
func (um *UploadManager) Append(id string, r io.Reader) (*UploadSession, int64, error)
```
**What it does:** Appends data from an `io.Reader` to the session's temp file via `io.Copy`. Increments `Size`, updates `UpdatedAt`. Returns the session, the number of bytes written, and any error. Holds a write-lock.

**Called from:** `Server.handleUploadChunk`, `Server.handleUploadComplete` (server.go:393, 427).

---

### Method: `(*UploadManager).Remove`
```go
func (um *UploadManager) Remove(id string)
```
**What it does:** Removes an upload session: closes the temp file, deletes it from disk via `os.Remove`, and deletes from the sessions map. Idempotent (no-op if session not found).

**Called from:** `Server.handleUploadComplete` (success), `Server.handleUploadCancel`, `Server.uploadCleanupLoop` (via `Cleanup`), and `handleUploadComplete` on digest mismatch (server.go:436, 441, 452, 464, and indirectly via `Cleanup`).

---

### Method: `(*UploadManager).Cleanup`
```go
func (um *UploadManager) Cleanup(maxAge time.Duration) int
```
**What it does:** Removes all sessions whose `UpdatedAt` is before `time.Now() - maxAge`. For each stale session, closes the temp file and deletes it from disk. Returns the count of removed sessions.

**Called from:** `Server.uploadCleanupLoop` (server.go:63), with `maxAge = 30 * time.Minute`.

---

### Method: `(*UploadSession).SeekToStart`
```go
func (s *UploadSession) SeekToStart() error
```
**What it does:** Seeks the temp file to offset 0 (beginning of file). Used to reset the read position before computing a digest.

**Called from:** `Digest` (line 110).

---

### Method: `(*UploadSession).Digest`
```go
func (s *UploadSession) Digest() (string, error)
```
**What it does:** Computes the SHA-256 digest of the accumulated upload data:
1. Seeks to the start of the temp file.
2. Hashes the entire file content via `io.Copy` into a `sha256.New()` hasher.
3. Seeks back to the start (for potential re-read).
4. Returns `"sha256:<hex>"`.

**Called from:** `Server.handleUploadComplete` (server.go:434).

---

### Method: `(*UploadManager).MarshalJSON`
```go
func (um *UploadManager) MarshalJSON() ([]byte, error)
```
**What it does:** Serializes all active upload sessions to JSON (excluding the file handles). Returns an array of `uploadStateJSON` objects. Defined but **not called** in the current codebase; likely for debugging/inspection.

---

## Cross-file call graph summary

```
cmd/monofs-registry/main.go
  ├── NewClient         → monofs_client.go
  ├── NewBlobStore      → blobs.go
  ├── NewTagStore       → manifests.go
  ├── NewProxy          → proxy.go
  ├── NewServer         → server.go
  │     ├── NewBlobStore
  │     ├── NewTagStore
  │     ├── NewUploadManager → uploads.go
  │     ├── NewProxy (if nil)
  │     └── uploadCleanupLoop
  └── Handler()         → server.go
        ├── handleV2 → all manifest/blob/tag/referrer handlers
        ├── handleStats, handleRepos, handleRepoDetail, handleHealth
        └── middleware
```

All server handlers → BlobStore, TagStore, UploadManager, Proxy, Stats (atomic counters).
Proxy → BlobStore, TagStore, Stats, HTTP upstream requests.
TagStore → Client (for tag/catalog I/O), BlobStore (for manifest content).
BlobStore → Client (for blob I/O).
