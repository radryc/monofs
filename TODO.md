# MonoFS Monorepo Readiness Fix Plan

This plan turns the current gap list into an implementation sequence for a fully functional monorepo platform, including controlled automatic upstream push behavior.

## Goals

1. Durable auditability of all publish/push/refresh activity.
2. Upstream Git history fidelity (no mandatory squash).
3. Policy-controlled automation for upstream push.
4. Repository-level CI/CD and governance.
5. Operationally complete release and documentation model.

---

## Phase 0 — Foundation and Safety Rails

### Step 0.1: Define rollout toggles and compatibility mode
**What to implement**
- Add explicit feature flags for:
  - durable job storage
  - non-squash upstream push mode
  - auto-push scheduler/worker
  - server-side policy gates
- Keep defaults backward-compatible (`off` by default for new behavior).

**Implementation details**
- Add router/fetcher config fields:
  - `WorkspaceJobStoreEnabled`
  - `SourcePushMode` (`squash`, `preserve`)
  - `AutoPushEnabled`
  - `PolicyGateEnabled`
- Expose flags in `cmd/monofs-router` and `cmd/monofs-fetcher`.
- Plumb values into `internal/router` and `internal/fetcher`.

**Acceptance criteria**
- Existing workflow remains unchanged when flags are off.
- New behavior can be enabled per environment without code changes.

---

## Phase 1 — Durable Job and Commit Audit Trail

### Step 1.1: Introduce persistent workspace sync job store
**What to implement**
- Replace in-memory-only job map persistence with durable storage.
- Keep in-memory cache for hot reads, but source of truth must be persistent.

**Implementation details**
- Add a job repository module (new package): `internal/router/workspacejobstore`.
- Persist `WorkspaceSyncJob` lifecycle transitions:
  - created, started, repository events, completed/failed/cancelled
- Store indexed fields:
  - `job_id`, `workspace_id`, `action`, `state`, `created_at`, `finished_at`, `requested_by_client_id`, `logical_branch`
- Add retention policy config:
  - e.g. `JobRetentionDays`, `MaxJobsPerWorkspace`.
- Update read paths:
  - `GetWorkspaceSyncJob` and `ListWorkspaceSyncJobs` read from store.
- Update UI endpoint to support pagination and filters.

**Likely touchpoints**
- `internal/router/router.go`
- `internal/router/workspace_sync.go`
- `internal/router/workspace_sync_ui.go`

**Acceptance criteria**
- Router restart does not lose job history.
- UI/API can query historical jobs beyond process lifetime.
- Pagination works for large job sets.

### Step 1.2: Persist staged bundle metadata for forensics
**What to implement**
- Persist metadata for staged bundles and source commit bundles, even if payload TTL expires.

**Implementation details**
- Persist metadata rows:
  - `bundle_id`, `workspace_id`, `created_at`, `expires_at`, `type`, `byte_size`, `repo_count`, `local_commit_ids`.
- Keep payload TTL behavior, but preserve metadata and final status linkage to jobs.
- Capture discard reason (`completed`, `expired`, `cancelled`, `error`).

**Likely touchpoints**
- `internal/router/workspace_source_push.go`
- `internal/fetcher/service.go`

**Acceptance criteria**
- Operators can audit what was staged and when, after payload deletion.

### Step 1.3: Add append-only platform audit event log
**What to implement**
- Emit structured immutable events for key workflow actions.

**Implementation details**
- Event types:
  - `workspace.commit.created`
  - `workspace.push.started|completed|failed`
  - `workspace.refresh.started|completed|failed`
  - `workspace.policy.denied`
  - `workspace.autopush.triggered`
- Include correlation ids (`job_id`, `bundle_id`, `workspace_id`, `local_commit_id`).
- Add minimal query API for ops use.

**Acceptance criteria**
- Any upstream change can be traced to actor, branch, commits, and decision path.

---

## Phase 2 — Upstream Commit Fidelity (No Mandatory Squash)

### Step 2.1: Implement preserved local commit sequence push mode
**What to implement**
- New mode in source push path that replays local virtual commits in-order upstream.

