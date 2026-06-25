# MonoFS

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Distributed source workspace, code search, and publish engine for monolithic repositories.**

MonoFS is a FUSE-based virtual filesystem that projects the subset of a monorepo you actually need onto your local machine. Instead of `git clone`-ing the world, you mount a virtual monorepo, edit code like it's local, and publish through an explicit `monofs-session commit`. All storage, indexing, and ingestion are pushed to a horizontally scalable backend.

[Architecture](docs/architecture.md) · [Usage Guide](docs/usage.md) · [Docs Index](docs/README.md) · [Session CLI Reference](cmd/monofs-session/README.md)

---

## The Problem

As engineering organizations scale, monolithic repositories become a bottleneck:

- `git clone` takes long enough to brew coffee
- IDE indexers choke on millions of files
- Distributed builds become an operational nightmare
- Source-of-truth for code is split across Git, S3, lockfile caches, and deployment manifests

MonoFS flips the standard source-control model. It is not just a FUSE daemon — it is a **router-led distributed workspace platform** that pushes the heavy lifting (storage, indexing, fetching, publishing) to an immutable, scalable backend.

---

## Architecture

```
┌─────────────────────────────┐
│   Developer Machine         │
│                             │
│   ┌─────────────────────┐   │
│   │   FUSE Client       │◀──┐  gRPC (topology + data)
│   │   (monofs-client)   │   │
│   └─────────────────────┘   │
│            ▲                │
│   ┌────────┴────────┐       │  gRPC (session socket)
│   │ Session CLI     │       │
│   │ (monofs-session)│───────┘
│   └─────────────────┘
└────────────┬────────────────┘
             │
             ▼
┌──────────────────────────────────────────────────┐
│              MONOFS CLUSTER                      │
│                                                  │
│   ┌──────────────┐                               │
│   │   Router     │  control plane + UI (:8080)   │
│   │              │  gRPC API (:9090)             │
│   └──────┬───────┘                               │
│          │                                       │
│          ├──▶ ┌──────────────┐  upstream Git     │
│          │    │  Fetcher(s)  │  / S3 / GCS       │
│          │    └──────────────┘                   │
│          │                                       │
│          ├──▶ ┌──────────────┐                   │
│          │    │   Search     │  (out-of-band)    │
│          │    └──────────────┘                   │
│          │                                       │
│          ▼                                       │
│   ┌──────────────────────────────┐               │
│   │   Storage Nodes (sharded)    │               │
│   │   • Indexed-Tail WORM        │               │
│   │   • HRW-sharded by path      │               │
│   │   • Replicated (factor N)    │               │
│   └──────────────────────────────┘               │
└──────────────────────────────────────────────────┘
```

A developer mounts a directory and edits files like they are local. Reads stream from the cluster; writes are captured in a local overlay and explicitly published upstream through the router → fetcher → upstream Git path.

For a full architectural deep dive, see [docs/architecture.md](docs/architecture.md).

---

## Components

| Binary               | Role                                                        |
|----------------------|-------------------------------------------------------------|
| `monofs-router`      | Control plane: topology, health, ingest, publish/refresh jobs, web UI |
| `monofs-server`      | Storage node: HRW-sharded WORM data plane, file serving, replication |
| `monofs-fetcher`     | Stateless DMZ bridge to upstream Git, S3, GCS, MinIO       |
| `monofs-search`      | Out-of-band Zoekt code indexing and query                   |
| `monofs-client`      | FUSE mount: local filesystem with writable overlay and session socket |
| `monofs-session`     | Developer CLI: status, diff, commit, pull, push, search, discard |
| `monofs-admin`       | Operator CLI: ingest, failover, drain, rebalance, rebuild-index |
| `monofs-loadtest`    | Filesystem load generator                                   |
| `monofs-trace-dump`  | Storage log engine query tool                               |

Plus: a [VS Code extension](vscode-monofs/) for build/deploy/status commands, and an experimental [kernel module](monofs-kmod/) for a native VFS path.

---

## Key Features

### Virtual Monorepo Projection

`--virtual-monorepo` mode projects a source-first root: it hides internal namespaces (`doctor/`, `guardian/`, `guardian-system/`), strips nested `.git` directories, and synthesizes a root `.git` and `.gitignore`. The result is a clean workspace where `git status`, `git diff`, IDE indexers, and `ripgrep` all work as expected against the mounted tree.

### Writable Overlay + Explicit Publish

All writes are captured in a local overlay **outside the mount point**. Nothing touches remote storage directly. When you're ready to publish:

```
monofs-session commit -m "your message"
  → overlay changes collected into a workspace bundle
  → bundle uploaded to the router
  → router dispatches to the fetcher
  → fetcher applies the bundle to upstream Git (clone, apply, commit, push)
  → session archived on success, kept active on failure for inspection/retry
```

Branch strategies: `direct` (push to tracked branch), `workspace_branch`, or `per_repo_branch`.

### Workspace Refresh

`monofs-session pull` brings upstream changes back into the mount: the client sends its current refs → router checks upstream via fetcher → changed repos are re-ingested → mount and synthetic Git baseline are updated together.

### HRW-Sharded WORM Storage

