# Authz Router Documentation

This document covers the authorization (authz) functions in `internal/router/`, implementing partition-based read enforcement, subtree-ownership merge gates, external merge request creation, and the authz UI API handlers.

---

## File: `authz_merge_external.go`

Implements external merge/pull request creation for non-owner changes (authz epic D2), bridging the subtree-ownership gate to GitHub/GitLab providers.

### Struct: `externalMergeRequest`

```go
type externalMergeRequest struct {
    RepoCloneURL string
    SourceBranch string
    TargetBranch string
    Title        string
    UnownedPaths []string
    Reviewers    []string
}
```

Describes a pull/merge request to be opened on an external git provider for a non-owner change. Used as the internal data model before conversion to the provider-agnostic `CreatePRRequest`.

---

### Function: `reviewersFromOwnerRefs`

```go
func reviewersFromOwnerRefs(refs []authz.OwnerRef) []string
```

**Purpose:** Converts a slice of `authz.OwnerRef` values into a slice of human-readable reviewer strings.

**How it works:** Iterates over each `OwnerRef`, calling its `String()` method to produce a reviewer identifier string (e.g., `"team:platform-eng"` or `"alice@example.com"`).

**Parameters:**
- `refs` — list of owner references (teams or individual subjects) from the ownership resolution.

**Returns:** A `[]string` of reviewer identifiers, one per `OwnerRef`.

**Callers:** Called from test code (`authz_merge_external_test.go:30`). Intended to be called by the merge-request routing logic (D2/D3 flow) to resolve reviewers from the unowned subtree owners.

---

### Function: `buildPRRequest`

```go
func buildPRRequest(mr externalMergeRequest) workspacepr.CreatePRRequest
```

**Purpose:** Renders an `externalMergeRequest` into a provider-agnostic `workspacepr.CreatePRRequest`, formatting the PR body with the unowned paths and requested reviewers.

**How it works:**
1. Builds a body string explaining this is an automated merge request requiring owner review.
2. If `UnownedPaths` is non-empty, appends a "Paths requiring owner review:" section, one path per line.
3. If `Reviewers` is non-empty, appends a "Requested reviewers:" line with comma-separated reviewer identifiers.
4. Sets the title from `mr.Title`; if empty, defaults to `"Merge request: owner review required"`.

**Parameters:**
- `mr` — the internal merge request descriptor.

**Returns:** A `workspacepr.CreatePRRequest` ready to be passed to a `PullRequestProvider`.

**Callers:**
- `openExternalMergeRequest` (`authz_merge_external.go:65`)
- Test code (`authz_merge_external_test.go:32`, `authz_merge_external_test.go:45`)

---

### Function: `openExternalMergeRequest`

```go
func openExternalMergeRequest(ctx context.Context, provider workspacepr.PullRequestProvider, mr externalMergeRequest) (*workspacepr.CreatePRResult, error)
```

**Purpose:** Opens a pull/merge request on an external git provider using the given `PullRequestProvider`.

**How it works:**
1. Checks if `provider` is nil — returns an error if so, since there's no backend to create the PR.
2. Calls `buildPRRequest(mr)` to convert the internal model.
3. Calls `provider.Create(ctx, req)` to actually create the PR on the external provider.

**Parameters:**
- `ctx` — request context.
- `provider` — the `PullRequestProvider` implementation (e.g., GitHub, GitLab backend). Must not be nil.
- `mr` — the internal merge request details.

**Returns:**
- `*workspacepr.CreatePRResult` — result of the PR creation (contains URL, PR number, etc.).
- `error` — nil if successful; non-nil if no provider is configured or if the provider fails.

**Callers:** Currently only from test code (`authz_merge_external_test.go:53`, `authz_merge_external_test.go:69`). Designed to be wired into the D2/D3 merge-request routing after the subtree-ownership gate blocks a direct push.

---

## File: `authz_merge_gate.go`

Implements the subtree-ownership gate (authz epic D) that decides whether a caller may push changes directly or must go through a merge request.

### Type: `MergeDecision`

```go
type MergeDecision int
```

An enum representing the outcome of the subtree-ownership gate.

#### Constants

| Constant | Value | Meaning |
|---|---|---|
| `MergeDecisionDirect` | `0` (iota) | Caller owns all affected subtrees — direct write allowed. |
| `MergeDecisionMergeRequest` | `1` | Caller does not own one or more affected subtrees — must route through a merge request. |

---

### Method: `MergeDecision.String`

```go
func (d MergeDecision) String() string
```

**Purpose:** Renders the decision as a human-readable string for logs and events.

**Returns:**
- `"direct"` for `MergeDecisionDirect`
- `"merge_request"` for `MergeDecisionMergeRequest`
- `"unknown"` for any unrecognized value

**Callers:** Used in log messages and test assertions.

---

### Interface: `ownershipChecker`

```go
type ownershipChecker interface {
    OwnsAll(ctx context.Context, paths []string, id authz.Identity) (ownsAll bool, unowned []string, err error)
}
```

**Purpose:** Abstracts `*authz.OwnershipResolver` so the gate can be tested with a fake implementation. Passing `nil` to the router disables the gate.

