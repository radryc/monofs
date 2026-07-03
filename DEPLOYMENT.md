# Deployment Guide

## Quick start (Docker Compose)

```bash
# Build and start all services
docker compose up -d

# View logs
docker compose logs -f

# Stop
docker compose down
```

## Services

| Service | Port | Description |
|---------|------|-------------|
| `monofs-router` | 9090 (gRPC), 8080 (HTTP UI) | Cluster topology, workspace orchestration |
| `monofs-fetcher` | 9200 (gRPC) | Sync worker, Git clone/push |
| `monofs-server` | 9001–9004 (gRPC), 9101–9104 (pprof) | Storage nodes |
| `monofs-search` | 9100 (HTTP) | Full-text search |

## Configuration

### Router

```
monofs-router \
  --port 9090 \
  --http-port 8080 \
  --state-dir /var/lib/monofs/guardian \
  --workspace-state-dir /var/lib/monofs/workspace \
  --source-push-mode preserve \
  --policy-config /etc/monofs/policy.yaml \
  --nodes "node-1=server-1:9001,node-2=server-2:9002"
```

### Fetcher

Fetcher config is loaded from a JSON file:

```bash
monofs-fetcher \
  --config /etc/monofs/fetcher.json \
  --port 9200
```

Example `fetcher.json`:
```json
{
  "Port": 9200,
  "CacheDir": "/var/cache/monofs-fetcher",
  "MaxCacheGB": 50,
  "CacheAgeHours": 24,
  "PrefetchWorkers": 4,
  "Storage": {
    "Type": "s3",
    "S3Region": "us-east-1",
    "S3Bucket": "monofs-archives",
    "S3Prefix": "archives/"
  }
}
```

## S3 / MinIO

For local development with MinIO:

```bash
docker compose -f docker-compose.s3.yml up -d
```

For external S3 with MinIO:

```bash
docker compose -f docker-compose.s3-external.yml up -d
```

## Kubernetes

See [k8s/README.md](k8s/README.md) and [k8s/PRODUCTION.md](k8s/PRODUCTION.md).

## Health checks

```bash
# Router UI
curl http://localhost:8080/

# Router gRPC health
grpcurl -plaintext localhost:9090 mono_fs.MonoFSRouter/GetClusterInfo
```

## State directories

| Directory | Purpose | Retention |
|-----------|---------|-----------|
| `--state-dir` | Guardian principals, version history | Bounded (tens of records) |
| `--workspace-state-dir` | Job history, bundle metadata, audit events | 30 days hot, managed by compaction |
