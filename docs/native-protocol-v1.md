# MonoFS native protocol v1

Internal engineering note for the experimental kernel and userspace native-client path. This is not part of the release-grade public interface and should not be treated as a stable ABI commitment.

This document defines the first kernel-friendly protocol contract for native
MonoFS mounts. It is the protocol that a future `monofs` kernel module and a
userspace conformance client should implement against.

## Goals

- replace the current FUSE lower/read path with a native VFS client
- keep writes in the existing overlay/session workflow for v1
- move namespace aggregation, HRW failover, and topology handling out of the kernel
- avoid exposing gRPC/HTTP2 or Go-specific wire behavior to the kernel
- preserve current MonoFS correctness requirements, especially authoritative
  merged directory listings and failover-safe reads

## Non-goals

- create/write/rename/delete in the kernel client
- direct parsing of existing `MonoFSRouter` or `MonoFS` gRPC traffic in-kernel
- moving the overlay/session UX into kernel space
- final upstreamable ABI promises; this is an internal v1 contract

## Architecture

The native client boundary is:

```text
monofs-kmod / userspace conformance client
        |
        | native protocol v1
        v
monofs-native-gateway
        |
        | existing Go control/data logic
        v
router + storage nodes + fetchers
```

The gateway is authoritative for:

- mount/session negotiation
- auth validation
- namespace operations (`lookup`, `getattr`, `readdir`, `statfs`)
- merged directory listings across all healthy nodes
- failover and rebalance-aware read routing
- invalidation delivery to clients

The kernel client stays responsible for:

- VFS objects, dentries, inodes, page cache, and readahead
- mount lifecycle
- retry-on-stale behavior
- invalidating local caches when the gateway says state changed

## Why this boundary

The current Go client does three things that must not be copied into the kmod:

1. it speaks gRPC/HTTP2 to router and nodes
2. it reproduces cluster routing/failover behavior in userspace
3. it fans `ReadDir` out to all healthy nodes and merges results

The native gateway keeps those behaviors in Go and gives the kmod a smaller,
stable filesystem protocol instead.

## Transport

v1 uses a little-endian framed binary protocol over TCP. The kernel never
parses protobuf or HTTP2. A userspace conformance client must use the exact
same wire format as the kmod.

Two connections are used per mount session:

- **control connection**: request/reply filesystem operations
- **event connection**: server-to-client invalidations and session events

Separate event delivery keeps async invalidations out of the synchronous VFS
request path.

### Frame header

Every frame begins with:

```c
struct monofs_frame_v1 {
  __le32 magic;        /* "MNFS" */
  __le16 version;      /* 1 */
  __le16 opcode;       /* request/reply/event kind */
  __le32 flags;        /* MORE, RETRYABLE, EVENT, etc. */
  __le32 header_len;   /* bytes including this header */
  __le32 body_len;     /* payload bytes following the header */
  __le64 request_id;   /* 0 for unsolicited events */
  __le64 session_id;   /* 0 before MOUNT succeeds */
  __le32 status;       /* reply status, 0 on requests/events */
  __le32 reserved;
  __le64 generation;   /* cluster/event generation */
};
```

Rules:

- one request produces exactly one terminal reply
- large reads may use multiple reply frames with `MORE`
- all strings are UTF-8 without trailing NUL
- all variable-length arrays are length-prefixed inside the body
- unknown flags or optional body fields are ignored when the negotiated
  capability bit says they may appear

## Identity model

Namespace replies carry two identifiers:

- **object_id**: opaque 128-bit handle used for future requests
- **ino**: stable 64-bit inode number returned to VFS

The gateway, not the kernel, owns inode numbering. For v1, `ino` should be a
stable hash of canonical `(display_path, relative_path)` with reserved low
numbers for synthetic root/control objects.

Every object also carries:

- `mode`
- `size`
- `mtime`, `ctime`, `atime`
- `nlink`, `uid`, `gid`
- `attr_version`
- `data_version` for regular files

`attr_version` and `data_version` are monotonic per object and are used for
cache invalidation and stale-handle detection.

## Session lifecycle

### `HELLO`

Negotiates protocol version and capability bits.

Request fields:

- `min_version`, `max_version`
- `client_kind` (`kmod`, `conformance`)
- `client_version`
- `kernel_release` if present
- `requested_caps`

