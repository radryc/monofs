# `internal/sharding` — Package Documentation

Package `sharding` provides **Rendezvous (Highest Random Weight) hashing** for consistent distribution of keys across backend nodes. It implements two files:

- `hrw.go` — The HRW data structure with node management, node selection, and utility hash functions.
- `key.go` — Shared key-generation utilities used by the router, client, and FUSE layer to build deterministic storage IDs and shard keys.

---

## `hrw.go`

---

### `type Node struct`

```go
type Node struct {
    ID      string
    Address string
    Weight  uint32
    Healthy bool
}
```

Represents a backend node for sharding purposes.

| Field     | Purpose                                                        |
|-----------|----------------------------------------------------------------|
| `ID`      | Unique node identifier.                                        |
| `Address` | Network address (e.g. `host:port`).                            |
| `Weight`  | Positive integer weight; `0` is treated as `1` in scoring.     |
| `Healthy` | If `false`, the node is excluded from `GetNode` / `GetNodes`.  |

---

### `type HRW struct`

```go
type HRW struct {
    mu    sync.RWMutex
    nodes []Node
}
```

Core data structure implementing Rendezvous hashing.

| Field   | Purpose                                      |
|---------|----------------------------------------------|
| `mu`    | Protects concurrent reads/writes to `nodes`. |
| `nodes` | Slice of all registered nodes.               |

---

### `func NewHRW(nodes []Node) *HRW`

```go
func NewHRW(nodes []Node) *HRW
```

**What it does:** Allocates a new `HRW` hasher, deep-copying the provided node slice.

**How it's called:** Called throughout the codebase wherever an `HRW` instance is needed — the router (`internal/router/router.go:1384`, `router.go:1452`, `router.go:1502`, etc.), the client (`internal/client/sharded.go`), proxy (`internal/server/proxy.go:141`), and many tests.

**Parameters:**
- `nodes` — initial node set (may be empty).

**Returns:** A pointer to the newly allocated `HRW`.

**Implementation details:** Copies via `make` + `copy` so the caller retains ownership of the original slice.

---

### `func NewHRWFromProto(nodes []*pb.NodeInfo) *HRW`

```go
func NewHRWFromProto(nodes []*pb.NodeInfo) *HRW
```

**What it does:** Convenience constructor that converts protocol buffer `NodeInfo` messages into internal `Node` structs, then delegates to `NewHRW`.

**How it's called:** The proxy (`internal/server/proxy.go:141`) and the sharded client (`internal/client/sharded.go:375`) use it when they receive node lists from the cluster manager via gRPC.

**Parameters:**
- `nodes` — slice of protobuf `NodeInfo` messages.

**Returns:** A pointer to the newly allocated `HRW`.

---

### `func (h *HRW) UpdateNodes(nodes []Node)`

```go
func (h *HRW) UpdateNodes(nodes []Node)
```

**What it does:** Atomically replaces the entire node list. Acquires the write lock, deep-copies the given slice.

**How it's called:** Internally by `UpdateNodesFromProto`. Also used directly in tests (`hrw_test.go:200`, `hrw_test.go:374`).

**Parameters:**
- `nodes` — new complete node list.

**Returns:** Nothing.

---

### `func (h *HRW) UpdateNodesFromProto(nodes []*pb.NodeInfo)`

```go
func (h *HRW) UpdateNodesFromProto(nodes []*pb.NodeInfo)
```

**What it does:** Converts protobuf `NodeInfo` messages to internal `Node` structs, then calls `UpdateNodes` to atomically replace the list.

**How it's called:** Used directly in tests (`hrw_test.go:223`) to simulate node-list updates from the cluster manager.

**Parameters:**
- `nodes` — slice of protobuf `NodeInfo` messages.

**Returns:** Nothing.

---

### `func (h *HRW) UpdateNodeHealthFromProto(nodes []*pb.NodeInfo)`

```go
func (h *HRW) UpdateNodeHealthFromProto(nodes []*pb.NodeInfo)
```

**What it does:** Updates node health, address, and weight from an incoming proto list while **preserving node order**. This is critical for HRW stability — node order affects the hash-space mapping. The algorithm:

1. Builds a lookup map from the incoming nodes.
2. For each existing node: if it appears in the incoming list, update its `Address`, `Weight`, and `Healthy` fields in place; otherwise, mark it `Healthy = false` but keep its position.
3. Appends truly new nodes (those not already present) at the end.

**How it's called:** The proxy (`internal/server/proxy.go:143`) and the sharded client (`internal/client/sharded.go:377`) call this on every health-check / heartbeat response to reflect the latest cluster state without reshuffling node order.

**Parameters:**
- `nodes` — slice of protobuf `NodeInfo` representing current cluster state.

**Returns:** Nothing.

**Implementation details:** Uses `seen` map to prevent double-adding; also double-checks for pre-existing node IDs that may have been added since the last update. O(n²) in worst case due to the inner loop at line 108–113, but node counts are small in practice.

