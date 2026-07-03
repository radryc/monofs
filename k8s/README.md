# MonoFS on Kubernetes

## Overview

MonoFS components can run in Kubernetes as a StatefulSet for storage nodes and Deployments for stateless services (router, fetcher, search).

## Prerequisites

- kind or any Kubernetes cluster (v1.27+)
- `kubectl` configured
- Container registry with MonoFS images

## Quick start (kind)

```bash
# Create cluster
kind create cluster --config kind-config.yaml

# Load images
kind load docker-image monofs:latest monofs-search:latest

# Apply manifests
kubectl apply -f k8s/
```

## Component layout

| Component | Workload | Storage | Notes |
|-----------|----------|---------|-------|
| Router | Deployment (1 replica) | PVC for state dirs | Anti-affinity preferred |
| Fetcher | Deployment (2 replicas) | PVC for cache | Stateless otherwise |
| Server nodes | StatefulSet (3+ replicas) | PVC per pod | Use local PVs for performance |
| Search | Deployment (1 replica) | None | Stateless |
| FUSE device plugin | DaemonSet | None | Required for FUSE mounts |

## State directories

Persist these with PVCs:

- Router: `/var/lib/monofs/guardian`, `/var/lib/monofs/workspace`
- Fetcher: `/var/cache/monofs-fetcher`
- Server: `/data/monofs`

## Monitoring

Export Prometheus metrics from each component's diagnostics port. See [PRODUCTION.md](PRODUCTION.md) for production configuration.
