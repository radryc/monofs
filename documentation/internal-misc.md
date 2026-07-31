# Internal Misc — Function Documentation

Auto-generated documentation for all exported and unexported functions in the listed source files.

---

## `internal/fsstat/fsstat.go`

Package `fsstat` provides a logical filesystem statistics view for statfs (statvfs) responses. It models statfs fields using logical constants rather than real block counts, producing a bounded synthetic view of the filesystem.

### Types

#### `Snapshot`

```go
type Snapshot struct {
    Blocks  uint64
    Bfree   uint64
    Bavail  uint64
    Files   uint64
    Ffree   uint64
    Bsize   uint32
    Frsize  uint32
    NameLen uint32
}
```

A filesystem statistics snapshot suitable for statfs/statvfs responses. Mirrors the typical UNIX statfs fields with logical (not physical) values.

---

### Function: `FromUsage`

```go
func FromUsage(usedBytes, totalFiles uint64) Snapshot
```

**What it does**: Converts logical usage counters (`usedBytes` consumed and `totalFiles` created) into a `Snapshot` suitable for statfs replies. Caps `usedBytes` at `LogicalTotalBytes` (2^60) and derives free bytes subtractively. Files are modeled as `totalFiles + LogicalFreeFiles` so consumers always see plenty of available inodes.

**How it's called**: Called by server-side statfs handlers (e.g., the native protocol server or FUSE layer) whenever a `statfs`/`statvfs` request arrives. These callers pass their current usage trackers (bytes used and total files created so far).

**Key parameters**:
- `usedBytes` — logical bytes consumed; clamped to `LogicalTotalBytes`.
- `totalFiles` — number of files/nodes created; added to a large free-files constant to simulate headroom.

**Return value**: A fully populated `Snapshot` with block and file counters derived from the logical model.

**Implementation details**:
- `Blocks`, `Bfree`, and `Bavail` are computed via `ceilDiv(value, BlockSize)` to translate byte counts into 4096-byte block counts.
- `Bavail` is set equal to `Bfree` (no reserved blocks for root).
- `Files` is `totalFiles + LogicalFreeFiles` so the total capacity appears huge.
- `Ffree` is always `LogicalFreeFiles` (the pool of still-available inodes).

---

### Function: `ceilDiv`

```go
func ceilDiv(value, unit uint64) uint64
```

**What it does**: Performs ceiling division of `value` by `unit`, returning the smallest integer `n` such that `n * unit >= value`. Returns 0 when `value` is 0.

**How it's called**: Only called internally by `FromUsage` to convert byte counts to block counts.

**Key parameters**:
- `value` — the numerator.
- `unit` — the denominator (block size, typically `BlockSize = 4096`).

**Return value**: `(value + unit - 1) / unit`, unless `value == 0` in which case 0.

**Implementation details**: Uses the classic `(value + unit - 1) / unit` integer ceiling pattern. Has a short-circuit for zero to avoid returning a spurious 1-block count from the formula.

---

## `internal/grpcutil/conn.go`

Package `grpcutil` provides utilities for creating gRPC client connections with configurable message sizes, compression, and timeouts. All connections use insecure transport by default (suitable for internal cluster communication).

### Types

#### `ClientConfig`

```go
type ClientConfig struct {
    Timeout           time.Duration
    MaxRecvMsgSize    int
    MaxSendMsgSize    int
    EnableCompression bool
    ExtraOpts         []grpc.DialOption
}
```

Configuration for a gRPC client connection. Zero values for `MaxRecvMsgSize` and `MaxSendMsgSize` mean "use gRPC defaults" (typically 4 MB). Zero `Timeout` means no timeout.

---

### Function: `DefaultConfig`

```go
func DefaultConfig() ClientConfig
```

**What it does**: Returns a `ClientConfig` with all fields at their zero values — conservative defaults that leave message size limits and compression at gRPC's built-in defaults.

**How it's called**: Used as the default argument to `NewClient` when callers don't need custom configuration. Also called by `NewSimpleClient`.

**Return value**: An empty `ClientConfig{}`.

---

### Function: `NewClient`

```go
func NewClient(addr string, cfg ClientConfig) (*grpc.ClientConn, error)
```

**What it does**: Creates a new gRPC client connection to `addr` using insecure transport credentials. Applies optional message size limits, gzip compression, and any user-provided `ExtraOpts`.

**How it's called**: The primary connection factory. Called directly by server bootstrap code that needs custom gRPC options. Also called indirectly by `NewSimpleClient` and `NewMinimalClient`.

**Key parameters**:
- `addr` — target address (e.g. `"localhost:8080"`), passed directly to `grpc.NewClient`.
- `cfg` — a `ClientConfig` specifying optional overrides.

**Return value**: A `*grpc.ClientConn` ready for use, or an error if the connection fails.

**Implementation details**:
- Always appends `grpc.WithTransportCredentials(insecure.NewCredentials())` — all connections are insecure.
- Wraps `MaxRecvMsgSize` and `MaxSendMsgSize` in `grpc.WithDefaultCallOptions(...)` only when the values are positive.
- If `EnableCompression` is true, adds `grpc.UseCompressor("gzip")` as a default call option.
- Appends any `ExtraOpts` after all built-in options so callers can override defaults if needed.
- Does **not** use `cfg.Timeout` — that field is reserved for future use; connection timeouts are the caller's responsibility (e.g., via `context.WithTimeout` on the dial context).

