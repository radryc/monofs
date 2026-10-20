# Repository watcher deployment checklist

Use this after building images from this tree. These checks are for the
MonoFS watcher, its Kubernetes Lease, and the Guardian repo-import loop.

## 1. Build and load images

- [ ] Build `monofs-router` from the current tree.
- [ ] Build `monofs-fetcher` from the current tree. The metadata-only probe
      lives in the fetcher; an old fetcher will silently fall back to a clone.
- [ ] Build `guardiand` from the current tree.
- [ ] Load all three images into the target cluster / registry.
- [ ] Confirm the pods use the new image digests, not a cached tag.
- [ ] If your image pipeline keys off `partitions/_bootstrap/intents/images.yaml`
      versions, bump the router/fetcher/guardian versions or force the build.

Commands (adjust for your image pipeline):

```bash
cd ~/strata/monofs
docker build --target router  -t monofs-router:latest  .
docker build --target fetcher -t monofs-fetcher:latest .
# The router embeds the UI; build it before a local Go build.
cd ~/strata/monofs/internal/router/ui && npm ci && npm run build
cd ~/strata/guardian && go build ./cmd/guardiand
```

## 2. RBAC

- [ ] `monofs` default ServiceAccount can get/list/watch/create/update/patch
      `leases` in the monofs namespace.
- [ ] The guardian ServiceAccount can do the same in the guardian namespace.
      The bootstrap manifest now includes an explicit lease Role/RoleBinding;
      the guardian SA also has `cluster-admin` today.

The bootstrap manifest is:

```text
stratatools/deploy/bootstrap/storage/rbac-repowatch.yaml
```

It binds the `default` ServiceAccount. If you set `serviceAccountName` on the
router pod, update the RoleBinding subject or create an equivalent RoleBinding
for that account.

## 3. Deployment environment

MonoFS router:

| Variable | Value |
|---|---|
| `POD_NAME` | downward API `metadata.name` |
| `POD_NAMESPACE` | downward API `metadata.namespace` |
| `MONOFS_AUTO_REFRESH` | `true` |
| `MONOFS_AUTO_REFRESH_INTERVAL` | `1m` |
| `MONOFS_AUTO_REFRESH_MAX_INTERVAL` | `1h` |
| `MONOFS_AUTO_REFRESH_CONCURRENCY` | `4` |
| `MONOFS_AUTO_REFRESH_BATCH_SIZE` | `32` |
| `MONOFS_AUTO_REFRESH_SHARDS` | `2` (or more) |
| `MONOFS_AUTO_REFRESH_STATE_DIR` | `/var/lib/monofs/repowatch` |
| `MONOFS_AUTO_REFRESH_LEASE` | `true` |
| `MONOFS_AUTO_REFRESH_LEASE_NAME` | `monofs-repowatch` |
| `MONOFS_AUTO_REFRESH_OWNER` | empty when leases are enabled |

Guardian:

| Variable | Value |
|---|---|
| `GUARDIAN_REPO_IMPORT_ENABLED` | `true` |
| `GUARDIAN_REPO_IMPORT_INTERVAL` | `30s` |
| `GUARDIAN_REPO_IMPORT_SHARDS` | `2` (or more) |
| `GUARDIAN_REPO_IMPORT_LEASE` | `true` |
| `GUARDIAN_REPO_IMPORT_LEASE_NAME` | `guardian-repo-import` (leases become `...-0`, `...-1` when shards > 1) |

- [ ] The router has the `repowatch` emptyDir mounted at
      `/var/lib/monofs/repowatch`.
- [ ] Outside Kubernetes, set `MONOFS_AUTO_REFRESH_LEASE=false` and give each
      router a distinct `MONOFS_AUTO_REFRESH_SHARD` (static ownership).
- [ ] At least two router pods are running (`router-a` + `router-b`). Each
      router competes for every shard lease; a dead router's shards are taken
      over by the survivor.
- [ ] `guardiand` keeps its normal replica count. The shipped deployment uses
      one replica; Kubernetes restarts it and the lease is reacquired. Scale
      Guardian replicas only if the rest of `guardiand` is HA-safe.

## 4. Deploy

- [ ] Apply the updated bootstrap/RBAC/route manifests, or rebuild the control
      plane with `guardianctl dev init`.
- [ ] Roll out `monofs-router`.
- [ ] Roll out `guardiand`.
- [ ] Confirm both deployments become ready.

## 5. Worker status

```bash
kubectl -n monofs port-forward svc/... 8080:8080
curl -s http://localhost:8080/api/workers/auto-refresh | jq
```

Expected:

- `enabled: true`
- `shards` matches the configured value
- `shard` is `-1` when aggregated across shards, or the specific shard
- `lease_held: true` on one replica per shard
- `tracked` grows as repos are ingested
- `scheduled` is close to `tracked`
- `interval` / `max_interval` / `batch_size` match the deployment

