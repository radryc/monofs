# MonoFS

MonoFS is a distributed filesystem and workspace service for large monorepos. It separates **metadata**, **content**, and **coordination**:

- **Storage nodes** (`monofs-server`) keep the namespace, file metadata, and directory indexes in Pebble, placed by weighted rendezvous hashing (HRW).
- **Fetchers** (`monofs-fetcher`) keep content in encrypted, content-addressed packager archives, backed by local disk, S3, or GCS, and act as the Git sync worker.
- **The router** (`monofs-router`) coordinates topology, ingestion, workspace publish/push/refresh, Guardian control paths, Doctor telemetry queries, pipelines, and the web UI.
- **Clients** (`monofs-client`, `monofs-session`) expose a FUSE mount and a local-overlay write model with local virtual commits.

## Documentation

The documentation in this repository was regenerated from the source code. The previous docs were moved to `docs_backup/` and are not used as input.

| Document | Contents |
|---|---|
| [docs/README.md](docs/README.md) | Documentation index and one-page architecture |
| [docs/architecture.md](docs/architecture.md) | Components, data model, deployment, consistency, boundaries |
| [docs/logical-flows.md](docs/logical-flows.md) | Ingestion, reads, writes, publish/push, auth, pipelines, search, registry, telemetry |
| [docs/security-architecture.md](docs/security-architecture.md) | Assets, threat model, trust boundaries, controls, findings, remediation |
| [docs/internal/README.md](docs/internal/README.md) | Internal assessment: missing functionality, duplication, overcomplexity, simplification plan, and code inventory |
| [docs/internal/code-inventory.md](docs/internal/code-inventory.md) | Every tracked code file with purpose and key symbols |

## Repository map

```text
api/proto/            gRPC contracts: router, node, fetcher, search
cmd/                  all binaries
internal/router/      control plane, Guardian, workspaces, pipelines, UI
internal/server/      storage node
internal/fetcher/     BlobFetcher + RepoSyncWorker services
internal/storage/     blob/file/git backends, log engine, workspace store
internal/fuse/        go-fuse node/overlay/session/commit implementation
internal/client/      sharded cluster client
internal/search/      Zoekt indexing and query
internal/registry/    OCI registry and pull-through proxy
internal/worker/      pipeline worker and executor
pkg/authz/            identity, OIDC, policy, grants, OWNERS/CODEOWNERS
monofs-kmod/          out-of-tree kernel client for the native protocol
```

## Build

```bash
make build            # selected binaries into bin/
make proto            # regenerate protobuf
make test-unit        # go test ./internal/...
make deploy           # docker compose build/up (requires MONOFS_ENCRYPTION_KEY)
```

The main `Dockerfile` is a multi-stage build for the UI and all runtime images. The checked-in Compose topology is a development harness; review [docs/architecture.md](docs/architecture.md#10-deployment-topology) and [docs/security-architecture.md](docs/security-architecture.md) before using it outside a trusted lab.

## Security

The codebase currently assumes a trusted network and lacks several production controls (TLS, enforced authorization on most control RPCs, tenant isolation, and locked-down fetcher/registry/executor planes). Do not expose it to an untrusted network without reading [docs/security-architecture.md](docs/security-architecture.md) and applying the P0 checklist.
