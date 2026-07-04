# Phase 1 — Durable Job and Commit Audit Trail

## Design

> Note: ownership and placement of ledger WAL in this phase are superseded by [Phase 1B — Storage-node WAL and router proxy](phase-1b-storage-node-wal-proxy.md). This document remains the baseline for durability mechanics (segmenting, replay, compaction), while Phase 1B changes where those mechanics run.

### Objective
Make workspace sync history durable across router restarts and build an auditable timeline for staged bundles and push/refresh/publish decisions.

### Current baseline

- Jobs are in-memory maps (`workspaceSyncJobs`, `workspaceBundles`) and expire on process restart (`internal/router/workspace_sync.go`, `internal/router/workspace_publish.go`).
- Fetcher staged bundles are also in-memory TTL cache (`internal/fetcher/workspace_publish.go`, `internal/fetcher/workspace_source_push.go`).
- UI list API is fixed-limit, no paging token/filter dimensions beyond action/state (`internal/router/workspace_sync_ui.go`, `api/proto/monofs.proto`).
- Router already has a local persisted-state pattern (atomic JSON snapshots) for Guardian stores (`internal/router/guardian_principals.go`, `internal/router/guardian_version_store.go`).

### Target architecture

#### 0. State directory separation from Guardian

The router currently persists Guardian deployment state (principals, file versions) in `--guardian-state-dir`. Workspace job data is a different concern: developer repository state, not infrastructure orchestration state. Mixing them in the same directory couples lifecycle, backup, and retention policies that have no business being coupled.

A dedicated **`--workspace-state-dir`** flag is introduced. On disk:

```
/var/lib/monofs/
  guardian/           # --guardian-state-dir (existing)
    guardian_principals.json
    guardian_versions.json
  workspace/          # --workspace-state-dir (new, Phase 1)
    jobs/
    bundles/
    audit/
```

Operational implications of the separation:

| Concern | Guardian state | Workspace state |
|---------|---------------|-----------------|
| What it tracks | Deployment topology, principal auth, file versions | Developer job history, bundle metadata, audit events |
| Growth profile | Bounded (tens of records) | Unbounded (thousands/day, managed by compaction + retention) |
| Backup strategy | Snapshot with Guardian config | Snapshot + S3 cold export (auditstore) |
| Retention | Tied to Guardian lifecycle | Independent: 30 days hot, 365 days cold |
| Disaster recovery | Losing it breaks the cluster | Losing it loses audit history but cluster keeps running |
| Disk I/O | Negligible (sync full-snapshot writes on small files) | WAL appends + periodic compaction (potentially high) |

**Config flag:** `--workspace-state-dir` (default: empty → pure in-memory, same zero-config behavior as Guardian stores today). Not the same as `--guardian-state-dir`. Must be on local persistent storage (not NFS), same as guardian state.

#### 1. Log-structured persistent store (replaces Guardian-style full snapshots)

Guardian's full-snapshot model works for bounded state (tens of principals, version history capped at 50 per path with machinery paths excluded). Workspace job data is fundamentally different: jobs accumulate over time, audit events are an unbounded append log, and bundle metadata grows with every publish. Rewriting a monolithic JSON file on every mutation is O(total data) — unacceptable at scale.

**Persistence model: Write-Ahead Log (WAL) + periodic snapshot compaction.**

```
<workspace-state-dir>/
  jobs/
    wal/
      wal-000001.log         # active or sealed segment
      wal-000002.log
      ...
    snapshots/
      snapshot-000100.json.gz
    checkpoints/
      checkpoint.json
  bundles/
    wal/
    snapshots/
    checkpoints/
  audit/
    wal/
    snapshots/
    checkpoints/
```

**WAL segment format (JSON Lines):**

Each segment file is append-only newline-delimited JSON. One JSON object per line, `\n` terminated. On recovery, truncated final lines (mid-write crash) are discarded; the WAL is self-repairing.

```jsonl
{"seq":1,"ts":"2026-07-03T10:00:00Z","op":"UPSERT","kind":"JOB","data":{...}}
{"seq":2,"ts":"2026-07-03T10:00:01Z","op":"UPSERT","kind":"BUNDLE","data":{...}}
{"seq":3,"ts":"2026-07-03T10:00:02Z","op":"INSERT","kind":"AUDIT","data":{...}}
{"seq":4,"ts":"2026-07-03T10:00:05Z","op":"UPSERT","kind":"JOB","data":{...}}
```

**Concurrency model:**

