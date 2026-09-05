# Internal Router Subsystems Documentation

## Table of Contents

- [mergerequest/store.go](#mergerequeststorego)
- [workspaceledger/ledger.go](#workspaceledgerledgergo)
- [workspacepolicy/policy.go](#workspacepolicypolicygo)
- [workspacepr/provider.go](#workspaceprprovidergo)

---

## mergerequest/store.go

**Package:** `mergerequest` — provides a native merge-request (proposal) store for MonoFS-managed partitions that are not backed by an external git provider. Non-owners open proposals; owners of affected subtrees review, approve, and merge them. This is the MonoFS-native counterpart to the external GitHub/GitLab pull-request flow (authz epic D3).

**Imports:** `context`, `fmt`, `sync`, `time`, `github.com/radryc/monofs/pkg/authz`

### Types

####  Type `State`

```go
type State string
```

The lifecycle state of a proposal.

**Constants defined:**

| Constant       | Value      | Meaning |
|----------------|------------|---------|
| `StateOpen`    | `"open"`   | Proposal is open for review |
| `StateApproved`| `"approved"` | At least one owner has approved |
| `StateMerged`  | `"merged"` | Proposal has been merged |
| `StateRejected`| `"rejected"`| Proposal has been rejected |

#### Struct `Proposal`

```go
type Proposal struct {
    ID          string
    Partition   string
    Paths       []string
    Author      string
    Title       string
    Description string
    State       State
    Approvals   []string
    CreatedAt   int64
    UpdatedAt   int64
}
```

Represents a proposed change to a set of partition subtree paths authored by a non-owner, awaiting owner review.

- `ID` — auto-generated proposal identifier (format: `"mr-N"`)
- `Partition` — the partition name this proposal targets
- `Paths` — the subtree paths affected by the proposal
- `Author` — principal ID of the proposer
- `Approvals` — list of principal IDs of owners who have approved
- `CreatedAt`, `UpdatedAt` — Unix timestamps

#### Interface `OwnershipChecker`

```go
type OwnershipChecker interface {
    OwnsAll(ctx context.Context, paths []string, id authz.Identity) (ownsAll bool, unowned []string, err error)
}
```

Reports whether an identity owns (may directly modify) all of the given paths. `*authz.OwnershipResolver` satisfies this interface.

#### Struct `Store`

```go
type Store struct {
    mu        sync.Mutex
    proposals map[string]*Proposal
    owners    OwnershipChecker
    seq       int
    now       func() time.Time
}
```

An in-memory, thread-safe proposal store gated on subtree ownership. All fields are unexported; access is synchronized via `sync.Mutex`. The `now` function enables deterministic timestamps in tests.

---

### Functions

#### `NewStore(owners OwnershipChecker) *Store`

```go
func NewStore(owners OwnershipChecker) *Store
```

**What it does:** Constructs a new proposal store whose approval/merge gate uses the given `OwnershipChecker` for authorization checks.

**Callers:** Called by server initialization code (e.g., in `internal/server/server.go`) when setting up MonoFS-managed partitions not backed by an external git provider. The `owners` parameter is typically an `*authz.OwnershipResolver`.

**Parameters:**
- `owners` — the ownership checker used to gate Approve, Merge, and Reject operations

**Returns:** A pointer to an initialized `Store` with an empty proposals map and `time.Now` as the clock.

**Implementation details:**
- Initializes `proposals` as an empty `map[string]*Proposal`
- Sets `now` to `time.Now` (injectable for testing)
- Sets `seq` to 0 (auto-incremented on each `Create` call)

---

#### `(s *Store) Create(author, partition string, paths []string, title, description string) (*Proposal, error)`

```go
func (s *Store) Create(author, partition string, paths []string, title, description string) (*Proposal, error)
```

**What it does:** Opens a new proposal authored by `author` for the given partition paths.

**Callers:** Called by the router when a non-owner principal initiates a merge request (the `CreateMergeRequest` or equivalent RPC handler).

**Parameters:**
- `author` — principal ID of the proposer (required, non-empty)
- `partition` — the partition name
- `paths` — the subtree paths affected (required, at least one)
- `title` — proposal title
- `description` — proposal description

**Returns:** A cloned copy of the newly created `Proposal`, or an error if validation fails.

**Implementation details:**
- Validates that `author` is non-empty and `paths` is non-empty
- Acquires the mutex lock, increments `seq`, generates ID as `"mr-{seq}"`
- Copies `paths` via `append([]string(nil), paths...)` to avoid aliasing
- Sets state to `StateOpen`, timestamps to `now().Unix()`
- Returns a shallow clone (value copy of the struct, slices are shared but the `Approvals` slice is empty at creation)

---

#### `(s *Store) Get(id string) (*Proposal, bool)`

```go
func (s *Store) Get(id string) (*Proposal, bool)
```

**What it does:** Returns a copy of a proposal by its ID.

**Callers:** Called by the router to retrieve a specific proposal for display or status checks.

**Parameters:**
- `id` — the proposal identifier (e.g., `"mr-1"`)

**Returns:** A cloned copy of the `Proposal` and `true` if found; `nil, false` if not found.

**Implementation details:**
- Thread-safe via mutex lock
- Returns a value copy (shallow clone) of the stored proposal, so mutations to the returned struct do not affect the store

---

#### `(s *Store) List(partition string) []*Proposal`

```go
func (s *Store) List(partition string) []*Proposal
```

**What it does:** Returns copies of all proposals, optionally filtered by partition.

**Callers:** Called by the router to list proposals (e.g., for a dashboard or API endpoint).

**Parameters:**
- `partition` — partition name to filter by; empty string (`""`) means return all proposals

**Returns:** A slice of cloned `*Proposal` pointers. An empty slice (not nil) if no proposals match.

**Implementation details:**
- Thread-safe via mutex lock
- Iterates over all proposals in the map; skips those not matching the partition filter
- Each returned proposal is a shallow clone (value copy)

---

#### `(s *Store) Approve(ctx context.Context, id string, approver authz.Identity) (*Proposal, error)`

```go
func (s *Store) Approve(ctx context.Context, id string, approver authz.Identity) (*Proposal, error)
```

**What it does:** Records an owner approval on a proposal. The approver must own every affected path, must not be the author, and the proposal must be in the `open` state. A single approval moves the proposal to the `approved` state.

**Callers:** Called by the router when an owner principal approves a merge request.

**Parameters:**
- `ctx` — context for the ownership check
- `id` — the proposal identifier
- `approver` — the identity of the approving principal

**Returns:** A cloned copy of the updated `Proposal`, or an error if any validation fails.

**Error conditions:**
- `"mergerequest: approver identity required"` — the approver's `PrincipalID()` is empty
- `"mergerequest: proposal %q not found"` — proposal ID not in the store
- `"mergerequest: proposal %q is %s, not open"` — proposal is not in `StateOpen`
- `"mergerequest: author cannot approve own proposal"` — the author tries to approve
- Ownership check failure from `requireOwner` — approver doesn't own all paths

**Implementation details:**
- Deduplicates approvals: if the approver already exists in `p.Approvals`, it is not appended again
- Sets state to `StateApproved` and updates `UpdatedAt` timestamp
- Returns a clone of the mutation

---

#### `(s *Store) Merge(ctx context.Context, id string, merger authz.Identity) (*Proposal, error)`

```go
func (s *Store) Merge(ctx context.Context, id string, merger authz.Identity) (*Proposal, error)
```

**What it does:** Merges an approved proposal. The merger must own every affected path.

**Callers:** Called by the router when an owner principal merges an approved merge request.

**Parameters:**
- `ctx` — context for the ownership check
- `id` — the proposal identifier
- `merger` — the identity of the merging principal

**Returns:** A cloned copy of the updated `Proposal`, or an error if validation fails.

**Error conditions:**
- `"mergerequest: proposal %q not found"` — proposal ID not in the store
- `"mergerequest: proposal %q must be approved before merge (state=%s)"` — proposal is not in `StateApproved`
- Ownership check failure from `requireOwner`

**Implementation details:**
- Sets state to `StateMerged` and updates `UpdatedAt` timestamp
- Does not perform any actual file merging — only records the state transition

---

#### `(s *Store) Reject(ctx context.Context, id string, by authz.Identity) (*Proposal, error)`

```go
func (s *Store) Reject(ctx context.Context, id string, by authz.Identity) (*Proposal, error)
```

**What it does:** Closes a proposal without merging. Only an owner may reject.

**Callers:** Called by the router when an owner rejects a merge request.

**Parameters:**
- `ctx` — context for the ownership check
- `id` — the proposal identifier
- `by` — the identity of the rejecting principal

**Returns:** A cloned copy of the updated `Proposal`, or an error if validation fails.

**Error conditions:**
- `"mergerequest: proposal %q not found"` — proposal ID not in the store
- `"mergerequest: proposal %q already merged"` — proposal is in `StateMerged`
- Ownership check failure from `requireOwner`

**Implementation details:**
- A merged proposal cannot be rejected
- An already rejected proposal can be re-rejected (no check for current state besides merged)
- Sets state to `StateRejected` and updates `UpdatedAt`

---

#### `(s *Store) requireOwner(ctx context.Context, p *Proposal, id authz.Identity) error`

```go
func (s *Store) requireOwner(ctx context.Context, p *Proposal, id authz.Identity) error
```

**What it does:** Verifies that the given identity owns all paths in the proposal. This is the internal authorization gate used by `Approve`, `Merge`, and `Reject`.

**Callers:** Called internally by `Approve`, `Merge`, and `Reject`.

**Parameters:**
- `ctx` — context for the ownership check
- `p` — the proposal whose paths must be checked
- `id` — the identity to check ownership for

**Returns:** `nil` if the identity owns all paths; an error otherwise.

**Error conditions:**
- `"mergerequest: no ownership checker configured"` — `s.owners` is nil
- `"mergerequest: ownership check: ..."` — the `OwnsAll` call returned an error
- `"mergerequest: %q does not own paths %v"` — the identity does not own all paths

**Implementation details:**
- Delegates to `s.owners.OwnsAll(ctx, p.Paths, id)` for the actual check
- Returns the list of unowned paths in the error message for debugging

---

#### `contains(items []string, want string) bool`

```go
func contains(items []string, want string) bool
```

**What it does:** A linear search helper that checks whether a string slice contains a specific value.

**Callers:** Called internally by `Approve` to deduplicate approvals.

**Parameters:**
- `items` — the string slice to search
- `want` — the value to find

**Returns:** `true` if `want` is present in `items`; `false` otherwise.

**Implementation details:** Simple O(n) linear scan. Used only for the small `Approvals` slice (typically 1–3 entries).

---

## workspaceledger/ledger.go

**Package:** `workspaceledger` — provides an in-memory ledger for tracking workspace-local commits, push outcomes, and refresh events. Supports optional WAL (Write-Ahead Log) persistence and configurable queries with filtering.

**Imports:** `encoding/json`, `sort`, `sync`, `github.com/radryc/monofs/api/proto` (as `pb`)

### Types

#### Interface `WALWriter`

```go
type WALWriter interface {
    InsertLedger(data []byte) error
}
```

Abstracts the write-ahead log sink. Implementations persist ledger records to durable storage (e.g., a file-based WAL). Used by `Ledger` to append records before in-memory insertion.

#### Struct `Ledger`

```go
type Ledger struct {
    mu          sync.RWMutex
    wal         WALWriter
    commits     []*pb.LocalCommit
    outcomes    []*pb.PushOutcome
    refreshes   []*pb.RefreshEvent
    byCommitID  map[string]*pb.LocalCommit
    byJobID     map[string]*pb.PushOutcome
    byWorkspace map[string][]int
    byPrincipal map[string][]int
    byRepo      map[string][]int
    byStatus    map[string][]int
}
```

An in-memory, thread-safe multi-table ledger with secondary indexes.

- `mu` — `RWMutex` allowing concurrent reads
- `wal` — optional WAL writer for durability; nil if disabled
- `commits`, `outcomes`, `refreshes` — ordered slices of records in insertion order
- `byCommitID` — maps local commit IDs to their `*pb.LocalCommit`
- `byJobID` — maps push job IDs to their `*pb.PushOutcome`
- `byWorkspace`, `byPrincipal`, `byRepo` — maps from string keys to slice indexes into the corresponding record arrays (used for fast lookup in queries; not directly used as indexes in the current `Query` implementation, which does linear scans with filter functions instead)
- `byStatus` — maps status strings to push outcome indexes (e.g., `"outcome:success"`)

#### Unexported Struct `ledgerRecord`

```go
type ledgerRecord struct {
    Table string          `json:"table"`
    Data  json.RawMessage `json:"data"`
}
```

The on-disk format for a WAL entry. `Table` identifies the logical table (`"local_commits"`, `"push_outcomes"`, or `"refresh_events"`), and `Data` is the JSON-encoded protobuf message.

---

### Functions

#### `New() *Ledger`

```go
func New() *Ledger
```

**What it does:** Constructs a new in-memory Ledger without WAL support.

**Callers:** Called by `internal/server/server.go:382` during server initialization when no WAL path is configured.

**Returns:** A pointer to an initialized `Ledger` with empty slices, all index maps initialized, and `wal` set to nil.

**Implementation details:**
- Allocates empty maps for all six index maps
- Does not set `wal` (remains nil)

---

#### `NewWithWAL(wal WALWriter) *Ledger`

```go
func NewWithWAL(wal WALWriter) *Ledger
```

**What it does:** Constructs a new Ledger with a WAL writer for durability. Every insert operation will also be persisted to the WAL.

**Callers:** Called by `internal/server/server.go:394,397` when a WAL directory path is configured in the server settings.

**Parameters:**
- `wal` — the WAL writer implementation to use for persisting records

**Returns:** A pointer to an initialized `Ledger` with the WAL writer set.

---

#### `(l *Ledger) InsertCommit(c *pb.LocalCommit)`

```go
func (l *Ledger) InsertCommit(c *pb.LocalCommit)
```

**What it does:** Inserts a local commit record into the ledger. If a WAL writer is configured, the record is also persisted to the WAL.

**Callers:** Called by the router when a workspace-local commit is created (e.g., after a `SOURCE_PUSH` or local snapshot operation).

**Parameters:**
- `c` — the `*pb.LocalCommit` protobuf message to insert

**Returns:** Nothing (fire-and-forget). WAL write errors are silently ignored.

**Implementation details:**
- Marshals the commit into a `ledgerRecord{Table: "local_commits", Data: ...}` JSON blob
- Calls `l.wal.InsertLedger(data)` if WAL is configured (ignoring errors)
- Indexes by `LocalCommitId`, `WorkspaceId`, `PrincipalId`, and `RepoStorageId`

---

#### `(l *Ledger) InsertPushOutcome(o *pb.PushOutcome)`

```go
func (l *Ledger) InsertPushOutcome(o *pb.PushOutcome)
```

**What it does:** Inserts a push outcome record into the ledger. If a WAL writer is configured, the record is also persisted.

**Callers:** Called by the router when a push operation completes (success or failure).

**Parameters:**
- `o` — the `*pb.PushOutcome` protobuf message to insert

**Returns:** Nothing.

**Implementation details:**
- Marshals into `ledgerRecord{Table: "push_outcomes", Data: ...}`
- Indexes by `JobId`, `WorkspaceId`, `RepoStorageId`, and `Status` (status key format: `"outcome:<status>"`)

---

#### `(l *Ledger) InsertRefreshEvent(r *pb.RefreshEvent)`

```go
func (l *Ledger) InsertRefreshEvent(r *pb.RefreshEvent)
```

**What it does:** Inserts a refresh event record into the ledger. If a WAL writer is configured, the record is also persisted.

**Callers:** Called by the router when a workspace refresh completes.

**Parameters:**
- `r` — the `*pb.RefreshEvent` protobuf message to insert

**Returns:** Nothing.

**Implementation details:**
- Marshals into `ledgerRecord{Table: "refresh_events", Data: ...}`
- Unlike commits/outcomes, refresh event insertion is done inline in the locked section (not delegated to a separate locked helper)
- Indexes by `WorkspaceId` and `RepoStorageId`

---

#### `(l *Ledger) ReplayFromWAL(entryData []byte) error`

```go
func (l *Ledger) ReplayFromWAL(entryData []byte) error
```

**What it does:** Replays a single WAL entry into the ledger. Used during startup to rebuild in-memory state from the WAL.

**Callers:** Called by the server's WAL replay logic during initialization, iterating over all persisted WAL entries.

**Parameters:**
- `entryData` — raw JSON bytes of a `ledgerRecord` from the WAL

**Returns:** An error if the JSON cannot be unmarshaled into a `ledgerRecord` or the nested data cannot be unmarshaled into the corresponding protobuf message.

**Implementation details:**
- Unmarshals the outer `ledgerRecord` to determine the table
- Routes to the appropriate table-specific unmarshal and insertion:
  - `"local_commits"` → unmarshal as `pb.LocalCommit` → call `insertCommitLocked`
  - `"push_outcomes"` → unmarshal as `pb.PushOutcome` → call `insertOutcomeLocked`
  - `"refresh_events"` → unmarshal as `pb.RefreshEvent` → index directly (same inline approach as `InsertRefreshEvent`)
- Does NOT re-persist to the WAL (avoids infinite replay loop)
- NOT thread-safe — must be called before the ledger is exposed to concurrent access

---

#### `(l *Ledger) insertCommitLocked(c *pb.LocalCommit)`

```go
func (l *Ledger) insertCommitLocked(c *pb.LocalCommit)
```

**What it does:** Internal helper that appends a commit to the in-memory slice and updates all relevant indexes. Caller must hold the write lock.

**Callers:** Called by `InsertCommit` (under lock) and `ReplayFromWAL` (during initialization, before concurrent access).

**Parameters:**
- `c` — the `*pb.LocalCommit` to insert

**Implementation details:**
- Appends to `l.commits` and records the index
- Indexes by: `local_commit_id` → `*pb.LocalCommit`, `workspace_id` → index, `principal_id` → index, `repo_storage_id` → index

---

#### `(l *Ledger) insertOutcomeLocked(o *pb.PushOutcome)`

```go
func (l *Ledger) insertOutcomeLocked(o *pb.PushOutcome)
```

**What it does:** Internal helper that appends a push outcome to the in-memory slice and updates indexes. Caller must hold the write lock.

**Callers:** Called by `InsertPushOutcome` (under lock) and `ReplayFromWAL`.

**Parameters:**
- `o` — the `*pb.PushOutcome` to insert

**Implementation details:**
- Appends to `l.outcomes` and records the index
- Indexes by: `job_id` → `*pb.PushOutcome`, `workspace_id` → index, `repo_storage_id` → index, `status` → index (key: `"outcome:<status>"`)

---

#### `(l *Ledger) Query(req *pb.QueryLedgerRequest) *pb.QueryLedgerResponse`

```go
func (l *Ledger) Query(req *pb.QueryLedgerRequest) *pb.QueryLedgerResponse
```

**What it does:** Queries the ledger with the given request filters. Supports filtering by `ResultKind` (all, commits-only, push-outcomes-only, refresh-events-only) and various field filters (workspace, principal, repo, branch, status, time range). Applies pagination via `PageSize`.

**Callers:** Called by the router's gRPC handler for `QueryLedger` RPC requests.

**Parameters:**
- `req` — a `*pb.QueryLedgerRequest` containing filter criteria and pagination settings

**Returns:** A `*pb.QueryLedgerResponse` with matching commits, push outcomes, and refresh events, plus a `TotalMatches` count.

**Implementation details:**
- Acquires a read lock (`RLock`)
- Determines the `ResultKind`:
  - `UNSPECIFIED`, `ALL`, or `COMMITS_ONLY` → scans `l.commits` with `matchesCommitFilters`
  - `ALL`, `PUSH_OUTCOMES_ONLY`, or `UNSPECIFIED` → scans `l.outcomes` with `matchesOutcomeFilters`
  - `ALL`, `REFRESH_EVENTS_ONLY`, or `UNSPECIFIED` → scans `l.refreshes` with `matchesRefreshFilters`
- Default page size is 50 if `PageSize <= 0`
- If total results exceed `pageSize`, each result slice is truncated using `limitSlice`
- Currently performs linear scans over all records; the secondary indexes (`byWorkspace`, etc.) are maintained but not yet used for accelerated queries

---

#### `matchesCommitFilters(req *pb.QueryLedgerRequest, c *pb.LocalCommit) bool`

```go
func matchesCommitFilters(req *pb.QueryLedgerRequest, c *pb.LocalCommit) bool
```

**What it does:** Checks whether a local commit matches the filter criteria in the query request.

**Callers:** Called by `Query` during linear scan of `l.commits`.

**Parameters:**
- `req` — the query request with filter fields
- `c` — the commit being evaluated

**Returns:** `true` if the commit passes all non-empty filter criteria.

**Filters applied (all are optional; empty/zero values are ignored):**
- `WorkspaceId` — exact match
- `PrincipalId` — exact match
- `RepoStorageId` — exact match
- `LocalCommitId` — exact match
- `CreatedAfter` — commit timestamp must be >= this value
- `CreatedBefore` — commit timestamp must be <= this value

---

#### `matchesOutcomeFilters(req *pb.QueryLedgerRequest, o *pb.PushOutcome) bool`

```go
func matchesOutcomeFilters(req *pb.QueryLedgerRequest, o *pb.PushOutcome) bool
```

**What it does:** Checks whether a push outcome matches the filter criteria in the query request.

**Callers:** Called by `Query` during linear scan of `l.outcomes`.

**Parameters:**
- `req` — the query request with filter fields
- `o` — the push outcome being evaluated

**Returns:** `true` if the outcome passes all non-empty filter criteria.

**Filters applied:**
- `WorkspaceId`, `JobId`, `RepoStorageId`, `PushStatus`, `Branch` — exact matches
- `CreatedAfter`, `CreatedBefore` — timestamp range

---

#### `matchesRefreshFilters(req *pb.QueryLedgerRequest, r *pb.RefreshEvent) bool`

```go
func matchesRefreshFilters(req *pb.QueryLedgerRequest, r *pb.RefreshEvent) bool
```

**What it does:** Checks whether a refresh event matches the filter criteria.

**Callers:** Called by `Query` during linear scan of `l.refreshes`.

**Parameters:**
- `req` — the query request with filter fields
- `r` — the refresh event being evaluated

**Returns:** `true` if the event passes all non-empty filter criteria.

**Filters applied:**
- `WorkspaceId`, `RepoStorageId` — exact matches
- `CreatedAfter`, `CreatedBefore` — timestamp range

---

#### `limitSlice[T any](s []T, limit int) []T`

```go
func limitSlice[T any](s []T, limit int) []T
```

**What it does:** Truncates a slice to at most `limit` elements.

**Callers:** Called by `Query` to enforce pagination on result slices.

**Parameters:**
- `s` — the slice to truncate
- `limit` — the maximum number of elements to keep

**Returns:** The original slice if `len(s) <= limit`; otherwise `s[:limit]`.

**Implementation details:** Generic function (Go 1.18+ type parameters). Used three times in `Query` — once for each result slice.

---

#### `sortByTimestamp[T interface{ GetTimestampUnix() int64 }](s []T)`

```go
func sortByTimestamp[T interface{ GetTimestampUnix() int64 }](s []T)
```

**What it does:** Sorts a slice in-place by descending `GetTimestampUnix()` value (most recent first).

**Callers:** **Currently unused** — defined but never called in the codebase. Likely intended for future use or left over from a refactor.

**Parameters:**
- `s` — the slice to sort (must contain types with a `GetTimestampUnix() int64` method)

**Implementation details:** Generic function using `sort.Slice`. Sorts in descending order (newest first).

---

#### `mustMarshal(v interface{}) []byte`

```go
func mustMarshal(v interface{}) []byte
```

**What it does:** Marshals a value to JSON, ignoring errors. Used only for internal protobuf types where marshaling is expected to always succeed.

**Callers:** Called by `InsertCommit`, `InsertPushOutcome`, and `InsertRefreshEvent` to serialize protobuf messages into `ledgerRecord.Data`.

**Parameters:**
- `v` — any value to marshal (always a protobuf message in practice)

**Returns:** JSON bytes; empty `nil` slice on error (which is silently discarded).

**Implementation details:** Wraps `json.Marshal`, discarding the error. This is safe because protobuf messages with only scalar/string fields never fail to marshal. Panics on error are avoided to prevent crashing the server from a WAL write failure.

---

## workspacepolicy/policy.go

**Package:** `workspacepolicy` — provides a YAML-based policy engine for controlling workspace operations (REFRESH, PUBLISH, SOURCE_PUSH). Policies are rule-based with glob matching and allow/deny effects, evaluated in first-match-wins order with a configurable default.

**Imports:** `fmt`, `os`, `path/filepath`, `strings`, `gopkg.in/yaml.v3`

### Constants

| Constant                          | Value                    | Usage |
|-----------------------------------|--------------------------|-------|
| `EffectAllow`                     | `"allow"`                | Policy effect: permit the action |
| `EffectDeny`                      | `"deny"`                 | Policy effect: forbid the action |
| `ActionRefresh`                   | `"REFRESH"`              | Action: refresh workspace from remote |
| `ActionPublish`                    | `"PUBLISH"`              | Action: publish workspace changes |
| `ActionSourcePush`                | `"SOURCE_PUSH"`          | Action: push source changes (direct commit) |
| `PushModeSquash`                  | `"squash"`               | Push mode: squash commits |
| `PushModePreserve`                | `"preserve"`             | Push mode: preserve individual commits |
| `BranchStrategyDirect`            | `"direct"`               | Branch: push directly to target branch |
| `BranchStrategyWorkspace`         | `"workspace_branch"`     | Branch: push to a workspace-specific branch |
| `BranchStrategyPerRepoBranch`     | `"per_repo_branch"`      | Branch: push to a per-repository branch |
| `ReasonPolicyDenied`              | `"POLICY_DENIED"`        | Generic deny reason |
| `ReasonPolicyProtectedBranch`     | `"POLICY_PROTECTED_BRANCH"` | Branch is protected |
| `ReasonPolicyProtectedWorkspace`  | `"POLICY_PROTECTED_WORKSPACE"` | Workspace is protected |
| `ReasonPolicyModeRestricted`      | `"POLICY_MODE_RESTRICTED"` | Push mode not allowed |
| `ReasonPolicyStrategyRestricted`  | `"POLICY_STRATEGY_RESTRICTED"` | Branch strategy not allowed |
| `ReasonPolicyRepoRestricted`      | `"POLICY_REPO_RESTRICTED"` | Repository not allowed |
| `ReasonPolicyPrincipalNotAuth`    | `"POLICY_PRINCIPAL_NOT_AUTHORIZED"` | Principal not authorized |
| `ReasonPolicyDefaultDeny`         | `"POLICY_DEFAULT_DENY"` | Default deny (no matching rule) |
| `ReasonPolicyAllowed`             | `"POLICY_ALLOWED"`       | Policy allowed the action |
| `ReasonPolicyDefaultAllow`        | `"POLICY_DEFAULT_ALLOW"` | Default allow (no matching rule) |
| `ReasonPolicyRequiresMergeRequest`| `"POLICY_REQUIRES_MERGE_REQUEST"` | Action requires a merge request |

### Types

#### Struct `PolicyConfig`

```go
type PolicyConfig struct {
    Version int    `yaml:"version"`
    Default string `yaml:"default"`
    Rules   []Rule `yaml:"rules"`
}
```

Top-level policy configuration loaded from YAML.

- `Version` — must be `1` (the only supported version)
- `Default` — the default effect when no rule matches; must be `"allow"` or `"deny"`
- `Rules` — ordered list of rules evaluated first-match-wins

#### Struct `Rule`

```go
type Rule struct {
    Name     string   `yaml:"name"`
    Match    MatchSet `yaml:"match"`
    MatchNot MatchSet `yaml:"match_not"`
    Effect   string   `yaml:"effect"`
    Reason   string   `yaml:"reason"`
}
```

A single policy rule.

- `Name` — human-readable rule name (required)
- `Match` — conditions that must ALL be satisfied for the rule to apply
- `MatchNot` — conditions that must ALL NOT be satisfied (negation). If a `MatchNot` field matches, the rule is skipped.
- `Effect` — `"allow"` or `"deny"`
- `Reason` — optional reason string for audit/display; if empty, a reason code is derived from the rule name

#### Struct `MatchSet`

```go
type MatchSet struct {
    PrincipalIDs     []string `yaml:"principal_ids"`
    WorkspaceIDs     []string `yaml:"workspace_ids"`
    LogicalBranches  []string `yaml:"logical_branches"`
    RepositoryIDs    []string `yaml:"repository_ids"`
    Actions          []string `yaml:"actions"`
    PushModes        []string `yaml:"push_modes"`
    BranchStrategies []string `yaml:"branch_strategies"`
}
```

A set of match conditions. An empty MatchSet (all fields are zero-length) matches **everything**.

- `PrincipalIDs`, `WorkspaceIDs`, `LogicalBranches`, `RepositoryIDs` — matched using glob patterns (via `filepath.Match`)
- `Actions`, `PushModes`, `BranchStrategies` — matched using exact string comparison

#### Struct `EvaluationRequest`

```go
type EvaluationRequest struct {
    PrincipalID    string
    WorkspaceID    string
    LogicalBranch  string
    RepositoryIDs  []string
    Action         string
    PushMode       string
    BranchStrategy string
}
```

Input to the policy evaluation engine. Represents the operation being checked.

#### Struct `EvaluationResult`

```go
type EvaluationResult struct {
    Effect     string
    ReasonCode string
    Reason     string
    RuleName   string
}
```

Output of policy evaluation.

- `Effect` — `"allow"` or `"deny"`
- `ReasonCode` — machine-readable reason code (e.g., `"POLICY_DENIED"`)
- `Reason` — human-readable reason string
- `RuleName` — name of the matching rule (empty if default was applied)

### Package-level variables

```go
var validActions = map[string]bool{
    ActionRefresh: true, ActionPublish: true, ActionSourcePush: true,
}
var validPushModes = map[string]bool{PushModeSquash: true, PushModePreserve: true}
var validBranchStrategies = map[string]bool{
    BranchStrategyDirect: true, BranchStrategyWorkspace: true, BranchStrategyPerRepoBranch: true,
}
var validEffects = map[string]bool{EffectAllow: true, EffectDeny: true}
```

Validation lookup tables used by `Parse` to reject unknown values during configuration loading.

---

### Functions

#### `Load(path string) (*PolicyConfig, error)`

```go
func Load(path string) (*PolicyConfig, error)
```

**What it does:** Reads a policy YAML file from disk and parses it into a `PolicyConfig`.

**Callers:** Called by `internal/router/router.go:438` during router initialization when `cfg.PolicyConfigPath` is set.

**Parameters:**
- `path` — filesystem path to the policy YAML file

**Returns:** A parsed and validated `*PolicyConfig`, or an error.

**Error conditions:**
- `"policy config path is empty"` — path is blank
- `"read policy config <path>: ..."` — file read error
- Any validation errors from `Parse`

**Implementation details:**
- Delegates to `os.ReadFile` then `Parse`
- Trims whitespace from path before checking emptiness

---

#### `Parse(data []byte) (*PolicyConfig, error)`

```go
func Parse(data []byte) (*PolicyConfig, error)
```

**What it does:** Parses raw YAML bytes into a `PolicyConfig` and validates all fields.

**Callers:** Called by `Load` and potentially by tests or inline configurations.

**Parameters:**
- `data` — raw YAML bytes

**Returns:** A validated `*PolicyConfig`, or an error.

**Validation performed:**
1. YAML syntax — via `yaml.Unmarshal`
2. `Version` — must be exactly `1`
3. `Default` — must be `"allow"` or `"deny"`
4. Each rule:
   - `Name` must be non-empty
   - `Effect` must be `"allow"` or `"deny"`
   - All `Match.Actions` must be valid actions (`REFRESH`, `PUBLISH`, `SOURCE_PUSH`)
   - All `Match.PushModes` must be valid (`squash`, `preserve`)
   - All `Match.BranchStrategies` must be valid (`direct`, `workspace_branch`, `per_repo_branch`)
   - Same validation for `MatchNot` fields

**Implementation details:**
- Validates all `Match` and `MatchNot` fields separately
- Each invalid field produces a descriptive error including the rule name

---

#### `Evaluate(cfg *PolicyConfig, req *EvaluationRequest) *EvaluationResult`

```go
func Evaluate(cfg *PolicyConfig, req *EvaluationRequest) *EvaluationResult
```

**What it does:** Evaluates a policy request against a policy configuration using first-match-wins semantics.

**Callers:** Called by `internal/router/router.go:1825` via the router's internal `evaluatePolicy` helper.

**Parameters:**
- `cfg` — the policy configuration to evaluate against
- `req` — the evaluation request describing the operation

**Returns:** An `*EvaluationResult` with the decided effect and reason.

**Evaluation algorithm:**
1. If `cfg` or `req` is nil, returns `EffectDeny` with `ReasonPolicyDenied`
2. Iterates through `cfg.Rules` in order (first-match-wins):
   - Checks if `Match` conditions are satisfied via `matchesRule`
   - If `MatchNot` is non-empty, checks if `MatchNot` conditions are satisfied; if so, the rule is skipped (negation)
   - On match, returns the rule's `Effect` and `Reason` (or auto-derived reason code from rule name)
3. If no rule matches, returns the `Default` effect:
   - `EffectDeny` → `ReasonPolicyDefaultDeny`
   - `EffectAllow` → `ReasonPolicyDefaultAllow`

**Implementation details:**
- Reason code is auto-derived from the rule name by uppercasing and replacing spaces/hyphens with underscores when rule's `Reason` is empty
- Never returns nil

---

#### `matchesRule(ms *MatchSet, req *EvaluationRequest) bool`

```go
func matchesRule(ms *MatchSet, req *EvaluationRequest) bool
```

**What it does:** Checks whether a `MatchSet` matches an evaluation request. All non-empty fields in the MatchSet must match for the overall result to be true.

**Callers:** Called by `Evaluate` for both `Match` and `MatchNot` checks.

**Parameters:**
- `ms` — the match set to check (may be nil)
- `req` — the evaluation request

**Returns:** `true` if all non-empty MatchSet fields match the request.

**Implementation details:**
- If `ms` is nil, returns `false`
- If `ms.isEmpty()`, returns `true` (empty MatchSet matches everything)
- Checks each field in order, short-circuiting on first failure:
  - `PrincipalIDs`, `WorkspaceIDs`, `LogicalBranches` — glob match against single string value
  - `RepositoryIDs` — glob match against any of the repository IDs (list vs list)
  - `Actions`, `PushModes`, `BranchStrategies` — exact match against single string value

---

#### `(ms *MatchSet) isEmpty() bool`

```go
func (ms *MatchSet) isEmpty() bool
```

**What it does:** Reports whether all fields in the MatchSet are empty (zero-length).

**Callers:** Called by `matchesRule` and `Evaluate` (for `MatchNot` emptiness check).

**Returns:** `true` if every slice field in the MatchSet has length 0.

**Implementation details:** Checks all seven slice fields independently. An empty MatchSet matches all requests (catch-all behavior).

---

#### `matchStringGlob(patterns []string, value string) bool`

```go
func matchStringGlob(patterns []string, value string) bool
```

**What it does:** Tests whether a single string value matches any of the given glob patterns.

**Callers:** Called by `matchesRule` for `PrincipalIDs`, `WorkspaceIDs`, and `LogicalBranches` fields.

**Parameters:**
- `patterns` — glob patterns to test against (e.g., `"*-admin"`, `"*"`)
- `value` — the string value to test

**Returns:** `true` if the value matches at least one pattern OR if patterns is empty (match-all); `false` otherwise.

**Implementation details:**
- Uses `filepath.Match` for glob matching (supports `*`, `?`, `[...]`)
- An empty patterns list is treated as "match everything" (no filter applied)

---

#### `matchAnyStringGlob(patterns []string, values []string) bool`

```go
func matchAnyStringGlob(patterns []string, values []string) bool
```

**What it does:** Tests whether any value in a string slice matches any glob pattern. This is a many-to-many match: if ANY value matches ANY pattern, it returns true.

**Callers:** Called by `matchesRule` for the `RepositoryIDs` field (list of repo IDs against list of glob patterns).

**Parameters:**
- `patterns` — glob patterns to test against
- `values` — string values to test

**Returns:** `true` if any value matches any pattern OR if patterns is empty; `false` otherwise.

**Implementation details:**
- O(n*m) nested loop over values and patterns
- An empty patterns list is treated as "match everything"

---

#### `matchStringExact(patterns []string, value string) bool`

```go
func matchStringExact(patterns []string, value string) bool
```

**What it does:** Tests whether a single string value matches any pattern by exact equality (no glob).

**Callers:** Called by `matchesRule` for `Actions`, `PushModes`, and `BranchStrategies` fields.

**Parameters:**
- `patterns` — exact strings to match against
- `value` — the value to test

**Returns:** `true` if value matches any pattern OR if patterns is empty; `false` otherwise.

**Implementation details:**
- Simple `==` comparison (not glob), because actions/modes/strategies are finite well-known constants
- An empty patterns list is treated as "match everything"

---

## workspacepr/provider.go

**Package:** `workspacepr` — detects hosting providers (GitHub, GitLab) from repository clone URLs and generates comparison/PR creation URLs. Used to bridge MonoFS workspaces with external PR workflows.

**Imports:** `context`, `fmt`, `net/url`, `strings`

### Types

#### Interface `PullRequestProvider`

```go
type PullRequestProvider interface {
    Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error)
    ProviderName() string
}
```

Abstracts a PR hosting provider. Implementations generate a web URL for creating a pull request (no actual API calls are made — only URL construction).

- `Create` — builds a PR creation URL from the given request
- `ProviderName` — returns a human-readable provider identifier (`"github"` or `"gitlab"`)

#### Struct `CreatePRRequest`

```go
type CreatePRRequest struct {
    RepoCloneURL string
    SourceBranch string
    TargetBranch string
    Title        string
    Body         string
}
```

Input for PR creation.

- `RepoCloneURL` — the git clone URL of the repository (used to detect the provider and parse owner/repo)
- `SourceBranch` — the branch containing the changes
- `TargetBranch` — the target base branch
- `Title`, `Body` — PR title and description (currently unused in URL construction but available for future API-based implementations)

#### Struct `CreatePRResult`

```go
type CreatePRResult struct {
    WebURL  string
    ID      string
    Created bool
}
```

Output of PR creation.

- `WebURL` — the URL users can open in a browser to create or view the PR
- `ID` — provider-specific PR identifier (empty for URL-only implementations)
- `Created` — always `false` for current URL-only implementations (indicates the PR was not auto-created)

#### Struct `GitHubProvider`

```go
type GitHubProvider struct{}
```

Implements `PullRequestProvider` for GitHub. Empty struct — no configuration needed.

#### Struct `GitLabProvider`

```go
type GitLabProvider struct {
    BaseURL string
}
```

Implements `PullRequestProvider` for GitLab. `BaseURL` can be customized for self-hosted GitLab instances; defaults to `"https://gitlab.com"`.

---

### Functions

#### `DetectProvider(repoCloneURL string, gitLabBaseURL string) (PullRequestProvider, error)`

```go
func DetectProvider(repoCloneURL string, gitLabBaseURL string) (PullRequestProvider, error)
```

**What it does:** Detects the PR provider from a repository clone URL and returns the appropriate provider implementation.

**Callers:** Called by the router when determining which provider to use for creating a PR for a workspace push operation.

**Parameters:**
- `repoCloneURL` — the repository clone URL to parse (e.g., `"https://github.com/owner/repo.git"` or `"git@github.com:owner/repo.git"`)
- `gitLabBaseURL` — optional base URL for self-hosted GitLab (used for host matching)

**Returns:** A `PullRequestProvider` implementation (`*GitHubProvider` or `*GitLabProvider`), or an error.

**Error conditions:**
- `"cannot parse host from repo URL: ..."` — host could not be extracted
- `"unknown provider for host: ..."` — host is not recognized as GitHub or GitLab

**Detection logic:**
- Parses the host from the clone URL using `parseHost`
- `"github.com"` → `*GitHubProvider{}`
- `"gitlab.com"` OR host matches the provided `gitLabBaseURL` → `*GitLabProvider{BaseURL: gitLabBaseURL}`
- Anything else → error

---

#### `CompareURL(repoCloneURL, sourceBranch, targetBranch string) string`

```go
func CompareURL(repoCloneURL, sourceBranch, targetBranch string) string
```

**What it does:** Generates a comparison/PR creation URL for the given repository and branches, suitable for opening in a browser.

**Callers:** Called by the router to generate a link users can click to compare branches or create a PR on the hosting provider.

**Parameters:**
- `repoCloneURL` — repository clone URL
- `sourceBranch` — the source branch (URL-encoded in the output)
- `targetBranch` — the target/base branch

**Returns:** A URL string for the comparison/PR page on the detected provider.

**URL formats by provider:**
- **GitHub:** `https://github.com/{owner}/{repo}/compare/{target}...{source}`
- **GitLab / self-hosted GitLab:** `https://{host}/{owner}/{repo}/-/merge_requests/new?merge_request[source_branch]={source}&merge_request[target_branch]={target}`
- **Unknown host:** a human-readable string `"Create PR: {source} → {target} on {cloneURL}"`

**Implementation details:**
- Uses `url.PathEscape` for GitHub branch names and `url.QueryEscape` for GitLab query parameters
- Works with both HTTPS and SSH-style clone URLs

---

#### `parseHost(cloneURL string) string`

```go
func parseHost(cloneURL string) string
```

**What it does:** Extracts the hostname from a git clone URL, handling both HTTPS and SSH formats.

**Callers:** Called by `DetectProvider` and `CompareURL`.

**Parameters:**
- `cloneURL` — a git clone URL (HTTPS or SSH format)

**Returns:** The hostname string, or empty string if parsing fails.

**Supported formats:**
- SSH: `"git@github.com:owner/repo.git"` → `"github.com"` (extracted by splitting `git@` prefix and `:` separator)
- HTTPS: `"https://github.com/owner/repo.git"` → `"github.com"` (parsed via `net/url.Parse`)

**Implementation details:**
- SSH URLs are detected by the `"git@"` prefix; the part between `@` and `:` is the host
- HTTPS URLs use `net/url.Parse` and return `parsed.Host`
- Returns empty string if neither format is recognized

---

#### `parseOwnerRepo(cloneURL string) (string, string)`

```go
func parseOwnerRepo(cloneURL string) (string, string)
```

**What it does:** Extracts the owner (org/user) and repository name from a git clone URL.

**Callers:** Called by `CompareURL`, `GitHubProvider.Create`, and `GitLabProvider.Create`.

**Parameters:**
- `cloneURL` — a git clone URL

**Returns:** Two strings: `(owner, repo)`. If only one path segment is found, returns `("", path)`. If no path is found, returns `("", "")`.

**Implementation details:**
- For SSH URLs (`git@...`), extracts the path after `:`, strips `.git` suffix, then splits by `/`
- For HTTPS URLs, parses with `net/url.Parse`, extracts `Path`, strips leading `/` and `.git` suffix, then splits by `/`
- Returns up to two path segments; extra segments are discarded

---

#### `(p *GitHubProvider) Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error)`

```go
func (p *GitHubProvider) Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error)
```

**What it does:** Generates a GitHub PR creation URL. Does not make any API calls — returns the web URL for manual PR creation.

**Callers:** Called via the `PullRequestProvider` interface by the router when a GitHub-hosted repository needs a PR link.

**Parameters:**
- `ctx` — context (unused but required by the interface)
- `req` — PR creation request with clone URL, source/target branches, title, and body

**Returns:** A `*CreatePRResult` with the web URL and `Created: false`, or an error if owner/repo cannot be parsed.

**Generated URL format:** `https://github.com/{owner}/{repo}/pull/new/{target}...{source}`

**Error conditions:**
- `"cannot parse owner/repo from ..."` — the clone URL could not be parsed

---

#### `(p *GitHubProvider) ProviderName() string`

```go
func (p *GitHubProvider) ProviderName() string
```

**What it does:** Returns the provider identifier.

**Callers:** Called via the `PullRequestProvider` interface.

**Returns:** `"github"`

---

#### `(p *GitLabProvider) Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error)`

```go
func (p *GitLabProvider) Create(ctx context.Context, req CreatePRRequest) (*CreatePRResult, error)
```

**What it does:** Generates a GitLab merge request creation URL. Does not make any API calls. Supports self-hosted GitLab via `BaseURL`.

**Callers:** Called via the `PullRequestProvider` interface by the router when a GitLab-hosted repository needs a PR link.

**Parameters:**
- `ctx` — context (unused)
- `req` — PR creation request

**Returns:** A `*CreatePRResult` with the web URL and `Created: false`, or an error.

**Generated URL format:** `{baseURL}/{owner}/{repo}/-/merge_requests/new?merge_request[source_branch]={source}&merge_request[target_branch]={target}`

**Implementation details:**
- If `p.BaseURL` is empty, defaults to `"https://gitlab.com"`
- Source and target branches are URL-query-escaped

**Error conditions:**
- `"cannot parse owner/repo from ..."` — clone URL parse failure

---

#### `(p *GitLabProvider) ProviderName() string`

```go
func (p *GitLabProvider) ProviderName() string
```

**What it does:** Returns the provider identifier.

**Callers:** Called via the `PullRequestProvider` interface.

**Returns:** `"gitlab"`
