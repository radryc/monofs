# Virtual Monorepo Workflow

MonoFS is easiest to use as a projected multi-repository development workspace.

Instead of exposing every backend namespace at the root of the mount, MonoFS can present a source-first workspace that behaves like a virtual monorepo: one mounted tree for editing, searching, testing, and publishing changes across many repositories.

This guide walks through the practical flow:

1. bootstrap the shared stack on Kubernetes
2. release the development partitions you need
3. ingest repositories into MonoFS
4. mount a projected workspace locally
5. edit, publish, refresh, and manage dependency changes with `monofs-session`

## Before You Mount

### 1. Bootstrap the shared stack

From the sibling workspace layout:

```bash
mt-bootstrap deploy
mt-bootstrap stamp-urls
```

This brings up the MonoFS storage stack in `storage-k8s`, the bootstrap Guardian control plane in `guardian-configs`, and the shared cluster prerequisites used by the released partitions.

### 2. Release the partitions you need

For a typical development setup:

```bash
mt-release --partition doctor
mt-release --partition dev-workspace
```

To roll the full application set in dependency order:

```bash
mt-release --all
```

### 3. Make the router reachable from your workstation

If your cluster already exposes a reachable router endpoint, use that. Otherwise, forward the MonoFS service locally:

```bash
mt-bootstrap port-forward
```

That gives you local access to the router on `localhost:9090` and the HTTP surface on `localhost:8080`.

### 4. Ingest the repositories you want in the workspace

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

## Mount a Projected Workspace

For daily development, mount MonoFS with `--virtual-monorepo`, `--writable`, and an overlay directory outside the mountpoint.

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

What those flags mean:

- `--virtual-monorepo` projects a source-first workspace instead of exposing the raw namespace tree at the root.
- `--writable` enables overlay-backed edits.
- `--overlay` points at the local state directory that stores pending changes.
- `--use-external-addrs` tells the client to use host-reachable backend addresses when the router advertises them.

Keep the overlay, cache, and workspace Git state outside the mountpoint. If the overlay lives under the mounted tree, the client can recurse through its own FUSE state and basic file operations will hang.

## What the Projected Root Looks Like

In virtual-monorepo mode, the mount root is shaped for source development.

- Repository content remains available at natural paths such as `github.com/acme/service-a`.
- `dependency/` stays visible at the projected root so local build caches and pushed blob-backed artifacts remain reachable.
- Root-level system namespaces such as `doctor/`, `guardian/`, and `guardian-system/` are hidden from the projected source root.
- Nested repository `.git` directories are hidden throughout the mount.
- MonoFS synthesizes a root `.git`, `.gitignore`, and `.monofs/workspace.json` so root-level Git tooling can work against the projected workspace.

The root Git view is there to support tools such as editors, language servers, `git status`, `git diff`, and `ripgrep`. It is not the publish mechanism.

- Use `monofs-session commit` to publish source changes upstream.
- Direct `git commit` at the mount root is intentionally blocked and points back to `monofs-session commit`.

## Daily Development Loop

```bash
cd /tmp/monofs-dev

# Inspect the projected workspace.
git status
git diff
rg "NewRouter" github.com/acme

# Build or test directly from the mounted workspace.
go test ./github.com/acme/service-a/...

# Review pending overlay changes.
./bin/monofs-session status
./bin/monofs-session diff

# Publish source changes upstream.
./bin/monofs-session commit -m "Update service-a to new shared client"

# Refresh if upstream moved while you were working.
./bin/monofs-session pull
```

This gives you:

- one mounted workspace for multiple repositories
- root-level Git visibility over the projected source tree
- normal editor and shell workflows against mounted files
- explicit publish and refresh steps instead of hidden write-back behavior

## Which Command To Use

- Use `monofs-session status` to see what changed in the current overlay session.
- Use `monofs-session diff` to inspect source changes before publishing.
- Use `monofs-session commit` to publish source-repository edits.
- Use `monofs-session pull` to refresh the mounted workspace from upstream state.
- Use `monofs-session push` for dependency or blob-backed changes under `dependency/**`.
- Use `monofs-session discard` to throw away the current overlay session.

## Publish Flow

`monofs-session commit` publishes source changes through the router and fetcher workflow rather than writing directly to backend storage.

1. The writable session gathers overlay changes and groups them by repository.
2. MonoFS builds a workspace bundle containing file changes, deletes, directory operations, and symlink targets.
3. The client uploads that bundle to the router.
4. The router hands the job to the appropriate fetcher for the workspace shard.
5. The fetcher applies the bundle to upstream repositories, commits the result, and pushes it.
6. The session is archived only after publish succeeds.

If publish fails, the session remains active so you can inspect, fix, or retry it.

### Supported Source Operations

The current publish bundle supports:

- file create and modify
- file delete
- directory create and remove
- symlink create

## Branch Strategies

`monofs-session commit` supports three publish strategies.

- `direct`: push to each repository's tracked branch.
- `workspace_branch`: push all touched repositories to `monofs/<workspace>/<job>`.
- `per_repo_branch`: push each touched repository to `monofs/<workspace>/<storage-id>`.

Example:

```bash
./bin/monofs-session commit \
  -m "Prepare coordinated change" \
  --author-name "MonoFS Bot" \
  --author-email "monofs@example.com" \
  --branch-strategy workspace_branch
```

Author fields default from `MONOFS_AUTHOR_NAME` and `MONOFS_AUTHOR_EMAIL`, then fall back to `GIT_AUTHOR_*` or `GIT_COMMITTER_*`.

In `direct` mode, a successful publish also triggers an immediate refresh so the mounted workspace and synthetic root Git baseline return to the new upstream state.

## Refresh Flow

`monofs-session pull` refreshes the projected workspace through the same router-managed sync path used by publish.

- The client sends the current repository refs and base commits to the router.
- The router checks upstream state through the fetcher tier.
- Repositories that changed upstream are re-ingested.
- The mounted workspace and synthetic root Git baseline are updated together.

That means `git status` and `git diff` at the mount root stay aligned with the refreshed workspace view.

## Dependency and Blob-Backed Changes

Repository publish intentionally excludes `dependency/**`.

- Use `monofs-session commit` for source repository changes.
- Use `monofs-session push` for dependency and blob-backed changes.

If a session contains both kinds of changes, push dependency changes first. Source publish will reject sessions that still contain dependency updates.

## Observability

The router UI and fetcher metrics expose publish and refresh activity.

- The dashboard shows recent workspace jobs, per-repository status, target branches, pushed commits, and failures.
- Fetcher metrics expose sync-worker activity such as staged bundles, published repositories, worktree bytes, and staging failures.

## Current Limits

MonoFS covers the core edit, publish, and refresh loop, but a few surrounding workflows still live outside MonoFS.

- It does not create pull requests.
- It does not automatically watch for branch merges and refresh non-direct publish branches after merge.
- It does not include `dependency/**` in source-repository publish.

Those steps still happen in your Git hosting system or through future MonoFS automation.
