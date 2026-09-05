# MonoFS FUSE Layer Documentation — Part 2

This document covers all exported and unexported functions in the following source files:

- `op_create.go`, `op_getattr.go`, `op_lookup.go`, `op_mkdir.go`, `op_open.go`
- `op_read.go`, `op_readdir.go`, `op_rename.go`, `op_rmdir.go`, `op_setattr.go`
- `op_statfs.go`, `op_symlink.go`, `op_unlink.go`, `op_write.go`
- `workspace.go`, `workspace_git.go`, `writable.go`

---

## op_create.go

### `(*MonoNode) Create`

```go
func (n *MonoNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno)
```

**What it does:** Implements `fs.NodeCreater` for the go-fuse library. Creates a new regular file in the FUSE filesystem. Called by the Linux kernel when a userspace process issues `creat(2)` or `open(2)` with `O_CREAT`.

**How it's called:** By go-fuse's FUSE server in response to CREATE kernel requests from the mounted FUSE filesystem.

**Key parameters:**
- `name` — the basename of the file to create
- `flags` — open flags from the kernel (e.g. `O_RDWR`, `O_TRUNC`)
- `mode` — permission bits for the new file
- `out` — output `fuse.EntryOut` struct filled with the new file's attributes

**Return values:**
- `*fs.Inode` — the new inode for the child file (or nil on failure)
- `fs.FileHandle` — a `*monofsFileHandle` wrapping the underlying `os.File` (or nil)
- `uint32` — FUSE open flags for the kernel; always `fuse.FOPEN_DIRECT_IO`
- `syscall.Errno` — 0 on success, `EROFS`, `EPERM`, or `EIO` on failure

**Implementation details:**
1. Panic recovery via `defer n.recoverPanic("Create")`.
2. Returns `EROFS` if `sessionMgr` is nil (read-only mode) or the parent path is a workspace read-only path.
3. Returns `EROFS` if the parent path is the filesystem root (`n.path == ""`); files cannot be created directly under `/`.
4. Checks whether the parent is inside a user-created root directory (`IsUserRootDir`) or inside a repository.
5. Ensures a write session exists via `n.sessionMgr.StartSession()`.
6. Checks `isWorkspaceReadOnlyPath` and `shouldHideWorkspacePath` on the new full path (`n.path + "/" + name`).
7. Resolves the physical on-disk path via `n.sessionMgr.GetLocalPath(newPath)`.
8. Creates parent directories with `os.MkdirAll`.
9. Creates the file with `os.OpenFile` using `O_CREATE|O_RDWR|O_TRUNC`.
10. Tracks the creation via `n.sessionMgr.TrackChange(ChangeCreate, newPath, "")`.
11. Constructs a child `MonoNode` via `n.newChild`, marks it as a local write (`isLocalWrite = true`), stores the file handle in `localHandle`, and stamps it via `stampNode()`.
12. Populates `fuse.EntryOut` with mode, size (0), inode number (via `hashPathForNode`), nlink=1, and sets timeouts via `overlayEntryTimeout()`.
13. Creates a `monofsFileHandle` wrapping the open `*os.File`.
14. Returns `FOPEN_DIRECT_IO` to force the kernel to bypass the page cache for overlay-tracked files.

---

## op_getattr.go

### `(*MonoNode) Getattr`

```go
func (n *MonoNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno
```

**What it does:** Implements `fs.NodeGetattrer` for the go-fuse library. Returns file attributes (stat info) for the node. This is the FUSE equivalent of `stat(2)`.

**How it's called:** By go-fuse on every `getattr` kernel request. Called frequently by the kernel to revalidate cached attributes.

**Key parameters:**
- `f` — optional file handle; unused in this implementation
- `out` — output `fuse.AttrOut` filled with the node's attributes

**Return value:** 0 on success, `ENOENT`, `EINTR`, or a converted error on failure.

**Implementation details:** The function resolves attributes through a priority chain:
1. **FS_ERROR.txt special case** (path == "FS_ERROR.txt" with backend error): Returns a synthetic read-only regular file containing the error message, with inode `0xFFFFFFFF`.
2. **Root directory** (path == ""): Returns synthetic directory attributes (mode 0755, nlink=2).
3. **Synthetic workspace paths**: Delegates to `n.getattrSyntheticWorkspacePath()`. If handled, returns.
4. **Hidden workspace paths**: Returns `ENOENT` if `shouldHideWorkspacePath` is true.
5. **Session manager / overlay check** (if `sessionMgr != nil` and not a system view path):
   - **Doctor virtual file** (`doctor/v1/query/<id>/results.json`): Returns synthetic file attributes.
   - **User root directory** (single-component path, `IsUserRootDir`): Returns synthetic directory attributes.
   - **Deleted in overlay** (`IsDeleted`): Returns `ENOENT`.
   - **Symlink in overlay** (`IsSymlink`): Returns symlink attributes with target length as size.
   - **Has override in overlay DB** (`HasOverride`): Delegates to `n.getattrFromOverlay()`.
   - **Under user root dir** (parent component `IsUserRootDir`): Only overlay matters; returns `ENOENT` if not found in overlay.
6. **Cache check** (`n.cache.GetAttr(backendPath)`): On cache hit, returns cached attributes. Adds write bits to mode if session manager is present and path is not a dependency/system path.
7. **Backend query** (`n.client.GetAttr`): Fetches attributes from the remote backend. Retries up to `maxMetadataRetries` with exponential backoff. On failure, updates the backend error state and returns a converted error code. On success, clears the backend error and populates the cache.
8. Adds write bits (`addWriteBits`) to the mode if the session manager is present (non-dependency, non-system paths) to make backend files appear writable.
9. Sets `backendEntryTimeout()` for timeout when session manager is present (shorter invalidation), otherwise `attrTimeout()`.

---

## op_lookup.go

### `(*MonoNode) Lookup`

```go
func (n *MonoNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno)
```

**What it does:** Implements `fs.NodeLookuper` for the go-fuse library. Looks up a child entry by name within a directory. This is the most frequently called FUSE operation — it maps a name to an inode.

**How it's called:** By go-fuse on every `lookup` kernel request when the kernel needs to resolve a path component.

**Key parameters:**
- `name` — the basename of the child to look up
- `out` — output `fuse.EntryOut` filled with the child's attributes

**Return values:**
- `*fs.Inode` — the inode for the child (nil if not found)
- `syscall.Errno` — 0 on success, `ENOENT`, `EINTR`, or converted error on failure

