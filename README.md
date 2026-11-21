# MonoFS

MonoFS is a distributed filesystem and workspace service for large monorepos. It separates **metadata**, **content**, and **coordination**:

- **Storage nodes** (`monofs-server`) keep the namespace, file metadata, and directory indexes in Pebble, placed by weighted rendezvous hashing (HRW).
- **Fetchers** (`monofs-fetcher`) keep content in encrypted, content-addressed packager archives, backed by local disk, S3, or GCS, and act as the Git sync worker.
- **The router** (`monofs-router`) coordinates topology, ingestion, workspace publish/push/refresh, Guardian control paths, Doctor telemetry queries, pipelines, and the web UI.
- **Clients** (`monofs-client`, `monofs-session`) expose a FUSE mount and a local-overlay write model with local virtual commits.

### Headless workspace client

`monofs-client --headless` (alias `--no-mount`) runs the same write session,
overlay, and cluster client **without mounting FUSE**. It serves filesystem
operations over the existing session socket:

```bash
monofs-client --headless --router localhost:9090 \
  --overlay /var/lib/monofs/overlay --writable --virtual-monorepo

monofs-session list github.com/acme/repo
monofs-session read github.com/acme/repo/main.go
monofs-session write github.com/acme/repo/main.go --content 'package main'
monofs-session stat github.com/acme/repo/main.go
monofs-session mkdir github.com/acme/repo/internal
```

This path needs no `/dev/fuse` and no `SYS_ADMIN`. Reads merge the local
overlay with cluster content (read-your-writes); writes land in the overlay and
are committed/pushed with the normal `add`/`commit`/`push` flow. The JSON
socket verbs are also usable directly by non-Go clients.

## Documentation

The documentation in this repository is derived from the source code and is
updated with the code.

| Document | Contents |
|---|---|
| [docs/README.md](docs/README.md) | Documentation index and one-page architecture |
| [docs/architecture.md](docs/architecture.md) | Components, data model, deployment, consistency, boundaries |
| [docs/logical-flows.md](docs/logical-flows.md) | Ingestion, reads, writes, publish/push, auth, pipelines, search, registry, telemetry |
| [docs/security-architecture.md](docs/security-architecture.md) | Assets, threat model, trust boundaries, controls, telemetry encryption |
| [docs/duckdb-logquery.md](docs/duckdb-logquery.md) | DuckDB sidecar, SQL views, limits, ClickHouse parity |
| [docs/blob-layout-v2.md](docs/blob-layout-v2.md) | Content-addressed sharded blob pack layout v2, manifest CAS, compaction |

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
make build            # core binaries into bin/ (see Makefile for CGo duckdb targets)
make proto            # regenerate protobuf
make test-unit        # go test ./internal/...
make deploy           # docker compose build/up (requires MONOFS_ENCRYPTION_KEY and MONOFS_TOKEN)
```

The main `Dockerfile` is a multi-stage build for the UI and all runtime images. The checked-in Compose topology is a development harness; review [docs/architecture.md](docs/architecture.md#10-deployment-topology) and [docs/security-architecture.md](docs/security-architecture.md) before using it outside a trusted lab.

## Transport security (mTLS)

The Go services (router, storage nodes, fetcher, search, registry, clients, admin, session, and cache) read the shared `STRATA_TLS_*` contract implemented by
[`github.com/radryc/tlsx`](../tlsx). `STRATA_TLS_MODE=off` is the default and
keeps gRPC and HTTP plaintext, so existing deployments are unchanged. The native
gateway and diagnostics listeners are not covered by this layer.

| Variable | Default | Meaning |
|---|---|---|
| `STRATA_TLS_MODE` | `off` | `off`, `optional`, or `required` |
| `STRATA_TLS_CERT_FILE` | `/var/run/secrets/strata-tls/tls.crt` | leaf certificate |
| `STRATA_TLS_KEY_FILE` | `/var/run/secrets/strata-tls/tls.key` | leaf private key |
| `STRATA_TLS_CA_FILE` | `/var/run/secrets/strata-tls/ca.crt` | CA bundle |
| `STRATA_TLS_SERVER_NAME` | dialed host | override the verified server name |
| `STRATA_TLS_REQUIRE_CLIENT_CERT` | `true` | require and verify client certificates on servers |
| `STRATA_TLS_ALLOWED_SPIFFE_IDS` | | comma-separated peer SPIFFE IDs a server accepts |
| `STRATA_TLS_EXPECTED_SPIFFE_IDS` | | comma-separated server SPIFFE IDs a client accepts |

Mode semantics:

- **`off`** — plaintext gRPC and HTTP; no certificate files are read.
- **`optional`** — servers terminate TLS when both the certificate and key
  exist; clients use TLS when the CA bundle exists and stay plaintext
  otherwise. Servers verify a client certificate when one is presented but do
  not require it. Intended only for migration.
- **`required`** — servers serve TLS/mTLS only (client certificates are
  required unless `STRATA_TLS_REQUIRE_CLIENT_CERT=false`); clients verify the
  configured CA and present their certificate. A missing or untrusted
  certificate fails the connection.

Certificates are re-read when the files change, so rotation does not need a
restart. Generate a local CA and one certificate per service (written to
`.tls/<service>/`):

```bash
go run github.com/radryc/tlsx/cmd/tlsx gen --dir .tls --reuse-ca
go run github.com/radryc/tlsx/cmd/tlsx verify --dir .tls
```

For the local Compose stack, use the override. It mounts
`./.tls/<service>` into each container at `/var/run/secrets/strata-tls` and
sets `STRATA_TLS_MODE=required` with a per-service SPIFFE allowlist:

```bash
docker compose -f docker-compose.yml -f docker-compose.mtls.yml up -d
```

`monofs-admin` and `monofs-session` honor the same environment, so point them
at the stack with `STRATA_TLS_MODE=required` and the generated files (their
HTTP calls switch to `https://` automatically).

## Security

The codebase still assumes a trusted network for several production controls (enforced authorization on most control RPCs, tenant isolation, and locked-down fetcher/registry/executor planes). Transport security is available through `STRATA_TLS_*` (see above), but it is off by default. Do not expose the stack to an untrusted network without reading [docs/security-architecture.md](docs/security-architecture.md) and applying the P0 checklist.
