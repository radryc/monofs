# MonoFS

> Part of the **Strata** platform.

MonoFS is a distributed filesystem that gives teams a monorepo-style workspace
without forcing them into a single Git repository. It ingests repositories into
a cluster, mounts them as one coherent writable workspace, and brings source
code, Guardian-managed config, and Doctor telemetry together under one tree.

See [`docs/architecture.md`](docs/architecture.md) for the full system
architecture.

## What it is

MonoFS is a distributed system, not a mount script:

| Component | Binary | Purpose | Ports |
| --- | --- | --- | --- |
| Router | `monofs-router` | cluster coordination, ingestion, workspace sync, Guardian/Doctor namespaces, UI/API | gRPC `:9090`, HTTP `:8080` |
| Storage node | `monofs-server` | sharded metadata + content, directory indexes, optional logengine/KVS | gRPC `:9000` |
| Fetcher | `monofs-fetcher` | blob/git retrieval, archive cache (local + S3/GCS), publish/push execution | gRPC `:9200` |
| Search | `monofs-search` | Zoekt full-text + `sym:` symbol search | gRPC `:9100` |
| FUSE client | `monofs-client` | mounts the workspace with overlays and session VCS | session socket |
| Session CLI | `monofs-session` | status/diff/commit/branch/push/pull over the session socket | — |
| Admin CLI | `monofs-admin` | ingest, rebalance, failover, drain, whitelist, stats | — |
| OCI registry | `monofs-registry` | Docker Registry v2 backed by MonoFS, pull-through proxy | HTTP `:5000` |
| Bazel cache | `monofs-cache` | Bazel remote cache (AC + CAS) | HTTP `:9092` |
| Pipeline worker | `monofs-pipeline-worker` | claims CI tasks from Guardian queues and runs them | executor HTTP |
| Bazel workspace | `monofs-bazelctl` | generates/manages a Bazel workspace over the mount | — |

## Feature highlights

- **Virtual monorepo** over independent Git repos, with upstream publish/pull.
- **Sparse workspaces** via `--include`/`--exclude` display-path globs.
- **Ref pinning** at ingest: branch, tag, commit SHA, or `module@version`.
- **Review workflow**: OWNERS/CODEOWNERS subtree ownership, automatic PRs/MRs
  for non-direct pushes, native merge-request store.
- **Code intelligence**: full-text and `sym:` symbol search (universal-ctags).
- **Auto-refresh**: poll- and webhook-driven re-ingestion of upstreams.
- **Session VCS**: local virtual commits, logical branches, 3-way pull merges
  with conflicts, squash/preserve push modes, upstream `log`/`tags`/`blame`.
- **CI/CD pipelines** with monorepo-aware affected-builds and pluggable
  executor (builder/docker/deployer/bazel workers).
- **Managed namespaces**: `guardian/<partition>/...` for Guardian desired
  state and `doctor/v1/...` for Doctor telemetry and query surfaces.
- **OCI registry** and **Bazel remote cache** integrated with the same store.

## Prerequisites

- Go 1.26.4 (see `go.mod`).
- Sibling checkouts of `kvs` and `cfg`, because `go.mod` contains replace
  directives:

  ```text
  replace github.com/radryc/kvs => ../kvs
  replace github.com/radryc/cfg => ../cfg
  ```

- Node 22 to build the router UI (or use prebuilt binaries).
- FUSE 3 for mounting (`fusermount3`).

## Quick start

Build:

```bash
make build          # binaries land in ./bin
```

Run a local 3-node cluster plus router:

```bash
make run-cluster    # router at localhost:9090
```

Ingest a repository (admin CLI talks to the router):

```bash
./bin/monofs-admin ingest \
  --router localhost:9090 \
  --source https://github.com/your-org/service-a.git
# authenticated clusters also accept --token / MONOFS_TOKEN
```

Mount the workspace:

```bash
make mount MOUNT_POINT=/mnt/monofs WRITABLE=1
# equivalent raw invocation:
# ./bin/monofs-client --router localhost:9090 --mount /mnt/monofs \
#   --overlay /tmp/monofs-overlay --writable --virtual-monorepo
```

Use the session CLI against the client's socket:

```bash
./bin/monofs-session --socket /tmp/monofs-dev/overlay/session.sock status
./bin/monofs-session --socket /tmp/monofs-dev/overlay/session.sock diff
./bin/monofs-session --socket /tmp/monofs-dev/overlay/session.sock commit -m "Update platform routing"
./bin/monofs-session --socket /tmp/monofs-dev/overlay/session.sock pull
./bin/monofs-session --socket /tmp/monofs-dev/overlay/session.sock push
```

The router UI/API is served at `http://localhost:8080` (also mirrored under
`/monofs` for sub-path deployments).

## Docker deployment

```bash
cp .env.example .env
# required: all services must share the same archive key
openssl rand -hex 32          # put the value in MONOFS_ENCRYPTION_KEY

make deploy                   # router + 3 nodes + fetchers + search + clients
make deploy-s3                # adds MinIO-backed archive storage
make deploy-stop              # stop
```

## Managed namespaces

- `guardian/<partition>/...` — Guardian partition content, written through
  router Guardian APIs (versioned, authorized per principal).
- `guardian-system/...` — Guardian platform state (registry, queues, scans,
  archives) backed by the embedded KVS.
- `doctor/v1/...` — Doctor telemetry surfaces; queries are served from the
  logengine hosted on storage nodes.

Normal ingestion is blocked from colliding with these reserved namespaces.

## Repository layout

```text
cmd/            service binaries (router, server, client, fetcher, search, ...)
internal/       sharding, metastore, router, server, fuse, fetcher, search,
                registry, worker, logengine, workspacebundle, ...
api/proto/      gRPC contracts (router, server, fetcher, search)
pkg/authz       shared authentication/authorization (OIDC, tokens, OWNERS)
pkg/grpcx       shared gRPC helpers (also used by Guardian)
docs/           architecture and operations documentation
```

## Read more

- [Architecture](docs/architecture.md)
- [Getting Started](docs/getting-started.md)
- [Workspace and Integration Guide](docs/workspace-and-integrations.md)
- [Production Operations Playbook](docs/production-operations.md)
- [Bazel Integration](docs/bazel-integration.md)

## Status notes

- The native TCP protocol (`--native-addr`) and the out-of-tree kernel module
  (`monofs-kmod/`) are experimental.
- `k8s/` contains deployment guidance only; no manifests ship in this repo.
