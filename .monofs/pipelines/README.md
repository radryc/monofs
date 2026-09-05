# MonoFS CI/CD Pipeline System

Pipeline-as-code for monorepos. Configure, run, and observe CI/CD pipelines directly from your MonoFS workspace — no external CI service required.

## Where pipelines live

Pipelines are YAML files in `.monofs/pipelines/*.yml` — committed through `monofs-session` like any other file. The router auto-discovers them from the monofs workspace — no separate registration needed, no FUSE mount on the router.

```
workspace/                              # monorepo root
├── .monofs/pipelines/
│   ├── full-cicd.yml                   # root-level: runs on any change
│   └── release.yml                     # root-level: tag-triggered
├── packages/
│   ├── server/
│   │   └── .monofs/pipelines/
│   │       └── ci.yml                  # scoped to packages/server/
│   ├── frontend/
│   │   └── .monofs/pipelines/
│   │       └── ci.yml                  # scoped to packages/frontend/
│   └── shared/
│       └── .monofs/pipelines/
│           └── test.yml                # scoped to packages/shared/
├── infra/
│   └── .monofs/pipelines/
│       └── deploy.yml                  # scoped to infra/
└── docs/
    └── .monofs/pipelines/
        └── spellcheck.yml              # scoped to docs/
```

### How pipelines reach the router

1. Create/update `.monofs/pipelines/*.yml` in your mounted workspace
2. Commit with `monofs-session commit -m "add CI pipeline"`
3. The router auto-loads pipeline configs from `/.pipelines/` in the Guardian namespace
4. Changes are picked up live via Guardian Watch — no restart needed

### Scoping rules

- **Root-level** (`.monofs/pipelines/` at repo root) → triggers on every matching event
- **Package-level** (e.g., `packages/server/.monofs/pipelines/`) → set `source_dir: packages/server` in the YAML to auto-scope:

```yaml
name: server-ci
source_dir: packages/server     # only triggers when packages/server/** changes
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: builder
    steps:
      - run: make build-server
```

You can also override with explicit `on.push.paths` filters in the YAML.

### Removing a pipeline

Remove the file from the workspace and commit:

```bash
rm .monofs/pipelines/old-ci.yml
monofs-session commit -m "remove old CI pipeline"
```

Or use the API directly:
```bash
curl -X DELETE http://router:8080/api/pipelines/old-ci
```

## Pipeline lifecycle

```
 push/PR/tag ──► ┌──────────┐    ┌──────────┐    ┌─────────────┐    ┌────────────┐    ┌──────────┐
                 │  BUILD   │───►│  TEST    │───►│ IMAGE BUILD │───►│ IMAGE PUSH │───►│  DEPLOY  │
                 └──────────┘    └──────────┘    └─────────────┘    └────────────┘    └──────────┘
                   make build      make test       docker build      docker push     guardianctl deploy
                    make vet       make test-all    docker tag                        kubectl apply
                                                                                     rollout verify
```

## Five pipeline stages

### 1. Build

Compiles binaries. Run first — everything downstream depends on it.

```yaml
jobs:
  build:
    runs-on: builder
    steps:
      - run: make build             # all binaries
      - run: make build-server      # single binary
```

Worker: `builder` — has Go toolchain + Make. Mounts source from `/monofs`.

### 2. Test

Runs after build. Unit tests, linting, vet checks run in parallel.

```yaml
jobs:
  test:
    needs: [build]
    runs-on: builder
    steps:
      - run: make test-unit
      - run: make test-all
```

Worker: `builder`. Failures here block image build and deploy.

### 3. Image build

Creates Docker/OCI images from built binaries.

```yaml
jobs:
  containerize:
    needs: [test]
    runs-on: docker
    steps:
      - run: |
          VERSION=$(git rev-parse --short HEAD)
          docker build -t registry.strata.local:5000/monofs-server:$VERSION -f Dockerfile .
```

Worker: `docker` — has Docker socket, can `docker build` and `docker tag`.