**Implementation details:**
1. Panic recovery and operation recording.
2. **FS_ERROR.txt lookup in root**: If `name == "FS_ERROR.txt"` and `n.path == ""`, checks for catastrophic errors and backend errors. If any error exists, creates a synthetic child node with the error message as content. Returns `ENOENT` if no error.
3. **Synthetic workspace entries**: Delegates to `n.lookupSyntheticWorkspaceEntry()`. If handled, returns.
4. Builds `childPath` (synthetic FUSE path) and `backendChildPath` (path on the cluster backend).
5. **Hidden workspace paths**: Returns `ENOENT` if `shouldHideWorkspacePath` is true.
6. **Session manager / overlay check** (if `sessionMgr != nil` and not a system view path):
   - **Doctor virtual file** (`doctor/v1/query/<id>/results.json`): Creates a synthetic child node.
   - **User root directory** (parent path is root and `IsUserRootDir`): Creates a directory child with `hashPathForNode` inode.
   - **Deleted in overlay** (`IsDeleted`): Returns `ENOENT`.
   - **Symlink in overlay** (`IsSymlink`): Creates a symlink child with the target stored in `symlinkTarget`.
   - **Has override in overlay DB** (`HasOverride`): Delegates to `n.lookupFromOverlay()`.
   - **Under user root dir**: Only overlay matters; delegates to `n.lookupFromOverlay()` and returns `ENOENT` on miss.
7. **Cache check** (`n.cache.GetAttr(backendChildPath)`): On hit, returns cached attributes with write bits added as appropriate. Builds a child node with `n.newChild`.
8. **Backend query** (`n.client.Lookup(backendChildPath)`): Retries up to `maxMetadataRetries`. On failure updates backend error and returns converted error. On success clears backend error and populates cache.
9. Sets `backendEntryTimeout()` for both entry and attr timeouts when session manager is active (shorter invalidation), otherwise `attrTimeout()`.

---

## op_mkdir.go

### `(*MonoNode) Mkdir`

```go
func (n *MonoNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno)
```

**What it does:** Implements `fs.NodeMkdirer` for the go-fuse library. Creates a new directory in the FUSE filesystem. Called when a process issues `mkdir(2)`.

**How it's called:** By go-fuse in response to MKDIR kernel requests.

**Key parameters:**
- `name` — the name of the directory to create
- `mode` — permission bits
- `out` — output `fuse.EntryOut` filled with the new directory's attributes

**Return values:**
- `*fs.Inode` — the inode for the new directory (nil on failure)
- `syscall.Errno` — 0 on success, `EROFS`, `EPERM`, or `EIO` on failure

**Implementation details:**
1. Returns `EROFS` if `sessionMgr` is nil (read-only) or the parent is a workspace read-only path.
2. Ensures a write session exists via `n.sessionMgr.StartSession()`.
3. **Root-level directory creation** (`n.path == ""`): Allows user-created root directories. Checks `shouldReserveWorkspaceRoot(name)` — returns `EPERM` for reserved names (guardian, doctor, etc.). Calls `n.sessionMgr.CreateUserRootDir(name)`. Creates a child node, stamps it, populates attributes with `hashPathForNode`.
4. **Subdirectory creation** (`n.path != ""`): Checks `isWorkspaceReadOnlyPath(newPath)` and `shouldHideWorkspacePath(newPath)`. Resolves local path via `n.sessionMgr.GetLocalPath`. Creates directory with `os.MkdirAll`. Tracks change via `n.sessionMgr.TrackChange(ChangeMkdir, ...)`. Creates child node with `n.newChild`.
5. Sets `nlink=2` for directories (standard Unix convention: "." and ".." entries).

---

## op_open.go

### `(*MonoNode) Open`

```go
func (n *MonoNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno)
```

**What it does:** Implements `fs.NodeOpener` for the go-fuse library. Opens a file for read or write. This is where the FUSE layer decides whether to serve content from the backend, overlay, or virtual files.

**How it's called:** By go-fuse on `open` kernel requests from `open(2)` syscalls.

**Key parameters:**
- `flags` — open flags from the kernel (e.g. `O_RDONLY`, `O_WRONLY`, `O_RDWR`, `O_CREAT`, `O_TRUNC`, `O_APPEND`)

**Return values:**
- `fs.FileHandle` — a `*monofsFileHandle` for disk-backed files, or nil for cache-backed/virtual files
- `uint32` — FUSE open response flags: `FOPEN_DIRECT_IO` (bypass kernel page cache) or `FOPEN_KEEP_CACHE` (allow kernel caching)
- `syscall.Errno` — 0 on success

**Implementation details:** The function follows a priority-based resolution path:
1. **FS_ERROR.txt** (path == "FS_ERROR.txt", content already set): Returns `FOPEN_KEEP_CACHE`.
2. **Synthetic workspace file** (e.g. `.gitignore`, `workspace.json`): Loads content via `loadSyntheticWorkspaceFileContent`, stores in `n.content` under lock, returns `FOPEN_KEEP_CACHE`.
3. **Write mode detection**: Checks `flags` for `O_WRONLY`, `O_RDWR`, `O_APPEND`, `O_TRUNC`, or `O_CREAT`. If any are set, this is treated as a write path.
4. **Write path**: Returns `EROFS` if no session manager or read-only workspace path. Ensures write session, calls `ensureLocalCopy(ctx)` to materialize a local copy from the backend, opens the local file with `O_RDWR` (respecting `O_APPEND` and `O_TRUNC` from flags), marks `isLocalWrite = true`, creates a `monofsFileHandle`, stamps the node, returns `FOPEN_DIRECT_IO`.
5. **Stale-inode guard**: Checks `sessionMgr.GetPathState(n.path).IsDeleted` — returns `ENOENT` if the file was deleted via Rename.
6. **Overlay for reads** (`HasLocalOverride`): Opens the local file read-only, creates a `monofsFileHandle`, returns `FOPEN_DIRECT_IO`.
7. **Doctor virtual file** (`doctor/v1/query/<id>/results.json`): Returns nil handle with `FOPEN_DIRECT_IO` (read will be intercepted in `op_read.go`).
8. **User root dir files**: Opens the local file on disk read-only via `os.OpenFile`, returns a `monofsFileHandle` with `FOPEN_DIRECT_IO`. If not found, returns `ENOENT`.
9. **Backend read**: Calls `n.client.Read(ctx, n.backendPath(), 0, 0)` to fetch the entire file content. Retries up to 3 times with exponential backoff + jitter. On success, stores content in `n.content` and updates `n.size` under lock. Normalizes nil content to empty slice.
10. **DIRECT_IO decisions**:
    - If actual content size differs from metadata size (synthetic/generated files), returns `FOPEN_DIRECT_IO`.
    - If the path is a dependency path and dependencies were pushed recently (`DepsPushedRecently()`), returns `FOPEN_DIRECT_IO` to prevent stale cache reads (important for `go mod verify`).
    - Otherwise returns `FOPEN_KEEP_CACHE`.

---

## op_read.go

### `(*MonoNode) Read`

```go
func (n *MonoNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno)
```

**What it does:** Implements `fs.NodeReader` for the go-fuse library. Reads file content from a given offset into `dest`. This is the FUSE equivalent of `read(2)`.

**How it's called:** By go-fuse on `read` kernel requests. Also called internally when the file handle's `Read` method is not delegated.

**Key parameters:**
- `f` — file handle; if non-nil and of type `*monofsFileHandle`, delegates to its `Read` method
- `dest` — destination byte slice (the kernel provides the buffer)
- `off` — offset in bytes from the start of the file

