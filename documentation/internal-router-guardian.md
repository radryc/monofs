# Internal Router — Guardian Documentation

Generated from:
- `internal/router/guardian_inject.go`
- `internal/router/guardian_path_mapper.go`
- `internal/router/guardian_paths.go`
- `internal/router/guardian_principals.go`
- `internal/router/guardian_version_store.go`

---

## File: `guardian_inject.go`

### Type: `guardianNodeTarget`

```go
type guardianNodeTarget struct {
    id        string
    address   string
    client    pb.MonoFSClient
    kvsStatus *pb.KVSNodeStatus
}
```

Internal struct holding a reference to a specific cluster node for guardian operations. Contains the node's ID, address, a gRPC client (may be nil), and the node's KVS status (normalized via `normalizedKVSNodeStatus`).

---

### `(r *Router) InjectGuardianPartition(ctx context.Context, req *pb.InjectGuardianPartitionRequest) (*pb.InjectGuardianPartitionResponse, error)`

**What it does:** Stores inline YAML files for a guardian partition directly on all cluster nodes, bypassing the git/S3 ingestion pipeline.

**Called from:** External gRPC callers (guardian control-plane or CLI clients).

**Parameters:**
- `req.PartitionName` — required partition name.
- `req.Files` — slice of `InjectGuardianFile` (each with `Path` and `Content` bytes).
- `req.GuardianToken` — a valid guardian client token.

**Returns:** A response with `Success`, `StorageId` (sha256-based), `Message`, and `FilesIngested` count.

**Implementation details:** Each file path is cleaned via `cleanGuardianRelativePath`, then prepended with `/partitions/<partitionName>/`. The batch is forwarded to `UpsertGuardianPaths`. On error, returns a response with `Success: false` alongside the error.

---

### `(r *Router) collectHealthyGuardianNodes() []guardianNodeTarget`

**What it does:** Gathers all cluster nodes that are currently marked healthy.

**Called from:**
- `guardianKVSMutationTarget` (this file)
- `DeleteGuardianPaths` (`guardian_paths.go`)
- `applyGuardianUpsertGroups` (`guardian_paths.go`)
- `guardianNodeClientForPath` (`pipeline_kvs.go`)

**Returns:** A slice of `guardianNodeTarget` containing only nodes where `state.info.Healthy == true`, each including their client connection and normalized KVS status.

**Implementation details:** Holds `r.mu.RLock()` for the duration.

---

### `(r *Router) guardianMutationTargets(nodes []guardianNodeTarget, displayPath string) []guardianNodeTarget`

**What it does:** Determines which nodes to target for a guardian mutation. For multi-node setups where the storage backend is KVS, it routes to a single node (the KVS leader). Otherwise returns all nodes.

**Called from:**
- `guardianKVSMutationTarget` (this file)
- `applyGuardianUpsertGroup` (`guardian_paths.go`)

**Parameters:**
- `nodes` — healthy nodes from `collectHealthyGuardianNodes`.
- `displayPath` — used to determine the storage backend via `guardianRepoStorageBackend`.

**Returns:** The reduced set of target nodes.

**Implementation details:** If there's only 1 node, or the storage backend is not `"kvs"`, returns all nodes unchanged. For KVS backends, picks the leader via `pickGuardianKVSLeader`; if no leader is found, falls back to deterministic selection (lowest `id`, then `address`).

---

### `(r *Router) guardianKVSMutationTarget(displayPath string) (guardianNodeTarget, bool)`

**What it does:** Resolves the single KVS mutation target for a display path, if applicable.

**Called from:**
- `DeleteGuardianPaths` (`guardian_paths.go`)

**Parameters:**
- `displayPath` — used to check if the backend is KVS.

**Returns:** The KVS leader target and `true`, or zero-value and `false` if the path is not KVS-backed or no single target could be resolved.

**Implementation details:** Short-circuits to `false` if `guardianRepoStorageBackend(displayPath) != "kvs"`. Otherwise combines `collectHealthyGuardianNodes` + `guardianMutationTargets`.

---

### `(r *Router) guardianDisplayPathByStorageID(storageID string) string`

**What it does:** Looks up the display path (repo ID) for a given storage ID from the router's `ingestedRepos` map.

**Called from:**
- `deleteGuardianFileFromAllNodes` (`delete.go`)
- `deleteGuardianDirFromAllNodes` (`delete.go`)

**Returns:** The display path string, or `""` if the storage ID is unknown.

---

### `pickGuardianKVSLeader(nodes []guardianNodeTarget) (guardianNodeTarget, bool)`

**What it does:** Selects the KVS leader node from a set of guardian node targets.

**Called from:** `guardianMutationTargets` (this file).

**Returns:** The leader node and `true`, or zero-value and `false`.

