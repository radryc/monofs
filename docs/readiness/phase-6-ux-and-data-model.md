# Phase 6 — Monorepo UX and Data Model Enhancements

## Design

### Objective
Add platform-wide commit/diff observability and close operation-model gaps (rename/chmod) without breaking existing sessions and bundles.

### Current baseline

- Local commit metadata exists in session overlay DB and source commit bundle (`internal/fuse/session_vcs.go`, `internal/workspacebundle/commit_bundle.go`), but no cross-workspace searchable ledger.
- Publish/source-push operation model currently supports:
  - `upsert`, `delete`, `mkdir`, `rmdir`, `symlink` (`internal/workspacebundle/bundle.go` and fetcher apply logic in `internal/fetcher/workspace_publish.go`).
- Rename/chmod are not represented as first-class bundle operations.

### Commit/diff ledger model

Three normalized tables stored in the Phase 1 WAL (`kind: LEDGER`). All records are immutable after write. The in-memory index provides point and range queries; cold data lives in S3 snapshots via `auditstore`.

#### Table 1: `local_commits`

One record per virtual commit created in any session overlay. Immutable.

```json
{
  "table": "local_commits",
  "local_commit_id": "c0a1b2...",       // PK — session overlay commit ID
  "workspace_id": "ws-prod-frontend",
  "principal_id": "client-uuid-1234",    // FUSE client that created the commit
  "repo_storage_id": "github-frontend",  // which monorepo sub-repository
  "author_name": "Jane Doe",
  "author_email": "jane@example.com",
  "message": "fix: navbar z-index",
  "message_full": "fix: navbar z-index\n\n...",  // full body (optional, may be large)
  "files_changed": 3,
  "additions": 12,                       // optional, tracked if session provides diff stats
  "deletions": 4,
  "parent_commit_id": "a9b8c7...",       // previous local commit in this workspace, null for root
  "timestamp_unix": 1720000000
}
```

#### Table 2: `push_outcomes`

One record per repository when a source-push job completes. Links local commits to upstream.

```json
{
  "table": "push_outcomes",
  "push_outcome_id": "job-xyz:github-frontend",  // PK — composite of {job_id}:{repo_storage_id}
  "job_id": "job-xyz",
  "workspace_id": "ws-prod-frontend",
  "repo_storage_id": "github-frontend",
  "branch": "main",
  "push_mode": "squash",                // or "preserve"
  "branch_strategy": "direct",          // "direct" | "workspace_branch" | "per_repo_branch"
  "upstream_commit_hash": "d4e5f6...",  // the commit hash now on upstream (squash: one; preserve: tip)
  "local_commit_ids": ["c0a1b2...", "c0a1b3..."],  // commits included in this push
  "status": "pushed",                   // "pushed" | "conflict" | "failed"
  "error_message": "",                  // set if conflict or failed
  "pr_url": "https://github.com/org/repo/pull/42",  // populated if auto-PR was created (Phase 3)
  "timestamp_unix": 1720000100
}
```

#### Table 3: `refresh_events`

One record per repository when a workspace refresh detects upstream changes.

```json
{
  "table": "refresh_events",
  "refresh_event_id": "ws-prod-frontend:github-frontend:1720000300",  // PK — composite
  "workspace_id": "ws-prod-frontend",
  "repo_storage_id": "github-frontend",
  "previous_upstream_hash": "b2c3d4...",  // hash before refresh
  "new_upstream_hash": "d4e5f6...",       // hash after refresh (new tip)
  "incoming_commits": 5,                  // how many commits were pulled
  "timestamp_unix": 1720000300
}
```

#### In-memory indexes

Rebuilt from WAL on startup. All indexes store `seq` numbers (WAL sequence) pointing into a contiguous slice for O(1) record access. Total memory: ~200K records × ~500 bytes avg ≈ 100 MiB.

