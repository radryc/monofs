# Router Internal: Native Gateway & Workspace Documentation

This document covers all exported and unexported functions in the following files within `internal/router/`:

- `native_gateway.go`
- `native_generation.go`
- `native_namespace.go`
- `native_read.go`
- `workspace_ledger.go`
- `workspace_publish.go`
- `workspace_source_push.go`
- `workspace_sync.go`
- `workspace_sync_ui.go`

---

## Native Gateway (`native_gateway.go`)

Serves the framed native protocol from inside the router process, reusing the router's authoritative namespace view.

### Types

#### `NativeGateway`

```go
type NativeGateway struct {
    router      *Router
    logger      *slog.Logger
    nextSession atomic.Uint64
}
```

The native gateway server. Holds a reference to the router, a logger, and an atomic counter for session IDs.

#### `nativeGatewaySession`

```go
type nativeGatewaySession struct {
    id           uint64
    nextHandle   uint64
    pathByObject map[nativeproto.ObjectID]string
    objectByPath map[string]nativeproto.ObjectID
    pathByHandle map[uint64]string
}
```

Per-mount-session state. Maps between native protocol object IDs (derived from SHA-256 of path prefixes), filesystem paths, and open file handles.

---

### Exported Functions

#### `NewNativeGateway`

```go
func NewNativeGateway(r *Router, logger *slog.Logger) *NativeGateway
```

**What it does:** Constructs a new `NativeGateway` that uses the router's namespace view.

**How it's called:** Called once at startup from router initialization code to create the gateway that listens for native protocol connections.

**Parameters:**
- `r *Router` — The parent router, used for all namespace and data operations.
- `logger *slog.Logger` — Logger; if nil, `slog.Default()` is used and annotated with `component=native-gateway`.

**Returns:** `*NativeGateway` — Initialized gateway instance.

---

### Unexported Functions — NativeGateway methods

#### `(g *NativeGateway) currentGeneration`

```go
func (g *NativeGateway) currentGeneration() uint64
```

**What it does:** Returns the current effective generation by delegating to `g.router.nativeEffectiveGeneration()`.

**How it's called:** Called from `handleHello`, `handleMount`, `handleLookup`, `handleGetAttr`, `handleReadDir`, `handleStatFS`, `handleOpenRead`, `handleRead`, `handleClose`, and `dispatchFrame` to populate the `Generation` field in every response header.

**Returns:** `uint64` — Composite generation encoding cluster version in upper 32 bits and namespace generation in lower 32 bits.

---

#### `(g *NativeGateway) Serve`

```go
func (g *NativeGateway) Serve(l net.Listener) error
```

**What it does:** Accept-loop that listens on a `net.Listener` and spawns a goroutine per accepted connection. Returns nil on graceful shutdown (`net.ErrClosed`), otherwise returns the listener error.

**How it's called:** Called from router startup code after the listener is established (likely from `Router.Start` or equivalent).

**Parameters:**
- `l net.Listener` — The TCP or Unix socket listener.

**Returns:** `error` — `nil` on graceful close, otherwise the accept error.

**Implementation details:** Each connection is handled by `g.handleConn(conn)` in its own goroutine.

---

#### `(g *NativeGateway) handleConn`

```go
func (g *NativeGateway) handleConn(conn net.Conn)
```

**What it does:** Read-dispatch-write loop for a single connection. Reads framed messages, dispatches them to the appropriate opcode handler, writes the reply, and optionally manages session lifecycle (mount/unmount).

**How it's called:** Spawned by `Serve` per accepted connection.

**Parameters:**
- `conn net.Conn` — The client connection.

**Implementation details:**
- Sets a 2-minute deadline per frame with `conn.SetDeadline`.
- Reads frames using `nativeproto.ReadFrame(conn)`.
- Dispatches via `dispatchFrame`.
- After a successful mount, creates a new session and populates it with the mount response data.
- On `OpcodeUnmount`, the connection is closed and the goroutine exits.

---

#### `(g *NativeGateway) dispatchFrame`

```go
func (g *NativeGateway) dispatchFrame(ctx context.Context, session *nativeGatewaySession, hdr nativeproto.Header, body []byte) ([]byte, uint32, uint64, uint64)
```

**What it does:** Routes an incoming frame to the appropriate handler based on the opcode. This is the central dispatch switch for the native protocol.

**How it's called:** Called from `handleConn` for every received frame.

**Parameters:**
- `ctx context.Context` — Background context.
- `session *nativeGatewaySession` — Current session (may be nil before mount).
- `hdr nativeproto.Header` — Incoming frame header.
- `body []byte` — Incoming frame body (raw bytes).

**Returns:**
- `[]byte` — Encoded reply body.
- `uint32` — Status code (`nativeproto.StatusOK`, etc.).
- `uint64` — New session ID (only non-zero for mount).
- `uint64` — Generation number.

**Opcode mapping:**

| Opcode | Handler |
|--------|---------|
| `OpcodeHello` | `handleHello` |
| `OpcodeMount` | `handleMount` |
| `OpcodeUnmount` | No handler (returns OK) |
| `OpcodeLookup` | `handleLookup` |
| `OpcodeGetAttr` | `handleGetAttr` |
| `OpcodeReadDir` | `handleReadDir` |
| `OpcodeStatFS` | `handleStatFS` |
| `OpcodeOpenRead` | `handleOpenRead` |
| `OpcodeRead` | `handleRead` |
| `OpcodeClose` | `handleClose` |
| `OpcodePing` | No handler (returns OK) |
| Default | Returns `StatusUnsupported` |

---

#### `(g *NativeGateway) handleHello`

```go
func (g *NativeGateway) handleHello(body []byte) ([]byte, uint32, uint64)
```

**What it does:** Handles protocol version negotiation. Decodes the hello request, checks version compatibility, and returns a `HelloResponse` with the selected version and server capabilities.

**How it's called:** From `dispatchFrame` for `OpcodeHello`.

**Parameters:**
- `body []byte` — Encoded `HelloRequest`.

**Returns:**
- `[]byte` — Encoded `HelloResponse` with `Version1`, supported capabilities (`CapabilityRouteTTLs | CapabilityStatFS`), max frame bytes, and max read bytes.
- `uint32` — Status code.
- `uint64` — Current generation.

**Implementation details:** Rejects the request if `MinVersion > Version1` or `MaxVersion < Version1`.

---

#### `(g *NativeGateway) handleMount`

```go
func (g *NativeGateway) handleMount(ctx context.Context, body []byte) ([]byte, uint32, uint64, uint64)
```

**What it does:** Decodes the mount request, calls `router.NativeMountInfo` to get the root node metadata and TTLs, assigns a new session ID (via atomic increment), and returns an encoded `MountResponse`.

**How it's called:** From `dispatchFrame` for `OpcodeMount`.

**Parameters:**
- `ctx context.Context` — Context for the RPC call.
- `body []byte` — Encoded `MountRequest`.

**Returns:**
- `[]byte` — Encoded `MountResponse` containing cluster version, namespace generation, guardian visibility, root object ID, root attributes, and TTLs (entry, attr, dir, route) in milliseconds.
- `uint32` — Status code.
- `uint64` — New session ID (non-zero).
- `uint64` — Namespace generation.

**Implementation details:** The root object ID is computed via `pathObjectID("")`. Session IDs are generated atomically via `g.nextSession.Add(1)`.

---

#### `(g *NativeGateway) handleLookup`

```go
func (g *NativeGateway) handleLookup(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64)
```

**What it does:** Resolves a directory entry by name within a parent directory. Validates the session, decodes the request, resolves the parent path from the session's object-ID-to-path mapping, constructs the full child path, calls `router.NativeLookup`, and optionally calls `router.NativeGetAttr` for richer attributes.

**How it's called:** From `dispatchFrame` for `OpcodeLookup`.

**Parameters:**
- `ctx context.Context` — Context for RPC calls.
- `session *nativeGatewaySession` — Must be non-nil and match the session ID.
- `sessionID uint64` — Expected session ID.
- `body []byte` — Encoded `LookupRequest`.

**Returns:**
- `[]byte` — Encoded `LookupResponse` with found status, entry TTL, object ID, and attributes.
- `uint32` — Status code.
- `uint64` — Generation.

**Implementation details:**
- Rejects empty or slash-containing names as invalid.
- Falls back to `attrFromLookup` if `GetAttr` fails after the lookup succeeds.
- New child paths are bound into the session via `session.bindPath(path)`.

---

#### `(g *NativeGateway) handleGetAttr`

```go
func (g *NativeGateway) handleGetAttr(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64)
```

**What it does:** Fetches file/directory metadata for a given object ID. Validates the session, resolves the object ID to a path, calls `router.NativeGetAttr`, and returns the encoded response.

**How it's called:** From `dispatchFrame` for `OpcodeGetAttr`.

**Parameters:**
- `ctx context.Context` — Context for the RPC call.
- `session *nativeGatewaySession` — Must be non-nil and match the session ID.
- `sessionID uint64` — Expected session ID.
- `body []byte` — Encoded `GetAttrRequest`.

**Returns:**
- `[]byte` — Encoded `GetAttrResponse` with found status, attr TTL, and file attributes (ino, mode, size, mtime, atime, ctime, nlink, uid, gid).
- `uint32` — Status code.
- `uint64` — Generation.

