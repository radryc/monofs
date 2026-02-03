# MonoFS Admin CLI

Command-line administration tool for MonoFS distributed filesystem.

## Installation

```bash
# Build from source
make build-admin

# Binary location
./bin/monofs-admin
```

## Commands Overview

| Command | Description |
|---------|-------------|
| `status` | Show cluster health, nodes, fetchers, and search indexer status |
| `ingest` | Ingest a Git repository into the cluster |
| `failover` | Show replication status and failover mappings |
| `fetchers` | Show fetcher cluster status and statistics |
| `node-files` | List files on a node or across all nodes |
| `repos` | List all ingested repositories |
| `rebuild-index` | Rebuild directory indexes for a repository |
| `drain` | Drain cluster for maintenance |
| `undrain` | Resume cluster after maintenance |
| `trigger-failover` | Manually trigger failover for a node |
| `clear-failover` | Clear failover state after node recovery |

## Command Details

### Status Command

Shows comprehensive cluster status including nodes, fetchers, and search indexer:

```bash
./bin/monofs-admin status --router=localhost:9090
```

**Example Output:**
```
CLUSTER STATUS
Router: localhost:9090
Cluster ID: monofs-cluster | Config Version: 6 | Total Nodes: 5

Cluster Health: [OK] EXCELLENT (100%)
Healthy Nodes: 5/5 | Total Weight: 500 | Total Files: 330,696

╔═════════╦════════════╦═════════╦═══════════╦════════╦════════╦════════════════════════════════╗
║ Node ID ║ Address    ║ Status  ║ Op Status ║ Weight ║ Files  ║ Disk Usage                     ║
╠═════════╬════════════╬═════════╬═══════════╬════════╬════════╬════════════════════════════════╣
║ node1   ║ node1:9000 ║ HEALTHY ║ Active    ║ 100    ║ 82,792 ║ 0.1GB used / 50.0GB free (0%)  ║
║ node2   ║ node2:9000 ║ HEALTHY ║ Active    ║ 100    ║ 41,298 ║ 0.2GB used / 50.0GB free (0%)  ║
╚═════════╩════════════╩═════════╩═══════════╩════════╩════════╩════════════════════════════════╝

FETCHERS
[OK] Fetchers: 2/2 healthy | Cache: 1.2 GB (85.3% hit rate) | Requests: 267,729

SEARCH INDEXER
[OK] Indexed: 31 repos (164,509 files) | Failed: 0 | Searches: 156 | Uptime: 12m43s
```

**Health Indicators:**
- `[OK]` - 100% healthy / no issues
- `[!!]` - Partial issues (some nodes unhealthy, some jobs failed)
- `[XX]` - Critical (majority unhealthy, service unavailable)

### Fetchers Command

Shows detailed fetcher pool status:

```bash
./bin/monofs-admin fetchers --router=localhost:8080
./bin/monofs-admin fetchers --router=localhost:8080 --detailed  # Per-source stats
./bin/monofs-admin fetchers --router=localhost:8080 --format=json
```

**Example Output:**
```
╔═══════════════════════════════════════════════════════════════════════════════╗
║                           FETCHER CLUSTER STATUS                              ║
╚═══════════════════════════════════════════════════════════════════════════════╝

  CLUSTER OVERVIEW
  [OK] Fetchers: 2/2 (100% healthy)
  Cache Hit Rate:   85.3%
  Cache Size:       1.2 GB (15,234 entries)
  Total Requests:   267,729
  Bytes Fetched:    500.0 MB
  Bytes Served:     1.7 GB

  FETCHER INSTANCES
  FETCHER                STATUS       REQUESTS   HIT RATE      CACHE   ACTIVE
  ---------------------- -------- ------------ ---------- ---------- --------
  fetcher-1              UP             187126      88.1%       800MB        0
  fetcher-2              UP              80603      79.5%       400MB        1
```

### Node Files Command

List files stored on a specific node or across all nodes:

