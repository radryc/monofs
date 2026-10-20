# Repository watcher: failover and scaling

Status: core failover, due-time scheduler, metadata-only probe and Guardian
sharding/leases are implemented in this tree. Deployment verification and the
event-driven Guardian repo index remain pending.

See [repo-watcher-deploy-checklist.md](repo-watcher-deploy-checklist.md) for the
post-deployment checks.

This document covers the MonoFS repository watcher and the Guardian repo-import
loop. It is the control-plane path that turns a Git push into a MonoFS refresh
and a Guardian deployment.

## Goals

- Survive the loss of a watcher/import worker without manual intervention.
- Scale to tens of thousands of ingested repositories.
- Keep MonoFS data placement/replication unchanged.
- Make scheduling, probe latency, failures and lag observable.
- Provide a deployment checklist that can be verified later.

## Non-goals

- Changing MonoFS data sharding, replication or rebalancing.
- Production-grade audit/history (separate follow-up).
- Replacing the existing Git webhook receiver.

## Limits addressed by this change

The following were true before this implementation:

- One static `--auto-refresh-owner`; no automatic failover.
- Every interval scanned all ingested repositories.
- The fetcher probe could clone/open a repository; too expensive at scale.
- State was in memory; a restart lost the schedule/backoff.
- Guardian's repo-import loop scanned every repository every 30s. It is now
  sharded and leased, but still lists and content-compares authored bundles
  per shard until the repo-index/event queue follow-up lands.

## Architecture

```text
GitHub/GitLab webhooks ─┐
                        ├─→ MonoFS change intent
Periodic fallback ──────┘
                                  │
                    ┌─────────────┴─────────────┐
                    │  MonoFS repo watcher       │
                    │  due-time heap + shards    │
                    │  Kubernetes Lease per shard│
                    └─────────────┬─────────────┘
                                  │ metadata-only head probe
                                  ▼
                         re-ingest queue (coalesced)
                                  │
                         MonoFS re-ingest + commit update
                                  │
                    ┌─────────────┴─────────────┐
                    │ Guardian repo importer     │
                    │ sharded + leases           │
                    └─────────────┬─────────────┘
                                  │ changed authored bundles
                                  │ (content compare per shard)
                                  ▼
                         Guardian reconcile/apply
```

### Component boundaries

| Component | Responsibility | Sharding |
|---|---|---|
| MonoFS router watcher | Due-time probes, re-ingest scheduling, per-repo trace | `hash(storage_id) % shards` |
| MonoFS fetcher | Metadata-only head probe, git cache, rate limits | per-host concurrency |
| Guardian repo importer | Import changed `guardian/partitions/**` bundles | `hash(repo_name) % shards` |
| Kubernetes Lease | One owner per shard, automatic takeover | one lease per shard |
| Repo state | Last probe, next probe, commit, failures, counters | sharded JSON snapshot |

MonoFS data sharding remains unchanged.

## Scheduling

Each repository gets a watch record:

```json
{
  "storage_id": "repo-...",
  "repo_url": "https://github.com/acme/demo.git",
  "branch": "main",
  "commit_hash": "abc123",
  "next_probe_at": "2026-10-20T10:00:00Z",
  "last_probe_at": "2026-10-20T09:59:00Z",
  "last_status": "unchanged",
  "last_error": "",
  "last_change_at": "2026-10-20T09:30:00Z",
  "changes_total": 2,
  "reingests_total": 2,
  "failure_count": 0,
  "backoff_until": "0001-01-01T00:00:00Z"
}
```

The worker keeps a min-heap ordered by `next_probe_at`:

1. Pop due repositories (up to concurrency/batch size).
2. Probe their upstream heads in one fetcher RPC.
3. On `ADVANCED`, enqueue a re-ingest.
4. Compute the next interval from the result and schedule the repo back into the heap.
5. Persist a shard snapshot every 15–30 seconds and on shutdown.

### Adaptive intervals

| State | Interval |
|---|---|
| changed in last hour | `auto-refresh-interval` (default 1m) |
| stable 1–24h | 5m |
| stable 1–7d | 15m |
| stable >7d | `auto-refresh-max-interval` (default 1h) |
| failures | exponential backoff, capped at 1h |

A ±20% jitter is applied to every next-probe time. After a watcher restart or
failover, the first probe for repos without persisted state uses the commit
age but is capped at 15 minutes, bounding the cold-start drain.

### Batching and rate limits

- `--auto-refresh-batch-size` groups probes into one fetcher RPC.
- `--auto-refresh-concurrency` bounds the number of in-flight probe RPCs.
- Per-host token buckets are a follow-up; until then, tune concurrency and use
  repo-level exponential backoff on 429/403/auth failures.