---

#### `(g *NativeGateway) handleReadDir`

```go
func (g *NativeGateway) handleReadDir(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64)
```

**What it does:** Reads a directory listing with pagination support. Validates the session, calls `router.NativeReadDir` for the authoritative merged directory listing across all healthy nodes, applies pagination using a cookie-based cursor, and returns the encoded response.

**How it's called:** From `dispatchFrame` for `OpcodeReadDir`.

**Parameters:**
- `ctx context.Context` — Context for the RPC call.
- `session *nativeGatewaySession` — Must be non-nil and match the session ID.
- `sessionID uint64` — Expected session ID.
- `body []byte` — Encoded `ReadDirRequest` containing `DirObjectID`, `Cookie`, and `MaxEntries`.

**Returns:**
- `[]byte` — Encoded `ReadDirResponse` with entries, next cookie, EOF flag, and dir TTL.
- `uint32` — Status code.
- `uint64` — Generation.

**Implementation details:**
- Cookie-based pagination: `start = int(req.Cookie)`, entries sliced `entries[start : start+limit]`.
- Each entry's child path is bound into the session for future lookups.

---

#### `(g *NativeGateway) handleStatFS`

```go
func (g *NativeGateway) handleStatFS(ctx context.Context, session *nativeGatewaySession, sessionID uint64) ([]byte, uint32, uint64)
```

**What it does:** Returns filesystem statistics aggregated from the router's healthy active nodes. Validates the session and calls `router.NativeStatFS`.

**How it's called:** From `dispatchFrame` for `OpcodeStatFS`.

**Parameters:**
- `ctx context.Context` — Context.
- `session *nativeGatewaySession` — Must be non-nil and match the session ID.
- `sessionID uint64` — Expected session ID.

**Returns:**
- `[]byte` — Encoded `StatFSResponse` with blocks, free blocks, available blocks, total files, free files, block size, fragment size, and max name length.
- `uint32` — Status code.
- `uint64` — Generation (from `statfs.NamespaceGeneration`).

---

#### `(g *NativeGateway) handleOpenRead`

```go
func (g *NativeGateway) handleOpenRead(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64)
```

**What it does:** Opens a file for reading. Validates the session, resolves the path from the object ID, calls `router.NativeGetAttr` to verify the entry exists and is not a directory, binds a file handle, and returns it.

**How it's called:** From `dispatchFrame` for `OpcodeOpenRead`.

**Parameters:**
- `ctx context.Context` — Context for RPC call.
- `session *nativeGatewaySession` — Must be non-nil and match the session ID.
- `sessionID uint64` — Expected session ID.
- `body []byte` — Encoded `OpenReadRequest` with `ObjectID`.

**Returns:**
- `[]byte` — Encoded `OpenReadResponse` with handle ID, file attributes, and route TTL.
- `uint32` — Status code (`StatusNotFound` if not found, `StatusIsDir` if a directory).
- `uint64` — Generation.

**Implementation details:** Checks `attr.GetMode()&S_IFMT == S_IFDIR` to reject directories.

---

#### `(g *NativeGateway) handleRead`

```go
func (g *NativeGateway) handleRead(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64)
```

**What it does:** Reads file data at a given offset and length. Validates the session, resolves the path from the handle ID, calls `router.NativeRead`.

**How it's called:** From `dispatchFrame` for `OpcodeRead`.

**Parameters:**
- `ctx context.Context` — Context for RPC call.
- `session *nativeGatewaySession` — Must be non-nil and match the session ID.
- `sessionID uint64` — Expected session ID.
- `body []byte` — Encoded `ReadRequest` with `HandleID`, `Offset`, `Length`.

**Returns:**
- `[]byte` — Encoded `ReadResponse` with data and EOF flag.
- `uint32` — Status code.
- `uint64` — Generation.

---

#### `(g *NativeGateway) handleClose`

```go
func (g *NativeGateway) handleClose(session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64)
```

**What it does:** Releases an open file handle. Validates the session and calls `session.releaseHandle`.

**How it's called:** From `dispatchFrame` for `OpcodeClose`.

**Parameters:**
- `session *nativeGatewaySession` — Must be non-nil and match the session ID.
- `sessionID uint64` — Expected session ID.
- `body []byte` — Encoded `CloseRequest` with `HandleID`.

**Returns:**
- `[]byte` — nil (no body).
- `uint32` — Status code.
- `uint64` — Generation.

---

#### `(g *NativeGateway) newSession`

```go
func (g *NativeGateway) newSession(id uint64) *nativeGatewaySession
```

**What it does:** Allocates a new, empty session with initialized maps for path-to-object, object-to-path, and path-to-handle mappings.

**How it's called:** From `handleConn` after a successful mount handshake.

**Parameters:**
- `id uint64` — The session ID assigned during mount.

**Returns:** `*nativeGatewaySession` — Initialized session.

---

#### `(g *NativeGateway) populateMountSession`

```go
func (g *NativeGateway) populateMountSession(session *nativeGatewaySession, body []byte) error
```

**What it does:** Populates a new session with the root directory mapping. Decodes the mount response body and inserts the root object ID (mapped to the empty path `""`) into both `pathByObject` and `objectByPath`.

**How it's called:** From `handleConn` after creating a new session, using the already-encoded mount response body.

**Parameters:**
- `session *nativeGatewaySession` — The session to populate.
- `body []byte` — Encoded `MountResponse` bytes.

**Returns:** `error` — Decode error, if any.

---

### Unexported Functions — nativeGatewaySession methods

#### `(s *nativeGatewaySession) bindPath`

```go
func (s *nativeGatewaySession) bindPath(path string) nativeproto.ObjectID
```

**What it does:** Returns an existing or newly-created object ID for a filesystem path. If the path already has an object ID, returns it; otherwise computes one via `pathObjectID(path)`, stores the bidirectional mapping, and returns the new ID.

**How it's called:** From `handleLookup` and `handleReadDir` to register child paths, and from `populateMountSession` indirectly.

**Parameters:**
- `path string` — The filesystem path.

**Returns:** `nativeproto.ObjectID` — The object ID (16-byte SHA-256 truncated hash of `"monofs-native-path:" + path`).

---

#### `(s *nativeGatewaySession) pathForObject`

```go
func (s *nativeGatewaySession) pathForObject(objectID nativeproto.ObjectID) (string, bool)
```

**What it does:** Looks up a filesystem path given an object ID.

**How it's called:** From `handleLookup`, `handleGetAttr`, `handleReadDir`, and `handleOpenRead` to resolve object IDs back to paths.

**Parameters:**
- `objectID nativeproto.ObjectID` — The 16-byte object ID.

**Returns:**
- `string` — The filesystem path.
- `bool` — Whether the mapping exists.

---

#### `(s *nativeGatewaySession) bindHandle`

```go
func (s *nativeGatewaySession) bindHandle(path string) uint64
```

**What it does:** Allocates a new file handle ID for a path by incrementing the session's `nextHandle` counter.

**How it's called:** From `handleOpenRead` to create a handle for subsequent `Read`/`Close` operations.

**Parameters:**
- `path string` — The filesystem path to associate with the handle.

**Returns:** `uint64` — The handle ID.

---

#### `(s *nativeGatewaySession) pathForHandle`

```go
func (s *nativeGatewaySession) pathForHandle(handleID uint64) (string, bool)
```

**What it does:** Looks up a path given a handle ID.

**How it's called:** From `handleRead` to resolve the handle back to a path for the actual read.

**Parameters:**
- `handleID uint64` — The handle ID.

**Returns:**
- `string` — The filesystem path.
- `bool` — Whether the handle exists.

---

#### `(s *nativeGatewaySession) releaseHandle`

```go
func (s *nativeGatewaySession) releaseHandle(handleID uint64)
```

**What it does:** Deletes a handle from the session's handle-to-path map.

**How it's called:** From `handleClose`.

**Parameters:**
- `handleID uint64` — The handle to release.

---

### Unexported Package-Level Functions

#### `pathObjectID`

```go
func pathObjectID(path string) nativeproto.ObjectID
```

**What it does:** Computes a deterministic 16-byte object ID for a path by SHA-256 hashing the string `"monofs-native-path:" + path` and truncating to the first 16 bytes.

**How it's called:** From `session.bindPath` and from `handleMount` (for the empty-string root path).

**Parameters:**
- `path string` — The filesystem path.

**Returns:** `nativeproto.ObjectID` — 16-byte deterministic object ID.

---

#### `attrFromLookup`

```go
func attrFromLookup(resp *pb.LookupResponse) nativeproto.Attr
```

**What it does:** Converts a protobuf `LookupResponse` to a native protocol `Attr` (fields: Ino, Mode, Size, Mtime only).

**How it's called:** From `handleLookup` when `GetAttr` is unavailable after a successful `Lookup`.

**Parameters:**
- `resp *pb.LookupResponse` — The lookup result from the router.

**Returns:** `nativeproto.Attr` — Converted attributes.

---

#### `attrFromGetAttr`

```go
func attrFromGetAttr(resp *pb.GetAttrResponse) nativeproto.Attr
```

**What it does:** Converts a protobuf `GetAttrResponse` to a native protocol `Attr` (full fields: Ino, Mode, Size, Mtime, Atime, Ctime, Nlink, UID, GID).