| Index | Structure | Use case |
|-------|-----------|----------|
| `by_local_commit_id` | `map[string]*LocalCommit` | "What's the status of commit X?" — point lookup |
| `by_job_id` | `map[string]*PushOutcome` | "Show me the push for job Y" — point lookup |
| `by_workspace` | `map[string][]uint64` | "What commits are in workspace W?" — ordered scan |
| `by_principal` | `map[string][]uint64` | "What did engineer E do today?" — filtered scan |
| `by_repo` | `map[string][]uint64` | "What's happening on repo R?" — filtered scan |
| `by_push_status` | `map[string][]uint64` | "Show me all failed pushes" — filtered scan |
| `time_index` | `[]uint64` | Time-range scans, sorted by `timestamp_unix` ascending |

Filtered scans work by intersecting the time_index with the relevant secondary index (e.g., "workspace W, last 7 days" = `intersect(time_range, by_workspace[W])`).

#### Query API (proto)

```protobuf
message QueryLedgerRequest {
    // Zero, one, or many filters — ANDed together.
    string workspace_id     = 1;
    string principal_id     = 2;
    string repo_storage_id  = 3;
    string local_commit_id  = 4;
    string job_id           = 5;
    string push_status      = 6;  // "pushed" | "conflict" | "failed"
    string branch           = 7;

    // Time range (Unix seconds).
    int64 created_after     = 10;
    int64 created_before    = 11;

    // Pagination.
    int32  page_size        = 20;
    string page_token       = 21;

    // Result kind filter.
    LedgerResultKind result_kind = 30;  // COMMITS_ONLY | PUSH_OUTCOMES_ONLY | REFRESH_EVENTS_ONLY | ALL
}

enum LedgerResultKind {
    LEDGER_RESULT_KIND_UNSPECIFIED = 0;
    LEDGER_RESULT_KIND_ALL         = 1;
    LEDGER_RESULT_KIND_COMMITS_ONLY       = 2;
    LEDGER_RESULT_KIND_PUSH_OUTCOMES_ONLY = 3;
    LEDGER_RESULT_KIND_REFRESH_EVENTS_ONLY = 4;
}

message QueryLedgerResponse {
    repeated LocalCommit   commits        = 1;
    repeated PushOutcome   push_outcomes  = 2;
    repeated RefreshEvent  refresh_events = 3;
    string next_page_token = 4;
    int32  total_matches   = 5;  // approximate (capped at 10000)
}

// Entity messages mirror the stored tables (proto-compatible with JSON above).
message LocalCommit {
    string local_commit_id  = 1;
    string workspace_id     = 2;
    string principal_id     = 3;
    string repo_storage_id  = 4;
    string author_name      = 5;
    string author_email     = 6;
    string message          = 7;
    string message_full     = 8;
    int32  files_changed    = 9;
    int32  additions        = 10;
    int32  deletions        = 11;
    string parent_commit_id = 12;
    int64  timestamp_unix   = 13;
}

message PushOutcome {
    string push_outcome_id      = 1;
    string job_id               = 2;
    string workspace_id         = 3;
    string repo_storage_id      = 4;
    string branch               = 5;
    string push_mode            = 6;
    string branch_strategy      = 7;
    string upstream_commit_hash = 8;
    repeated string local_commit_ids = 9;
    string status               = 10;
    string error_message        = 11;
    string pr_url               = 12;
    int64  timestamp_unix       = 13;
}

message RefreshEvent {
    string refresh_event_id       = 1;
    string workspace_id           = 2;
    string repo_storage_id        = 3;
    string previous_upstream_hash = 4;
    string new_upstream_hash      = 5;
    int32  incoming_commits       = 6;
    int64  timestamp_unix         = 7;
}
```

#### CLI surface

```bash
# What commits exist in a workspace?
monofs-session ledger commits --workspace ws-prod-frontend

# What's the push status of a specific commit?
monofs-session ledger status --commit c0a1b2...

# What did engineer X do today?
monofs-session ledger activity --principal client-uuid-1234 --since 24h

# What pushes failed in the last week?
monofs-session ledger failures --since 7d

# Full search with pagination:
monofs-session ledger query \
  --workspace ws-prod-frontend \
  --repo github-frontend \
  --status failed \
  --since 2026-07-01 \
  --limit 50
```

