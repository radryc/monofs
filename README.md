# MonoFS

MonoFS is a distributed filesystem for SRE Tool Hub that gives teams a monorepo-style workspace without forcing them into a single Git repository.

It ingests repositories into a cluster, mounts them as one coherent workspace, and brings source code, Guardian-managed config, and Doctor telemetry closer together.

## Why MonoFS

- Work across many repositories as if they were one workspace.
- Keep upstream repos independent while getting monorepo ergonomics.
- Mount code, platform intent, and operational context in one place.
- Publish changes back to the correct upstream repositories.

## What It Is

MonoFS is not just a mount script. It is a distributed system with:

- a router for ingestion, coordination, workspace sync, and managed namespaces
- storage nodes for sharded metadata and file ownership
- fetchers for source retrieval, caching, publish, and refresh flows
- search services for indexing the unified workspace
- a FUSE client that presents the workspace locally

## What Makes It Different

MonoFS creates a virtual monorepo.

Instead of moving everything into one Git repository, you ingest existing repos and mount them into a single writable workspace. Developers and SREs get one project tree, one search surface, and one place to make cross-repo changes.

MonoFS also integrates with:

- Guardian, which exposes managed partition content under `guardian/<partition>/...`
- Doctor, which exposes operational data and query surfaces under `doctor/v1/...`

That means the workspace includes more than code. It includes desired state and operational reality.

## Quick Example

Ingest a repository:

```bash
./bin/monofs-admin ingest \
  --router localhost:9090 \
  --source https://github.com/your-org/service-a.git
```

Mount the virtual monorepo:

```bash
./bin/monofs-client \
  --mount /tmp/monofs \
  --router localhost:9090 \
  --use-external-addrs \
  --virtual-monorepo \
  --writable \
  --overlay /tmp/monofs-overlay
```

Inspect and publish changes:

```bash
./bin/monofs-session status
./bin/monofs-session diff
./bin/monofs-session commit -m "Update platform routing"
./bin/monofs-session pull
```

## Read More

- [Architecture](docs/architecture.md)
- [Getting Started](docs/getting-started.md)
- [Workspace and Integration Guide](docs/workspace-and-integrations.md)
- [Foo Application End-to-End Examples](docs/foo-application-lifecycle.md)
- [Production Operations Playbook](docs/production-operations.md)

## Summary

MonoFS is for teams that want monorepo developer experience, distributed storage, Guardian-managed config, and Doctor-integrated operations in one workspace.