**How it's called:** From `handleLookup`, `handleGetAttr`, and `handleOpenRead`.

**Parameters:**
- `resp *pb.GetAttrResponse` — The getattr result from the router.

**Returns:** `nativeproto.Attr` — Converted attributes.

---

#### `durationMS`

```go
func durationMS(d time.Duration) uint32
```

**What it does:** Converts a Go `time.Duration` to milliseconds as `uint32`.

**How it's called:** From `handleMount`, `handleLookup`, `handleGetAttr`, `handleReadDir`, and `handleOpenRead` to populate TTL fields in response messages.

**Parameters:**
- `d time.Duration` — The duration.

**Returns:** `uint32` — Duration in milliseconds.

---

#### `mapErrorStatus`

```go
func mapErrorStatus(err error) uint32
```

**What it does:** Maps a Go error to a native protocol status code. `context.Canceled` and `context.DeadlineExceeded` map to `StatusCancelled`; all other errors map to `StatusUnavailable`.

**How it's called:** From `handleMount`, `handleLookup`, `handleGetAttr`, `handleReadDir`, `handleStatFS`, `handleOpenRead`, and `handleRead` when router methods return errors.

**Parameters:**
- `err error` — The error to map.

**Returns:** `uint32` — Native protocol status code.

---

## Native Generation (`native_generation.go`)

Encapsulates generation-tracking logic for the native namespace, composing a cluster version and a monotonic namespace counter into a single 64-bit value.

### Unexported Functions

#### `(r *Router) nativeNamespaceGeneration`

```go
func (r *Router) nativeNamespaceGeneration() uint64
```

**What it does:** Returns the current namespace generation counter from the router's atomic `namespaceGeneration` field. If it has never been set (zero), returns 1 as the floor.

**How it's called:** From `nativeEffectiveGeneration` and `NativeStatFS`.

**Returns:** `uint64` — The monotonic namespace generation, minimum 1.

---

#### `(r *Router) nativeEffectiveGeneration`

```go
func (r *Router) nativeEffectiveGeneration() uint64
```

**What it does:** Composes the cluster version (upper 32 bits) and the namespace generation (lower 32 bits) into a single 64-bit integer. This allows the client to detect both cluster membership changes and namespace changes from a single value.

**How it's called:** From `NativeGateway.currentGeneration()`, `NativeMountInfo`, `NativeLookup`, `NativeGetAttr`, `NativeReadDir`, `NativeStatFS`, and all native gateway handler methods.

**Returns:** `uint64` — `(version << 32) | (namespaceGeneration & 0xffffffff)`.

---

#### `(r *Router) bumpNativeNamespaceGeneration`

```go
func (r *Router) bumpNativeNamespaceGeneration(reason string) uint64
```

**What it does:** Atomically increments the namespace generation and returns the new value. Logs at debug level if a reason is provided.

**How it's called:** Called from other router methods when the namespace state changes (e.g., after a workspace sync, ingest, or mount/unmount), invalidating all client sessions that hold a stale generation.

**Parameters:**
- `reason string` — Description of why the generation was bumped (may be empty).

**Returns:** `uint64` — The new (post-increment) generation.

---

## Native Namespace (`native_namespace.go`)

Provides authoritative namespace resolution backed by the router's view of healthy storage nodes, using HRW (Highest Random Weight) consistent hashing for path-based target selection with fallback semantics.

### Constants

| Constant | Value | Description |
|----------|-------|-------------|
| `nativeNamespaceRPCTimeout` | `5s` | Per-node RPC timeout for namespace operations. |
| `nativeNamespaceEntryTTL` | `1s` | Default entry cache TTL. |
| `nativeNamespaceAttrTTL` | `1s` | Default attribute cache TTL. |
| `nativeNamespaceDirTTL` | `1s` | Default directory listing cache TTL. |
| `nativeNamespaceRouteTTL` | `30s` | Default routing TTL. |
| `nativeNamespaceBlockSize` | `fsstat.BlockSize` | Block size for statfs calculations. |

### Types

#### `NativeTTLConfig`

```go
type NativeTTLConfig struct {
    EntryTTL time.Duration
    AttrTTL  time.Duration
    DirTTL   time.Duration
    RouteTTL time.Duration
}
```

Describes default cache lifetimes that the native gateway advertises to a kernel client during mount negotiation.

#### `NativeMountInfo`

```go
type NativeMountInfo struct {
    ClusterVersion      int64
    NamespaceGeneration int64
    GuardianVisible     bool
    Root                *pb.GetAttrResponse
    TTLs                NativeTTLConfig
}
```

Initial namespace snapshot returned by the native gateway mount handshake. Contains cluster membership version, namespace generation, whether guardian is visible, root directory attributes, and default TTLs.

#### `NativeStatFS`

```go
type NativeStatFS struct {
    Blocks              uint64
    Bfree               uint64
    Bavail              uint64
    Files               uint64
    Ffree               uint64
    Bsize               uint32
    Frsize              uint32
    NameLen             uint32
    ClusterVersion      int64
    NamespaceGeneration int64
}
```

Filesystem statistics for native clients. Aggregated across all active storage nodes.

#### `nativeNodeTarget`

```go
type nativeNodeTarget struct {
    id     string
    node   sharding.Node
    client pb.MonoFSClient
}
```

Internal target descriptor bundling a node's string ID, sharding metadata, and its gRPC client.

---

### Exported Functions

#### `DefaultNativeTTLConfig`

```go
func DefaultNativeTTLConfig() NativeTTLConfig
```

**What it does:** Returns the current default TTL policy for native namespace/data path operations: `EntryTTL=1s`, `AttrTTL=1s`, `DirTTL=1s`, `RouteTTL=30s`.

**How it's called:** From `NativeGateway.handleMount`, `handleLookup`, `handleGetAttr`, `handleReadDir`, `handleOpenRead`, and `NativeMountInfo`.

**Returns:** `NativeTTLConfig` — Default TTL values.

---

#### `(r *Router) NativeMountInfo`

```go
func (r *Router) NativeMountInfo(ctx context.Context) (*NativeMountInfo, error)
```

**What it does:** Returns the initial root metadata and cache policy surfaced during mount negotiation. Checks context validity and that there is at least one healthy node. Constructs a synthetic root `GetAttrResponse` with `Ino=1`, mode `0755|S_IFDIR`, and current timestamps.

**How it's called:** From `NativeGateway.handleMount`.

**Parameters:**
- `ctx context.Context` — Context for cancellation/timeout.

**Returns:**
- `*NativeMountInfo` — Cluster version, namespace generation, guardian visibility, root attributes, and TTLs.
- `error` — Context error or "no healthy nodes available".

---

#### `(r *Router) NativeLookup`

```go
func (r *Router) NativeLookup(ctx context.Context, path string) (*pb.LookupResponse, int64, error)
```

**What it does:** Resolves a path using authoritative fallback semantics. First tries HRW-ranked targets (up to 3), then remaining healthy nodes, then route-based targets. Returns the first "found" response or a "not found" response.

**How it's called:** From `NativeGateway.handleLookup`.

**Parameters:**
- `ctx context.Context` — Context for RPC calls.
- `path string` — The path to resolve (sent as `ParentPath` with empty `Name`).

**Returns:**
- `*pb.LookupResponse` — The lookup result (may have `Found=false`).
- `int64` — Namespace generation at the time of the call.
- `error` — Context error, "no healthy nodes", or last RPC error.

**Implementation details:**
- Three-tier fallback: ranked targets -> healthy targets (excluding primary) -> route targets.
- Each RPC uses `nativeNamespaceRPCTimeout` (5s) deadline.
- If no target finds the entry and the last error was non-nil, route targets are also tried.

---

#### `(r *Router) NativeGetAttr`

```go
func (r *Router) NativeGetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, int64, error)
```

**What it does:** Fetches metadata for a path using the same three-tier fallback as `NativeLookup`: ranked targets, remaining healthy targets, then route targets. Returns the first "found" response.

**How it's called:** From `NativeGateway.handleGetAttr`, `handleLookup` (for richer attributes), and `handleOpenRead`.

**Parameters:**
- `ctx context.Context` — Context for RPC calls.
- `path string` — The path to get attributes for.

**Returns:**
- `*pb.GetAttrResponse` — Metadata (may have `Found=false`).
- `int64` — Namespace generation.
- `error` — Context error, "no healthy nodes", or last RPC error.

---

#### `(r *Router) NativeReadDir`

```go
func (r *Router) NativeReadDir(ctx context.Context, path string) ([]*pb.DirEntry, int64, error)
```

**What it does:** Returns the authoritative merged directory listing across all healthy nodes. It never returns a partial listing as success — if any node fails, it retries failed nodes; if retries fail, it returns an error.

**How it's called:** From `NativeGateway.handleReadDir`.

**Parameters:**
- `ctx context.Context` — Context for RPC calls.
- `path string` — The directory path to list.

**Returns:**
- `[]*pb.DirEntry` — Deduplicated, sorted list of directory entries.
- `int64` — Namespace generation.
- `error` — Context error, "no healthy nodes", or per-node failure.

**Implementation details:**
- Fan-out to all healthy nodes concurrently using goroutines + `sync.WaitGroup`.
- Entries are deduplicated by name across nodes.
- On node failures, retries failed nodes; if still failing, returns an error with the node ID.
- Results are sorted alphabetically by name.

