# Phase 2 — Upstream Commit Fidelity (No Mandatory Squash)

## Design

### Objective
Support preserved local commit replay upstream while retaining squash mode as explicit fallback.

### Current baseline

- Source push currently aggregates operations per repo and emits one commit per repo (`sourcePushRepositoryPlans` in `internal/fetcher/workspace_source_push.go`).
- Commit message explicitly states squash behavior (`sourcePushCommitMessage`).
- CLI also warns users about squashing (`cmd/monofs-session/main.go`).
- Local commit metadata already exists in session and bundle models:
  - `LocalVirtualCommit` in FUSE/session
  - `SourceCommitBundle` with ordered commits and author metadata (`internal/workspacebundle/commit_bundle.go`).

### Target behavior

- `SourcePushMode=squash`:
  - preserve current behavior exactly.
- `SourcePushMode=preserve`:
  - replay local commits in deterministic order for each repository
  - preserve local commit author/message and logical ordering
  - add provenance trailers in upstream commit body:
    - `MonoFS-Local-Commit: <id>`
    - `MonoFS-Workspace: <workspace-id>`
    - `MonoFS-Job: <job-id>`

### Conflict and retry semantics

- Track replay checkpoint per repository:
  - `last_pushed_local_commit_index`
- On conflict:
  - mark specific offending local commit ID and stop replay for that repository
- On retry:
  - resume from first unpushed local commit
  - never duplicate already pushed commits.

## Implementation plan

1. Add source push mode plumbing from Phase 0 to fetcher push path.
2. Refactor `sourcePushRepositoryPlans` into per-repo, per-commit replay plans.
3. Implement replay executor:
   - for each repo: apply operations from one local commit, stage, commit, push, repeat
   - deterministic ordering by `created_at_unix`, then commit ID.
4. Add provenance trailer builder and attach to commit messages.
5. Extend progress payloads / router job repository result model with:
   - `local_commit_id`
   - `local_commit_index`
   - checkpoint/resume metadata for failed repos.
6. Update session CLI messaging:
   - remove squash warning when mode is preserve
   - surface mode and conflict commit IDs in output.
7. Add tests:
   - preserve mode yields N upstream commits for N local commits
   - squash mode unchanged
   - retry after midpoint failure does not duplicate pushed commits.

