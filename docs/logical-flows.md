# Logical Flows

**Scope:** end-to-end behavior derived from code. Each flow names its trigger, participants, call chain, state changes, and failure behavior. Line references are navigation aids.

## 1. Flow map

| # | Flow | Primary entry points | Main components |
|---|---|---|---|
| 1 | Startup and shutdown | each `cmd/*/main.go` | all |
| 2 | Git ingestion | `Router.IngestRepository`, `POST /api/ingest` | router, Git backend, fetcher, nodes |
| 3 | File ingestion | `POST /api/ingest/file` | router, file backend, fetcher, nodes |
| 4 | Guardian partition injection | `POST /api/guardian/inject`, `InjectGuardianPartition` | router, Git backend, KVS/nodes |
| 5 | Blob archive write/read | `StoreArchive`, `FetchBlob` | router/nodes, fetcher, S3/GCS |
| 6 | FUSE read | kernel -> FUSE ops | client, sharded client, router, nodes, fetcher |
| 7 | Native read | kernel module -> native gateway | kmod, router, nodes |
| 8 | Overlay write | VFS write -> FUSE ops | client, NutsDB, session |
| 9 | Session commit | `monofs-session add/commit` | session CLI, client socket, overlay DB |
| 10 | Workspace publish | `UploadWorkspaceBundle`, `PublishWorkspace` | client, router, fetcher, Git remote |
| 11 | Source push | `UploadWorkspaceCommitBundle`, `PushWorkspaceCommits` | client, router, fetcher, Git remote, ledger |
| 12 | Workspace refresh | `RefreshWorkspace`, auto-refresh | router, fetcher, Git remote, nodes |
| 13 | Auto-push | `autoPushWorker` | router, fetcher, Git remote |
| 14 | Browser login | `/auth/login`, `/auth/callback` | browser, router, IdP |
| 15 | Service auth | gRPC metadata | service, router/nodes/search, authz package |
| 16 | Guardian path write/delete/read | `UpsertGuardianPaths*`, `DeleteGuardianPaths`, versions | router, KVS/nodes |
| 17 | Pipeline run | webhook/manual -> orchestrator -> worker | router, pipeline package, worker, Guardian queue |
| 18 | Search index/query | `IndexRepository`, `Search` | router, search, Zoekt |
| 19 | Registry pull/push | `/v2/...` | registry, MonoFS nodes, upstream |
| 20 | Doctor ingest/query | `Ingest*`, `Query*` | services, router, nodes, log engine |
| 21 | Node failure/failover | health check -> failover | router, nodes, client |
| 22 | Deploy/build | `Makefile`, Compose, Dockerfiles | CI, Docker, cluster |

## 2. Startup and shutdown

### 2.1 Storage node

`cmd/monofs-server/main.go`:

1. Parse flags/env; require `--node-id`.
2. Open Pebble metadata at `--db-path`.
3. Open the optional embedded KVS store and raft listener.
4. Register a fetcher client if `--fetcher` is configured. **The fetcher client is created inside `ConfigureFetcher`, which is only called when prediction is enabled**; `--fetcher` without `--enable-prediction` leaves every blob-backed read failing with `FailedPrecondition`.
5. Configure access control (only if a policy source exists), the Doctor log engine, telemetry, and diagnostics.
6. Start gRPC and the optional forwarding topology refresh loop.
7. Wait for SIGINT/SIGTERM (SIGHUP is also treated as shutdown). Close the DB before `GracefulStop`, so in-flight handlers can observe a closed store.

### 2.2 Router

`cmd/monofs-router/main.go`:

1. Register Git/file ingestion backends.
2. Set up telemetry/logging.
3. Parse flags; construct `router.NewRouter`.
4. Open Guardian principal/version stores; optionally open workspace store; load workspace policy; build grants; start auto-push/auto-refresh; initialize pipeline.
5. Optionally install the ownership gate.
6. Configure search/fetcher/registry/peer credentials; register static nodes; start health checks.
7. Start access-policy polling.
8. Build OIDC and break-glass verifiers; seed the Guardian control-plane principal from `MONOFS_TOKEN`.
9. Serve gRPC, HTTP, and optional native TCP.
10. On signal: close HTTP abruptly, close native listener, `GracefulStop` gRPC without a timeout, close the router.

### 2.3 Client