---

#### `(r *Router) NativeStatFS`

```go
func (r *Router) NativeStatFS(ctx context.Context) (*NativeStatFS, error)
```

**What it does:** Aggregates filesystem statistics from the router's healthy active nodes. Computes total used bytes and total files across all active nodes, then derives block counts using `fsstat.FromUsage`. Requires at least one active node.

**How it's called:** From `NativeGateway.handleStatFS`.

**Parameters:**
- `ctx context.Context` — Context for cancellation.

**Returns:**
- `*NativeStatFS` — Aggregated filesystem statistics.
- `error` — Context error or "no active nodes available".

**Implementation details:**
- Holds `r.mu.RLock()` for reading node state.
- Uses `maxInt64(state.diskUsedBytes, state.info.GetDiskUsedBytes())` and `maxInt64(state.ownedFilesCount, state.info.GetTotalFiles())` for each active node.

---

### Unexported Functions

#### `(r *Router) nativeHealthyTargets`

```go
func (r *Router) nativeHealthyTargets() []nativeNodeTarget
```

**What it does:** Collects all currently healthy and active nodes into a sorted slice of `nativeNodeTarget`. Filters by: non-nil state, non-nil info, non-nil client, `Healthy == true`, `status == NodeActive`.

**How it's called:** From `NativeMountInfo`, `nativeRankedTargets`, `nativeHealthyTargetsExcept`, `NativeReadDir`, `NativeStatFS`, and `nativeRouteTargets`.

**Returns:** `[]nativeNodeTarget` — Sorted (by node ID) slice of healthy targets.

**Implementation details:** Holds `r.mu.RLock()`. If a node's weight is zero, defaults it to 1.

---

#### `(r *Router) nativeHealthyTargetsExcept`

```go
func (r *Router) nativeHealthyTargetsExcept(excludeNodeID string) []nativeNodeTarget
```

**What it does:** Returns all healthy targets except the one with the specified node ID.

**How it's called:** From `NativeLookup` and `NativeGetAttr` for the second-tier fallback (trying remaining nodes after the primary ranked target).

**Parameters:**
- `excludeNodeID string` — The node ID to exclude.

**Returns:** `[]nativeNodeTarget` — Filtered healthy targets.

---

#### `(r *Router) nativeRankedTargets`

```go
func (r *Router) nativeRankedTargets(path string, max int) ([]nativeNodeTarget, string)
```

**What it does:** Ranks healthy targets using HRW (Highest Random Weight) consistent hashing based on the path's shard key. Returns up to `max` top-ranked targets.

**How it's called:** From `NativeLookup`, `NativeGetAttr`, and `NativeRead` as the primary tier for path-based resolution.

**Parameters:**
- `path string` — The filesystem path, converted to a shard key via `monopath.BuildShardKey`.
- `max int` — Maximum number of ranked targets to return.

**Returns:**
- `[]nativeNodeTarget` — Ranked targets.
- `string` — The computed shard key (for debugging).

---

#### `(r *Router) nativeRouteTargets`

```go
func (r *Router) nativeRouteTargets(ctx context.Context, path string) []nativeNodeTarget
```

**What it does:** Determines targets based on the monopath route table. Splits the path into display-path and file-path components, generates a storage ID, calls `router.GetNodeForFile`, and orders targets by the response's primary node ID followed by fallback node IDs.

**How it's called:** From `NativeLookup`, `NativeGetAttr`, and `NativeRead` as the final fallback tier.

**Parameters:**
- `ctx context.Context` — Context for the `GetNodeForFile` RPC.
- `path string` — The filesystem path.

**Returns:** `[]nativeNodeTarget` — Route-ordered targets, deduplicated. Returns nil if the path cannot be split or the route lookup fails.

---

#### `(r *Router) nativeReadDirFromNode`

```go
func (r *Router) nativeReadDirFromNode(ctx context.Context, client pb.MonoFSClient, nodeID, path string) ([]*pb.DirEntry, error)
```

**What it does:** Streams a directory listing from a single storage node. Opens a `ReadDir` gRPC streaming call and collects all entries.

**How it's called:** From `NativeReadDir` for each healthy node concurrently.

**Parameters:**
- `ctx context.Context` — Context; a 5-second timeout child context is created internally.
- `client pb.MonoFSClient` — gRPC client for the target node.
- `nodeID string` — Node ID (for error messages).
- `path string` — Directory path.

**Returns:**
- `[]*pb.DirEntry` — All entries from the node.
- `error` — RPC or stream error.

---

### Unexported Helper Functions

#### `ceilDiv`

```go
func ceilDiv(value uint64, divisor uint32) uint64
```

**What it does:** Computes `ceil(value / divisor)` for unsigned types. Returns 0 if value is 0.

#### `maxInt64`

```go
func maxInt64(a, b int64) int64
```

**What it does:** Returns the larger of two `int64` values.

#### `uint64FromInt64`

```go
func uint64FromInt64(v int64) uint64
```

**What it does:** Converts int64 to uint64, returning 0 for negative or zero values. Used as a safe cast when aggregating disk usage and file counts.

---

## Native Read (`native_read.go`)

Provides data-read operations for the native gateway using the same HRW/failover routing as existing userspace clients.

### Exported Functions

#### `(r *Router) NativeRead`

```go
func (r *Router) NativeRead(ctx context.Context, path string, offset, size int64) ([]byte, error)
```

**What it does:** Returns file bytes using HRW-ranked targets with fallback to route targets. Increments `routerNativeReadOpsTotal` and `routerNativeReadBytesTotal` Prometheus counters on success.

**How it's called:** From `NativeGateway.handleRead`.

**Parameters:**
- `ctx context.Context` — Context for RPC calls.
- `path string` — The file path.
- `offset int64` — Byte offset to start reading from.
- `size int64` — Number of bytes to read.

**Returns:**
- `[]byte` — The file data.
- `error` — "no healthy nodes available", context error, or last RPC error.

**Implementation details:** Two-tier fallback: ranked targets -> route targets.

---

### Unexported Functions

#### `(r *Router) nativeReadFromTarget`

```go
func (r *Router) nativeReadFromTarget(ctx context.Context, client pb.MonoFSClient, nodeID, path string, offset, size int64) ([]byte, error)
```

**What it does:** Reads file data from a single storage node via gRPC streaming `Read`. Collects all chunks into a single byte slice.

**How it's called:** From `NativeRead` for each target in the ranked and route fallback lists.

**Parameters:**
- `ctx context.Context` — Context; a 5-second timeout child context is created internally.
- `client pb.MonoFSClient` — gRPC client for the target node.
- `nodeID string` — Node ID (for error messages).
- `path string` — File path.
- `offset int64` — Byte offset.
- `size int64` — Number of bytes.

**Returns:**
- `[]byte` — All data chunks collected from the stream.
- `error` — RPC or stream error.

---

## Workspace Ledger (`workspace_ledger.go`)

Implements the router-side ledger query and append logic, routing to the appropriate storage node using HRW hashing on `(workspaceID|clientID)`.

### Exported Functions

#### `(r *Router) QueryLedger`

```go
func (r *Router) QueryLedger(ctx context.Context, req *pb.QueryLedgerRequest) (*pb.QueryLedgerResponse, error)
```

**What it does:** Routes a ledger query to the appropriate node. If a workspace ID is provided, routes to the HRW-selected ledger node for that workspace. If no workspace ID, fans out to all healthy nodes and merges results.

**How it's called:** Exposed as a gRPC method on `MonoFSRouterServer`; called by clients querying the distributed ledger.

**Parameters:**
- `ctx context.Context` — Context.
- `req *pb.QueryLedgerRequest` — Must not be nil (defaults to empty request).

**Returns:**
- `*pb.QueryLedgerResponse` — Query result.
- `error` — Routing or RPC error.

---

### Unexported Functions

#### `(r *Router) appendPushOutcome`

```go
func (r *Router) appendPushOutcome(ctx context.Context, outcome *pb.PushOutcome) error
```

**What it does:** Appends a push outcome to the distributed ledger. Determines ledger node clients via HRW on `(workspaceID|clientID)`, tries each candidate in ranked order, and succeeds on the first successful append. Returns an error if all candidates fail.

**How it's called:** From `runWorkspaceCommitPushJob` after each repository result in a source push.

**Parameters:**
- `ctx context.Context` — Context.
- `outcome *pb.PushOutcome` — The push outcome to record. Must have a non-empty `WorkspaceId`.

**Returns:** `error` — Routing error or "all candidate nodes rejected append".

---

#### `(r *Router) queryLedgerFanout`

```go
func (r *Router) queryLedgerFanout(ctx context.Context, req *pb.QueryLedgerRequest) (*pb.QueryLedgerResponse, error)
```

**What it does:** Fans out a ledger query to all healthy nodes and merges their responses. Combines commits, push outcomes, and refresh events. Sums `TotalMatches`.

**How it's called:** From `QueryLedger` when no workspace ID is specified.

**Parameters:**
- `ctx context.Context` — Context.
- `req *pb.QueryLedgerRequest` — The query request.

**Returns:**
- `*pb.QueryLedgerResponse` — Merged response.
- `error` — "no healthy nodes available" or "no ledger responses available" (if all nodes returned empty or errored).

