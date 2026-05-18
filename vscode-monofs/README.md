# MonoFS VS Code Extension

> Part of the **Strata** platform.

This extension wraps the MonoFS virtual-monorepo development loop inside VS Code. It is designed for the sibling checkout layout used by this workspace, where MonoFS lives next to `../stratatools`.

## Features

- Explorer view with MonoFS workflow commands for bootstrap, release, mount, ingest, and session operations.
- Status bar entry that changes between mount, open-workspace, and configuration states.
- Terminal-backed commands for the sibling Strata tooling flow and local MonoFS binaries.
- Safe writable mount defaults that keep overlay and cache paths outside the mount point.
- Auto-detection of the `dev-workspace` partition runtime, with profile-specific defaults and local-only commands hidden when the extension is running inside the deployed VS Code workspace image.

## Requirements

- Linux or WSL-style environment with FUSE support.
- A built MonoFS checkout with binaries under `bin/`, or a custom `monofs.binaryDir` setting.
- A sibling Strata tooling checkout. The setting name is still `monofs.scriptsRepoPath` for backward compatibility.
- Kubernetes, Docker, and `guardianctl` available when you use bootstrap or release commands.

## Extension Settings

This extension contributes the following settings:

- `monofs.repoPath`: Absolute path to the MonoFS checkout.
- `monofs.scriptsRepoPath`: Absolute path to the sibling Strata tooling checkout used by workflow commands. The setting name is retained for backward compatibility.
- `monofs.binaryDir`: Directory that contains built MonoFS binaries.
- `monofs.routerAddress`: Router address used by mount, ingest, and session commands.
- `monofs.mountPath`: Local mount point for the projected workspace.
- `monofs.overlayPath`: Writable overlay state directory. Keep this outside the mount.
- `monofs.cachePath`: Optional client cache directory. Keep this outside the mount.
- `monofs.useExternalAddresses`: Enables `--use-external-addrs` on the mount command.
- `monofs.defaultBranchStrategy`: Default branch strategy for `monofs-session commit`.
- `monofs.openMountInNewWindow`: Controls whether the mounted workspace opens in a new window.

## Common Flow

1. Run `MonoFS: Build Binaries` if the local CLI tools are not built yet.
2. Run `MonoFS: Bootstrap Deploy` and `MonoFS: Bootstrap Stamp URLs`.
3. Run `MonoFS: Release Partitions` and choose the common dev partitions or `--all`.
4. Run `MonoFS: Port-Forward Storage` if the router is not directly reachable.
5. Run `MonoFS: Ingest Repository` for each repository you want in the workspace.
6. Run `MonoFS: Mount Virtual Monorepo` and then `MonoFS: Open Mounted Workspace`.
7. Use the session commands for status, diff, commit, pull, push, and discard.

## Dev-Workspace Partition

The `dev-workspace` image now bakes this extension into OpenVSCode Server.

- `uv run st-image build --partition dev-workspace` from the sibling `stratatools` checkout packages the extension into the `dev-workspace-vscode` image.
- The partition entrypoint passes MonoFS mount, overlay, cache, router, and workspace-root values into the VS Code server process.
- When the extension detects that runtime, it switches to the `devWorkspacePartition` profile automatically.
- In that profile, the extension uses `/usr/local/bin`, `/mnt/monofs`, `/home/monofs/.monofs/overlay`, and `/var/cache/monofs` by default.
- Bootstrap, release, port-forward, local build, and manual mount commands stay hidden because the partition already manages those concerns.
- Session and workspace-opening commands stay available so the remote development environment remains useful without a local MonoFS checkout.

## Using It In dev-workspace

If you are using the deployed workspace at localhost:8888, use the extension like this:

1. Open Explorer. The extension appears as a MonoFS section inside Explorer, not as a separate Activity Bar icon.
2. If you do not see the section yet, open the Command Palette and search for MonoFS. The same commands are available there.
3. Run MonoFS: Open MonoFS Workspace. In the deployed workspace, this is usually the first command you want because the mount is already managed for you.
4. Edit files in the opened workspace normally.
5. Run MonoFS: Session Status to see pending changes.
6. Run MonoFS: Session Diff to inspect what will be published.
7. If you changed files under dependency/, run MonoFS: Session Push Dependencies before publishing source changes.
8. Run MonoFS: Session Commit to publish source changes upstream.
9. Run MonoFS: Session Pull to refresh from upstream, or MonoFS: Session Discard to throw away the current writable overlay changes.

Notes:

- The status bar also exposes MonoFS actions. When the workspace is ready, clicking the MonoFS status item opens the mounted workspace.
- There is no explicit Start Session command in the extension yet. The writable dev-workspace mount is already present, and the session commands operate on that mount. If you want the CLI equivalent, run monofs-session start in the integrated terminal.
- Open Configuration is mainly for adjusting paths and defaults. In the deployed workspace, the runtime profile should already be detected automatically.

## Development

```bash
cd vscode-monofs
npm run compile
```

Press `F5` in VS Code to launch an Extension Development Host and exercise the workflow commands.

## Known Issues

- Commands run in fresh integrated terminals. Long-running tasks such as port-forward and mount remain attached to those terminals.
- The extension does not create pull requests; publish still flows through `monofs-session commit`.
