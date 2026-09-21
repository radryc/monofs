# Workspace and Integrations Guide

## Purpose

This document explains how MonoFS behaves as a workspace system, especially when used with Guardian, Doctor, and multi-repository development workflows.

## The Workspace Model

MonoFS gives users a mounted filesystem that can contain:

- ingested source repositories
- managed Guardian partition content
- managed Doctor namespace content
- writable overlay changes from the current session

The key user experience goal is simple: one workspace, even when the underlying data comes from many systems.

## Working With Source Repositories

For normal Git-based sources:

- ingest the repository into MonoFS
- optionally assign a custom display path with `source_id`
- mount the workspace in virtual-monorepo mode
- edit files through the mounted tree
- publish changes upstream with `monofs-session commit`

Example:

```bash
./bin/monofs-admin ingest \
  --router localhost:9090 \
  --source https://gitlab.com/your-org/platform-api.git \
  --source-id sre/platform-api
```

This allows teams to shape the workspace around operating domains rather than upstream hosting layout.

## Why `source_id` Matters

`source_id` is the display path used in the filesystem view.

If omitted, MonoFS generates a path from the source URL. If provided, MonoFS uses that custom value as the visible workspace path.

That means:

- `source` is the upstream identity
- `source_id` is the workspace identity
- the resulting display path drives resolution and storage identity inside MonoFS

This is useful when the ideal operator-facing layout is not the same as the Git hosting layout.

## Writable Sessions

Writable MonoFS mounts use a session-based overlay.

This gives users a development loop that feels local:

- create files
- modify files
- delete files
- inspect diffs
- publish or discard changes

Common commands:

```bash
./bin/monofs-session status
./bin/monofs-session diff
./bin/monofs-session commit -m "Describe the change"
./bin/monofs-session pull
./bin/monofs-session push
./bin/monofs-session discard
```

## Guardian in the Workspace

Guardian content is visible under `guardian/<partition>/...`.

This makes Guardian useful as workspace data instead of only an external control plane.

What this enables:

- inspect partition YAML and intents from the same workspace as source code
- line up platform desired state with the repos it affects
- build tools and automation that use filesystem semantics for operational config

Guardian paths are managed namespaces, so they are treated differently from normal ingested repositories.

## Review Workflow

MonoFS can enforce a subtree-ownership review gate on source pushes so that
non-maintainers cannot silently publish changes into a subtree they do not own.
The gate is opt-in and disabled by default.

Ownership is declared in `.guardian/OWNERS` files (YAML) and, as a fallback
for paths no OWNERS file governs, in GitHub-style `CODEOWNERS` files
(`docs/CODEOWNERS`, `.github/CODEOWNERS`, or `CODEOWNERS` at the repo root).
A directory owner is listed under the `maintainers` role.

The review flow works like this:

1. OWNERS file declares the maintainers of a partition subtree
   (e.g. `guardian/doctor/.guardian/OWNERS`).
2. A source push that touches a subtree the caller does not maintain is
   rejected for **direct** pushes with a "review required" reason.
3. The caller retries using a logical branch (a non-direct strategy); MonoFS
   then pushes to a review branch and automatically opens a pull/merge request
   on the forge.
4. When the change lands on a review branch, the maintainers of the touched
   subtrees are requested as reviewers on the pull request.

Enable the gate with `--ownership-gate` on the router. An optional
`--ownership-team-mapping` YAML file maps OWNERS `@team` handles to IdP group
names.

For guardian-managed partitions that are not backed by an external git
provider, MonoFS exposes native merge-request endpoints that route proposal
creation, review, approval, and rejection through the same ownership model:

- `POST /api/merge-requests` — open a proposal (`{partition, paths, title}`)
- `GET  /api/merge-requests` — list proposals (`?status=open|approved|...`)
- `POST /api/merge-requests/{id}/approve` — approve (maintainer only)
- `POST /api/merge-requests/{id}/merge` — merge (maintainer only)
- `POST /api/merge-requests/{id}/reject` — reject (maintainer only)

## Doctor in the Workspace

Doctor content appears under `doctor/v1/...`.

MonoFS also exposes Doctor query and ingest APIs for:

- logs
- metrics
- traces

This allows a single platform to bridge:

- code under development
- configuration and desired state
- operational data used for debugging

## Virtual Monorepo Flow

A typical virtual-monorepo flow looks like this:

1. ingest the repositories needed for the workspace
2. mount MonoFS with `--virtual-monorepo --writable`
3. open the mounted workspace in the editor
4. edit across repository boundaries
5. inspect changes with `monofs-session`
6. publish source changes back to upstream repositories
7. refresh when upstream state changes

The result is a unified developer and operator workspace that still respects multi-repository ownership.

## What You Can and Cannot Create

MonoFS supports creating local overlay content in writable sessions.

However, there is an important distinction:

- you can create local files and scratch directories in the mounted workspace
- you can publish changes for repositories that MonoFS already knows about
- you cannot provision a brand-new upstream repository directly through the publish path

The normal workflow for a new project is:

1. create the repository in Git hosting first
2. ingest it into MonoFS
3. mount and work on it through the shared workspace

## Recommended User Journey

For a new team adopting MonoFS:

1. pick a small set of related repositories
2. ingest them with meaningful workspace-oriented display paths
3. mount a writable virtual monorepo
4. validate cross-repo search, edit, and publish workflows
5. introduce Guardian-managed partitions into the same workspace
6. connect Doctor queries for debugging and operational feedback

This sequence helps users understand that MonoFS is more than storage. It is a workspace model for SRE and platform engineering.
