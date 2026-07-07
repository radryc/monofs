# Production Configuration

## Resource requests

| Component | CPU request | Memory request | CPU limit | Memory limit |
|-----------|-------------|---------------|-----------|--------------|
| Router | 500m | 512Mi | 2 | 2Gi |
| Fetcher | 1 | 1Gi | 4 | 4Gi |
| Server | 2 | 2Gi | 4 | 8Gi |
| Search | 500m | 512Mi | 2 | 2Gi |

## Storage

Use `local` PersistentVolumes for server node data directories. For state directories (router, fetcher cache), provisioned volumes with SSD-backed StorageClass are recommended.

### Example StorageClass

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: monofs-ssd
provisioner: kubernetes.io/no-provisioner
volumeBindingMode: WaitForFirstConsumer
```

## High availability

- **Router**: run 1 replica (active-passive; only one router per workspace state dir). Use a readiness probe on the gRPC port.
- **Fetcher**: 2+ replicas for redundancy. The router fans out to healthy fetchers.
- **Server nodes**: 3+ replicas with `podAntiAffinity` spread across nodes/racks. Replication factor should be set to `number_of_server_nodes - 1`.
- **Replication factor**: `--replication-factor 2` (primary + 1 backup). Set to `len(server_nodes) - 1` for full cluster fault tolerance.

## Security

- Run containers as non-root (uid 1000).
- Use Kubernetes Secrets for `ENCRYPTION_KEY` (32-byte hex).
- Use ServiceAccounts with minimal RBAC.
- Set `readOnlyRootFilesystem: true` on stateless pods.
- Enable `AppArmor` or `seccomp` profiles where available.

## Backup

- **Guardian state**: snapshot `--state-dir` PVC periodically.
- **Workspace state**: WAL is append-only; compacted snapshots are exported to S3 via `auditstore`. Enable `AuditStorageEnabled=true`.
- **Server data**: rely on replication factor for durability. For disaster recovery, use S3 archive backend.

## Upgrades

1. Drain the router (`monofs-admin drain`).
2. Upgrade server StatefulSet one pod at a time (default `OnDelete` strategy).
3. Upgrade fetcher Deployment (rolling update).
4. Upgrade router Deployment.
5. Verify cluster health.

## Monitoring

Prometheus metrics exported on each component's diagnostics HTTP port at `/metrics`. Key metrics:

- `monofs_router_workspace_sync_jobs_total`
- `monofs_fetcher_git_sync_jobs_total`
- `monofs_server_kvs_node_healthy`
- `monofs_workspacestore_wal_size_bytes`

## Config checklist for production

- [ ] `--state-dir` on persistent storage
- [ ] `--workspace-state-dir` on persistent storage
- [ ] `--encryption-key` set via env var or K8s Secret
- [ ] `--replication-factor` ≥ 2
- [ ] `--rebalance-delay` ≥ 10m
- [ ] `--source-push-mode` configured (squash or preserve)
- [ ] `--policy-config` pointing to validated policy YAML
- [ ] Resource limits set on all pods
- [ ] PVC protection (no accidental deletes via `Delete` reclaim policy)
- [ ] Audit log S3 export enabled