Data is placed on storage nodes using **Rendezvous (HRW) consistent hashing** over `(cluster_version, path)`. Adding or removing a node reshuffles only a fraction of paths. Storage uses an append-only **Indexed-Tail WORM** format: densely packed, compressed, encrypted per commit, with mid-append safety (partial commits are detectable; readers fall back to the last valid commit).

### Replication, Failover, and Rebalancing

Configurable replication factor (default 2). The router computes the top-N HRW nodes for each path (primary + replicas). Writes go to all replicas in parallel; reads failover automatically on primary health failure. After a permanent failure and `--rebalance-delay`, the router re-replicates orphaned paths to healthy nodes. Full operator control via `monofs-admin drain`, `undrain`, `trigger-failover`, `clear-failover`, and `rebalance`.

### Out-of-Band Code Search

Zoekt-based indexing runs on a worker pool completely separate from the read path — zero impact on FUSE latency. Supports regex, literal queries, and file pattern filtering. Accessible via `monofs-session search` and the router UI's Code Search tab.

### Encryption at Rest

All packager archives are encrypted with **ChaCha20-Poly1305**. The encryption key (32 bytes) must be consistent across all services and is required for Docker deployments.

### Observability

Every service exposes Prometheus `/metrics` and `net/http/pprof` endpoints. The router UI's **Performance** tab collects profiles from all services into a single zip (`POST /api/pprof/collect`). Default profiles: CPU (30s), heap, goroutine. Optional: allocs, mutex, block, threadcreate, trace.

### Dual Addressing

Storage nodes have both internal pod-network addresses and optional external host-reachable addresses. The router advertises the right set per client, enabling cluster-internal routing and WSL/Docker host access simultaneously.

### Multi-Backend Fetcher

Fetchers support Git, S3 (MinIO), GCS, and local/blob backends — configurable per source via JSON. The fetcher tier runs in the DMZ; storage nodes stay on the internal network.

---

## The Strata Ecosystem

MonoFS is the foundational storage and workspace layer of the **Strata Platform**:

- **[Guardian](https://github.com/radryc/guardian):** intent-based deployment engine ("City Builder" architecture: Pushers, Builders, Blueprints) that orchestrates state across Customer Accounts.
- **[Doctor](https://github.com/radryc/doctor):** enterprise observability layer with cross-account telemetry.

The router natively exposes `doctor/` and `guardian/` namespaces so the integrated stack is visible from a single mount, a single UI, and a single gRPC API.

---

## Quick Start

### 1. Bring up the cluster and ingest repositories

```bash
# Start the shared stack and release the partitions you need
mt-bootstrap deploy
mt-bootstrap stamp-urls
mt-release --partition doctor
mt-release --partition dev-workspace

# Make the router reachable from your workstation
mt-bootstrap port-forward    # router on :9090, UI on :8080

# Ingest repos you want to work on
./bin/monofs-admin ingest --router=localhost:9090 \
  --source=git@github.com:acme/service-a.git --ref=main
./bin/monofs-admin ingest --router=localhost:9090 \
  --source=git@github.com:acme/shared-lib.git --ref=main
```

### 2. Mount a writable virtual monorepo

```bash
mkdir -p /tmp/monofs-dev /tmp/monofs-overlay

./bin/monofs-client \
  --mount=/tmp/monofs-dev \
  --router=localhost:9090 \
  --use-external-addrs \
  --virtual-monorepo \
  --writable \
  --overlay=/tmp/monofs-overlay
```

### 3. Develop, then publish

```bash
cd /tmp/monofs-dev
git status
git diff
go test ./github.com/acme/service-a/...

# Inspect pending changes
./bin/monofs-session status
./bin/monofs-session diff

# Publish source changes upstream
./bin/monofs-session commit -m "Update service-a to new shared client"

# Pick up upstream changes
./bin/monofs-session pull
```

For the full daily workflow, flag reference, troubleshooting, and publish/refresh details, see [docs/usage.md](docs/usage.md).

---

## Documentation

| Document                                              | What it covers                                    |
|-------------------------------------------------------|---------------------------------------------------|
| [docs/architecture.md](docs/architecture.md)          | System design, components, data flows, code map   |
| [docs/usage.md](docs/usage.md)                        | Daily developer workflow and CLI reference        |
| [cmd/monofs-session/README.md](cmd/monofs-session/README.md) | `monofs-session` command reference               |
| [docs/native-protocol-v1.md](docs/native-protocol-v1.md) | Experimental native VFS protocol (internal)      |

---

## Observability

| Service          | Default diagnostics address          |
|------------------|--------------------------------------|
| `monofs-router`  | `:8080/debug/pprof/`, `:8080/metrics` |
| `monofs-server`  | `--metrics-addr` (default `:9100`)   |
| `monofs-search`  | `--diagnostics-addr` (default `:9101`) |
| `monofs-fetcher` | `--diagnostics-addr` (default `:9201`) |

UI API: `POST /api/pprof/collect`. Default profiles: `cpu` (30s), `heap`, `goroutine`. Optional: `allocs`, `mutex`, `block`, `threadcreate`, `trace`.

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