`cmd/monofs-client/main.go`:

1. Resolve visible owner from the nearest existing ancestor of `--overlay`.
2. Build a sharded client; if the router is unavailable, use `NewDisconnectedClient` and retry.
3. Optionally create the metadata cache (NutsDB, 30 s TTL); delete the cache directory unless `--keep-cache`.
4. If writable, create `SessionManager`, `CommitManager`, and the Unix socket handler.
5. Build the root node, enable virtual monorepo/repo filter/Git projection as requested.
6. `fs.Mount` with `AllowOther`, 30 s attr/entry timeouts, and negative timeout 0 when writable.
7. Sync the synthetic Git workspace after mount.
8. On unmount, close socket/cache/client. No session commit or discard is performed.

## 3. Ingestion

### 3.1 Git ingestion over gRPC

**Trigger:** `MonoFSRouter.IngestRepository` (admin CLI, session, auto-refresh, workspace refresh, Guardian).

**Call chain:**

```text
IngestRepository (internal/router/ingest.go)
  -> metric + whitelist check (if enabled)
  -> validate source/ref/guardian fields
  -> displayPath = source_id | normalizeRepoID(source)
  -> storageID = SHA-256(displayPath)
  -> authorizeIngest (grant evaluator; fails closed when enforced)
  -> storage.DefaultRegistry.CreateIngestionBackend(type)
  -> backend.Validate -> backend.Initialize (goroutine + 10 s keepalives)
  -> RegisterRepository on every healthy node
  -> backend.WalkFiles
       -> shardKey = storageID + ":" + filePath
       -> HRW primary + replicas
       -> primaryBatches / replicaBatches
  -> dedupe by content hash; build 2 GiB packager archives
       -> fetcher.StoreArchive(storageID, chunkIndex, encrypted archive)
  -> 10 workers x 1,000-file batches -> node.IngestFileBatch
  -> best-effort replica goroutines -> node.IngestReplicaBatch
  -> MarkRepositoryOnboarded on active nodes
  -> update r.ingestedRepos, bump native generation
  -> rebalance on re-ingestion; triggerSearchReindex
  -> stream COMPLETED
```

**Validation:**

- `normalizeRepoID` converts URL forms to `host/path`, strips `.git`, and leaves SSH/module paths as-is.
- `goModuleVersion` splits a trailing `@version` for non-URL sources and uses the version as the Git ref.
- `validateGitRef` rejects whitespace, `..`, revision operators, leading/trailing `/`, `.lock`, and trailing `.`.
- Guardian ingestion forces `guardian/<source_id>` and rejects reserved `guardian*`/`doctor*` paths for ordinary ingestion.

**Key limits:**

- 1,000 files per batch; 10 concurrent primary workers; 60 s per batch.
- 2 GiB archive split; no total repository size, file count, or memory cap.
- Replica batches are best-effort and unbounded in concurrency.

**Failure behavior:**

- HTTP ingestion returns 200 before validation/clone; failures are only logged.
- Partial primary failures are counted but still end in `COMPLETED`; only every-batch failure fails the RPC.
- Replica failures never fail the request.
- Archives written before a later failure are not cleaned up.
- Concurrent ingests of the same display path share a single `inProgressIngestions[storageID]` slot and can clobber each other's progress.
- File/HTTP ingestion is broken by an enum-string mismatch: `INGESTION_FILE` becomes `"ingestion_file"` instead of `"file"`, so backend lookup fails asynchronously.

### 3.2 HTTP ingestion

`POST /api/ingest` (`handleIngest`):

1. POST only.
2. If the whitelist is enabled, check `client_id` form or `X-Client-ID` (caller-controlled).
3. Parse `source`, `ref`, `source_id`, `ingestion_type`, `fetch_type`, `replicate_data`; reject Guardian type.
4. Start a goroutine with a detached context that preserves the caller identity but not gRPC metadata.
5. Return `200 {"status":"in_progress"}` immediately.

`POST /api/ingest/file` (`handleIngestFileUpload`):

1. Parse multipart with a 500 MiB memory threshold (not a total request cap).
2. Create `/tmp/monofs-upload-*`.
3. Write each `header.Filename` with `filepath.Join(tempDir, name)`; no explicit `filepath.Base`.
4. Ingest the temp directory as `INGESTION_FILE` (which fails as above).
5. Delete the temp directory when the goroutine finishes.