```bash
# List files on a single node (shows only files on THAT node due to sharding)
./bin/monofs-admin node-files --router=localhost:9090 --node-id=node1

# List ALL files for a repository across ALL nodes
./bin/monofs-admin node-files --router=localhost:9090 --node-id=all --storage-id=<hash>

# JSON output
./bin/monofs-admin node-files --router=localhost:9090 --node-id=all --storage-id=<hash> --format=json
```

**Example Output (all nodes):**
```
╔═══════════════════════════════════════════════════════════════════════════════╗
║                       REPOSITORY FILES (All Nodes)                            ║
╚═══════════════════════════════════════════════════════════════════════════════╝

  [*] Router:        localhost:9090
  [*] Storage ID:    abc123def456...
  [*] Total Files:   1,234 (across all nodes)
  [*] Nodes Queried: 5

  Files per Node:
    node1: 312 files
    node2: 198 files
    node3: 201 files
    node4: 267 files
    node5: 256 files

  Files (showing first 20):
    src/main.go  [node1]
    src/util.go  [node3]
    README.md    [node2, node4]  # Replicated file
    ...
```

### Failover Command

Shows detailed replication status and failover information:

```bash
./bin/monofs-admin failover --router=localhost:9090
```

**When all nodes are healthy:**
```
╔═══════════════════════════════════════════════════════════════════════════════╗
║                          🛡️  REPLICATION STATUS                               ║
╚═══════════════════════════════════════════════════════════════════════════════╝

  ✅ Active Nodes:   3
  ❌ Failed Nodes:   0
  🛡️  Replication:   2x (Active)

╔═══════════════════════════════════════════════════════════════════════════════╗
║                   ✅ All nodes healthy - No failovers active                  ║
╚═══════════════════════════════════════════════════════════════════════════════╝
```

**When nodes have failed:**
Shows detailed failover mappings and explains what happens during node failure.

### Ingest Repository

Ingest a Git repository into the MonoFS cluster. Automatically detects branch from GitHub URL format.

#### Basic Usage

```bash
# Ingest from GitHub URL (auto-detect branch)
./bin/monofs-admin ingest --url=https://github.com/radryc/prompusher/tree/main

# Ingest from develop branch
./bin/monofs-admin ingest --url=https://github.com/prometheus/prometheus/tree/develop

# Ingest with custom repository ID
./bin/monofs-admin ingest \
  --url=https://github.com/golang/go/tree/master \
  --repo-id=golang
```

#### URL Format Support

The CLI automatically parses GitHub URLs in these formats:

- `https://github.com/owner/repo` - defaults to `main` branch
- `https://github.com/owner/repo/tree/branch` - uses specified branch
- `https://github.com/owner/repo/tree/feature/branch` - supports branches with slashes
- `https://github.com/owner/repo.git` - defaults to `main` branch

#### Options

| Flag | Description | Default | Required |
|------|-------------|---------|----------|
| `--url` | Git repository URL with optional `/tree/branch` | - | Yes |
| `--repo-id` | Custom repository ID for lookup | Auto-generated | No |
| `--router` | Router gRPC address | `localhost:9090` | No |
| `--debug` | Enable debug logging | `false` | No |

#### Examples

**Production deployment:**
```bash
./bin/monofs-admin ingest \
  --router=prod-router.example.com:9090 \
  --url=https://github.com/kubernetes/kubernetes/tree/release-1.29 \
  --repo-id=k8s
```

**Debug mode:**
```bash
./bin/monofs-admin ingest \
  --url=https://github.com/owner/repo/tree/main \
  --debug
```

**Output:**
```
time=2026-01-28T10:15:30.000Z level=INFO msg="parsed repository URL" repo_url=https://github.com/radryc/prompusher.git branch=main custom_repo_id=
time=2026-01-28T10:15:30.123Z level=INFO msg="ingesting repository" router=localhost:9090 repo_url=https://github.com/radryc/prompusher.git branch=main
time=2026-01-28T10:16:45.456Z level=INFO msg="✅ ingestion successful" files_ingested=42 message="Repository ingested successfully"
```