**Method:**
- `OwnsAll(ctx, paths, id)` — checks whether the given identity owns all the provided paths. Returns `ownsAll=true` if the identity owns every path, otherwise returns the `unowned` subset and `ownsAll=false`.

---

### Method: `Router.SetOwnershipResolver`

```go
func (r *Router) SetOwnershipResolver(checker ownershipChecker)
```

**Purpose:** Installs the subtree-ownership resolver used by the merge-request gate. Passing `nil` disables the gate (all changes are treated as direct).

**Parameters:**
- `checker` — any implementation of `ownershipChecker`, typically `*authz.OwnershipResolver`.

**Callers:** Wiring/setup code during router initialization or tests.

---

### Method: `Router.evaluateSubtreeOwnership`

```go
func (r *Router) evaluateSubtreeOwnership(ctx context.Context, paths []string) (MergeDecision, []string, error)
```

**Purpose:** Decides whether the identity in the context may modify the given paths directly or must open a merge request. This is the core of the merge-request gate (authz epic D).

**How it works:**
1. If no `ownershipResolver` is configured or `paths` is empty, returns `MergeDecisionDirect` (no gate active or nothing to check).
2. Extracts the caller's identity from context via `authz.IdentityFromContext(ctx)`. If no identity is present, the zero-value identity is used (may fail ownership check).
3. Calls `r.ownershipResolver.OwnsAll(ctx, paths, id)`.
4. If the resolver returns an error, defaults to `MergeDecisionDirect` (fail-open) while propagating the error for logging.
5. If `ownsAll` is true, returns `MergeDecisionDirect`.
6. Otherwise, returns `MergeDecisionMergeRequest` along with the `unowned` paths.

**Parameters:**
- `ctx` — request context, must contain the caller's identity (set by the auth interceptor).
- `paths` — list of absolute display paths being modified.

**Returns:**
- `MergeDecision` — the gate decision.
- `[]string` — the subset of paths the caller does **not** own (empty when direct).
- `error` — non-nil if the ownership check itself failed (errors cause fail-open to `MergeDecisionDirect`).

**Callers:** `workspace_source_push.go:125` — called during source-push processing to gate direct pushes against subtree ownership.

---

### Function: `changedPathsFromSourceBundle`

```go
func changedPathsFromSourceBundle(bundle *workspacebundle.SourceCommitBundle) []string
```

**Purpose:** Collects the distinct absolute display paths touched by a source commit bundle. Used to determine which subtrees are affected for the subtree-ownership gate.

**How it works:**
1. Handles nil bundle by returning nil.
2. Iterates over all commits, their repositories, and each operation within each repository.
3. For each operation, constructs the full display path as `base + "/" + op.Path` (with proper trimming of leading/trailing slashes).
4. Deduplicates paths using a `map[string]bool`.
5. Returns the unique sorted-natural-order slice of paths (note: iteration order over maps is non-deterministic in Go, but the test suite handles this).

**Parameters:**
- `bundle` — the source commit bundle containing changed files, or nil.

**Returns:** A `[]string` of distinct absolute display paths, or nil if bundle is nil.

**Callers:** `workspace_source_push.go:126` — feeds the paths into `evaluateSubtreeOwnership`.

---

## File: `authz_read.go`

Implements partition-scoped read/mount viewer enforcement (authz epic C2).

### Method: `Router.authorizeRead`

```go
func (r *Router) authorizeRead(ctx context.Context, partition string) error
```

**Purpose:** Enforces the viewer role on read/mount access to a partition. It is a no-op unless `authzEnforceRead` is set to `true` and a grant evaluator is present. Anonymous callers are denied when enforcement is on.

**How it works:**
1. If `authzEnforceRead` is disabled or `grantEvaluator` is nil, returns nil immediately (no-op).
2. Reads the caller's identity from context via `authz.IdentityFromContext(ctx)`.
3. Calls `r.grantEvaluator.Can(ctx, id, partition, authz.ActionView)` to check if the identity has viewer permission.
4. If permitted, returns nil.
5. If denied, logs a warning and returns a gRPC `PermissionDenied` status error with the principal and partition names.

**Parameters:**
- `ctx` — request context containing the caller's identity (set by the auth interceptor).
- `partition` — the partition name being read or mounted.

**Returns:**
- `nil` — access is allowed (or enforcement is disabled).
- `error` — a gRPC `codes.PermissionDenied` error if the principal lacks viewer privileges.

**Callers:** Currently only from test code (`authz_read_test.go`, `authz_breakglass_test.go`). Designed to be wired into read/mount RPC handlers.

---

### Method: `Router.SetAuthzEnforceRead`

```go
func (r *Router) SetAuthzEnforceRead(enforce bool)
```

**Purpose:** Toggles read/mount viewer enforcement at runtime. Used by tests and wiring.

**Parameters:**
- `enforce` — `true` to enable partition-level read authorization, `false` to disable.

**Callers:** Tests and initialization wiring in `router.go`.

---

## File: `authz_ui.go`

Implements HTTP API handlers for toggling and querying the ingest authorization enforcement state from the admin UI.