### 3.3 Guardian partition injection

`POST /api/guardian/inject` or `InjectGuardianPartition`:

1. Require `X-Guardian-Token` or `guardian_token`; validate against a Guardian principal.
2. Validate `source` and `partition_name`; HTTP rejects `/` in the name, the direct RPC does not.
3. Clone/walk the source with the Git ingestion backend.
4. Translate files to `InjectGuardianFile` with clean relative paths.
5. Write to the selected node(s): KVS leader for `guardian-system` paths, all healthy nodes or HRW replicas otherwise.
6. HTTP returns 200 before clone/injection finishes; failures are logged only.

## 4. Content archive and fetch

### 4.1 Archive write

```text
router ingestion
  -> packager.NewArchiveWriter(pipeline.NewPipeline(32-byte key))
  -> AddFile(entry=contentHash, encrypted)
  -> close, stream chunks to fetcher.StoreArchive(storageID, chunkIndex, bytes)
       -> BlobBackend.StoreArchive
            -> write archives/<storageID>/chunk-%04d.pack (temp + rename)
            -> indexArchive (open, ListFiles, filter hex entry names)
            -> queue async cloud upload
```

`StoreBlob` writes a loose archive `_loose/<hash>.pack`; batch ingestion packs into `_batch/batch-<time>-*.pack` split at 512 MiB.

### 4.2 Archive read

```text
node read miss
  -> fetcher.Client.CheckCacheSimple (heuristic)
  -> fetcher.Client.FetchBlob (up to 3 fetchers)
       -> BlobBackend.FetchBlob
            -> blobIndex[ContentID]
            -> on miss: findBlobOnDisk scans every archives/*/*.pack
            -> getArchiveReader (local open or cloud download, max 256 readers)
            -> ArchiveReader.GetFile(entryPath)
```

There is no content-hash verification at fetch time. Cloud-only archives are not discoverable by content ID if the local index is lost.

## 5. Read paths

### 5.1 FUSE read

**Trigger:** kernel lookup/open/read.

```text
MonoNode.Lookup
  -> synthetic workspace entries
  -> overlay DB state
  -> metadata cache (30 s)
  -> ShardedClient.Lookup (2 retries)
  -> NewInode(FNV-1a path hash)

MonoNode.Open
  -> synthetic content
  -> write intent?  (O_WRONLY/O_RDWR/O_APPEND/O_TRUNC/O_CREAT)
  -> overlay file? open local fd
  -> ShardedClient.Read(path, 0, 0)  (whole file, 3 retries)
  -> cache n.content; FOPEN_KEEP_CACHE (or DIRECT_IO if size changed)

MonoNode.Read
  -> slice n.content[off:off+len]
```

**Failure behavior:**

- Any backend read error in `ensureLocalCopy` is treated as “new file” and creates an empty overlay file.
- Whole-file caching has no TTL or size cap.
- `ReadDir` retries on a fresh 30 s background context even after a FUSE interrupt.
- Error mapping collapses most failures to `EIO`.

### 5.2 Sharded client read

```text
ShardedClient.Read(path, offset, size)
  -> key = monopath.BuildShardKey(path)
  -> hrw.GetNodes(key, 3)
  -> for each node: stream chunks into memory
  -> on stream error: next ranked node
  -> if all fail: router.GetNodeForFile -> primary + fallback IDs
```

- `Lookup`/`GetAttr`: rank 3; a clean not-found scans all other healthy nodes; `Lookup` falls back to the router, `GetAttr` does not.
- `ReadDir`: queries all healthy nodes concurrently, merges by name, retries failed nodes once, and errors rather than returning a partial listing.
- `ReadDirStream`: k-way merge of sorted per-node streams.
- `StatFS`: derived from router cluster info, filtering non-`Active` nodes.

### 5.3 Server read resolution

`Lookup`/`GetAttr`/`Read`/`ReadDir` follow a common sequence:

1. Build path from parent/name (no cleaning).
2. Authorize read if access control is configured.
3. Synthetic root/managed namespace/repo/intermediate directory.
4. Resolve `displayPath -> storageID` by longest prefix.
5. KVS/CFG or local metadata.
6. On local miss: failover cache lookup.
7. If forwarding is enabled and the target is another healthy node, forward with a loop marker.
8. Return `NotFound`.