---

### `func (h *HRW) SetNodeHealth(nodeID string, healthy bool) bool`

```go
func (h *HRW) SetNodeHealth(nodeID string, healthy bool) bool
```

**What it does:** Directly sets the `Healthy` flag for a single node by ID.

**How it's called:** Tests (`internal/client/failover_test.go:378`, `failover_test.go:564–566`, `failover_test.go:600`, `failover_test.go:626`) use it to simulate node failures and recoveries.

**Parameters:**
- `nodeID` — the node's unique identifier.
- `healthy` — the desired health state.

**Returns:** `true` if the node was found and updated; `false` otherwise.

---

### `func (h *HRW) GetNode(key string) *Node`

```go
func (h *HRW) GetNode(key string) *Node
```

**What it does:** Selects the **single best** healthy node for a given key using HRW scoring. Iterates over all nodes, computes `hash(key || node.ID) * weight`, and returns the node with the highest score. Only healthy nodes are considered.

**How it's called:** The router uses this for workspace-ledger node assignment (`internal/router/workspace_ledger_test.go:106,176,215`) and rebalancing logic. The sharded client uses it for directory-forward routing.

**Parameters:**
- `key` — the shard key (typically in the format `storageID:filePath`).

**Returns:** A **copy** of the winning node, or `nil` if no healthy nodes exist. The copy prevents mutation of internal state.

---

### `func (h *HRW) GetNodes(key string, n int) []Node`

```go
func (h *HRW) GetNodes(key string, n int) []Node
```

**What it does:** Returns the **top N** nodes for a given key, sorted by descending HRW score. Useful for replication (owner + replicas) or fallback scenarios.

**How it's called:** The workspace ledger (`internal/router/workspace_ledger.go:122`) uses it to get ranked nodes for client assignment. The native namespace handler (`internal/router/native_namespace.go:456`) uses it to select candidate nodes for partition placement.

**Parameters:**
- `key` — the shard key.
- `n` — maximum number of nodes to return (clamped to available healthy nodes).

**Returns:** A slice of up to `n` `Node` copies, ordered by descending score.

**Implementation details:** Uses a local `scored` struct with `sort.Slice` (O(m log m) where m = number of healthy nodes).

---

### `func (h *HRW) computeScore(key string, node *Node) uint64`

```go
func (h *HRW) computeScore(key string, node *Node) uint64
```

**What it does:** Computes the HRW score for a key-node pair: `FNV-64a(key || node.ID) * weight`. If `weight == 0`, weight defaults to `1`.

**How it's called:** Internally by `GetNode` and `GetNodes`.

**Parameters:**
- `key` — the shard key.
- `node` — pointer to the node being scored.

**Returns:** A `uint64` score. Higher is better.

**Implementation details:** Uses Go's `hash/fnv.New64a()` for speed. The multiplication is technically not saturating (despite the comment on line 225–227), but since HRW only needs relative ordering, overflow wrap-around does not affect correctness.

---

### `func (h *HRW) GetNodeByID(nodeID string) *Node`

```go
func (h *HRW) GetNodeByID(nodeID string) *Node
```

**What it does:** Looks up a node by its `ID` field.

**How it's called:** The proxy (`internal/server/proxy.go:255`) uses it to resolve upstream addresses for forwarding. Tests (`hrw_test.go:238,247`) verify lookup behavior. The failover test (`internal/client/failover_test.go:365`) uses it to retrieve node info for health manipulation.

**Parameters:**
- `nodeID` — the unique node identifier.

**Returns:** A **copy** of the node if found, or `nil`.

---

### `func (h *HRW) GetAllNodes() []Node`

```go
func (h *HRW) GetAllNodes() []Node
```

**What it does:** Returns a deep copy of all nodes regardless of health.

**How it's called:** The proxy (`internal/server/proxy.go:541`) uses it for stats reporting. The sharded client (`internal/client/sharded.go:1340`) exposes it via its own `GetAllNodes()` wrapper. Tests (`hrw_test.go:260`) verify copy semantics.

**Parameters:** None.

**Returns:** A copy of the full node slice.

---

### `func (h *HRW) GetHealthyNodes() []Node`

```go
func (h *HRW) GetHealthyNodes() []Node
```

**What it does:** Returns only healthy nodes.

**How it's called:** Very widely used. The proxy (`internal/server/proxy.go:540`) for stats. The sharded client in `internal/client/sharded.go:643,842,914,1351,1640,1933` and `repository_changes.go:65` for server selection. The client's `main.go:151` for logging. The router tests run `HealthyNodeCount()` which is logically similar.

**Parameters:** None.

**Returns:** A slice of healthy `Node` copies (may be empty).

---

### `func (h *HRW) NodeCount() int`

```go
func (h *HRW) NodeCount() int
```

**What it does:** Returns the total number of nodes (healthy + unhealthy).

