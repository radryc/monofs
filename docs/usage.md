# Using MonoFS

> A practical guide to the MonoFS day-to-day workflow.

This document is the **how** that pairs with [architecture.md](architecture.md), which is the **what** and **why**. If you are setting up a cluster for the first time, start with [Before You Mount](#before-you-mount). If you already have a mount, jump to [The Daily Loop](#the-daily-loop).

![MonoFS developer workflow](assets/developer-flow.svg)

---

## Table of Contents

1. [Concepts in 60 Seconds](#concepts-in-60-seconds)
2. [The Tools](#the-tools)
3. [Before You Mount](#before-you-mount)
4. [Mount a Workspace](#mount-a-workspace)
5. [What the Projected Root Looks Like](#what-the-projected-root-looks-like)
6. [The Daily Loop](#the-daily-loop)
7. [Publishing Changes](#publishing-changes)
8. [Refreshing From Upstream](#refreshing-from-upstream)
9. [Dependency and Blob-Backed Changes](#dependency-and-blob-backed-changes)
10. [Searching Code](#searching-code)
11. [Mount Flags Cheat Sheet](#mount-flags-cheat-sheet)
12. [Common Tasks](#common-tasks)
13. [Troubleshooting](#troubleshooting)
14. [Current Limits](#current-limits)

---

## Concepts in 60 Seconds

- **Cluster** — the long-lived backend: router + storage nodes + fetchers + search.
- **Storage ID** — a stable identifier assigned to each ingested repository.
- **Mount** — a local FUSE mount point populated from the cluster. Either read-only, or **writable** with an overlay.
- **Overlay** — local-only change buffer that lives outside the mount. Required for any write.
- **Session** — a writable mount’s active overlay plus its identity on the cluster.
- **Virtual monorepo** — mount mode that hides internal namespaces and synthesizes a root `.git` so you can `git status` and `git diff` at the mount root.
- **Commit / Pull / Push** — `commit` publishes source changes upstream, `pull` refreshes the mount from upstream, `push` uploads dependency blobs.

---

## The Tools

There are two CLIs you will use directly:

| Command             | Audience   | Talks to                         | Purpose                            |
|---------------------|------------|----------------------------------|------------------------------------|
| `monofs-client`     | Developer  | Router (gRPC), then storage nodes | Mount the filesystem              |
| `monofs-session`    | Developer  | Client session socket (Unix)      | Inspect/publish/refresh/search    |
| `monofs-admin`      | Operator   | Router (gRPC)                     | Ingest, status, failover, drain    |
| `monofs-trace-dump` | Operator   | Storage node log engine           | Query ingest logs                 |

You usually only need `monofs-client` and `monofs-session` for day-to-day development.

---

## Before You Mount

### 1. Reach a running cluster

You need:

- A reachable router address (default gRPC `:9090`, HTTP UI `:8080`).
- For local dev, port-forward the MonoFS service:
  ```bash
  mt-bootstrap port-forward   # exposes router on localhost:9090, UI on localhost:8080
  ```

### 2. Ingest the repositories you want

Pick the source-first set you plan to work on:

```bash
./bin/monofs-admin ingest \
  --router=localhost:9090 \
  --source=git@github.com:acme/service-a.git \
  --ref=main

./bin/monofs-admin ingest \
  --router=localhost:9090 \
  --source=git@github.com:acme/shared-lib.git \
  --ref=main
```

`monofs-admin ingest` returns once the data is in the cluster and assigned a storage ID. Re-running it is safe; it refreshes the storage ID.

For the full operator reference, see `monofs-admin --help` and the source in `cmd/monofs-admin/main.go`.

---

## Mount a Workspace

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

What the flags mean:

| Flag                   | What it does                                                                  |
|------------------------|-------------------------------------------------------------------------------|
| `--mount=...`          | **Required.** Local mount point.                                              |
| `--router=...`         | Router gRPC address.                                                          |
| `--use-external-addrs` | Use host-reachable addresses the router advertises (important for WSL/Docker). |
| `--virtual-monorepo`   | Project a source-first root; hide system namespaces and nested `.git`.        |
| `--writable`           | Enable overlay-backed writes.                                                 |
| `--overlay=...`        | Local state directory for pending changes. **Must be outside the mount.**     |

> **Important:** keep the overlay, cache, and any workspace Git state **outside** the mountpoint. If the overlay is under the mount, the client will recurse through its own FUSE state and basic operations will hang.

The mount blocks until you send it a signal. Send `SIGTERM` (Ctrl-C) to unmount cleanly.

---

## What the Projected Root Looks Like

In `--virtual-monorepo` mode the root is shaped for source development:

```
/tmp/monofs-dev/
├── .git/                    # synthesized by the client
├── .gitignore               # synthesized by the client
├── .monofs/
│   └── workspace.json       # tracks included repos, refs, base commits
├── dependency/              # visible: pushed blob-backed artifacts
├── github.com/
│   └── acme/
│       ├── service-a/       # content of acme/service-a at ingest ref
│       └── shared-lib/      # content of acme/shared-lib at ingest ref
└── (no .git/ inside repos)  # hidden in virtual-monorepo mode
```

What is **hidden** in virtual-monorepo mode:

- `/doctor/...`
- `/guardian/...`
- `/guardian-system/...`
- Nested `.git` directories inside ingested repos

What stays **visible**:

- Ingested source repositories
- `dependency/` (your pushed blobs, build caches, etc.)
- `dependency/<repo>/...` is treated like any other mount content

The synthesized root `.git` exists to make `git status`, `git diff`, IDE indexers, and `ripgrep` work against the projected workspace. It is **not** the publish path. Use `monofs-session commit` to publish; direct `git commit` at the mount root is intentionally blocked and points you back to `monofs-session commit`.

---

## The Daily Loop

```bash
cd /tmp/monofs-dev

# 1. Look around — root Git works like a real repo
git status
git diff
rg "NewRouter" github.com/acme

# 2. Build and test directly through the mount
go test ./github.com/acme/service-a/...

# 3. See your pending changes from the overlay
./bin/monofs-session status
./bin/monofs-session diff

# 4. Publish source changes upstream
./bin/monofs-session commit -m "Update service-a to new shared client"

# 5. Pick up upstream changes
./bin/monofs-session pull
```

### Which command for what

| Want to…                                          | Use                                |
|---------------------------------------------------|------------------------------------|
| See what changed in the overlay                   | `monofs-session status`            |
| Inspect source changes before publishing         | `monofs-session diff`              |
| See the authoritative tracked refs for a repo     | `monofs-session branch`            |
| Publish source-repository edits                   | `monofs-session commit`            |
| Refresh the mount from upstream                   | `monofs-session pull`              |
| Upload dependency/blob changes                    | `monofs-session push`              |
| Throw away the current overlay                    | `monofs-session discard`           |
| Search code across ingested repos                 | `monofs-session search`            |

---

## Publishing Changes

`monofs-session commit` is the only way to publish source edits from a mount. It does not write to the cluster directly; it goes through the router → fetcher → upstream Git path.

```bash
./bin/monofs-session commit \
  -m "Refactor router sync jobs" \
  --author-name "Jane Developer" \
  --author-email "jane@example.com" \
  --branch-strategy direct
```

Flags:

- `-m`, `--message` — commit message for the upstream publish commit
- `--author-name` / `--author-email` — author for the publish commit
- `--branch-strategy` — one of `direct`, `workspace_branch`, `per_repo_branch`

Author fields resolve in this order:

1. `--author-name` / `--author-email`
2. `MONOFS_AUTHOR_NAME` / `MONOFS_AUTHOR_EMAIL`
3. `GIT_AUTHOR_NAME` / `GIT_AUTHOR_EMAIL`
4. `GIT_COMMITTER_NAME` / `GIT_COMMITTER_EMAIL`

### What happens during `commit`

1. The session collects overlay changes and groups them by repository.
2. It builds a **workspace bundle**: file create/modify, file delete, directory create/remove, symlink create.
3. The bundle is uploaded to the router.
4. The router hands the job to the fetcher for the workspace shard.
5. The fetcher applies the bundle to upstream repos, commits, and pushes.
6. The session is **archived** only after a successful publish. If publish fails, the session stays active so you can inspect, fix, or retry.

### Branch strategies

| Strategy            | What it does                                                                 |
|---------------------|------------------------------------------------------------------------------|
| `direct`            | Push to each repo’s tracked branch. Triggers an automatic refresh on success. |
| `workspace_branch`  | Push all touched repos to `monofs/<workspace>/<job>`.                        |
| `per_repo_branch`   | Push each touched repo to `monofs/<workspace>/<storage-id>`.                |

Use `direct` for normal synchronized dev. Use a branch strategy when you want MonoFS to stage work on separate publish branches you can review before merging.

### Supported source operations

The current publish bundle covers:

- file create / modify
- file delete
- directory create / remove
- symlink create

For everything else (rename, chmod, special files), do it upstream and re-mount.

---

## Refreshing From Upstream

```bash
./bin/monofs-session pull
```

`pull` re-runs the same router-managed sync flow as publish, in the other direction:

1. Client sends its current refs and base commits to the router.
2. Router asks a fetcher to check upstream state.
3. Changed repositories are re-ingested.
4. The mount and the synthetic root Git baseline are updated together.

Use `pull` when:

- Upstream landed commits after you mounted.
- You published with a non-`direct` branch strategy and want the mount to reflect upstream merges.

---

## Dependency and Blob-Backed Changes

Files under `dependency/**` are **not** part of source-repository publish. They are blob-backed and have their own path:

- `monofs-session push` uploads them to the cluster.
- `monofs-session blobs-info` summarizes what is pending.
- `monofs-session setup` prepares per-tool cache directories and prints shell exports.

If your session has both source edits and dependency changes, **push first**. Source publish rejects sessions that still contain dependency updates.

---

## Searching Code

```bash
# literal search
./bin/monofs-session search --query "func main" --max-results 10

# regex + file glob
./bin/monofs-session search --query "TODO" --regex --file-pattern "*.go"
```

Search runs against the cluster’s `monofs-search` service. The router UI also exposes a Code Search tab that uses the same backend.

---

## Mount Flags Cheat Sheet

```
--mount=DIR            (required) mount point
--router=HOST:PORT     router gRPC address
--cache=DIR            local metadata cache (empty = disabled)
--overlay=DIR          local overlay dir (implied --writable)
--use-external-addrs   use host-reachable node addresses
--virtual-monorepo     project a source-first root
--writable             allow writes via overlay
--debug                verbose MonoFS logs
--fuse-debug           verbose go-fuse logs (very noisy)
--log-file=PATH        JSON log output (DEBUG+)
--keep-cache           don't clear cache on mount
--rpc-timeout=DURATION per-RPC timeout (default 10s)
--client-id=ID         persistent client identifier
--version              print version and exit
```

The CLI will refuse to start without `--mount`. Run `monofs-client --help` for the full list.

---

## Common Tasks

### Inspect the cluster

```bash
./bin/monofs-admin status            # overall health
./bin/monofs-admin repos             # ingested repos
./bin/monofs-admin fetchers          # registered fetchers
./bin/monofs-admin stats             # per-node storage stats
./bin/monofs-admin node-files --node=node-a
```

The router UI at `http://localhost:8080` shows the same data with nicer presentation, plus workspace job history and pprof collection.

### Add a new repo to a running mount

```bash
./bin/monofs-admin ingest --router=localhost:9090 \
  --source=git@github.com:acme/new-thing.git --ref=main

# refresh the mount so it picks up the new content
./bin/monofs-session pull
```

### Change the published branch strategy per session

```bash
./bin/monofs-session commit -m "..." --branch-strategy workspace_branch
```

### Throw away local edits

```bash
./bin/monofs-session discard
```

The overlay is wiped; the mount stays. Reads continue to come from the cluster.

---

## Troubleshooting

**Mount hangs on startup.**
Check that `--overlay` (if set) is not under `--mount`. Look at `--debug` output to see where the client is stuck.

**Files I ingested don’t show up.**
Run `monofs-admin repos` to confirm the storage ID exists, then `monofs-session pull` to refresh the mount.

**`commit` fails halfway through.**
The session stays active. Run `monofs-session status` to see which repository failed and why, fix upstream, and try again. Use `monofs-session discard` to bail out.

**A node went down mid-session.**
The router marks it unhealthy and reads failover to the next replica. The client will log `stale` errors briefly. No action needed unless the failure is permanent, in which case the router will re-replicate after `--rebalance-delay`.

**Slow directory listings.**
Listings fan out to all healthy nodes. If one node is degraded, the merge waits on it; that is intentional (see [architecture.md — Read Path](architecture.md#read-path)). The router UI Performance tab has pprof snapshots if you need to diagnose.

**Search returns nothing.**
Make sure the search service has indexed your repos; this happens out of band after ingest and may take a few minutes. `monofs-admin status` reports search worker state.

---

## Current Limits

MonoFS covers the core edit, publish, and refresh loop. A few adjacent workflows still live elsewhere:

- It does not create pull requests — your Git hosting UI handles that.
- It does not auto-watch branch merges for non-`direct` publish strategies.
- It does not include `dependency/**` in source-repository publish.

Those steps happen in your existing Git tooling or in future MonoFS automation.

---

For the underlying design, see [architecture.md](architecture.md). For the session CLI command reference, see [cmd/monofs-session/README.md](../cmd/monofs-session/README.md).