**Implementation details:** Iterates nodes looking for one whose normalized KVS status has `Enabled && Role == "leader"`. If no node declares itself as leader, falls back to checking `LeaderId` from node status and matching by node ID.

---

### `(r *Router) guardianNodeClient(target guardianNodeTarget) (pb.MonoFSClient, func(), error)`

**What it does:** Returns a gRPC client for a guardian node target, creating one if not already cached.

**Called from:**
- `lookupGuardianExistingFiles` (this file)
- `applyGuardianUpsertGroup` (`guardian_paths.go`)
- `DeleteGuardianPaths` (`guardian_paths.go`)
- `guardianNodeClientForPath` (`pipeline_kvs.go`)
- `deleteGuardianFileFromAllNodes` (`delete.go`)
- `deleteGuardianDirFromAllNodes` (`delete.go`)

**Returns:** A `pb.MonoFSClient`, a cleanup function (no-op if cached, closes a new connection otherwise), and an error.

**Implementation details:** If `target.client` is non-nil (from the router's cached node connections), returns it with a no-op cleanup. Otherwise dials a new gRPC connection with insecure credentials and wraps it in a `MonoFSClient`.

---

### `(r *Router) lookupGuardianExistingFiles(ctx context.Context, nodes []guardianNodeTarget, displayPath string, files []*pb.InjectGuardianFile) map[string]bool`

**What it does:** Checks which files from an inject request already exist on the first healthy node.

**Called from:** `InjectGuardianPartition` (this file) (indirectly, see note below — this function is defined but not directly called in the files we read; it may be called elsewhere or be dead code).

**Returns:** A `map[string]bool` keyed by cleaned relative path, true if the file was found.

**Implementation details:** For each file, calls `GetAttr` on the display path with a 5-second timeout. Populates the map for paths where `resp.Found` is true.

---

### `(r *Router) publishGuardianInjectedFiles(storageID string, files []*pb.InjectGuardianFile, existing map[string]bool)`

**What it does:** Publishes change events for each injected file, marking them as ADDED or MODIFIED based on prior existence.

**Called from:** (Defined but caller not found in the files reviewed — likely called from `InjectGuardianPartition` or similar).

**Parameters:**
- `storageID` — the storage ID for the partition.
- `files` — injected files.
- `existing` — map from `lookupGuardianExistingFiles`.

**Implementation details:** Content < 64KB is inlined in the change event. Uses `guardianContentHash` for the blob hash. Publishes via `r.publishGuardianChange`.

---

### `cleanGuardianRelativePath(input string) string`

**What it does:** Normalizes a raw file path string for guardian operations: trims whitespace, strips leading `/`, runs `path.Clean`, and rejects ".".

**Called from:**
- `InjectGuardianPartition` (this file)
- `lookupGuardianExistingFiles` (this file)
- `publishGuardianInjectedFiles` (this file)
- `mapGuardianLogicalPath` (`guardian_path_mapper.go`)
- `guardianLogicalPathFromPhysical` (`guardian_path_mapper.go`)
- `guardianDisplayPathJoin` — used indirectly.

**Returns:** Cleaned relative path, or `""` if the input is empty/dot.

---

### `guardianContentHash(content []byte) string`

**What it does:** Computes a SHA-256 hex digest of arbitrary byte content.

**Called from:**
- `publishGuardianInjectedFiles` (this file)
- `processGuardianUpsert` (`guardian_paths.go`)

**Returns:** Lowercase hex string.

---

## File: `guardian_path_mapper.go`

### Type: `guardianPhysicalPath`

```go
type guardianPhysicalPath struct {
    LogicalPath  string
    DisplayPath  string
    RelativePath string
    StorageID    string
}
```

Holds the decomposed representation of a guardian logical path: the original normalized logical path, the display path (repo-level prefix), the relative path within that repository, and the derived storage ID.

---

### `normalizeGuardianLogicalPath(input string) (string, error)`

**What it does:** Validates and canonicalizes a guardian logical path. Ensures it starts with `/`, runs `path.Clean`, rejects root-only paths and paths containing `..`.

**Called from:**
- `mapGuardianLogicalPath` (this file)
- `ListGuardianVersions` (`guardian_paths.go`)
- `GetGuardianVersion` (`guardian_paths.go`)
- `normalizeGuardianLogicalPrefixes` (`guardian_paths.go`)

**Returns:** Cleaned absolute path, or an error if the input is empty, resolves to root, or contains `..`.

---

### `mapGuardianLogicalPath(logicalPath string) (guardianPhysicalPath, error)`

**What it does:** Maps a guardian logical path to its physical representation (display path + relative path + storage ID).

**Called from:**
- `processGuardianUpsert` (`guardian_paths.go`)
- `DeleteGuardianPaths` (`guardian_paths.go`)
- `publishLegacyGuardianChange` (`guardian_paths.go`)
- `processGuardianDelete` (`pipeline_kvs.go`)

**Returns:** A `guardianPhysicalPath` struct, or an error.

**Implementation details:** Dispatches on the first path segment:
- `partitions` → displayPath `guardian/<partitionName>`
- `doctor` → displayPath `doctor/<version>`
- `.queues` or `.archive` → displayPath `guardian-system`
- Anything else → error (outside managed namespace)

---

### `guardianLogicalPathFromPhysical(displayPath, relativePath string) (string, error)`

**What it does:** Reconstructs a guardian logical path from a display path and relative path (reverse of `mapGuardianLogicalPath`).

**Called from:** Not found in the reviewed files — might be used by UI or API handlers not covered here.

**Returns:** The reconstructed logical path or an error.

**Implementation details:** Handles the three canonical display path forms (`guardian-system`, `doctor/<version>`, `guardian/<partition>`).

---

### `guardianDisplayPathJoin(displayPath, relativePath string) string`

**What it does:** Joins a display path and relative path into a single path string.

**Called from:**
- `DeleteGuardianPaths` (`guardian_paths.go`)

**Returns:** `displayPath + "/" + relativePath`, or just `displayPath` if `relativePath` is empty.

---

## File: `guardian_paths.go`

### Type: `guardianLogicalChangeSubscriber`

```go
type guardianLogicalChangeSubscriber struct {
    id                 uint64
    logicalPrefixes    []string
    includeInlineBytes bool
    events             chan *pb.GuardianChangeEvent
}
```

Represents a stream subscriber for guardian logical change events, identified by a unique sequence number, filtering events by logical path prefixes, with an optional flag to include inline content.

---

### Type: `guardianUpsertGroup`

```go
type guardianUpsertGroup struct {
    displayPath string
    storageID   string
    files       []*pb.FileMetadata
}
```

Groups upsert files by their display path and storage ID, batched for single-node ingestion.

---

### Type: `guardianUpsertPlan`

```go
type guardianUpsertPlan struct {
    write      *pb.GuardianPathWrite
    mapped     guardianPhysicalPath
    changeType pb.ChangeType
}
```

Holds the resolved plan for a single upsert: the original write request, the mapped physical path, and whether the operation is ADDED or MODIFIED.

---

### Type: `guardianDeletePlan`

```go
type guardianDeletePlan struct {
    deleteReq    *pb.GuardianPathDelete
    mapped       guardianPhysicalPath
    existing     *storedGuardianFileVersion
    content      []byte
    changeType   pb.ChangeType
    physicalPath string
    isDir        bool
    deleteRepo   bool
    needsDelete  bool
}
```

Holds the resolved plan for a single delete, including whether the target is a repository delete, directory delete, or file delete, along with fetched content for tombstone recording.

---

### `guardianRepoStorageBackend(displayPath string) string`

**What it does:** Returns the storage backend override for a display path.

**Called from:**
- `guardianMutationTargets` (`guardian_inject.go`)
- `guardianKVSMutationTarget` (`guardian_inject.go`)
- `processGuardianUpsert` (this file)
- `applyGuardianRepoStorageBackend` (this file)

**Returns:** `"kvs"` for `guardian-system`, empty string otherwise (default backend).

---

### `applyGuardianRepoStorageBackend(req *pb.RegisterRepositoryRequest) *pb.RegisterRepositoryRequest`

**What it does:** Mutates a `RegisterRepositoryRequest` to inject `storage_backend` into fetch/ingestion config if the display path requires KVS storage.

**Called from:** `applyGuardianUpsertGroup` (this file).

**Returns:** The (potentially modified) request. Nil-safe.

---

### `(r *Router) UpsertGuardianPathsStream(stream grpc.ClientStreamingServer[pb.GuardianPathWriteChunk, pb.UpsertGuardianPathsResponse]) error`

**What it does:** gRPC streaming endpoint that accumulates `GuardianPathWriteChunk` messages until the last chunk, then calls `processGuardianUpsert`.

**Called from:** External gRPC client streaming callers.

**Implementation details:** Collects `GuardianToken` and `GuardianMutationContext` from the first chunk that provides them. Requires at least one write across all chunks. Uses `SendAndClose` for the response.

---

### `(r *Router) UpsertGuardianPaths(ctx context.Context, req *pb.UpsertGuardianPathsRequest) (*pb.UpsertGuardianPathsResponse, error)`

**What it does:** Primary RPC handler for upserting guardian files (unary version). Delegates to `processGuardianUpsert`.

**Called from:**
- `InjectGuardianPartition` (`guardian_inject.go`)
- External gRPC callers.

**Returns:** Upsert response with batch revision ID, version records, and success message.

---

### `(r *Router) processGuardianUpsert(ctx context.Context, req *pb.UpsertGuardianPathsRequest) (*pb.UpsertGuardianPathsResponse, error)`

**What it does:** Core implementation for guardian path upserts. Authenticates the caller, maps logical paths, performs optimistic concurrency checks, builds file metadata, ingests to nodes, commits versions, and publishes change events.

**Called from:**
- `UpsertGuardianPaths` (this file)
- `UpsertGuardianPathsStream` (this file)

**Key steps:**
1. Authenticate via `authenticateGuardianMutation`.
2. Map each logical path using `mapGuardianLogicalPath` (rejects bare directory paths).
3. Authorize each write via `authorizeGuardianMutation`.
4. Check `ExpectedVersionId` for optimistic concurrency: `""` = no check, `"absent"` = must not exist, specific value = must match current version.
5. Group files by `DisplayPath` into `guardianUpsertGroup` structs.
6. Call `applyGuardianUpsertGroups` to register repos + ingest to nodes.
7. Commit each version via `r.guardianVersions.commit`.
8. Publish change events via `publishGuardianLogicalChange` and `publishLegacyGuardianChange`.
9. Update metrics (`routerGuardianUpsert*`).

---

### `(r *Router) DeleteGuardianPaths(ctx context.Context, req *pb.DeleteGuardianPathsRequest) (*pb.DeleteGuardianPathsResponse, error)`

**What it does:** Primary RPC handler for deleting guardian files, directories, or entire partition repositories.

**Called from:** External gRPC callers.

**Key steps:**
1. Authenticate + authorize.
2. For each delete request, resolve the target type:
   - Empty relative path → repository delete (`deleteRepo = true`).
   - `guardian-system` root → rejected.
   - File/directory → calls `GetAttr` to check existence and determine type.
3. Execute deletes: `deleteRepositoryInternal`, `deleteGuardianDirFromAllNodes`, or `deleteGuardianFileFromAllNodes`.
4. Commit tombstones via `r.guardianVersions.commit`.
5. Publish change events (logical + legacy).
6. Update metrics (`routerGuardianDelete*`).

---

### `(r *Router) ListGuardianVersions(ctx context.Context, req *pb.ListGuardianVersionsRequest) (*pb.ListGuardianVersionsResponse, error)`

**What it does:** Lists version history for a guardian logical path with pagination.

**Called from:** External gRPC callers.

**Parameters:**
- `req.GuardianToken` — validated via `authenticateGuardianToken`.
- `req.LogicalPath` — normalized via `normalizeGuardianLogicalPath`.
- `req.PageSize` and `req.PageToken` — pagination controls.

**Returns:** A paginated list of `GuardianFileVersion` protos, with an opaque next page token.

---

### `(r *Router) GetGuardianVersion(ctx context.Context, req *pb.GetGuardianVersionRequest) (*pb.GetGuardianVersionResponse, error)`

**What it does:** Retrieves a specific version of a guardian file by version ID, including its content.

**Called from:** External gRPC callers.

**Parameters:**
- `req.VersionId` — required.
- `req.LogicalPath` — normalized via `normalizeGuardianLogicalPath`.

**Returns:** The version proto and raw content, or `NotFound`.

---

### `(r *Router) SubscribeGuardianChanges(req *pb.SubscribeGuardianChangesRequest, stream grpc.ServerStreamingServer[pb.GuardianChangeEvent]) error`

**What it does:** Establishes a server-streaming subscription for guardian logical change events, filtered by logical path prefixes.

**Called from:** External gRPC stream callers.

**Implementation details:**
- Normalizes prefixes via `normalizeGuardianLogicalPrefixes` (empty prefixes = subscribe to all).
- Creates a buffered channel subscriber (`guardianChangeBufferSize = 128`).
- Registers the subscriber in `r.guardianLogicalChangeSubs`.
- Blocks sending events until the client disconnects.
- On return, unregisters the subscriber.

---

### `(r *Router) applyGuardianUpsertGroups(ctx context.Context, groups map[string]*guardianUpsertGroup, principal *guardianPrincipal) error`

**What it does:** Iterates over upsert groups and applies each to nodes.

**Called from:** `processGuardianUpsert` (this file).

**Implementation details:** Collects healthy nodes once, then calls `applyGuardianUpsertGroup` for each group. Returns the first error.

---

### `(r *Router) applyGuardianUpsertGroup(ctx context.Context, nodes []guardianNodeTarget, group *guardianUpsertGroup, principal *guardianPrincipal) error`

**What it does:** Applies a single upsert group to the target nodes: registers the repository, ingests file batches (with directory hints), and records the ingested repo in the router's `ingestedRepos` map.

**Called from:** `applyGuardianUpsertGroups` (this file).

**Implementation details:**
- Appends directory hints via `appendGuardianDirHints` to ensure parent directories exist.
- Uses `guardianMutationTargets` to reduce nodes for KVS backends.
- Fan-out: Each target node gets a goroutine that (a) calls `RegisterRepository` (best-effort, warnings logged), then (b) calls `IngestFileBatch` with a configurable timeout.
- On success, updates `r.ingestedRepos` and bumps the native namespace generation.

---

### `appendGuardianDirHints(files []*pb.FileMetadata) []*pb.FileMetadata`

**What it does:** For each file in the batch, appends a cloned "dir hint" entry with empty content and `dir_hint: "true"` metadata, ensuring parent directories are materialized on kvs-backed storage nodes.

**Called from:** `applyGuardianUpsertGroup` (this file).

---

### `readGuardianFileContent(ctx context.Context, client pb.MonoFSClient, fullPath string) ([]byte, error)`

**What it does:** Reads the full content of a guardian file from a node via the gRPC `Read` streaming RPC.

**Called from:** `DeleteGuardianPaths` (this file).

**Returns:** The complete file content as bytes, or an error.

**Implementation details:** Opens a read stream with `Offset: 0, Size: 0` (reads entire file), aggregates all chunks.

---

### `buildGuardianChangeEvent(version *pb.GuardianFileVersion, logicalPath string, changeType pb.ChangeType, correlationID string, content []byte) *pb.GuardianChangeEvent`

**What it does:** Constructs a `GuardianChangeEvent` proto from a version record and metadata.

**Called from:**
- `processGuardianUpsert` (this file)
- `DeleteGuardianPaths` (this file)
- `processGuardianDelete` (`pipeline_kvs.go`)

**Implementation details:** Inlines content < 64KB into the event. Content is cloned (not shared).

---

### `(r *Router) publishGuardianLogicalChange(event *pb.GuardianChangeEvent)`

**What it does:** Distributes a guardian logical change event to all matching subscribers.

**Called from:**
- `processGuardianUpsert` (this file)
- `DeleteGuardianPaths` (this file)
- `processGuardianDelete` (`pipeline_kvs.go`)

**Implementation details:** Iterates all subscribers in `r.guardianLogicalChangeSubs`, filters by prefix match via `matchesGuardianLogicalPrefixes`, clones the event (or strips inline content if the subscriber doesn't want it), and non-blockingly sends on the subscriber's channel. Drops events for slow subscribers with a warning log.

---

### `(r *Router) publishLegacyGuardianChange(event *pb.GuardianChangeEvent)`

**What it does:** Converts a guardian logical change event to the legacy `ChangeEvent` format and publishes it via `r.publishGuardianChange`.

**Called from:**
- `processGuardianUpsert` (this file)
- `DeleteGuardianPaths` (this file)
- `processGuardianDelete` (`pipeline_kvs.go`)

**Implementation details:** Maps the logical path back to physical via `mapGuardianLogicalPath`, then creates a `ChangeEvent` with the relative path and optional inline content.

---

### `normalizeGuardianLogicalPrefixes(prefixes []string) ([]string, error)`

**What it does:** Normalizes a list of logical path prefixes for subscription filtering.

**Called from:** `SubscribeGuardianChanges` (this file).

**Returns:** Normalized prefixes, or `nil` if all prefixes are empty/root (meaning subscribe to everything). Returns an error if any prefix fails `normalizeGuardianLogicalPath`.

---

### `matchesGuardianLogicalPrefixes(logicalPath string, prefixes []string) bool`

**What it does:** Checks if a logical path matches any of the given prefixes for subscription filtering.

**Called from:** `publishGuardianLogicalChange` (this file).

**Returns:** `true` if the path equals a prefix, starts with a prefix + `/`, or a prefix starts with the path + `/`. Always `true` if prefixes is empty.

---

### `cloneGuardianLogicalChangeEvent(event *pb.GuardianChangeEvent) *pb.GuardianChangeEvent`

**What it does:** Deep-copies a `GuardianChangeEvent` proto (safe for concurrent subscribers).

**Called from:** `publishGuardianLogicalChange` (this file).

**Returns:** A cloned proto, or `nil` if the input is `nil`.

---

### `guardianBatchRevisionID(now time.Time) string`

**What it does:** Generates a unique batch revision ID from a timestamp.

**Called from:**
- `processGuardianUpsert` (this file)
- `DeleteGuardianPaths` (this file)
- `processGuardianDelete` (`pipeline_kvs.go`)

**Returns:** `"batch_<unix_nano>"`.

---

### `authorizeGuardianMutation(principal *guardianPrincipal, logicalPath string, deleteOp bool) error`

**What it does:** Enforces role-based authorization for guardian mutations. Determines whether a principal may write/delete at a given logical path.

**Called from:**
- `processGuardianUpsert` (this file)
- `DeleteGuardianPaths` (this file)

**Rules by role:**
- `control-plane`, `cli` → full access.
- `doctor` → may only mutate paths under `/doctor/` or `/partitions/doctor-system/`.
- `pusher` → may only mutate paths under `/.queues/<pusher-name>/`:
  - Writes: only to `.claims` or `.results` sub-paths.
  - Deletes: only to `.claims` sub-paths.
  - Cannot mutate queue root.
- `pusher` can also write to `/.state/` paths.
- Unknown roles → denied.

---

## File: `guardian_principals.go`

### Type: `guardianPrincipal`

```go
type guardianPrincipal struct {
    PrincipalID string        `json:"principal_id"`
    TokenHash   string        `json:"token_hash"`
    Role        string        `json:"role"`
    DisplayName string        `json:"display_name"`
    CreatedAt   int64         `json:"created_at"`
    Disabled    bool          `json:"disabled"`
    BaseURL     string        `json:"base_url,omitempty"`
    Grants      []authz.Grant `json:"grants,omitempty"`
}
```

Represents a registered guardian client identity with hashed token, role, display name, creation timestamp, enabled/disabled flag, optional base URL, and optional per-partition authorization grants.

---

### Type: `guardianPrincipalStore`

```go
type guardianPrincipalStore struct {
    mu          sync.RWMutex
    principals  map[string]*guardianPrincipal
    persistPath string
}
```

Thread-safe in-memory store for guardian principals with optional JSON file persistence.

---

### `newGuardianPrincipalStore(stateDir string) (*guardianPrincipalStore, error)`

**What it does:** Creates a new principal store, optionally loading persisted data from `<stateDir>/guardian_principals.json`.

**Called from:** `NewRouter` in `router.go` (during router initialization).

**Parameters:**
- `stateDir` — if empty, operates in memory only with no persistence.

**Returns:** The initialized store, or an error if directory creation or loading fails.

---

### `(s *guardianPrincipalStore) load() error`

**What it does:** Reads the JSON principals file from disk and populates the in-memory map.

**Called from:** `newGuardianPrincipalStore`.

**Implementation details:** Skips nil entries and entries with empty `PrincipalID`. Clones each principal. Non-existent file is silently skipped.

---

### `(s *guardianPrincipalStore) saveLocked() error`

**What it does:** Persists all principals to disk as a JSON array (atomic write via tmp + rename).

**Called from:** `upsertConnectedClient`, `setGrants` (must be called with `s.mu.Lock()` held).

**Returns:** An error if encoding or I/O fails.

---

### `(s *guardianPrincipalStore) upsertConnectedClient(principalID, token, role, displayName, baseURL string) (*guardianPrincipal, error)`

**What it does:** Registers or updates a guardian connected client. Creates a new principal if one doesn't exist for the given ID, or updates an existing one.

**Called from:** Router's `ConnectGuardianClient` RPC handler (in `router.go`).

**Parameters:**
- `principalID`, `token` — both required.
- `role` — if empty, inferred from `principalID` via `inferGuardianPrincipalRole`.
- `displayName` — if empty, defaults to `principalID`.
- `baseURL` — optional.

**Returns:** A clone of the upserted principal.

**Implementation details:** Hashes the token with `guardianTokenHash`. Sets `Disabled = false`. Persists immediately.

---

### `(s *guardianPrincipalStore) validateToken(token string) (*guardianPrincipal, bool)`

**What it does:** Validates a raw token against all registered principals by comparing token hashes.

**Called from:**
- `r.authenticateGuardianToken` (`router.go`)

**Returns:** The matching principal (cloned) and `true`, or `nil, false`.

**Implementation details:** Iterates all principals, skipping disabled ones. Uses constant-time SHA-256 hash comparison.

---

### `(s *guardianPrincipalStore) validateTokenForPrincipal(token, principalID string) (*guardianPrincipal, bool)`

**What it does:** Validates a token against a specific principal ID, ensuring both exist and the token hash matches.

**Called from:**
- `r.authenticateGuardianMutation` (`router.go`)

**Returns:** The principal and `true`, or `nil, false`.

---

### `(s *guardianPrincipalStore) setGrants(principalID string, grants []authz.Grant) error`

**What it does:** Replaces the per-partition authorization grants for a principal. All grants are validated before storage.

**Called from:** (External/API — not found in reviewed files, likely called via an RPC handler).

**Returns:** An error if any grant validation fails or the principal doesn't exist.

---

### `(s *guardianPrincipalStore) grantsFor(principalID string) []authz.Grant`

**What it does:** Returns a copy of a principal's grants.

**Called from:** (External/API — likely for authorization checks).

**Returns:** A slice of grants (copy), or `nil` if unknown.

---

### `(s *guardianPrincipalStore) grantEvaluator() authz.GrantEvaluator`

**What it does:** Builds an `authz.GrantEvaluator` from all enabled principals' grants, allowing principal-scoped authorization through a shared evaluation path.

**Called from:** (External/API — likely for authorization decisions).

**Returns:** A `GrantEvaluator` backed by a temporary `GrantStore` containing all non-disabled principals' grants.

---

### `guardianTokenHash(token string) string`

**What it does:** Hashes a raw token using SHA-256, returning a hex-encoded string.

**Called from:**
- `guardianPrincipalStore.upsertConnectedClient`
- `guardianPrincipalStore.validateToken`
- `guardianPrincipalStore.validateTokenForPrincipal`

**Returns:** Lowercase hex SHA-256 digest.

---

### `inferGuardianPrincipalRole(clientID string) string`

**What it does:** Infers the principal role from the client ID string.

**Called from:**
- `guardianPrincipalStore.upsertConnectedClient`
- `r.authenticateGuardianToken` (`router.go`)
- `r.authenticateGuardianMutation` (`router.go`)

**Returns:**
- Contains `"pusher"` → `"pusher"`
- Contains `"cli"` → `"cli"`
- Otherwise → `"control-plane"`

---

## File: `guardian_version_store.go`

### Type: `storedGuardianFileVersion`

```go
type storedGuardianFileVersion struct {
    LogicalPath     string `json:"logical_path"`
    DisplayPath     string `json:"display_path"`
    StorageID       string `json:"storage_id"`
    VersionID       string `json:"version_id"`
    BatchRevisionID string `json:"batch_revision_id"`
    ContentSHA256   string `json:"content_sha256"`
    CommittedAt     int64  `json:"committed_at"`
    Tombstone       bool   `json:"tombstone"`
    PrincipalID     string `json:"principal_id"`
    Reason          string `json:"reason"`
    CorrelationID   string `json:"correlation_id,omitempty"`
    Content         []byte `json:"-"`
}
```

In-memory representation of a single guardian file version. `Content` is kept in memory only (not persisted via JSON) to avoid unbounded snapshot growth.

---

### Type: `guardianVersionSnapshot`

```go
type guardianVersionSnapshot struct {
    Records map[string][]*storedGuardianFileVersion `json:"records"`
    Current map[string]string                       `json:"current"`
}
```

Serialization format for the guardian version store: maps logical paths to their version history, and a separate map of logical paths to current version IDs.

---

### Type: `guardianVersionCommit`

```go
type guardianVersionCommit struct {
    LogicalPath     string
    DisplayPath     string
    StorageID       string
    BatchRevisionID string
    PrincipalID     string
    Reason          string
    CorrelationID   string
    Content         []byte
    Tombstone       bool
    CommittedAt     int64
}
```

Parameter struct passed to `guardianVersionStore.commit`, capturing all metadata for a new version record.

---

### Type: `guardianVersionStore`

```go
type guardianVersionStore struct {
    mu          sync.RWMutex
    records     map[string][]*storedGuardianFileVersion
    current     map[string]string
    ephemeral   map[string]*storedGuardianFileVersion
    persistPath string
    dirty       bool
    stopFlush   context.CancelFunc
    flushDone   chan struct{}
}
```

Thread-safe version store with:
- `records` — full history for asset paths (persisted).
- `current` — maps logical paths to current version IDs.
- `ephemeral` — current version only for machinery paths (never persisted).
- Background flush loop that writes to disk every 5 seconds when dirty.

---

### `newGuardianVersionStore(stateDir string) (*guardianVersionStore, error)`

**What it does:** Creates a new version store, loads persisted data from `<stateDir>/guardian_versions.json` if available, and starts a background flush goroutine.

**Called from:** `NewRouter` in `router.go`.

**Parameters:**
- `stateDir` — if empty, operates in memory only with no persistence or flush loop.

**Returns:** The initialized store, or an error.

---

### `(s *guardianVersionStore) flushLoop(ctx context.Context)`

**What it does:** Background goroutine that periodically writes the dirty snapshot to disk every 5 seconds. On context cancellation, performs a final flush and exits.

**Called from:** `newGuardianVersionStore` (launched as a goroutine).

**Implementation details:** Coalesces per-file saves from a batch into a single write, avoiding per-commit O(n) I/O amplification.

---

### `(s *guardianVersionStore) close()`

**What it does:** Shuts down the background flush goroutine and waits for it to complete.

**Called from:** Router shutdown in `router.go` (line 1837).

---

### `(s *guardianVersionStore) load() error`

**What it does:** Reads the JSON version snapshot from disk, populates `records` and `current` maps, and prunes any machinery paths that were persisted before the asset/ephemeral split.

**Called from:** `newGuardianVersionStore`.

**Implementation details:** Non-existent file is silently skipped. Calls `pruneMachineryLocked` after loading.

---

### `(s *guardianVersionStore) pruneMachineryLocked()`

**What it does:** Removes machinery path entries (non-asset paths) from `records` and `current` maps, shrinking the state file on the next flush.

**Called from:** `load()` (must be called with `s.mu` held).

**Implementation details:** Uses `isGuardianAssetPath` to determine which paths to keep.

---

### `isGuardianAssetPath(logicalPath string) bool`

**What it does:** Determines whether a logical path is a user-managed asset (intent YAML, partition config) vs. internal machinery (task queue files, claim files, result files, state snapshots, archive logs).

**Called from:**
- `pruneMachineryLocked` (this file)
- `commit` (this file)

**Returns:** `false` for paths under `/.queues/`, `/.archive/`, or containing any segment starting with `.` (e.g., `/partitions/doctor/.state/...`). `true` otherwise.

---

### `(s *guardianVersionStore) saveLocked() error`

**What it does:** Writes the current `records` and `current` maps to disk as a JSON snapshot (atomic write via tmp + rename).

**Called from:** `flushLoop` (must be called with `s.mu.Lock()` held).

**Implementation details:** Updates metrics `routerGuardianVersionStoreWriteBytesTotal` and `routerGuardianVersionStoreFileBytes`.

---

### `(s *guardianVersionStore) currentVersion(logicalPath string) (*storedGuardianFileVersion, bool)`

**What it does:** Returns the current version record for a logical path, checking the ephemeral (machinery) tier first, then the persisted (asset) tier.

**Called from:**
- `processGuardianUpsert` (`guardian_paths.go`)
- `DeleteGuardianPaths` (`guardian_paths.go`)
- `processGuardianDelete` (`pipeline_kvs.go`)
- `listKVSConfigs`, `checkKVSConfig` (`pipeline_kvs.go`)
- Pipeline router (`pipeline_router.go`)

**Returns:** A cloned copy of the version record (with cloned content), and `true` if found.

---

### `(s *guardianVersionStore) commit(change guardianVersionCommit) (*pb.GuardianFileVersion, error)`

**What it does:** Records a new version for a logical path. Asset paths are appended to the history (capped at 50 versions) and marked dirty. Machinery paths are stored ephemerally only (no history, no persistence).

**Called from:**
- `processGuardianUpsert` (`guardian_paths.go`)
- `DeleteGuardianPaths` (`guardian_paths.go`)
- `processGuardianDelete` (`pipeline_kvs.go`)

**Parameters:**
- `change` — all metadata for the new version.

**Returns:** The proto representation of the committed version, or an error.

**Implementation details:**
- Computes content SHA-256.
- Generates a version ID via `guardianVersionID`.
- Content is cloned before storage.
- Asset history is trimmed to `maxVersionsPerPath = 50` (oldest entries dropped).

---

### `(s *guardianVersionStore) list(logicalPath string, pageSize int32, pageToken string) ([]*pb.GuardianFileVersion, string, error)`

**What it does:** Returns paginated version history for a logical path, newest first.

**Called from:**
- `ListGuardianVersions` (`guardian_paths.go`)
- `listKVSConfigs` (`pipeline_kvs.go`)
- Pipeline router (`pipeline_router.go`)

**Parameters:**
- `pageSize` — defaults to 50 if ≤ 0.
- `pageToken` — integer offset string; empty for first page.

**Returns:** A page of `GuardianFileVersion` protos and an opaque next page token (offset for next page), or an empty string if there are no more results.

---

### `(s *guardianVersionStore) get(logicalPath, versionID string) (*pb.GuardianFileVersion, []byte, bool)`

**What it does:** Retrieves a specific version by ID, checking ephemeral tier first, then asset records.

**Called from:** `GetGuardianVersion` (`guardian_paths.go`).

**Returns:** The version proto, the raw content bytes, and `true`; or `nil, nil, false` if not found.

---

### `guardianVersionID(logicalPath, contentSHA string, committedAt int64, tombstone bool) string`

**What it does:** Generates a deterministic version ID from path, content hash, timestamp, and tombstone flag.

**Called from:** `guardianVersionStore.commit` (this file).

**Returns:** `"ver_<committedAt>_<12-char hex>"` where the hex is a SHA-256 of the concatenated fields.

---

### `(v *storedGuardianFileVersion) toProto() *pb.GuardianFileVersion`

**What it does:** Converts an internal `storedGuardianFileVersion` to its protobuf representation.

**Called from:**
- `commit` (this file)
- `list` (this file)
- `get` (this file)

**Returns:** A `GuardianFileVersion` proto, or `nil` if the receiver is `nil`.

**Implementation details:** Note that `Content` is intentionally excluded from the proto — callers that need content retrieve it separately from the store.