---

#### `(r *Router) ledgerNodeClient`

```go
func (r *Router) ledgerNodeClient(ctx context.Context, workspaceID string) (pb.MonoFSClient, error)
```

**What it does:** Returns the top-ranked ledger node client for a workspace.

**How it's called:** From `QueryLedger` for workspace-scoped queries.

**Parameters:**
- `ctx context.Context` — Context for extracting client ID.
- `workspaceID string` — The workspace ID.

**Returns:**
- `pb.MonoFSClient` — The gRPC client for the top-ranked node.
- `error` — Routing error.

**Implementation details:** Delegates to `ledgerNodeClients` and returns the first result.

---

#### `(r *Router) ledgerNodeClients`

```go
func (r *Router) ledgerNodeClients(ctx context.Context, workspaceID string) ([]pb.MonoFSClient, error)
```

**What it does:** Ranks all healthy active nodes using HRW hashing with the key `workspaceID + "|" + clientID`. The client ID is extracted from the context via `extractClientID(ctx)` (defaults to "anonymous").

**How it's called:** From `ledgerNodeClient` and `appendPushOutcome`.

**Parameters:**
- `ctx context.Context` — Context for extracting client ID.
- `workspaceID string` — The workspace ID.

**Returns:**
- `[]pb.MonoFSClient` — Ranked list of gRPC clients.
- `error` — "no healthy nodes available" if no nodes match.

---

## Workspace Publish (`workspace_publish.go`)

Handles workspace bundle upload and the publish workflow: accepting a bundled workspace, staging it with a fetcher cluster, and streaming progress events back to the caller.

### Constants

| Constant | Value | Description |
|----------|-------|-------------|
| `workspaceBundleTTL` | `30m` | How long a staged bundle remains valid before expiry. |

### Types

#### `stagedWorkspaceBundle`

```go
type stagedWorkspaceBundle struct {
    bundleID     string
    workspaceID  string
    data         []byte
    bundle       *workspacebundle.Bundle
    commitBundle *workspacebundle.SourceCommitBundle
    createdAt    time.Time
    expiresAt    time.Time
}
```

Represents a workspace bundle (or commit bundle) that has been uploaded and is staged in memory, awaiting a publish or push operation. A bundle may be for either a publish (using `bundle`) or a source push (using `commitBundle`).

---

### Exported Functions

#### `(r *Router) UploadWorkspaceBundle`

```go
func (r *Router) UploadWorkspaceBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.UploadWorkspaceBundleResponse]) error
```

**What it does:** Receives a workspace bundle via client-streaming gRPC. Accumulates chunks, verifies workspace ID consistency, parses the bundle, and stores it as a `stagedWorkspaceBundle` with a 30-minute TTL. Returns a response with the bundle ID, workspace ID, byte count, repository count, and expiry time.

**How it's called:** gRPC service method `MonoFSRouter.UploadWorkspaceBundle` (client-streaming).

**Parameters:**
- `stream` — gRPC client-streaming server interface.

**Returns:** `error` — Validation or storage error.

**Implementation details:**
- Verifies that `workspace_id` does not change mid-upload.
- Verifies that the parsed bundle's workspace ID matches the stream's workspace ID.
- Stores via `r.storeWorkspaceBundle(entry)`.
- Increments `routerWorkspaceSyncBundleBytesTotal` counter.

---

#### `(r *Router) PublishWorkspace`

```go
func (r *Router) PublishWorkspace(req *pb.PublishWorkspaceRequest, stream pb.MonoFSRouter_PublishWorkspaceServer) error
```

**What it does:** Initiates and streams a workspace publish job. Looks up the staged bundle, creates a publish job, stores it, and then runs the publish workflow via `runWorkspacePublishJob`. Sends initial "job accepted" event before starting the run.

**How it's called:** gRPC service method `MonoFSRouter.PublishWorkspace` (server-streaming).

**Parameters:**
- `req *pb.PublishWorkspaceRequest` — Contains `BundleId`, optional `WorkspaceId`, `LogicalCommitMessage`, `AuthorName`, `AuthorEmail`, `RequestedBranchStrategy`.
- `stream` — gRPC server-streaming interface for progress events.

**Returns:** `error` — Bundle not found, ID mismatch, or publish failure.

**Implementation details:**
- Tracks `routerWorkspaceSyncJobsTotal`, `routerWorkspaceSyncActiveJobs`, `routerWorkspaceSyncDurationSeconds` metrics.
- Created job has state `QUEUED`, then transitions to `RUNNING` in `runWorkspacePublishJob`.

---

### Unexported Functions

#### `(r *Router) runWorkspacePublishJob`

```go
func (r *Router) runWorkspacePublishJob(ctx context.Context, entry *workspaceSyncJobEntry, req *pb.PublishWorkspaceRequest, bundleEntry *stagedWorkspaceBundle, send func(*pb.WorkspaceSyncEvent) error) error
```

**What it does:** The core publish execution flow. Transitions the job to `RUNNING`, gets the fetcher client, stages the workspace bundle on the fetcher cluster, calls `StartWorkspacePublish` on the fetcher, and streams each repository result back as progress events. On completion, finalizes the job and sends a terminal event.

**How it's called:** From `PublishWorkspace`.

**Parameters:**
- `ctx context.Context` — Cancellable context derived from the stream.
- `entry *workspaceSyncJobEntry` — The job entry.
- `req *pb.PublishWorkspaceRequest` — The publish request.
- `bundleEntry *stagedWorkspaceBundle` — The staged bundle.
- `send func(*pb.WorkspaceSyncEvent) error` — Callback to stream events to the client.

**Returns:** `error` — Fetcher unavailable, stage failure, publish failure, or stream-send error.

**Implementation details:**
- Defers `fetcherClient.DiscardWorkspaceBundle` to clean up.
- After `StartWorkspacePublish`, iterates over results and calls `r.updateWorkspaceSyncRepository` for each.
- Finalizes via `r.finalizeWorkspaceSyncJob`.

---

#### `(r *Router) newWorkspacePublishJob`

```go
func (r *Router) newWorkspacePublishJob(req *pb.PublishWorkspaceRequest, bundle *workspacebundle.Bundle, clientID string) *pb.WorkspaceSyncJob
```

**What it does:** Creates a new `WorkspaceSyncJob` in `QUEUED` state for a publish action. Generates a unique job ID (`wsync-<nanosecond timestamp>`), sets the repository count from the bundle, and populates the logical commit message.

**How it's called:** From `PublishWorkspace`.

**Parameters:**
- `req *pb.PublishWorkspaceRequest` — The publish request.
- `bundle *workspacebundle.Bundle` — The parsed bundle (may be nil).
- `clientID string` — The client ID extracted from the gRPC context.

**Returns:** `*pb.WorkspaceSyncJob` — The initialized job.

---

#### `(r *Router) storeWorkspaceBundle`

```go
func (r *Router) storeWorkspaceBundle(entry *stagedWorkspaceBundle) error
```

**What it does:** Persists a staged workspace bundle. If `r.workspaceJobStore` is configured, also writes bundle metadata (ID, workspace ID, kind, byte size, repo count, local commit IDs, timestamps). Stores in the in-memory map `r.workspaceBundles` under the bundle ID.

**How it's called:** From `UploadWorkspaceBundle` and `UploadWorkspaceCommitBundle`.

**Parameters:**
- `entry *stagedWorkspaceBundle` — The staged bundle to store.

**Returns:** `error` — Persistence error.

**Implementation details:**
- Determines the kind: `"publish"` for regular bundles, `"source_push"` for commit bundles.
- Holds `r.workspaceBundleMu.Lock()` for the in-memory map.

---

#### `(r *Router) getWorkspaceBundle`

```go
func (r *Router) getWorkspaceBundle(bundleID string) *stagedWorkspaceBundle
```

**What it does:** Retrieves a staged bundle by ID. If the bundle has expired (past `expiresAt`), removes it from the map and returns nil.

**How it's called:** From `PublishWorkspace` and `PushWorkspaceCommits`.

**Parameters:**
- `bundleID string` — The bundle ID.

**Returns:** `*stagedWorkspaceBundle` — The bundle, or nil if not found or expired.

---

#### `sendWorkspaceSyncTerminalEvent`

```go
func sendWorkspaceSyncTerminalEvent(send func(*pb.WorkspaceSyncEvent) error, entry *workspaceSyncJobEntry, message string) error
```

**What it does:** Sends a terminal `JOB_COMPLETED` event to the stream. Used as the final event in both publish and push workflows.

**How it's called:** From `runWorkspacePublishJob`, `runWorkspaceCommitPushJob`, and various error paths in publish/push flows.

**Parameters:**
- `send func(*pb.WorkspaceSyncEvent) error` — The stream-send callback.
- `entry *workspaceSyncJobEntry` — The job entry (for its snapshot).
- `message string` — Human-readable completion message.

**Returns:** `error` — Stream-send error.

---

## Workspace Source Push (`workspace_source_push.go`)

Handles uploading source commit bundles and executing source-push jobs with policy evaluation and subtree ownership gating.

### Exported Functions

#### `(r *Router) UploadWorkspaceCommitBundle`