Reply fields:

- `selected_version`
- `server_caps`
- `max_frame_bytes`
- `max_read_bytes`

### `MOUNT`

Creates a native mount session.

Request fields:

- auth token or auth reference
- requested mount flags (`readonly`, `overlay_writes`, `debug`)
- client ID
- hostname
- requested TTL caps

Reply fields:

- `session_id`
- root object metadata
- `cluster_version`
- `namespace_generation`
- default TTLs (`entry_ttl_ms`, `attr_ttl_ms`, `dir_ttl_ms`, `route_ttl_ms`)
- `event_window`
- capability bits

### `UNMOUNT`

Best-effort session close. Loss of the TCP connection also implicitly tears the
session down.

## Operation set

| Opcode | Purpose |
| --- | --- |
| `HELLO` | Version/capability negotiation |
| `MOUNT` | Create a mount session and return root metadata |
| `UNMOUNT` | Close a mount session |
| `LOOKUP` | Resolve `parent_object_id + name` |
| `GETATTR` | Fetch metadata for one object |
| `READDIR` | Enumerate a directory page with stable cookies |
| `STATFS` | Return filesystem capacity metadata |
| `OPEN_READ` | Open a file for read and resolve routing/data version |
| `READ` | Read a range from a read handle |
| `CLOSE` | Close a read handle |
| `WATCH` | Attach the event connection to an existing session |
| `PING` | Health/liveness |

## Namespace semantics

### `LOOKUP`

Request:

- `parent_object_id`
- `name`
- optional `parent_attr_version`

Reply:

- `found`
- child metadata if found
- `entry_ttl_ms`
- `namespace_generation`

Rules:

- `LOOKUP` is authoritative; the gateway must apply the same namespace rules as
  current MonoFS (`managed namespaces`, repository roots, virtual directories,
  and ordinary file lookups)
- a successful lookup must return the same `ino` for the same canonical path
- `found=false` is distinct from transport/backend failure

### `GETATTR`

Request:

- `object_id`
- optional `known_attr_version`

Reply:

- full metadata
- `attr_ttl_ms`
- `namespace_generation`

### `READDIR`

Request:

- `dir_object_id`
- `cookie`
- `max_entries`
- `max_bytes`

Reply:

- ordered directory entries
- `next_cookie`
- `eof`
- `dir_attr_version`
- `namespace_generation`

Rules:

- replies must be **authoritative merged listings**
- `READDIR` must never return partial cluster state as success
- if the gateway cannot produce a complete merged listing across all required
  healthy nodes, it must return an error and let the client retry
- entry order must be deterministic (`name` ascending) so callers like
  `go mod verify` see stable directory traversal

This is a direct carry-forward of current `ShardedClient.ReadDir` semantics.

### `STATFS`

Returns filesystem-level counts/capacity. v1 may return synthetic totals if
exact distributed accounting is too expensive, but replies must be explicit and
internally consistent.

## Read-path semantics

### `OPEN_READ`

Request:

- `object_id`
- open flags (v1 expects read-only)

Reply:

- `handle_id`
- file metadata snapshot
- `data_version`
- `route_generation`
- `route_ttl_ms`
- one or more data targets

Each data target contains:

- endpoint kind (`gateway`, `storage-node`)
- network address
- opaque bearer token for that target
- priority

The gateway may return itself as the only target in early implementations. That
keeps the wire contract stable while allowing later direct-to-node reads.

### `READ`

Request:

- `handle_id`
- `data_version`
- `offset`
- `length`

Reply:

- data bytes
- `eof`
- optional `remaining_bytes`

Rules:

- if the route is stale, reply `STALE_ROUTE` instead of silently proxying a
  different object
- if file content changed relative to `data_version`, reply `STALE_DATA`
- short reads are allowed only at EOF
- chunking across multiple frames is allowed when `MORE` is set

### `CLOSE`

Best-effort close for read handles. The protocol must tolerate leaked handles on
client crash by expiring them server-side.

## Invalidation model

The event connection is attached with `WATCH(session_id, last_seen_event_seq)`.
Events are strictly ordered by `event_seq`.

Event kinds:

