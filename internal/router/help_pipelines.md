# MonoFS CI/CD Pipeline System

Pipeline-as-code for monorepos. Configure, run, and observe CI/CD pipelines directly from the MonoFS cluster — no external CI service required.

## Where pipelines live

Pipeline configs are YAML objects in the Guardian KVS under `/.pipelines/`.
The router loads that prefix at startup and watches it for changes
(`internal/router/pipeline_router.go`). There is no workspace directory scan,
no local-file loader, and no per-directory discovery.

The repository convention is to author them under `.monofs/pipelines/`, and the
checked-in examples live in `.monofs/pipelines/examples/`, but the router does
not read those workspace files directly.

```
Guardian logical namespace
└── /.pipelines/
    ├── full-cicd.yml
    ├── release.yml
    └── ci.yml
```

### Registration and scoping

1. Write the YAML object at `/.pipelines/<name>.yml` in Guardian (the logical
   path maps to the `guardian-system` storage namespace).
2. The router validates it and registers it. Changes are picked up live through
   Guardian Watch; no restart is required.
3. Scope a pipeline with the trigger filters in the YAML (`on.push.paths`,
   `paths-ignore`, branches, tags). **`source_dir` is not a user field** — the
   config struct marks it `yaml:"-"` — so writing it in a pipeline file has no
   effect, and per-directory auto-scoping is not implemented.

```yaml
name: server-ci
on:
  push:
    branches: [main]
    paths: ["packages/server/**"]
jobs:
  build:
    runs-on: builder
    steps:
      - run: make build-server
```

### Common patterns

**Single pipeline for the whole repo.** One object at `/.pipelines/ci.yml` covers
everything.

**Multiple pipelines at root.** Several objects in the same prefix — each is a
separate pipeline with its own name, triggers, and jobs.

### Removing a pipeline

Delete the object from Guardian:

```bash
curl -X DELETE http://router:8080/api/pipelines/old-ci
```

The router unregisters it from the in-memory orchestration state.

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

### 1. Author the pipeline

```bash
cp .monofs/pipelines/examples/build-test.yml /tmp/build-test.yml
```

Edit it for your repository, then register the object at
`/.pipelines/build-test.yml` in Guardian (through the Guardian path API or your
Guardian tooling). The router loads and watches `/.pipelines/`.

### 2. Trigger and watch

```bash
guardianctl pipeline list             # see registered pipelines
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
| `runs-on` | yes | `builder`, `docker`, `deployer`, `bazel`, `lambda` |
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
| `bazel` | Bazel build/test | Bazel/Bazelisk, BEP parsing |
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
| [`build-test.yml`](./../../.monofs/pipelines/examples/build-test.yml) | Build → unit test + vet + fmt check |
| [`image-build.yml`](./../../.monofs/pipelines/examples/image-build.yml) | Detect changes → build → Docker build |
| [`image-push.yml`](./../../.monofs/pipelines/examples/image-push.yml) | Docker build → push to registry |
| [`deploy.yml`](./../../.monofs/pipelines/examples/deploy.yml) | Deploy storage → Guardian → workers → services |
| [`full-cicd.yml`](./../../.monofs/pipelines/examples/full-cicd.yml) | **All stages merged**: detect → build → test → lint → containerize → push → deploy → notify |
| [`pr-checks.yml`](./../../.monofs/pipelines/examples/pr-checks.yml) | Fast PR feedback: lint + test + build (only affected) |
| [`release.yml`](./../../.monofs/pipelines/examples/release.yml) | Tag-triggered: full build → push all images → staging deploy → smoke test → production deploy |
| [`monorepo-matrix.yml`](./../../.monofs/pipelines/examples/monorepo-matrix.yml) | Cross-platform matrix: linux/amd64 + linux/arm64 |
| [`nightly.yml`](./../../.monofs/pipelines/examples/nightly.yml) | Scheduled full build + test + benchmark + security scan + nightly images |

### Package-level examples (path-filtered)

These examples demonstrate `on.push.paths` filters; the directory they live in
does not scope them automatically.

| File | Example path filter | What it does |
|------|-------------|-------------|
| [`packages/server/.monofs/pipelines/ci.yml`](./../../.monofs/pipelines/examples/packages/server/.monofs/pipelines/ci.yml) | `packages/server/**` | build-server → test-unit |
| [`packages/frontend/.monofs/pipelines/ci.yml`](./../../.monofs/pipelines/examples/packages/frontend/.monofs/pipelines/ci.yml) | `packages/frontend/**` | npm ci → lint → test → build |
| [`infra/.monofs/pipelines/deploy.yml`](./../../.monofs/pipelines/examples/infra/.monofs/pipelines/deploy.yml) | `infra/**` | terraform plan → apply |

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

Pipeline configs are Guardian KVS objects under `/.pipelines/`; the examples here are authoring templates, not auto-loaded files.

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
- **No separate CI config** — pipeline YAML lives in Guardian KVS under `/.pipelines/` (author them under `.monofs/pipelines/` as a convention)
- **Path-scoped** — use `on.push.paths`/`paths-ignore`; `source_dir` is not a user field and per-directory auto-scoping is not implemented
- **Loaded from Guardian** — the router reads `/.pipelines/` and picks up changes live via Guardian Watch
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