UI:

- MonoFS → Fetchers → Repository Watcher
- MonoFS → Repositories → Watcher column

- [ ] Both pages show the watcher and per-repo probes.

## 6. Lease failover

Find the current lease holder:

```bash
kubectl -n monofs get lease monofs-repowatch-0 -o yaml
kubectl -n monofs get lease monofs-repowatch-1 -o yaml
```

- [ ] Kill the pod holding shard 0.
- [ ] Within ~20 seconds, another router pod acquires `monofs-repowatch-0`.
- [ ] The new holder's `/api/workers/auto-refresh` reports `lease_held: true`.
- [ ] No sustained duplicate re-ingests occur; the 60s dedup window may allow
      one overlap.

Repeat for shard 1.

Guardian leases (when shards > 1):

```bash
kubectl -n guardian get lease guardian-repo-import-0 -o yaml
kubectl -n guardian get lease guardian-repo-import-1 -o yaml
```

- [ ] Kill the guardiand pod holding a shard; the restarted pod (or another
      replica, if configured) acquires it after the lease TTL.

## 7. Per-repo tracking

- [ ] Ingest a test repository with a branch.
- [ ] Wait for it to appear in `/api/repositories`.
- [ ] Confirm it has a `refresh` object with `last_probed_at` and
      `last_probe_status`.
- [ ] `POST /api/repositories/refresh` with `storage_id=<id>` queues a check.
- [ ] The watcher UI shows the check.

## 8. Automatic re-ingest

- [ ] Push a commit to an ingested repository.
- [ ] Wait for the probe interval.
- [ ] Confirm `/api/repositories` shows the new `commit_hash`.
- [ ] Confirm `monofs_router_auto_refresh_reingests_total` increments.
- [ ] Confirm the repository content in MonoFS reflects the new commit.

If the probe reports `base_commit is required` after a router restart, the
discovery path did not restore the commit hash; check the current image.

## 9. Guardian auto-import

- [ ] Push a change under `guardian/partitions/<name>/`.
- [ ] Do **not** run `guardianctl repo import`.
- [ ] Within the import interval + apply time, Guardian should show an APPLY
      rollout and the partition should converge.
- [ ] Guardian logs should show `repo-import: shard ... imported ...`.

If the partition does not update:

- verify `GUARDIAN_REPO_IMPORT_ENABLED=true`
- verify the repo source is a Git URL or `/.repos/...`
- verify the repo has an authored `guardian/partitions/<name>/config.yaml` or
  `partition.yaml`
- verify the guardian pod can read `/.repos/...`

## 10. Sharding

- [ ] With `shards=2`, confirm lease 0 and lease 1 are held by different pods
      (or the same pod if replicas=1).
- [ ] Confirm the `/api/workers/auto-refresh` aggregate `tracked` is roughly the
      total repo count, not double.
- [ ] Confirm each shard's per-repo list is disjoint.
- [ ] Scale router replicas from 2 to 3; confirm one replica remains standby
      (or acquires a shard only after a failure).

## 11. Load test

Use the 10k synthetic scheduler test as a smoke test:

```bash
cd ~/strata/monofs
go test ./internal/router -run TestAutoRefreshSchedulesTenThousandRepos -v
```

For a real cluster load test, ingest 10k repositories with a staging branch and
record:

- scheduler due lag (`next_probe_at` vs now)
- probe duration p50/p95
- re-ingest queue depth and lag
- CPU/memory of router and fetcher
- rate-limit / 429 counts
- Guardian import lag

Targets to agree before rollout:

- p95 probe lag < 2× interval
- no unbounded queue growth
- no sustained rate-limit errors
- failover within 2× lease TTL

## 12. Known gaps / next checks

- Repo bundle **deletions** are not pruned by the importer yet.
- Guardian still lists all repos and content-compares bundles per shard; a
  repo-index/event queue is the next scaling step.
- Per-host probe rate limits are not implemented; tune concurrency and watch
  for upstream 429/403 responses.
- Re-ingest fencing is a best-effort lease check; a strict fencing token on the
  MonoFS write path is a follow-up.
- The watcher still runs inside the router; a dedicated watcher deployment is
  an optional follow-up.
- Production audit/history is not implemented.
- Webhook automation at org scale (GitHub App / org webhook) is not included.
- Non-Kubernetes deployments fall back to static shard ownership.

## 13. Rollback

- [ ] Set `MONOFS_AUTO_REFRESH=false` on the router if the watcher must stop.
- [ ] Set `GUARDIAN_REPO_IMPORT_ENABLED=false` on guardiand to stop imports.
- [ ] Keep the Lease objects; they expire automatically.
- [ ] Revert images/manifests to the previous revision if needed.