**How it's called:** Tests (`hrw_test.go:202,225,295`), the router tests (`router_test.go:47,62,79,95,101,115,402,427`), failover tests (`failover_test.go:278,295`), and integration tests (`failover_integration_test.go:159,204`).

**Parameters:** None.

**Returns:** Integer count of all nodes.

---

### `func (h *HRW) HealthyNodeCount() int`

```go
func (h *HRW) HealthyNodeCount() int
```

**What it does:** Returns the count of healthy nodes.

**How it's called:** Extensively in tests and integration tests (`test/router_integration_test.go:270,284,528,542`, `test/failover_integration_test.go:56,246`). The router tests (`router_test.go:66,83,272,290,319,335`) use it for health assertions. The failover router test (`internal/router/failover_test.go:355,447,472`) also uses it.

**Parameters:** None.

**Returns:** Integer count of healthy nodes.

**Implementation details:** Iterates the full node list (O(n)). Not cached — cheap for typical cluster sizes.

---

### `func HashKey(key string) uint64`

```go
func HashKey(key string) uint64
```

**What it does:** Computes a standalone FNV-64a hash of a string. This is a utility function, **not** the same as `computeScore` (which includes the node ID and weight).

**How it's called:** Used for debugging and logging purposes; no production callers were found outside tests.

**Parameters:**
- `key` — any string.

**Returns:** A `uint64` hash value.

---

### `func HashKeyBytes(data []byte) uint64`

```go
func HashKeyBytes(data []byte) uint64
```

**What it does:** Computes a standalone FNV-64a hash of arbitrary bytes.

**How it's called:** Internally by `HashKeyUint64`. No external callers found outside tests.

**Parameters:**
- `data` — byte slice to hash.

**Returns:** A `uint64` hash value.

---

### `func HashKeyUint64(v uint64) uint64`

```go
func HashKeyUint64(v uint64) uint64
```

**What it does:** Encodes a `uint64` as 8 little-endian bytes, then hashes via `HashKeyBytes`. Produces a deterministic, well-distributed hash for a single integer.

**How it's called:** No callers were found in production code. Available as a utility.

**Parameters:**
- `v` — the `uint64` to hash.

**Returns:** A `uint64` hash value.

---

## `key.go`

---

### `func GenerateStorageID(displayPath string) string`

```go
func GenerateStorageID(displayPath string) string
```

**What it does:** Produces a deterministic, fixed-length internal identifier by hashing the display path with SHA-256 and hex-encoding the result. This decouples the user-facing path format from the internal storage layout.

**How it's called:** This is the **most widely used function** in the sharding package. It is called by:

| Caller                                               | Purpose                                                       |
|------------------------------------------------------|---------------------------------------------------------------|
| `internal/router/guardian_path_mapper.go:59,71,80`   | Path mapping for Guardian                                     |
| `internal/router/native_namespace.go:474`            | Namespace storage ID generation                               |
| `internal/router/ingest.go:200`                      | Ingest pipeline storage ID                                    |
| `internal/router/ui.go:1789`                         | UI-layer storage ID                                           |
| `internal/router/guardian_inject.go:63`              | Guardian inject pipeline                                      |
| `internal/client/sharded.go:508,1625,1918`           | Client-side shard key construction                            |
| `internal/client/repository_changes.go:46`           | Repository change tracking                                    |
| `internal/server/directory.go:1243`                  | Server-side directory operations                              |
| `internal/monopath/path.go:66`                       | Monopath key derivation                                       |
| `internal/fuse/commit.go:397,569,873,911,919`        | FUSE commit layer — filesystem operations                     |
| `internal/registry/monofs_client.go:309`             | Registry client                                               |

**Parameters:**
- `displayPath` — the user-visible path (e.g. `"docker-registry/doctor-query"`).

**Returns:** A 64-character hex-encoded SHA-256 digest (e.g. `"a1b2c3...f8"`).

**Implementation details:** Uses `crypto/sha256.Sum256` for one-shot hashing. The return value is always 64 hex characters.

---

### `func BuildShardKey(storageID, filePath string) string`

```go
func BuildShardKey(storageID, filePath string) string
```

**What it does:** Constructs the sharding key in the canonical format `"storageID:filePath"`. This key is fed into `HRW.GetNode()` / `HRW.GetNodes()` to deterministically assign content to nodes.

**How it's called:**

| Caller                                               | Purpose                                            |
|------------------------------------------------------|----------------------------------------------------|
| `internal/monopath/path.go:71`                       | Core path-to-shard-key transformation              |
| `internal/client/sharded.go:1717,1947`               | Client-side shard routing                          |
| `internal/client/repository_changes.go:84`           | Repository change shard key                        |

**Parameters:**
- `storageID` — the output of `GenerateStorageID`.
- `filePath` — the file path within the storage context.

**Returns:** A colon-delimited string (e.g. `"a1b2c3...f8:/images/foo.png"`).

**Implementation details:** Simple string concatenation. Both the router and the client use this identical format to guarantee that HRW hashing produces the same node assignment regardless of which component asks.