All mutations (job upserts, bundle metadata writes, audit event inserts) flow through a **single `sync.Mutex`** — `store.mu`. This is non-negotiable because the global `seq` counter defines total ordering across all three entity kinds, and that ordering is the correctness foundation for audit traceability (an audit event with `seq=N+1` must be chronologically after the job transition at `seq=N`).

```
Readers (GetJob, ListJobs, etc.):
  acquire mu.RLock      ← concurrent reads, no contention
  read in-memory index
  release mu.RUnlock

Writers (UpsertJob, UpsertBundle, InsertAudit):
  acquire mu.Lock       ← serialised writes, global ordering
  assign seq = nextSeq++
  update in-memory index
  append to WAL
  release mu.Unlock

Compaction:
  uses a SEPARATE mutex (store.compactMu) — does NOT block writes
  snapshots the current max seq under mu.RLock
  processes WAL segments below that seq
  writers continue appending to new/current segments unimpeded
```

No file locks (`flock`, `fcntl`). This is a single-process store within `--workspace-state-dir`. If multiple router processes share a workspace state dir (not supported), corruption is guaranteed. Config must document: one router process per workspace state dir.

Entry fields:
- `seq` — monotonic sequence number (global across all entity kinds for total ordering)
- `ts` — RFC 3339 wall-clock time of mutation
- `op` — `UPSERT` (job state transitions, bundle metadata) or `INSERT` (audit events; immutable)
- `kind` — entity type: `JOB`, `BUNDLE`, `AUDIT`
- `data` — the full entity record

**Write path:**

```
mutate(job) →
  1. acquire mu.Lock
  2. assign seq = nextSeq++
  3. update in-memory index
  4. append JSON line to active WAL segment file (O_APPEND | O_WRONLY, single fwrite)
  5. if segment size >= 64 MiB → rotate segment (close current, open wal-{next}.log)
  6. release mu.Lock
```

Appends are single `write()` syscalls per entry. On Linux `write()` to a regular file opened with `O_APPEND` is atomic (the kernel positions and writes in one step). A crash mid-write produces a truncated final line which recovery detects and discards. No `fsync` on every write — durability is relaxed to OS buffer flush interval (~30s by default). This trades a small crash window for throughput. For audit-sensitive deployments, an optional `fsync` mode can be enabled per config.

**Compaction:**

A background goroutine runs compaction when either condition is met:
- Total WAL size across all segments exceeds 256 MiB
- Compaction interval elapsed (configurable, default 5 minutes)

Compaction process:
1. Take checkpoint: record the latest WAL `seq` that will be compacted.
2. Read all WAL entries with `seq ≤ checkpoint.Seq`.
3. Reduce per entity kind:
   - **Jobs**: keep latest `UPSERT` per `job_id` (last-write-wins).
   - **Bundles**: keep all (immutable after creation; first UPSERT wins).
   - **Audit events**: keep all in `seq` order (append-only immutable).
4. Write compacted data as gzip-compressed JSON to `<store>/snapshots/snapshot-{checkpointSeq}.json.gz`.
5. Upload compacted snapshot to object storage via `auditstore` (`internal/storage/auditstore/`).
6. Delete WAL segments wholly below the checkpoint:
   - A segment file is deletable if all entries in it have `seq ≤ checkpoint.Seq`.
   - Segments that straddle the checkpoint are retained.
7. Write `checkpoint.json` with `{"last_compacted_seq": N}` — atomically via tmp+Rename.

**Recovery on startup:**
1. Load `checkpoint.json`. If missing, WAL is source of truth.
2. Load latest snapshot (`snapshot-{seq}.json.gz`).
3. Download and merge any compacted snapshots from S3 that are newer than the local snapshot.
4. Replay WAL entries with `seq > last_compacted_seq` from all remaining WAL segments, in `seq` order.
5. Rebuild in-memory hot indexes (job map, bundle map, audit slice).

**Scale ceiling:** This design handles 100K+ jobs and 1M+ audit events comfortably — WAL segments are rotated before they grow large, compaction is incremental (processes only new segments), and snapshots are compressed. The hot in-memory index holds recent jobs (pruned by retention policy); cold data lives in S3 snapshots.

#### 2. Router durable workspace job store (authoritative)

Package: `internal/router/workspacejobstore`

Wraps the log-structured store described above with entity-specific operations:
- `UpsertJob(job)` — write to WAL, update in-memory map
- `GetJob(jobID)` — O(1) from in-memory map
- `ListJobs(filter, pageToken)` — scan in-memory map with cursor pagination
- `JobCount()` — `len(inMemoryMap)`

#### 3. Bundle metadata ledger (payload-independent)

