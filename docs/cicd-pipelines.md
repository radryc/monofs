# CI/CD Pipelines Guide

## Purpose

This guide covers the complete CI/CD pipeline system: writing pipeline configs, registering them, deploying workers, integrating webhooks, and monitoring runs.

## How It Works

```
push/PR/tag ──► Router (Pipeline Orchestrator)
                     │
            ┌────────┼─────────┐
            ▼        ▼         ▼
        KVS Queue  Guardian  REST API
        (tasks)    Watch     (/api/pipelines)
            │        │         │
            ▼        ▼         ▼
    Builder Worker   Docker Worker   Deployer Worker   CICD Page (Vue)
    (make build)    (docker build)  (guardianctl)     (live status)
```

- **No Redis** — tasks flow through the existing Raft KVS + Guardian Watch
- **No external runners** — workers are k8s pods
- **No FUSE mount on router** — pipeline configs are read from `/.pipelines/` in Guardian namespace, committed via `monofs-session`
- **No registration step** — create the file, commit, done. Router auto-discovers.
## Pipeline Config Files

### Where to put them

Create `.monofs/pipelines/*.yml` anywhere in your monorepo:

```
workspace/
├── .monofs/pipelines/
│   ├── main-ci.yml       # global: runs on any change
│   └── release.yml       # global: tag-triggered
├── packages/server/.monofs/pipelines/ci.yml   # scoped
├── packages/frontend/.monofs/pipelines/ci.yml # scoped
└── infra/.monofs/pipelines/deploy.yml         # scoped
```

### Basic structure

```yaml
name: my-pipeline           # unique pipeline name
on:                         # when to trigger
  push:
    branches: [main]
  pull_request:
    branches: [main]
  tags: ["v*"]
jobs:                       # job DAG
  <job-name>:
    needs: [<upstream>]     # DAG dependency (optional)
    if: "condition"         # conditional (optional)  
    runs-on: builder        # worker type
    timeout-minutes: 10     # timeout (default 10)
    strategy:               # matrix (optional)
      matrix:
        var: [a, b, c]
    steps:
      - name: Step label
        run: shell command
      - uses: monofs/affected@v1
        id: output-id
        with:
          key: value
```

## Trigger Rules

| Trigger | When | Example |
|---------|------|---------|
| `push` | Code pushed to branch | `push: { branches: [main, "release/*"] }` |
| `pull_request` | PR opened/updated | `pull_request: { branches: [main] }` |
| `tags` | Tag pushed | `tags: ["v*"]` |

Path filters control which file changes trigger the pipeline:

```yaml
on:
  push:
    branches: [main]
    paths: [cmd/**, internal/**]          # only these paths
    paths-ignore: [docs/**, "*.md"]       # skip these paths
```

## Pipeline Stages

### Build

Compile binaries from source:

```yaml
build:
  runs-on: builder
  steps:
    - run: make build-server
    - run: make build-router
```

Worker type `builder` has: Go toolchain, Make, git, `/monofs` FUSE mount for source access.

### Test

Run tests after build succeeds:

```yaml
test:
  needs: [build]
  runs-on: builder
  steps:
    - run: make test-unit
    - run: make test-e2e
```

Tests can also run in parallel with linting/vetting:

```yaml
lint:
  needs: [build]
  runs-on: builder
  steps:
    - run: make vet
    - run: make fmt-check
```

For monorepos, use the matrix strategy with affected packages:

```yaml
test:
  needs: [build, detect]
  if: "affected != ''"
  strategy:
    matrix:
      package: ${{ needs.detect.outputs.packages }}
  runs-on: builder
  steps:
    - run: make test-${{ matrix.package }}
```

### Image Build

Create Docker images from built binaries:

```yaml
containerize:
  needs: [test]
  runs-on: docker
  steps:
    - run: |
        VERSION=$(git rev-parse --short HEAD)
        docker build -t registry.strata.local:5000/monofs-server:$VERSION -f Dockerfile .
```

Worker type `docker` has: Docker socket mount, privileged mode.

### Image Push

Push tagged images to container registry:

```yaml
push:
  needs: [containerize]
  runs-on: docker
  steps:
    - run: |
        VERSION=$(git rev-parse --short HEAD)
        docker push registry.strata.local:5000/monofs-server:$VERSION
        docker push registry.strata.local:5000/monofs-server:latest
```

### Deploy

Roll out to K8s via Guardian:

