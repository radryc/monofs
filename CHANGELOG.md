# Changelog

## [Unreleased]

### Added
- Phase 1: WAL-backed durable workspace job and audit event store (`internal/storage/workspacestore/`).
- Phase 1: S3 snapshot export for compacted audit data (`internal/storage/auditstore/`).
- Phase 1: `--workspace-state-dir` flag on `monofs-router` for persistent job state.
- Phase 2: `SourcePushMode=preserve` — per-commit upstream replay with provenance trailers.
- Phase 2: `SourcePushMode` proto enum and `--source-push-mode` flag.
- Phase 2: `local_commit_id` and `local_commit_index` fields in progress payloads.
- Phase 3: Policy-gated push governance engine (`internal/router/workspacepolicy/`).
- Phase 3: YAML-based policy config with glob matching and match_not negation.
- Phase 3: `--policy-gate` and `--policy-config` flags on `monofs-router`.
- Phase 3: Pull request provider interface and GitHub/GitLab implementations (`internal/router/workspacepr/`).
- Phase 4: CI/CD workflows (go, proto, security, docker-build).
- Phase 4: Repository governance (CODEOWNERS, PR template, issue templates).
- Phase 5: Release process documentation and operational guides.

### Changed
- Workspace sync jobs persisted to WAL when `--workspace-state-dir` is configured.
- Session CLI no longer shows squash warning (mode selectable via `--source-push-mode`).

### Fixed
- WAL truncated-line recovery: truncated final entries are discarded on replay.
- WAL pointer-capture bug in recovery loop.
- Policy engine empty `match_not` inverted all rules — fixed to treat empty as unset.
