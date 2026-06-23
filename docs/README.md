# MonoFS Documentation

MonoFS is a distributed source workspace, code search, and publish engine for monolithic repositories. It projects only the subset of a monorepo you need through a FUSE mount, while storage, indexing, and ingestion live on a horizontally scalable backend.

## Start here

| If you want to…                                    | Read this                                     |
|----------------------------------------------------|-----------------------------------------------|
| Understand what MonoFS is and how it works         | [architecture.md](architecture.md)            |
| Mount a workspace and develop against it          | [usage.md](usage.md)                          |
| See the `monofs-session` command reference         | [../cmd/monofs-session/README.md](../cmd/monofs-session/README.md) |
| Read about the experimental native VFS protocol    | [native-protocol-v1.md](native-protocol-v1.md) |

## Diagrams

- [assets/architecture-overview.svg](assets/architecture-overview.svg) — cluster components and request flow
- [assets/developer-flow.svg](assets/developer-flow.svg) — the four-step developer workflow

## Project layout

```
monofs/
├── api/                 # gRPC protobuf definitions
├── cmd/
│   ├── monofs-client/   # FUSE mount client (developer)
│   ├── monofs-session/  # overlay/inspect/publish CLI (developer)
│   ├── monofs-admin/    # operator CLI
│   ├── monofs-router/   # control plane + UI
│   ├── monofs-server/   # storage node (data plane)
│   ├── monofs-fetcher/  # upstream bridge
│   ├── monofs-search/   # out-of-band code indexing
│   ├── monofs-loadtest/ # filesystem load generator
│   └── monofs-trace-dump/ # storage log query tool
├── internal/
│   ├── router/          # control plane service
│   ├── server/          # storage node service
│   ├── client/          # gRPC client + HRW fan-out
│   ├── fuse/            # FUSE filesystem implementation
│   ├── fetcher/         # upstream job runner
│   ├── search/          # indexing service
│   ├── storage/         # storage backends + log engine
│   ├── sharding/        # HRW placement
│   ├── cache/           # local metadata cache
│   └── ...
└── docs/                # you are here
```

## Contributing

Issues and PRs are welcome on the upstream repository. Build with `make` (see the `Makefile`) and run tests with `go test ./...` from the repo root.