---

### Cluster Status

Display cluster health and node information.

#### Usage

```bash
# Local cluster
./bin/monofs-admin status

# Remote cluster
./bin/monofs-admin status --router=prod-router.example.com:9090

# With debug logging
./bin/monofs-admin status --debug
```

#### Options

| Flag | Description | Default | Required |
|------|-------------|---------|----------|
| `--router` | Router gRPC address | `localhost:9090` | No |
| `--debug` | Enable debug logging | `false` | No |

#### Output Example

```
🌐 Cluster Status
================
Cluster ID: prod-cluster
Config Version: 12
Total Nodes: 3

Node: node1
  Address: backend1.example.com:9001
  Weight: 100
  Status: ✅ HEALTHY

Node: node2
  Address: backend2.example.com:9002
  Weight: 100
  Status: ✅ HEALTHY

Node: node3
  Address: backend3.example.com:9003
  Weight: 150
  Status: ❌ UNHEALTHY

Healthy Nodes: 2/3
```

---

## Integration with Kubernetes

### ConfigMap for Router Address

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: monofs-admin-config
  namespace: monofs
data:
  ROUTER_ADDR: "monofs-router.monofs.svc.cluster.local:9090"
```

### Job for Repository Ingestion

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: monofs-ingest-repo
  namespace: monofs
spec:
  template:
    spec:
      containers:
      - name: admin
        image: monofs-admin:latest
        command:
        - /app/monofs-admin
        - ingest
        - --router=$(ROUTER_ADDR)
        - --url=https://github.com/owner/repo/tree/main
        - --repo-id=myrepo
        env:
        - name: ROUTER_ADDR
          valueFrom:
            configMapKeyRef:
              name: monofs-admin-config
              key: ROUTER_ADDR
      restartPolicy: OnFailure
```

### CronJob for Regular Ingestion

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: monofs-ingest-daily
  namespace: monofs
spec:
  schedule: "0 2 * * *"  # Daily at 2 AM
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: admin
            image: monofs-admin:latest
            command:
            - /app/monofs-admin
            - ingest
            - --router=monofs-router.monofs.svc.cluster.local:9090
            - --url=https://github.com/myorg/config/tree/production
            - --repo-id=prod-config
          restartPolicy: OnFailure
```

---

## Development

### Build

```bash
make build-admin
```

### Test

```bash
# Unit tests
go test ./cmd/monofs-admin/... -v

# Test with local cluster
./bin/monofs-admin ingest --url=https://github.com/test/repo/tree/main
./bin/monofs-admin status
```

### Docker Build

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o /app/monofs-admin ./cmd/monofs-admin

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/monofs-admin /app/monofs-admin
ENTRYPOINT ["/app/monofs-admin"]
```

---

## Troubleshooting

### Connection Refused

**Error:**
```
Error: connect to router: context deadline exceeded
```

**Solution:**
- Verify router address: `--router=correct-address:9090`
- Check router is running: `kubectl get pods -n monofs`
- Test connectivity: `nc -zv router-address 9090`

### Invalid URL Format

**Error:**
```
Error: parse URL: invalid GitHub URL format: expected owner/repo
```

**Solution:**
- Use correct URL format: `https://github.com/owner/repo/tree/branch`
- Ensure URL includes owner and repository name

### Ingestion Timeout

**Error:**
```
Error: ingest request failed: context deadline exceeded
```

**Solution:**
- Large repositories may take longer to ingest
- The CLI has a 5-minute timeout - this should be sufficient for most repos
- Check router and backend logs for issues

---

## See Also

- [Deployment Guide](../DEPLOYMENT.md)
- [Kubernetes Setup](../k8s/README.md)
- [Production Configuration](../k8s/PRODUCTION.md)