## Metadata-only probe

The watcher sets `metadata_only=true` on the probe reference. The fetcher then
uses `git ls-remote` against the remote (or the cached repository) instead of
cloning/opening a worktree. Only an `ADVANCED` result triggers a full ingest.

This is the largest scaling improvement: 10k head probes no longer imply 10k
clones.

## Failover

### Kubernetes Lease

- One Lease per shard: `monofs-repowatch-<shard>` and
  `guardian-repo-import-<shard>`.
- Helm/Kubernetes manifests provide `POD_NAME` and `POD_NAMESPACE`.
- Lease duration 15s, renew 5s.
- The leader renews; on renewal failure it stops scheduling new probes and a
  standby takes over.
- In-flight probes may finish; enqueueing a re-ingest checks the worker's
  lease state and the per-repo dedup window. A strict fencing token on the
  MonoFS write path is a follow-up; duplicate re-ingests are idempotent.
- Non-Kubernetes deployments fall back to the static shard/owner flags.

### Sharding

`hash(storage_id) % shards` for MonoFS and `hash(repo_name) % shards` for
Guardian. With 8 shards on 4 replicas, each replica can own two shards. A dead
replica's leases are re-acquired by the survivors.

## State and persistence

- MonoFS state: `<state-dir>/repowatch-<shard>.json` (atomic rename).
- Guardian state: in-memory last-import content comparison is retained; the
  shard/lease limits the scan surface.
- The repo's durable commit hash remains on the MonoFS node metadata; the
  watcher snapshot is an optimization, not the source of truth.

## Observability

Add/keep:

- `monofs_router_auto_refresh_probes_total{result}`
- `monofs_router_auto_refresh_reingests_total`
- `monofs_router_auto_refresh_backoff_total`
- `monofs_router_auto_refresh_tracked_repositories{shard}`
- `monofs_router_auto_refresh_scheduled_repositories{shard}`
- `monofs_router_auto_refresh_lease_held{shard}`
- `monofs_router_auto_refresh_next_probe_timestamp_seconds{shard}`
- scheduler lag can be derived from `next_probe_timestamp_seconds` vs `time()`
- Guardian shard import/failure logs; Prometheus counters are a follow-up

The MonoFS UI exposes:

- Repositories → Watcher
- Fetchers → Repository Watcher
- `GET /api/workers/auto-refresh`
- `POST /api/repositories/refresh`

## Rollout phases

1. **Failover** — Lease package, shard ownership, static fallback.
2. **Scheduler** — due-time heap, adaptive intervals, jitter, persisted snapshots.
3. **Probe** — metadata-only `ls-remote`, batching. Per-host limits remain a
   follow-up.
4. **Guardian** — sharded importers with leases.
5. **Load test** — automated coverage includes a 10k-repo scheduler sync test,
   lease acquire/takeover tests, shard-filter tests and a metadata-probe test.
   Cluster-level 10k/50k load, leader-kill and rate-limit tests remain manual
   (see the deployment checklist).

## Deployment checklist

- [ ] Rebuild `monofs-router`, `monofs-fetcher` and `guardiand` images from this tree.
- [ ] Confirm `POD_NAME`/`POD_NAMESPACE` are exposed via the downward API.
- [ ] Create/confirm Lease RBAC (`coordination.k8s.io/leases`) for the router
      and guardiand service accounts.
- [ ] Set `MONOFS_AUTO_REFRESH=true`, interval, concurrency, shards, batch size,
      max interval, state dir.
- [ ] Set `GUARDIAN_REPO_IMPORT_ENABLED=true`, interval, shards.
- [ ] Verify `GET /api/workers/auto-refresh` reports the expected shard/owner.
- [ ] Kill the leader; confirm another replica acquires the lease within ~20s.
- [ ] Push a commit; confirm the MonoFS commit hash changes and Guardian
      deploys without a manual `repo import`.
- [ ] Confirm no sustained duplicate re-ingests (a brief handoff overlap is
      possible and idempotent).
- [ ] Confirm 10k-repo load test meets the probe-lag target.
- [ ] Check rate limits/backoff on a 429 or auth failure.
- [ ] Check restart: schedule reloads and probes are jittered.

## Open follow-ups

- Webhook automation at org scale (GitHub App / org webhook).
- Event-driven Guardian import (currently sharded content compare).
- Per-host probe rate limits.
- Dedicated watcher deployment (currently sharded router workers).
- Strict fencing tokens for re-ingest (best-effort lease check today).
- Bundle deletion/pruning.
- Production audit/history.
