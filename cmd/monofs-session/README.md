# monofs-session

`monofs-session` is the companion CLI for writable MonoFS mounts.

You use it after `monofs-client` has mounted a workspace with `--writable --overlay=...`. It talks to the running client over the session Unix socket and gives you the commands you need to inspect changes, publish source edits, refresh from upstream, and manage dependency data.

## What It Is For

Most day-to-day work comes down to a small set of commands:

- `status`: see what changed in the current overlay session
- `branch`: inspect the authoritative tracked refs for the mounted repositories
- `diff`: inspect the pending source changes
- `commit`: publish source-repository changes upstream
- `pull`: refresh the mounted workspace from upstream state
- `push`: upload dependency and blob-backed changes under `dependency/**`
- `discard`: throw away the current overlay session
- `search`: query the MonoFS search index from the CLI

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

# Publish source changes.
./bin/monofs-session commit -m "Update search path"

# Refresh when upstream moves.
./bin/monofs-session pull
```

## Which Command To Use

- Use `commit` when you changed source files in repositories mounted through MonoFS.
- Use `push` when you changed dependency or blob-backed files under `dependency/**`.
- Use `pull` when upstream repositories changed after your mount was created, or after publishing with a non-`direct` branch strategy.
- Use `discard` when you want to reset the current overlay session instead of publishing it.

If a session contains both source edits and dependency changes, run `push` first and then `commit`.

## Common Commands

```bash
./bin/monofs-session start
./bin/monofs-session status
./bin/monofs-session branch
./bin/monofs-session diff
./bin/monofs-session commit -m "Update search path"
./bin/monofs-session pull
./bin/monofs-session push
./bin/monofs-session discard
./bin/monofs-session search --query "router" --max-results 20
```

## Commit

`commit` publishes source changes through the router and fetcher workspace-sync path.

```bash
./bin/monofs-session commit \
  -m "Refactor router sync jobs" \
  --author-name "Jane Developer" \
  --author-email "jane@example.com" \
  --branch-strategy direct
```

Supported flags:

- `-m`, `--message`: commit message used for the upstream publish commit
- `--author-name`: author name for the publish commit
- `--author-email`: author email for the publish commit
- `--branch-strategy`: one of `direct`, `workspace_branch`, or `per_repo_branch`

Author fields fall back in this order:

- `MONOFS_AUTHOR_NAME`
- `MONOFS_AUTHOR_EMAIL`
- `GIT_AUTHOR_NAME`
- `GIT_AUTHOR_EMAIL`
- `GIT_COMMITTER_NAME`
- `GIT_COMMITTER_EMAIL`

`commit` keeps the session active on failure and archives it only after a successful publish.

If the mount is running in virtual-monorepo mode, a successful `direct` publish also refreshes the mounted workspace and re-baselines the synthetic root Git metadata so `git status` returns clean again.

## Branch

`branch` prints the authoritative tracked ref and base commit for each included repository in the mounted virtual workspace.

```bash
./bin/monofs-session branch
```

Use it when root Git is too synthetic for branch inspection, or when you want to confirm which upstream refs MonoFS will treat as the source of truth for publish and refresh.

## Branch Strategies

- `direct`: push to each repository's tracked branch
- `workspace_branch`: push all touched repositories to `monofs/<workspace>/<job>`
- `per_repo_branch`: push each touched repository to `monofs/<workspace>/<storage-id>`

Use `direct` when you want the mount to behave like a normal synchronized development workspace. Use one of the branch strategies when you want MonoFS to stage work on separate publish branches.

## Pull

`pull` refreshes the current mounted workspace through the router-managed workspace sync flow.

```bash
./bin/monofs-session pull
```

Use it to pick up upstream commits that landed after the mount was created, or after publishing with a non-`direct` branch strategy.

`pull` also refreshes the synthetic root Git baseline maintained for the projected mount root.

## Dependency Data

Dependency and blob-backed files under `dependency/**` are handled separately from source-repository publish.

- `push` uploads dependency changes to the storage backend
- `blobs-info` summarizes dependency files tracked in the current session
- `setup` prepares per-tool cache directories inside MonoFS and prints shell exports

If you are updating dependencies and source code in one session, push dependencies first. Source publish will reject sessions that still contain dependency changes.

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
3. `~/.monofs/overlay/session.sock`

The socket is created by `monofs-client --writable --overlay=...`.

## Related Docs

- [../../README.md](../../README.md)
- [../../docs/virtual-monorepo-workflow.md](../../docs/virtual-monorepo-workflow.md)
