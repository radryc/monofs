# Blob Layout v2 — durable digest→pack index, batching, compaction

Status: implemented; `BlobBackend` uses v2 by default. Legacy remains a read fallback and an opt-out (`--blob-layout=legacy`).
Goal: keep the number of pack objects small and the number of listings bounded
(ideally zero) on the read path, at 10^4–10^6 images and parallel deployments.

## Problems with the legacy layout

- `_loose/<digest>.pack` — one archive + `.idx` per small object (tags, manifests,
  small blobs). Object count grows with every metadata write.
- `_batch/batch-<ts>-<seq>.pack` — packed only when several blobs arrive in the
  same `StoreBlobBatch` call; one-object writes never batch.
- Reads resolve a digest by listing `_batch/` + `_loose/` (capped at 5000
  objects) and reading every `.idx` sidecar. O(N) per miss, and it times out.
- The legacy layout has no background compactor for blob archives. `DeleteBlobs(compact)`
  only rewrites archives on delete; `monofs-logengine-compact` is telemetry.
  v2 provides an optional compactor (`--blob-compactor`).

## Layout

```
packs/v2/sha256/<shard>/<packID>.pack
packs/v2/sha256/<shard>/<packID>.pack.idx
packs/v2/sha256/<shard>/MANIFEST.json
```

- `<shard>` = first two lowercase hex chars of the digest (`256` shards).
- `<packID>` = `<unixnano>-<seq>` (lexicographically time ordered).
- `.idx` = JSON array of digests contained in the pack (existing format).
- `MANIFEST.json` = versioned list of active and retired packs for the shard.

Reads never list. A read computes the shard from the digest, fetches
`MANIFEST.json`, then at most `maxActivePacks` `.idx` files, then ranges/streams
the owning pack:

```
shard = digest[0:2]
m     = GET packs/v2/sha256/<shard>/MANIFEST.json   (cached by version/ETag)
for p in m.packs (bounded):
    idx = GET <p>.idx                               (cached)
    if digest in idx: stream pack p
```

With compaction keeping `len(m.packs) <= 2` per shard, this is 1 manifest read
+ at most 2 index reads + 1 pack read, and normally 0 network calls for cached
manifest/index.

## Manifest

```json
{
  "layout": "v2",
  "shard": "ab",
  "version": 42,
  "packs": [
    {"id":"1794730000000000000-0000","bytes":536870912,"blobs":8192,"created_unix":1794730000,"sealed":true},
    {"id":"1794730100000000000-0000","bytes":1048576,"blobs":64,"created_unix":1794730100,"sealed":false}
  ],
  "retired": [
    {"id":"1794729900000000000-0000","bytes":536870912,"blobs":8192,"created_unix":1794729900,"sealed":true,"retired_unix":1794730200}
  ],
  "updated_unix": 1794730200
}
```

- `packs` = readable now.
- `retired` = superseded but kept for a grace period so in-flight readers with
  an older manifest version can still resolve.
- Updates are compare-and-swap on `version` (S3 conditional write, or a small
  metadata store). A failed CAS re-reads and retries.

## Batching policy

Writes are accumulated per shard and flushed on the first of:

| Trigger | Default |
|---|---|
| bytes in builder | 512 MiB (`TargetBytes`) |
| objects in builder | 8192 (`MaxObjects`) |
| builder age | 2 s (`FlushAfter`) |

- A single blob larger than `TargetBytes` is sealed into its own pack immediately.
- Flushing writes `<packID>.pack` + `.idx`, uploads, then CASes the manifest to
  add the pack and seal the previous active pack.
- Small, mutable registry metadata (tags/manifests) should not go through this
  path at all — it belongs in the metadata store. If it must, it is batched by
  the same writer.
- `MaxShards` bounds memory: if more shards are active, flush the oldest.

## Durable uploads (WAL)

Cloud uploads are asynchronous, so the backend records each archive in a
disk-backed upload WAL **before** it queues the S3/GCS copy. The WAL lives at
`<cache-dir>/archives/upload.wal.json`; pending paths are replayed at startup,
which closes the window where an archive was created but the process died
before the object-store upload completed.

`BlobBackend.Observability()` exposes the WAL and read-through state
(`wal_path`, `wal_pending`, `wal_recorded_total`, `wal_replayed_total`,
`wal_upload_failures_total`, cloud index/download counters). The router's
`GET /api/fetchers/observability` aggregates it across fetchers, and the
`/fetchers` UI renders the "Durable Uploads & S3 Read-Through" and "Blob Layout
& Compaction" cards from that endpoint.

## Compaction

Runs as its own component (not on the request path), round-robin over 256 shards.

For each shard:

1. Read `MANIFEST.json`.
2. Delete retired packs whose `retired_unix + grace <= now`.
3. If `len(active) <= MaxPacksPerShard` and no pack is dead-heavy, skip.
4. Choose sources: sealed packs with `dead_bytes/bytes >= DeadRatio`, or all
   sealed packs when there are too many. Never touch the unsealed active pack.