### 4. Image push

Pushes tagged images to the container registry.

```yaml
jobs:
  push:
    needs: [containerize]
    runs-on: docker
    steps:
      - run: |
          VERSION=$(git rev-parse --short HEAD)
          docker push registry.strata.local:5000/monofs-server:$VERSION
```

Worker: `docker`.

### 5. Deploy

Rolls out new images via Guardian.

```yaml
jobs:
  deploy:
    needs: [push]
    runs-on: deployer
    steps:
      - run: guardianctl deploy monofs --set-version=$VERSION
      - run: guardianctl rollout status monofs --timeout=10m
```

Worker: `deployer` — has `guardianctl`, `kubectl`, RBAC.

## Quick start

### 1. Create a pipeline

```bash
mkdir -p .monofs/pipelines
cp .monofs/pipelines/examples/build-test.yml .monofs/pipelines/
```

### 2. Commit through monofs-session

```bash
monofs-session commit -m "add build-test CI pipeline"
```

The router picks up the new pipeline automatically from `/.pipelines/`.

### 3. Trigger and watch

```bash
guardianctl pipeline list             # see the auto-discovered pipeline
guardianctl pipeline run build-test   # trigger manually (or push via webhook)
```

Open `http://localhost:8080/cicd` — live pipeline status, stage DAG, job logs.

## Reference

### Trigger rules

| Trigger | Example |
|---------|---------|
| Push to branch | `push: { branches: [main] }` |
| Pull request | `pull_request: { branches: [main] }` |
| Tag | `tags: ["v*"]` |
| Path filter | `push: { branches: [main], paths: [cmd/**, internal/**] }` |
| Ignore paths | `push: { branches: [main], paths-ignore: [docs/**, "*.md"] }` |

### Job fields

| Field | Required | Example |
|-------|----------|---------|
| `needs` | no | `[build, test]` — upstream jobs |
| `runs-on` | yes | `builder`, `docker`, `deployer`, `lambda` |
| `if` | no | `"affected != ''"`, `"push_to_branch == 'main'"` |
| `strategy.matrix` | no | `{ package: [server, router] }` |
| `timeout-minutes` | no | `15` (default 10) |
| `steps` | yes | Array of `run` / `uses` |

### Step fields

| Field | Purpose |
|-------|---------|
| `run` | Shell command (`/bin/sh -c`) |
| `uses` | Built-in action (`monofs/affected@v1`) |
| `name` | Display label in UI |
| `id` | Output identifier |
| `with` | Action parameters |

### Built-in actions

```yaml
- uses: monofs/affected@v1
  id: affected
  with:
    packages: monofs-packages.yaml
```

Outputs: `affected.packages` — JSON array of changed package names.

### Runner types

| `runs-on` | Purpose | Capabilities |
|-----------|---------|-------------|
| `builder` | Build, test, lint | Go toolchain, Make, FUSE mount |
| `docker` | Image build/push | Docker socket, privileged |
| `deployer` | Deploy, rollout | guardianctl, kubectl, RBAC |
| `lambda` | Short tasks | Invoked via AWS SDK |

### Concurrency control

```yaml
concurrency:
  group: main-ci-${{ branch }}
  cancel-in-progress: true
```

Cancels older in-progress runs for the same group when a new push arrives.

## Monorepo-aware builds

Create `monofs-packages.yaml` at the workspace root:

```yaml
packages:
  server:
    path: cmd/monofs-server
    deps: [internal/server, internal/storage, api/proto]
    build: make build-server
    test: make test-unit
  router:
    path: cmd/monofs-router
    deps: [internal/router, api/proto]
    build: make build-router
    test: make test-unit
```

The `affected` action computes the transitive closure of changed packages from git diff. Unchanged packages are skipped — only affected builds run.

## Example pipelines

### Root-level examples (scope: entire repo)

