# MonoFS Documentation Index

Generated: 2026-07-24 | Total: 30,300 lines across 21 files | Project: `github.com/radryc/monofs`

## Overview

This directory contains comprehensive function-level documentation for every Go source file in the MonoFS project (excluding auto-generated protobuf `.pb.go` files).

Each entry documents: function signature, purpose, callers/call-sites, parameters, return values, and implementation details.

---

## Documentation Files

| # | File | Lines | Covers |
|---|------|-------|--------|
| 1 | [cmd.md](cmd.md) | 2,200 | 14 entrypoint `main.go` files + admin CLI (drain, whitelist) |
| 2 | [internal-cache-client.md](internal-cache-client.md) | 1,439 | `internal/cache`, `internal/client` (sharded, workspace, identity, repository changes), `internal/diagnostics` |
| 3 | [internal-fetcher.md](internal-fetcher.md) | 2,242 | `internal/fetcher` (backend, client, service, workspace publish/push, metrics) |
| 4 | [internal-fuse-part1.md](internal-fuse-part1.md) | 3,634 | `internal/fuse` core: commit, file handles, node, overlay, overlaydb, overlaydb_vcs, ownership, session, session_socket, session_vcs |
| 5 | [internal-fuse-part2.md](internal-fuse-part2.md) | 1,156 | `internal/fuse` operations: create, getattr, lookup, mkdir, open, read, readdir, rename, rmdir, setattr, statfs, symlink, unlink, write, workspace, workspace_git, writable |
| 6 | [internal-git.md](internal-git.md) | 740 | `internal/git` (blob_cache, repo) |
| 7 | [internal-misc.md](internal-misc.md) | 1,141 | `internal/fsstat`, `internal/grpcutil`, `internal/monopath`, `internal/nativeproto`, `internal/workspacebundle` |
| 8 | [internal-registry.md](internal-registry.md) | 1,517 | `internal/registry` (blobs, manifests, monofs_client, proxy, server, stats, uploads) |
| 9 | [internal-router-authz.md](internal-router-authz.md) | 344 | `internal/router` authz: merge_external, merge_gate, read, ui |
| 10 | [internal-router-core.md](internal-router-core.md) | 1,606 | `internal/router` core: router, metrics, stats, ingest, delete, drain, autopush, subscribe, kvs_status, whitelist, repository_links |
| 11 | [internal-router-guardian.md](internal-router-guardian.md) | 1,017 | `internal/router` guardian: inject, path_mapper, paths, principals, version_store |
| 12 | [internal-router-native-workspace.md](internal-router-native-workspace.md) | 1,958 | `internal/router`: native_gateway, native_generation, native_namespace, native_read, workspace_ledger, workspace_publish, workspace_source_push, workspace_sync, workspace_sync_ui |
| 13 | [internal-router-pipeline-ui.md](internal-router-pipeline-ui.md) | 1,645 | `internal/router/pipeline` (affected, config, discover, orchestrator, reporter, taskqueue, types, webhook), pipeline_kvs, pipeline_router, pipeline_ui_handler, ui, ui_handler, ui_types |
| 14 | [internal-router-subs.md](internal-router-subs.md) | 1,296 | `internal/router` subpackages: mergerrequest, workspaceledger, workspacepolicy, workspacepr |
| 15 | [internal-search.md](internal-search.md) | 966 | `internal/search` (handlers, indexer, service) |
| 16 | [internal-server.md](internal-server.md) | 1,727 | `internal/server` (cfg_backend, directory, doctor_backend, failover, fs, kvs_backend, ledger, managed_namespaces, metrics, predictor, proxy, replication, server, server_stub) |
| 17 | [internal-sharding.md](internal-sharding.md) | 415 | `internal/sharding` (hrw, key) |
| 18 | [internal-storage.md](internal-storage.md) | 1,880 | `internal/storage` (auditstore, blob, file, git, logengine, logquery, stats, workspacestore) |
| 19 | [internal-telemetry.md](internal-telemetry.md) | 636 | `internal/telemetry` (config, slog_handler, telemetry) |
| 20 | [internal-worker-pkg-authz.md](internal-worker-pkg-authz.md) | 2,006 | `internal/worker` (client, handler) + `pkg/authz` (authz, compiler, device, grants, interceptor, machine, oidc, owners, pkce, resolver, verifier, webauth) |
| 21 | [test.md](test.md) | 735 | `test/` integration/E2E tests (client, consistency, dual_addressing, embedded_kvs, failover, full, router, server, stress) |

---

## Package Quick Reference

### cmd/ — Service Entrypoints
| Binary | Main File |
|--------|-----------|
| `k8s-fuse-device-plugin` | `cmd/k8s-fuse-device-plugin/main.go` |
| `monofs-admin` | `cmd/monofs-admin/main.go` + `drain.go` + `whitelist.go` |
| `monofs-client` | `cmd/monofs-client/main.go` |
| `monofs-fetcher` | `cmd/monofs-fetcher/main.go` |
| `monofs-loadtest` | `cmd/monofs-loadtest/main.go` |
| `monofs-pipeline-worker` | `cmd/monofs-pipeline-worker/main.go` |
| `monofs-registry` | `cmd/monofs-registry/main.go` |
| `monofs-router` | `cmd/monofs-router/main.go` |
| `monofs-search` | `cmd/monofs-search/main.go` |
| `monofs-server` | `cmd/monofs-server/main.go` |
| `monofs-session` | `cmd/monofs-session/main.go` |
| `monofs-trace-dump` | `cmd/monofs-trace-dump/main.go` |

### Key Internal Packages
| Package | Role |
|---------|------|
| `internal/server` | Core filesystem server (KVS backend, directory management, replication, failover) |
| `internal/fuse` | FUSE mount layer (node ops, overlay filesystem, session management, VCS) |
| `internal/router` | Cluster router (ingest, authz, websocket subscriptions, drain/failover, pipelines) |
| `internal/fetcher` | Blob fetcher service (cache, staging, workspace publish/push) |
| `internal/client` | MonoFS client library (sharded, workspace management) |
| `internal/registry` | OCI-compatible container registry (blobs, manifests, proxy) |
| `internal/search` | Full-text code search (bluge indexer, service, handlers) |
| `internal/storage` | Storage backends (blob, git, log engine, workspace store, audit) |
| `pkg/authz` | Authorization framework (grants, devices, OIDC, machine auth, PKCE) |

---

## Stats
- **Total Go source files documented**: ~179 (excluding `_test.go` and `.pb.go` files)
- **Total documentation lines**: 30,300
- **Documentation files**: 21
- **Test files also documented**: 9 integration test files