```yaml
deploy:
  needs: [push]
  runs-on: deployer
  steps:
    - run: guardianctl deploy monofs --set-version=$VERSION
    - run: guardianctl rollout status monofs --timeout=10m
```

Worker type `deployer` has: `guardianctl`, `kubectl`, RBAC for deployments.

## Full Merged Pipeline

Combine all five stages:

```yaml
name: full-cicd
on:
  push:
    branches: [main]
concurrency:
  group: full-cicd-main
  cancel-in-progress: true
jobs:
  detect:
    runs-on: builder
    steps:
      - uses: monofs/affected@v1
        id: affected
        with:
          packages: monofs-packages.yaml

  build:
    needs: [detect]
    if: "affected != ''"
    strategy:
      matrix:
        package: ${{ needs.detect.outputs.packages }}
    runs-on: builder
    steps:
      - run: make build-${{ matrix.package }}

  test:
    needs: [build]
    if: "affected != ''"
    strategy:
      matrix:
        package: ${{ needs.detect.outputs.packages }}
    runs-on: builder
    steps:
      - run: make test-unit

  lint:
    needs: [detect]
    runs-on: builder
    steps:
      - run: make vet && make fmt-check

  containerize:
    needs: [test, lint]
    strategy:
      matrix:
        package: ${{ needs.detect.outputs.packages }}
    runs-on: docker
    steps:
      - run: |
          VERSION=$(git rev-parse --short HEAD)
          docker build --target ${{ matrix.package }} \
            -t registry.strata.local:5000/monofs-${{ matrix.package }}:$VERSION \
            -f Dockerfile .

  push:
    needs: [containerize]
    strategy:
      matrix:
        package: ${{ needs.detect.outputs.packages }}
    runs-on: docker
    steps:
      - run: |
          VERSION=$(git rev-parse --short HEAD)
          docker push registry.strata.local:5000/monofs-${{ matrix.package }}:$VERSION

  deploy:
    needs: [push]
    runs-on: deployer
    steps:
      - run: |
          VERSION=$(git rev-parse --short HEAD)
          guardianctl deploy monofs --set-version=$VERSION
          guardianctl rollout status monofs --timeout=10m
```

## Conditions

Control whether a job runs:

| Expression | Meaning |
|-----------|---------|
| `always()` | Always run |
| `success()` | Only if no upstream failure |
| `failure()` | Only on upstream failure |
| `cancelled()` | Only if pipeline cancelled |
| `affected != ''` | Only if monorepo packages changed |

Use on notification/summary jobs:

```yaml
notify:
  needs: [deploy]
  if: always()
  runs-on: builder
  steps:
    - run: echo "Pipeline $PIPELINE_STATE"
```

## Matrix Strategy

Run a job multiple times with different variables:

```yaml
build:
  strategy:
    matrix:
      os: [linux, darwin]
      arch: [amd64, arm64]
    max-parallel: 4
  runs-on: builder
  steps:
    - run: GOOS=${{ matrix.os }} GOARCH=${{ matrix.arch }} make build
```

Variables are available as `${{ matrix.<name> }}` in step commands.

## Concurrency Control

Cancel older in-progress runs when a new push arrives:

```yaml
concurrency:
  group: deployment-${{ branch }}
  cancel-in-progress: true
```

## Monorepo-Aware Builds

### monofs-packages.yaml

Define packages at the workspace root:

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
  fetcher:
    path: cmd/monofs-fetcher
    deps: [internal/fetcher, api/proto]
    build: make build-fetcher
    test: make test-unit
```

### Affected detection

Use the built-in `affected` action to detect which packages changed:

```yaml
detect:
  runs-on: builder
  steps:
    - uses: monofs/affected@v1
      id: affected
      with:
        packages: monofs-packages.yaml