`Read` then:

- KVS/CFG: read full content, slice `[offset:offset+size]`, stream 64 KiB chunks.
- Blob: read metadata, require a fetcher client, `CheckCacheSimple` + `FetchBlobSimple`, buffer the whole blob, clamp/truncate, stream 64 KiB chunks.

**Failure behavior:**

- KVS/CFG `Read` with a negative offset panics.
- Forwarding drops bearer and `x-client-id` metadata.
- Failover backup selection is effectively unreachable because HRW only returns healthy nodes.
- A local metadata hit with a missing fetcher blob returns an error; peers are not tried.

### 5.4 Native gateway read

```text
kmod -> router native TCP (52-byte frame)
  -> Hello/Mount (AuthToken checked against the gateway token)
  -> Lookup(parent, name)
       -> Router.NativeLookup -> nodes (HRW 3, then all healthy)
       -> Router.NativeGetAttr
       -> session.bindPath
  -> OpenRead(objectID) -> session.pathForObject -> NativeGetAttr -> bind handle
  -> Read(handle, offset, length)
       -> Router.NativeRead -> HRW 3 -> router.GetNodeForFile fallback
  -> Close(handle)
```

`ReadDir` re-fetches the full merged listing on every page and treats `Cookie` as a slice index. `MaxReadBytes` (256 KiB) is advertised but not enforced; a large read can exceed the 1 MiB frame limit and close the connection.

## 6. Write and workspace flows

### 6.1 Overlay write

```text
Create(path)
  -> StartSession
  -> create local file (O_CREATE|O_RDWR|O_TRUNC)
  -> TrackChange(ChangeCreate)
  -> return handle with FOPEN_DIRECT_IO

Open(existing, write flags)
  -> StartSession
  -> ensureLocalCopy: full backend read -> local file 0644
  -> open local fd, mark isLocalWrite
  -> FOPEN_DIRECT_IO

Write(data, off)
  -> WriteAt
  -> update node.size, set isLocalWrite
  -> no DB write

Flush (every close)
  -> TrackChangeWithMeta(ChangeModify/Create, size)
  -> NutsDB PutFile
```

**Crash window:** a process kill between `write(2)` and `close(2)` leaves the file on disk but no DB entry. `RebuildFromDisk` only runs when the DB is completely empty, so repository-path files remain invisible.

**Delete/rename/symlink:**

- `Unlink` removes local content and records a tombstone, or removes the DB entry outright for session-created paths.
- `Rmdir` at root removes a user root dir; nested dirs are recursively removed locally and recorded.
- `Rename` copies up the source, renames on disk, re-keys overlay children, rewrites node paths, and records delete+create.
- `Symlink` creates a real symlink in the session and records a symlink entry.

### 6.2 Session add/commit

```text
monofs-session add <path>
  -> client socket handleAdd
  -> GetChanges
  -> classify + select workspace changes
  -> snapshot full local content into staged bucket
  -> reject conflict markers

monofs-session commit -m ...
  -> handleCommit
  -> HasSessionConflicts must be false
  -> CommitChanges
       -> ListStagedEntries
       -> build LocalVirtualCommit (grouped by repository, parent = latest local commit)
       -> PutLocalVirtualCommit
       -> advanceOverlayBaseline: committed entries become ChangeBaseline
       -> ClearStagedEntries
```

Local commits are `local-<12hex>` entries in NutsDB. Branch mappings map `(principal, logical branch, storageID)` to the actual remote branch.

### 6.3 Publish

```text
client -> UploadWorkspaceBundle (client stream)
  -> buffer all chunks; one workspace_id
  -> workspacebundle.Parse + Validate
  -> bundleID = wbundle-<UnixNano>; store in memory + workspacestore
  -> PublishWorkspace(bundleID, workspaceID, message, author, strategy)
       -> create PUBLISH job
       -> emit JOB_ACCEPTED
       -> runWorkspacePublishJob
            -> StageWorkspaceBundle(fetcher)
            -> StartWorkspacePublish
                 -> per repository:
                      filterSensitiveOperations
                      clone worktree, verify base commit if provided
                      checkout/create target branch
                      apply operations
                      stage, commit (unsigned), push
            -> per result: update job; on PUBLISHED trigger search reindex + PR
            -> finalize; emit JOB_COMPLETED
       -> defer DiscardWorkspaceBundle
```