**Return values:**
- `fuse.ReadResult` — the read data, or nil for EOF
- `syscall.Errno` — 0 on success, `EIO` on failure

**Implementation details:**
1. **File handle delegation**: If `f` is a `*monofsFileHandle`, immediately delegates to `mfh.Read(ctx, dest, off)`. This is the common path for overlay/opened files.
2. **Synthetic workspace file**: Tries `loadSyntheticWorkspaceFileContent`. If handled, slices content into `dest` from `off`.
3. **Doctor virtual file** (`doctor/v1/query/<id>/results.json`): Reads the statement file, checks if results are stale (by comparing modification times), calls `ensureDoctorQueryResults` to run the query via RPC, then reads the local results file via `readLocalFileRange`.
4. **File handle re-check**: Checks `f` for `*monofsFileHandle` again (in case doctor handling created a new handle).
5. **Overlay disk read**: If `HasLocalOverride(n.path)` is true, reads the file from disk using `os.ReadFile` and slices the data. This handles cases where go-fuse dispatches to the node's Read instead of the file handle's Read.
6. **In-memory content**: Reads from `n.content` under a read lock.
7. **Reload on nil content**: If `n.content` is nil (e.g., after cleanup or a race):
   - For user root dir paths, returns `EIO` immediately (backend doesn't know about these).
   - Otherwise retries `n.client.Read(ctx, n.backendPath(), 0, 0)` up to 3 times with exponential backoff. Normalizes nil content to empty slice. On failure, records the error and returns `EIO`.
8. **Common read logic**: Computes the end index (`min(int(off)+len(dest), len(content))`). If `off >= len(content)`, returns nil (EOF). Otherwise records the operation and bytes read, then returns the sliced content.

### `(*MonoNode) ensureDoctorQueryResults`

```go
func (n *MonoNode) ensureDoctorQueryResults(ctx context.Context, statementPath, resultsPath string) error
```

**What it does:** Ensures that doctor query results are cached locally and up-to-date. Compares modification times of the statement file and results file; if results are stale or missing, executes the query via the backend RPC and writes results to a local file.

**Key parameters:**
- `statementPath` — path to the statement/query file
- `resultsPath` — path where results should be written

**Implementation details:**
1. Stats both `statementPath` and `resultsPath`. If `resultsPath` exists and its modification time is not before the statement's, returns nil (results are current).
2. Reads the statement file content.
3. Creates the results directory with `os.MkdirAll`.
4. Creates a temporary file with `os.CreateTemp`.
5. Calls `n.client.WriteQueryLogs(ctx, queryString, tmpFile)` to execute the query via RPC.
6. Closes the temporary file and atomically renames it to `resultsPath`.
7. Cleans up the temp file in a deferred function.

### `(*MonoNode) readLocalFileRange`

```go
func (n *MonoNode) readLocalFileRange(path string, dest []byte, off int64) (fuse.ReadResult, syscall.Errno)
```

**What it does:** Reads a range of bytes from a local file on disk. Used for reading doctor query results from the local overlay.

**Key parameters:**
- `path` — filesystem path to the local file
- `dest` — destination buffer
- `off` — offset to read from

**Implementation details:**
1. Opens the file with `os.Open`.
2. Uses `file.ReadAt(dest, off)` for positioned read.
3. Handles `io.EOF` as a normal condition (truncated read).
4. Returns nil if `nread <= 0`.
5. Records the operation and bytes read via the client.

---

## op_readdir.go

### `(*MonoNode) Readdir`

```go
func (n *MonoNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno)
```

**What it does:** Implements `fs.NodeReaddirer` for the go-fuse library. Reads directory entries (the FUSE equivalent of `getdents(2)` / `readdir(3)`).

**How it's called:** By go-fuse on `readdir` kernel requests (e.g., `ls`, `find`, `go mod verify`).

**Key parameters:**
- `ctx` — context for cancellation

**Return values:**
- `fs.DirStream` — a directory stream (backed by `fs.NewListDirStream`)
- `syscall.Errno` — 0 on success

**Implementation details:**
1. Panic recovery, operation recording, debug logging.
2. **Synthetic workspace control directory** (`.monofs`): Returns synthetic entries directly via `syntheticWorkspaceControlEntries()`.
3. **Hidden workspace paths**: Returns `ENOENT`.
4. **User root directory** (parent path or subdirectory of one, `IsUserRootDir`): Creates an `OverlayManager`, merges readdir with overlay, filters workspace entries, and returns.
5. **Cache check** (`n.cache.GetDir(backendPath)`): On cache hit, converts cached entries to `fuse.DirEntry` slice. Adds `FS_ERROR.txt` to root if there's a catastrophic error. Merges with overlay. Filters workspace entries.
6. **Catastrophic error check for root**: Checks if root node has a catastrophic error (to include `FS_ERROR.txt` later).
7. **Backend query** (`n.client.ReadDir(backendPath)`):
   - On first failure: invalidates dir cache, waits with `retryDelay(0)`, retries with a fresh context using `context.Background()` with `readdirTimeout` (the FUSE context may have been cancelled by FUSE_INTERRUPT due to timeout, but callers like `go mod verify` need the complete listing).
   - On second failure (root directory): stores error in root node's `catastrophicError`, returns a single-entry directory with just `FS_ERROR.txt`.
   - On second failure (non-root): returns the converted error.
8. On backend success: clears backend error and catastrophic error, converts entries to FUSE entries and cache entries.
9. Adds `FS_ERROR.txt` to root listing if there's a catastrophic error or backend error.
10. Caches the directory entries.
11. Merges with overlay via `OverlayManager.MergeReadDir(dirEntries, n.path)`.
12. **Doctor virtual file** (`doctor/v1/query/<id>` directory with 4 path components): Appends a synthetic `results.json` entry.
13. Without overlay, sorts entries deterministically by name.
14. Filters workspace entries to hide system-namespace and nested-git paths.
15. Logs final entries for debugging.

---

## op_rename.go

### `(*MonoNode) Rename`

```go
func (n *MonoNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno
```

**What it does:** Implements `fs.NodeRenamer` for the go-fuse library. Renames or moves a file/directory from one location to another. This is the FUSE equivalent of `rename(2)`.

**How it's called:** By go-fuse on `rename` kernel requests.

**Key parameters:**
- `name` — the basename of the source entry in the current directory
- `newParent` — the destination parent inode (can be cast to `*MonoNode` for the path)
- `newName` — the new basename
- `flags` — rename flags (unused)

**Return value:** 0 on success, `EROFS`, `EPERM`, or `EIO` on failure.

**Implementation details:**
1. Returns `EROFS` if `sessionMgr` is nil or parent is a workspace read-only path.
2. Returns `EROFS` for root-level renames (`n.path == ""`).
3. Extracts `newParentPath` from the `*MonoNode` cast; returns `EROFS` if the new parent is the root.
4. Ensures a write session.
5. Constructs `oldPath` and `newPath`. Checks both against `isWorkspaceReadOnlyPath` and `shouldHideWorkspacePath(newPath)`.
6. Calls `n.ensureLocalCopyFor(ctx, oldPath)` to ensure the source file is materialized locally.
7. Resolves local paths for source and destination.
8. Creates destination parent directories with `os.MkdirAll`.
9. Performs `os.Rename(oldLocalPath, newLocalPath)`.
10. **Overlay DB re-keying**: Calls `n.sessionMgr.RenameChildren(oldPath, newPath)` to update all overlay entries under the old path prefix so they point to the new path. This keeps the overlay DB consistent with the physical file layout.
11. **Inode path update**: Finds the child inode via `n.GetChild(name)`. If it's a `MonoNode`, updates its path to `newPath` under lock. Recursively calls `updateDescendantPaths` to fix all descendant inode paths.
12. Tracks as `ChangeDelete` (old path) + `ChangeCreate` (new path) via `TrackChange`.
13. Does NOT call `invalidateEntry` / `NotifyEntry` — the kernel's RENAME opcode already atomically updates its dentry cache for both names. Explicit invalidation from inside the handler would cause a deadlock (the kernel holds a dentry lock on `newName`).

### `updateDescendantPaths`

```go
func updateDescendantPaths(inode *fs.Inode, oldPrefix, newPrefix string)
```

**What it does:** Recursively walks the go-fuse inode tree below `inode` and rewrites every `MonoNode.path` that starts with `oldPrefix` to start with `newPrefix`. Called during `Rename` so that child inodes have consistent MonoNode paths after the kernel moves them in the inode tree.

**Key parameters:**
- `inode` — the root of the subtree to update
- `oldPrefix` — the old path prefix to replace
- `newPrefix` — the replacement path prefix

**Implementation details:**
1. Iterates over `inode.Children()`.
2. For each child, casts `Operations()` to `*MonoNode`.
3. If the path matches `oldPrefix` or starts with `oldPrefix/`, replaces the prefix with `newPrefix`.
4. Recursively calls itself for the child inode.

---

## op_rmdir.go

### `(*MonoNode) Rmdir`

```go
func (n *MonoNode) Rmdir(ctx context.Context, name string) syscall.Errno
```

**What it does:** Implements `fs.NodeRmdirer` for the go-fuse library. Removes a directory. This is the FUSE equivalent of `rmdir(2)`.

**How it's called:** By go-fuse on `rmdir` kernel requests.

**Key parameters:**
- `name` — the name of the directory to remove

**Return value:** 0 on success, `EROFS` or `EIO` on failure.

**Implementation details:**
1. Returns `EROFS` if `sessionMgr` is nil or parent is a workspace read-only path.
2. Checks for an active session with `n.sessionMgr.HasActiveSession()` (fast path); starts one if needed.
3. **Root-level removal** (`n.path == ""`): Only allows removal of user-created root directories (checked via `IsUserRootDir`). Returns `EROFS` for repository directories. Calls `n.sessionMgr.RemoveUserRootDir(name)`.
4. **Subdirectory removal** (`n.path != ""`): Checks `isWorkspaceReadOnlyPath(targetPath)`. If a local copy exists (`HasLocalOverride`), removes it with `os.RemoveAll`. Tracks the deletion via `n.sessionMgr.TrackChange(ChangeRmdir, ...)`.

---

## op_setattr.go

### `(*MonoNode) Setattr`

```go
func (n *MonoNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno
```

**What it does:** Implements `fs.NodeSetattrer` for the go-fuse library. Handles file attribute changes: truncation (`ftruncate`, `truncate`) and mode changes (`chmod`).

**How it's called:** By go-fuse on `setattr` kernel requests from `truncate(2)`, `ftruncate(2)`, `chmod(2)`, `utimensat(2)`.

**Key parameters:**
- `fh` — optional file handle (unused here)
- `in` — `fuse.SetAttrIn` indicating which attributes are being set (via the `Valid` bitmask)
- `out` — output `fuse.AttrOut` filled with the new attributes

**Return value:** 0 on success, `EROFS` or `EIO` on failure.

**Implementation details:**
1. Returns `EROFS` if no session manager or read-only workspace path.
2. Ensures a write session.
3. Determines if a local copy is needed (`FATTR_SIZE` or `FATTR_MODE` is set).
4. **Truncate-to-zero fast path**: If `FATTR_SIZE` is set and size is 0, creates an empty local file directly via `os.WriteFile` without fetching the backend content. This is common with `O_TRUNC` opens.
5. **Non-zero truncate or mode change**: Calls `n.ensureLocalCopy(ctx)` to materialize the backend content first.
6. **Truncate handling** (`FATTR_SIZE`): For non-zero sizes, calls `os.Truncate(localPath, int64(in.Size))`. Updates `n.size` and `n.isLocalWrite` under lock.
7. **Mode change** (`FATTR_MODE`): Calls `os.Chmod(localPath, os.FileMode(in.Mode))`. Updates `n.mode` under lock.
8. **Change tracking**: If anything was modified, stamps the node and calls `n.sessionMgr.TrackChangeWithMeta(ChangeModify, n.path, "", sz)` with the known size to avoid an `os.Lstat` syscall.
9. **Attr population**: Fills `out` from the node's local `mode` and `size` fields, sets `hashPathForNode(n.path)` as inode, sets current timestamp for mtime/atime/ctime, sets owner via `setAttrOwner`, nlink=1, timeout via `overlayEntryTimeout()`.

---

## op_statfs.go

### `(*MonoNode) Statfs`

```go
func (n *MonoNode) Statfs(ctx context.Context, out *fuse.StatfsOut) (errno syscall.Errno)
```

**What it does:** Implements `fs.NodeStatfser` for the go-fuse library. Returns filesystem statistics (the FUSE equivalent of `statfs(2)`/`statvfs(2)`). Used by tools like `df` to report disk usage.

**How it's called:** By go-fuse on `statfs` kernel requests.

**Key parameters:**
- `out` — output `fuse.StatfsOut` filled with filesystem stats

**Return value:** `fs.OK` (0) on success, `EIO` if no client, or a converted error on failure.

**Implementation details:**
1. Returns `EIO` if `n.client` is nil.
2. Calls `n.client.StatFS(ctx)` to get a filesystem snapshot from the backend.
3. Copies all fields from the snapshot to `out`: `Blocks`, `Bfree`, `Bavail`, `Files`, `Ffree`, `Bsize`, `NameLen`, `Frsize`.

---

## op_symlink.go

### `(*MonoNode) Symlink`

```go
func (n *MonoNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno)
```

**What it does:** Implements `fs.NodeSymlinker` for the go-fuse library. Creates a symbolic link. This is the FUSE equivalent of `symlink(2)`.

**How it's called:** By go-fuse on `symlink` kernel requests.

**Key parameters:**
- `target` — the symlink target (content of the symlink)
- `name` — the name of the new symlink
- `out` — output `fuse.EntryOut` filled with the symlink's attributes

**Return values:**
- `*fs.Inode` — the inode for the new symlink
- `syscall.Errno` — 0 on success, `EROFS`, `EPERM`, or `EIO` on failure

**Implementation details:**
1. Returns `EROFS` if no session manager or read-only workspace path.
2. Returns `EROFS` for root-level symlinks (`n.path == ""`).
3. Ensures write session.
4. Checks `isWorkspaceReadOnlyPath(newPath)` and `shouldHideWorkspacePath(newPath)`.
5. Calls `n.sessionMgr.CreateSymlink(newPath, target)` to store the symlink in the session overlay.
6. Creates a child node with `n.newChild(name, false, 0777|syscall.S_IFLNK, len(target))`, sets `symlinkTarget = target`, stamps it.
7. Populates `out` with `S_IFLNK | 0777` mode, target length as size, `hashPathForNode(newPath)` as inode, nlink=1, and attr/entry timeouts.

### `(*MonoNode) Readlink`

```go
func (n *MonoNode) Readlink(ctx context.Context) ([]byte, syscall.Errno)
```

**What it does:** Implements `fs.NodeReadlinker` for the go-fuse library. Reads the target of a symbolic link. This is the FUSE equivalent of `readlink(2)`.

**How it's called:** By go-fuse on `readlink` kernel requests.

**Return values:**
- `[]byte` — the symlink target content
- `syscall.Errno` — 0 on success, `EINVAL` if not a symlink

**Implementation details:** Checks for the target in three places, in order:
1. **Node cache** (`n.symlinkTarget`): Returns if non-empty.
2. **Session manager** (`n.sessionMgr.GetSymlinkTarget(n.path)`): Returns if found.
3. **Local filesystem overlay** (`os.Readlink(localPath)`): Reads the symlink from the on-disk overlay.
4. If none found, returns `EINVAL`.

---

## op_unlink.go

### `(*MonoNode) Unlink`

```go
func (n *MonoNode) Unlink(ctx context.Context, name string) syscall.Errno
```

**What it does:** Implements `fs.NodeUnlinker` for the go-fuse library. Deletes (unlinks) a file. This is the FUSE equivalent of `unlink(2)`.

**How it's called:** By go-fuse on `unlink` kernel requests.

**Key parameters:**
- `name` — the name of the file to delete

**Return value:** 0 on success, `EROFS` or `EIO` on failure.

**Implementation details:**
1. Returns `EROFS` if no session manager, read-only workspace path, or root-level deletion.
2. Checks for active session with `HasActiveSession()` (fast path); starts one if needed.
3. Checks `isWorkspaceReadOnlyPath(targetPath)`.
4. If a local copy exists (`HasLocalOverride`), removes it with `os.Remove`.
5. Tracks deletion via `n.sessionMgr.TrackChange(ChangeDelete, targetPath, "")`.

---

## op_write.go

### `(*MonoNode) Write`

```go
func (n *MonoNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno)
```

**What it does:** Implements `fs.NodeWriter` for the go-fuse library. Writes data to a file at a given offset. This is the FUSE equivalent of `write(2)`.

**How it's called:** By go-fuse on `write` kernel requests. The primary write path goes through `monofsFileHandle.Write`. This fallback path is hit when go-fuse dispatches a Write without a valid file handle (e.g., re-opened inodes).

**Key parameters:**
- `fh` — file handle; if `*monofsFileHandle`, delegates to it directly
- `data` — the data to write
- `off` — the byte offset to write at

**Return values:**
- `uint32` — number of bytes written
- `syscall.Errno` — 0 on success, `EROFS` or `EIO` on failure

**Implementation details:**
1. **File handle delegation**: If `fh` is a `*monofsFileHandle`, delegates to `gfh.Write(ctx, data, off)`. This is the fast, common path.
2. Returns `EROFS` if no session manager or read-only workspace path.
3. Calls `ensureLocalCopy(ctx)` (short-circuits if already on disk).
4. **Cached file handle**: Checks `n.localHandle` under lock. If nil, opens the local path with `O_RDWR` and stores it. Reusing the handle avoids `open+close` per write chunk (~25,000 syscall pairs for a 100MB file at 4KB writes).
5. Calls `f.WriteAt(data, off)` for positioned write.
6. Updates `n.size` under lock if the write extended the file. Sets `isLocalWrite = true`.
7. Does NOT call `TrackChange` per-write. The dirty flag (`isLocalWrite`) is set; actual DB write is deferred to `Flush`/`Release` via the file handle path. This avoids a NutsDB transaction + `os.Lstat` + JSON marshaling on every 4KB write chunk.

---

## workspace.go

### `NewWorkspaceManifest`

```go
func NewWorkspaceManifest(provider client.WorkspaceMetadataProvider) *WorkspaceManifest
```

**What it does:** Creates a new `WorkspaceManifest` with the given backend provider and a default TTL of 30 seconds.

**Key parameters:**
- `provider` — a `client.WorkspaceMetadataProvider` that can list workspace repositories and their metadata

**Implementation details:** Initializes with `workspaceManifestTTL` (30 seconds). The manifest caches repository discovery results so the FUSE layer can project the synthetic source-root view without repeatedly querying the cluster backend.

### `(*WorkspaceManifest) List`

```go
func (m *WorkspaceManifest) List(ctx context.Context) ([]WorkspaceManifestEntry, error)
```

**What it does:** Returns the latest discovered repositories along with their inclusion status in the projected workspace. Caches results for `m.ttl` duration.

**How it's called:** By `ResolvePath`, `JSONContent`, and other manifest methods.

**Key parameters:**
- `ctx` — context for cancellation

**Return values:**
- `[]WorkspaceManifestEntry` — each entry contains a `client.WorkspaceRepository`, an `Included` boolean, and an `ExclusionReason`

**Implementation details:**
1. If the provider is nil, returns nil.
2. If cached entries exist and are within the TTL, returns a copy under a read lock.
3. Otherwise, calls `m.provider.ListWorkspaceRepositories(ctx)`.
4. For each repository, determines inclusion via `workspaceExcludedPath(repo.DisplayPath)`.
5. Sorts entries by `DisplayPath`, then `StorageID`.
6. Caches under a write lock and returns a copy.

### `(*WorkspaceManifest) ResolvePath`

```go
func (m *WorkspaceManifest) ResolvePath(ctx context.Context, path string) (*WorkspacePathResolution, error)
```

**What it does:** Resolves a path against the current manifest and reports whether that path is part of the projected source-root view.

**Key parameters:**
- `path` — the logical FUSE path to resolve

**Return values:**
- `*WorkspacePathResolution` — contains the path, matching repository (if any), inclusion status, and exclusion reason
- `error` — non-nil if manifest listing failed

**Implementation details:**
1. Checks path for exclusion via `workspaceExcludedPath`.
2. Lists repositories from the manifest.
3. Performs longest-prefix matching: finds the repository whose `DisplayPath` is a prefix of the given path.
4. If the matching repository is excluded, overrides the resolution's `Included` and `ExclusionReason`.

### `(*WorkspaceManifest) ShouldHidePath`

```go
func (m *WorkspaceManifest) ShouldHidePath(path string) bool
```

**What it does:** Returns true if the given path should be hidden from the FUSE namespace (system-namespace paths, nested `.git` directories).

**How it's called:** By `MonoNode.shouldHideWorkspacePath`.

### `(*WorkspaceManifest) ShouldReserveRoot`

```go
func (m *WorkspaceManifest) ShouldReserveRoot(name string) bool
```

**What it does:** Returns true if the given name is reserved at the filesystem root (e.g., `.monofs`, `guardian`, `doctor`). User mkdir at root is denied for these names.

**How it's called:** By `MonoNode.shouldReserveWorkspaceRoot` during `Mkdir`.

### `(*WorkspaceManifest) ShouldHideChild`

```go
func (m *WorkspaceManifest) ShouldHideChild(parentPath, name string) bool
```

**What it does:** Returns true if a child entry should be hidden in a directory listing. The root `.gitignore` is never hidden. All other paths are checked via `workspaceHiddenPath`.

### `(*WorkspaceManifest) FilterDirEntries`

```go
func (m *WorkspaceManifest) FilterDirEntries(path string, entries []fuse.DirEntry) []fuse.DirEntry
```

**What it does:** Filters a list of `fuse.DirEntry` by removing hidden children and adding synthetic entries where appropriate.

**Key parameters:**
- `path` — the parent directory path
- `entries` — the unfiltered list of directory entries

**Implementation details:**
1. Filters out entries where `ShouldHideChild(path, entry.Name)` is true.
2. Deduplicates entries by name.
3. **At root** (`path == ""`): Injects a synthetic `.gitignore` entry (if not already present) and a synthetic `.monofs` directory entry.
4. Sorts entries by name for deterministic output.

### `(*WorkspaceManifest) GitignoreContent`

```go
func (m *WorkspaceManifest) GitignoreContent() []byte
```

**What it does:** Returns the content of the synthetic `.gitignore` file at the monorepo root. Returns nil if the manifest is nil. The content includes patterns for `.git`, `dependency/`, `doctor/`, `guardian/`, `guardian-system/`, `.monofs/`, and `**/.git`.

### `(*WorkspaceManifest) Invalidate`

```go
func (m *WorkspaceManifest) Invalidate()
```

**What it does:** Clears the cached repository list, forcing the next `List()` call to re-fetch from the backend.

**How it's called:** After backend errors that may indicate stale workspace state.

### `(*WorkspaceManifest) JSONContent`

```go
func (m *WorkspaceManifest) JSONContent(ctx context.Context) ([]byte, error)
```

**What it does:** Generates a JSON representation of the workspace manifest (content for `.monofs/workspace.json`). Returns a minimal empty document if the manifest is nil.

**Return values:** JSON-encoded document with `generated_at` timestamp and a `repositories` array. Each repository entry includes `storage_id`, `display_path`, `source`, `ref`, `commit_hash`, `commit_time`, `commit_message`, `included`, and `exclusion_reason`.

### `joinWorkspacePath`

```go
func joinWorkspacePath(parentPath, name string) string
```

**What it does:** Joins a parent path and a child name into a workspace path. Handles empty parent paths by returning just the name. Strips leading/trailing slashes from `name`.

**Implementation details:** Simple path concatenation with a guard for empty parent.

### `isWorkspaceSystemPath`

```go
func isWorkspaceSystemPath(path string) bool
```

**What it does:** Returns true if the path is `.monofs/system` or a subpath thereof.

### `isWorkspaceReadOnlyPath`

```go
func isWorkspaceReadOnlyPath(path string) bool
```

**What it does:** Returns true if the path is read-only in the workspace. This includes `.monofs`, `.monofs/workspace.json`, and any path under `.monofs/system`.

### `backendPathForSystemView`

```go
func backendPathForSystemView(path string) (string, bool)
```

**What it does:** Returns the backend path corresponding to a synthetic `.monofs/system/*` path by stripping the `.monofs/system/` prefix. Returns `(string, true)` if the path is under the system view.

### `workspaceHiddenPath`

```go
func workspaceHiddenPath(path string) (WorkspaceExclusionReason, bool)
```

**What it does:** Checks a path against the `hiddenWorkspaceRoots` map. Returns the exclusion reason and true if hidden.

### `workspaceExcludedPath`

```go
func workspaceExcludedPath(path string) (WorkspaceExclusionReason, bool)
```

**What it does:** Checks a path against the `excludedWorkspaceRoots` map (which includes `dependency` in addition to the hidden roots). Returns the exclusion reason and true if excluded.

### `workspacePathExclusion`

```go
func workspacePathExclusion(path string, roots map[string]WorkspaceExclusionReason) (WorkspaceExclusionReason, bool)
```

**What it does:** Core exclusion logic against a given `roots` map. Checks:
- Empty path -> not excluded.
- Path equals `.git` -> not excluded (allow `.git` files).
- Path starts with `.monofs` -> not excluded (system view paths are not hidden).
- First component matches a key in `roots` -> excluded.
- Any component equals `.git` -> excluded (nested Git directories).

---

## workspace_git.go

### `NewWorkspaceGitProjection`

```go
func NewWorkspaceGitProjection(mountPoint, stateDir string, sessionMgr *SessionManager, logger *slog.Logger, uid, gid uint32) (*WorkspaceGitProjection, error)
```

**What it does:** Creates a new `WorkspaceGitProjection` that maintains a local bare Git repository mirroring the mounted virtual monorepo root. This enables Git-aware tooling to treat the FUSE mount as a worktree.

**Key parameters:**
- `mountPoint` — the FUSE mount point directory
- `stateDir` — directory for storing the bare Git repository
- `sessionMgr` — session manager for tracking user-created root directories
- `logger` — structured logger
- `uid`, `gid` — owner UID/GID for the Git repository files

**Implementation details:**
1. Validates that `mountPoint`, `stateDir` are non-empty.
2. Checks that `git` is in `PATH` via `exec.LookPath`.
3. Creates the state directory with `os.MkdirAll`.
4. Names the bare Git directory as `workspace-<sha256_hex_of_mountpoint>.git` using a hash of the mount point for uniqueness.

### `(*WorkspaceGitProjection) SetOwner`

```go
func (p *WorkspaceGitProjection) SetOwner(uid, gid uint32)
```

**What it does:** Updates the owner UID/GID for the workspace Git projection. Used when the FUSE server starts with different credentials than the default.

### `workspaceGitProjectionKey`

```go
func workspaceGitProjectionKey(mountPoint string) string
```

**What it does:** Generates a unique key for the workspace Git directory by computing the first 6 bytes of the SHA-256 hash of the mount point, encoded as hex. This ensures different mount points get different Git directories.

### `(*WorkspaceGitProjection) GitFileContent`

```go
func (p *WorkspaceGitProjection) GitFileContent() []byte
```

**What it does:** Returns the content for the synthetic `.git` file at the mount root. The content is `gitdir: <path_to_bare_repo>\n`, which tells Git to use a detached worktree setup (the mount point is the worktree, the bare repo is the actual Git directory).

### `(*WorkspaceGitProjection) Sync`

```go
func (p *WorkspaceGitProjection) Sync(ctx context.Context) error
```

**What it does:** Synchronizes the Git projection with the current filesystem state. Ensures the bare repo exists, updates the `.git/info/exclude` file, ensures ownership, removes excluded paths from the index, stages all changes, and creates a commit if there are changes.

**Implementation details:**
1. Locks via `p.mu` to prevent concurrent sync operations.
2. Calls `ensureRepo(ctx)` to create and configure the bare repository if it doesn't exist.
3. Calls `writeInfoExclude()` to update the exclude list (prevents excluded directories from being tracked).
4. Calls `ensureOwnership()` to fix file ownership.
5. Calls `removeExcludedPathsFromIndex(ctx)` to remove any previously-tracked excluded paths.
6. Runs `git add -A` to stage all changes.
7. Checks `git status --porcelain` for changes. If no changes and HEAD exists, returns nil.
8. Creates a commit with environment override (`MONOFS_WORKSPACE_GIT_SYNC=1`) to bypass the commit guard hook. Uses author `MonoFS Workspace <monofs-workspace@local>`. Handles `nothing to commit` gracefully.

### `(*WorkspaceGitProjection) ensureRepo`

```go
func (p *WorkspaceGitProjection) ensureRepo(ctx context.Context) error
```

**What it does:** Ensures the bare Git repository exists and is configured. Creates the bare repo directory and parent dirs if needed.

**Implementation details:**
1. Stats `p.gitDir/HEAD`. If it exists, just runs `configureRepo` (idempotent).
2. Otherwise creates parent directories, initializes a bare repo with `git init --bare`, ensures ownership, and configures the repo.

### `(*WorkspaceGitProjection) ensureOwnership`

```go
func (p *WorkspaceGitProjection) ensureOwnership() error
```

**What it does:** Ensures the Git directory and its parent are owned by the configured UID/GID. Calls `ensurePathOwner` for both paths.

### `(*WorkspaceGitProjection) configureRepo`

```go
func (p *WorkspaceGitProjection) configureRepo(ctx context.Context) error
```

**What it does:** Configures the bare Git repository for detached worktree use.

**Implementation details:**
1. Sets `core.bare` to `false` (even though it's in a bare directory, Git needs to know this is not a bare working copy).
2. Sets `core.worktree` to the FUSE mount point.
3. Sets the symbolic-ref `HEAD` to `refs/heads/main`.
4. Installs the commit guard hook via `installCommitGuardHook`.

### `(*WorkspaceGitProjection) installCommitGuardHook`

```go
func (p *WorkspaceGitProjection) installCommitGuardHook() error
```

**What it does:** Installs a `pre-commit` hook in the bare repo that prevents users from accidentally committing through the synthetic Git directory. The hook allows commits only when `MONOFS_WORKSPACE_GIT_SYNC=1` is set (used by the `Sync` method). Otherwise it prints an error message directing users to use `monofs-session commit`.

### `(*WorkspaceGitProjection) writeInfoExclude`

```go
func (p *WorkspaceGitProjection) writeInfoExclude() error
```

**What it does:** Writes the `.git/info/exclude` file to exclude system paths (`.monofs/`, `.git`, user root dirs, `FS_ERROR.txt`) from being tracked by Git.

**Implementation details:**
1. Collects excluded root paths from `excludedRootPaths()` plus `.monofs/`.
2. For each path, adds a `/pattern/` entry (directory exclusion).
3. For `FS_ERROR.txt`, adds `/FS_ERROR.txt` (file exclusion).
4. Deduplicates patterns, sorts, and writes to the exclude file.

### `(*WorkspaceGitProjection) excludedRootPaths`

```go
func (p *WorkspaceGitProjection) excludedRootPaths() []string
```

**What it does:** Returns a list of paths to exclude from Git tracking. Always includes `.git`, `.monofs`, `FS_ERROR.txt`, and any user-created root directories.

### `(*WorkspaceGitProjection) removeExcludedPathsFromIndex`

```go
func (p *WorkspaceGitProjection) removeExcludedPathsFromIndex(ctx context.Context) error
```

**What it does:** Runs `git rm -r --cached --ignore-unmatch -- <paths...>` to remove excluded paths from the Git index. This cleans up any previously-tracked files that are now in excluded directories.

### `deduplicateStrings`

```go
func deduplicateStrings(values []string) []string
```

**What it does:** Deduplicates a slice of strings, preserving order, skipping empty/whitespace-only values.

### `(*WorkspaceGitProjection) gitOutput`

```go
func (p *WorkspaceGitProjection) gitOutput(ctx context.Context, args ...string) ([]byte, error)
```

**What it does:** Runs `git` with `--git-dir=<p.gitDir>` and `--work-tree=<p.mountPoint>` and returns the combined output. Used for commands where output is needed (e.g., `status --porcelain`).

### `(*WorkspaceGitProjection) gitOutputWithEnv`

```go
func (p *WorkspaceGitProjection) gitOutputWithEnv(ctx context.Context, extraEnv map[string]string, args ...string) ([]byte, error)
```

**What it does:** Same as `gitOutput` but with additional environment variables. Used for the commit command which needs `MONOFS_WORKSPACE_GIT_SYNC=1`.

### `(*WorkspaceGitProjection) runGit`

```go
func (p *WorkspaceGitProjection) runGit(ctx context.Context, args ...string) error
```

**What it does:** Runs `git` with worktree settings, discarding output, returning only errors.

### `(*WorkspaceGitProjection) runGitWithEnv`

```go
func (p *WorkspaceGitProjection) runGitWithEnv(ctx context.Context, extraEnv map[string]string, args ...string) error
```

**What it does:** Same as `runGit` but with additional environment variables.

### `(*WorkspaceGitProjection) runGitBare`

```go
func (p *WorkspaceGitProjection) runGitBare(ctx context.Context, args ...string) error
```

**What it does:** Runs `git` with only `--git-dir=<p.gitDir>` (no worktree). Used for repo initialization and configuration commands that don't need a worktree (e.g., `init`, `config`, `symbolic-ref`).

### `formatGitEnv`

```go
func formatGitEnv(extraEnv map[string]string) []string
```

**What it does:** Converts a map of environment variables to a sorted slice of `KEY=VALUE` strings suitable for `exec.Cmd.Env`. Returns nil if the map is empty.

---

## writable.go

### `(*LocalFileHandle) Read`

```go
func (h *LocalFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno)
```

**What it does:** Implements `fs.FileReader` for local overlay files. Reads data from the local `*os.File` at the given offset.

**Implementation details:** Uses `h.file.ReadAt(dest, off)`. Handles `io.EOF` as a normal termination.

### `(*LocalFileHandle) Write`

```go
func (h *LocalFileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno)
```

**What it does:** Implements `fs.FileWriter` for local overlay files. Writes data to the local file at the given offset and updates the `WritableNode` size if the write extended the file.

**Implementation details:** Uses `h.file.WriteAt(data, off)`. Updates `h.node.size` under lock if the new end offset exceeds the current size.

### `(*LocalFileHandle) Flush`

```go
func (h *LocalFileHandle) Flush(ctx context.Context) syscall.Errno
```

**What it does:** Implements `fs.FileFlusher`. Calls `h.file.Sync()` to flush buffered writes to disk.

### `(*LocalFileHandle) Release`

```go
func (h *LocalFileHandle) Release(ctx context.Context) syscall.Errno
```

**What it does:** Implements `fs.FileReleaser`. Closes the underlying `*os.File` and sets the pointer to nil. Called when the kernel releases all references to the file handle.

### `(*LocalFileHandle) Getattr`

```go
func (h *LocalFileHandle) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno
```

**What it does:** Implements `fs.FileGetattrer`. Returns file attributes from the local `*os.File` via `Stat()`. Uses hardcoded UID/GID of 1000.

### `syncRWMutex.Lock / Unlock / RLock / RUnlock`

```go
func (m *syncRWMutex) Lock()    { m.mu.Lock() }
func (m *syncRWMutex) Unlock()  { m.mu.Unlock() }
func (m *syncRWMutex) RLock()   { m.mu.RLock() }
func (m *syncRWMutex) RUnlock() { m.mu.RUnlock() }
```

**What they do:** Thin wrapper methods around `sync.RWMutex` exposing lock/unlock semantics. Used by `WritableNode` for concurrent access protection.

### `(*WritableNode) Setattr`

```go
func (n *WritableNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno
```

**What it does:** Implements `fs.NodeSetattrer` for `WritableNode`. Handles truncation and mode changes on the local overlay copy.

**Implementation details:**
1. If a `*LocalFileHandle` is provided, delegates to `n.setattrLocal` for truncate.
2. Direct truncate path: calls `ensureLocalCopy`, then `os.Truncate`, updates `n.size` and `n.isLocalWrite`.
3. Direct chmod path: only if a local override exists, calls `os.Chmod` on the local path, updates `n.mode`.
4. Fills the attr output via `n.fillAttrOut(out)`.

### `(*WritableNode) setattrLocal`

```go
func (n *WritableNode) setattrLocal(_ context.Context, lh *LocalFileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno
```

**What it does:** Truncates the file through the `LocalFileHandle`'s open file descriptor. Updates `n.size`. Fills attr output via `fillAttrOut`.

### `(*WritableNode) fillAttrOut`

```go
func (n *WritableNode) fillAttrOut(out *fuse.AttrOut) syscall.Errno
```

**What it does:** Populates a `fuse.AttrOut` from the `WritableNode`'s local fields (`size`, `mode`, `isDir`). Uses hardcoded UID/GID of 1000. Sets `nlink=2` for directories, `nlink=1` for files. ORs in `S_IFDIR` or `S_IFREG` to the mode.

### `(*WritableNode) Create`

```go
func (n *WritableNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno)
```

**What it does:** Implements `fs.NodeCreater` for `WritableNode`. Creates a new file in the local overlay.

**Implementation details:**
1. Starts a write session.
2. Constructs `newPath` (parent path + name).
3. Gets local path via `GetLocalPath`.
4. Creates parent directories and then the file with `O_CREATE|O_RDWR|O_TRUNC`.
5. Tracks creation via `TrackChange(ChangeCreate, ...)`.
6. Creates a child `WritableNode` with `isLocalWrite = true`.
7. Uses `hashPath(newPath)` for stable inode number.
8. Wraps the file in a `LocalFileHandle`, returns `FOPEN_DIRECT_IO`.

### `(*WritableNode) Mkdir`

```go
func (n *WritableNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno)
```

**What it does:** Implements `fs.NodeMkdirer` for `WritableNode`. Creates a new directory in the local overlay.

**Implementation details:** Similar to `Create` but creates a directory with `os.MkdirAll`, tracks as `ChangeMkdir`, creates a child `WritableNode` with `isDir = true`, and sets `nlink=2`.

### `(*WritableNode) Unlink`

```go
func (n *WritableNode) Unlink(ctx context.Context, name string) syscall.Errno
```

**What it does:** Implements `fs.NodeUnlinker` for `WritableNode`. Deletes a file from the local overlay.

**Implementation details:** Removes local file with `os.Remove` if a local copy exists. Tracks as `ChangeDelete`.

### `(*WritableNode) Rmdir`

```go
func (n *WritableNode) Rmdir(ctx context.Context, name string) syscall.Errno
```

**What it does:** Implements `fs.NodeRmdirer` for `WritableNode`. Removes a directory from the local overlay.

**Implementation details:** Removes local directory with `os.RemoveAll` if a local copy exists. Tracks as `ChangeRmdir`.

### `(*WritableNode) Rename`

```go
func (n *WritableNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno
```

**What it does:** Implements `fs.NodeRenamer` for `WritableNode`. Moves/renames a file in the local overlay.

**Implementation details:**
1. Starts write session.
2. Extracts `newParentPath` from either `*WritableNode` or `*MonoNode` cast.
3. Calls `ensureLocalCopyFor(ctx, oldPath)` to materialize the source.
4. Creates destination parent dirs.
5. Performs `os.Rename` on local paths.
6. Tracks as `ChangeDelete` + `ChangeCreate`.

### `(*WritableNode) ensureLocalCopy`

```go
func (n *WritableNode) ensureLocalCopy(ctx context.Context) error
```

**What it does:** Convenience wrapper. Calls `ensureLocalCopyFor(ctx, n.path)`.

### `(*WritableNode) ensureLocalCopyFor`

```go
func (n *WritableNode) ensureLocalCopyFor(ctx context.Context, monofsPath string) error
```

**What it does:** Copies a file from the backend to the local overlay. Short-circuits if the local file already exists.

**Implementation details:**
1. Gets the local path via `GetLocalPath(monofsPath)`.
2. Returns immediately if the local file already exists (`os.Stat` succeeds).
3. Creates parent directories.
4. **User root dir paths**: Creates an empty file (backend doesn't know about these paths).
5. **Other paths**: Fetches content from backend via `n.client.Read(ctx, monofsPath, 0, 0)`. On failure, creates empty file. On success, writes content and computes SHA-256 hash stored in `origBlobHash`.

### `(*WritableNode) OpenForWrite`

```go
func (n *WritableNode) OpenForWrite(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno)
```

**What it does:** Opens a file for writing through the `WritableNode`. Ensures a local copy exists, opens with appropriate flags, tracks the modification, and returns a `LocalFileHandle`.

**Implementation details:**
1. Starts write session.
2. Calls `ensureLocalCopy` to materialize.
3. Builds open flags (`O_RDWR`, optionally `O_APPEND` and `O_TRUNC`).
4. Opens local path with `os.OpenFile`.
5. Tracks change via `TrackChange` — uses `ChangeCreate` if `origBlobHash` is empty (new file), otherwise `ChangeModify`.
6. Marks `isLocalWrite = true`.
7. Returns a `LocalFileHandle` with `FOPEN_DIRECT_IO`.

### `hashPath`

```go
func hashPath(path string) uint64
```

**What it does:** Creates a stable 64-bit inode number from a path string using FNV-1a hashing.

**How it's called:** By `WritableNode.Create`, `WritableNode.Mkdir`.

**Implementation details:** Uses `hash/fnv.New64a()`.
