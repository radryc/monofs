# MonoFS Readiness Program — Phase Design and Implementation Docs

These documents convert `TODO.md` into executable, code-grounded design and implementation plans.

Each phase document is structured in this order:

1. **Design** (target architecture, data model, behavior)
2. **Implementation plan** (concrete steps, file touchpoints, rollout)

## Phases

1. [Phase 0 — Foundation and safety rails](phase-0-foundation.md)
2. [Phase 1 — Durable jobs and audit trail](phase-1-durability-and-audit.md)
3. [Phase 1B — Storage-node WAL and router proxy](phase-1b-storage-node-wal-proxy.md)
4. [Phase 2 — Upstream commit fidelity](phase-2-commit-fidelity.md)
5. [Phase 3 — Policy-gated and automatic push](phase-3-policy-and-autopush.md)
6. [Phase 4 — Governance, CI, and deployment pipelines](phase-4-governance-ci-cd.md)
7. [Phase 5 — Release and operational completeness](phase-5-release-and-operations.md)
8. [Phase 6 — UX and data model enhancements](phase-6-ux-and-data-model.md)

## Design assumptions (applies to all phases)

All performance, scale, and concurrency targets are derived from a single organizational profile. Phases that don't specify their own targets inherit these.

| Parameter | Value |
|-----------|-------|
| Engineers in the monorepo org | 2,000 |
| Local commits per engineer per day | 20 |
| Push jobs per engineer per day | 4 (batches 5 commits on average) |
| Peak window | 4 hours (40% of daily load) |
| CI/automation workspace overhead | +50% on top of engineer workspace count |
| Target uptime | 99.9% (≤ 8.7h downtime/year) |

**Derived numbers** (used in Phase 1, 3, and 6 target tables):

| Metric | Daily | Peak (per second) |
|--------|-------|-------------------|
| Source-push jobs | 8,000 | ~0.6 sustained, ~2 burst |
| Audit events | 80,000 | ~5 sustained, ~50 burst |
| Concurrent push operations | — | ≤ 50 |
| WAL appends | 80,000 | ~5 sustained, ~50 burst |
| Policy evaluations | 8,000 | ~0.6 sustained, ~2 burst |

## Security threat model

Phases 1–3 introduce new attack surfaces beyond the baseline MonoFS security posture (which already handles transport encryption, blob encryption at rest, and Guardian principal auth). This section enumerates each new threat, its attack vector, impact, and how the design mitigates or detects it.

### T1: Policy engine bypass

**Attack vector:** A compromised router process or a malicious gRPC caller crafts a push request that skips `workspacepolicy.Evaluate()`. Code bug in the `workspace_source_push.go` handler that omits the policy check call.

**Impact:** Unauthorized push to protected branches, bypassing governance rules.

**Mitigation:**
- The policy evaluation call is inserted as a **single mandatory code path** in the push handler, immediately before the fetcher RPC. No alternate paths exist.
- Integration tests assert `deny-before-write` — a policy denial must never be followed by a successful upstream push.
- The Phase 1 audit event `policy.denied` is emitted **before** any side effects; even if a bug allows the push to proceed, the denial is logged immutably.
- Policy config is loaded at startup and validated (unknown fields/values → reject); runtime config corruption is a crash, not a silent bypass.

### T2: Unauthorized auto-push triggering

**Attack vector:** An authenticated caller with a valid client ID invokes the explicit auto-push trigger API (`TriggerAutoPush` admin RPC) for a workspace they do not own. Or a compromised router config sets `AutoPushInterval` to 0, flooding upstream repos.

**Impact:** Denial of service on upstream Git hosts; unauthorized mutation of repository state; CI pipeline spam from auto-created PRs.

**Mitigation:**
- `TriggerAutoPush` RPC checks that `requested_by_client_id` matches the workspace's owner (the client that registered it). Unauthorized triggers return `PERMISSION_DENIED`.
- The auto-push worker has a **global concurrency cap** (20 concurrent pushes). Flood attacks are rate-limited by the worker itself.
- `AutoPushInterval` has a minimum of 30 seconds (validated at startup; values below this are clamped).
- Every auto-trigger emits a `policy.auto_triggered` audit event with the triggering principal ID — even if the push itself is denied by policy.

### T3: Audit log tampering

**Attack vector:** An attacker with filesystem access to `--workspace-state-dir` modifies or deletes WAL segments or snapshot files to remove evidence of malicious activity. An attacker with S3 access deletes compacted snapshots.

**Impact:** Loss of forensic trail; ability to hide unauthorized pushes or policy violations.

**Mitigation:**
- **WAL is append-only.** Segments are never modified after rotation — only deleted by the compaction worker after a checkpoint is written. Tampering with a sealed segment is detectable: replay would produce a truncated or missing `seq` range.
- **Snapshots are checksummed.** Compaction writes a gzip archive; the checkpoint file includes a SHA256 hash of the snapshot. On recovery, the hash is verified before loading. Mismatch → crash with alert.
- **S3 bucket has object versioning enabled** (deployment requirement). Deleted snapshots are recoverable. Bucket policy denies `s3:DeleteObjectVersion` to the router's IAM role.
- `monofs-admin` will include a `verify-audit` subcommand that replays the WAL + snapshots and reports any integrity violations (gaps, hash mismatches, backwards seq jumps).

### T4: WAL injection of fake records

**Attack vector:** An attacker with filesystem access to `--workspace-state-dir` appends forged JSON lines to the active WAL segment, creating fake audit events or job transitions.