#### Query execution plan

```
QueryLedger(request):
  1. Collect candidate seq sets from each non-empty filter:
     - workspace_id   → by_workspace[workspace_id]
     - principal_id   → by_principal[principal_id]
     - repo_storage_id → by_repo[repo_storage_id]
     - push_status    → by_push_status[push_status]
     - local_commit_id → single-record lookup (short-circuit)
     - job_id         → single-record lookup (short-circuit)
     - time range     → binary search on time_index
  2. Intersect all non-nil candidate sets (AND semantics).
  3. Sort by timestamp_unix descending.
  4. Apply result_kind filter (skip non-matching tables).
  5. Paginate: serialize records up to page_size, encode cursor as (last_timestamp, last_seq).
  6. Return records + next_page_token + approximate total_matches (cap at 10,000).
```

### Scale and performance targets

Design assumptions from global profile: 2,000 engineers × 20 commits/day = 40,000 local commits/day. Each commit generates one ledger record. Each push outcome covers ~5 commits on average. Each refresh generates one record per repo per workspace.

| Metric | Target |
|--------|--------|
| Ledger records / day | ~60,000 (40K local commits + 8K push outcomes + ~12K refresh events) |
| Ledger records in hot index | ≤ 200,000 (~3 days; pruned by retention) |
| Cold storage (S3 via Phase 1 auditstore) | 365 days, partitioned by date |
| List query response (p99) | ≤ 100 ms for scans up to 10,000 records |
| Point query (by local_commit_id or job_id) | ≤ 5 ms (in-memory index) |

### Extended operation model

- Add explicit operation kinds:
  - `rename`
  - `chmod`
- Bundle schema versioning:
  - include format version in bundle payload
  - retain backward compatibility for old clients.

## Implementation plan

1. **Ledger data model and persistence** (`internal/router/workspaceledger/`):
   - `tables.go` — Go structs for `LocalCommit`, `PushOutcome`, `RefreshEvent` with JSON and proto tags
   - `index.go` — in-memory index structures (`by_local_commit_id`, `by_workspace`, `by_principal`, `by_repo`, `by_push_status`, `time_index`)
   - `store.go` — `Insert(record)`, `Query(req) → response`, `RebuildIndexes()` (called on startup from WAL replay)
   - `recovery.go` — rebuild all in-memory indexes from WAL entries with `table: LEDGER` during Phase 1 store recovery
   - Interfaces: `CommitLedger` (query surface), `LedgerWriter` (ingestion surface)
2. **Ingestion hook points** (write-through, no async ingestion):
   - `internal/fuse/session_vcs.go` — after `createLocalCommit`, emit `local_commit` record via `LedgerWriter.Insert()`
   - `internal/router/workspace_source_push.go` — after fetcher reports push completion per repo, emit `push_outcome` record
   - `internal/router/workspace_sync.go` — after refresh completes per repo, emit `refresh_event` record
   - All emits go through the Phase 1 WAL (`kind: LEDGER`) for durability; the in-memory index is updated synchronously
3. **Proto and API updates**:
   - Add `QueryLedgerRequest/Response`, `LocalCommit`, `PushOutcome`, `RefreshEvent` messages to `api/proto/monofs.proto`
   - Wire `QueryLedger` RPC in the router gRPC server
   - Update `cmd/monofs-session/` CLI with `ledger` subcommand group (commits, status, activity, failures, query)
4. Extend bundle operation enums and validation in `internal/workspacebundle`.
5. Extend fetcher operation applier to safely handle `rename` and `chmod`.
6. Add compatibility layer:
   - old bundles still parse and apply
   - new operations gated by bundle version and feature flags.
7. Add tests:
   - rename/chmod apply semantics
   - mixed old/new bundle compatibility
   - ledger query correctness at scale filters.

