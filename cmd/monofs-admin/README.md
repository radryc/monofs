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
| `ingest` | Ingest a Git repository into the cluster |
| `delete` | Delete a repository from all nodes |
| `status` | Show cluster health, nodes, fetchers, and search indexer status |
| `failover` | Show replication status and failover mappings |
| `fetchers` | Show fetcher cluster status and statistics |
| `node-files` | List files on a node or across all nodes |
| `repos` | List all ingested repositories |
| `rebalance` | Trigger rebalancing for a specific repository |
| `rebuild-index` | Rebuild directory indexes for a repository |
| `stats` | Show cluster or node statistics |
| `drain` | Drain cluster for maintenance |
| `undrain` | Resume cluster after maintenance |
| `trigger-failover` | Manually trigger failover for a node |
| `clear-failover` | Clear failover state after node recovery |
| `whitelist` | Manage ingestion whitelist (`add`, `remove`, `list`, `enable`, `disable`) |
| `dogfood` | Ingest all local sibling repositories into MonoFS |
| `repair-namespace-index` | Rebuild the repository namespace index for an offline database |

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

### Whitelist Command

Manage the in-memory ingestion whitelist. The operation is selected with
`--action`; `add` and `remove` require `--client-id`.

```bash
./bin/monofs-admin whitelist --router=localhost:9090 --action=list
./bin/monofs-admin whitelist --router=localhost:9090 --action=add \
  --client-id=ci-runner --label="CI runner"
./bin/monofs-admin whitelist --router=localhost:9090 --action=remove --client-id=ci-runner
./bin/monofs-admin whitelist --router=localhost:9090 --action=enable
./bin/monofs-admin whitelist --router=localhost:9090 --action=disable
```

| Flag | Description | Default | Required |
|------|-------------|---------|----------|
| `--action` | `add`, `remove`, `list`, `enable`, or `disable` | `list` | No |
| `--client-id` | Client ID to add/remove | - | For `add`/`remove` |
| `--label` | Human-friendly label (used with `add`) | - | No |
| `--router` | Router gRPC address | `localhost:9090` | No |

The whitelist is in-memory and resets when the router restarts.

### Ingest Repository

Ingest a Git repository, file path, or S3 prefix into the MonoFS cluster.

#### Basic Usage

```bash
# Ingest a Git repository (ref defaults to main)
./bin/monofs-admin ingest --source=https://github.com/radryc/prompusher

# Ingest a specific branch, tag, or commit
./bin/monofs-admin ingest --source=https://github.com/prometheus/prometheus --ref=develop

# Ingest with a custom source ID (display path)
./bin/monofs-admin ingest \
  --source=https://github.com/golang/go \
  --ref=master \
  --source-id=github.com/golang/go
```

`--source` accepts a plain URL or the legacy GitHub `/tree/<branch>` form; when
`--ref` is empty the branch from the URL is used, otherwise `main` for Git.

#### Options

| Flag | Description | Default | Required |
|------|-------------|---------|----------|
| `--source` | Git URL, file path, or S3 bucket | - | Yes |
| `--ref` | Git branch/tag/SHA (or S3 prefix) | `main` for Git | No |
| `--source-id` | Custom source/display ID | Auto-generated | No |
| `--ingestion-type` | `git`, `s3`, or `file` | `git` | No |
| `--fetch-type` | `blob` or `git` (other values fall back to `blob`) | `blob` | No |
| `--replicate-data` | Replicate blob data to the fetch backend during ingest | `false` | No |
| `--client-id` | Client ID for the ingestion whitelist | - | No |
| `--token` | Bearer token for the ingest RPC (defaults to `$MONOFS_TOKEN`) | env | No |
| `--router` | Router gRPC address | `localhost:9090` | No |

The ingest RPC has a 30-minute client timeout. Router and search gRPC require a
bearer token by default, and `ingest` (plus `dogfood`, which reuses the same
path) is the only admin command that attaches one. If `--token` is empty, pass
the router's token via `MONOFS_TOKEN`, or run with
`MONOFS_GRPC_REQUIRE_TOKEN=false`/`--insecure-dev-disable-auth` on a local lab.
The other commands (`status`, `delete`, `drain`, …) send no bearer token and
fail `Unauthenticated` against the default configuration.

#### Examples

**Production deployment with an explicit token:**
```bash
./bin/monofs-admin ingest \
  --router=prod-router.example.com:9090 \
  --source=https://github.com/kubernetes/kubernetes \
  --ref=release-1.29 \
  --source-id=k8s \
  --token="$MONOFS_ROUTER_TOKEN"
```

**File ingestion:**
```bash
./bin/monofs-admin ingest \
  --source=/srv/configs \
  --ingestion-type=file \
  --source-id=configs
```

**Output:**
```
Ingesting source: https://github.com/radryc/prompusher (ref: main)
Ingesting from router: localhost:9090
...
✅ Ingestion complete: 42 files
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
```

#### Options

| Flag | Description | Default | Required |
|------|-------------|---------|----------|
| `--router` | Router gRPC address | `localhost:9090` | No |

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
        - --source=https://github.com/owner/repo
        - --ref=main
        - --source-id=myrepo
        env:
        - name: ROUTER_ADDR
          valueFrom:
            configMapKeyRef:
              name: monofs-admin-config
              key: ROUTER_ADDR
        - name: MONOFS_TOKEN
          valueFrom:
            secretKeyRef:
              name: monofs-router-token
              key: token
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
            - --source=https://github.com/myorg/config
            - --ref=production
            - --source-id=prod-config
            env:
            - name: MONOFS_TOKEN
              valueFrom:
                secretKeyRef:
                  name: monofs-router-token
                  key: token
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
./bin/monofs-admin ingest --source=https://github.com/test/repo --ref=main
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
- The ingest RPC has a 30-minute client timeout (`context.WithTimeout(..., 30*time.Minute)`)
- Check router and backend logs for issues

---

## See Also

- [Architecture](../../docs/architecture.md)
- [Logical Flows](../../docs/logical-flows.md)
- [Kubernetes deployment](https://github.com/radryc/strata/blob/main/docs/kubernetes.md)
- [Local kind stack](https://github.com/radryc/strata/blob/main/docs/local-kind.md)