### Method: `Router.handleAuthzIngestToggleAPI`

```go
func (r *Router) handleAuthzIngestToggleAPI(w http.ResponseWriter, req *http.Request)
```

**Purpose:** HTTP POST handler at `/api/authz/ingest/toggle` that enables or disables partition ingest authorization enforcement at runtime.

**How it works:**
1. Rejects non-POST methods with `405 Method Not Allowed`.
2. Decodes a JSON body of the form `{"enforce": bool}`.
3. On decode failure, returns `400 Bad Request` with `{"success": false, "message": "invalid request body"}`.
4. Calls `r.SetAuthzEnforceIngest(body.Enforce)` to toggle the state (defined in `ingest.go:113`).
5. Logs the toggle event.
6. Returns `200 OK` with `{"success": true, "message": "partition ingest authorization <enabled|disabled>"}`.

**Parameters:**
- `w` — HTTP response writer.
- `req` — HTTP request; body must be JSON with an `enforce` boolean field.

**Registered at:** `ui.go:154` as `mux.HandleFunc("/api/authz/ingest/toggle", r.handleAuthzIngestToggleAPI)`.

---

### Method: `Router.handleAuthzIngestStatusAPI`

```go
func (r *Router) handleAuthzIngestStatusAPI(w http.ResponseWriter, req *http.Request)
```

**Purpose:** HTTP GET handler at `/api/authz/ingest` that returns the current ingest authorization enforcement state.

**How it works:**
1. Rejects non-GET methods with `405 Method Not Allowed`.
2. Returns a JSON response `{"enforce": bool}` where the value comes from `r.AuthzEnforceIngest()` (defined in `ingest.go:118`).

**Parameters:**
- `w` — HTTP response writer.
- `req` — HTTP request (ignored beyond method check).

**Registered at:** `ui.go:153` as `mux.HandleFunc("/api/authz/ingest", r.handleAuthzIngestStatusAPI)`.

---

## Cross-File Summary Table

| Function/Method | File | Exported | Signature |
|---|---|---|---|
| `reviewersFromOwnerRefs` | `authz_merge_external.go` | No | `func(refs []authz.OwnerRef) []string` |
| `buildPRRequest` | `authz_merge_external.go` | No | `func(mr externalMergeRequest) workspacepr.CreatePRRequest` |
| `openExternalMergeRequest` | `authz_merge_external.go` | No | `func(ctx context.Context, provider workspacepr.PullRequestProvider, mr externalMergeRequest) (*workspacepr.CreatePRResult, error)` |
| `MergeDecision` (type) | `authz_merge_gate.go` | Yes | `type MergeDecision int` |
| `MergeDecision.String` | `authz_merge_gate.go` | Yes | `func(d MergeDecision) String() string` |
| `ownershipChecker` (interface) | `authz_merge_gate.go` | No | `interface { OwnsAll(...) }` |
| `Router.SetOwnershipResolver` | `authz_merge_gate.go` | Yes | `func(r *Router) SetOwnershipResolver(checker ownershipChecker)` |
| `Router.evaluateSubtreeOwnership` | `authz_merge_gate.go` | No | `func(r *Router) evaluateSubtreeOwnership(ctx context.Context, paths []string) (MergeDecision, []string, error)` |
| `changedPathsFromSourceBundle` | `authz_merge_gate.go` | No | `func(bundle *workspacebundle.SourceCommitBundle) []string` |
| `Router.authorizeRead` | `authz_read.go` | No | `func(r *Router) authorizeRead(ctx context.Context, partition string) error` |
| `Router.SetAuthzEnforceRead` | `authz_read.go` | Yes | `func(r *Router) SetAuthzEnforceRead(enforce bool)` |
| `Router.handleAuthzIngestToggleAPI` | `authz_ui.go` | No | `func(r *Router) handleAuthzIngestToggleAPI(w http.ResponseWriter, req *http.Request)` |
| `Router.handleAuthzIngestStatusAPI` | `authz_ui.go` | No | `func(r *Router) handleAuthzIngestStatusAPI(w http.ResponseWriter, req *http.Request)` |

## Caller/Lifetime Summary

| Function/Method | Primary Caller(s) |
|---|---|
| `reviewersFromOwnerRefs` | Tests; future D2/D3 merge-request routing |
| `buildPRRequest` | `openExternalMergeRequest`; tests |
| `openExternalMergeRequest` | Tests; future D2/D3 merge-request routing |
| `Router.SetOwnershipResolver` | Router initialization wiring |
| `Router.evaluateSubtreeOwnership` | `workspace_source_push.go:125` (source-push handler) |
| `changedPathsFromSourceBundle` | `workspace_source_push.go:126` (source-push handler) |
| `Router.authorizeRead` | Tests; future read/mount RPC handlers |
| `Router.SetAuthzEnforceRead` | Router initialization wiring; tests |
| `Router.handleAuthzIngestToggleAPI` | HTTP handler at `/api/authz/ingest/toggle` |
| `Router.handleAuthzIngestStatusAPI` | HTTP handler at `/api/authz/ingest` |