**Implementation details**
- In `preserve` mode:
  - apply each local virtual commit as one upstream commit per repo, preserving order.
  - preserve author name/email from local virtual commit metadata.
  - include local commit ID in upstream commit trailer, e.g.:
    - `MonoFS-Local-Commit: <id>`
    - `MonoFS-Workspace: <workspace-id>`
- In `squash` mode:
  - keep current behavior as fallback.
- Validate:
  - all commits belong to current logical branch before replay.
  - deterministic ordering by created time + local id tie-breaker.

**Likely touchpoints**
- `internal/fetcher/workspace_source_push.go`
- `internal/fuse/commit.go`
- `internal/workspacebundle/commit_bundle.go`

**Acceptance criteria**
- Preserved mode creates N upstream commits for N local commits.
- Squash mode remains available and explicit.

### Step 2.2: Strengthen conflict handling and partial failure semantics
**What to implement**
- Per-repository replay status, resumable failure handling.

**Implementation details**
- Track progress per repo + per local commit index.
- On conflict:
  - mark repository result as conflict with offending local commit id.
  - leave unresolved commits as pending.
- Add retry behavior:
  - retry from first unpushed local commit.

**Acceptance criteria**
- Failed replay does not corrupt commit state.
- Retry is deterministic and does not duplicate already pushed commits.

---

## Phase 3 — Policy-Gated and Automatic Upstream Push

### Step 3.1: Add server-side push policy engine
**What to implement**
- Enforce branch, actor, and repository policy before upstream mutation.

**Implementation details**
- Add policy config model:
  - allowed branches/patterns
  - branch strategy restrictions
  - required checks profile
  - allow/deny list by principal/repo path
- Evaluate policy before push begins.
- Emit explicit deny event and user-facing error details.

**Likely touchpoints**
- `internal/router/workspace_source_push.go`
- `internal/fetcher/workspace_source_push.go`
- config structs + CLI flags

**Acceptance criteria**
- Disallowed push attempts are rejected before any upstream write.
- Decision reason is visible in CLI/UI and audit log.

### Step 3.2: Add auto-push orchestrator
**What to implement**
- Background worker that can push pending local commits automatically based on policy.

**Implementation details**
- Trigger model:
  - interval scan (`AutoPushInterval`)
  - optional explicit trigger endpoint
- Selection logic:
  - workspace has pending commits
  - policy allows auto push
  - no active push job for same workspace/branch
- Concurrency controls:
  - per-workspace lock
  - global max parallel pushes
- Safety:
  - exponential backoff on repeated failure
  - dead-letter / paused state after configurable retry count

**Acceptance criteria**
- Eligible commits are pushed without manual CLI invocation.
- System avoids duplicate concurrent pushes per workspace branch.

### Step 3.3: Add auto-PR for non-direct branch strategies
**What to implement**
- Optional provider integration for PR creation and merge status tracking.

**Implementation details**
- Post-push hook for `workspace_branch` / `per_repo_branch`:
  - create PR if missing
  - annotate PR with local commit IDs and job id
- Poll/subscribe merge result:
  - mark branch mapping state as merged/rejected.
- If provider integration unavailable:
  - expose manual command output with exact compare URL.

**Acceptance criteria**
- Non-direct strategy no longer depends on purely manual PR creation.

---

## Phase 4 — Repository Governance, CI, and Deployment Pipelines

### Step 4.1: Add repository governance baseline
**What to implement**
- Add `.github/CODEOWNERS`.
- Add PR and issue templates.
- Add branch protection documentation.

**Implementation details**
- CODEOWNERS by domain:
  - router, fetcher, fuse, server, docs, ci.
- Add `.github/pull_request_template.md`.
- Add `.github/ISSUE_TEMPLATE/*`.
- Document required checks expected by branch protection.

**Acceptance criteria**
- Reviews are auto-routed.
- Contribution flow is standardized.

### Step 4.2: Add CI workflows
**What to implement**
- Build/test/lint/security workflows in `.github/workflows`.