| File | What it does |
|------|-------------|
| [`build-test.yml`](./examples/build-test.yml) | Build → unit test + vet + fmt check |
| [`image-build.yml`](./examples/image-build.yml) | Detect changes → build → Docker build |
| [`image-push.yml`](./examples/image-push.yml) | Docker build → push to registry |
| [`deploy.yml`](./examples/deploy.yml) | Deploy storage → Guardian → workers → services |
| [`full-cicd.yml`](./examples/full-cicd.yml) | **All stages merged**: detect → build → test → lint → containerize → push → deploy → notify |
| [`pr-checks.yml`](./examples/pr-checks.yml) | Fast PR feedback: lint + test + build (only affected) |
| [`release.yml`](./examples/release.yml) | Tag-triggered: full build → push all images → staging deploy → smoke test → production deploy |
| [`monorepo-matrix.yml`](./examples/monorepo-matrix.yml) | Cross-platform matrix: linux/amd64 + linux/arm64 |
| [`nightly.yml`](./examples/nightly.yml) | Scheduled full build + test + benchmark + security scan + nightly images |

### Package-level examples (scoped to their directory)

| File | Scope | What it does |
|------|-------|-------------|
| [`packages/server/.monofs/pipelines/ci.yml`](./examples/packages/server/.monofs/pipelines/ci.yml) | `packages/server/**` | build-server → test-unit |
| [`packages/frontend/.monofs/pipelines/ci.yml`](./examples/packages/frontend/.monofs/pipelines/ci.yml) | `packages/frontend/**` | npm ci → lint → test → build |
| [`infra/.monofs/pipelines/deploy.yml`](./examples/infra/.monofs/pipelines/deploy.yml) | `infra/**` | terraform plan → apply |

## Webhook integration

### GitHub

```
URL:    https://<router-host>:8080/api/webhooks/github
Events: Push, Pull request, Create (tags)
Secret: MONOFS_GITHUB_WEBHOOK_SECRET
```

### GitLab

```
URL:    https://<router-host>:8080/api/webhooks/gitlab
Events: Push, Merge request, Tag push
Secret: MONOFS_GITLAB_WEBHOOK_SECRET
```

## Guardianctl

```bash
guardianctl pipeline list                    # list pipelines
guardianctl pipeline run <name>             # trigger
guardianctl pipeline status <run-id>        # status + job tree
guardianctl pipeline cancel <run-id>        # cancel
guardianctl pipeline watch <run-id>         # live progress

guardianctl deploy pipeline-workers         # deploy worker infrastructure
guardianctl worker list                     # active workers
```

Pipeline configs are managed through `monofs-session commit` — not through guardianctl.

## Architecture

```
GitHub/GitLab webhook ──► Router (pipeline orchestrator)
                              │
                    ┌─────────┼──────────┐
                    ▼         ▼          ▼
                KVS Queue  Guardian    REST API
                (tasks)    Watch       (/api/pipelines)
                    │         │          │
                    ▼         ▼          ▼
            Pipeline Worker  Worker     CICD Page
            (k8s pod)        (lambda)   (Vue UI)
```

- **No Redis** — tasks flow through existing Raft KVS + Guardian Watch
- **No external runners** — workers are k8s pods/Lambda functions
- **No separate CI config** — pipeline YAML lives in `.monofs/pipelines/`, committed via `monofs-session`
- **Auto-scoped** — per-package pipelines only trigger for changes in their `source_dir`
- **Auto-discovered** — router reads configs from `/.pipelines/`, picks up changes live via Guardian Watch
- **Guardian deploys workers** — `guardianctl deploy pipeline-workers`

## Deploying workers

```bash
guardianctl deploy pipeline-workers
```

Workers are defined as a Guardian partition in the appropriate partition directory:

| Worker | Replicas | Image | Capabilities |
|--------|----------|-------|-------------|
| Builder | 4 | `monofs-pipeline-worker` | FUSE mount, Go toolchain, privileged |
| Docker | 2 | `monofs-pipeline-worker` | Docker socket, privileged |
| Deployer | 1 | `guardianctl` | kubectl, guardianctl, RBAC |
