# Phase 1B - Storage-Node WAL and Router Proxy

## Design

### Objective
Move workspace ledger durability out of the router and into storage nodes.

After this change:
- Router is control-plane only for workspace ledger traffic.
- WAL ownership lives on storage nodes.
- Ledger proxy node selection is deterministic using client identity and rendezvous hashing.

### Why change
Current Phase 1 introduced router-local WAL (`workspace-state-dir`) for job and ledger durability. That improves restart recovery, but it makes the router stateful and puts write-path durability in the wrong tier.

For MonoFS architecture, storage nodes should remain the persistence boundary, while the router should route and enforce policy.

### Target architecture

#### 1. Router becomes ledger proxy
Router no longer persists workspace ledger WAL locally. Instead it:
- authenticates request
- resolves client ID
- picks ledger owner node via HRW
- forwards write/query RPC to chosen storage node
- returns streamed/aggregated response to client

Router keeps orchestration metadata in-memory as before (job lifecycle coordination), but durable ledger records are delegated to storage.

#### 2. Storage nodes own WAL
Each storage node runs a local WAL-backed ledger store for the subset of keys it owns.

On node disk:

```text
/var/lib/monofs/node/
  ledger/
    wal/
      wal-000001.log
      wal-000002.log
    snapshots/
      snapshot-000100.json.gz
    checkpoints/
      checkpoint.json
```

WAL format remains append-only JSONL with monotonic sequence numbers per node-owned shard.

#### 3. Deterministic owner selection (client ID + HRW)
Owner is selected with existing rendezvous hasher over healthy storage nodes.

Routing key:

```text
ledger-key = workspace_id + "|" + client_id
```

Selection:
- `owner = HRW(nodes).GetNode(ledger-key)`
- optional replicas: `HRW(nodes).GetNodes(ledger-key, replication_factor)`

This gives:
- stable affinity for the same workspace/client pair
- minimal churn on topology changes
- reuse of existing sharding behavior in MonoFS

#### 4. Write and query flows

Write path (`PushOutcome`, `LocalCommit`, `RefreshEvent`):
1. Client calls router API (unchanged).
2. Router extracts `client_id` from metadata.
3. Router computes `ledger-key` and owner with HRW.
4. Router forwards append RPC to owner storage node.
5. Storage node appends to WAL and updates in-memory indexes.
6. Router relays status to client.

Read path (`QueryLedger`):
1. Router computes same owner using workspace + client ID.
2. Router forwards query to owner node.
3. Router returns node response directly.

Fallback behavior:
- if owner unavailable, route to next HRW candidate
- optionally fan out to top-K owners for reads when recovery is in progress

#### 5. Fault tolerance and recovery

Minimum mode:
- single owner per key
- local WAL + snapshot replay on owner restart

Recommended mode:
- replicate append to top-2 HRW nodes
- ack policy configurable (`owner-only` or `owner+1`)
- read repair from replica when owner is down

#### 6. API surface changes

`MonoFS` service (storage node) gains ledger APIs:
- `AppendLedgerEvents(AppendLedgerEventsRequest) returns (AppendLedgerEventsResponse)`
- `QueryLedger(QueryLedgerRequest) returns (QueryLedgerResponse)`

`MonoFSRouter` service keeps existing `QueryLedger` API for clients, but implementation becomes proxy-only.

### Invariants
- Router must not append WAL locally for ledger data.
- For a given `(workspace_id, client_id)` and topology version, owner selection is deterministic.
- Every accepted append is durable on at least one storage node before ACK.
- Query routing uses the same HRW key derivation as write routing.

## Implementation plan

1. Introduce storage-node ledger service
- Add WAL-backed ledger package under `internal/server/ledgerstore/`.
- Reuse WAL segmenting + compaction behavior from current workspace WAL approach.

2. Extend protobuf and generated stubs
- Add ledger RPCs to `MonoFS` in `api/proto/monofs.proto`.
- Regenerate protobuf bindings.

3. Implement node handlers
- Add `AppendLedgerEvents` and `QueryLedger` handlers in `internal/server/`.
- Wire to new `ledgerstore`.

4. Convert router ledger path to proxy
- Remove router-local WAL coupling for ledger (`internal/router/workspace_ledger.go` and initialization in `internal/router/router.go`).
- In router handlers, resolve `client_id`, compute HRW owner, forward RPC to storage node client.

5. Routing key and helper
- Add shared helper in `internal/sharding/` or router package for `ledger-key` derivation:
  - empty client ID fallback: `"anonymous"`
  - required format stability tests

6. Migration from router WAL to node WAL
- Add one-shot export from router WAL snapshots.
- Import records by recomputing owner key:
  - historical records without explicit client ID use `principal_id` when present
  - otherwise use `"legacy"`
- Run dual-write (router WAL + node WAL) behind feature flag for one release.
- Cut over to proxy-only, then remove router ledger WAL writes.

7. Rollout flags
- `--ledger-proxy-mode=router|dual|storage` (default `router` for safe rollout)
- `--ledger-replication-factor` (default `1`)
- `--ledger-ack-policy=owner-only|quorum`

8. Validation
- Consistency tests: owner determinism under node membership changes.
- Crash tests: node restart replays WAL correctly.
- End-to-end tests: router restart does not affect ledger durability.
- Latency tests: p50/p95 impact of proxy hop stays within target.

## Test matrix

- Determinism:
  - same `(workspace_id, client_id)` maps to same owner across calls
  - minimal remap when adding/removing one node

- Durability:
  - append acknowledged then node restart preserves record
  - truncated last WAL line is discarded safely on replay

- Proxy behavior:
  - router restart with no local state still serves ledger queries
  - owner down -> next candidate fallback works

- Migration:
  - exported router WAL imported without data loss
  - dual-write comparison reports zero divergence before cutover

## Operational notes

- Router disk sizing no longer driven by ledger WAL retention.
- Storage node disk alerts must include `ledger/wal` growth.
- Backups and snapshot retention policy move to storage node fleet policy.

## Out of scope

- Full deprecation of router durable job orchestration metadata.
- Cross-region replication protocol for ledger shards.
- Multi-tenant encryption key separation for ledger records.