**Implementation details**
- Required workflows:
  - `ci-go.yml`:
    - `go test ./internal/...`
    - selected `./test` short path
    - `go vet ./...`
    - format check via `gofmt -l .`
  - `ci-proto.yml`:
    - verify generated proto state clean
  - `security.yml`:
    - `govulncheck` + dependency scan
  - `docker-build.yml`:
    - build container images for changed Docker targets
- Use path filters to reduce monorepo CI cost.

**Acceptance criteria**
- PRs are gated on deterministic required checks.
- CI is incremental for unrelated path changes.

### Step 4.3: Add deployment pipelines
**What to implement**
- Controlled deploy workflows for staging/production.

**Implementation details**
- Workflow files:
  - `deploy-staging.yml`
  - `deploy-production.yml` (manual approval gate)
- Artifacts:
  - versioned image tags (`sha`, `semver`, `latest` policy-defined)
- Add environment protections and required reviewers.

**Acceptance criteria**
- Deployments are reproducible, approval-gated, and auditable.

---

## Phase 5 — Release and Operational Completeness

### Step 5.1: Add release process documentation and automation
**What to implement**
- Add root:
  - `RELEASING.md`
  - `CHANGELOG.md`
  - optional `.goreleaser.yml` (if chosen)

**Implementation details**
- Define:
  - versioning policy
  - release branch/tag process
  - rollback process
  - artifact matrix (binaries/images)
- Auto-generate changelog from conventional labels or commit metadata.

**Acceptance criteria**
- Team can produce and roll back releases using documented steps.

### Step 5.2: Fix broken documentation references and command semantics
**What to implement**
- Resolve missing docs linked by admin README.
- Align session docs with current commit/push behavior.

**Implementation details**
- Either create missing docs or update links:
  - `DEPLOYMENT.md`
  - `k8s/README.md`
  - `k8s/PRODUCTION.md`
- Update `docs/usage.md` and `cmd/monofs-session/README.md`:
  - clearly distinguish local virtual commit vs upstream push.

**Acceptance criteria**
- No dead internal doc links.
- CLI behavior and docs are consistent.

---

## Phase 6 — Monorepo UX and Data Model Enhancements

### Step 6.1: Add platform-wide commit/diff ledger API
**What to implement**
- Cross-workspace, cross-user searchable listing of:
  - local virtual commits
  - pushed commits
  - diff summaries

**Implementation details**
- Persist normalized entities:
  - commit metadata
  - repository deltas (file counts, operation counts)
  - links to job IDs and upstream commit hashes
- Add query filters:
  - workspace, principal, repo, branch, date range, status.

**Acceptance criteria**
- Operators and developers can answer “what changed, where, by whom” without local overlay access.

### Step 6.2: Extend supported source operations
**What to implement**
- Add rename/chmod support in publish bundle path.

**Implementation details**
- Extend operation model in workspace bundle.
- Preserve backward compatibility with old bundle versions.
- Ensure fetcher applies operations safely and atomically per repo plan.

**Acceptance criteria**
- Common Git workspace operations work natively through MonoFS flows.

---

## Recommended Execution Order

1. Phase 0 (feature flags/safety)
2. Phase 1 (durability/audit)
3. Phase 2 (commit fidelity)
4. Phase 3 (policy + auto push)
5. Phase 4 (repo governance + CI/CD)
6. Phase 5 (release/docs)
7. Phase 6 (enhancements)

---

## Validation Matrix (per phase)

- **Unit**: new stores, policy engine, replay planner, branch mapping behavior.
- **Integration**: router/fetcher restart recovery, replay push correctness, policy deny paths.
- **E2E**: commit → push/auto-push → upstream verification → refresh consistency.
- **Operational**: job querying at scale, retention pruning, deploy/rollback drills.

---

## Definition of Done (Program-Level)

- Job and bundle lifecycle are durable and queryable after restarts.
- Upstream push can preserve local commit lineage end-to-end.
- Auto-push works under explicit policy controls and emits audit events.
- Repository has enforced CI/CD + governance files and protected deployment flow.
- Release and deployment docs are complete, accurate, and linked from root docs.