- No policy/ownership/authz check is applied on publish.
- Publish is not all-or-nothing; later repositories continue after a failure.
- The sensitive-path filter is deny-list based and only checks operation path/target, not symlink targets.

### 6.4 Source push

```text
client -> UploadWorkspaceCommitBundle
  -> validate SourceCommitBundle (branch + base commit required)
  -> PushWorkspaceCommits
       -> workspace_id / logical branch checks
       -> resolve push mode (request > config > squash)
       -> evaluate workspace policy
       -> ownership gate (direct pushes only)
       -> runWorkspaceCommitPushJob
            -> StageWorkspaceCommitBundle
            -> StartWorkspaceCommitPush
                 -> squash: group by repo, apply all ops, one commit, push
                 -> preserve: replay each local commit, push per commit
            -> on hard failure: best-effort revert commits for already-published repos
            -> append PushOutcome to ledger
            -> finalize; emit JOB_COMPLETED
```

- Policy/ownership denial is surfaced as a failed job event, not an RPC error.
- Ledger append failure after a successful Git push marks the job failed.
- The ownership gate is only applied here, not on publish or auto-push.
- Sensitive-path filtering is not applied to source push.

### 6.5 Refresh and auto-refresh

```text
RefreshWorkspace
  -> create REFRESH job
  -> fetcher.ProbeWorkspaceRefresh (metadata-only or full)
  -> per repository:
       UNCHANGED -> unchanged
       ADVANCED  -> REFRESH_REQUIRED -> emit REINGEST_STARTED
                    -> IngestRepository inline -> REFRESHED/FAILED
       DIVERGED / MISSING_BRANCH -> CONFLICT
       auth/transient/failed -> FAILED
  -> finalize; JOB_COMPLETED
```

`autoRefreshWorker` maintains a min-heap of due repositories, probes in batches, applies adaptive intervals (base, 5 min after 1 h, 15 min after 24 h, 1 h after 7 days, ±20% jitter), persists `repowatch-<shard>.json`, and uses a Kubernetes Lease to elect one worker per shard when enabled.

### 6.6 Auto-push

`autoPushWorker` scans `workspaceSyncJobs` for `SOURCE_PUSH` jobs in `QUEUED` state and retries them with exponential backoff. It evaluates policy with principal `"autopush"`, stages the bundle, and calls `StartWorkspaceCommitPush`.

It does **not** update the job state, write a ledger outcome, open a PR, trigger search reindex, discard the bundle, or apply the ownership gate. A successful push leaves the job `QUEUED`, so the next scan can push it again.

## 7. Authentication flows

### 7.1 Browser OIDC (Authorization Code + PKCE)

```text
GET /auth/login
  -> GeneratePKCE (S256), random state
  -> store pending[state] = verifier
  -> 302 to IdP with client_id, redirect_uri, state, code_challenge
  -> Set-Cookie monofs_oauth_state (HttpOnly, SameSite=Lax, 10 min)

GET /auth/callback?code&state
  -> state cookie + pending entry check
  -> POST token endpoint with code + code_verifier
  -> Verify id_token: signature (asymmetric allowlist), iss, aud, exp/nbf
  -> Set-Cookie monofs_session = raw id_token (HttpOnly, SameSite=Lax, 12 h)
  -> 302 to post-login redirect
```

There is no nonce, no token refresh, no revocation, and the opaque session store is never used. `azp` is treated as a machine `ClientID`.

### 7.2 Service-to-service

```text
service -> IdP client_credentials -> access token
service -> gRPC authorization: Bearer <token>
server -> Authenticator interceptor -> ChainVerifier -> Identity
server -> per-handler authorization (if coded)
```

Router gRPC uses `RequireToken=false`, so missing/invalid tokens become anonymous. Storage nodes install the interceptor only when a policy source exists. The search service is also `requireToken=false`.

### 7.3 Internal cluster token

`x-monofs-internal: <token>` (or a bearer equal to the token) is accepted by storage nodes and search as a policy bypass and becomes `Identity{ClientID:"monofs-internal"}`. `MONOFS_TOKEN` is the default value.

### 7.4 Guardian token