**Impact:** Injection of false records into the immutable audit trail; corruption of in-memory job state on restart.

**Mitigation:**
- **File permissions:** `workspace-state-dir` is created with `0o700`. WAL segments are `0o600`. The router process runs as a dedicated unprivileged user.
- **seq monotonicity guard:** On WAL replay at startup, every entry's `seq` must be strictly greater than the previous. A gap or duplicate → replay halts and logs `WAL_INTEGRITY_ERROR`. The router does not start until the corrupted segment is manually repaired or removed.
- **TRUSTED BY DEFAULT constraint:** WAL entries from before the last checkpoint are not replayed (they were compacted into a snapshot). This limits the injection window to only the current, non-compacted WAL segments.

### T5: PR provider credential exposure

**Attack vector:** `PRProviderToken` leaks through error messages, debug logs, or diagnostics endpoints. Token is stored in plaintext in the router config file or env vars.

**Impact:** Attacker gains write access to all GitHub/GitLab repos the token can access. Can create malicious PRs, modify repo settings, or exfiltrate source code.

**Mitigation:**
- `PRProviderToken` is **never logged**. The config loader redacts it before any log output.
- `PRProviderTokenFile` is preferred over inline `PRProviderToken` — the token lives in a file with `0o600` permissions, readable only by the router process.
- Diagnostics endpoints (`/debug/vars`, `/metrics`) never include token values.
- The token is stored in memory as `[]byte` (not `string`) and zeroed on config reload.
- Consideration for future: secret manager integration (Vault, AWS Secrets Manager) as a token source, replacing file-based config.

### T6: Policy config injection

**Attack vector:** An attacker with write access to the policy YAML file (`--policy-config`) adds a permissive rule (e.g., `workspace_ids: ["*"], effect: allow`) as the first rule in the list.

**Impact:** All subsequent policy evaluations are bypassed; unauthorized pushes succeed.

**Mitigation:**
- Policy file permissions: `0o600`, owned by the router process user. Only operators with sudo access to the router host can modify it.
- `SIGHUP` hot-reload validates the new config before activating it. Unknown fields, bad enums, or missing `effect` → reload is rejected, previous config stays active.
- Every config load (startup and hot-reload) emits a `policy.config_loaded` audit event with a SHA256 hash of the policy file content. Operators can compare this against the known-good hash.
- Future: policy config could be stored in Guardian as a versioned intent, leveraging Guardian's immutable version audit trail for policy changes.

### T7: Cross-workspace job state leakage

**Attack vector:** A client queries `ListWorkspaceSyncJobs` or `QueryLedger` for a workspace it does not own, retrieving commit messages, file counts, or branch names that should be private.

**Impact:** Information disclosure across workspace boundaries.

**Mitigation:**
- Query APIs filter results by the authenticated client's `principal_id`. A client can only see jobs and ledger records for workspaces they registered.
- The `WorkspaceSyncJob.requested_by_client_id` field is set at job creation and cannot be changed. All list/query handlers apply `WHERE requested_by_client_id = <authenticated_principal>` before any other filter.
- Admin RPCs (`ExportAuditSnapshot`, `TriggerAutoPush`) require a separate admin role (TBD in Phase 4 governance model — currently gated by Guardian principal role check).

### Threat model summary

| Threat | Severity | Detection | Prevention |
|--------|----------|-----------|------------|
| T1: Policy bypass | Critical | Audit event logged before mutation; integration test | Single mandatory code path |
| T2: Unauthorized auto-push | High | `policy.auto_triggered` event + rate cap | Ownership check + concurrency cap |
| T3: Audit log tampering | High | WAL integrity check + snapshot hash verification | Append-only WAL + S3 versioning |
| T4: WAL injection | Medium | seq monotonicity guard on replay | File permissions + checkpoint truncation |
| T5: Credential exposure | Critical | Token redaction in logs | Token file + memory zeroing |
| T6: Policy config injection | Medium | Hash audit on every config load | File permissions + validation + Guardian versioning (future) |
| T7: Workspace data leakage | Medium | Query result filtering | `principal_id` enforcement in all list/query handlers |

## Code baselines used

- Router/fetcher workspace sync and source push:
  - `internal/router/workspace_sync.go`
  - `internal/router/workspace_publish.go`
  - `internal/router/workspace_source_push.go`
  - `internal/fetcher/workspace_publish.go`
  - `internal/fetcher/workspace_source_push.go`
- Current API surface:
  - `api/proto/monofs.proto`
  - `api/proto/fetcher.proto`
- Existing persistence pattern in router:
  - `internal/router/guardian_principals.go`
  - `internal/router/guardian_version_store.go`
- Existing object-store path for archives:
  - `internal/storage/interface.go`
  - `internal/storage/blob/backend.go`
  - `cmd/monofs-fetcher/main.go`
- New packages introduced by readiness:
  - `internal/storage/auditstore/` — S3/GCS export of compacted workspace job and audit snapshots (Phase 1)
  - `internal/router/workspacejobstore/` — WAL-backed durable job/bundle/audit store (Phase 1)
  - `internal/router/workspacepolicy/` — push governance policy engine (Phase 3)
- Existing deployment/runtime ecosystem:
  - `Makefile`, `docker-compose*.yml`
  - `~/strata/guardian/README.md`, `~/strata/guardian/Makefile`
  - `~/strata/stratatools/README.md`, `~/strata/stratatools/docs/USAGE.md`
  - `~/strata/stratatools/deploy/bootstrap/bootstrap.yaml`