```go
func (r *Router) UploadWorkspaceCommitBundle(stream grpc.ClientStreamingServer[pb.WorkspaceBundleChunk, pb.UploadWorkspaceBundleResponse]) error
```

**What it does:** Receives a commit bundle (source code with commit metadata) via client-streaming gRPC. Accumulates chunks, validates workspace ID, parses using `workspacebundle.ParseSourceCommitBundle`, and stores it as a `stagedWorkspaceBundle` with the `commitBundle` field populated. Returns a response with bundle ID, workspace ID, byte count, repository count (from `RepositoryRefs()`), and expiry time.

**How it's called:** gRPC service method `MonoFSRouter.UploadWorkspaceCommitBundle` (client-streaming).

**Parameters:**
- `stream` — gRPC client-streaming server interface.

**Returns:** `error` — Validation, parse, or storage error.

**Implementation details:** Bundle ID format is `"wcommit-<nanosecond timestamp>"`. Increments `routerWorkspaceSyncBundleBytesTotal`.

---

#### `(r *Router) PushWorkspaceCommits`

```go
func (r *Router) PushWorkspaceCommits(req *pb.PushWorkspaceCommitsRequest, stream pb.MonoFSRouter_PushWorkspaceCommitsServer) error
```

**What it does:** Initiates a source push job. Looks up the staged commit bundle, resolves the logical branch, evaluates workspace policy, performs subtree ownership gating, and then runs the push workflow. If policy denies, immediately returns a failed job. If ownership gating blocks, routes to a merge request. Otherwise, proceeds with the full push flow.

**How it's called:** gRPC service method `MonoFSRouter.PushWorkspaceCommits` (server-streaming).

**Parameters:**
- `req *pb.PushWorkspaceCommitsRequest` — Contains `BundleId`, optional `WorkspaceId`, `LogicalBranch`, `SourcePushMode`.
- `stream` — gRPC server-streaming interface.

**Returns:** `error` — Bundle not found, policy denied, ownership blocked, or push failure.

**Implementation details:**
- Calls `resolveSourcePushLogicalBranch` to reconcile requested and bundle branch names.
- Evaluates policy via `r.evalPolicy` with `ActionSourcePush`.
- If policy denies: job marked `FAILED`, metric labeled `"denied"`.
- Ownership gating via `r.evaluateSubtreeOwnership`: if `MergeDecisionMergeRequest`, job is blocked with message about opening a merge request.
- Sets up job entry, stores it, cancels on stream close.
- Delegates to `runWorkspaceCommitPushJob` for the actual execution.

---

### Unexported Functions

#### `(r *Router) runWorkspaceCommitPushJob`

```go
func (r *Router) runWorkspaceCommitPushJob(ctx context.Context, entry *workspaceSyncJobEntry, req *pb.PushWorkspaceCommitsRequest, logicalBranch string, bundleEntry *stagedWorkspaceBundle, send func(*pb.WorkspaceSyncEvent) error) error
```

**What it does:** The core source push execution. Transitions the job to `RUNNING`, gets the fetcher client, stages the commit bundle on the fetcher cluster, calls `StartWorkspaceCommitPush` on the fetcher, and for each result: updates the repository result in the job, appends a push outcome to the distributed ledger, and streams the event back. On completion, finalizes the job.

**How it's called:** From `PushWorkspaceCommits`.

**Parameters:**
- `ctx context.Context` — Cancellable context.
- `entry *workspaceSyncJobEntry` — The job entry.
- `req *pb.PushWorkspaceCommitsRequest` — The push request.
- `logicalBranch string` — Resolved logical branch name.
- `bundleEntry *stagedWorkspaceBundle` — The staged commit bundle.
- `send func(*pb.WorkspaceSyncEvent) error` — Stream callback.

**Returns:** `error` — Fetcher unavailable, stage failure, push failure, ledger append failure, or stream-send error.

**Implementation details:**
- After each repository result, calls `r.appendPushOutcome` to record the push outcome in the ledger. If ledger append fails, the entire job is marked failed.
- Defers `fetcherClient.DiscardWorkspaceBundle` for cleanup.
- Push mode is resolved via `resolveSourcePushMode`.

---

#### `(r *Router) newWorkspaceCommitPushJob`

```go
func (r *Router) newWorkspaceCommitPushJob(req *pb.PushWorkspaceCommitsRequest, bundle *workspacebundle.SourceCommitBundle, logicalBranch, clientID string) *pb.WorkspaceSyncJob
```

**What it does:** Creates a new `WorkspaceSyncJob` in `QUEUED` state for a source push action. Populates repository count from `bundle.RepositoryRefs()`, local commit IDs from `bundle.LocalCommitIDs()`, and the resolved logical branch.

**How it's called:** From `PushWorkspaceCommits`.

**Parameters:**
- `req *pb.PushWorkspaceCommitsRequest` — The push request.
- `bundle *workspacebundle.SourceCommitBundle` — The parsed commit bundle.
- `logicalBranch string` — The resolved logical branch.
- `clientID string` — The client ID.

**Returns:** `*pb.WorkspaceSyncJob` — The initialized job.

---

#### `resolveSourcePushLogicalBranch`

```go
func resolveSourcePushLogicalBranch(requested string, bundle *workspacebundle.SourceCommitBundle) (string, error)
```

**What it does:** Resolves the logical branch name for a source push. If both the request and the bundle specify a branch and they differ, returns an error. Otherwise returns whichever is non-empty (requested takes precedence).

**How it's called:** From `PushWorkspaceCommits`.

**Parameters:**
- `requested string` — Branch name from the request.
- `bundle *workspacebundle.SourceCommitBundle` — The commit bundle (may contain `LogicalBranch`).

**Returns:**
- `string` — The resolved branch name (may be empty).
- `error` — Mismatch error if both are non-empty and differ.

---

#### `resolveSourcePushMode`

```go
func resolveSourcePushMode(requested pb.SourcePushMode, configDefault string) string
```

**What it does:** Resolves the source push mode. If the request specifies an explicit mode (`PRESERVE` or `SQUASH`), uses that. Otherwise falls back to the router's configured default, and then to `"squash"` as the ultimate default.

**How it's called:** From `PushWorkspaceCommits` and `runWorkspaceCommitPushJob`.

**Parameters:**
- `requested pb.SourcePushMode` — The mode from the request (may be `UNSPECIFIED`).
- `configDefault string` — The router's `config.SourcePushMode`.

**Returns:** `string` — Either `"squash"` or `"preserve"`.

---

#### `storageIDsFromSourceBundle`

```go
func storageIDsFromSourceBundle(bundle *workspacebundle.SourceCommitBundle) []string
```

**What it does:** Extracts all unique storage IDs from a source commit bundle by iterating over all commits and their repositories. Used for policy evaluation to know which storage IDs are affected.

**How it's called:** From `PushWorkspaceCommits` for policy evaluation.

**Parameters:**
- `bundle *workspacebundle.SourceCommitBundle` — The commit bundle.

**Returns:** `[]string` — Deduplicated storage IDs. Returns nil if bundle is nil.

---

#### `pushStatusFromRepositoryResult`

```go
func pushStatusFromRepositoryResult(repo *pb.WorkspaceSyncRepositoryResult) string
```

**What it does:** Maps a `WorkspaceSyncRepositoryStatus` to a human-readable push status string: `"pushed"` for `PUBLISHED`, `"conflict"` for `CONFLICT`, `"failed"` otherwise.

**How it's called:** From `runWorkspaceCommitPushJob` when constructing ledger push outcomes.

**Parameters:**
- `repo *pb.WorkspaceSyncRepositoryResult` — The repository result.

**Returns:** `string` — Push status label.

---

## Workspace Sync (`workspace_sync.go`)

Core workspace synchronization infrastructure: job management (create, retrieve, list, cancel), refresh workflow, job state machine, repository result updates, metric tracking, and helper functions.

### Types

#### `workspaceSyncJobEntry`

```go
type workspaceSyncJobEntry struct {
    mu     sync.RWMutex
    job    *pb.WorkspaceSyncJob
    cancel context.CancelFunc
}
```

In-memory representation of a workspace sync job, protected by a read-write mutex. Holds the job protobuf and a cancel function for the associated context.

---

### Exported Functions

#### `(r *Router) RefreshWorkspace`

```go
func (r *Router) RefreshWorkspace(req *pb.RefreshWorkspaceRequest, stream pb.MonoFSRouter_RefreshWorkspaceServer) error
```

**What it does:** Creates a workspace refresh job (state `QUEUED`), stores it, sends an initial `JOB_ACCEPTED` event, then delegates to `runWorkspaceRefreshJob`. Tracks metrics for jobs, active jobs, and duration.

**How it's called:** gRPC service method `MonoFSRouter.RefreshWorkspace` (server-streaming).

**Parameters:**
- `req *pb.RefreshWorkspaceRequest` — Contains `WorkspaceId`, `Repositories`, `RejectIfLocalChanges`, `AllowFastForwardOnly`.
- `stream` — gRPC server-streaming interface.

**Returns:** `error` — Storage, context, or refresh error.

---

#### `(r *Router) GetWorkspaceSyncJob`

```go
func (r *Router) GetWorkspaceSyncJob(ctx context.Context, req *pb.GetWorkspaceSyncJobRequest) (*pb.WorkspaceSyncJob, error)
```

