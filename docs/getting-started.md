# Getting Started

## Purpose

This guide gives a practical entry path for teams adopting MonoFS in SRE Tool Hub.

It covers two common environments:

- local development, where you bring up the platform yourself
- an existing production cluster, where MonoFS is already deployed and reachable

## Before You Start

You need:

- Git access to the repositories you want to ingest
- a Linux environment with FUSE support for local mounts
- the MonoFS binaries or a built MonoFS checkout
- access to the cluster router address

For local bring-up, you also need the SRE Tool Hub bootstrap and release tooling.

## Local Development Setup

This is the normal first-time setup flow when you want a full local platform with MonoFS, Guardian, Doctor, and monitoring.

### 1. Prepare the sibling checkout layout

Clone `stratatools` first, then use the toolchain to prepare the sibling repositories beside it.

```bash
git clone <your-stratatools-repo-url>
cd stratatools
uv sync
uv run st-setup
```

### 2. Deploy the local platform

Bootstrap MonoFS and Guardian, then release the managed partitions.

```bash
uv run st-bootstrap deploy
uv run st-release --all --bump --wait
```

This gives you a local environment with:

- bootstrap MonoFS storage and router
- Guardian
- Doctor
- monitoring
- supporting partitions such as OpenTelemetry and `k8s-top`

### 3. Verify the MonoFS endpoints

The local bootstrap flow normally exposes:

- router HTTP on `localhost:8080`
- router gRPC on `localhost:9090`

You can inspect cluster state with:

```bash
cd ../monofs
./bin/monofs-admin status --router localhost:9090
```

### 4. Ingest repositories

Ingest the repos you want in the workspace.

```bash
./bin/monofs-admin ingest \
  --router localhost:9090 \
  --source https://gitlab.com/your-org/foo.git \
  --source-id sre/foo
```

Repeat for related services, libraries, or operational repos that you want in the same workspace.

### 5. Mount the virtual monorepo

Mount a writable workspace with the overlay outside the mountpoint.

```bash
./bin/monofs-client \
  --mount /tmp/monofs \
  --router localhost:9090 \
  --use-external-addrs \
  --virtual-monorepo \
  --writable \
  --overlay /tmp/monofs-overlay
```

### 6. Work through the session flow

Use the mounted workspace like a monorepo, then inspect and publish changes with:

```bash
./bin/monofs-session status
./bin/monofs-session diff
./bin/monofs-session commit -m "Your change"
./bin/monofs-session pull
```

## Existing Production Cluster Setup

This is the normal flow when MonoFS is already deployed in a real cluster and you only need to connect to it.

### 1. Get the reachable router address

You need the production router gRPC endpoint. This may be:

- a directly reachable service address
- a bastion-forwarded address
- a local port-forward

If you need a temporary local tunnel, use your normal Kubernetes access method, for example:

```bash
kubectl port-forward -n monofs svc/monofs-router 9090:9090 8080:8080
```

Adjust namespace and service names to match your deployment.

### 2. Check cluster health

Before mounting, verify that the cluster is healthy.

```bash
./bin/monofs-admin status --router <url>:9090
./bin/monofs-admin repos --router <url>:8080
```

### 3. Ingest the production repositories you need

If the target repositories are not already present, ingest them into the cluster.

```bash
./bin/monofs-admin ingest \
  --router <url>:9090 \
  --source https://gitlab.com/your-org/foo.git \
  --source-id sre/foo
```

### 4. Use the existing production-mounted workspace

Assume the production environment already provides a mounted MonoFS workspace. In that case, connect to that existing workspace instead of creating a new local mount.

Typical examples:

- a remote development workspace with MonoFS already mounted
- an operator host where `/mnt/monofs` is already available
- a managed environment where mount lifecycle is handled outside your session

### 5. Develop carefully and publish explicitly

Use `monofs-session` to inspect and publish changes from that existing mounted workspace. In production-backed workspaces, avoid treating the mount as scratch space. Publish should be deliberate and traceable.

```bash
./bin/monofs-session status
./bin/monofs-session diff
./bin/monofs-session commit -m "Production change"
```

### 6. Use monitoring and operational controls

Production clusters should already have Doctor and monitoring partitions deployed. Use them to validate behavior after rollout.

Recommended checks:

- MonoFS health dashboards
- Guardian rollout dashboards
- Doctor ingest and query health dashboards
- cluster health from `monofs-admin status`

## Recommended Adoption Sequence

For most teams, the least risky adoption path is:

1. start in local dev with two or three related repositories
2. validate ingest, mount, edit, publish, and pull workflows
3. add Guardian-managed partitions into the same workspace
4. connect Doctor and monitoring for operational feedback
5. repeat the same workflow against an existing real cluster

## Related Guides

- [Workspace and Integration Guide](workspace-and-integrations.md)
- [Foo Application End-to-End Examples](foo-application-lifecycle.md)
- [Architecture](architecture.md)