Co-located in `workspacejobstore`, using the same WAL (`kind: BUNDLE`). Persisted fields:
- `bundle_id`, `workspace_id`, `kind` (publish/source-push/refresh), `byte_size`, `repo_count`, `local_commit_ids`, `created_at`, `expires_at`, `discard_reason`, `job_id`

#### 4. Append-only audit events

Co-located in `workspacejobstore`, using the same WAL (`kind: AUDIT`, `op: INSERT`). Schema:
- `workspace_id`, `job_id`, `bundle_id`, `local_commit_id`, `actor_principal_id`, `decision` (allow/deny/auto-triggered), `reason_code`, `timestamp`, `correlation_id`

#### 5. S3 cold-storage export (`internal/storage/auditstore/`)

New package for uploading compacted snapshots to object storage and retrieving them on recovery. Reuses the existing `CloudStorageConfig` from `internal/storage` (`S3Region`, `S3Bucket`, `S3Prefix`, `S3Endpoint`, `S3AccessKeyID`, `S3SecretAccessKey`, `S3UsePathStyle`, and GCS equivalents). Does **not** go through the fetcher blob pipeline — the router connects directly to S3/GCS for audit data, keeping the fetcher pipeline dedicated to packager archives.

S3 key layout:
```
<prefix>/workspace-jobs/YYYY/MM/DD/snapshot-{checkpointSeq}-{timestamp}.json.gz
<prefix>/bundle-metadata/YYYY/MM/DD/snapshot-{checkpointSeq}-{timestamp}.json.gz
<prefix>/audit-events/YYYY/MM/DD/snapshot-{checkpointSeq}-{timestamp}.json.gz
```

Operations:
- `UploadSnapshot(entityKind, data []byte, checkpointSeq, timestamp)` → S3 key
- `ListSnapshots(entityKind, afterSeq uint64)` → []SnapshotInfo (key, seq, timestamp, size)
- `DownloadSnapshot(key)` → []byte
- Uses the AWS SDK and GCS SDK already vendored in the project.

Router config additions (in Phase 0 feature flags):
```
WorkspaceStateDir       string // path for WAL + snapshots; separate from GuardianStateDir
                                 // default: "" → pure in-memory (backward-compatible)
AuditStorageEnabled     bool   // enable S3/GCS export of compacted snapshots
AuditStorageType        string // "s3" or "gcs"
AuditS3Bucket           string
AuditS3Prefix           string // e.g. "monofs/audit/"
AuditS3Region           string
AuditS3Endpoint         string // for MinIO/Ceph
AuditS3AccessKeyID      string
AuditS3SecretAccessKey  string
AuditS3UsePathStyle     bool
AuditGCSBucket          string
AuditGCSPrefix          string
AuditGCSCredentialsFile string
```

If `AuditStorageEnabled` is false (default), compaction still runs locally but no S3 upload occurs — suitable for single-node deployments where local disk retention is sufficient.

### API evolution

- Extend `ListWorkspaceSyncJobsRequest` for cursor pagination and richer filters:
  - `workspace_id`, `requested_by_client_id`, `logical_branch`, `created_after`, `created_before`, `page_size`, `page_token`
- Add query endpoints for audit events and bundle metadata.
- Add `ExportAuditSnapshot` admin RPC for on-demand compaction+upload triggers.

### Scale and performance targets

Design assumptions: 2,000 engineers in a monorepo organization, each producing ~20 local commits/day. Engineers push to upstream 3–4 times per day, peaking in a 4-hour afternoon window.

| Metric | Target | Derived from |
|--------|--------|-------------|
| Active workspaces | ≤ 3,000 | 2,000 engineers + 50% slack for CI/automation workspaces |
| Source-push jobs / day | ~8,000 | 2,000 × 4 pushes/day |
| Audit events / day | ~80,000 | 8,000 jobs × ~10 events/job (lifecycle + repo-level) |
| Peak audit writes / sec | ≤ 50 | 4× burst over average during peak window |
| Active jobs in hot index | ≤ 10,000 | ~1 day of jobs, pruned by retention |
| Concurrent pushes | ≤ 50 | ~13/minute sustained, 50 peak (auto-push bursts + manual overlap) |
| WAL segment size | 64 MiB before rotation | Keeps per-segment replay fast |
| Compaction trigger (size) | 256 MiB total WAL | ~40,000 audit events before compaction fires |
| Compaction trigger (time) | 5 minutes | Upper bound: compaction runs even at low volume |
| S3 upload throughput | Best-effort, async after compaction | Snapshots are ~hundreds of KB gzipped |
| Recovery time (10K jobs, 100K events) | ≤ 3 seconds | Cold start; typical is sub-second after crash |
| Audit event retention (hot) | 30 days (local WAL + snapshots) | Configurable; beyond this lives in S3 |
| Audit event retention (cold) | 365 days (S3 snapshots) | Bucket lifecycle policy handles expiry |