- `ENTRY_INVALIDATE(parent_object_id, name)`
- `ATTR_INVALIDATE(object_id, attr_version)`
- `DATA_INVALIDATE(object_id, data_version)`
- `SUBTREE_INVALIDATE(object_id)`
- `ROUTE_INVALIDATE(object_id, route_generation)`
- `TOPOLOGY_INVALIDATE(cluster_version)`
- `OVERLAY_INVALIDATE(path_prefix or object_id)`
- `RESYNC_REQUIRED(reason)`
- `SESSION_REVOKED(reason)`

Rules:

- if the client detects an event gap, it must treat it as `RESYNC_REQUIRED`
- `RESYNC_REQUIRED` means drop cached dentries/inodes/page-cache state that
  depends on remote metadata and re-fetch from the gateway
- topology changes and failover transitions must arrive through this stream or
  force a resync on the next request

## Overlay-backed write interaction

v1 keeps writes outside the native protocol, but the protocol must still carry
the effects of those writes to read-only native mounts.

Required behavior:

1. userspace overlay/session flows commit or publish changes using existing Go
   paths
2. router/gateway increments namespace and/or data generations
3. gateway emits `OVERLAY_INVALIDATE`, `ENTRY_INVALIDATE`, `ATTR_INVALIDATE`,
   or `DATA_INVALIDATE` events to mounted native clients
4. kmod drops stale dentries/page cache and re-fetches on next access

This is especially important for:

- dependency uploads/deletes
- Guardian path mutations
- repository ingestion completion
- failover or rebalance-driven content movement

## Error model

Wire statuses must be explicit and map cleanly to Linux errno handling:

| Status | Meaning | Kernel action |
| --- | --- | --- |
| `OK` | Success | normal return |
| `NOT_FOUND` | Missing object/entry | `ENOENT` |
| `NOT_DIR` | Parent/object not a directory | `ENOTDIR` |
| `IS_DIR` | File op on directory | `EISDIR` |
| `PERM` | Permission denied | `EPERM` or `EACCES` |
| `AUTH` | Auth/session invalid | fail mount or `EACCES` |
| `STALE_NAMESPACE` | Cached dentry/object obsolete | invalidate and retry once |
| `STALE_ROUTE` | Read routing obsolete | reopen route and retry once |
| `STALE_DATA` | Content changed under open handle | reopen and retry once |
| `RETRY` | Transient retryable failure | bounded retry |
| `DRAINED` | Cluster temporarily drained | surface `EAGAIN`/`EIO` per mount policy |
| `UNAVAILABLE` | No healthy backend path | `EIO` |
| `BACKEND_IO` | Authoritative backend failure | `EIO` |
| `CANCELLED` | Request cancelled | `EINTR` |
| `UNSUPPORTED` | Capability/opcode not available | `EOPNOTSUPP` |

Current MonoFS behavior should be preserved:

- backend failures surface as I/O failure, not fake success
- cancellation should be distinguishable from backend failure
- `READDIR` must fail instead of returning partial contents

## Capability bits

v1 negotiation should include at least:

- `CAP_EVENT_STREAM`
- `CAP_DIRECT_NODE_READS`
- `CAP_ROUTE_TTLS`
- `CAP_INLINE_SMALL_READS`
- `CAP_STATFS`
- `CAP_REVALIDATE_HINTS`

This lets the conformance client and kmod start against a minimal gateway while
keeping room for direct-node read optimization later.

## Rollout plan

1. build a userspace conformance client that speaks this protocol
2. add `monofs-native-gateway` in Go, backed by existing router/server logic
3. implement `MOUNT`, `LOOKUP`, `GETATTR`, `READDIR`, and `STATFS`
4. implement `OPEN_READ` + gateway-proxied `READ`
5. add the event stream and invalidation wiring
6. replace `seed_paths` scaffolding in the kmod with real gateway-backed ops
7. optionally add direct-node data targets after control-plane semantics are stable

## Follow-on implementation slices

This spec currently maps to two follow-on implementation slices:

- **`add-router-namespace-ops`**: create authoritative Go gateway handlers for
  `MOUNT`, `LOOKUP`, `GETATTR`, `READDIR`, and `STATFS`
- **`implement-kmod-io`**: teach the kmod to replace seeded namespace lookups
  with real control-connection requests, then add read handles/page-cache reads