---

### Function: `NewSimpleClient`

```go
func NewSimpleClient(addr string) (*grpc.ClientConn, error)
```

**What it does**: Convenience wrapper that creates a gRPC client with `DefaultConfig()`.

**How it's called**: Used throughout the codebase wherever a simple insecure connection is needed without custom configuration.

**Return value**: Delegates to `NewClient(addr, DefaultConfig())`.

---

### Function: `NewMinimalClient`

```go
func NewMinimalClient(addr string) (*grpc.ClientConn, error)
```

**What it does**: Creates a gRPC client with a zero-value `ClientConfig` (no message size limits, no compression, no extra options). Functionally identical to `NewSimpleClient` given that `DefaultConfig()` returns a zero-value `ClientConfig`, but semantically distinct — it signals "I explicitly want gRPC defaults."

**How it's called**: Used when callers want to opt out of any MonoFS-specific defaults and use bare gRPC behavior.

**Return value**: Delegates to `NewClient(addr, ClientConfig{})`.

---

## `internal/monopath/path.go`

Package `monopath` centralizes MonoFS path semantics. It defines how user-visible paths (like `ns/scout/org/repo/path/to/file`) split into a display-path (namespace prefix) and a file-path (within that namespace), and how those are converted to shard keys for the ingestion router.

---

### Function: `SplitDisplayPath`

```go
func SplitDisplayPath(fullPath string) (displayPath, filePath string, ok bool)
```

**What it does**: Splits a user-visible MonoFS path (e.g., `ns/scout/org/repo/file.go`) into its display-path (the mountable namespace prefix) and file-path (the remainder within that namespace). Returns `ok == false` if the path is empty, root-only, or doesn't have enough segments to identify a valid namespace.

**How it's called**: Called by `BuildShardKey`. Also used by any code that needs to decompose a full path into its namespace and inner-path parts (e.g., FUSE handlers, routing logic).

**Key parameters**:
- `fullPath` — a user-visible path like `"ns/scout/org/repo/src/main.go"` (may have leading/trailing slashes).

**Return values**:
- `displayPath` — the mountable namespace prefix (2 or 3 segments depending on the namespace type).
- `filePath` — the path within that namespace (empty string if the path points to the namespace root).
- `ok` — false if the path is invalid or too short.

**Implementation details**:

Namespace routing logic:

| First segment | displayPath length | Notes |
|---|---|---|
| `"dependency"` | 1 segment | e.g. `dependency/path/to/lib` |
| `"guardian-system"` | 1 segment | e.g. `guardian-system/path/to/file` |
| `"guardian"`, `"doctor"` | 2 segments | e.g. `guardian/instance/path/to/file` |
| anything else | 3 segments | conventional `ns/scout/org/repo/...` |

- Trailing/leading slashes are trimmed. The path `"/"` and `""` both return `ok == false`.
- If the path has exactly the needed number of segments for the display-path (no file-path), `filePath` is returned as `""`.

---

### Function: `BuildShardKey`

```go
func BuildShardKey(fullPath string) string
```

