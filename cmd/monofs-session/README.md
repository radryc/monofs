# monofs-session

`monofs-session` is the companion CLI for writable MonoFS mounts.

You use it after `monofs-client` has mounted a workspace with `--writable --overlay=...`. It talks to the running client over the session Unix socket and gives you the commands you need to inspect changes, create local virtual commits, publish those commits upstream, refresh from upstream, and manage dependency data.

## What It Is For

Most day-to-day work comes down to a small set of commands:

- `start`: start or ensure a writable overlay session
- `status`: see what changed in the current overlay session
- `add` / `rm`: stage or remove local overlay changes
- `commit`: create a **local virtual commit** from staged changes (nothing is pushed)
- `push`: publish pending local virtual commits upstream through the router/fetcher
- `branch`: show, create, switch, or delete logical branches (`branch show|create|switch|delete`)
- `refs` / `log` / `tags` / `blame`: inspect tracked refs, local/upstream history, tags, and blame
- `diff`: inspect the pending source changes
- `conflicts`: inspect workspace merge/replay conflicts
- `pull`: refresh the mounted workspace from upstream state
- `push-blobs` / `push-deps`: upload dependency and blob-backed changes under `dependency/**`
- `discard`: throw away the current overlay session
- `search`: query the MonoFS search index from the CLI
- `setup` / `blobs-info`: prepare per-tool cache directories and inspect dependency blobs

## Quick Example

```bash
# Mount MonoFS in writable projected-workspace mode first.
./bin/monofs-client \
  --mount=/tmp/monofs \
  --router=localhost:9090 \
  --use-external-addrs \
  --virtual-monorepo \
  --writable \
  --overlay=/tmp/monofs-overlay

# Inspect local changes.
./bin/monofs-session status
./bin/monofs-session diff
git -C /tmp/monofs status
git -C /tmp/monofs diff

# Stage, checkpoint locally, then publish.
./bin/monofs-session add github.com/acme/repo/main.go
./bin/monofs-session commit -m "Update search path" --push-mode squash
./bin/monofs-session push

# Refresh when upstream moves.
./bin/monofs-session pull
```

## Which Command To Use

- Use `add` then `commit` to checkpoint source changes into one or more **local virtual commits**. `commit` does not contact the router or the Git remote.
- Use `push` to publish those pending local commits upstream. `--push-mode=preserve` replays each local commit; `--push-mode=squash` (or the router default) combines them per repository.
- Use `push-blobs` (alias `push-deps`) when you changed dependency or blob-backed files under `dependency/**`; `push` only handles source commits.
- Use `pull` when upstream repositories changed after your mount was created, or after publishing with a non-direct strategy.
- Use `discard` when you want to reset the current overlay session instead of publishing it.

## Common Commands

```bash
./bin/monofs-session start
./bin/monofs-session status
./bin/monofs-session add github.com/acme/repo/main.go
./bin/monofs-session commit -m "Update search path"
./bin/monofs-session push --push-mode squash
./bin/monofs-session branch
./bin/monofs-session refs
./bin/monofs-session diff
./bin/monofs-session pull
./bin/monofs-session push-blobs
./bin/monofs-session discard
./bin/monofs-session search --query "router" --max-results 20
```

## Commit (local only)

`commit` creates a local virtual commit from the entries staged with `add`/`rm`, grouped by repository. It persists the commit in the overlay session, advances the overlay baseline, and clears the staged entries. It does **not** push to the upstream repository.

```bash
./bin/monofs-session commit \
  -m "Refactor router sync jobs" \
  --author-name "Jane Developer" \
  --author-email "jane@example.com" \
  --push-mode squash
```

Supported flags:

- `-m`, `--message`: message recorded on the **local virtual commit** (not an upstream commit message)
- `--author-name`: author recorded on the local virtual commit
- `--author-email`: author email recorded on the local virtual commit
- `--push-mode`: records the preferred mode for a later `push` (`squash` or `preserve`; otherwise the router default)
- `--branch-strategy`: accepted for compatibility with `direct`, `workspace_branch`, or `per_repo_branch`, but reserved and ignored when creating local commits. `push` does not accept this flag.

Author fields default from:

- `MONOFS_AUTHOR_NAME`
- `MONOFS_AUTHOR_EMAIL`
- `GIT_AUTHOR_NAME`
- `GIT_AUTHOR_EMAIL`
- `GIT_COMMITTER_NAME`
- `GIT_COMMITTER_EMAIL`

`commit` fails if there are no staged entries or if the session has unresolved conflicts. It leaves the session active; use `discard` to abandon it.

## Push (publish local commits)

