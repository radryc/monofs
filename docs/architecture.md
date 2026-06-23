# MonoFS Architecture

> Distributed source workspaces, code search, and publish engine for monolithic repositories.

This document explains **how MonoFS works**. For day-to-day commands and developer workflows, see [usage.md](usage.md). For the Strata platform context, see the top-level [README](../README.md).

![MonoFS architecture overview](assets/architecture-overview.svg)

---

## Table of Contents

1. [What MonoFS Is](#1-what-monofs-is)
2. [The Big Picture](#2-the-big-picture)
3. [Components](#3-components)
   - [Router (Control Plane)](#router-control-plane)
   - [Storage Nodes (Data Plane)](#storage-nodes-data-plane)
   - [Fetcher Tier (Upstream Bridge)](#fetcher-tier-upstream-bridge)
   - [Search Service](#search-service)
   - [FUSE Client](#fuse-client)
   - [Admin CLI and Session CLI](#admin-cli-and-session-cli)
4. [Data Model](#4-data-model)
   - [Namespaces](#namespaces)
   - [Storage IDs and Shards](#storage-ids-and-shards)
   - [WORM / Indexed-Tail Format](#worm--indexed-tail-format)
5. [Request Flows](#5-request-flows)
   - [Read Path](#read-path)
   - [Write Path (Overlay + Publish)](#write-path-overlay--publish)
   - [Refresh Path](#refresh-path)
6. [Replication, Failover, and Rebalancing](#6-replication-failover-and-rebalancing)
7. [Native Protocol (Experimental)](#7-native-protocol-experimental)
8. [Built-in Namespaces: Doctor and Guardian](#8-built-in-namespaces-doctor-and-guardian)
9. [Observability](#9-observability)
10. [Where to Read the Code](#10-where-to-read-the-code)

---

## 1. What MonoFS Is

MonoFS solves a single problem: **make a giant monorepo feel local**.

Instead of `git clone`-ing the world onto every laptop and watching IDE indexers choke, MonoFS projects exactly the subset of files you need through a FUSE mount. Storage, indexing, and ingestion live on a horizontally scalable backend. The local machine only ever holds the files it is actively touching.

The system is composed of five small services and two CLI tools:

| Service          | Binary              | Role                                      |
|------------------|---------------------|-------------------------------------------|
| Router           | `monofs-router`     | Control plane, UI, topology, publish jobs |
| Storage Node     | `monofs-server`     | Sharded WORM backend, file serving        |
| Fetcher          | `monofs-fetcher`    | Upstream Git/blob bridge                  |
| Search           | `monofs-search`     | Out-of-band code indexing                 |
| FUSE Client      | `monofs-client`     | Local mount (developer machine)           |
| Session CLI      | `monofs-session`    | Inspect/publish/refresh (developer)       |
| Admin CLI        | `monofs-admin`      | Operator tasks (ingest, failover, etc.)   |
| Loadtest         | `monofs-loadtest`   | Filesystem load generator                 |
| Trace Dump       | `monofs-trace-dump` | Log engine query tool                     |

---

## 2. The Big Picture

```
┌─────────────────────────────┐
│   Developer Machine         │
│                             │
│   ┌─────────────────────┐   │
│   │   FUSE Client       │◀──┐
│   │   (monofs-client)   │   │  gRPC (control + data)
│   └─────────────────────┘   │
│            ▲                │
│   ┌────────┴────────┐       │   gRPC (session socket)
│   │ Session CLI     │       │
│   │ (monofs-session)│───────┘
│   └─────────────────┘
└────────────┬────────────────┘
             │
             ▼
┌────────────────────────────────────────────────────┐
│                MONOFS CLUSTER                      │
│                                                    │
│   ┌──────────────┐                                 │
│   │   Router     │  control plane + UI (HTTP/8080) │
│   │              │  gRPC API (9090)                │
│   └──────┬───────┘                                 │
│          │                                         │
│          ├──▶ ┌──────────────┐  ←─ upstream Git    │
│          │    │  Fetcher(s)  │     / object store  │
│          │    └──────────────┘                     │
│          │                                         │
│          ├──▶ ┌──────────────┐                     │
│          │    │   Search     │  (out-of-band)      │
│          │    └──────────────┘                     │
│          │                                         │
│          ▼                                         │
│   ┌──────────────────────────────┐                 │
│   │   Storage Nodes (sharded)    │                 │
│   │   • Indexed-Tail WORM        │                 │
│   │   • HRW-sharded by path      │                 │
│   │   • Replicated (factor N)    │                 │
│   └──────────────────────────────┘                 │
└────────────────────────────────────────────────────┘
```

A user mounts a directory and edits files like they are local. Reads stream from the cluster; writes are captured in a local overlay and explicitly published upstream.

---

## 3. Components

### Router (Control Plane)

**Binary:** `monofs-router`
**Source:** `cmd/monofs-router/main.go`, `internal/router/`

The router is stateless from the data perspective, but owns the **cluster view**:

- Tracks healthy storage nodes via periodic health checks
- Computes HRW (Rendezvous / Highest Random Weight) shard placement for paths
- Serves cluster topology to FUSE clients (`--use-external-addrs`)
- Drives publish and refresh jobs through the fetcher tier
- Hosts the web UI on `:8080` (cluster status, workspace jobs, pprof collection)
- Hosts the gRPC API on `:9090` (ingest, status, failover, delete, drain, …)
- Exposes the built-in `doctor/` and `guardian/` namespaces as virtual root entries

Key files:
- `internal/router/router.go` — gRPC service, health, topology
- `internal/router/ingest.go` — repository and blob ingestion
- `internal/router/workspace_sync.go` — publish and refresh orchestration
- `internal/router/native_gateway.go` — experimental native-protocol gateway
- `internal/router/ui.go` — embedded web UI

### Storage Nodes (Data Plane)

**Binary:** `monofs-server`
**Source:** `cmd/monofs-server/main.go`, `internal/server/`, `internal/storage/`

Storage nodes hold the actual file data. Each node:

- Serves a gRPC API for `lookup`, `getattr`, `readdir`, `read`, `statfs`, …
- Stores content in the **Indexed-Tail** WORM format
- Republishes via the [WORM / Indexed-Tail Format](#worm--indexed-tail-format) below
- Reports health and capacity metrics to the router
- Runs the **ingest** path for new repository data
- Maintains replica state for paths it is a backup of

Key files:
- `internal/server/fs.go`, `internal/server/directory.go` — filesystem semantics
- `internal/server/replication.go` — replica sync
- `internal/server/failover.go` — drain, drain, takeover
- `internal/storage/logengine/` — Indexed-Tail engine
- `internal/storage/blob/`, `internal/storage/git/` — pluggable backends

### Fetcher Tier (Upstream Bridge)

**Binary:** `monofs-fetcher`
**Source:** `cmd/monofs-fetcher/main.go`, `internal/fetcher/`

Fetchers are stateless proxies that have **outbound network access**. They:

- Clone Git remotes and stage workspace bundles
- Push/pull from upstream for the publish and refresh flows
- Cache blobs locally for re-use across nodes
- Prefetch queue to keep popular content warm
- Run in the DMZ; storage nodes stay on the internal network only

The fetcher speaks to storage nodes for staged bundles and to the upstream world (Git, S3, GCS, MinIO) for source data.

Key files:
- `internal/fetcher/service.go` — gRPC service
- `internal/fetcher/workspace_publish.go` — `commit` / `pull` jobs
- `internal/fetcher/backend.go` — multi-backend dispatch

### Search Service

**Binary:** `monofs-search`
**Source:** `cmd/monofs-search/main.go`, `internal/search/`

Search runs **out of band** so heavy indexing never impacts FUSE read latency:

- Builds Zoekt indexes on a worker pool
- Re-clones repositories from the cluster as needed
- Serves regex and literal queries over the index
- Exposes a CLI proxy through `monofs-session search`

The router’s UI calls into the search service for the Code Search tab.

### FUSE Client

**Binary:** `monofs-client`
**Source:** `cmd/monofs-client/main.go`, `internal/fuse/`, `internal/client/`

The client is the developer’s local interface. It:

- Mounts the projected workspace as a normal POSIX filesystem
- Resolves cluster topology from the router on every operation
- Fans directory listings out to all healthy nodes and merges the result
- Streams file data directly from the storage node that owns the path
- (Optional) hosts a writable overlay that captures local edits
- (Optional) exposes a session Unix socket for `monofs-session`

Key files:
- `cmd/monofs-client/main.go` — flags, mount setup, overlay
- `internal/fuse/` — `lookup`, `readdir`, `read`, `write`, `commit`, …
- `internal/client/sharded.go` — HRW-aware gRPC fan-out
- `internal/client/workspace.go` — workspace bundle assembly
- `internal/fuse/session_socket.go` — Unix socket RPC

### Admin CLI and Session CLI

**Binaries:** `monofs-admin`, `monofs-session`

- `monofs-admin` is the **operator** tool: ingest, status, delete, failover, rebalance, drain, rebuild-index, repos, stats, node-files, fetchers.
- `monofs-session` is the **developer** tool: status, diff, commit, pull, push, discard, search. It runs against a mounted client’s session socket.

Full flag reference: see [usage.md](usage.md).

---

## 4. Data Model

### Namespaces

A *namespace* is a path tree under the mount root. MonoFS exposes a few native namespaces plus everything you ingest:

| Path prefix                | Source                       | Notes                                              |
|----------------------------|------------------------------|----------------------------------------------------|
| `/<git-source-path>/...`   | Ingested Git repository      | Virtual paths preserved from `monofs-admin ingest` |
| `/blob/...`                | Packager blob archive        | Content-addressed uploads                          |
| `/s3/...`                  | Ingested S3 prefix           | Read-through view                                  |
| `/dependency/...`          | Developer-pushed blobs       | Build caches, lockfiles, generated artifacts       |
| `/doctor/...`              | Doctor observability view    | Built-in cross-account telemetry                   |
| `/guardian/...`            | Guardian deployment view     | Tenant-scoped deployment tree                      |
| `/guardian-system/...`     | Guardian system view         | Internal control-plane state                       |
| `/`.monofs/...`            | Internal metadata            | Hidden from `virtual-monorepo` mounts              |

A full namespace tree (the “raw” view) is what the router UI shows. A **virtual-monorepo** mount projects a source-first view that hides `doctor`, `guardian`, `guardian-system`, and nested `.git` directories, synthesizes a root `.git` and `.gitignore`, and keeps `dependency/` visible.

See [usage.md — What the Projected Root Looks Like](usage.md#what-the-projected-root-looks-like) for the exact shape of a virtual-monorepo mount.

### Storage IDs and Shards

When a repository is ingested, MonoFS assigns it a stable **storage ID**. Paths are placed on storage nodes using **HRW (Rendezvous) hashing** over `(cluster_version, path)`:

- Adding or removing a node only reshuffles a small fraction of paths.
- The router can compute placement deterministically for any path given the current healthy node set.
- The client receives placement hints from the router and streams reads directly to the owning node.

Implementation: `internal/sharding/hrw.go`.

### WORM / Indexed-Tail Format

Storage nodes persist data using a **Write-Once, Read-Many** Indexed-Tail format. Each commit appends a small immutable "tail" record (file create/modify/delete, directory mutation, symlink op) and a new index entry. Reads walk the index and resolve the latest tail entry for a path.

Properties:

- **Append-only** — no in-place rewrites, so replication and backups are simple
- **Densely packed** — files in a commit are compressed and encrypted as a group, so a directory of small files costs about one object read
- **HRW-placed** — each commit lands on a small, deterministic set of storage nodes
- **Failable mid-append** — a partial commit is detectable and ignorable; readers fall back to the previous valid commit

The engine is in `internal/storage/logengine/`. Blobs are stored via pluggable backends in `internal/storage/blob/` and `internal/storage/git/`.

---

## 5. Request Flows

### Read Path

```
App / IDE / shell
      │
      ▼
FUSE VFS (kernel) ─── page cache
      │
      ▼
monofs-client
  • looks up node for path (HRW + router hint)
  • issues gRPC Read / ReadDir / GetAttr
      │
      ▼
monofs-server
  • resolves latest tail for path
  • returns data (streamed)
      │
      ▼
back to FUSE → app
```

A `ReadDir` fans out to all healthy nodes because directories can be sharded across them; results are merged in the client. This merge is **authoritative** — the client never returns a partial listing. If any required node is unavailable, the operation fails fast and the client retries.

See `internal/client/sharded.go` for the merge implementation.

### Write Path (Overlay + Publish)

Writes never go directly to the storage backend. They go to a local **overlay** attached to the mount.

```
App writes file
      │
      ▼
FUSE VFS
      │
      ▼
monofs-client (writable)
  • stores delta in overlay db (--overlay=...)
  • serves subsequent reads from overlay on top of remote data
      │
      ▼ (user runs `monofs-session commit`)
monofs-session
  • collects overlay changes
  • builds a workspace bundle (file ops, deletes, dirs, symlinks)
  • uploads bundle to router
      │
      ▼
monofs-router
  • picks a fetcher for the workspace shard
  • hands off the bundle
      │
      ▼
monofs-fetcher
  • checks out upstream repos
  • applies bundle
  • commits + pushes upstream
  • reports back to router
      │
      ▼
monofs-session
  • archives overlay on success, keeps it on failure
```

The session remains active until publish succeeds, so you can inspect, retry, or `discard`.

### Refresh Path

`monofs-session pull` brings upstream changes back into the mount:

1. Client sends its current refs and base commits to the router.
2. Router asks a fetcher to check upstream state.
3. Changed repositories are re-ingested into the cluster.
4. The mount and the synthetic root Git baseline are updated together.

For `direct` branch strategy, a successful `commit` triggers an automatic refresh.

---

## 6. Replication, Failover, and Rebalancing

Replication is configured at the router (`--replication-factor`, default `2`):

- The router computes the **top N HRW nodes** for each path. The first is the **primary**; the rest are **replicas**.
- Writes (WORM appends) are sent to all replicas in parallel and acked once a quorum writes.
- If a primary is unhealthy, reads failover to the next replica.
- After `--rebalance-delay` of permanent failure, the router re-replicates the orphaned paths to a new healthy node.

Operations to control this:

| Command                          | What it does                                  |
|----------------------------------|-----------------------------------------------|
| `monofs-admin status`            | Show cluster state                            |
| `monofs-admin failover`          | List / inspect failover regions               |
| `monofs-admin trigger-failover`  | Inject a planned failover for a region        |
| `monofs-admin clear-failover`    | Resume normal operation                      |
| `monofs-admin drain`             | Drain a node for maintenance                 |
| `monofs-admin undrain`           | Restore a drained node                       |
| `monofs-admin rebalance`         | Trigger shard rebalancing                    |
| `monofs-admin node-files`        | Inspect files owned by a node                |
| `monofs-admin rebuild-index`     | Rebuild directory index for a storage ID     |

See `internal/router/drain.go` and `internal/server/failover.go` for implementation.

---

## 7. Native Protocol (Experimental)

There is a new experimental native VFS protocol designed to eventually replace the FUSE client’s lower and read path. It is implemented as a small, framed binary protocol over TCP that keeps namespace aggregation, HRW failover, and topology handling in the gateway rather than the kernel.

This is **internal and not part of the public ABI**. The full spec lives in [native-protocol-v1.md](native-protocol-v1.md). The Go gateway implementation is in `internal/router/native_gateway.go`.

---

## 8. Built-in Namespaces: Doctor and Guardian

MonoFS is the foundational storage and workspace layer of the **Strata Platform**. The router natively exposes:

- **`/doctor/...`** — cross-account observability data from [Doctor](https://github.com/radryc/doctor)
- **`/guardian/...`** — tenant-scoped deployment data from [Guardian](https://github.com/radryc/guardian)
- **`/guardian-system/...`** — Guardian control-plane state

These namespaces are first-class in the mount, the UI, and the gRPC API. They are hidden in `--virtual-monorepo` mounts so that source development sees a clean repo tree.

See `internal/router/guardian_paths.go` and `internal/router/guardian_path_mapper.go`.

---

## 9. Observability

Every service ships with a Prometheus `/metrics` endpoint and `net/http/pprof` debug endpoints. The router UI bundles all of them into a **Performance** tab.

| Service         | Default diagnostics address |
|-----------------|-----------------------------|
| `monofs-router` | `:8080/debug/pprof/`, `:8080/metrics` |
| `monofs-server` | `--metrics-addr` (default `:9100`) |
| `monofs-search` | `--diagnostics-addr` (default `:9101`) |
| `monofs-fetcher`| `--diagnostics-addr` (default `:9201`) |

The UI calls `POST /api/pprof/collect` to grab a zip of named profiles from all services in one shot. Default profiles: `cpu` (30s), `heap`, `goroutine`. Optional: `allocs`, `mutex`, `block`, `threadcreate`, `trace`.

For log queries, the `monofs-trace-dump` tool exposes a CLI for the storage node’s log engine.

---

## 10. Where to Read the Code

| Concern               | Start here                                                  |
|-----------------------|-------------------------------------------------------------|
| gRPC API              | `api/proto/`                                                |
| Router service        | `internal/router/router.go`                                 |
| Workspace publish     | `internal/router/workspace_publish.go`, `internal/router/workspace_sync.go` |
| Workspace refresh     | `internal/router/workspace_sync.go`                         |
| Fetcher jobs          | `internal/fetcher/workspace_publish.go`                     |
| Client gRPC fan-out   | `internal/client/sharded.go`                                |
| Client identity       | `internal/client/identity.go`                               |
| FUSE ops              | `internal/fuse/op_*.go`                                     |
| Writable overlay      | `internal/fuse/writable.go`, `internal/fuse/overlay.go`     |
| Session socket RPC    | `internal/fuse/session_socket.go`                           |
| Sharding / HRW        | `internal/sharding/hrw.go`                                  |
| Storage engine        | `internal/storage/logengine/engine.go`                      |
| Blob backends         | `internal/storage/blob/backend.go`, `internal/storage/git/` |
| Search                | `internal/search/service.go`                                |
| Native gateway        | `internal/router/native_gateway.go`                         |
| Guardian namespaces   | `internal/router/guardian_paths.go`                         |

For day-to-day usage, jump to [usage.md](usage.md).