**What it does**: Converts a user-visible path into the shard key used by the ingestion router. For non-repo paths (those that don't match any known namespace pattern), returns `fullPath` unchanged. For repo paths, generates a deterministic storage ID from the display-path and combines it with the file-path.

**How it's called**: Called by the FUSE layer and any ingestion-client code that needs to produce the exact shard key the router expects.

**Key parameters**:
- `fullPath` — a user-visible MonoFS path.

**Return value**: A shard key string. For repo paths this is `"storageID:filePath"`; for namespace-root paths it's just the storage ID; for non-repo paths it's the original `fullPath` unchanged.

**Implementation details**:
- Internally calls `SplitDisplayPath` to decompose the path.
- Calls `sharding.GenerateStorageID(displayPath)` to produce a deterministic storage identifier.
- If there is a file-path, combines them via `sharding.BuildShardKey(storageID, filePath)`.
- If `SplitDisplayPath` returns `ok == false` (invalid/unrecognized path), returns the original `fullPath` as a pass-through fallback.

---

## `internal/nativeproto/protocol.go`

Package `nativeproto` defines the binary wire protocol for native FUSE mounts. It includes frame-level I/O (`ReadFrame`/`WriteFrame`), a binary encoder/decoder framework, and Encode/Decode functions for every message type.

### Constants

| Constant | Value | Meaning |
|---|---|---|
| `FrameMagic` | `0x53464e4d` | Magic number `"SFNM"` in little-endian |
| `Version1` | `1` | Only supported protocol version |
| `HeaderSize` | `52` | Fixed header size in bytes |
| `MaxFrameBytes` | `1 << 20` (1 MB) | Max total frame body size |
| `MaxReadBytes` | `256 << 10` (256 KB) | Max read data size |

**Opcodes** (`uint16`):

| Constant | Value | Purpose |
|---|---|---|
| `OpcodeHello` | `0x0001` | Capability negotiation |
| `OpcodeMount` | `0x0002` | Mount a namespace |
| `OpcodeUnmount` | `0x0003` | Unmount a namespace |
| `OpcodeLookup` | `0x0010` | Look up a directory entry |
| `OpcodeGetAttr` | `0x0011` | Get file/directory attributes |
| `OpcodeReadDir` | `0x0012` | Read directory entries |
| `OpcodeStatFS` | `0x0014` | Filesystem statistics |
| `OpcodeOpenRead` | `0x0020` | Open a file for reading |
| `OpcodeRead` | `0x0021` | Read file data |
| `OpcodeClose` | `0x0022` | Close an open handle |
| `OpcodeWatch` | `0x0030` | Subscribe to namespace events |
| `OpcodePing` | `0x0031` | Keepalive/heartbeat |

**Status codes** (`uint32`, iota starting at 0):

| Constant | Value | Meaning |
|---|---|---|
| `StatusOK` | `0` | Success |
| `StatusInvalidRequest` | `1` | Malformed request |
| `StatusAuth` | `2` | Authentication failure |
| `StatusNotFound` | `3` | Entry not found |
| `StatusNotDir` | `4` | Expected directory, got file |
| `StatusIsDir` | `5` | Expected file, got directory |
| `StatusStaleNamespace` | `6` | Namespace generation mismatch |
| `StatusStaleRoute` | `7` | Route TTL expired |
| `StatusUnavailable` | `8` | Service unavailable |
| `StatusBackendIO` | `9` | Backend I/O error |
| `StatusCancelled` | `10` | Operation cancelled |
| `StatusUnsupported` | `11` | Unsupported operation |

**Capabilities** (`uint64`, bitmask):

| Constant | Bit | Meaning |
|---|---|---|
| `CapabilityRouteTTLs` | `1 << 0` | Server supports route TTLs |
| `CapabilityStatFS` | `1 << 1` | Server supports statfs |

**Mount flags** (`uint32`, bitmask):

| Constant | Bit | Meaning |
|---|---|---|
| `MountFlagReadOnly` | `1 << 0` | Read-only mount |
| `MountFlagOverlayWrites` | `1 << 1` | Overlay writes |
| `MountFlagDebug` | `1 << 2` | Debug mode |

**Frame flag**:

| Constant | Bit | Meaning |
|---|---|---|
| `FlagMore` | `1 << 0` | More frames follow |

### Types

#### `ObjectID`

```go
type ObjectID [16]byte
```

A 128-bit object identifier (UUID-like) used to reference files and directories.

#### `Header`

```go
type Header struct {
    Magic      uint32
    Version    uint16
    Opcode     uint16
    Flags      uint32
    HeaderLen  uint32
    BodyLen    uint32
    RequestID  uint64
    SessionID  uint64
    Status     uint32
    Reserved   uint32
    Generation uint64
}
```

Fixed-size frame header (52 bytes). Every frame on the wire starts with this header followed by `BodyLen` bytes of body data.

#### `Attr`

```go
type Attr struct {
    Ino   uint64
    Mode  uint32
    Size  uint64
    Mtime int64
    Atime int64
    Ctime int64
    Nlink uint32
    UID   uint32
    GID   uint32
}
```

File/directory attributes (FUSE-like stat structure). Uses UNIX epoch `int64` for timestamps.

#### Wire message types (structs)

The file defines these request/response struct pairs, plus `DirEntry` and `StatFSResponse`:

- `HelloRequest` / `HelloResponse`
- `MountRequest` / `MountResponse`
- `LookupRequest` / `LookupResponse`
- `GetAttrRequest` / `GetAttrResponse`
- `DirEntry`
- `ReadDirRequest` / `ReadDirResponse`
- `StatFSResponse`
- `OpenReadRequest` / `OpenReadResponse`
- `ReadRequest` / `ReadResponse`
- `CloseRequest`

#### `encoder`

```go
type encoder struct {
    bytes.Buffer
}
```

Internal type for building binary wire format. Embeds `bytes.Buffer` and adds typed write methods for little-endian primitives.

#### `decoder`

```go
type decoder struct {
    data []byte
    off  int
}
```

Internal type for parsing binary wire format. Holds the raw byte slice and current read offset.

---

### Frame I/O Functions

#### Function: `ReadFrame`

```go
func ReadFrame(r io.Reader) (Header, []byte, error)
```

**What it does**: Reads a complete protocol frame from `r` — first the 52-byte `Header`, then the body bytes (whose length is specified by `Header.BodyLen`). Validates magic number, protocol version, header length, and body size against protocol constants.

**How it's called**: Called by server/client connection loops that read frames from a TCP/Unix socket. Every incoming frame passes through this function.

**Return values**:
- `Header` — the parsed frame header.
- `[]byte` — the raw body bytes (owned by the caller).
- `error` — on I/O failure or validation error (bad magic, wrong version, wrong header length, body too large).

**Implementation details**:
- Uses `io.ReadFull` for both the header and body to guarantee complete reads.
- Frame magic must equal `FrameMagic` (`0x53464e4d`).
- Version must equal `Version1` (`1`).
- `HeaderLen` must equal `HeaderSize` (`52`).
- `BodyLen` must not exceed `MaxFrameBytes` (`1 << 20`).

---

#### Function: `WriteFrame`

```go
func WriteFrame(w io.Writer, hdr Header, body []byte) error
```

**What it does**: Writes a complete protocol frame to `w`. Forces the header's `Magic`, `Version`, `HeaderLen`, and `BodyLen` to the correct protocol values before serializing. Writes the 52-byte header followed by the body.

**How it's called**: Called by server/client code that sends frames over TCP/Unix sockets. Every outgoing frame is serialized through this function.

**Key parameters**:
- `w` — the writer (typically a `net.Conn`).
- `hdr` — the header to serialize; `Magic`/`Version`/`HeaderLen`/`BodyLen` will be overwritten with protocol constants.
- `body` — the body bytes to append after the header.

**Return value**: An error if writing fails or if `len(body) > MaxFrameBytes`.

**Implementation details**:
- Validates body size before writing anything.
- Overwrites `hdr.Magic`, `hdr.Version`, `hdr.HeaderLen`, and `hdr.BodyLen` in the struct (the original values are ignored in favor of protocol correctness).
- If `body` is empty, only the header is written — no extra `Write` call for zero-length body.
- All integers are serialized in little-endian byte order.

---

### Encoder Methods (`*encoder`)

All encoder methods are unexported helpers that write typed values in little-endian format.

#### `(*encoder).u8`

```go
func (e *encoder) u8(v uint8)
```

Writes a single byte.

#### `(*encoder).u16`

```go
func (e *encoder) u16(v uint16)
```

Writes a 2-byte little-endian `uint16`.

#### `(*encoder).u32`

```go
func (e *encoder) u32(v uint32)
```

Writes a 4-byte little-endian `uint32`. Used for message counts, sizes, flags, TTLs, and mode bits.

#### `(*encoder).u64`

```go
func (e *encoder) u64(v uint64)
```

Writes an 8-byte little-endian `uint64`. Used for inode numbers, sizes, request IDs, session IDs, generation numbers, cookies, and handle IDs.

#### `(*encoder).i64`

```go
func (e *encoder) i64(v int64)
```

Writes an 8-byte little-endian `int64` by delegating to `u64(uint64(v))`. Used for timestamps (mtime, atime, ctime).

#### `(*encoder).bool`

```go
func (e *encoder) bool(v bool)
```

Encodes a boolean as a single byte: `1` for true, `0` for false.

#### `(*encoder).objectID`

```go
func (e *encoder) objectID(id ObjectID)
```

Writes a raw 16-byte `ObjectID` to the buffer.

#### `(*encoder).str`

```go
func (e *encoder) str(v string)
```

Writes a length-prefixed string: first a `uint32` length, then the raw string bytes. Empty strings write a length of 0 followed by no bytes.

---

### Decoder Methods (`*decoder`)

All decoder methods are unexported helpers that read typed values in little-endian format. Each returns the parsed value and an error (typically `io.ErrUnexpectedEOF` if there aren't enough bytes remaining).

#### `(*decoder).remaining`

```go
func (d *decoder) remaining() int
```

Returns the number of unconsumed bytes in the buffer (`len(d.data) - d.off`). Used by all other decoder methods to check boundary conditions.

#### `(*decoder).u8`

```go
func (d *decoder) u8() (uint8, error)
```

Reads and returns a single byte. Errors with `io.ErrUnexpectedEOF` if fewer than 1 byte remains.

#### `(*decoder).u16`

```go
func (d *decoder) u16() (uint16, error)
```

Reads and returns a little-endian `uint16`. Errors if fewer than 2 bytes remain.

#### `(*decoder).u32`

```go
func (d *decoder) u32() (uint32, error)
```

Reads and returns a little-endian `uint32`. Errors if fewer than 4 bytes remain.

#### `(*decoder).u64`

```go
func (d *decoder) u64() (uint64, error)
```

Reads and returns a little-endian `uint64`. Errors if fewer than 8 bytes remain.

#### `(*decoder).i64`

```go
func (d *decoder) i64() (int64, error)
```

Reads a little-endian `uint64` and casts to `int64`. Errors if fewer than 8 bytes remain.

#### `(*decoder).bool`

```go
func (d *decoder) bool() (bool, error)
```

Reads a single byte and returns `true` if non-zero. Errors if fewer than 1 byte remains.

#### `(*decoder).objectID`

```go
func (d *decoder) objectID() (ObjectID, error)
```

Reads 16 bytes into an `ObjectID`. Errors if fewer than 16 bytes remain.

#### `(*decoder).str`

```go
func (d *decoder) str() (string, error)
```

Reads a length-prefixed string: first a `uint32` length, then that many bytes of string data. Returns a new Go string (copying from the buffer). Errors if the length field is unreadable or if insufficient bytes remain for the string body.

---

### Attribute Encoding Helpers

#### Function: `encodeAttr`

```go
func encodeAttr(enc *encoder, attr Attr)
```

**What it does**: Serializes an `Attr` struct into the encoder buffer. Writes `Ino` (u64), `Mode` (u32), `Size` (u64), `Mtime` (i64), `Atime` (i64), `Ctime` (i64), `Nlink` (u32), `UID` (u32), `GID` (u32) — in that order.

**How it's called**: Used internally by `EncodeMountResponse`, `EncodeLookupResponse`, `EncodeGetAttrResponse`, and `EncodeOpenReadResponse`.

**Key parameters**:
- `enc` — the encoder to write into.
- `attr` — the attribute values to serialize.

**Return value**: None (writes directly to the encoder buffer).

---

#### Function: `decodeAttr`

```go
func decodeAttr(dec *decoder) (Attr, error)
```

**What it does**: Deserializes an `Attr` struct from the decoder buffer, reading the nine fields in the same order written by `encodeAttr`. Returns a zero-value `Attr` on any read error.

**How it's called**: Used internally by `DecodeMountResponse`, `DecodeLookupResponse`, `DecodeGetAttrResponse`, and `DecodeOpenReadResponse`.

**Return values**:
- `Attr` — the parsed attributes.
- `error` — from any decoder read failure (typically `io.ErrUnexpectedEOF`).

---

### Message Encode/Decode Functions

Each message type has a paired `Encode*` and `Decode*` function. All follow the same pattern: `Encode*` serializes a request/response struct into `[]byte`; `Decode*` deserializes `[]byte` back into the struct, returning an error on any parse failure.

---

#### `EncodeHelloRequest` / `DecodeHelloRequest`

```go
func EncodeHelloRequest(req HelloRequest) []byte
func DecodeHelloRequest(data []byte) (HelloRequest, error)
```

Serializes/deserializes a `HelloRequest`. Wire format: `MinVersion` (u16), `MaxVersion` (u16), `RequestedCaps` (u64), `ClientKind` (str), `ClientVersion` (str), `KernelRelease` (str).

---

#### `EncodeHelloResponse` / `DecodeHelloResponse`

```go
func EncodeHelloResponse(resp HelloResponse) []byte
func DecodeHelloResponse(data []byte) (HelloResponse, error)
```

Serializes/deserializes a `HelloResponse`. Wire format: `SelectedVersion` (u16), `ServerCaps` (u64), `MaxFrameBytes` (u32), `MaxReadBytes` (u32).

---

#### `EncodeMountRequest` / `DecodeMountRequest`

```go
func EncodeMountRequest(req MountRequest) []byte
func DecodeMountRequest(data []byte) (MountRequest, error)
```

Serializes/deserializes a `MountRequest`. Wire format: `MountFlags` (u32), `ClientID` (str), `Hostname` (str), `AuthToken` (str).

---

#### `EncodeMountResponse` / `DecodeMountResponse`

```go
func EncodeMountResponse(resp MountResponse) []byte
func DecodeMountResponse(data []byte) (MountResponse, error)
```

Serializes/deserializes a `MountResponse`. Wire format: `ClusterVersion` (u64), `NamespaceGeneration` (u64), `GuardianVisible` (bool), `RootObjectID` (16 bytes), `Root` (Attr via `encodeAttr`/`decodeAttr`), `EntryTTLMS` (u32), `AttrTTLMS` (u32), `DirTTLMS` (u32), `RouteTTLMS` (u32).

---

#### `EncodeLookupRequest` / `DecodeLookupRequest`

```go
func EncodeLookupRequest(req LookupRequest) []byte
func DecodeLookupRequest(data []byte) (LookupRequest, error)
```

Serializes/deserializes a `LookupRequest`. Wire format: `ParentObjectID` (16 bytes), `Name` (str).

---

#### `EncodeLookupResponse` / `DecodeLookupResponse`

```go
func EncodeLookupResponse(resp LookupResponse) []byte
func DecodeLookupResponse(data []byte) (LookupResponse, error)
```

Serializes/deserializes a `LookupResponse`. Wire format: `Found` (bool), `EntryTTLMS` (u32). If `Found` is true, followed by `ObjectID` (16 bytes) and `Attr` (via `encodeAttr`/`decodeAttr`). The decoder conditionalizes on `Found` to skip the trailing fields when the entry was not found.

---

#### `EncodeGetAttrRequest` / `DecodeGetAttrRequest`

```go
func EncodeGetAttrRequest(req GetAttrRequest) []byte
func DecodeGetAttrRequest(data []byte) (GetAttrRequest, error)
```

Serializes/deserializes a `GetAttrRequest`. Wire format: `ObjectID` (16 bytes).

---

#### `EncodeGetAttrResponse` / `DecodeGetAttrResponse`

```go
func EncodeGetAttrResponse(resp GetAttrResponse) []byte
func DecodeGetAttrResponse(data []byte) (GetAttrResponse, error)
```

Serializes/deserializes a `GetAttrResponse`. Wire format: `Found` (bool), `AttrTTLMS` (u32). If `Found` is true, followed by `Attr` (via `encodeAttr`/`decodeAttr`).

---

#### `EncodeReadDirRequest` / `DecodeReadDirRequest`

```go
func EncodeReadDirRequest(req ReadDirRequest) []byte
func DecodeReadDirRequest(data []byte) (ReadDirRequest, error)
```

Serializes/deserializes a `ReadDirRequest`. Wire format: `DirObjectID` (16 bytes), `Cookie` (u64), `MaxEntries` (u32), `MaxBytes` (u32).

---

#### `EncodeReadDirResponse` / `DecodeReadDirResponse`

```go
func EncodeReadDirResponse(resp ReadDirResponse) []byte
func DecodeReadDirResponse(data []byte) (ReadDirResponse, error)
```

Serializes/deserializes a `ReadDirResponse`. Wire format: `DirTTLMS` (u32), `NextCookie` (u64), `EOF` (bool), entry count (u32), followed by that many `DirEntry` records. Each entry: `Name` (str), `ObjectID` (16 bytes), `Ino` (u64), `Mode` (u32).

---

#### `EncodeStatFSResponse` / `DecodeStatFSResponse`

```go
func EncodeStatFSResponse(resp StatFSResponse) []byte
func DecodeStatFSResponse(data []byte) (StatFSResponse, error)
```

Serializes/deserializes a `StatFSResponse`. Wire format: `Blocks` (u64), `Bfree` (u64), `Bavail` (u64), `Files` (u64), `Ffree` (u64), `Bsize` (u32), `Frsize` (u32), `NameLen` (u32).

---

#### `EncodeOpenReadRequest` / `DecodeOpenReadRequest`

```go
func EncodeOpenReadRequest(req OpenReadRequest) []byte
func DecodeOpenReadRequest(data []byte) (OpenReadRequest, error)
```

Serializes/deserializes an `OpenReadRequest`. Wire format: `ObjectID` (16 bytes).

---

#### `EncodeOpenReadResponse` / `DecodeOpenReadResponse`

```go
func EncodeOpenReadResponse(resp OpenReadResponse) []byte
func DecodeOpenReadResponse(data []byte) (OpenReadResponse, error)
```

Serializes/deserializes an `OpenReadResponse`. Wire format: `HandleID` (u64), `Attr` (via `encodeAttr`/`decodeAttr`), `RouteTTLMS` (u32).

---

#### `EncodeReadRequest` / `DecodeReadRequest`

```go
func EncodeReadRequest(req ReadRequest) []byte
func DecodeReadRequest(data []byte) (ReadRequest, error)
```

Serializes/deserializes a `ReadRequest`. Wire format: `HandleID` (u64), `Offset` (u64), `Length` (u32).

---

#### `EncodeReadResponse` / `DecodeReadResponse`

```go
func EncodeReadResponse(resp ReadResponse) []byte
func DecodeReadResponse(data []byte) (ReadResponse, error)
```

Serializes/deserializes a `ReadResponse`. Wire format: `EOF` (bool), data length (u32), raw data bytes. The `DecodeReadResponse` makes a copy of the data slice (`append([]byte(nil), ...)`) so the caller owns the bytes independently of the decoder buffer.

---

#### `EncodeCloseRequest` / `DecodeCloseRequest`

```go
func EncodeCloseRequest(req CloseRequest) []byte
func DecodeCloseRequest(data []byte) (CloseRequest, error)
```

Serializes/deserializes a `CloseRequest`. Wire format: `HandleID` (u64).

---

## `internal/workspacebundle/bundle.go`

Package `workspacebundle` defines the JSON-based workspace bundle format for seeding workspace filesystem state. A bundle carries a list of repositories with file operations (upsert, delete, mkdir, rmdir, symlink, rename, chmod) to apply on top of a git base commit.

### Types

#### `Bundle`

```go
type Bundle struct {
    WorkspaceID  string             `json:"workspace_id"`
    Repositories []RepositoryBundle `json:"repositories"`
}
```

Top-level workspace bundle. Contains a unique workspace identifier and a list of repository bundles.

#### `RepositoryBundle`

```go
type RepositoryBundle struct {
    StorageID   string      `json:"storage_id"`
    DisplayPath string      `json:"display_path"`
    RepoURL     string      `json:"repo_url"`
    Branch      string      `json:"branch"`
    BaseCommit  string      `json:"base_commit"`
    Operations  []Operation `json:"operations"`
}
```

Describes a single repository within a workspace. `StorageID` is the shard-storage identifier. `DisplayPath` is the user-visible namespace prefix. `BaseCommit` is the git commit that operations are applied on top of.

#### `Operation`

```go
type Operation struct {
    Kind    string `json:"kind"`
    Path    string `json:"path"`
    Mode    int64  `json:"mode,omitempty"`
    Content []byte `json:"content,omitempty"`
    Target  string `json:"target,omitempty"`
}
```

A single filesystem operation. `Kind` must be one of the `Operation*` constants. `Mode` applies to `mkdir` and `chmod`. `Content` is file body bytes for `upsert`. `Target` is the symlink target or rename destination.

### Operation Constants

| Constant | Value | Meaning |
|---|---|---|
| `OperationUpsert` | `"upsert"` | Create or overwrite a file with content |
| `OperationDelete` | `"delete"` | Remove a file |
| `OperationMkdir` | `"mkdir"` | Create a directory |
| `OperationRmdir` | `"rmdir"` | Remove a directory |
| `OperationSymlink` | `"symlink"` | Create a symbolic link |
| `OperationRename` | `"rename"` | Rename a file/directory |
| `OperationChmod` | `"chmod"` | Change file mode |

---

### Function: `Parse`

```go
func Parse(data []byte) (*Bundle, error)
```

**What it does**: Parses a JSON byte slice into a `*Bundle`, then validates it. Returns an error if the JSON is invalid or if validation fails.

**How it's called**: Called by ingestion endpoints (e.g., gRPC handlers) when receiving a workspace bundle from a client. Also called by test fixtures.

**Key parameters**:
- `data` — raw JSON bytes representing a workspace bundle.

**Return values**:
- `*Bundle` — the parsed and validated bundle (nil on error).
- `error` — wraps JSON decode errors with `"decode workspace bundle:"` prefix, or returns validation errors directly.

**Implementation details**: Uses `json.Unmarshal` then calls `bundle.Validate()`. The returned pointer is owned by the caller.

---

### Function: `(*Bundle).Validate`

```go
func (b *Bundle) Validate() error
```

**What it does**: Validates that the bundle is non-nil, has a non-empty `WorkspaceID`, contains at least one repository, and that every repository and operation passes individual validation.

**How it's called**: Called automatically by `Parse`. May also be called independently by code that constructs bundles programmatically.

**Return value**: An error describing the first validation failure, or nil.

**Implementation details**:

Checks performed on the bundle:
1. `b` is not nil.
2. `WorkspaceID` is not blank.
3. At least one `Repositories` entry exists.
4. No duplicate `StorageID` values across repositories.

Checks per repository:
5. `StorageID` is not blank.
6. `DisplayPath` is not blank.
7. `RepoURL` is not blank.
8. `Branch` is not blank.
9. `BaseCommit` is not blank.
10. Each operation passes `validateOperation`.

---

### Function: `(*Bundle).RepositoryRefs`

```go
func (b *Bundle) RepositoryRefs() []*pb.WorkspaceRepositoryRef
```

**What it does**: Converts the bundle's repositories into a slice of protobuf `WorkspaceRepositoryRef` messages. Returns nil if the bundle is nil (nil-safe).

**How it's called**: Called by ingestion code to convert bundle data into the protobuf format expected by downstream services.

**Return value**: A slice of `*pb.WorkspaceRepositoryRef`, one per repository in the bundle.

**Implementation details**: Maps `StorageID`, `DisplayPath`, `RepoURL`, `Branch`, and `BaseCommit` directly into the corresponding protobuf fields. Does not include operations.

---

### Function: `(*Bundle).RepositoryByStorageID`

```go
func (b *Bundle) RepositoryByStorageID(storageID string) *RepositoryBundle
```

**What it does**: Looks up a `RepositoryBundle` by its `StorageID`. Returns nil if the bundle is nil or no repository matches.

**How it's called**: Called by code that needs to find a specific repository's operations within a bundle (e.g., ingestion processing loops).

**Key parameters**:
- `storageID` — the storage identifier to search for.

**Return value**: A pointer to the matching `RepositoryBundle`, or nil.

**Implementation details**: Linear scan over `b.Repositories`. Returns a pointer into the bundle's slice (the caller can mutate the struct but not the slice).

---

### Function: `validateOperation`

```go
func validateOperation(storageID string, idx int, op Operation) error
```

**What it does**: Validates a single `Operation` within a repository. Checks that the operation kind is one of the recognized constants, the path is non-empty and safe (no absolute paths, no `..` traversal, no `.git` components), and that required fields for `symlink` and `rename` operations are present.

**How it's called**: Called by `Bundle.Validate` and `SourceCommitBundle.Validate` during validation, once per operation.

**Key parameters**:
- `storageID` — the repository's storage ID (used only in error messages).
- `idx` — the operation's index within the repository (used in error messages).
- `op` — the operation to validate.

**Return value**: An error describing the validation failure, or nil.

**Implementation details**:

Checks performed:
1. `op.Kind` must be one of: `"upsert"`, `"delete"`, `"mkdir"`, `"rmdir"`, `"symlink"`, `"rename"`, `"chmod"`.
2. `op.Path` must be non-blank.
3. `op.Path` must pass `isSafeRelativePath`.
4. If kind is `"symlink"`, `op.Target` must be non-blank.
5. If kind is `"rename"`, `op.Target` must be non-blank.

---

### Function: `isSafeRelativePath`

```go
func isSafeRelativePath(path string) bool
```

**What it does**: Checks whether a path is a safe relative path — not empty, not `.`, not absolute, doesn't escape the root via `..`, and doesn't traverse into `.git`.

**How it's called**: Called by `validateOperation` for every operation path.

**Key parameters**:
- `path` — the raw relative path string.

**Return value**: `true` if the path is safe to use; `false` otherwise.

**Implementation details**:
- Rejects `"."`, `""`, and absolute paths (`filepath.IsAbs`).
- Runs `filepath.Clean` to resolve `..` and `.` components, then rejects if the cleaned result is `"."`, `".."`, or starts with `"../"`.
- Splits on `"/"` and rejects any path component equal to `".git"` — prevents operations from touching the git directory.

---

## `internal/workspacebundle/commit_bundle.go`

Package `workspacebundle` (same package, separate file) defines the source-commit bundle format. A source commit bundle represents one or more git commits, each carrying repository operations that should be applied to the workspace.

### Types

#### `SourceCommitBundle`

```go
type SourceCommitBundle struct {
    WorkspaceID   string         `json:"workspace_id"`
    PrincipalID   string         `json:"principal_id,omitempty"`
    LogicalBranch string         `json:"logical_branch,omitempty"`
    Commits       []SourceCommit `json:"commits"`
}
```

Top-level source-commit bundle. Contains optional metadata (`PrincipalID`, `LogicalBranch`) and a list of commits.

#### `SourceCommit`

```go
type SourceCommit struct {
    ID            string                   `json:"id"`
    ParentID      string                   `json:"parent_id,omitempty"`
    Message       string                   `json:"message"`
    AuthorName    string                   `json:"author_name,omitempty"`
    AuthorEmail   string                   `json:"author_email,omitempty"`
    PrincipalID   string                   `json:"principal_id,omitempty"`
    CreatedAtUnix int64                    `json:"created_at_unix,omitempty"`
    Repositories  []SourceCommitRepository `json:"repositories"`
}
```

A single commit. `ID` is the commit hash. `ParentID` is the parent commit hash (empty for root commits). `Repositories` lists the repository operations for this commit.

#### `SourceCommitRepository`

```go
type SourceCommitRepository struct {
    StorageID   string      `json:"storage_id"`
    DisplayPath string      `json:"display_path"`
    RepoURL     string      `json:"repo_url"`
    Branch      string      `json:"branch"`
    BaseCommit  string      `json:"base_commit"`
    Operations  []Operation `json:"operations"`
}
```

Identical in structure to `RepositoryBundle` (same fields, same semantics). Reuses the `Operation` type from `bundle.go`.

---

### Function: `ParseSourceCommitBundle`

```go
func ParseSourceCommitBundle(data []byte) (*SourceCommitBundle, error)
```

**What it does**: Parses a JSON byte slice into a `*SourceCommitBundle`, then validates it. Returns an error if the JSON is invalid or if validation fails.

**How it's called**: Called by ingestion endpoints when receiving a commit bundle from a client.

**Key parameters**:
- `data` — raw JSON bytes representing a source commit bundle.

**Return values**:
- `*SourceCommitBundle` — the parsed and validated bundle (nil on error).
- `error` — wraps JSON decode errors with `"decode source commit bundle:"` prefix, or returns validation errors directly.

**Implementation details**: Same pattern as `Parse` in `bundle.go`: `json.Unmarshal` then `bundle.Validate()`.

---

### Function: `(*SourceCommitBundle).Validate`

```go
func (b *SourceCommitBundle) Validate() error
```

**What it does**: Validates the source commit bundle: checks for non-nil, non-empty `WorkspaceID`, at least one commit, no duplicate commit IDs, and validates every repository and operation within each commit.

**How it's called**: Called automatically by `ParseSourceCommitBundle`. May also be called independently.

**Return value**: An error describing the first validation failure, or nil.

**Implementation details**:

Checks performed:
1. `b` is not nil.
2. `WorkspaceID` is not blank.
3. At least one commit exists.
4. No duplicate commit IDs across commits.
5. Each commit has at least one repository.
6. No duplicate `StorageID` values within a single commit (deduplicated per-commit, not across commits).
7. Each repository has non-blank `StorageID`, `DisplayPath`, `RepoURL`, `Branch`, `BaseCommit`.
8. Each operation passes `validateOperation` (shared with `bundle.go`).

---

### Function: `(*SourceCommitBundle).RepositoryRefs`

```go
func (b *SourceCommitBundle) RepositoryRefs() []*pb.WorkspaceRepositoryRef
```

**What it does**: Collects all unique repository references across all commits and returns them as protobuf `WorkspaceRepositoryRef` messages, deduplicated by `StorageID`. Returns nil if the bundle is nil.

**How it's called**: Called by ingestion code to get a flat, deduplicated list of all repositories referenced by any commit in the bundle.

**Return value**: A deduplicated slice of `*pb.WorkspaceRepositoryRef`.

**Implementation details**: Iterates all commits and their repositories, using a `map[string]struct{}` keyed by `StorageID` to skip duplicates. Since bundles can have multiple commits touching the same repository, this ensures only one reference per storage ID is emitted.

---

### Function: `(*SourceCommitBundle).LocalCommitIDs`

```go
func (b *SourceCommitBundle) LocalCommitIDs() []string
```

**What it does**: Extracts all non-blank commit IDs from the bundle and returns them as a string slice. Returns nil if the bundle is nil.

**How it's called**: Called by ingestion code that needs the list of commit IDs for tracking/processing purposes.

**Return value**: A slice of commit ID strings (in bundle order), with blank IDs filtered out.

---

### Function: `(*SourceCommitBundle).RepositoryByStorageID`

```go
func (b *SourceCommitBundle) RepositoryByStorageID(storageID string) *SourceCommitRepository
```

**What it does**: Searches all commits' repositories for one matching the given `StorageID`. Returns the first match found (commits are searched in order). Returns nil if the bundle is nil or no match is found.

**How it's called**: Called by code that needs to find a specific repository's data across all commits.

**Key parameters**:
- `storageID` — the storage identifier to search for.

**Return value**: A pointer to the matching `SourceCommitRepository`, or nil.

**Implementation details**: Linear scan over commits, then linear scan over each commit's repositories. Returns the first match. Multiple commits may reference the same storage ID; this returns whichever appears first.
