# Phase 0 — Foundation and Safety Rails

## Design

### Objective
Introduce feature gates so all new behavior ships dark, can be enabled per environment, and can be rolled back instantly.

### Current baseline

- Router and fetcher startup flags exist, but none for workspace job durability, source push mode, auto-push, or policy gates (`cmd/monofs-router/main.go`, `cmd/monofs-fetcher/main.go`).
- Source push behavior is implicitly squash-per-repo in fetcher (`internal/fetcher/workspace_source_push.go`).
- Workspace sync jobs are in-memory only (`internal/router/workspace_sync.go`, `internal/router/router.go`).

### Target configuration model

- Add router config gates:
  - `WorkspaceStateDir string` — path for Phase 1 durable WAL + snapshots (default: "" = in-memory; separate from `GuardianStateDir`)
  - `WorkspaceJobStoreEnabled bool`
  - `PolicyGateEnabled bool`
  - `AutoPushEnabled bool`
  - `SourcePushMode string` (`squash` default, `preserve`)
  - `AutoPushInterval time.Duration`
  - retention knobs (`JobRetentionDays`, `MaxJobsPerWorkspace`)
  - `PolicyConfigPath string` — path to Phase 3 policy YAML
  - `PRProviderEnabled bool` + related Phase 3 auto-PR config fields
- Add fetcher config gate:
  - `SourcePushMode string` (`squash` default, `preserve`)
- Keep defaults backward-compatible with today’s behavior.

### Compatibility contract

- If all new flags are unset, existing publish/push/refresh behavior is unchanged.
- Enabling any phase-specific behavior must not require rebuild; config and flags only.

## Implementation plan

1. Extend `internal/router.RouterConfig` and fetcher `ServiceConfig` for new knobs.
2. Add CLI flags and env support in:
   - `cmd/monofs-router/main.go`
   - `cmd/monofs-fetcher/main.go`
3. Wire config into runtime components:
   - router workspace sync handlers (`internal/router/workspace_*.go`)
   - fetcher source push path (`internal/fetcher/workspace_source_push.go`)
4. Add strict validation:
   - reject invalid `SourcePushMode`
   - reject nonsensical intervals/retention values
5. Expose effective config in diagnostics/stats output so operators can verify runtime mode.
6. Add table-driven tests for defaults, explicit flags, and invalid values.

### Rollout

1. Ship with all new toggles off.
2. Enable per non-prod environment in sequence: durability -> preserve mode -> policy -> autopush.
3. Keep one-command rollback by setting toggles off.

