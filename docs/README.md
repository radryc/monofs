# MonoFS Documentation

MonoFS is a distributed source workspace, code search, and publish engine for monolithic repositories. It projects only the subset of a monorepo you need through a FUSE mount, while storage, indexing, and ingestion live on a horizontally scalable backend.

## Start here

| If you want to…                                           | Read this                                            |
|-----------------------------------------------------------|------------------------------------------------------|
| See the landing page and quick start                      | [../README.md](../README.md)                         |
| Understand what MonoFS is and how it works                | [architecture.md](architecture.md)                   |
| Mount a workspace, develop, and publish                   | [usage.md](usage.md)                                 |
| See the `monofs-session` command reference                | [../cmd/monofs-session/README.md](../cmd/monofs-session/README.md) |
| Read about the experimental native VFS protocol           | [native-protocol-v1.md](native-protocol-v1.md)       |
| Review the phased readiness design + implementation plans | [readiness/README.md](readiness/README.md)            |
| Understand the VS Code extension workflow                 | [../vscode-monofs/](../vscode-monofs/)               |

## Diagrams

- [assets/architecture-overview.svg](assets/architecture-overview.svg) — cluster components and request flow
- [assets/developer-flow.svg](assets/developer-flow.svg) — the four-step developer workflow

## Project layout

```
monofs/
├── api/proto/              # gRPC protobuf definitions (4 services)
├── cmd/
│   ├── monofs-client/      # FUSE mount client (developer)
│   ├── monofs-session/     # overlay/inspect/publish CLI (developer)
│   ├── monofs-admin/       # operator CLI
│   ├── monofs-router/      # control plane + UI
│   ├── monofs-server/      # storage node (data plane)
│   ├── monofs-fetcher/     # upstream bridge (DMZ)
│   ├── monofs-search/      # out-of-band code indexing
│   ├── monofs-loadtest/    # filesystem load generator
│   └── monofs-trace-dump/  # storage log query tool
├── internal/
│   ├── router/             # control plane: health, ingest, publish, failover, UI
│   ├── server/             # storage node: filesystem RPCs, replication, failover
│   ├── client/             # sharded gRPC client: HRW fan-out, workspace publish
│   ├── fuse/               # FUSE per-operation implementation, overlay, session socket
│   ├── fetcher/            # upstream job runner, multi-backend dispatch
│   ├── search/             # Zoekt indexing, regex/literal query
│   ├── storage/            # pluggable backends (git, blob), log engine, log query
│   ├── sharding/           # HRW (Rendezvous) consistent hashing
│   ├── cache/              # local metadata cache for FUSE client
│   ├── workspacebundle/    # Bundle and SourceCommitBundle types for publish/sync
│   ├── telemetry/          # OpenTelemetry setup, config, slog handler
│   ├── monopath/           # path parsing/verification for managed namespaces
│   ├── nativeproto/        # native VFS protocol types (experimental)
│   ├── grpcutil/           # gRPC connection utility
│   ├── fsstat/             # filesystem statfs utility
│   ├── diagnostics/        # diagnostics/pprof helpers
├── config/                 # fetcher backend configs (JSON)
├── docker/                 # fetcher entrypoint script
├── scripts/                # key generation, test runners, safe restart/shutdown
├── test/                   # integration/E2E tests
├── monofs-kmod/            # experimental Linux kernel module (C)
├── vscode-monofs/          # VS Code extension
└── docs/                   # you are here
```

## Key concepts

| Term                  | Meaning                                                                          |
|-----------------------|----------------------------------------------------------------------------------|
| Cluster               | The long-lived backend: router + storage nodes + fetchers + search               |
| Storage ID            | Stable identifier assigned to each ingested repository                           |
| Mount                 | Local FUSE mount populated from the cluster; read-only or writable with overlay  |
| Overlay               | Local-only change buffer outside the mount; required for writes                  |
| Session               | Active overlay + cluster identity for a writable mount                           |
| Virtual monorepo      | Source-first projection hiding system namespaces, synthesizing root `.git`       |
| Workspace bundle      | Collection of file/dir/symlink operations grouped by repository for publish      |
| HRW                   | Rendezvous consistent hashing for deterministic, minimally-disruptive sharding   |
| Indexed-Tail WORM     | Append-only storage format: packed, compressed, encrypted, failable mid-append   |
| Refreshing            | `pull` re-ingests upstream changes into the cluster and updates the mount        |

## Building and testing

```bash
make build-all          # build all 9 binaries
make test               # run unit tests
make test-e2e           # run end-to-end tests
make proto              # regenerate protobuf stubs
make deploy-local       # run a 3-node local cluster
```

For the full `Makefile` reference, run `make help`.

## Contributing

Issues and PRs are welcome on the upstream repository. Build with `make` (see the `Makefile`) and run tests with `go test ./...` from the repo root.
