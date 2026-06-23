# MonoFS

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Distributed source workspace, code search, and publish engine for monolithic repositories.**

MonoFS projects only the subset of a monorepo you need through a high-performance FUSE mount, while storage, indexing, and ingestion live on a horizontally scalable backend. Instead of cloning the world to your laptop, you mount a virtual monorepo, edit code like it is local, and publish through an explicit `monofs-session commit`.

[Architecture](docs/architecture.md) · [Usage Guide](docs/usage.md) · [Docs Index](docs/README.md) · [Session CLI](cmd/monofs-session/README.md)

---

## The Problem

As engineering organizations scale, monolithic repositories become a bottleneck:

- `git clone` takes long enough to brew coffee
- IDE indexers choke on millions of files
- Distributed builds become an operational nightmare
- Source-of-truth for code is split across Git, S3, lockfile caches, and deployment manifests

MonoFS flips the standard source-control model. It is not just a FUSE daemon — it is a router-led distributed workspace platform that pushes the heavy lifting (storage, indexing, fetching, publishing) to an immutable, scalable backend.

## The Strata Ecosystem

MonoFS is the foundational storage and workspace layer of the **Strata Platform**:

- **[Guardian](https://github.com/radryc/guardian):** intent-based deployment engine ("City Builder" architecture: Pushers, Builders, Blueprints) that orchestrates state across **Customer Accounts**.
- **[Doctor](https://github.com/radryc/doctor):** enterprise observability layer with cross-account telemetry.

The router natively exposes `doctor/` and `guardian/` namespaces so the integrated stack is visible from a single mount, a single UI, and a single gRPC API.

## Components at a Glance

| Component             | Role                                                       |
|-----------------------|------------------------------------------------------------|
| **Router**            | Control plane: topology, health, publish/refresh jobs, UI  |
| **Storage Nodes**     | Sharded WORM data plane; HRW-placed, replicated            |
| **Fetcher Tier**      | Stateless bridge to upstream Git, S3, GCS, MinIO           |
| **Search Service**    | Out-of-band Zoekt indexing (no read-path impact)           |
| **FUSE Client**       | Local mount with writable overlay and session socket       |
| **Session CLI**       | `monofs-session`: status, diff, commit, pull, push, search |
| **Admin CLI**         | `monofs-admin`: ingest, failover, drain, rebalance         |

For a full architectural deep dive, see [docs/architecture.md](docs/architecture.md).

![MonoFS architecture overview](docs/assets/architecture-overview.svg)

## Quick Start

A typical first-time flow:

```bash
# 1. Bring up the shared stack and release the partitions you need
mt-bootstrap deploy
mt-bootstrap stamp-urls
mt-release --partition doctor
mt-release --partition dev-workspace

# 2. Make the router reachable from your workstation
mt-bootstrap port-forward    # router on :9090, UI on :8080

# 3. Ingest the repos you want to work on
./bin/monofs-admin ingest --router=localhost:9090 \
  --source=git@github.com:acme/service-a.git --ref=main
./bin/monofs-admin ingest --router=localhost:9090 \
  --source=git@github.com:acme/shared-lib.git --ref=main

# 4. Mount a virtual monorepo
mkdir -p /tmp/monofs-dev /tmp/monofs-overlay
./bin/monofs-client \
  --mount=/tmp/monofs-dev \
  --router=localhost:9090 \
  --use-external-addrs \
  --virtual-monorepo \
  --writable \
  --overlay=/tmp/monofs-overlay

# 5. Develop, then publish
cd /tmp/monofs-dev
git status
./bin/monofs-session status
./bin/monofs-session commit -m "Update service-a to new shared client"
./bin/monofs-session pull
```

For the full daily workflow, flag reference, troubleshooting, and publish/refresh details, see [docs/usage.md](docs/usage.md).

## Observability

Every service exposes Prometheus `/metrics` and `net/http/pprof` endpoints. The router UI bundles them in a **Performance** tab that collects profiles from all services in a single zip.

| Service         | Default diagnostics address          |
|-----------------|--------------------------------------|
| `monofs-router` | `:8080/debug/pprof/`, `:8080/metrics` |
| `monofs-server` | `--metrics-addr` (default `:9100`)   |
| `monofs-search` | `--diagnostics-addr` (default `:9101`) |
| `monofs-fetcher`| `--diagnostics-addr` (default `:9201`) |

UI API: `POST /api/pprof/collect`. Default profiles: `cpu` (30s), `heap`, `goroutine`. Optional: `allocs`, `mutex`, `block`, `threadcreate`, `trace`.

## Documentation

- [docs/architecture.md](docs/architecture.md) — system design, components, data flows
- [docs/usage.md](docs/usage.md) — daily developer workflow and CLI reference
- [cmd/monofs-session/README.md](cmd/monofs-session/README.md) — `monofs-session` command reference
- [docs/native-protocol-v1.md](docs/native-protocol-v1.md) — experimental native VFS protocol (internal)

## License

Apache 2.0 — see [LICENSE](LICENSE).
