# MonoFS Architecture

MonoFS is a distributed filesystem that transforms Git repositories into a unified, mountable filesystem. This document explains how the system works and how its components interact.

---

## System Overview

MonoFS allows you to mount Git repositories as a local filesystem. Instead of cloning repositories to your workstation, you browse them directly through a FUSE mount. The system distributes file metadata across a cluster of backend nodes for scalability and fault tolerance.

```mermaid
graph TB
    subgraph Workstation["Your Workstation"]
        Mount["/mnt/monofs/"]
        GH["github.com/"]
        GL["gitlab.com/"]
        K8s["kubernetes/kubernetes/"]
        Go["golang/go/"]
        
        Mount --> GH
        Mount --> GL
        GH --> K8s
        GH --> Go
    end
    
    style Workstation fill:#e1f5fe
    style Mount fill:#fff
```

---

## Core Components

### Component Diagram

```mermaid
flowchart TB
    subgraph Cluster["MonoFS Cluster"]
        Client["FUSE Client<br/>(monofs-client)"]
        LB["Load Balancer<br/>(HAProxy)"]
        R1["Router 1<br/>(Primary)"]
        R2["Router 2<br/>(Backup)"]
        N1["Node 1"]
        N2["Node 2"]
        N3["Node N"]
        
        subgraph Fetchers["Fetcher Pool"]
            F1["Fetcher 1"]
            F2["Fetcher 2"]
        end
        
        Search["Search Service<br/>(Zoekt)"]
        
        Client --> LB
        LB --> R1
        LB --> R2
        R1 <--> R2
        R1 --> N1
        R1 --> N2
        R1 --> N3
        R2 --> N1
        R2 --> N2
        R2 --> N3
        N1 --> Fetchers
        N2 --> Fetchers
        N3 --> Fetchers
    end
    
    Git["Git Remotes"]
    GoMod["Go Module Proxy"]
    
    Fetchers --> Git
    Fetchers --> GoMod
```

### Component Responsibilities

| Component | Purpose |
|-----------|---------|
| **FUSE Client** | Mounts the filesystem, handles read/write operations, caches metadata locally |
| **Router** | Coordinates the cluster, determines which node owns which files, monitors health |
| **Backend Node** | Stores file metadata in NutsDB, serves file lookups, replicates data for failover |
| **Fetcher** | Retrieves actual file content from Git remotes or Go module proxies |
| **Search Service** | Indexes repositories for full-text code search using Zoekt |
| **HAProxy** | Load balances requests, handles router failover |

---

## Data Flow

### Reading a File

When you read a file from the mounted filesystem:

```mermaid
sequenceDiagram
    participant User
    participant FUSE as FUSE Client
    participant Router
    participant Node
    participant Fetcher
    
    User->>FUSE: cat file.go
    FUSE->>Router: Which node owns this file?
    Router-->>FUSE: Node 2
    FUSE->>Node: Get file metadata
    Node-->>FUSE: File attrs + blob reference
    FUSE->>Fetcher: Fetch blob content
    Fetcher-->>FUSE: File bytes
    FUSE-->>User: File content
```

### Ingesting a Repository

When you add a new repository to the cluster:

```mermaid
sequenceDiagram
    participant Admin as Admin CLI
    participant Router
    participant Fetcher
    participant Nodes as Nodes 1,2,3
    participant Search
    
    Admin->>Router: Ingest repository
    Router->>Fetcher: Clone repo
    Fetcher-->>Router: File list
    Router->>Router: Compute sharding<br/>(which node owns each file)
    Router->>Nodes: Distribute metadata
    Router->>Search: Index for search
    Router-->>Admin: Complete
```

---

## Sharding Strategy

MonoFS uses **Highest Random Weight (HRW) hashing** to distribute files across nodes. This ensures:

- **Deterministic placement**: Any component can compute which node owns a file
- **Minimal reshuffling**: Adding/removing nodes only moves files to/from that node
- **Load balancing**: Files are evenly distributed based on their paths

```mermaid
flowchart LR
    subgraph HRW["HRW Hash Calculation"]
        File["File: github.com/org/repo/pkg/server/main.go"]
        H1["hash(file + node1) = 0.72"]
        H2["hash(file + node2) = 0.91 ✓"]
        H3["hash(file + node3) = 0.45"]
        Result["Result: Stored on Node 2"]
        
        File --> H1
        File --> H2
        File --> H3
        H2 -->|Highest wins| Result
    end
    
    style H2 fill:#90EE90
```

---

## Failover & High Availability

### Node Failure Recovery

When a backend node fails, the system automatically redirects traffic to backup nodes:

```mermaid
flowchart TB
    subgraph Normal["Normal Operation"]
        N1a["Node 1 ✓<br/>Files A"]
        N2a["Node 2 ✓<br/>Files B"]
        N3a["Node 3 ✓<br/>Files C"]
    end
    
    subgraph Failed["Node 2 Fails"]
        N1b["Node 1 ✓<br/>Files A + Files B"]
        N2b["Node 2 ✗"]
        N3b["Node 3 ✓<br/>Files C"]
    end
    
    Normal -->|"Node 2 becomes unhealthy"| Failed
    
    style N2b fill:#ffcdd2
    style N1b fill:#c8e6c9
```

