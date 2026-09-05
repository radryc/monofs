# MonoFS Internal FUSE Layer Documentation — Part 1

> Auto-generated documentation for every exported and unexported function in the listed files.
> Test files (`_test.go`) are excluded.

---

## Table of Contents

1. [commit.go](#commitgo)
2. [file_handle.go](#file_handlego)
3. [node.go](#nodego)
4. [overlay.go](#overlaygo)
5. [overlaydb.go](#overlaydbgo)
6. [overlaydb_vcs.go](#overlaydb_vcsgo)
7. [ownership.go](#ownershipgo)
8. [session.go](#sessiongo)
9. [session_socket.go](#session_socketgo)
10. [session_vcs.go](#session_vcsgo)

---

## commit.go

Package fuse — FUSE filesystem layer for MonoFS.
Implements the `CommitManager` that handles pushing local changes to the backend.

### Types

#### `CommitManager`

```go
type CommitManager struct {
    sessionMgr  *SessionManager
    client      commitClient
    workspace   *WorkspaceManifest
    principalID string
    logger      *slog.Logger
    mu          sync.Mutex
}
```

Manages the commit lifecycle: staging changes, building local virtual commits, and pushing them to the backend.

#### `commitChangeScope`

```go
type commitChangeScope int
```

Constants:
- `commitChangeWorkspace` — change belongs to a workspace repository.
- `commitChangeBlob` — change is a dependency/blob file.
- `commitChangeExcluded` — change is excluded from the virtual monorepo view.

#### `classifiedCommitChange`

```go
type classifiedCommitChange struct {
    Change
    Scope      commitChangeScope
    Repository *client.WorkspaceRepository
}
```

A `Change` tagged with its scope and optionally the repository it belongs to.

#### `commitRepositoryGroup`

```go
type commitRepositoryGroup struct {
    Key         string
    DisplayPath string
    Repository  *client.WorkspaceRepository
    Changes     []Change
}
```

A group of changes belonging to a single repository.

#### `repositoryChangeApplier` (interface)

```go
type repositoryChangeApplier interface {
    ApplyRepositoryChanges(ctx context.Context, repo client.WorkspaceRepository, changes []client.RepositoryChange) (*client.ApplyRepositoryChangesResult, error)
}
```

Apply a set of repository-level changes to the backend.

#### `workspaceBundlePublisher` (interface)

```go
type workspaceBundlePublisher interface {
    PublishWorkspaceBundle(ctx context.Context, bundle *workspacebundle.Bundle, opts client.WorkspacePublishOptions) (*client.WorkspacePublishResult, error)
}
```

Publish a workspace bundle to the backend.

#### `workspaceCommitBundlePusher` (interface)

```go
type workspaceCommitBundlePusher interface {
    PushWorkspaceCommitBundle(ctx context.Context, bundle *workspacebundle.SourceCommitBundle) (*client.WorkspaceSourcePushResult, error)
}
```

Push a source commit bundle to the backend.

#### `commitClient` (interface)

```go
type commitClient interface {
    client.MonoFSClient
    repositoryChangeApplier
    workspaceBundlePublisher
    workspaceCommitBundlePusher
}
```

Composite interface combining the MonoFS client with repository change, bundle publish, and commit push capabilities.

#### `CommitOptions`

```go
type CommitOptions struct {
    LogicalCommitMessage    string
    AuthorName              string
    AuthorEmail             string
    RequestedBranchStrategy string
}
```

Options for a logical virtual commit operation.

#### `CommitResult`

```go
type CommitResult struct {
    Success               bool
    Repositories          int
    FilesProcessed        int
    FilesUploaded         int
    FilesFailed           int
    Errors                map[string]string
    SessionID             string
    Message               string
    RefreshedRepositories []client.WorkspaceRepository
    LocalCommitID         string
}
```

The result of a commit operation. Includes counts, session info, errors keyed by path, and the new local commit ID.

#### `PushResult`

```go
type PushResult struct {
    Success       bool
    SessionID     string
    LogicalBranch string
    PushedCommits int
    Repositories  int
    JobID         string
    Message       string
}
```

The result of pushing pending local commits to the backend.

### Functions

#### `NewCommitManager`

```go
func NewCommitManager(sessionMgr *SessionManager, c commitClient, logger *slog.Logger) *CommitManager
```

**What it does**: Creates a new `CommitManager` with the given session manager and client. If `logger` is nil, falls back to `slog.Default()`.

**Called from**: Likely the FUSE mount setup code (not in these files).

---

#### `(cm *CommitManager) SetWorkspaceManifest`

```go
func (cm *CommitManager) SetWorkspaceManifest(manifest *WorkspaceManifest)
```

**What it does**: Enables virtual-monorepo commit classification by setting the workspace manifest. The manifest is used to resolve which repository a change belongs to.

**Called from**: The FUSE mount / root node setup when virtual monorepo mode is enabled.

---

#### `(cm *CommitManager) SetPrincipalID`

```go
func (cm *CommitManager) SetPrincipalID(principalID string)
```

**What it does**: Records the mounted client identity used for local commits. The principal ID is stored trimmed of whitespace under the mutex lock.

**Called from**: Session socket handler when `SetPrincipalID` is called.

---

#### `(cm *CommitManager) CommitChanges`

```go
func (cm *CommitManager) CommitChanges(ctx context.Context, opts CommitOptions) (*CommitResult, error)
```

**What it does**: Creates a new local virtual commit from the staged index. Acquires the mutex, validates an active session exists, lists staged entries, builds a local virtual commit, persists it to the overlay DB, advances the overlay baseline (reconciling committed entries with the current disk state), and clears staged entries.

**Key parameters**: `opts` provides author name/email, commit message, and branch strategy.

**Returns**: `CommitResult` with local commit ID, repository count, and files processed; or error if no session, no staged entries, or persistence fails.

**Called from**: `SessionSocketHandler.handleCommit` (indirectly via the socket protocol).

---

#### `(cm *CommitManager) PushPendingLocalCommits`

```go
func (cm *CommitManager) PushPendingLocalCommits(ctx context.Context) (*PushResult, error)
```

**What it does**: Pushes all un-pushed local virtual commits on the current logical branch to the backend. Builds a `SourceCommitBundle`, calls `PushWorkspaceCommitBundle` on the client, marks each pushed commit as pushed with its job ID and timestamp, records branch mappings, and returns a summary.

**Returns**: `PushResult` with job ID, pushed count, repository count, and formatted message; or error if no session, no pending commits, or push fails.

**Called from**: `SessionSocketHandler.handlePushSource`.

---

#### `(cm *CommitManager) buildLocalVirtualCommit`

```go
func (cm *CommitManager) buildLocalVirtualCommit(ctx context.Context, stagedEntries []StagedIndexEntry, opts CommitOptions) (LocalVirtualCommit, int, error)
```

**What it does**: Constructs a `LocalVirtualCommit` from the given staged entries. Groups entries by repository using workspace metadata, determines the current logical branch, assigns a parent commit ID from the latest sibling commit, creates per-repository operation lists sorted by path and kind, and sorts repository commits by `DisplayPath`.

**Returns**: The built commit, the count of processed operations, and any error.

**Called from**: `CommitChanges`.

---

#### `(cm *CommitManager) pendingLocalCommitsForBranch`

```go
func (cm *CommitManager) pendingLocalCommitsForBranch(logicalBranch string) ([]LocalVirtualCommit, error)
```

**What it does**: Lists all local virtual commits, filters to those that are not yet pushed and match the given `logicalBranch` (trimmed). Sorts them chronologically.

**Called from**: `PushPendingLocalCommits`.

---

#### `(cm *CommitManager) workspacePushWorkspaceID`

```go
func (cm *CommitManager) workspacePushWorkspaceID(session *WriteSession) string
```

**What it does**: Resolves the workspace ID for push operations. Prefers the principal ID if set, falls back to the session ID, and lastly to a timestamp-based fallback.

**Called from**: `buildSourceCommitBundle`.

---

#### `(cm *CommitManager) buildSourceCommitBundle`

```go
func (cm *CommitManager) buildSourceCommitBundle(ctx context.Context, workspaceID, logicalBranch string, commits []LocalVirtualCommit) (*workspacebundle.SourceCommitBundle, error)
```

**What it does**: Builds a `SourceCommitBundle` from a list of local commits. Fetches workspace repository metadata, converts each local commit to a source commit (preserving IDs, messages, timestamps), translates each local repository to a source repository (filling in missing metadata from the workspace manifest), and validates the bundle.

**Called from**: `PushPendingLocalCommits`.

---

#### `sourceCommitRepositoryFromLocal`

```go
func sourceCommitRepositoryFromLocal(repo LocalCommitRepository, metadataByDisplayPath, metadataByStorage map[string]client.WorkspaceRepository) (workspacebundle.SourceCommitRepository, error)
```

**What it does**: Converts a single `LocalCommitRepository` to a `workspacebundle.SourceCommitRepository`. Resolves missing fields (StorageID, DisplayPath, RepoURL, Branch, BaseCommit) from the workspace metadata maps. Generates a StorageID from the display path if still empty. Copies operations with a defensive append of content bytes.

**Called from**: `buildSourceCommitBundle`.

---

#### `(cm *CommitManager) recordSourcePushBranchMappings`

```go
func (cm *CommitManager) recordSourcePushBranchMappings(logicalBranch string, repos []*pb.WorkspaceSyncRepositoryResult) error
```

**What it does**: After a successful push, creates or updates `SessionBranchMapping` entries for each pushed repository. Records the principal ID, logical branch, storage ID, display path, original branch, actual (target) branch, and last pushed commit. Returns early if logical branch or principal ID is empty.

**Called from**: `PushPendingLocalCommits`.

---

#### `formatSourcePushSummary`

```go
func formatSourcePushSummary(commitCount, repoCount int, logicalBranch string) string
```

**What it does**: Formats a human-readable push summary string. If `logicalBranch` is empty, mentions "tracked upstream branches"; otherwise includes the branch name.

**Called from**: `PushPendingLocalCommits`.

---

#### `(cm *CommitManager) groupStagedEntriesByRepository`

```go
func (cm *CommitManager) groupStagedEntriesByRepository(ctx context.Context, stagedEntries []StagedIndexEntry) ([]stagedRepositoryGroup, error)
```

**What it does**: Groups staged index entries by their affiliated repository. Fetches workspace metadata, resolves each entry to a repository via `resolveStagedRepository`, and groups them under a key (StorageID preferred, falling back to DisplayPath). Results are sorted by `DisplayPath`.

**Called from**: `buildLocalVirtualCommit`.

---

#### `(cm *CommitManager) workspaceRepositoryMetadata`

```go
func (cm *CommitManager) workspaceRepositoryMetadata(ctx context.Context) (map[string]client.WorkspaceRepository, error)
```

**What it does**: Returns a map of `DisplayPath` → `WorkspaceRepository` for all included entries in the workspace manifest. Returns nil if no workspace manifest is set.

**Called from**: `buildSourceCommitBundle`, `groupStagedEntriesByRepository`.

---

#### `(cm *CommitManager) resolveStagedRepository`

```go
func (cm *CommitManager) resolveStagedRepository(entry StagedIndexEntry, repoMetadata map[string]client.WorkspaceRepository) (client.WorkspaceRepository, error)
```

**What it does**: Resolves which repository a staged entry belongs to. If `entry.RepositoryPath` is set, uses it directly. Otherwise infers the display path from the first 3 path components. First checks the workspace metadata map, falling back to generating a `StorageID` from the display path.

**Called from**: `groupStagedEntriesByRepository`.

---

#### `localCommitOperationFromStagedEntry`

```go
func localCommitOperationFromStagedEntry(displayPath string, entry StagedIndexEntry) (LocalCommitOperation, error)
```

**What it does**: Converts a staged index entry to a `LocalCommitOperation`. Computes the repository-relative path, then maps the change type to the appropriate operation kind: `Create`/`Modify` → `OperationUpsert`, `Delete` → `OperationDelete`, `Mkdir` → `OperationMkdir` (defaulting mode to 0o755), `Rmdir` → `OperationRmdir`, `Symlink` → `OperationSymlink`. Returns an error for unsupported change types.

**Called from**: `buildLocalVirtualCommit`.

---

#### `newLocalCommitID`

```go
func newLocalCommitID() string
```

**What it does**: Generates a new local commit ID by generating a session ID (random hex), truncating to 12 characters, and prefixing with "local-".

**Called from**: `buildLocalVirtualCommit`.

---

#### `defaultLocalCommitMessage`

```go
func defaultLocalCommitMessage(message string) string
```

**What it does**: Returns the trimmed message if non-empty, otherwise returns "local commit".

**Called from**: `buildLocalVirtualCommit`.

---

#### `latestLocalCommitID`

```go
func latestLocalCommitID(commits []LocalVirtualCommit, logicalBranch string) string
```

**What it does**: Finds the most recent (latest by creation time) local commit ID on a given logical branch. Returns empty string if no matching commits exist. Uses a defensive copy and reverse iteration over sorted commits.

**Called from**: `buildLocalVirtualCommit`.

---

#### `(cm *CommitManager) advanceOverlayBaseline`

```go
func (cm *CommitManager) advanceOverlayBaseline(stagedEntries []StagedIndexEntry) error
```

**What it does**: Iterates over all staged entries and calls `reconcileCommittedEntry` on each to update the overlay DB to match the post-commit disk state.

**Called from**: `CommitChanges`.

---

#### `(cm *CommitManager) reconcileCommittedEntry`

```go
func (cm *CommitManager) reconcileCommittedEntry(entry StagedIndexEntry) error
```

**What it does**: Updates a single overlay entry to reflect the committed state. For delete/rmdir: marks as deleted in the overlay DB if the file is gone or the session already considers it deleted; otherwise reclassifies from disk. For create/modify/mkdir: if marked as deleted in the session, re-marks with the appropriate delete type; otherwise compares the staged content with the current disk state — if they match, writes the file as `ChangeBaseline` (baseline), otherwise reclassifies based on the current file type (symlink, dir, or regular file).

**Important**: This is called after a commit is persisted but before staged entries are cleared.

**Called from**: `advanceOverlayBaseline`.

---

#### `stagedEntryMatchesCurrent`

```go
func stagedEntryMatchesCurrent(entry StagedIndexEntry, localPath string) (bool, FileEntryType, error)
```

**What it does**: Compares a staged index entry with the current state on disk. For `Create`/`Modify`: reads and compares content and mode bytes. For `Mkdir`: checks the entry is a directory. For `Symlink`: reads the symlink target and compares. Returns the current file type regardless of match status.

**Called from**: `reconcileCommittedEntry`.

---

#### `fileEntryTypeFromInfo`

```go
func fileEntryTypeFromInfo(info os.FileInfo) FileEntryType
```

**What it does**: Maps `os.FileInfo` to a `FileEntryType`: symlink, directory, or regular file.

**Called from**: `reconcileCommittedEntry`, `putOverlayFileFromDisk`, `stagedEntryMatchesCurrent`.

---

#### `(cm *CommitManager) reclassifyPathFromDisk`

```go
func (cm *CommitManager) reclassifyPathFromDisk(monofsPath string, baselineWasDelete bool) error
```

**What it does**: Re-reads a path from disk and writes a fresh overlay DB entry. If the path no longer exists on disk, marks it as deleted with `ChangeBaseline`. Otherwise classifies by file type: dir → `ChangeMkdir`, symlink → `ChangeSymlink`, regular file → `ChangeModify` (or `ChangeCreate` if `baselineWasDelete` is true).

**Called from**: `reconcileCommittedEntry`.

---

#### `(cm *CommitManager) putOverlayFileFromDisk`

```go
func (cm *CommitManager) putOverlayFileFromDisk(monofsPath string, changeType ChangeType) error
```

**What it does**: Lstats the local file, constructs a `FileEntry` from its attributes (type, mode, size, mtime, change type), reads the symlink target if applicable, preserves the `OrigHash` from any existing DB entry, and writes the entry to the overlay DB.

**Called from**: `reconcileCommittedEntry`, `reclassifyPathFromDisk`.

---

#### `(cm *CommitManager) classifyCommitChanges`

```go
func (cm *CommitManager) classifyCommitChanges(ctx context.Context, changes []Change) ([]classifiedCommitChange, error)
```

**What it does**: Classifies each change as workspace, blob (dependency), or excluded. Dependency paths are classified as blob. Otherwise, resolves the path against the workspace manifest: if not included, it's excluded; if included (or no manifest), it's workspace.

**Called from**: (possibly referenced elsewhere for the old `Commit` codepath).

---

#### `(cm *CommitManager) groupChangesByRepository`

```go
func (cm *CommitManager) groupChangesByRepository(changes []classifiedCommitChange) []commitRepositoryGroup
```

**What it does**: Groups workspace-scoped changes by their repository. Uses StorageID or DisplayPath as the grouping key. For paths without an explicit repository, infers the display path from the first 3 path components. Results are sorted by `DisplayPath`.

**Called from**: (possibly referenced elsewhere for the old `Commit` codepath).

---

#### `formatCommitRepositorySummary`

```go
func formatCommitRepositorySummary(groups []commitRepositoryGroup) string
```

**What it does**: Formats a summary of repository group names, showing up to 3 names followed by "and N more".

---

#### `(cm *CommitManager) resolveCommitRepository`

```go
func (cm *CommitManager) resolveCommitRepository(group commitRepositoryGroup) (*client.WorkspaceRepository, error)
```

**What it does**: Resolves a `WorkspaceRepository` from a group. Returns a copy of the group's repository, generating a StorageID if missing. Returns an error if the group has no repository and no valid display path.

**Called from**: `buildWorkspaceBundle`.

---

#### `(cm *CommitManager) repositoryClientChanges`

```go
func (cm *CommitManager) repositoryClientChanges(repo client.WorkspaceRepository, changes []Change) ([]client.RepositoryChange, error)
```

**What it does**: Converts `Change` entries to `client.RepositoryChange` entries for a given repository. Computes repository-relative paths, loads file content for create/modify, skips mkdir/rmdir (unsupported), and returns an error for symlink commits.

**Called from**: (possibly referenced elsewhere).

---

#### `(cm *CommitManager) buildWorkspaceBundle`

```go
func (cm *CommitManager) buildWorkspaceBundle(workspaceID string, repoGroups []commitRepositoryGroup) (*workspacebundle.Bundle, int, error)
```

**What it does**: Builds a `workspacebundle.Bundle` from repository groups. Validates each repository has a source URL, branch, and base commit. Converts changes to workspace bundle operations, skipping repositories with no operations. Validates the final bundle.

**Returns**: The bundle, count of processed files, and any error.

**Called from**: (possibly referenced elsewhere for the old `Commit` codepath).

---

#### `(cm *CommitManager) workspaceBundleOperation`

```go
func (cm *CommitManager) workspaceBundleOperation(repo client.WorkspaceRepository, change Change) (workspacebundle.Operation, bool, error)
```

**What it does**: Converts a single `Change` into a `workspacebundle.Operation`. Handles all change types: `Create`/`Modify` → `OperationUpsert`, `Delete` → `OperationDelete`, `Mkdir` → `OperationMkdir`, `Rmdir` → `OperationRmdir`, `Symlink` → `OperationSymlink`.

**Returns**: The operation, a boolean indicating whether the change should be included, and any error.

**Called from**: `buildWorkspaceBundle`.

---

#### `commitLocalMode`

```go
func commitLocalMode(localPath string, fallback os.FileMode) uint32
```

**What it does**: Returns the permission bits of a local file, or the fallback mode if the path is empty or the file can't be stated.

**Called from**: `workspaceBundleOperation`, `snapshotStagedEntry` (session_socket.go).

---

#### `(cm *CommitManager) commitSymlinkTarget`

```go
func (cm *CommitManager) commitSymlinkTarget(change Change) (string, error)
```

**What it does**: Resolves the symlink target for a commit. Checks `change.SymlinkTarget` first, then the session manager's tracked symlink target, and finally reads from disk via `os.Readlink`.

**Called from**: `workspaceBundleOperation`.

---

#### `commitRepositoryRelativePath`

```go
func commitRepositoryRelativePath(displayPath, fullPath string) (string, error)
```

**What it does**: Computes a repository-relative path by stripping the display path prefix from a full path. Validates that the full path is a descendant of the display path.

**Called from**: `localCommitOperationFromStagedEntry`, `repositoryClientChanges`, `workspaceBundleOperation`.

---

#### `loadCommitLocalFile`

```go
func loadCommitLocalFile(localPath string) ([]byte, uint32, error)
```

**What it does**: Reads a local file's content and permission mode. Returns an error if the file is a symlink (symlink commits are not directly supported for file reads).

**Called from**: `repositoryClientChanges`, `workspaceBundleOperation`, `stagedEntryMatchesCurrent`, `snapshotStagedEntry` (session_socket.go).

---

#### `(cm *CommitManager) DryRun`

```go
func (cm *CommitManager) DryRun() (*CommitResult, error)
```

**What it does**: Returns a preview of what would be committed without actually committing. Checks if the session exists, lists changes, and verifies that local files still exist for create/modify changes. Tracks failure counts.

---

## file_handle.go

Package fuse — FUSE filesystem layer for MonoFS. Implements the file handle used for FUSE read/write operations.

### Types

#### `monofsFileHandle`

```go
type monofsFileHandle struct {
    file       *os.File
    node       *MonoNode
    logger     *slog.Logger
    flushCount int
}
```

Wraps an `os.File` for FUSE write operations. Implements `FileReader`, `FileWriter`, `FileFlusher`, `FileFsyncer`, and `FileReleaser` from `go-fuse/v2/fs`.

**`flushCount`**: Counter to detect duplicate flushes (each `close()` on a file descriptor triggers `Flush`).

### Functions

#### `(h *monofsFileHandle) Read`

```go
func (h *monofsFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno)
```

**What it does**: Implements `fs.FileReader`. Reads from the underlying `os.File` at the given offset using `ReadAt`. On EOF, returns a partial result (not an error). Records the operation and bytes read on the node's client.

**Returns**: `ReadResultData` with the bytes read, and errno 0 or EIO on error.

---

#### `(h *monofsFileHandle) Write`

```go
func (h *monofsFileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno)
```

**What it does**: Implements `fs.FileWriter`. Writes data at the given offset using `WriteAt`. Records the operation on the client, marks the node as having local writes (`isLocalWrite = true`), and updates the node's cached size if the write extends the file.

**Returns**: Number of bytes written, and errno 0 or EIO on error.

---

#### `(h *monofsFileHandle) Flush`

```go
func (h *monofsFileHandle) Flush(ctx context.Context) syscall.Errno
```

**What it does**: Implements `fs.FileFlusher`. Called on every `close()` of a file descriptor. Increments the flush counter. If the node was marked as having local writes, it resets the dirty flag and calls `TrackChangeWithMeta` with the known file size to record the modification in the session overlay DB. If tracking fails, the dirty flag is restored so a subsequent Flush will retry.

**Important**: Does NOT fsync to disk — that is deferred to `Fsync`. This flush is for overlay tracking only.

---

#### `(h *monofsFileHandle) Fsync`

```go
func (h *monofsFileHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno
```

**What it does**: Implements `fs.FileFsyncer`. Called only when the application explicitly requests `fsync`/`fdatasync`. Calls `file.Sync()` to flush to disk. Returns EIO on failure.

---

#### `(h *monofsFileHandle) Release`

```go
func (h *monofsFileHandle) Release(ctx context.Context) syscall.Errno
```

**What it does**: Implements `fs.FileReleaser`. Closes the underlying `os.File` and sets it to nil. Logs a warning if close fails (possible data loss). FUSE Release cannot return errors to the kernel, so failures are logged only.

---

## node.go

Package fuse — FUSE filesystem layer for MonoFS.
Contains the `MonoNode` type definition, constructors, and shared helpers used across multiple operation files.

### Types

#### `MonoNode`

```go
type MonoNode struct {
    fs.Inode
    path              string
    isDir             bool
    mode              uint32
    size              uint64
    client            client.MonoFSClient
    cache             *cache.Cache
    sessionMgr        *SessionManager
    workspace         *WorkspaceManifest
    workspaceGit      workspaceGitProjection
    owner             nodeOwner
    logger            *slog.Logger
    mu                sync.RWMutex
    content           []byte
    backendError      error
    lastErrorCheck    time.Time
    catastrophicError string
    catErrorMu        sync.RWMutex
    isLocalWrite      bool
    localHandle       *os.File
    symlinkTarget     string
    modTime           time.Time
    sessionID         string
}
```

Represents a node in the MonoFS filesystem. Implements `NodeLookuper`, `NodeGetattrer`, `NodeReaddirer`, `NodeOpener`, `NodeReader`, `NodeStatfser`, `NodeSetattrer`, `NodeCreater`, `NodeMkdirer`, `NodeUnlinker`, `NodeRmdirer`, `NodeRenamer`, `NodeWriter`, `NodeSymlinker`, and `NodeReadlinker`.

### Constants

- `maxMetadataRetries = 2` — retries for Lookup/Getattr backend calls.
- `backendErrorTTL = 30 * time.Second` — how long a backend error is considered active before `hasBackendError()` returns false.
- `readdirTimeout = 30 * time.Second` — timeout for Readdir backend RPC retries.

### Functions

#### `NewRoot`

```go
func NewRoot(c client.MonoFSClient, cache *cache.Cache, logger *slog.Logger) *MonoNode
```

**What it does**: Creates the root `MonoNode` for the FUSE filesystem with directory mode `0755|S_IFDIR`, an empty path, and the process owner's UID/GID. Sets `isDir = true`.

**Called from**: Mount setup (read-only mode).

---

#### `NewRootWithSession`

```go
func NewRootWithSession(c client.MonoFSClient, cache *cache.Cache, sessionMgr *SessionManager, logger *slog.Logger) *MonoNode
```

**What it does**: Creates the root `MonoNode` with a session manager for write support. Otherwise identical to `NewRoot`.

**Called from**: Mount setup (writable mode).

---

#### `(n *MonoNode) EnableVirtualMonorepo`

```go
func (n *MonoNode) EnableVirtualMonorepo() error
```

**What it does**: Turns on the synthetic source-root projection. Checks that the client implements `WorkspaceMetadataProvider` and creates a `WorkspaceManifest`. Returns an error if the client doesn't support workspace metadata.

**Called from**: Mount setup when `--virtual-monorepo` is enabled.

---

#### `(n *MonoNode) EnableWorkspaceGitProjection`

```go
func (n *MonoNode) EnableWorkspaceGitProjection(mountPoint, stateDir string) error
```

**What it does**: Exposes a synthetic root `.git` file backed by a local gitdir snapshot. Requires `EnableVirtualMonorepo` to have been called first. Creates a `WorkspaceGitProjection` with the node's owner UID/GID.

**Called from**: Mount setup.

---

#### `(n *MonoNode) SyncWorkspaceGitProjection`

```go
func (n *MonoNode) SyncWorkspaceGitProjection(ctx context.Context) error
```

**What it does**: Synchronizes the workspace git projection. Returns nil if no projection is configured.

---

#### `(n *MonoNode) stampNode`

```go
func (n *MonoNode) stampNode()
```

**What it does**: Records the current time and active session ID on the node. Called on every mutation (create, write, setattr, rename, symlink) for auditing/debugging.

---

#### `(n *MonoNode) newChild`

```go
func (n *MonoNode) newChild(name string, isDir bool, mode uint32, size uint64) *MonoNode
```

**What it does**: Creates a child node inheriting the parent's client, cache, session manager, workspace, workspace git projection, owner, and logger. Computes the child's path by joining the parent path and name.

**Called from**: `lookupFromOverlay`, `lookupSyntheticWorkspaceEntry`, and various `op_*.go` files.

---

#### `toErrno`

```go
func toErrno(err error) syscall.Errno
```

**What it does**: Converts any error to `syscall.Errno`. Returns 0 for nil errors, and `syscall.EIO` for all others.

---

#### `(n *MonoNode) recordAndConvertError`

```go
func (n *MonoNode) recordAndConvertError(err error) syscall.Errno
```

**What it does**: Converts an error to errno while recording I/O error metrics. Context cancellations (`Canceled`, `DeadlineExceeded`) return `EINTR` without being counted as errors. Other errors are recorded on the client and return `EIO`.

---

#### `(n *MonoNode) getRootNode`

```go
func (n *MonoNode) getRootNode() *MonoNode
```

**What it does**: Safely returns the root `MonoNode` from the FUSE tree. Handles the case where the node is not mounted (e.g., unit tests) by returning self if `path` is empty.

---

#### `(n *MonoNode) updateBackendError`

```go
func (n *MonoNode) updateBackendError(err error)
```

**What it does**: Stores a backend error and the current timestamp on the node under the mutex lock.

---

#### `(n *MonoNode) getBackendError`

```go
func (n *MonoNode) getBackendError() (time.Time, error)
```

**What it does**: Returns the last backend error check time and the error itself.

---

#### `(n *MonoNode) hasBackendError`

```go
func (n *MonoNode) hasBackendError() bool
```

**What it does**: Checks if there's an active backend error within the TTL window (`backendErrorTTL`). Returns false if no error or the TTL has elapsed, preventing stale `FS_ERROR.txt` files from persisting after backend recovery.

---

#### `retryDelay`

```go
func retryDelay(attempt int) time.Duration
```

**What it does**: Returns an exponential backoff duration with ±25% jitter: 50ms, 100ms, 200ms, etc. Uses `math/rand` for jitter to avoid thundering-herd retries.

---

#### `attrTimeout`

```go
func attrTimeout() time.Duration
```

**What it does**: Returns the attribute cache timeout duration for FUSE (`cache.DefaultAttrTTL`).

---

#### `overlayEntryTimeout`

```go
func overlayEntryTimeout() time.Duration
```

**What it does**: Returns a short timeout (1 second) for user-dir overlay content, which changes rapidly during downloads.

---

#### `backendEntryTimeout`

```go
func backendEntryTimeout() time.Duration
```

**What it does**: Returns a shorter timeout (5 seconds) for backend entries when the filesystem is writable, so the kernel re-validates cached entries quickly after overlay mutations.

---

#### `addWriteBits`

```go
func addWriteBits(mode uint32) uint32
```

**What it does**: Upgrades backend-reported file/directory permissions so the kernel allows write operations. Directories get `0700` added (rwx for owner); regular files get `0200` added (write for owner). Without this, the kernel may reject mutations with EACCES before FUSE sees them.

---

#### `isDependencyPath`

```go
func isDependencyPath(path string) bool
```

**What it does**: Returns true if the path is under the "dependency/" tree. Used to preserve original permissions (0444 for files, 0555 for dirs) for `go mod verify` hash correctness.

---

#### `(n *MonoNode) virtualMonorepoEnabled`

```go
func (n *MonoNode) virtualMonorepoEnabled() bool
```

**What it does**: Returns true if a workspace manifest is set on the node.

---

#### `(n *MonoNode) shouldHideWorkspacePath`

```go
func (n *MonoNode) shouldHideWorkspacePath(path string) bool
```

**What it does**: Delegates to the workspace manifest's `ShouldHidePath` method.

---

#### `(n *MonoNode) backendPath`

```go
func (n *MonoNode) backendPath() string
```

**What it does**: Returns the backend path for the node, remapping system view paths if applicable (`backendPathForSystemView`).

---

#### `(n *MonoNode) backendChildPath`

```go
func (n *MonoNode) backendChildPath(name string) string
```

**What it does**: Computes the backend path for a child by joining the current path with the child name and checking for system view remapping.

---

#### `(n *MonoNode) isWorkspaceSystemViewPath`

```go
func (n *MonoNode) isWorkspaceSystemViewPath() bool
```

**What it does**: Delegates to `isWorkspaceSystemPath`.

---

#### `(n *MonoNode) isWorkspaceReadOnlyPath`

```go
func (n *MonoNode) isWorkspaceReadOnlyPath() bool
```

**What it does**: Delegates to `isWorkspaceReadOnlyPath`.

---

#### `(n *MonoNode) shouldHideWorkspaceChild`

```go
func (n *MonoNode) shouldHideWorkspaceChild(name string) bool
```

**What it does**: Hides the synthetic workspace git entry at root level; otherwise delegates to the workspace manifest's `ShouldHideChild`.

---

#### `(n *MonoNode) shouldReserveWorkspaceRoot`

```go
func (n *MonoNode) shouldReserveWorkspaceRoot(name string) bool
```

**What it does**: Returns true if the workspace git entry should be reserved at root, or delegates to `ShouldReserveRoot`.

---

#### `(n *MonoNode) filterWorkspaceDirEntries`

```go
func (n *MonoNode) filterWorkspaceDirEntries(entries []fuse.DirEntry) []fuse.DirEntry
```

**What it does**: Filters directory entries through the workspace manifest and injects the synthetic workspace git entry at root level if configured. Sorts the result by name.

---

#### `(n *MonoNode) syntheticWorkspaceFileContent`

```go
func (n *MonoNode) syntheticWorkspaceFileContent(path string) ([]byte, bool)
```

**What it does**: Returns synthetic file content for workspace paths: `.gitignore` content from the manifest, or `.git` file content from the workspace git projection.

---

#### `(n *MonoNode) loadSyntheticWorkspaceFileContent`

```go
func (n *MonoNode) loadSyntheticWorkspaceFileContent(ctx context.Context, path string) ([]byte, syscall.Errno, bool)
```

**What it does**: Loads synthetic file content for a path. For the workspace manifest JSON, generates it via `workspace.JSONContent(ctx)` and converts errors to errno.

---

#### `(n *MonoNode) WorkspaceManifest`

```go
func (n *MonoNode) WorkspaceManifest() *WorkspaceManifest
```

**What it does**: Returns the mounted virtual-monorepo manifest, if enabled.

---

#### `(n *MonoNode) lookupSyntheticWorkspaceEntry`

```go
func (n *MonoNode) lookupSyntheticWorkspaceEntry(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno, bool)
```

**What it does**: Handles lookup for synthetic workspace entries. For control/system paths, creates a directory inode. For files (`.gitignore`, `.git`, manifest JSON), loads content and creates a regular file inode. Returns the inode, errno, and a boolean indicating whether the lookup was handled.

---

#### `(n *MonoNode) newSyntheticInode`

```go
func (n *MonoNode) newSyntheticInode(ctx context.Context, child *MonoNode, stable fs.StableAttr) (inode *fs.Inode)
```

**What it does**: Creates a new go-fuse inode for a synthetic node. Recovers from panics (e.g., if the node is not mounted in a FUSE tree) and returns nil in that case.

---

#### `(n *MonoNode) getattrSyntheticWorkspacePath`

```go
func (n *MonoNode) getattrSyntheticWorkspacePath(ctx context.Context, out *fuse.AttrOut) (syscall.Errno, bool)
```

**What it does**: Populates `fuse.AttrOut` for synthetic workspace paths. For control/system directories: mode `0555|S_IFDIR`, nlink 2. For files: mode `0444|S_IFREG`, size from content length. Returns boolean indicating if the path was handled.

---

#### `(n *MonoNode) syntheticWorkspaceControlEntries`

```go
func (n *MonoNode) syntheticWorkspaceControlEntries() []fuse.DirEntry
```

**What it does**: Returns directory entries for the synthetic control directory: the system subdirectory and the manifest JSON file. Sorted by name.

---

#### `(n *MonoNode) invalidateEntry`

```go
func (n *MonoNode) invalidateEntry(name string)
```

**What it does**: Invalidates the kernel dentry cache for a child name. Removes the child from the go-fuse inode and calls `NotifyEntry` to force the kernel to re-issue a LOOKUP. Recovers from panics (safe for unit tests).

---

#### `(n *MonoNode) lookupFromOverlay`

```go
func (n *MonoNode) lookupFromOverlay(ctx context.Context, name, childPath string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno)
```

**What it does**: Creates a child node using local overlay attributes. Calls `OverlayManager.GetLocalAttr`, then constructs the child node, populates the `EntryOut` with mode, size, ino, mtime/atime/ctime, owner, and nlink. Uses `overlayEntryTimeout()` for both attribute and entry timeouts.

**Returns**: The new inode and errno 0, or `ENOENT` if the local file doesn't exist.

---

#### `(n *MonoNode) getattrFromOverlay`

```go
func (n *MonoNode) getattrFromOverlay(out *fuse.AttrOut) syscall.Errno
```

**What it does**: Populates `fuse.AttrOut` from the local overlay file using `OverlayManager.GetLocalAttr`. Sets mode, size, ino, mtime/atime/ctime, owner, and nlink. Uses `overlayEntryTimeout()`. Returns `ENOENT` if the local file doesn't exist.

---

#### `(n *MonoNode) recoverPanic`

```go
func (n *MonoNode) recoverPanic(operation string)
```

**What it does**: Catches panics and stores them as catastrophic errors. Tries to find the root node via the FUSE tree and stores the error there; if not mounted, stores on self. Logs the stack trace.

**Called from**: Every FUSE operation handler (via `defer n.recoverPanic(...)`) in `op_*.go` files.

---

#### `(n *MonoNode) ensureLocalCopy`

```go
func (n *MonoNode) ensureLocalCopy(ctx context.Context) error
```

**What it does**: Delegates to `ensureLocalCopyFor` with the node's own path. Copies the file from backend to local overlay.

---

#### `(n *MonoNode) ensureLocalCopyFor`

```go
func (n *MonoNode) ensureLocalCopyFor(ctx context.Context, monofsPath string) error
```

**What it does**: Copies a file from the backend to the local overlay. Starts a session if needed, checks if the local copy already exists, creates parent directories, and then either:
- For paths under user root directories: creates an empty file (the backend doesn't know about these paths).
- Otherwise: fetches content from the backend via `client.Read`, and if that fails, creates an empty file.

**Important**: Does NOT call `TrackChange` — the caller is responsible for tracking changes at the appropriate time.

---

#### `hashPathForNode`

```go
func hashPathForNode(path string) uint64
```

**What it does**: Creates a stable inode number from a path using the FNV-1a hash algorithm (offset basis `14695981039346656037`, prime `1099511628211`).

---

#### `splitPath`

```go
func splitPath(path string) []string
```

**What it does**: Splits a path into its components by "/". Returns nil for empty string.

---

## overlay.go

Package fuse — FUSE filesystem layer for MonoFS.
Implements `OverlayManager` which handles merging local changes with backend data.

### Types

#### `OverlayManager`

```go
type OverlayManager struct {
    sessionMgr *SessionManager
}
```

Handles merging local changes with backend data. Uses `OverlayDB` for all overlay queries — no filesystem scanning needed except for fallback paths.

#### `LocalAttr`

```go
type LocalAttr struct {
    Size  uint64
    Mode  uint32
    Mtime int64
    IsDir bool
}
```

Represents file attributes from the local overlay.

### Functions

#### `NewOverlayManager`

```go
func NewOverlayManager(sessionMgr *SessionManager) *OverlayManager
```

**What it does**: Creates a new `OverlayManager` with the given session manager.

**Called from**: `node.go` (`lookupFromOverlay`, `getattrFromOverlay`) and `op_*.go` files.

---

#### `(om *OverlayManager) MergeReadDir`

```go
func (om *OverlayManager) MergeReadDir(backendEntries []fuse.DirEntry, dirPath string) []fuse.DirEntry
```

**What it does**: Merges backend directory entries with local overlay changes to produce a complete directory listing.

**Logic**:
1. Builds a map from backend entries.
2. If `backendEntries` is nil (user-root-dir path), scans disk directly as the authoritative source, skipping the `overlay.db` directory.
3. At root level, adds user-created directories from the overlay DB.
4. Queries overlay files under the directory from the DB, adding/modifying entries.
5. Removes entries that are marked as deleted in the overlay DB.
6. Sorts the result by name for deterministic ordering.

**Called from**: `op_readdir.go` (the Readdir handler).

---

#### `(om *OverlayManager) toSlice`

```go
func (om *OverlayManager) toSlice(entries map[string]fuse.DirEntry) []fuse.DirEntry
```

**What it does**: Converts an entry map to a sorted slice. Sorts by name.

**Called from**: `MergeReadDir`.

---

#### `(om *OverlayManager) ShouldUseLocalFile`

```go
func (om *OverlayManager) ShouldUseLocalFile(monofsPath string) bool
```

**What it does**: Checks if a file should be read from local overlay instead of the backend. Delegates to `sessionMgr.HasLocalOverride`.

**Called from**: `op_read.go` (the Read handler).

---

#### `(om *OverlayManager) GetLocalContent`

```go
func (om *OverlayManager) GetLocalContent(monofsPath string) ([]byte, error)
```

**What it does**: Reads file content from the local overlay. Resolves the local path from the session manager and calls `os.ReadFile`.

---

#### `(om *OverlayManager) GetLocalAttr`

```go
func (om *OverlayManager) GetLocalAttr(monofsPath string) (*LocalAttr, error)
```

**What it does**: Gets file attributes from the local overlay. Lstats the actual file for fresh size/mtime, then constructs a `LocalAttr` with mode bits including the file type (`S_IFLNK`, `S_IFDIR`, or `S_IFREG`).

**Called from**: `node.go` (`lookupFromOverlay`, `getattrFromOverlay`).

---

#### `(om *OverlayManager) IsPathDeleted`

```go
func (om *OverlayManager) IsPathDeleted(monofsPath string) bool
```

**What it does**: Checks if a path has been marked as deleted in the current session. Delegates to `sessionMgr.IsDeleted`.

---

## overlaydb.go

Package fuse — FUSE filesystem layer for MonoFS.
Implements `OverlayDB`, a wrapper around NutsDB for tracking overlay file metadata.

### Types

#### `FileEntryType`

```go
type FileEntryType string
```

Constants: `FileEntryRegular` ("file"), `FileEntryDir` ("dir"), `FileEntrySymlink` ("symlink").

#### `FileEntry`

```go
type FileEntry struct {
    Type          FileEntryType `json:"type"`
    LocalPath     string        `json:"local_path"`
    Mode          uint32        `json:"mode"`
    Size          uint64        `json:"size"`
    Mtime         int64         `json:"mtime"`
    SymlinkTarget string        `json:"symlink_target,omitempty"`
    OrigHash      string        `json:"orig_hash,omitempty"`
    ChangeType    ChangeType    `json:"change_type"`
    Timestamp     time.Time     `json:"timestamp"`
}
```

Represents metadata about an overlayed file stored in NutsDB. Actual file content remains on disk; the DB only tracks what is overlayed.

#### `DeletedEntry`

```go
type DeletedEntry struct {
    Path       string     `json:"path"`
    ChangeType ChangeType `json:"change_type"`
    DeletedAt  time.Time  `json:"deleted_at"`
}
```

Preserves the change type for deleted paths so file deletes and directory removals can be reconstructed distinctly.

#### `OverlayDB`

```go
type OverlayDB struct {
    db     *nutsdb.DB
    dbPath string
    logger *slog.Logger
}
```

Wraps NutsDB to track which files are overlayed in a session.

#### `PathStatResult`

```go
type PathStatResult struct {
    IsDeleted     bool
    FileEntry     FileEntry
    HasOverride   bool
    IsUserRootDir bool
}
```

Batched result of path state queries (IsDeleted + GetFile + IsUserDir in a single transaction).

### Bucket Constants

- `bucketOverlayFiles` ("files") — monofs path → FileEntry JSON
- `bucketOverlayDeleted` ("deleted") — monofs path → deletion timestamp/JSON
- `bucketOverlayDirs` ("userdirs") — root dir name → creation timestamp
- `bucketOverlayStaged` ("staged") — monofs path → StagedIndexEntry JSON
- `bucketOverlayCommits` ("commits") — local commit id → LocalVirtualCommit JSON
- `bucketOverlayBranch` ("branch") — branch metadata and mappings

### Functions

#### `OpenOverlayDB`

```go
func OpenOverlayDB(dir string, logger *slog.Logger) (*OverlayDB, error)
```

**What it does**: Opens (or creates) the overlay database at the given directory. Creates `overlay.db/` subdirectory, configures NutsDB with 16MB segments and async writes (`SyncEnable = false`), and initializes all six buckets. Logs the open event.

**Called from**: `SessionManager.StartSession`, `SessionManager.recoverSession`.

---

#### `(odb *OverlayDB) initBuckets`

```go
func (odb *OverlayDB) initBuckets() error
```

**What it does**: Creates all six NutsDB buckets. Ignores `ErrBucketAlreadyExist` for idempotency.

**Called from**: `OpenOverlayDB`.

---

#### `(odb *OverlayDB) Close`

```go
func (odb *OverlayDB) Close() error
```

**What it does**: Closes the NutsDB database.

**Called from**: `SessionManager.CommitSession`, `SessionManager.DiscardSession`.

---

#### `(odb *OverlayDB) PutFile`

```go
func (odb *OverlayDB) PutFile(monofsPath string, entry FileEntry) error
```

**What it does**: Records a file entry in the overlay database. Marshals the entry to JSON, removes the path from the deleted bucket if present, and puts it in the files bucket with TTL 0 (persistent).

**Called from**: `SessionManager.trackChangeInternal`, `CommitManager.putOverlayFileFromDisk`, `SessionManager.CreateSynlink`, `SessionManager.CreateUserRootDir`, `OverlayDB.RefreshEntry`.

---

#### `(odb *OverlayDB) GetFile`

```go
func (odb *OverlayDB) GetFile(monofsPath string) (FileEntry, bool, error)
```

**What it does**: Retrieves a file entry from the overlay database. Returns the entry, whether it was found, and any error. Unmarshals JSON from the value bytes.

**Called from**: Multiple places throughout session.go, commit.go, session_socket.go.

---

#### `(odb *OverlayDB) DeleteFile`

```go
func (odb *OverlayDB) DeleteFile(monofsPath string) error
```

**What it does**: Removes a file entry from the overlay database. Does NOT mark it as deleted from the backend. Ignores not-found errors.

**Called from**: `SessionManager.trackChangeInternal` (for session-only entries), `SessionManager.RemoveUserRootDir`, `SessionManager.RemoveBlobChanges`.

---

#### `(odb *OverlayDB) MarkDeleted`

```go
func (odb *OverlayDB) MarkDeleted(monofsPath string) error
```

**What it does**: Records that a backend file was deleted in this session. Delegates to `MarkDeletedWithType` with `ChangeDelete`.

---

#### `(odb *OverlayDB) MarkDeletedWithType`

```go
func (odb *OverlayDB) MarkDeletedWithType(monofsPath string, changeType ChangeType) error
```

**What it does**: Records a deletion with a specific change type (`ChangeDelete` or `ChangeRmdir`). Creates a `DeletedEntry` with the current UTC timestamp, marshals to JSON, removes any existing file entry, and puts the deleted marker.

**Called from**: `SessionManager.trackChangeInternal`, `CommitManager.reconcileCommittedEntry`, `OverlayDB.MarkDeleted`.

---

#### `(odb *OverlayDB) UnmarkDeleted`

```go
func (odb *OverlayDB) UnmarkDeleted(monofsPath string) error
```

**What it does**: Removes a path from the deleted set.

**Called from**: `SessionManager.RemoveBlobChanges`.

---

#### `(odb *OverlayDB) IsDeleted`

```go
func (odb *OverlayDB) IsDeleted(monofsPath string) bool
```

**What it does**: Checks if a path has been marked as deleted.

**Called from**: `SessionManager.IsDeleted`.

---

#### `(odb *OverlayDB) PutUserDir`

```go
func (odb *OverlayDB) PutUserDir(name string) error
```

**What it does**: Records a user-created root directory. Stores the current RFC3339 timestamp as the value.

**Called from**: `SessionManager.trackChangeInternal` (for `ChangeUserRootDir`), `SessionManager.CreateUserRootDir`.

---

#### `(odb *OverlayDB) RemoveUserDir`

```go
func (odb *OverlayDB) RemoveUserDir(name string) error
```

**What it does**: Removes a user-created root directory record.

**Called from**: `SessionManager.trackChangeInternal` (for `ChangeRemoveUserRootDir`), `SessionManager.RemoveUserRootDir`, `SessionManager.RemoveBlobChanges`.

---

#### `(odb *OverlayDB) IsUserDir`

```go
func (odb *OverlayDB) IsUserDir(name string) bool
```

**What it does**: Checks if a name is a user-created root directory.

**Called from**: `SessionManager.IsUserRootDir`.

---

#### `(odb *OverlayDB) GetPathStatBatch`

```go
func (odb *OverlayDB) GetPathStatBatch(monofsPath string) PathStatResult
```

**What it does**: Performs all path-state lookups (IsDeleted, GetFile, IsUserDir) in a single NutsDB View transaction instead of three separate ones. Reduces transaction overhead by ~3x on every Lookup/Getattr.

**Called from**: `SessionManager.GetPathState`.

---

#### `(odb *OverlayDB) ListUserDirs`

```go
func (odb *OverlayDB) ListUserDirs() []string
```

**What it does**: Returns all user-created root directory names by scanning all keys in `bucketOverlayDirs`.

**Called from**: `SessionManager.recoverSession`, `SessionManager.ListUserRootDirs`, `OverlayManager.MergeReadDir`.

---

#### `(odb *OverlayDB) ListFilesUnderDir`

```go
func (odb *OverlayDB) ListFilesUnderDir(dirPath string) ([]FileEntry, []string, error)
```

**What it does**: Returns all overlay file entries whose path is a direct child of the given directory. For root (`""`), scans all entries and filters to those without "/" in the path. For non-root, uses `PrefixScanEntries` with the `dirPath + "/"` prefix and filters to direct children only (no "/" in the relative path).

**Returns**: The file entries, their names (relative to dirPath), and any error.

**Called from**: `OverlayManager.MergeReadDir`.

---

#### `(odb *OverlayDB) ListDeletedUnderDir`

```go
func (odb *OverlayDB) ListDeletedUnderDir(dirPath string) ([]string, error)
```

**What it does**: Returns paths deleted under the given directory prefix. Uses the same direct-child filtering logic as `ListFilesUnderDir`.

**Called from**: `OverlayManager.MergeReadDir`.

---

#### `(odb *OverlayDB) GetAllFiles`

```go
func (odb *OverlayDB) GetAllFiles() (map[string]FileEntry, error)
```

**What it does**: Returns all overlay file entries as a map of path → FileEntry. Used for commit operations.

**Called from**: `SessionManager.GetChanges`, `SessionManager.GetAllBlobFiles`, `SessionManager.RemoveBlobChanges`, `SessionManager.GetDependencyFilePaths`.

---

#### `(odb *OverlayDB) GetAllDeleted`

```go
func (odb *OverlayDB) GetAllDeleted() ([]string, error)
```

**What it does**: Returns all deleted paths (string keys from the deleted bucket).

**Called from**: `SessionManager.GetDeletedBlobPaths`, `SessionManager.RemoveBlobChanges`.

---

#### `(odb *OverlayDB) GetAllDeletedEntries`

```go
func (odb *OverlayDB) GetAllDeletedEntries() ([]DeletedEntry, error)
```

**What it does**: Returns all deleted paths with their persisted change types. Handles backwards compatibility: older sessions that only stored timestamps are treated as file deletes (`ChangeDelete`). Sorted by path.

**Called from**: `SessionManager.GetChanges`.

---

#### `(odb *OverlayDB) RebuildFromDisk`

```go
func (odb *OverlayDB) RebuildFromDisk(sessionDir string) error
```

**What it does**: Scans the session directory and populates the database from the filesystem. Used when the DB doesn't exist on mount (recovery).

**Logic** (two-phase):
1. **Walk phase**: Walks the session directory, collecting file entries and user dirs. Skips `overlay.db/`, hidden files/directories (dot-prefixed), and the root itself. For symlinks, reads the target.
2. **Write phase**: Writes all entries in a single NutsDB transaction to avoid write-lock contention with the background merge worker.

**Called from**: `SessionManager.recoverSession`.

---

#### `(odb *OverlayDB) RefreshEntry`

```go
func (odb *OverlayDB) RefreshEntry(monofsPath string, localPath string) error
```

**What it does**: Re-stats a file from disk and updates the DB entry. If the file doesn't exist on disk, deletes the entry. Preserves `OrigHash`, `ChangeType`, and `SymlinkTarget` from the existing entry if found.

---

#### `(odb *OverlayDB) FileCount`

```go
func (odb *OverlayDB) FileCount() int
```

**What it does**: Returns the number of files in the overlay (`bucketOverlayFiles` key count).

**Called from**: `SessionManager.recoverSession`.

---

#### `(odb *OverlayDB) DeletedCount`

```go
func (odb *OverlayDB) DeletedCount() int
```

**What it does**: Returns the number of deleted paths (`bucketOverlayDeleted` key count).

**Called from**: `SessionManager.recoverSession`.

---

#### `(odb *OverlayDB) deleteKeysWithPrefix`

```go
func (odb *OverlayDB) deleteKeysWithPrefix(bucket, path string) (int, error)
```

**What it does**: Deletes all keys in a bucket that start with `path + "/"`. Uses `PrefixScanEntries` to find matching keys and deletes them in a single `Update` transaction.

**Returns**: Number of keys deleted, and any error.

**Called from**: `DeleteFilesUnderPrefix`, `DeleteDeletedUnderPrefix`.

---

#### `(odb *OverlayDB) DeleteFilesUnderPrefix`

```go
func (odb *OverlayDB) DeleteFilesUnderPrefix(path string) (int, error)
```

**What it does**: Removes all overlay file entries below path, without touching the path itself. Delegates to `deleteKeysWithPrefix` on the files bucket.

**Called from**: `SessionSocketHandler.removeDirectoryTarget`.

---

#### `(odb *OverlayDB) DeleteDeletedUnderPrefix`

```go
func (odb *OverlayDB) DeleteDeletedUnderPrefix(path string) (int, error)
```

**What it does**: Removes all deleted markers below path, without touching the path itself. Delegates to `deleteKeysWithPrefix` on the deleted bucket.

**Called from**: `SessionSocketHandler.removeDirectoryTarget`.

---

#### `(odb *OverlayDB) RenamePrefix`

```go
func (odb *OverlayDB) RenamePrefix(oldPrefix, newPrefix string) (int, error)
```

**What it does**: Re-keys all overlay file entries whose monofsPath starts with `oldPrefix + "/"` so they instead start with `newPrefix + "/"`. Also rewrites the `LocalPath` field by applying the same substitution. Uses `PrefixScanEntries` for O(matches) instead of O(total_entries). Also renames deletion markers under the old prefix. Collects entries first, then deletes old keys and puts new ones in a single transaction.

**Returns**: Number of entries renamed.

**Called from**: `SessionManager.RenameChildren`.

---

#### `isNotFound`

```go
func isNotFound(err error) bool
```

**What it does**: Checks if a NutsDB error indicates a missing key/bucket. Returns true for `ErrBucketNotFound`, `ErrKeyNotFound`, `ErrPrefixScan`, `ErrBucketEmpty`, or `ErrNotFoundKey`.

**Called from**: Throughout overlaydb.go and overlaydb_vcs.go.

---

## overlaydb_vcs.go

Package fuse — VCS-related overlay database operations (staged entries, local commits, branches).

### Functions

#### `(odb *OverlayDB) putJSONValue`

```go
func (odb *OverlayDB) putJSONValue(bucket, key string, value any) error
```

**What it does**: Marshals a value to JSON and puts it in the specified NutsDB bucket. Generic helper used by all Put* methods.

**Called from**: `PutStagedEntry`, `PutLocalVirtualCommit`, `PutBranchMapping`.

---

#### `(odb *OverlayDB) getJSONValue`

```go
func (odb *OverlayDB) getJSONValue(bucket, key string, target any) (bool, error)
```

**What it does**: Gets a key from a NutsDB bucket and unmarshals JSON into the target. Returns whether the key was found.

**Called from**: `GetStagedEntry`, `GetLocalVirtualCommit`, `GetBranchMapping`.

---

#### `(odb *OverlayDB) deleteBucketKey`

```go
func (odb *OverlayDB) deleteBucketKey(bucket, key string) error
```

**What it does**: Deletes a key from a NutsDB bucket, ignoring not-found errors.

**Called from**: `DeleteStagedEntry`, `DeleteLocalVirtualCommit`, `SetCurrentLogicalBranch` (when branch is empty), `DeleteBranchMapping`.

---

#### `(odb *OverlayDB) countBucket`

```go
func (odb *OverlayDB) countBucket(bucket string) int
```

**What it does**: Returns the number of keys in a bucket.

**Called from**: `StagedEntryCount`, `LocalVirtualCommitCount`.

---

#### `(odb *OverlayDB) PutStagedEntry`

```go
func (odb *OverlayDB) PutStagedEntry(path string, entry StagedIndexEntry) error
```

**What it does**: Persists a staged index entry. Trims the path, validates it's non-empty, and fills in `entry.Path` if empty. Uses `putJSONValue`.

**Called from**: `SessionManager.PutStagedEntry`.

---

#### `(odb *OverlayDB) GetStagedEntry`

```go
func (odb *OverlayDB) GetStagedEntry(path string) (StagedIndexEntry, bool, error)
```

**What it does**: Retrieves a staged index entry by path. Uses `getJSONValue`.

**Called from**: `SessionManager.GetStagedEntry`.

---

#### `(odb *OverlayDB) DeleteStagedEntry`

```go
func (odb *OverlayDB) DeleteStagedEntry(path string) error
```

**What it does**: Deletes a staged entry by path. Uses `deleteBucketKey`.

**Called from**: `SessionManager.DeleteStagedEntry`.

---

#### `(odb *OverlayDB) ListStagedEntries`

```go
func (odb *OverlayDB) ListStagedEntries() ([]StagedIndexEntry, error)
```

**What it does**: Lists all staged index entries from the overlay DB. Unmarshals each entry, fills in `Path` from the key if empty, and sorts by path.

**Called from**: `SessionManager.ListStagedEntries`.

---

#### `(odb *OverlayDB) ClearStagedEntries`

```go
func (odb *OverlayDB) ClearStagedEntries() error
```

**What it does**: Deletes all entries from the staged bucket in a single transaction.

**Called from**: `SessionManager.ClearStagedEntries`.

---

#### `(odb *OverlayDB) StagedEntryCount`

```go
func (odb *OverlayDB) StagedEntryCount() int
```

**What it does**: Returns the number of staged entries. Uses `countBucket`.

**Called from**: `SessionManager.recoverSession`.

---

#### `(odb *OverlayDB) PutLocalVirtualCommit`

```go
func (odb *OverlayDB) PutLocalVirtualCommit(commit LocalVirtualCommit) error
```

**What it does**: Persists a local virtual commit. Trims and validates the commit ID.

**Called from**: `SessionManager.PutLocalVirtualCommit`.

---

#### `(odb *OverlayDB) GetLocalVirtualCommit`

```go
func (odb *OverlayDB) GetLocalVirtualCommit(id string) (LocalVirtualCommit, bool, error)
```

**What it does**: Retrieves a local virtual commit by ID.

**Called from**: `SessionManager.GetLocalVirtualCommit`.

---

#### `(odb *OverlayDB) DeleteLocalVirtualCommit`

```go
func (odb *OverlayDB) DeleteLocalVirtualCommit(id string) error
```

**What it does**: Deletes a local virtual commit by ID.

**Called from**: `SessionManager.DeleteLocalVirtualCommit`, `CommitManager.CommitChanges` (on rollback).

---

#### `(odb *OverlayDB) ListLocalVirtualCommits`

```go
func (odb *OverlayDB) ListLocalVirtualCommits() ([]LocalVirtualCommit, error)
```

**What it does**: Lists all local virtual commits. Sorts by creation time (ascending), breaking ties by ID.

**Called from**: `SessionManager.ListLocalVirtualCommits`.

---

#### `(odb *OverlayDB) LocalVirtualCommitCount`

```go
func (odb *OverlayDB) LocalVirtualCommitCount() int
```

**What it does**: Returns the count of local virtual commits.

**Called from**: `SessionManager.recoverSession`.

---

#### `(odb *OverlayDB) SetCurrentLogicalBranch`

```go
func (odb *OverlayDB) SetCurrentLogicalBranch(branch string) error
```

**What it does**: Sets the current logical branch. If the branch is empty, deletes the key (clears the current branch). Otherwise writes the branch name directly (no JSON marshaling).

**Called from**: `SessionManager.SetCurrentLogicalBranch`.

---

#### `(odb *OverlayDB) GetCurrentLogicalBranch`

```go
func (odb *OverlayDB) GetCurrentLogicalBranch() (string, bool, error)
```

**What it does**: Retrieves the current logical branch name.

**Called from**: `SessionManager.GetCurrentLogicalBranch`.

---

#### `branchMappingKey`

```go
func branchMappingKey(principalID, logicalBranch, storageID string) string
```

**What it does**: Constructs a composite key for branch mappings by joining the three fields with `\x1f` (ASCII unit separator).

**Called from**: `PutBranchMapping`, `GetBranchMapping`, `DeleteBranchMapping`.

---

#### `(odb *OverlayDB) PutBranchMapping`

```go
func (odb *OverlayDB) PutBranchMapping(mapping SessionBranchMapping) error
```

**What it does**: Persists a branch mapping. Trims and validates all three required fields (principalID, logicalBranch, storageID). Sets `CreatedAt` to now if zero. Uses `putJSONValue`.

**Called from**: `SessionManager.PutBranchMapping`.

---

#### `(odb *OverlayDB) GetBranchMapping`

```go
func (odb *OverlayDB) GetBranchMapping(principalID, logicalBranch, storageID string) (SessionBranchMapping, bool, error)
```

**What it does**: Retrieves a branch mapping by its composite key.

**Called from**: `SessionManager.GetBranchMapping`, `CommitManager.recordSourcePushBranchMappings`.

---

#### `(odb *OverlayDB) DeleteBranchMapping`

```go
func (odb *OverlayDB) DeleteBranchMapping(principalID, logicalBranch, storageID string) error
```

**What it does**: Deletes a branch mapping. Uses `deleteBucketKey`.

**Called from**: `SessionManager.DeleteBranchMapping`.

---

#### `(odb *OverlayDB) ListBranchMappings`

```go
func (odb *OverlayDB) ListBranchMappings() ([]SessionBranchMapping, error)
```

**What it does**: Lists all branch mappings, skipping the `currentLogicalBranchKey`. Sorts by principal ID, then logical branch, then storage ID.

**Called from**: `SessionManager.ListBranchMappings`.

---

#### `(odb *OverlayDB) BranchMappingCount`

```go
func (odb *OverlayDB) BranchMappingCount() int
```

**What it does**: Returns the count of branch mappings, excluding the `currentLogicalBranchKey`.

**Called from**: `SessionManager.recoverSession`.

---

## ownership.go

Package fuse — FUSE filesystem layer for MonoFS.
Implements ownership resolution and setting for FUSE nodes.

### Types

#### `nodeOwner`

```go
type nodeOwner struct {
    uid uint32
    gid uint32
}
```

Stores UID and GID for a node's owner.

### Functions

#### `currentProcessOwner`

```go
func currentProcessOwner() nodeOwner
```

**What it does**: Returns the current process's UID and GID via `os.Getuid()` and `os.Getgid()`.

**Called from**: `NewRoot`, `NewRootWithSession`, `MonoNode.ownerIDs`.

---

#### `ResolvePathOwner`

```go
func ResolvePathOwner(path string) (uint32, uint32, error)
```

**What it does**: Resolves the owner (UID, GID) of a path by finding the nearest existing ancestor path and stating it. Exported for use outside the package.

---

#### `ownerFromPath`

```go
func ownerFromPath(path string) (nodeOwner, error)
```

**What it does**: Resolves a `nodeOwner` from a path by calling `nearestExistingPath` then `statPathOwner`.

**Called from**: `ResolvePathOwner`.

---

#### `nearestExistingPath`

```go
func nearestExistingPath(path string) (string, error)
```

**What it does**: Finds the nearest existing ancestor of a path by walking up the directory tree (`filepath.Dir`) until finding a path that `os.Stat` succeeds on. Returns an error if no existing ancestor is found.

---

#### `statPathOwner`

```go
func statPathOwner(path string) (nodeOwner, error)
```

**What it does**: Stats a path and extracts the owner UID/GID from the `syscall.Stat_t`. Returns an error if the platform doesn't expose uid/gid.

---

#### `ensurePathOwner`

```go
func ensurePathOwner(path string, owner nodeOwner) error
```

**What it does**: Ensures a path has the given owner. If the owner matches, returns nil. Skips if the path doesn't exist or the process is not root. Otherwise calls `os.Chown`.

---

#### `(n *MonoNode) ownerIDs`

```go
func (n *MonoNode) ownerIDs() (uint32, uint32)
```

**What it does**: Returns the node's owner UID and GID. If the node is nil or the owner is (0,0), falls back to `currentProcessOwner`.

**Called from**: `setAttrOwner`, `setEntryOwner`, `EnableWorkspaceGitProjection`.

---

#### `(n *MonoNode) setAttrOwner`

```go
func (n *MonoNode) setAttrOwner(out *fuse.AttrOut)
```

**What it does**: Sets the UID and GID on a `fuse.AttrOut` from the node's owner.

**Called from**: `getattrFromOverlay`, `getattrSyntheticWorkspacePath`, and various `op_*.go` files.

---

#### `(n *MonoNode) setEntryOwner`

```go
func (n *MonoNode) setEntryOwner(out *fuse.EntryOut)
```

**What it does**: Sets the UID and GID on a `fuse.EntryOut` from the node's owner.

**Called from**: `lookupFromOverlay`, `lookupSyntheticWorkspaceEntry`, and various `op_*.go` files.

---

#### `(n *MonoNode) SetVisibleOwner`

```go
func (n *MonoNode) SetVisibleOwner(uid, gid uint32)
```

**What it does**: Sets the visible owner UID/GID on the node. If a workspace git projection is configured, also sets the owner on it. Safe to call on nil receiver.

---

## session.go

Package fuse — FUSE filesystem layer for MonoFS.
Implements `SessionManager` and `WriteSession` for managing write sessions.

### Types

#### `ChangeType`

```go
type ChangeType string
```

Constants:
- `ChangeCreate` ("create") — new file created
- `ChangeModify` ("modify") — existing file modified
- `ChangeDelete` ("delete") — file deleted
- `ChangeMkdir` ("mkdir") — directory created
- `ChangeRmdir` ("rmdir") — directory removed
- `ChangeSymlink` ("symlink") — symlink created
- `ChangeUserRootDir` ("user_root_dir") — user directory at root
- `ChangeRemoveUserRootDir` ("remove_user_root_dir") — user root dir removed
- `ChangeBaseline` ("baseline") — overlay state part of the committed baseline, excluded from `GetChanges()`

#### `Change`

```go
type Change struct {
    Type          ChangeType `json:"type"`
    Path          string     `json:"path"`
    LocalPath     string     `json:"local_path"`
    OrigHash      string     `json:"orig_hash"`
    SymlinkTarget string     `json:"symlink_target,omitempty"`
    Timestamp     time.Time  `json:"timestamp"`
}
```

Represents a single file change in a write session. Kept for backward compatibility.

#### `WriteSession`

```go
type WriteSession struct {
    ID        string    `json:"id"`
    CreatedAt time.Time `json:"created_at"`
    BasePath  string    `json:"base_path"`
    mu        sync.RWMutex
}
```

Represents an active write session. Metadata is stored in NutsDB via OverlayDB.

#### `SessionManager`

```go
type SessionManager struct {
    overlayBase  string
    current      *WriteSession
    db           *OverlayDB
    mu           sync.RWMutex
    logger       *slog.Logger
    depsPushedAt time.Time
}
```

Manages write sessions. All overlay state is persisted in NutsDB.

#### `BlobFileEntry`

```go
type BlobFileEntry struct {
    LocalPath     string
    Type          FileEntryType
    SymlinkTarget string
    Mode          uint32
}
```

Carries enough information for `collectBlobFiles` to handle regular files, symlinks, and empty directories.

#### `PathState`

```go
type PathState struct {
    HasSession    bool
    IsDeleted     bool
    IsSymlink     bool
    SymlinkTarget string
    HasOverride   bool
    IsUserRootDir bool
    LocalPath     string
}
```

Consolidated session state for a path.

### Constants

- `depsPushBypassDuration = 60 * time.Second`

### Functions

#### `isVisibleChangeType`

```go
func isVisibleChangeType(changeType ChangeType) bool
```

**What it does**: Returns true if the change type is non-empty and not `ChangeBaseline`. Baseline changes are hidden from `GetChanges()`.

**Called from**: `SessionManager.GetChanges`, `SessionManager.GetSessionInfo`.

---

#### `(sm *SessionManager) MarkDepsPushed`

```go
func (sm *SessionManager) MarkDepsPushed()
```

**What it does**: Records that a dependency push just completed by storing the current time in `depsPushedAt`.

**Called from**: `SessionSocketHandler.handleRemoveBlobChanges`.

---

#### `(sm *SessionManager) DepsPushedRecently`

```go
func (sm *SessionManager) DepsPushedRecently() bool
```

**What it does**: Returns true if a dependency push completed within `depsPushBypassDuration`. During this window, FUSE should bypass the kernel page cache for dependency paths.

---

#### `NewSessionManager`

```go
func NewSessionManager(overlayBase string, logger *slog.Logger) (*SessionManager, error)
```

**What it does**: Creates a new session manager. Creates `sessions/` and `committed/` directories, reads the "current" symlink for recovery, calls `recoverSession()`, and auto-starts a new session if none was recovered. This ensures overlay tracking works immediately when `--writable` is enabled.

**Called from**: Mount setup.

---

#### `(sm *SessionManager) recoverSession`

```go
func (sm *SessionManager) recoverSession() error
```

**What it does**: Attempts to recover an active session after a crash. Reads the "current" symlink, verifies the session directory exists, opens the overlay DB, logs statistics, and if the DB is empty, rebuilds from disk scan. Sets `sm.current` and `sm.db` on success.

**Called from**: `NewSessionManager`.

---

#### `(sm *SessionManager) StartSession`

```go
func (sm *SessionManager) StartSession() (*WriteSession, error)
```

**What it does**: Creates a new write session or returns the existing one. Uses a read-lock fast path for the common case. On slow path: acquires write lock, double-checks, generates a session ID, creates the session directory, opens the overlay DB, updates the "current" symlink, and sets `sm.current` and `sm.db`.

**Returns**: The session, or error if directory/DB creation fails.

**Called from**: `SessionSocketHandler.handleStart`, `SessionSocketHandler.handleRemove`, `MonoNode.ensureLocalCopyFor`.

---

#### `(sm *SessionManager) GetCurrentSession`

```go
func (sm *SessionManager) GetCurrentSession() *WriteSession
```

**What it does**: Returns the current active session (nil if none) under read lock.

**Called from**: Many places throughout commit.go, session_socket.go, node.go.

---

#### `(sm *SessionManager) HasActiveSession`

```go
func (sm *SessionManager) HasActiveSession() bool
```

**What it does**: Returns true if there's an active write session.

---

#### `(sm *SessionManager) FlushChanges`

```go
func (sm *SessionManager) FlushChanges() error
```

**What it does**: No-op with NutsDB — all writes are immediately persisted. Returns nil.

---

#### `(sm *SessionManager) CommitSession`

```go
func (sm *SessionManager) CommitSession() error
```

**What it does**: Finalizes the current session and archives it. Acquires write lock, closes the overlay DB, renames the session directory to `committed/<timestamp>-<shortID>`, removes the "current" symlink, and sets `sm.current = nil`.

---

#### `(sm *SessionManager) DiscardSession`

```go
func (sm *SessionManager) DiscardSession() error
```

**What it does**: Abandons the current session and removes all local changes. Closes the overlay DB, walks the session directory to make all files writable (Go modules are read-only), removes the directory tree, removes the "current" symlink, and sets `sm.current = nil`.

**Called from**: `SessionSocketHandler.handleDiscard`.

---

#### `(sm *SessionManager) GetPathState`

```go
func (sm *SessionManager) GetPathState(monofsPath string) PathState
```

**What it does**: Returns consolidated session state for a path using a single NutsDB transaction (`GetPathStatBatch`). Includes deletion status, symlink info, override status, user root dir status, and the local path.

---

#### `(sm *SessionManager) GetLocalPath`

```go
func (sm *SessionManager) GetLocalPath(monofsPath string) (string, error)
```

**What it does**: Returns the local overlay path for a MonoFS path by joining `sm.current.BasePath` with the path. Returns an error if no session is active.

**Called from**: Many places throughout the codebase.

---

#### `(sm *SessionManager) HasLocalOverride`

```go
func (sm *SessionManager) HasLocalOverride(monofsPath string) bool
```

**What it does**: Checks if a file has been modified locally by looking up the path in the overlay DB.

**Called from**: `OverlayManager.ShouldUseLocalFile`.

---

#### `(sm *SessionManager) IsDeleted`

```go
func (sm *SessionManager) IsDeleted(monofsPath string) bool
```

**What it does**: Checks if a path has been marked as deleted in the current session.

**Called from**: `OverlayManager.IsPathDeleted`, `CommitManager.reconcileCommittedEntry`, `SessionSocketHandler.resolveRemoveTarget`.

---

#### `(sm *SessionManager) RenameChildren`

```go
func (sm *SessionManager) RenameChildren(oldPath, newPath string) (int, error)
```

**What it does**: Re-keys all overlay entries under `oldPath` to sit under `newPath`. Called when a directory is renamed so tracked children are at their new paths. Delegates to `OverlayDB.RenamePrefix`.

**Called from**: `op_rename.go` (the Rename handler).

---

#### `(sm *SessionManager) TrackChangeWithMeta`

```go
func (sm *SessionManager) TrackChangeWithMeta(changeType ChangeType, monofsPath, origHash string, knownSize int64) error
```

**What it does**: Records a change using caller-supplied metadata, skipping the `os.Lstat` that `TrackChange` normally performs. Use on hot paths where the caller already knows the file size (e.g., after a Write or Truncate). A negative `knownSize` causes a fallback to `os.Lstat`.

**Called from**: `monofsFileHandle.Flush`.

---

#### `(sm *SessionManager) TrackChange`

```go
func (sm *SessionManager) TrackChange(changeType ChangeType, monofsPath, origHash string) error
```

**What it does**: Records a change in the session's overlay database. Delegates to `trackChangeInternal` with `knownSize = -1`.

**Called from**: `SessionSocketHandler.removeFileTarget`, `SessionSocketHandler.removeDirectoryTarget`, and various `op_*.go` handlers.

---

#### `(sm *SessionManager) trackChangeInternal`

```go
func (sm *SessionManager) trackChangeInternal(changeType ChangeType, monofsPath, origHash string, knownSize int64) error
```

**What it does**: Core change tracking logic. Acquires the write lock, validates session/DB exist, then switches on change type:

- **Delete/Rmdir**: If the entry was session-only (created or mkdired in this session), removes it from the overlay entirely (no phantom "[-]" entries for transient items). Otherwise marks it as deleted with the appropriate type.
- **Create/Modify/Mkdir**: If a `ChangeModify` comes in for a file already tracked as `ChangeCreate`, keeps it as `ChangeCreate`. Uses the fast path (caller-provided size) or falls back to `os.Lstat`. Creates a `FileEntry` and puts it in the DB.
- **Symlink**: No-op (handled by `CreateSymlink`).
- **UserRootDir**: Delegates to `PutUserDir`.
- **RemoveUserRootDir**: Delegates to `RemoveUserDir`.

---

#### `(sm *SessionManager) GetChanges`

```go
func (sm *SessionManager) GetChanges() []Change
```

**What it does**: Reconstructs the change list from the overlay database. Gets all file entries (filtering out baseline types) and all deleted entries, converting them to `Change` structs.

**Called from**: `SessionSocketHandler.handleStatus`, `SessionSocketHandler.handleAdd`, `SessionSocketHandler.handleRemove`, `SessionSocketHandler.handleDiff`, `SessionSocketHandler.handlePull`, `CommitManager.DryRun`.

---

#### `(sm *SessionManager) GetSessionInfo`

```go
func (sm *SessionManager) GetSessionInfo() (id string, createdAt time.Time, changeCount int, ok bool)
```

**What it does**: Returns session information for status display. Counts visible changes (excluding baseline type) from both file entries and deleted entries.

**Called from**: `SessionSocketHandler.handleStart`, `SessionSocketHandler.handleStatus`.

---

#### `generateSessionID`

```go
func generateSessionID() string
```

**What it does**: Creates a random UUID for session identification using `crypto/rand`. Falls back to `session-<UnixNano>` if random read fails.

**Called from**: `NewSessionManager` (via `StartSession`), and implicitly by `newLocalCommitID`.

---

#### `(sm *SessionManager) CreateUserRootDir`

```go
func (sm *SessionManager) CreateUserRootDir(name string) error
```

**What it does**: Creates a user directory at filesystem root. Makes the local directory (`MkdirAll`), records it as a user dir in the DB, and also records it as a directory file entry with `ChangeUserRootDir`.

**Called from**: `op_mkdir.go` (the Mkdir handler) when creating at root level.

---

#### `(sm *SessionManager) RemoveUserRootDir`

```go
func (sm *SessionManager) RemoveUserRootDir(name string) error
```

**What it does**: Removes a user-created root directory. Calls `os.RemoveAll` on the local path, removes the user dir record, and deletes the file entry.

**Called from**: `op_rmdir.go` (the Rmdir handler).

---

#### `(sm *SessionManager) IsUserRootDir`

```go
func (sm *SessionManager) IsUserRootDir(name string) bool
```

**What it does**: Checks if a name is a user-created root directory.

**Called from**: `MonoNode.ensureLocalCopyFor` (via `splitPath` check).

---

#### `(sm *SessionManager) ListUserRootDirs`

```go
func (sm *SessionManager) ListUserRootDirs() []string
```

**What it does**: Returns all active user-created root directories.

---

#### `(sm *SessionManager) CreateSymlink`

```go
func (sm *SessionManager) CreateSymlink(linkPath, target string) error
```

**What it does**: Creates a symlink. Makes parent directories, creates the symlink on disk via `os.Symlink`, and records it in the overlay DB with `FileEntrySymlink` type and `ChangeSymlink` change type.

**Called from**: `op_symlink.go` (the Symlink handler).

---

#### `(sm *SessionManager) GetSymlinkTarget`

```go
func (sm *SessionManager) GetSymlinkTarget(linkPath string) (string, bool)`
```

**What it does**: Returns the target of a symlink, or empty if not a symlink.

**Called from**: `CommitManager.commitSymlinkTarget`, `SessionSocketHandler.snapshotStagedEntry`.

---

#### `(sm *SessionManager) IsSymlink`

```go
func (sm *SessionManager) IsSymlink(monofsPath string) bool
```

**What it does**: Checks if a path is a symlink in the current session. Delegates to `GetSymlinkTarget`.

---

#### `(sm *SessionManager) GetOverlayDB`

```go
func (sm *SessionManager) GetOverlayDB() *OverlayDB
```

**What it does**: Returns the overlay database for direct access. Used by `OverlayManager` and `CommitManager`.

**Called from**: `OverlayManager.MergeReadDir`, `CommitManager.putOverlayFileFromDisk`, `CommitManager.reconcileCommittedEntry`, `SessionSocketHandler.resolveRemoveTarget`, `SessionSocketHandler.removeDirectoryTarget`.

---

#### `(sm *SessionManager) GetAllBlobFiles`

```go
func (sm *SessionManager) GetAllBlobFiles() map[string]BlobFileEntry
```

**What it does**: Returns all overlay entries whose monofs path starts with "dependency/". Includes regular files, symlinks, and directories.

**Called from**: `SessionSocketHandler.collectBlobFiles`, `SessionSocketHandler.handleBlobsInfo`.

---

#### `(sm *SessionManager) GetDeletedBlobPaths`

```go
func (sm *SessionManager) GetDeletedBlobPaths() []string
```

**What it does**: Returns monofs-relative paths of all dependency files marked as deleted. Strips the "dependency/" prefix from each path.

**Called from**: `SessionSocketHandler.handleUploadDeps`.

---

#### `(sm *SessionManager) RemoveBlobChanges`

```go
func (sm *SessionManager) RemoveBlobChanges() (int, error)
```

**What it does**: Removes all dependency-prefixed entries from the overlay DB. Handles both file entries (via `DeleteFile`) and deletion markers (via `UnmarkDeleted`). Also removes "dependency" from user root dirs. Does NOT touch disk files.

**Important**: Split into two phases with `RemoveBlobDisk`. The kernel must be told to forget dentries between phases.

**Called from**: `SessionSocketHandler.handleRemoveBlobChanges`.

---

#### `(sm *SessionManager) RemoveBlobDisk`

```go
func (sm *SessionManager) RemoveBlobDisk()
```

**What it does**: Bulk-removes the dependency/ subtree from disk. Atomically renames to a temp name first to prevent new FUSE writes, then calls `forceRemoveAll`. If rename fails, removes directly.

**Called from**: `SessionSocketHandler.handleRemoveBlobChanges`.

---

#### `(sm *SessionManager) GetDependencyFilePaths`

```go
func (sm *SessionManager) GetDependencyFilePaths(maxFiles int) []string
```

**What it does**: Returns a sample of dependency file paths (up to `maxFiles`) for backend verification before overlay cleanup. Used for atomic cleanup to ensure the backend has the files before removing overlay entries.

**Called from**: `SessionSocketHandler.handleRemoveBlobChanges`.

---

#### `forceRemoveFile`

```go
func forceRemoveFile(path string) error
```

**What it does**: Removes a single file. If the first attempt fails, makes the parent directory writable (0755) and retries. Go module cache files are 0444 inside 0555 directories — chmod-ing the parent dir is needed for unlink to succeed.

**Called from**: `SessionSocketHandler.removeFileTarget`.

---

#### `forceRemoveAll`

```go
func forceRemoveAll(root string) error
```

**What it does**: Like `os.RemoveAll` but first walks the tree and makes every directory writable (0755) so that files inside read-only dirs can be unlinked.

**Called from**: `SessionManager.RemoveBlobDisk`, `SessionSocketHandler.removeDirectoryTarget`.

---

## session_socket.go

Package fuse — FUSE filesystem layer for MonoFS.
Implements `SessionSocketHandler` for session management requests via Unix socket.

### Types

#### `SessionSocketHandler`

```go
type SessionSocketHandler struct {
    socketPath  string
    sessionMgr  *SessionManager
    commitMgr   *CommitManager
    principalID string
    ingester    BlobIngester
    deleter     BlobDeleter
    refresher   WorkspaceRefresher
    diffReader  DiffReader
    verifier    BackendVerifier
    attrCache   *cache.Cache
    rootNode    *MonoNode
    listener    net.Listener
    logger      *slog.Logger
    wg          sync.WaitGroup
    ctx         context.Context
    cancel      context.CancelFunc
}
```

Handles session management requests via Unix socket. Supports start, add, rm, status, branch, log, commit, pull, discard, push, push-blobs, blobs-info, and diff actions.

#### Interfaces

- `BlobIngester` — `IngestBlobs(ctx, files) (*BlobIngestResult, error)`
- `BlobDeleter` — `DeleteBlobs(ctx, paths) (*BlobDeleteResult, error)`
- `BlobDeleterFunc` — adapter function type for `BlobDeleter`
- `WorkspaceRefresher` — `RefreshWorkspaceRepositories(ctx, repos) (*WorkspaceRefreshResult, error)`
- `WorkspaceRefresherFunc` — adapter function type for `WorkspaceRefresher`
- `BackendVerifier` — `VerifyBlobs(ctx, paths) (bool, error)`
- `BackendVerifierFunc` — adapter function type for `BackendVerifier`
- `DiffReader` — `ReadOriginal(ctx, path) ([]byte, error)`
- `DiffReaderFunc` — adapter function type for `DiffReader`
- `BlobIngesterFunc` — adapter function type for `BlobIngester`

#### Result/Data Types

- `BlobIngestFile` — single file to ingest (Path, Content, Mode, FileType)
- `BlobIngestResult` — ingestion summary (FilesIngested, FilesFailed, FailedFiles)
- `BlobFailedFile` — per-file failure info (Path, Reason)
- `BlobFileInfo` — dependency file info (Path, Size)
- `BlobDeleteResult` — deletion summary (FilesDeleted, FilesFailed)
- `SessionRequest` — CLI request (Action, Path, Paths, BranchOp, BranchName, etc.)
- `SessionResponse` — CLI response (Success, SessionID, Changes, Message, Error, etc.)
- `FileDiff` — unified diff for a single file (Path, ChangeType, Repository, StorageID, Diff)
- `BlobsInfoData` — dependency info (TotalFiles, TotalBytes, Tools)
- `BlobsToolInfo` — per-tool info (Tool, Files, Bytes, FileList)
- `ChangeInfo` — change display info (Type, Path, Repository, StorageID, Timestamp)
- `LocalCommitInfo` — commit status summary (ID, ParentID, Message, Branch, Author, etc.)
- `BranchInfo` — logical branch summary (Name, Current, PendingCommits, HasMappings)
- `BranchMappingInfo` — branch-to-repo mapping (DisplayPath, OriginalBranch, ActualBranch, LastPushedCommit)
- `WorkspaceRef` — tracked workspace ref (DisplayPath, Ref, CommitHash)

#### Internal Types

- `sessionChangeScope` — enum: `sessionChangeWorkspace`, `sessionChangeBlob`, `sessionChangeExcluded`
- `classifiedSessionChange` — change + scope + repository/StorageID
- `removeTarget` — Path, LocalPath, IsDir, ShouldTrackDelete

### Functions

#### Setters

##### `(h *SessionSocketHandler) SetIngester`

```go
func (h *SessionSocketHandler) SetIngester(i BlobIngester)
```

Attaches a dependency ingester so push-deps can push files to the cluster.

##### `(h *SessionSocketHandler) SetDeleter`

```go
func (h *SessionSocketHandler) SetDeleter(d BlobDeleter)
```

Attaches a dependency deleter so push-deps can propagate deletions.

##### `(h *SessionSocketHandler) SetWorkspaceRefresher`

```go
func (h *SessionSocketHandler) SetWorkspaceRefresher(r WorkspaceRefresher)
```

Attaches a workspace refresher so pull can re-ingest source repositories.

##### `(h *SessionSocketHandler) SetDiffReader`

```go
func (h *SessionSocketHandler) SetDiffReader(dr DiffReader)
```

Attaches a reader for fetching original file content from the cluster.

##### `(h *SessionSocketHandler) SetAttrCache`

```go
func (h *SessionSocketHandler) SetAttrCache(c *cache.Cache)
```

Attaches a metadata cache so push can invalidate stale dependency entries.

##### `(h *SessionSocketHandler) SetRootNode`

```go
func (h *SessionSocketHandler) SetRootNode(n *MonoNode)
```

Attaches the FUSE root node so push can invalidate kernel dentry cache.

##### `(h *SessionSocketHandler) SetPrincipalID`

```go
func (h *SessionSocketHandler) SetPrincipalID(principalID string)
```

Records the mounted client identity used for branch scoping.

---

#### `NewSessionSocketHandler`

```go
func NewSessionSocketHandler(overlayDir string, sessionMgr *SessionManager, commitMgr *CommitManager, logger *slog.Logger) (*SessionSocketHandler, error)
```

**What it does**: Creates a new socket handler. Removes any existing socket file, creates a Unix domain socket at `overlayDir/session.sock`, sets permissions to 0600, creates a background context with cancel, and returns the handler.

**Called from**: Mount setup.

---

#### `(h *SessionSocketHandler) Start`

```go
func (h *SessionSocketHandler) Start()
```

**What it does**: Begins accepting connections by spawning the accept loop goroutine. Adds to WaitGroup so `Stop()` can wait for completion.

**Called from**: Mount setup.

---

#### `(h *SessionSocketHandler) Stop`

```go
func (h *SessionSocketHandler) Stop()
```

**What it does**: Cancels the context, closes the listener, waits for all goroutines to finish, and removes the socket file.

**Called from**: Mount teardown.

---

#### `(h *SessionSocketHandler) acceptLoop`

```go
func (h *SessionSocketHandler) acceptLoop()
```

**What it does**: Main accept loop. Accepts connections, spawns a goroutine for each via `handleConnection`. Exits when the context is cancelled.

---

#### `(h *SessionSocketHandler) handleConnection`

```go
func (h *SessionSocketHandler) handleConnection(conn net.Conn)
```

**What it does**: Handles a single connection. Reads a JSON `SessionRequest`, dispatches to the appropriate handler based on `req.Action`, and sends a JSON `SessionResponse`. Unknown actions return an error.

**Actions dispatched**:
- `"start"` → `handleStart`
- `"add"` → `handleAdd`
- `"rm"` → `handleRemove`
- `"status"` → `handleStatus`
- `"branch"` → `handleBranch`
- `"refs"` → `handleRefs`
- `"log"` → `handleLog`
- `"commit"` → `handleCommit`
- `"pull"`/`"refresh"` → `handlePull`
- `"discard"` → `handleDiscard`
- `"push"` → `handlePushSource`
- `"push-blobs"` → `handleUploadDeps`
- `"blobs-info"` → `handleBlobsInfo`
- `"diff"` → `handleDiff`

---

#### `(h *SessionSocketHandler) handleAdd`

```go
func (h *SessionSocketHandler) handleAdd(paths []string) SessionResponse
```

**What it does**: Stages source changes for the next commit. Validates paths, gets all changes, classifies them, selects workspace-visible changes matching the requested paths, snapshots each as a `StagedIndexEntry`, and persists them. Returns the count and list of staged changes.

---

#### `(h *SessionSocketHandler) handleRemove`

```go
func (h *SessionSocketHandler) handleRemove(paths []string) SessionResponse
```

**What it does**: Removes files/directories and stages the deletions. For each path: resolves the remove target, calls `removeFileTarget` or `removeDirectoryTarget`, and clears related staged entries. Then re-stages any remaining workspace changes matching the paths.

---

#### `(h *SessionSocketHandler) handleStart`

```go
func (h *SessionSocketHandler) handleStart() SessionResponse
```

**What it does**: Starts a new session (via `StartSession`) and returns the session ID and creation time.

---

#### `(h *SessionSocketHandler) handleStatus`

```go
func (h *SessionSocketHandler) handleStatus(showBlobs bool) SessionResponse
```

**What it does**: Returns the current session status. Classifies all changes into workspace, blob (dependency), and excluded scope. Compares workspace changes against staged entries to determine which are unstaged. Lists pending (un-pushed) local commits. If `showBlobs` is true, includes blob change details.

---

#### `(h *SessionSocketHandler) handleRefs`

```go
func (h *SessionSocketHandler) handleRefs() SessionResponse
```

**What it does**: Returns workspace ref info (DisplayPath, Ref, CommitHash) for all included repositories in the virtual monorepo manifest.

---

#### `(h *SessionSocketHandler) handleBranch`

```go
func (h *SessionSocketHandler) handleBranch(req SessionRequest) SessionResponse
```

**What it does**: Dispatches branch operations: `"show"` → `handleBranchShow`, `"create"` → `handleBranchCreate`, `"switch"` → `handleBranchSwitch`. Default branch op is "show".

---

#### `(h *SessionSocketHandler) handleBranchShow`

```go
func (h *SessionSocketHandler) handleBranchShow() SessionResponse
```

**What it does**: Shows current branch and lists all known logical branches with pending commit counts and mapping status, plus branch mapping details for the current branch.

---

#### `(h *SessionSocketHandler) handleBranchCreate`

```go
func (h *SessionSocketHandler) handleBranchCreate(rawBranchName string) SessionResponse
```

**What it does**: Creates a new logical branch. Validates the name, checks it doesn't already exist, seeds branch mappings from the workspace manifest, and switches to the new branch.

---

#### `(h *SessionSocketHandler) handleBranchSwitch`

```go
func (h *SessionSocketHandler) handleBranchSwitch(rawBranchName string) SessionResponse
```

**What it does**: Switches to an existing logical branch. Validates the name and that the branch exists. Working tree and index stay unchanged.

---

#### `(h *SessionSocketHandler) handleCommit`

```go
func (h *SessionSocketHandler) handleCommit(req SessionRequest) SessionResponse
```

**What it does**: Creates a local virtual commit via `CommitManager.CommitChanges`. Returns success with a formatted message including the commit ID and repository count.

---

#### `(h *SessionSocketHandler) handlePushSource`

```go
func (h *SessionSocketHandler) handlePushSource() SessionResponse
```

**What it does**: Pushes pending local commits via `CommitManager.PushPendingLocalCommits`.

---

#### `(h *SessionSocketHandler) handleLog`

```go
func (h *SessionSocketHandler) handleLog() SessionResponse
```

**What it does**: Lists all local virtual commits, newest first.

---

#### `(h *SessionSocketHandler) handlePull`

```go
func (h *SessionSocketHandler) handlePull() SessionResponse
```

**What it does**: Pulls (refreshes) workspace repositories. Validates no local changes or unpushed commits exist, collects pull repositories from the workspace manifest, calls the refresher, invalidates caches, and syncs the workspace git projection.

---

#### `(h *SessionSocketHandler) handleDiscard`

```go
func (h *SessionSocketHandler) handleDiscard() SessionResponse
```

**What it does**: Discards the current session via `SessionManager.DiscardSession`.

---

#### `(h *SessionSocketHandler) handleUploadDeps`

```go
func (h *SessionSocketHandler) handleUploadDeps() SessionResponse
```

**What it does**: Uploads dependency files to the cluster. Collects blob files for ingestion and deleted paths for propagation, calls the ingester and deleter, then calls `handleRemoveBlobChanges` to clean up the overlay.

---

#### `(h *SessionSocketHandler) handleRemoveBlobChanges`

```go
func (h *SessionSocketHandler) handleRemoveBlobChanges()
```

**What it does**: Cleans up dependency entries from the overlay after push. The order is critical for correctness:
1. Optional backend verification (`verifier.VerifyBlobs`) — ensures backend has files before cleanup.
2. DB cleanup (`RemoveBlobChanges`) — overlay entries removed.
3. Attr cache invalidation (`attrCache.InvalidatePrefix("dependency")`).
4. Kernel dentry invalidation (`invalidateDependencyTree`) — forces kernel to forget dependency dentries.
5. Disk cleanup (`RemoveBlobDisk`) — bulk-removes the dependency/ tree.
6. Mark push timestamp (`MarkDepsPushed`) — enables DIRECT_IO bypass.

---

#### `(h *SessionSocketHandler) invalidateDependencyTree`

```go
func (h *SessionSocketHandler) invalidateDependencyTree()
```

**What it does**: Walks the FUSE inode tree under root and removes all cached children under "dependency". First invalidates the top-level "dependency" entry, then walks depth-first, calling `RmChild` and `NotifyEntry` on each child. Recovers from panics for safety in unit test scenarios.

---

#### `(h *SessionSocketHandler) handleDiff`

```go
func (h *SessionSocketHandler) handleDiff(filterPath string, showBlobs bool) SessionResponse
```

**What it does**: Computes unified diffs for changed files. For each change:
- **Create**: diffs against empty.
- **Modify**: reads original content from the cluster via `diffReader`, reads overlay content from local path, computes unified diff. If original is not found, treats as create.
- **Delete**: reads original from cluster, diffs against empty. Drops files with identical content.

Separates dependency diffs from non-dependency diffs. If `filterPath` is set, only diffs that file.

---

#### `(h *SessionSocketHandler) classifySessionChange`

```go
func (h *SessionSocketHandler) classifySessionChange(ctx context.Context, change Change) classifiedSessionChange
```

**What it does**: Classifies a change as workspace, blob (dependency), or excluded. Dependency paths are blob. Otherwise resolves via workspace manifest: not included → excluded, included → workspace with repository info.

---

#### `sortClassifiedSessionChanges`

```go
func sortClassifiedSessionChanges(changes []classifiedSessionChange)
```

Sorts by scope, then repository, then path, then type.

#### `sortChangeInfos`

```go
func sortChangeInfos(changes []ChangeInfo)
```

Sorts by repository, then path, then type.

#### `sortLocalCommitInfos`

```go
func sortLocalCommitInfos(commits []LocalCommitInfo)
```

Sorts by creation time ascending, then ID ascending.

#### `sortLocalCommitInfosNewestFirst`

```go
func sortLocalCommitInfosNewestFirst(commits []LocalCommitInfo)
```

Sorts by creation time descending, then ID descending.

#### `sortBranchInfos`

```go
func sortBranchInfos(branches []BranchInfo)
```

Sorts with current branch first, then by name ascending.

#### `sortBranchMappingInfos`

```go
func sortBranchMappingInfos(mappings []BranchMappingInfo)
```

Sorts by DisplayPath ascending.

---

#### `changeInfoFromStagedEntry`

```go
func changeInfoFromStagedEntry(entry StagedIndexEntry) ChangeInfo
```

Converts a `StagedIndexEntry` to a `ChangeInfo` for display, formatting the stage time as `15:04:05`.

#### `localCommitInfoFromCommit`

```go
func localCommitInfoFromCommit(commit LocalVirtualCommit) LocalCommitInfo
```

Converts a `LocalVirtualCommit` to a `LocalCommitInfo` for display.

#### `localCommitOperationCount`

```go
func localCommitOperationCount(commit LocalVirtualCommit) int
```

Returns the total number of operations across all repositories in a commit.

#### `normalizeLogicalBranchName`

```go
func normalizeLogicalBranchName(rawBranchName string) (string, error)
```

**What it does**: Validates and normalizes a logical branch name. Rejects empty names, `"."`, `".."`, `"HEAD"`, names starting/ending with "/", names containing "//", names ending with "." or ".lock", names containing ".." or "@{", and names containing special characters (`~^:?*[\\`).

---

#### `(h *SessionSocketHandler) collectLogicalBranchState`

```go
func (h *SessionSocketHandler) collectLogicalBranchState(currentBranch string) ([]BranchInfo, []BranchMappingInfo, error)
```

**What it does**: Collects branch information for display. Iterates local commits to count pending commits per branch, iterates branch mappings to flag branches with mappings and collect current branch mapping details. Scoped to the handler's `principalID`.

---

#### `(h *SessionSocketHandler) seedLogicalBranchMappings`

```go
func (h *SessionSocketHandler) seedLogicalBranchMappings(branchName string) error
```

**What it does**: Creates initial branch mappings for all included workspace repositories when a new logical branch is created. Skips if principalID is empty or no workspace manifest.

---

#### `stagedEntriesEqual`

```go
func stagedEntriesEqual(staged, current StagedIndexEntry) bool
```

**What it does**: Compares two staged entries for equality across all fields (Path, RepositoryStorageID, RepositoryPath, ChangeType, Mode, SymlinkTarget, Content). Used to detect stale staged entries that need re-staging.

---

#### `normalizeRequestedSessionPaths`

```go
func normalizeRequestedSessionPaths(paths []string) ([]string, error)
```

**What it does**: Normalizes a list of paths. Trims whitespace and leading/trailing slashes, deduplicates, sorts by length (shortest first), and removes redundant paths (where one path is a prefix of another). Returns an error if no valid paths remain.

---

#### `(h *SessionSocketHandler) snapshotStagedEntry`

```go
func (h *SessionSocketHandler) snapshotStagedEntry(change classifiedSessionChange) (StagedIndexEntry, error)
```

**What it does**: Creates a `StagedIndexEntry` snapshot from a classified change. For `Create`/`Modify`: loads file content and mode. For `Delete`/`Rmdir`/`RemoveUserRootDir`: no content needed. For `Mkdir`/`UserRootDir`: captures mode. For `Symlink`: resolves the target from the change, session manager, or disk.

---

#### `(h *SessionSocketHandler) resolveRemoveTarget`

```go
func (h *SessionSocketHandler) resolveRemoveTarget(path string) (removeTarget, error)
```

**What it does**: Resolves what to remove for an "rm" action. Rejects dependency paths. Checks workspace manifest boundaries. Determines if the path needs a delete tracking marker by checking the overlay DB, staged entries, and backend existence. Handles the case where the path exists on disk but is not in the overlay.

---

#### `(h *SessionSocketHandler) removeFileTarget`

```go
func (h *SessionSocketHandler) removeFileTarget(target removeTarget) error
```

Removes a file from disk and tracks the delete in the session if needed.

#### `(h *SessionSocketHandler) removeDirectoryTarget`

```go
func (h *SessionSocketHandler) removeDirectoryTarget(target removeTarget) error
```

Removes a directory from disk, tracks the rmdir, and clears descendant overlay entries and delete markers.

#### `(h *SessionSocketHandler) clearStagedEntriesForPath`

```go
func (h *SessionSocketHandler) clearStagedEntriesForPath(path string) error
```

Removes all staged entries whose path matches the given path (including descendants).

---

#### `collectWorkspaceChangesForPaths`

```go
func collectWorkspaceChangesForPaths(paths []string, classified []classifiedSessionChange) []classifiedSessionChange
```

Selects workspace-scoped changes matching the given paths, deduplicating by path.

#### `(h *SessionSocketHandler) selectWorkspaceChangesForStaging`

```go
func (h *SessionSocketHandler) selectWorkspaceChangesForStaging(paths []string, classified []classifiedSessionChange) ([]classifiedSessionChange, error)
```

Selects workspace changes for staging. Supports `"."` to stage all. Returns errors for only-blob paths or excluded paths.

---

#### `(h *SessionSocketHandler) backendPathKind`

```go
func (h *SessionSocketHandler) backendPathKind(path string) (bool, bool, error)
```

Queries the backend for a path's existence and whether it's a directory. Used to determine if a path exists on the backend when resolving remove targets.

#### `requestedPathMatchesChange`

```go
func requestedPathMatchesChange(requested, changePath string) bool
```

Returns true if `changePath` equals `requested` or is a descendant (prefix match with trailing "/").

#### `sortFileDiffs`

```go
func sortFileDiffs(diffs []FileDiff)
```

Sorts by Repository, then Path, then ChangeType.

---

#### `(h *SessionSocketHandler) collectBlobFiles`

```go
func (h *SessionSocketHandler) collectBlobFiles() ([]BlobIngestFile, error)
```

**What it does**: Reads all overlay dependency entries and returns them as `BlobIngestFile` structs. Strips the "dependency/" prefix from paths. Skips `.info` files in Go module download cache (`@v/*.info`) because they are ephemeral metadata. Handles directories (zero-byte content with dir mode), symlinks (resolved to target content with original permissions), and regular files.

---

#### `(h *SessionSocketHandler) handleBlobsInfo`

```go
func (h *SessionSocketHandler) handleBlobsInfo() SessionResponse
```

**What it does**: Gathers dependency file information, grouping by tool (the second path component under "dependency/"). Returns per-tool file counts, byte totals, and file lists.

---

#### `splitN`

```go
func splitN(s, sep string, n int) []string
```

A simple `strings.SplitN` replacement that avoids importing `strings` just for `SplitN`. Splits `s` into at most `n` substrings separated by `sep`.

---

#### `(h *SessionSocketHandler) sendError`

```go
func (h *SessionSocketHandler) sendError(conn net.Conn, msg string)
```

Sends a `SessionResponse` with `Success: false` and the error message.

---

#### `formatCommitMessage`

```go
func formatCommitMessage(result *CommitResult) string
```

Formats a commit result message including repository count and upload/failure counts.

#### `formatWorkspacePullMessage`

```go
func formatWorkspacePullMessage(result *monoclient.WorkspaceRefreshResult) string
```

Formats a pull result message including requested, refreshed, and failed counts.

---

#### `(h *SessionSocketHandler) collectPullRepositories`

```go
func (h *SessionSocketHandler) collectPullRepositories(ctx context.Context) ([]monoclient.WorkspaceRepository, error)
```

Collects all included workspace repositories from the manifest for a pull operation.

#### `(h *SessionSocketHandler) invalidateWorkspaceAfterPull`

```go
func (h *SessionSocketHandler) invalidateWorkspaceAfterPull(repos []monoclient.WorkspaceRepository)
```

Invalidates workspace manifest cache, attribute cache (all entries + per-repo prefixes), and kernel dentry cache (namespace-level entries) after a pull.

#### `(h *SessionSocketHandler) appendWorkspaceGitSyncWarning`

```go
func (h *SessionSocketHandler) appendWorkspaceGitSyncWarning(message string, shouldSync bool) string
```

Appends a warning to the message if workspace git projection sync fails.

---

## session_vcs.go

Package fuse — VCS-related session types and methods (staged entries, local commits, branch mappings).

### Types

#### `StagedIndexEntry`

```go
type StagedIndexEntry struct {
    Path                string     `json:"path"`
    RepositoryStorageID string     `json:"repository_storage_id,omitempty"`
    RepositoryPath      string     `json:"repository_path,omitempty"`
    ChangeType          ChangeType `json:"change_type"`
    StagedAt            time.Time  `json:"staged_at"`
    Content             []byte     `json:"content,omitempty"`
    Mode                uint32     `json:"mode,omitempty"`
    SymlinkTarget       string     `json:"symlink_target,omitempty"`
    LocalPath           string     `json:"local_path,omitempty"`
}
```

A persisted snapshot of a source change selected for the next local virtual commit.

#### `LocalCommitOperation`

```go
type LocalCommitOperation struct {
    Kind    string `json:"kind"`
    Path    string `json:"path"`
    Mode    uint32 `json:"mode,omitempty"`
    Content []byte `json:"content,omitempty"`
    Target  string `json:"target,omitempty"`
}
```

A persisted repo-relative operation belonging to a local virtual commit.

#### `LocalCommitRepository`

```go
type LocalCommitRepository struct {
    StorageID   string                 `json:"storage_id"`
    DisplayPath string                 `json:"display_path"`
    RepoURL     string                 `json:"repo_url,omitempty"`
    Branch      string                 `json:"branch,omitempty"`
    BaseCommit  string                 `json:"base_commit,omitempty"`
    Operations  []LocalCommitOperation `json:"operations,omitempty"`
}
```

Groups operations for one repository inside a local virtual commit.

#### `LocalVirtualCommit`

```go
type LocalVirtualCommit struct {
    ID            string                  `json:"id"`
    ParentID      string                  `json:"parent_id,omitempty"`
    LogicalBranch string                  `json:"logical_branch,omitempty"`
    Message       string                  `json:"message"`
    AuthorName    string                  `json:"author_name,omitempty"`
    AuthorEmail   string                  `json:"author_email,omitempty"`
    PrincipalID   string                  `json:"principal_id,omitempty"`
    CreatedAt     time.Time               `json:"created_at"`
    Repositories  []LocalCommitRepository `json:"repositories,omitempty"`
    Pushed        bool                    `json:"pushed,omitempty"`
    PushJobID     string                  `json:"push_job_id,omitempty"`
    PushedAt      time.Time               `json:"pushed_at,omitempty"`
}
```

A session-local commit that has not necessarily been pushed upstream yet.

#### `SessionBranchMapping`

```go
type SessionBranchMapping struct {
    PrincipalID      string    `json:"principal_id"`
    LogicalBranch    string    `json:"logical_branch"`
    StorageID        string    `json:"storage_id"`
    DisplayPath      string    `json:"display_path,omitempty"`
    OriginalBranch   string    `json:"original_branch,omitempty"`
    ActualBranch     string    `json:"actual_branch"`
    LastPushedCommit string    `json:"last_pushed_commit,omitempty"`
    CreatedAt        time.Time `json:"created_at"`
}
```

Records the actual remote branch assigned to a logical branch for one repository and principal.

### Functions

All functions below are methods on `*SessionManager` that delegate to `*OverlayDB` methods after validating the session and database are active. Each uses a read lock (`RLock`) and returns a sentinel value if no session is active.

#### `(sm *SessionManager) PutStagedEntry`

```go
func (sm *SessionManager) PutStagedEntry(entry StagedIndexEntry) error
```

Persists a staged index entry. Sets `StagedAt` to UTC now if zero. Delegates to `odb.PutStagedEntry`.

#### `(sm *SessionManager) GetStagedEntry`

```go
func (sm *SessionManager) GetStagedEntry(path string) (StagedIndexEntry, bool, error)
```

Retrieves a staged entry by path. Delegates to `odb.GetStagedEntry`.

#### `(sm *SessionManager) ListStagedEntries`

```go
func (sm *SessionManager) ListStagedEntries() ([]StagedIndexEntry, error)
```

Lists all staged entries. Delegates to `odb.ListStagedEntries`.

#### `(sm *SessionManager) DeleteStagedEntry`

```go
func (sm *SessionManager) DeleteStagedEntry(path string) error
```

Deletes a staged entry. Delegates to `odb.DeleteStagedEntry`.

#### `(sm *SessionManager) ClearStagedEntries`

```go
func (sm *SessionManager) ClearStagedEntries() error
```

Clears all staged entries. Delegates to `odb.ClearStagedEntries`.

#### `(sm *SessionManager) PutLocalVirtualCommit`

```go
func (sm *SessionManager) PutLocalVirtualCommit(commit LocalVirtualCommit) error
```

Persists a local virtual commit. Sets `CreatedAt` to UTC now if zero. Delegates to `odb.PutLocalVirtualCommit`.

#### `(sm *SessionManager) GetLocalVirtualCommit`

```go
func (sm *SessionManager) GetLocalVirtualCommit(id string) (LocalVirtualCommit, bool, error)
```

Retrieves a local virtual commit by ID. Delegates to `odb.GetLocalVirtualCommit`.

#### `(sm *SessionManager) ListLocalVirtualCommits`

```go
func (sm *SessionManager) ListLocalVirtualCommits() ([]LocalVirtualCommit, error)
```

Lists all local virtual commits. Delegates to `odb.ListLocalVirtualCommits`.

#### `(sm *SessionManager) DeleteLocalVirtualCommit`

```go
func (sm *SessionManager) DeleteLocalVirtualCommit(id string) error
```

Deletes a local virtual commit. Delegates to `odb.DeleteLocalVirtualCommit`.

#### `(sm *SessionManager) SetCurrentLogicalBranch`

```go
func (sm *SessionManager) SetCurrentLogicalBranch(branch string) error
```

Sets the current logical branch. Delegates to `odb.SetCurrentLogicalBranch`.

#### `(sm *SessionManager) GetCurrentLogicalBranch`

```go
func (sm *SessionManager) GetCurrentLogicalBranch() (string, bool, error)
```

Gets the current logical branch. Delegates to `odb.GetCurrentLogicalBranch`.

#### `(sm *SessionManager) PutBranchMapping`

```go
func (sm *SessionManager) PutBranchMapping(mapping SessionBranchMapping) error
```

Persists a branch mapping. Delegates to `odb.PutBranchMapping`.

#### `(sm *SessionManager) GetBranchMapping`

```go
func (sm *SessionManager) GetBranchMapping(principalID, logicalBranch, storageID string) (SessionBranchMapping, bool, error)
```

Retrieves a branch mapping. Delegates to `odb.GetBranchMapping`.

#### `(sm *SessionManager) ListBranchMappings`

```go
func (sm *SessionManager) ListBranchMappings() ([]SessionBranchMapping, error)
```

Lists all branch mappings. Delegates to `odb.ListBranchMappings`.

#### `(sm *SessionManager) DeleteBranchMapping`

```go
func (sm *SessionManager) DeleteBranchMapping(principalID, logicalBranch, storageID string) error
```

Deletes a branch mapping. Delegates to `odb.DeleteBranchMapping`.

---

#### `sortLocalVirtualCommits`

```go
func sortLocalVirtualCommits(commits []LocalVirtualCommit)
```

Sorts commits by creation time ascending, breaking ties by ID ascending. Used in both `latestLocalCommitID` (commit.go) and `ListLocalVirtualCommits` (overlaydb_vcs.go).