Guardian tokens travel in message fields/headers, are hashed with SHA-256 in the principal store, and map to a principal/role. `RegisterClient` can persist a principal without authentication; role inference defaults to `control-plane`.

## 8. Guardian flows

### 8.1 Upsert

```text
UpsertGuardianPaths / UpsertGuardianPathsStream
  -> authenticateGuardianMutation
  -> map logical path -> physical display path/relative path/storage ID
  -> authorizeGuardianMutation (role + namespace)
  -> check expected_version_id ("" unconditional, "absent" create-only, else CAS)
  -> group files by display path, register repository on targets
  -> node.IngestFileBatch (KVS leader or all/HRW nodes)
  -> commit version records only after node writes
  -> publish logical/legacy change events; metrics
```

### 8.2 Delete

```text
DeleteGuardianPaths
  -> authenticate + authorize
  -> map/check expected version
  -> reject root guardian-system
  -> partition root -> delete repository
  -> file/dir -> GetAttr, optionally read content for asset tombstones
  -> delete on nodes (KVS leader or all)
  -> commit tombstones for every plan (even if no physical delete)
  -> publish events; metrics
```

### 8.3 Read/version/subscribe

`ListGuardianVersions`, `GetGuardianVersion`, and `SubscribeGuardianChanges` validate the token but do not authorize by role/scope. A subscriber with an empty prefix receives all events. Change delivery is at-most-once; slow subscribers are dropped.

### 8.4 Version storage

Asset paths (no dot-prefixed segment) keep up to 50 versions in a Pebble-backed store; content is held only in a bounded in-memory cache and is not persisted. Machinery paths (any dot-prefixed segment: `/.pipelines`, `/.queues`, `/.scans`, `/.registry`, `.guardian/OWNERS`) are ephemeral and disappear on restart. Current-version and history listings use prefix scans over the store instead of loading a single snapshot file.

## 9. Pipeline flow

### 9.1 Config and trigger

```text
configs in /.pipelines/*.yml (or local)
  -> ParseConfig (YAML, unknown fields ignored)
  -> Validate (name, jobs, steps, runner type, DAG cycle)
  -> stored in configs map and Guardian KVS

GitHub/GitLab webhook
  -> signature check only if secret is configured
  -> parse event (push, pull_request, tag)
  -> reject empty CommitSHA (kills tag events)
  -> load monofs-packages.yaml (guardian path mapping fails -> local file)
  -> DetectAffectedPackages
  -> MatchEvent -> StartRun
```

### 9.2 Run

```text
StartRun
  -> cancel running runs if cancel-in-progress (ignores groups)
  -> create PipelineRun + JobStatus per job
  -> executeRun goroutine
       -> entrypoint jobs -> enqueueJobTasks
       -> task JSON written to /.queues/pipeline/<run>/tasks/<task>.json
       -> on result: advancePipeline
            -> enqueue jobs whose dependencies reached any terminal state
            -> if all terminal: RunSucceeded/RunFailed
```

### 9.3 Worker

```text
Worker.Run
  -> SubscribeGuardianChanges("/.queues/pipeline", inline content)
  -> for ADDED event containing /tasks/:
       -> parse TaskData
       -> acquire concurrency semaphore
       -> claim: WritePath(claim, expected_version_id="absent")
       -> handler.Execute (builder/docker/deployer/bazel)
       -> write ResultData
```

- Any claim write error is treated as “already claimed”; a transient failure loses the task.
- A task with `TimeoutSec=0` gets an immediately expired context.
- The bazel handler returns `(exitCode, nil)` on an exit error, so failures are recorded as success.
- No logs are persisted; step output is discarded.

### 9.4 Persistence

Configs/tasks/claims/results are Guardian paths. Runs, job states, history, and stats are in-memory. After a router restart, old tasks may still run but their results are ignored because the run is unknown.

## 10. Search flow

### 10.1 Index

```text
Router.triggerSearchReindex (async, 30 min ctx)
  -> MonoFSSearch.IndexRepository
       -> queue job; persist QUEUED
       -> worker: status INDEXING
       -> Indexer.IndexRepository
            -> if router client: ReadDir + Read through MonoFS
            -> else: git clone --depth=1 --single-branch
            -> filter .git, binary, >=1 MiB
            -> 8 read workers -> Zoekt builder
            -> finish; compute index size
       -> status READY/ERROR; persist
```