**Failover behavior:**

| Scenario | Behavior |
|----------|----------|
| Node becomes unhealthy | Traffic redirected to backup node within seconds |
| Node returns healthy | Traffic gradually returns to primary node |
| Planned maintenance | Use `drain` command to prevent failover triggers |
| Multiple node failures | System remains available if any replica exists |

### Router Redundancy

Two routers operate in active-backup mode:

```mermaid
flowchart TB
    HAProxy["HAProxy"]
    R1["Router 1 (Primary)<br/>All traffic"]
    R2["Router 2 (Backup)<br/>Standby"]
    
    HAProxy -->|Active| R1
    HAProxy -.->|Failover| R2
    
    style R1 fill:#c8e6c9
    style R2 fill:#fff9c4
```

---

## Write Sessions

MonoFS supports editing files through **write sessions**. Changes are stored locally until you commit them:

```mermaid
flowchart TB
    subgraph Session["Write Session Flow"]
        Start["1. Start Session"]
        Edit["2. Edit Files"]
        Review["3. Review Changes"]
        Decision{Commit or Discard?}
        Commit["Push to backend"]
        Discard["Delete overlay"]
        
        Start -->|"Overlay created"| Edit
        Edit -->|"Changes in overlay"| Review
        Review --> Decision
        Decision -->|Commit| Commit
        Decision -->|Discard| Discard
    end
```

---

## Search Architecture

The search service indexes all ingested repositories for fast code search:

```mermaid
flowchart TB
    Repo["Repository Ingested"]
    Indexer["Indexer Workers<br/>Process source files<br/>Extract symbols"]
    Zoekt["Zoekt Index<br/>Regex, literal, symbol search"]
    Query["Search Query"]
    Results["Results in milliseconds<br/>with file context"]
    
    Repo --> Indexer
    Indexer --> Zoekt
    Query --> Zoekt
    Zoekt --> Results
```

---

## Fetcher Architecture

Fetchers are stateless services that retrieve file content from external sources:

```mermaid
flowchart TB
    subgraph Pool["Fetcher Pool"]
        subgraph Registry["Backend Registry"]
            Git["Git Backend"]
            GoMod["GoMod Backend"]
            S3["S3 Backend<br/>(future)"]
        end
        
        F1["Fetcher 1<br/>LRU Cache (50GB)"]
        F2["Fetcher 2<br/>LRU Cache (50GB)"]
        FN["Fetcher N<br/>LRU Cache (50GB)"]
    end
    
    External["Git Remotes<br/>Go Module Proxy"]
    
    F1 --> External
    F2 --> External
    FN --> External
```

**Features:**
- Horizontal scaling for throughput
- Local caching reduces external network calls
- Repo-affinity routing for cache efficiency

---

## Performance Characteristics

| Operation | Typical Latency | Notes |
|-----------|----------------|-------|
| File lookup (cached) | < 5ms | Metadata in local cache |
| File lookup (uncached) | 20-50ms | Network round-trip to node |
| File read (cached) | < 10ms | Content in fetcher cache |
| File read (uncached) | 100-500ms | Depends on file size and network |
| Directory listing | 10-30ms | Batched metadata fetch |
| Search query | 50-200ms | Depends on index size |
| Repository ingestion | 30s - 5min | Depends on repo size |

---

## Configuration Reference

### Router Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `replication-factor` | 2 | Number of copies for each file's metadata |
| `health-interval` | 2s | How often to check node health |
| `unhealthy-threshold` | 6s | Time before marking node unhealthy |
| `rebalance-delay` | 10m | Wait time before permanent rebalancing |
| `graceful-failover-delay` | 60s | Wait time for graceful failover |

### Node Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `db-path` | /tmp/monofs-db | NutsDB database location |
| `git-cache` | /tmp/monofs-git-cache | Local Git cache directory |
| `enable-prediction` | false | Enable access pattern prediction |

### Fetcher Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `cache-dir` | /data/fetcher-cache | Cache directory for repos/modules |
| `max-cache-gb` | 50 | Maximum cache size |
| `cache-age-hours` | 2 | Max age before eviction |
| `prefetch-workers` | 4 | Background prefetch workers |

### Client Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `cache` | (none) | Local metadata cache directory |
| `overlay` | ~/.monofs/overlay | Write session storage |
| `rpc-timeout` | 10s | Timeout for RPC calls |
| `writable` | false | Enable write support |

---

## Security Considerations

- **Network isolation**: Fetchers should be in a DMZ with external access; nodes should be internal-only
- **No authentication**: Current version does not include authentication (roadmap item)
- **TLS support**: Can be enabled for gRPC connections
- **File permissions**: FUSE mount respects Unix permissions based on Git file modes

---

## Limitations

- Maximum file size: **Unlimited** (files streamed in chunks via gRPC streaming)
- Search index limit: 1MB per file (larger files truncated for indexing)
- Maximum files per repository: No hard limit, but performance degrades above 1M files
- Concurrent writes: Single-writer model per session
- Real-time sync: Changes must be explicitly committed (not automatic sync)