5. Merge sources into one or more packs of `<= TargetBytes` live bytes.
6. Upload new pack(s) + idx, CAS the manifest (add new, retire sources), wait
   `GracePeriod`, then delete retired packs.
7. If CAS fails, retry with the fresh manifest; if the pack upload succeeded but
   the manifest never referenced it, it is an orphan and is deleted by the
   orphan sweep.

Safety invariants:

- Never delete a pack before the new manifest is committed.
- Never delete a pack before `GracePeriod` has elapsed.
- Never drop a blob that is live; without a liveness set, all blobs are kept.
- Compaction is rate-limited (`MaxShardsPerRun`, `MaxBytesPerRun`) so it cannot
  starve image pulls.

## Migration / dual-read

1. Writes go to `packs/v2/...`.
2. Reads try v2 first, then the legacy path:
   - `HEAD _loose/<digest>.pack` (deterministic, O(1)) before any listing.
3. A backfill job reads legacy archives, writes them into v2 packs, commits the
   manifest, and marks legacy archives for deletion after grace.
4. Once no reads hit legacy for a full TTL, remove the legacy fallback.

## Complexity

| Operation | v2 |
|---|---|
| fetch digest | O(1) bounded GETs, zero listings |
| write blob | O(1) + amortized pack flush |
| pack count | O(total bytes / TargetBytes), ≈ ≤2 per shard |
| listings | compactor/backfill only |
| memory | manifests + idx for hot shards, bounded |

## Non-goals for v2

- Tag/manifest storage (belongs in the metadata store, not blob packs).
- Layer distribution/CDN/P2P (separate system).

## Implementation status

| Piece | Location | Status |
|---|---|---|
| Layout, manifest CAS, batcher, compactor | `internal/storage/blob/layout/` | implemented + unit tested |
| S3 object adapter (`Get`/`Put`/`Delete`) | `internal/storage/blob/v2.go` | implemented + MinIO integration test |
| v2 write path (single blob) | `BlobBackend.StoreBlob` | implemented |
| v2 write path (streamed batch) | `StoreBlobBatchWriter` | implemented; groups by shard and flushes at 512 MiB / 8192 objects / 2 s |
| v2 read path | `BlobBackend.v2FindBlob` | implemented; manifest + index cached for 30 s |
| Legacy fallback | `BlobBackend.findBlobInCloud` | v2 → `_loose/<id>.pack` HEAD → legacy index |
| Compactor | `BlobBackend.RunCompactor` + fetcher loop | implemented (opt-in; not enabled by Compose) |
| Upload WAL | `internal/storage/blob/upload_wal.go`, `BlobBackend` | implemented; pending S3 uploads replay on startup |
| UI / API | `/api/fetchers/observability`, `/fetchers` cards | implemented |

## Configuration

v2 is the default layout. Deployments whose object store does not support
conditional writes can opt out with `--blob-layout=legacy` or
`MONOFS_BLOB_LAYOUT_V2=false`.

Fetcher flags:

```
--blob-layout=v2          # default
--blob-layout=legacy      # opt out
--blob-compactor
--blob-compactor-interval=5m
--blob-compactor-grace=1h
--blob-compactor-max-shards=256
--blob-compactor-target-bytes=536870912
```

Environment equivalents:

```
MONOFS_BLOB_LAYOUT_V2=false    # opt out; unset or true uses v2
MONOFS_BLOB_COMPACT_ENABLED=true
MONOFS_BLOB_COMPACT_INTERVAL=5m
```

The `monofs` partition (reconciled read-only) and the bootstrap storage
manifests used by `guardianctl dev init` both pin
`MONOFS_BLOB_LAYOUT_V2=true`. No checked-in Compose service enables
`--blob-compactor`/`MONOFS_BLOB_COMPACT_ENABLED`, so background compaction is
opt-in per fetcher. If several fetchers run it, the manifest CAS makes duplicate
runs safe.

## S3 conditional writes

Manifest CAS requires conditional PUT:
- create: `If-None-Match: *`
- update: `If-Match: <etag>`

Both were verified against the cluster's MinIO (status 412 on a stale ETag).
If an object store does not support conditional writes, the CAS cannot be used
and v2 must stay disabled for that deployment.

## Operational notes

- Writes are immediate: a single-blob write creates one pack. The batching
  policy applies to streamed multi-blob writes; the compactor then merges the
  small packs created by single writes.
- Compaction keeps at most two active packs per shard and retires the rest for
  the grace period. Retired packs stay readable until the next cycle deletes
  them.
- `RewritePack` downloads source packs into the local cache first; size the
  fetcher cache for at least two target packs plus headroom.
- Reads fall back to the legacy layout, so switching to v2 (now the default)
  does not strand existing blobs. Backfilling legacy blobs into v2 is a
  separate job.

## Verification

```
go test ./internal/storage/blob/...
MONOFS_TEST_S3_ENDPOINT=http://127.0.0.1:19002 \
  MONOFS_TEST_S3_BUCKET=monofs \
  go test -run TestV2S3StoreIntegration ./internal/storage/blob/ -v
```

On the running system, `/fetchers` shows direct hits vs index lookups, v2 pack
counts, batching pressure, and compactor runs.