**What it does:** Retrieves a single workspace sync job by job ID. Returns a snapshot (protobuf clone) of the job.

**How it's called:** gRPC method `MonoFSRouter.GetWorkspaceSyncJob` (unary), and from the HTTP API handler.

**Parameters:**
- `ctx context.Context` — Context.
- `req *pb.GetWorkspaceSyncJobRequest` — Contains `JobId`.

**Returns:**
- `*pb.WorkspaceSyncJob` — Job snapshot.
- `error` — "workspace sync job not found" if the job ID is unknown.

---

#### `(r *Router) ListWorkspaceSyncJobs`

```go
func (r *Router) ListWorkspaceSyncJobs(ctx context.Context, req *pb.ListWorkspaceSyncJobsRequest) (*pb.ListWorkspaceSyncJobsResponse, error)
```

**What it does:** Lists workspace sync jobs, optionally filtered by action and state. Results are sorted by creation time descending and limited by the request's `Limit` field.

**How it's called:** gRPC method `MonoFSRouter.ListWorkspaceSyncJobs` (unary), and from the HTTP API handler.

**Parameters:**
- `ctx context.Context` — Context.
- `req *pb.ListWorkspaceSyncJobsRequest` — Contains optional `Action`, `State`, and `Limit` filters.

**Returns:**
- `*pb.ListWorkspaceSyncJobsResponse` — Contains the filtered, sorted, limited job list.

---

#### `(r *Router) CancelWorkspaceSyncJob`

```go
func (r *Router) CancelWorkspaceSyncJob(ctx context.Context, req *pb.CancelWorkspaceSyncJobRequest) (*pb.CancelWorkspaceSyncJobResponse, error)
```

**What it does:** Cancels a running workspace sync job. If the job is already in a terminal state (`SUCCEEDED`, `FAILED`, `CANCELLED`), returns success=false. Otherwise sets the state to `CANCELLED`, sets `ErrorMessage` to "cancelled", persists, and invokes the stored cancel function.

**How it's called:** gRPC method `MonoFSRouter.CancelWorkspaceSyncJob` (unary), and from the HTTP API handler.

**Parameters:**
- `ctx context.Context` — Context.
- `req *pb.CancelWorkspaceSyncJobRequest` — Contains `JobId`.

**Returns:**
- `*pb.CancelWorkspaceSyncJobResponse` — `Success` and `Message` indicating the result.

**Implementation details:** If persistence fails, the job is rolled back to its previous state.

---

### Unexported Functions

#### `(r *Router) runWorkspaceRefreshJob`

```go
func (r *Router) runWorkspaceRefreshJob(ctx context.Context, entry *workspaceSyncJobEntry, req *pb.RefreshWorkspaceRequest, send func(*pb.WorkspaceSyncEvent) error) error
```

**What it does:** The core refresh execution flow. Transitions the job to `RUNNING`, probes the fetcher cluster for each repository's sync status, streams results, and for repositories requiring refresh (`REFRESH_REQUIRED`), triggers re-ingestion via `r.IngestRepository`. Sends `REINGEST_STARTED` and `REINGEST_COMPLETED` events for each refreshed repository.

**How it's called:** From `RefreshWorkspace`.

**Parameters:**
- `ctx context.Context` — Cancellable context.
- `entry *workspaceSyncJobEntry` — The job entry.
- `req *pb.RefreshWorkspaceRequest` — The refresh request.
- `send func(*pb.WorkspaceSyncEvent) error` — Stream callback.

**Returns:** `error` — Fetcher unavailable, probe failure, or ingest failure.

**Implementation details:**
- If no fetcher client is available, fails the job immediately.
- Uses `mockIngestStream` for the `IngestRepository` call (non-streaming ingestion path).
- Increments `routerWorkspaceSyncReingestTotal` for each re-ingestion attempt.

---

#### `(r *Router) newWorkspaceSyncJob`

```go
func (r *Router) newWorkspaceSyncJob(req *pb.RefreshWorkspaceRequest, clientID string) *pb.WorkspaceSyncJob
```

**What it does:** Creates a new `WorkspaceSyncJob` in `QUEUED` state for a refresh action. The repository count comes from `len(req.GetRepositories())`.

**How it's called:** From `RefreshWorkspace`.

**Parameters:**
- `req *pb.RefreshWorkspaceRequest` — The refresh request.
- `clientID string` — The client ID.

**Returns:** `*pb.WorkspaceSyncJob` — The initialized job.

---

#### `(r *Router) storeWorkspaceSyncJob`

```go
func (r *Router) storeWorkspaceSyncJob(entry *workspaceSyncJobEntry) error
```

**What it does:** Persists a workspace sync job. If `r.workspaceJobStore` is configured, persists to the store. Always adds the entry to the in-memory map `r.workspaceSyncJobs`.

**How it's called:** From `RefreshWorkspace`, `PublishWorkspace`, `PushWorkspaceCommits`, and policy-denied/ownership-blocked paths.

**Parameters:**
- `entry *workspaceSyncJobEntry` — The job entry.

**Returns:** `error` — Persistence error.

---

#### `(r *Router) getWorkspaceSyncJob`

```go
func (r *Router) getWorkspaceSyncJob(jobID string) *workspaceSyncJobEntry
```

**What it does:** Retrieves a job entry by ID from the in-memory map, holding a read lock.

**How it's called:** From `GetWorkspaceSyncJob`, `CancelWorkspaceSyncJob`, and various list operations.

**Parameters:**
- `jobID string` — The job ID.

**Returns:** `*workspaceSyncJobEntry` — The entry, or nil.

---

#### `(r *Router) listWorkspaceSyncJobs`

```go
func (r *Router) listWorkspaceSyncJobs(req *pb.ListWorkspaceSyncJobsRequest) []*pb.WorkspaceSyncJob`