```

This computes the transitive closure: if `api/proto` changes, all packages depending on it are affected. Unaffected packages are skipped.

### Per-package pipelines

Place `.monofs/pipelines/` inside each package directory and set `source_dir` to auto-scope:

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

Commit via monofs-session:

```bash
monofs-session commit -m "add server CI pipeline"
```

## Command Reference

### guardianctl pipeline

```bash
guardianctl pipeline list                                   # list all pipelines
guardianctl pipeline run <name> [--branch main]             # manual trigger
guardianctl pipeline status <run-id>                        # status + jobs
guardianctl pipeline cancel <run-id>                        # cancel running
```

Pipelines are committed through `monofs-session` — no register/unregister commands needed.

### Worker management

```bash
guardianctl deploy pipeline-workers                         # deploy worker partition
guardianctl worker list                                     # active workers
```

### API endpoints

```
GET  /api/pipelines                  # list pipelines with last run status
POST /api/pipelines                  # register a pipeline (JSON body)
GET  /api/pipelines/<name>/runs      # run history
GET  /api/pipelines/<run-id>         # run detail with jobs
POST /api/pipelines/<name>/run       # trigger manual run
POST /api/pipelines/<run-id>/cancel  # cancel run
DELETE /api/pipelines/<name>         # unregister
GET  /api/pipelines/stats            # aggregate statistics
POST /api/webhooks/github            # GitHub webhook receiver
POST /api/webhooks/gitlab            # GitLab webhook receiver
```

## Webhook Setup

### GitHub

1. Repo → Settings → Webhooks → Add webhook
2. Payload URL: `https://<router-host>:8080/api/webhooks/github`
3. Content type: `application/json`
4. Events: Push, Pull request, Create (tags)
5. Secret: optional — set `MONOFS_GITHUB_WEBHOOK_SECRET` on router

### GitLab

1. Repo → Settings → Webhooks
2. URL: `https://<router-host>:8080/api/webhooks/gitlab`
3. Events: Push, Merge request, Tag push
4. Secret: optional — set `MONOFS_GITLAB_WEBHOOK_SECRET` on router

## Worker Deployment

Workers are defined as a Guardian partition:

```
partitions/pipeline-workers/
  config.yaml
  intents/
    images.yaml           # pipeline-worker + guardianctl image builds
    workers.yaml          # 3 compute assets
  payloads/
    builder.k8s.yaml      # FUSE mount, privileged, node selector
    docker.k8s.yaml       # Docker socket, privileged
    deployer.k8s.yaml     # kubectl RBAC, service account
```

| Worker | Tasks | Concurrency | Resources |
|--------|-------|-------------|-----------|
| Builder | `make build`, `make test` | 4 | 2 CPU / 4Gi |
| Docker | `docker build`, `docker push` | 2 | 2 CPU / 4Gi |
| Deployer | `guardianctl deploy` | 1 | 1 CPU / 2Gi |

## Task Queue Lifecycle

```
1. ENQUEUE   Router writes task JSON to KVS:
             /.queues/pipeline/<run-id>/tasks/<uuid>.json

2. NOTIFY    Guardian Watch publishes ChangeEvent (inline content < 64KB)

3. CLAIM     Worker upserts claim with CAS (ExpectedVersionId=""):
             /.queues/pipeline/<run-id>/.claims/<uuid>.json
             Only one worker wins — others move on.

4. EXECUTE   Worker runs steps (make, docker, guardianctl)
             Logs stream to router, timeout enforced via context

5. RESULT    Worker writes result:
             /.queues/pipeline/<run-id>/.results/<uuid>.json

6. ADVANCE   Router watches .results/, updates DAG state,
             enqueues downstream jobs, reports commit status
```

## UI Dashboard

Open `http://localhost:8080/cicd` to see:

- **Pipeline list** — all registered pipelines with last run status, source directory, run count
- **Run history** — commit SHA, branch, trigger type, job dot matrix, duration, filterable by state
- **Pipeline DAG** — jobs grouped into numbered stages with connecting lines, per-job status (spinner for running, retry count)
- **Job detail** — click any job card to see duration, worker ID, error messages, exit code
- **Re-run / Cancel** — re-run any pipeline, cancel running ones
- **Affected packages** — which monorepo packages triggered the run
- **Stats bar** — total/succeeded/failed runs, success rate, avg duration

## Architecture Decision Records

1. **No Redis** — Tasks use the existing Raft KVS for persistence. Guardian Watch replaces Redis pub/sub for task dispatch.
2. **No FUSE on router** — Pipeline configs flow through `monofs-session commit` into the `/.pipelines/` namespace. The router reads them via Guardian Read API, no filesystem mount needed.
3. **Workers are separate binaries** — Clean separation: router orchestrates, workers execute. Workers can be k8s pods or AWS Lambdas.
4. **Guardian for workers only** — Pipeline configs live in the monofs workspace filesystem. Guardian is used only for worker deployment and deploy-step execution.
5. **CAS claims** — Task claiming uses `ExpectedVersionId=""` optimistic locking. Only one worker can claim each task, no distributed locking needed.