## Implementation plan

1. **New package: `internal/storage/auditstore/`**
   - S3/GCS client wrapper using existing `CloudStorageConfig`
   - `UploadSnapshot`, `ListSnapshots`, `DownloadSnapshot`
   - Unit tests with mock S3 (or MinIO testcontainer)

2. **New package: `internal/router/workspacejobstore/`**
   - `logstore.go` — WAL segment management (open, rotate, append, replay)
   - `compactor.go` — snapshot creation, gzip, checkpoint management, S3 upload via auditstore
   - `recovery.go` — startup: load checkpoint → load local snapshot → fetch S3 snapshots → replay WAL
   - `jobstore.go` — entity-specific ops (UpsertJob, GetJob, ListJobs, UpsertBundle, InsertAudit)
   - `retention.go` — prune hot index by age/count, delete local snapshots beyond retention window
   - Interfaces: `WorkspaceJobStore`, `WorkspaceBundleMetadataStore`, `WorkspaceAuditEventStore`

3. **WAL implementation details**
   - Single `sync.Mutex` for all writes to guarantee total `seq` ordering across entity kinds
   - `O_APPEND | O_WRONLY | O_CREATE` for segment files
   - `.tmp` segment for rotation: close old segment, rename `.tmp` → numbered `.log`, open new segment as `.tmp`
   - Recovery: scan each segment line-by-line, skip truncated final line (no trailing `\n`), sort by `seq`

4. **Compaction implementation details**
   - Background goroutine with `time.Ticker` (interval) + size threshold check on each write
   - Compaction mutex separate from write mutex — writes continue to new WAL segments during compaction
   - Snapshot format: `{"checkpoint_seq": N, "jobs": {...}, "bundles": {...}, "audit_events": [...]}` — then gzipped
   - On successful S3 upload, delete compacted WAL segments and optionally delete local snapshot (if S3 is the authoritative copy)
   - Checkpoint file is the fence: WAL segments below checkpoint can be deleted once checkpoint file is written

5. **Refactor job mutation paths**
   - `updateWorkspaceSyncJob`, `updateWorkspaceSyncRepository`, finalize/fail/cancel paths call `jobstore.UpsertJob()`
   - Bundle upload/discard handlers call `jobstore.UpsertBundle()`
   - Push/refresh/publish decision points call `jobstore.InsertAudit()`

6. **Router startup integration**
   - Wire `WorkspaceStateDir` into `RouterConfig` (separate field from `GuardianStateDir`)
   - Initialize `workspacejobstore` with `cfg.WorkspaceStateDir` before gRPC server starts
   - If `WorkspaceStateDir` is empty: operate pure in-memory (backward-compatible, no persistence)
   - Load hot index from WAL replay + snapshots
   - Register background compaction goroutine, wire into router lifecycle (shutdown → flush final WAL + compact)

7. **Proto and API updates**
   - Extend `ListWorkspaceSyncJobsRequest` for cursor pagination and richer filters
   - Add `ListAuditEventsRequest/Response`, `ListBundleMetadataRequest/Response`
   - Add `ExportAuditSnapshotRequest/Response` (admin RPC)
   - Update UI handler (`workspace_sync_ui.go`) to use paginated store queries

8. **Retention worker**
   - Hot index: prune jobs older than `JobRetentionDays` (config), capped at `MaxJobsPerWorkspace` (config)
   - Local snapshots: delete `.json.gz` files older than `LocalRetentionDays`
   - S3 snapshots: lifecycle policy on bucket (retention handled at bucket level, not by router)
   - Never delete audit events before associated jobs — ensure referential integrity

9. **Migration**
   - No backfill for pre-phase data
   - Clean bootstrap if no persisted files exist
   - If old-format Guardian-style JSON snapshot is found, ignore it (different path); no migration needed

### Operational notes

- Append-only WAL ensures crash safety: the worst case is losing `≤ flush_interval` of writes on OS crash.
- For stricter durability, set `AuditFsyncEnabled=true` — the router calls `fsync()` after each WAL append. This reduces write throughput but guarantees no data loss on power failure.
- S3 export is async and best-effort: if S3 is unreachable, compaction still completes locally. Next compaction cycle retries.
- Monitor: Prometheus metrics for WAL size, compaction duration, S3 upload success/failure, recovery time, hot index size.
- The `auditstore` package is intentionally separate from the fetcher blob pipeline — audit data has different access patterns (batch upload, range queries by time) vs packager archives (random access by blob hash).