```

**What it does:** Lists job snapshots from the in-memory map, filtered by action and state. Results are sorted by creation time descending and limited by `req.Limit`.

**How it's called:** From `ListWorkspaceSyncJobs` and the HTTP handler.

**Parameters:**
- `req *pb.ListWorkspaceSyncJobsRequest` — Contains `Action`, `State`, and `Limit` filters.

**Returns:** `[]*pb.WorkspaceSyncJob` — Filtered, sorted, limited job snapshots.

---

#### `(e *workspaceSyncJobEntry) snapshot`

```go
func (e *workspaceSyncJobEntry) snapshot() *pb.WorkspaceSyncJob
```

**What it does:** Returns a thread-safe protobuf clone of the job. Used whenever a job's state is read and sent externally to avoid race conditions on the shared job object.

**How it's called:** From all event-sending sites, `GetWorkspaceSyncJob`, `ListWorkspaceSyncJobs`, `CancelWorkspaceSyncJob`, and `finalizeWorkspaceSyncJob`.

**Returns:** `*pb.WorkspaceSyncJob` — Deep copy of the job protobuf.

---

#### `(r *Router) updateWorkspaceSyncJob`

```go
func (r *Router) updateWorkspaceSyncJob(entry *workspaceSyncJobEntry, update func(*pb.WorkspaceSyncJob)) error
```

**What it does:** Applies a mutating function to the job under write lock. If `r.workspaceJobStore` is configured, persists the updated job; on persistence failure, rolls back to the previous state.

**How it's called:** From `failWorkspaceSyncJob`, `cancelWorkspaceSyncJob`, `finalizeWorkspaceSyncJob`, `runWorkspaceRefreshJob`, `runWorkspacePublishJob`, and `runWorkspaceCommitPushJob`.

**Parameters:**
- `entry *workspaceSyncJobEntry` — The job entry.
- `update func(*pb.WorkspaceSyncJob)` — Mutating function applied in-place.

**Returns:** `error` — Persistence error.

---

#### `(r *Router) updateWorkspaceSyncRepository`

```go
func (r *Router) updateWorkspaceSyncRepository(entry *workspaceSyncJobEntry, repo *pb.WorkspaceSyncRepositoryResult) error
```

**What it does:** Adds or updates a repository result within a job. If a repository with the same `StorageId` already exists, replaces it; otherwise appends. Updates the job summary and persists. Increments `routerWorkspaceSyncRepositoriesTotal` metric.

**How it's called:** From `runWorkspaceRefreshJob`, `runWorkspacePublishJob`, and `runWorkspaceCommitPushJob` for each repository result.

**Parameters:**
- `entry *workspaceSyncJobEntry` — The job entry.
- `repo *pb.WorkspaceSyncRepositoryResult` — The repository result to upsert.

**Returns:** `error` — Persistence error.

---

#### `(r *Router) failWorkspaceSyncJob`

```go
func (r *Router) failWorkspaceSyncJob(entry *workspaceSyncJobEntry, actionLabel, message string) error
```

**What it does:** Marks a job as `FAILED` with a given error message and current timestamp. Increments the `routerWorkspaceSyncJobsTotal` counter with `"failed"` label.

**How it's called:** From error paths in refresh, publish, and push workflows.

**Parameters:**
- `entry *workspaceSyncJobEntry` — The job entry.
- `actionLabel string` — Metric label (e.g., `"publish"`, `"refresh"`, `"source_push"`).
- `message string` — Error message stored in `ErrorMessage`.

**Returns:** `error` — From `updateWorkspaceSyncJob`.

---

#### `(r *Router) cancelWorkspaceSyncJob`

```go
func (r *Router) cancelWorkspaceSyncJob(entry *workspaceSyncJobEntry) error
```

**What it does:** Marks a job as `CANCELLED` with `ErrorMessage = "cancelled"` and increments the cancelled metric. Used when the context is cancelled during job execution.

**How it's called:** From `runWorkspaceRefreshJob`, `runWorkspacePublishJob`, and `runWorkspaceCommitPushJob` when `ctx.Done()` fires.

**Parameters:**
- `entry *workspaceSyncJobEntry` — The job entry.

**Returns:** `error` — From `updateWorkspaceSyncJob`.

---

#### `(r *Router) finalizeWorkspaceRefreshJob`

```go
func (r *Router) finalizeWorkspaceRefreshJob(entry *workspaceSyncJobEntry) error
```

**What it does:** Finalizes a refresh job by calling `finalizeWorkspaceSyncJob` and incrementing the job total metric with the refresh action label.

**How it's called:** From `runWorkspaceRefreshJob`.

**Parameters:**
- `entry *workspaceSyncJobEntry` — The job entry.

**Returns:** `error` — From `finalizeWorkspaceSyncJob`.

---

#### `(r *Router) finalizeWorkspaceSyncJob`

```go
func (r *Router) finalizeWorkspaceSyncJob(entry *workspaceSyncJobEntry) error
```

**What it does:** Sets the job's `FinishedAtUnix` timestamp, updates the summary, and determines the final state. If any repositories failed or had conflicts, the job is marked `FAILED`; otherwise `SUCCEEDED`.

**How it's called:** From `finalizeWorkspaceRefreshJob`, `runWorkspacePublishJob`, and `runWorkspaceCommitPushJob`.

**Parameters:**
- `entry *workspaceSyncJobEntry` — The job entry.

**Returns:** `error` — From `updateWorkspaceSyncJob`.

---

#### `updateWorkspaceSyncSummary`

```go
func updateWorkspaceSyncSummary(job *pb.WorkspaceSyncJob)
```

**What it does:** Recalculates the `WorkspaceSyncSummary` from the job's repository results. Counts: `RepositoriesTotal` (from `len(Repositories)`), `RepositoriesSucceeded`, `RepositoriesRefreshed`, `RepositoriesPublished`, `RepositoriesConflicted`, `RepositoriesFailed`. Replaces the job's `Summary` field entirely.

**How it's called:** From `updateWorkspaceSyncRepository` and `finalizeWorkspaceSyncJob`.

**Status mapping for success:** `UNCHANGED`, `REFRESHED`, `PUBLISHED` → `RepositoriesSucceeded`; `REFRESHED` additionally increments `RepositoriesRefreshed`; `PUBLISHED` additionally increments `RepositoriesPublished`.

---

#### `workspaceRepositoryResultFromProbe`

```go
func workspaceRepositoryResultFromProbe(progress *pb.RepoSyncProgress) *pb.WorkspaceSyncRepositoryResult
```

**What it does:** Converts a fetcher `RepoSyncProgress` from a refresh probe into a `WorkspaceSyncRepositoryResult`. Maps repo sync statuses to workspace result statuses:
- `UNCHANGED` → `UNCHANGED`
- `ADVANCED` → `REFRESH_REQUIRED`
- `DIVERGED`/`MISSING_BRANCH` → `CONFLICT`
- `AUTH_FAILED`/`TRANSIENT_ERROR`/`FAILED` → `FAILED`

**How it's called:** From `runWorkspaceRefreshJob`.

**Returns:** `*pb.WorkspaceSyncRepositoryResult` — Converted result. If input is nil, returns a `FAILED` result with "invalid probe result".

**Implementation details:** Increments `routerWorkspaceSyncConflictsTotal` for conflict statuses.

---

#### `workspaceRepositoryResultFromPublish`

```go
func workspaceRepositoryResultFromPublish(progress *pb.RepoSyncProgress, actionLabel string) *pb.WorkspaceSyncRepositoryResult
```

**What it does:** Converts a fetcher `RepoSyncProgress` from a publish/push operation into a `WorkspaceSyncRepositoryResult`. Maps statuses:
- `UNCHANGED` → `UNCHANGED`
- `PUBLISHED` → `PUBLISHED`
- `CONFLICT`/`DIVERGED`/`MISSING_BRANCH` → `CONFLICT`
- `AUTH_FAILED`/`TRANSIENT_ERROR`/`FAILED` → `FAILED`

**How it's called:** From `runWorkspacePublishJob` and `runWorkspaceCommitPushJob`.

**Returns:** `*pb.WorkspaceSyncRepositoryResult` — Converted result with additional fields: `TargetBranch`, `PushedCommit`, `ConflictReason`. Defaults `TargetBranch` to the repo branch if empty.

**Implementation details:** Increments `routerWorkspaceSyncConflictsTotal` for conflict statuses.

---

#### `workspaceEventTypeForRepository`

```go
func workspaceEventTypeForRepository(repo *pb.WorkspaceSyncRepositoryResult) pb.WorkspaceSyncEventType
```

**What it does:** Determines the appropriate stream event type for a repository result:
- `UNCHANGED`, `REFRESH_REQUIRED`, `REFRESHED`, `PUBLISHED` → `REPOSITORY_COMPLETED`
- `CONFLICT` → `REPOSITORY_CONFLICTED`
- All others → `REPOSITORY_FAILED`

**How it's called:** From `runWorkspacePublishJob`, `runWorkspaceCommitPushJob`, and `runWorkspaceRefreshJob` when sending per-repository events.

---

#### `cloneWorkspaceSyncRepositoryResult`

```go
func cloneWorkspaceSyncRepositoryResult(repo *pb.WorkspaceSyncRepositoryResult) *pb.WorkspaceSyncRepositoryResult
```

**What it does:** Returns a deep copy of a repository result using `proto.Clone`. Returns nil for nil input.

**How it's called:** From all sites that send repository events and from `updateWorkspaceSyncRepository` (when appending/replacing entries to avoid aliasing).

---

#### `workspaceSyncResultLabel`

```go
func workspaceSyncResultLabel(state pb.WorkspaceSyncState) string
```

**What it does:** Converts a `WorkspaceSyncState` to a Prometheus metric label: `SUCCEEDED` → `"succeeded"`, `CANCELLED` → `"cancelled"`, otherwise `"failed"`.

**How it's called:** From `PublishWorkspace`, `PushWorkspaceCommits`, `RefreshWorkspace`, `finalizeWorkspaceRefreshJob`, `runWorkspacePublishJob`, and `runWorkspaceCommitPushJob` for metric tracking.

---

#### `workspaceSyncActionMetricLabel`

```go
func workspaceSyncActionMetricLabel(action pb.WorkspaceSyncAction) string
```

**What it does:** Converts a `WorkspaceSyncAction` to a Prometheus metric label: `PUBLISH` → `"publish"`, `REFRESH` → `"refresh"`, `SOURCE_PUSH` → `"source_push"`, otherwise `"unknown"`.

**How it's called:** From all job initiation, failure, cancellation, completion metric sites.

---

#### `workspaceSyncRepositoryMetricLabel`

```go
func workspaceSyncRepositoryMetricLabel(status pb.WorkspaceSyncRepositoryStatus) string
```

**What it does:** Converts a `WorkspaceSyncRepositoryStatus` to a Prometheus metric label: `UNCHANGED` → `"unchanged"`, `REFRESH_REQUIRED` → `"refresh_required"`, `REFRESHED` → `"refreshed"`, `PUBLISHED` → `"published"`, `CONFLICT` → `"conflict"`, `CANCELLED` → `"cancelled"`, otherwise `"failed"`.

**How it's called:** From `updateWorkspaceSyncRepository` when incrementing `routerWorkspaceSyncRepositoriesTotal`.

---

## Workspace Sync UI (`workspace_sync_ui.go`)

Minimal HTTP JSON API for listing, retrieving, and cancelling workspace sync jobs. Intended for use by a dashboard or UI.

### Unexported Functions

#### `(r *Router) handleWorkspaceSyncJobsAPI`

```go
func (r *Router) handleWorkspaceSyncJobsAPI(w http.ResponseWriter, req *http.Request)
```

**What it does:** HTTP handler registered at `/api/workspace-sync/jobs`. Supports three routes:

| Route | Method | Action |
|-------|--------|--------|
| `GET /api/workspace-sync/jobs` | GET | List all jobs (limit 100) via `ListWorkspaceSyncJobs`. |
| `GET /api/workspace-sync/jobs/{jobId}` | GET | Get a single job via `GetWorkspaceSyncJob`. |
| `POST /api/workspace-sync/jobs/{jobId}/cancel` | POST | Cancel a job via `CancelWorkspaceSyncJob`. |

All responses are JSON-encoded. Unsupported methods return 405. Missing jobs return 404. Internal errors return 500.

**How it's called:** Registered in the router's HTTP mux (typically as `"/api/workspace-sync/jobs"` and `"/api/workspace-sync/jobs/"`).

**Parameters:**
- `w http.ResponseWriter` — HTTP response writer.
- `req *http.Request` — HTTP request.

**Implementation details:** The path is parsed by stripping the prefix `/api/workspace-sync/jobs` and then splitting on `/`.