`push` sends pending local virtual commits upstream through the router/fetcher workspace-sync path.

```bash
./bin/monofs-session push --push-mode squash
./bin/monofs-session push --push-mode preserve
```

`push` takes no positional arguments and no branch-strategy flag. If `--push-mode` is omitted, the mode recorded at `commit` time is used, then the router configuration. After a successful push, the local commits are marked `Pushed` and branch mappings record the resulting upstream refs.

## Push Modes

`--push-mode` controls how local commits are replayed onto the upstream repository.

### `squash`

All pending local commits in a repository are combined into **one commit per repository**. The message is the latest local commit message, with a `MonoFS source push squashed N local commits for <repo>` trailer when more than one commit is collapsed.

```
Local commits:         [c1] [c2] [c3]
Upstream result:  ---> [single squashed commit]
```

**When to use squash:** Quick iterations where individual commit history is noise. CI workspaces. Feature branches destined for squash-merge. It is the default unless the router is configured otherwise.

### `preserve`

Each local commit is replayed individually in deterministic order (by `created_at_unix`, then commit ID). Author name, email, message, and ordering are preserved.

```
Local commits:         [c1] [c2] [c3]
Upstream result:  ---> [c1'] [c2'] [c3']   (replayed in order)
```

Each upstream commit message includes **provenance trailers** for traceability:

```
MonoFS-Local-Commit: c0a1b2c3d4e5...
MonoFS-Workspace: ws-prod-frontend
MonoFS-Job: job-xyz-abc123
```

These trailers link every upstream commit back to the exact local commit, workspace, and sync job that produced it. They never appear in squash mode.

**When to use preserve:** Feature work where individual commit history matters. Code review workflows. Any situation where "who wrote what and when" needs to survive the push to upstream.

**Conflict behavior:** If a local commit cannot be applied (upstream diverged), replay stops at that commit. The job result marks the specific commit ID and index. On retry, replay resumes from the first unpushed commit — already-pushed commits are never duplicated.

## Branch

`branch` manages session-level logical branches. `show` lists the current logical branch, known branches, pending commit counts, and per-repository mappings.

```bash
./bin/monofs-session branch show
./bin/monofs-session branch create feature-x
./bin/monofs-session branch switch feature-x
./bin/monofs-session branch delete feature-x
```

`monofs-session` is the source of truth for logical branches. A local virtual commit belongs to the current logical branch, and `switch` changes the branch without rewriting the working tree.

Use `refs` to see the tracked upstream ref and base commit for each included repository in the mounted virtual workspace:

```bash
./bin/monofs-session refs
```

## Branch Strategies

`commit` accepts `--branch-strategy` with `direct`, `workspace_branch`, or `per_repo_branch`, but the flag is reserved and ignored for local commits. `push` does not expose it; the fetcher chooses the target ref for each repository from the request/logical branch and the router's push configuration. Use `refs` and `branch show` to confirm the tracked and mapped branches.

## Pull

`pull` refreshes the current mounted workspace through the router-managed workspace sync flow.

```bash
./bin/monofs-session pull
```

Use it to pick up upstream commits that landed after the mount was created, or after publishing with a non-direct branch strategy.

`pull` also refreshes the synthetic root Git baseline maintained for the projected mount root.

## Dependency Data

Dependency and blob-backed files under `dependency/**` are handled separately from source-repository push.

- `push-blobs` (alias `push-deps`) packages and uploads dependency changes to the storage backend, then removes the pushed dependency entries from the overlay
- `blobs-info` summarizes dependency files tracked in the current session
- `setup` prepares per-tool cache directories inside MonoFS and prints shell exports

Use `push-blobs`/`push-deps`, not `push`, for dependency data.

## Search

The CLI also provides direct access to MonoFS search.

```bash
./bin/monofs-session search --query "func main" --max-results 10
./bin/monofs-session search --query "TODO" --regex --file-pattern "*.go"
```

## Socket Resolution

The CLI resolves the session socket in this order:

1. `--socket /path/to/session.sock`
2. `MONOFS_OVERLAY_DIR/session.sock`
3. `GITFS_OVERLAY_DIR/session.sock` (legacy fallback)
4. `MONOFS_OVERLAY/session.sock` (mount-helper fallback)
5. `~/.monofs/overlay/session.sock`

The socket is created by `monofs-client --writable --overlay=...`; point the
overlay environment variables at the same directory.

## Related Docs

- [../../README.md](../../README.md)
- [../../docs/logical-flows.md](../../docs/logical-flows.md)
- [../../docs/architecture.md](../../docs/architecture.md)