### 10.2 Query

```text
MonoFSSearch.Search
  -> build Zoekt query (substring or full syntax; case sensitivity ignored in syntax mode)
  -> optional StorageID -> repository filter
  -> searcher.Search
  -> convert matches (line, content, byte offsets)
  -> applyReadPolicy (after search)
  -> return results
```

`applyReadPolicy` keeps all results when no policy is configured, when enforcement is off, or when the caller is anonymous in observe mode. `DeleteIndex` with an empty/unknown storage ID removes every `.zoekt` shard because `strings.Contains(base, "")` is true.

## 11. Registry flow

### 11.1 Pull

```text
GET /v2/<repo>/manifests/<tag-or-digest>
  -> local tag/digest read
  -> on miss: proxy upstream (401 -> token -> retry)
  -> store manifest as blob + tag/catalog
  -> serve

GET /v2/<repo>/blobs/<digest>
  -> local base path; if missing, chunked read
  -> on miss: proxy upstream, store 512 MiB chunks
  -> serve
```

### 11.2 Push

```text
POST /v2/<repo>/blobs/uploads -> temp file + UUID
PATCH .../uploads/<uuid> -> append (global upload mutex)
PUT .../uploads/<uuid>?digest= -> hash temp file, compare, store chunks
PUT /v2/<repo>/manifests/<tag|digest>
  -> read body <=16 MiB, store as blob at its SHA-256
  -> verify digest if ref is a digest
  -> write tag + rewrite _catalog
```

There is no auth, no TLS, no cross-repo mount, no range support, and no reference counting. Blobs are global by digest.

## 12. Doctor telemetry flow

### 12.1 Ingest

```text
service -> IngestLogs/IngestMetrics/IngestTraces
  -> router selects a stable shard per signal/chunk and fans out
  -> node log engine flush chunk:
       -> compute min/max time, partition, manifest metadata
       -> write segment.parquet + index files + metadata.json (last)
```

The chunk manifest is the commit marker; a reader ignores a directory until
metadata.json is present.

### 12.2 Query

```text
StreamQueryLogs/Metrics/Traces
  -> parse query/time range
  -> list candidate chunks (time buckets)
  -> prune by manifests/labels/Bloom
  -> scan/merge records
  -> stream records or batch into items_json
```

Unknown pipe stages are silently ignored by the query parser.

## 13. Failure and failover flows

### 13.1 Fetcher failover

`fetcher.Client.FetchBlob` tries up to 3 fetchers across the healthy subset, excluding already-tried addresses. Only `FetchBlob` retries; stream, store, archive, and workspace methods either fail or use local fallbacks.

### 13.2 Node failover

```text
health check detects failure
  -> router updates node status/version
  -> selects backup nodes for repos owned by failed node (HRW/replica metadata)
  -> node.SyncMetadataFromNode(failedNodeFiles)
       -> backup copies replica metadata into failover bucket
  -> read on backup checks local metadata, then failover cache
```

Recovery calls `ClearFailoverCache`. The cache TTL is ignored and delete paths do not clear it, so deleted files can be resurrected.

### 13.3 Client read failover

HRW top 3 -> next node on RPC/stream error -> router `GetNodeForFile` primary + fallbacks. Health comes only from the router; the client never marks a node unhealthy itself. `ReadDir` queries all nodes and errors on any unrecovered node failure.

### 13.4 Workspace rollback

Source push (squash and preserve) attempts to revert repositories that were already published when a later repository fails. Rollback restores the base tree in a worktree and commits a normal revert; it never force-pushes. Publish does not roll back.

## 14. Build and deploy flow

```text
make build
  -> Go build of selected binaries into bin/
  -> router UI build (npm ci + vite) is required before Go embed

docker build (main Dockerfile)
  -> router-ui-builder -> builder -> per-binary builders -> runtime stages
  -> server/router/registry/fuse-plugin run as root
  -> search/fetcher run as monofs

docker compose up
  -> fetchers, search, HAProxy, routers, nodes, clients
  -> optional MinIO bucket + public-read policy
  -> clients mount through HAProxy
```

The supplied Compose topology is a development harness, not a production deployment: router auth is unconfigured, node ports are published directly, clients run privileged with a known SSH password, and several binaries have no build/deploy path. See [internal/README.md](internal/README.md).
