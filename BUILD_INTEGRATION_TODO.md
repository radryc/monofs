# MonoFS Build Integration — Implementation Guide

**Project Goal**: Enable build tools (Go, Bazel, npm, Maven, Cargo, pip) to read
modules/packages directly from MonoFS FUSE mount with zero network transfer and zero cache duplication.

**Status**: ALL PHASES NOT STARTED ⏸️

---

## Critical Design Principles

1. **All virtual paths are created on the BACKEND (server nodes), NOT in FUSE.**
   The FUSE layer is a dumb client — it calls `Lookup`, `ReadDir`, `Read` via gRPC.
   Virtual layout paths are **real entries** in the server's NutsDB databases (`repolookup`,
   `pathindex`, `dirindex`, `metadata`). FUSE doesn't know or care that they are "virtual".

2. **Modular plugin architecture.** Adding a new build system (npm, Maven, Cargo) requires
   implementing ONE interface (`LayoutMapper`) and registering it with ONE line.
   No FUSE, server, or core router code changes needed.

3. **Repo updates = re-ingest.** After a new commit/tag/version, the user runs `ingest`
   again. Re-ingestion is fast (metadata only — blob hashes, not file content). The layout
   mapper re-generates virtual entries for the updated version.

4. **Same blob hash = automatic dedup.** Virtual layout files point to the SAME `blob_hash`
   as the original repo file. The fetcher cache serves the content once regardless of how
   many virtual paths reference it.

---

## Architecture Overview

```
HOW IT WORKS:

EXISTING SYSTEM (already works):
  Admin CLI → Router.IngestRepository() → storage backend downloads ZIP/clones repo
    → WalkFiles() yields storage.FileMetadata (Path, ContentHash, Size, Mode, ModTime)
    → Router distributes files to server nodes via HRW sharding
    → Server stores in NutsDB: bucketMetadata, bucketPathIndex, bucketDirIndex, bucketRepoLookup
    → FUSE client calls Lookup/ReadDir/Read gRPC → server resolves path → fetcher returns blob

WHAT WE ADD (LayoutMapper):
  After ingestion completes, Router calls LayoutMapper.MapPaths():
    → Input:  RepoInfo + file list collected during WalkFiles()
    → Output: list of VirtualEntry{virtualDisplayPath, virtualFilePath, originalFilePath}
    → Router ingests these virtual entries using the EXISTING IngestFileBatch RPC
      (same blob_hash, same source/ref, different displayPath + storageID)
    → Server stores them in the SAME NutsDB buckets
    → FUSE sees them as regular repos — no FUSE code changes needed

RESULT:
  /mnt/monofs/
  ├── github.com/google/uuid@v1.6.0/       ← original (from ingestion)
  │   ├── uuid.go
  │   └── go.mod
  └── go-modules/pkg/mod/                   ← virtual (from LayoutMapper)
      └── github.com/google/uuid@v1.6.0/
          ├── uuid.go                       ← same blob_hash as original
          └── go.mod                        ← same blob_hash as original

  GOMODCACHE=/mnt/monofs/go-modules/pkg/mod go build ./...  ← works!
```

### Why This Design Works

| Aspect | This Design (Backend-side) |
|--------|---------------------------|
| Where virtual paths exist | In NutsDB on all server nodes (durable, survives restarts) |
| FUSE code changes | **ZERO** — FUSE is unchanged, sees regular repos |
| Multi-node clusters | Server nodes are authoritative, consistent across cluster |
| Search integration | Search already indexes all repos, including virtual ones |
| HRW sharding | Virtual files use different storageID, shard independently |
| Blob dedup | Same blob_hash → fetcher serves content once |
| Adding new build system | 1 file + 1 registration line, no other changes |

---

## Existing Code Reference

Before implementing, understand these key existing components:

### Router Ingestion Pipeline (`internal/router/ingest.go`)

The `IngestRepository()` method (line 71) does:
1. Parse source URL, determine displayPath and storageID
2. Create ingestion backend via `storage.DefaultRegistry`
3. Call `backend.Initialize()` then `backend.WalkFiles()`
4. For each file from WalkFiles: compute HRW shard key → send to target node
5. Files are batched by node and sent via `IngestFileBatch` RPC
6. After all files: call `BuildDirectoryIndexes` on each node
7. Call `MarkRepositoryOnboarded` on each node

**Key observation** (line 427-460): The WalkFiles callback receives `storage.FileMetadata`:
```go
backend.WalkFiles(ctx, func(meta storage.FileMetadata) error {
    // meta.Path        = "uuid.go"
    // meta.ContentHash = "abc123..."  (git blob hash or SHA-256)
    // meta.Size        = 1234
    // meta.Mode        = 0644
    // meta.ModTime     = time.Time{...}
    // meta.Metadata    = map[string]string{...}  (backend-specific)
    ...
})
```

The callback builds `*pb.FileMetadata` protobuf messages and groups them by target node.

### Server Storage (`internal/server/server.go`)

Bucket constants (line 88):
- `bucketMetadata` = "metadata" — file metadata by SHA-256 hash
- `bucketRepos` = "repos" — repository info by storageID
- `bucketPathIndex` = "pathindex" — path→hash mapping ("storageID:filePath")
- `bucketRepoLookup` = "repolookup" — displayPath→storageID mapping
- `bucketDirIndex` = "dirindex" — directory index ("storageID:sha256(dirPath)")
- `bucketOwnedFiles` = "ownedfiles" — files owned by node ("storageID:filePath")

**Key insight**: All these buckets are keyed by `storageID`. A virtual repo with a
different `storageID` is 100% isolated from the original. No conflicts possible.

### StorageID Generation (`internal/router/ingest.go`)

```go
storageID := generateStorageID(displayPath)  // SHA-256 hash of displayPath
```

A virtual displayPath like `go-modules/pkg/mod/github.com/google/uuid@v1.6.0`
generates a completely different storageID than `github.com/google/uuid@v1.6.0`.

### Existing Protobuf Messages (`api/proto/`)

`IngestFileBatchRequest` contains:
- `Files []*FileMetadata` — the file metadata to ingest
- `StorageId` — the storage ID for the repo
- `DisplayPath` — the display path
- `Source` — original source URL
- `Ref` — branch/tag/version

`FileMetadata` contains:
- `Path`, `StorageId`, `DisplayPath`, `Ref`, `Size`, `Mtime`, `Mode`
- `BlobHash` — the content hash (used by fetcher to retrieve content)
- `Source` — source URL (used by fetcher to locate repo/module)
- `SourceType` — "git" or "gomod" (tells fetcher which backend to use)
- `FetchType` — fetch backend type
- `BackendMetadata` — map[string]string for backend-specific data

**Critical for Go modules**: The fetcher's `GoModBackend.FetchBlob()` needs
`BackendMetadata` containing `module_path` and `version` to download the ZIP
and extract the file. This metadata is already set during normal gomod ingestion.
Virtual entries MUST copy this metadata from the original files.

### How Fetcher Resolves Content

When FUSE reads a file:
1. FUSE client calls server's `GetFile` or `Read` RPC
2. Server looks up `storedMetadata` from NutsDB (contains `BlobHash`, `RepoURL`, etc.)
3. Server calls fetcher with `BlobHash` + source info
4. Fetcher uses `BlobHash` to retrieve content from Git repo or Go module

For virtual files: same `BlobHash` + same `Source` + same `BackendMetadata`
= fetcher retrieves identical content. **No fetcher changes needed.**

---

## Key Interfaces

```go
// Package buildlayout defines the interface for build system layout mappers.
package buildlayout

// LayoutMapper transforms ingested repo files into virtual layout paths.
// Each build system implements this interface once.
//
// LIFECYCLE: Called by Router AFTER successful ingestion.
// INPUT:    Original repo info + file list from WalkFiles().
// OUTPUT:   List of VirtualEntry defining new displayPaths.
type LayoutMapper interface {
    // Type returns a unique identifier (e.g., "go", "bazel", "npm").
    Type() string

    // Matches returns true if this mapper should handle the given repo.
    // Called with the original ingestion parameters.
    Matches(info RepoInfo) bool

    // MapPaths generates virtual layout entries for a repo's files.
    // Each VirtualEntry creates a new displayPath in the server's NutsDB.
    // The virtual files reference the SAME blob_hash as the original.
    //
    // `files` is the complete list of files from the ingestion WalkFiles().
    // `info` contains displayPath, storageID, source, ref, ingestionType, fetchType.
    MapPaths(info RepoInfo, files []FileInfo) ([]VirtualEntry, error)

    // ParseDependencyFile reads a dependency manifest and returns dependencies.
    // Used by `ingest-deps` CLI to bulk-ingest all deps of a project.
    ParseDependencyFile(content []byte) ([]Dependency, error)
}

// RepoInfo contains the context about the repo being ingested.
type RepoInfo struct {
    DisplayPath   string            // e.g., "github.com/google/uuid@v1.6.0"
    StorageID     string            // SHA-256(DisplayPath)
    Source        string            // Original source URL/path
    Ref           string            // Branch, tag, or version
    IngestionType string            // "git", "go"
    FetchType     string            // "git", "gomod"
    Config        map[string]string // Backend-specific config
}

// FileInfo describes a file from WalkFiles().
type FileInfo struct {
    Path            string            // Relative path: "uuid.go", "internal/doc.go"
    BlobHash        string            // Content hash (from storage.FileMetadata.ContentHash)
    Size            uint64            // File size in bytes
    Mode            uint32            // Unix file mode
    Mtime           int64             // Modification time (Unix seconds)
    Source          string            // Source URL (copied from ingestion)
    BackendMetadata map[string]string // Backend-specific metadata (e.g., module_path, version)
}

// VirtualEntry is one virtual path the mapper wants to create.
type VirtualEntry struct {
    // VirtualDisplayPath is the new top-level repo path.
    // Example: "go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
    // This becomes a new entry in bucketRepoLookup on server nodes.
    VirtualDisplayPath string

    // VirtualFilePath is the file path within the virtual repo.
    // Usually same as OriginalFilePath.
    VirtualFilePath string

    // OriginalFilePath is the file path in the original repo.
    // Used to look up BlobHash, Size, Mode from the collected FileInfo list.
    OriginalFilePath string
}

// Dependency represents one dependency from a manifest file.
type Dependency struct {
    Module  string // e.g., "github.com/google/uuid"
    Version string // e.g., "v1.6.0"
    Source  string // Full source for ingestion: "github.com/google/uuid@v1.6.0"
}
```

---

## Phase 1: LayoutMapper Framework & Go Mapper

**Goal**: Create the framework AND Go module mapper. They compile and pass unit tests.
No integration with the router yet.

**Estimated effort**: 2-3 days

### Step 1.1: Create `internal/buildlayout/types.go`

Create the file with all types from "Key Interfaces" above plus the registry:

```go
// filepath: internal/buildlayout/types.go
package buildlayout

import "fmt"

// (paste LayoutMapper, RepoInfo, FileInfo, VirtualEntry, Dependency from above)

// LayoutMapperRegistry holds all registered layout mappers.
type LayoutMapperRegistry struct {
    mappers []LayoutMapper
}

// NewRegistry creates a new empty registry.
func NewRegistry() *LayoutMapperRegistry {
    return &LayoutMapperRegistry{}
}

// Register adds a layout mapper to the registry.
func (r *LayoutMapperRegistry) Register(m LayoutMapper) {
    r.mappers = append(r.mappers, m)
}

// MapAll runs all matching mappers and collects all virtual entries.
// Returns nil, nil if no mappers match (not an error).
func (r *LayoutMapperRegistry) MapAll(info RepoInfo, files []FileInfo) ([]VirtualEntry, error) {
    var allEntries []VirtualEntry
    for _, m := range r.mappers {
        if m.Matches(info) {
            entries, err := m.MapPaths(info, files)
            if err != nil {
                return nil, fmt.Errorf("layout mapper %q failed: %w", m.Type(), err)
            }
            allEntries = append(allEntries, entries...)
        }
    }
    return allEntries, nil
}

// HasMappers returns true if any mappers are registered.
func (r *LayoutMapperRegistry) HasMappers() bool {
    return len(r.mappers) > 0
}
```

**Lines**: ~100
**Depends on**: Nothing

### Step 1.2: Create `internal/buildlayout/types_test.go`

```go
// filepath: internal/buildlayout/types_test.go
package buildlayout

import "testing"

// mockMapper implements LayoutMapper for testing.
type mockMapper struct {
    typeStr    string
    matchAll   bool
    entries    []VirtualEntry
    shouldFail bool
}

func (m *mockMapper) Type() string { return m.typeStr }
func (m *mockMapper) Matches(info RepoInfo) bool { return m.matchAll }
func (m *mockMapper) MapPaths(info RepoInfo, files []FileInfo) ([]VirtualEntry, error) {
    if m.shouldFail {
        return nil, fmt.Errorf("mock error")
    }
    return m.entries, nil
}
func (m *mockMapper) ParseDependencyFile(content []byte) ([]Dependency, error) {
    return nil, nil
}

func TestRegistryEmpty(t *testing.T) {
    reg := NewRegistry()
    if reg.HasMappers() {
        t.Error("empty registry should have no mappers")
    }
    entries, err := reg.MapAll(RepoInfo{}, nil)
    if err != nil {
        t.Errorf("MapAll on empty registry should not error: %v", err)
    }
    if len(entries) != 0 {
        t.Errorf("expected 0 entries, got %d", len(entries))
    }
}

func TestRegistryMatchingMapper(t *testing.T) {
    reg := NewRegistry()
    reg.Register(&mockMapper{
        typeStr:  "test",
        matchAll: true,
        entries: []VirtualEntry{
            {VirtualDisplayPath: "virtual/repo", VirtualFilePath: "file.go", OriginalFilePath: "file.go"},
        },
    })

    entries, err := reg.MapAll(RepoInfo{DisplayPath: "test"}, nil)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(entries) != 1 {
        t.Fatalf("expected 1 entry, got %d", len(entries))
    }
    if entries[0].VirtualDisplayPath != "virtual/repo" {
        t.Errorf("wrong VirtualDisplayPath: %s", entries[0].VirtualDisplayPath)
    }
}

func TestRegistryNonMatchingMapper(t *testing.T) {
    reg := NewRegistry()
    reg.Register(&mockMapper{
        typeStr:  "test",
        matchAll: false, // doesn't match
        entries:  []VirtualEntry{{VirtualDisplayPath: "should-not-appear"}},
    })

    entries, err := reg.MapAll(RepoInfo{}, nil)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(entries) != 0 {
        t.Errorf("expected 0 entries from non-matching mapper, got %d", len(entries))
    }
}

func TestRegistryMapperError(t *testing.T) {
    reg := NewRegistry()
    reg.Register(&mockMapper{
        typeStr:    "failing",
        matchAll:   true,
        shouldFail: true,
    })

    _, err := reg.MapAll(RepoInfo{}, nil)
    if err == nil {
        t.Error("expected error from failing mapper")
    }
}

func TestRegistryMultipleMappers(t *testing.T) {
    reg := NewRegistry()
    reg.Register(&mockMapper{
        typeStr: "a", matchAll: true,
        entries: []VirtualEntry{{VirtualDisplayPath: "a/repo", VirtualFilePath: "f1", OriginalFilePath: "f1"}},
    })
    reg.Register(&mockMapper{
        typeStr: "b", matchAll: true,
        entries: []VirtualEntry{{VirtualDisplayPath: "b/repo", VirtualFilePath: "f2", OriginalFilePath: "f2"}},
    })

    entries, err := reg.MapAll(RepoInfo{}, nil)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(entries) != 2 {
        t.Fatalf("expected 2 entries, got %d", len(entries))
    }
}
```

**Lines**: ~100
**Run**: `go test -v ./internal/buildlayout/...`

### Step 1.3: Create `internal/buildlayout/golang/mapper.go`

This is the Go module layout mapper. Study the Go module cache layout:
`$GOMODCACHE/<module_path>@<version>/<file_path>`

Go's module cache uses **case encoding**: uppercase letters in module paths are
encoded as `!` + lowercase. Example: `github.com/Azure/go-autorest` →
`github.com/!azure/go-autorest` in the cache path.

```go
// filepath: internal/buildlayout/golang/mapper.go
package golang

import (
    "bufio"
    "bytes"
    "fmt"
    "strings"
    "unicode"

    "github.com/radryc/monofs/internal/buildlayout"
)

const (
    // GoModCachePrefix is the virtual mount prefix for Go module cache.
    // The full GOMODCACHE path will be: <mount>/go-modules/pkg/mod/
    GoModCachePrefix = "go-modules/pkg/mod"
)

// GoMapper implements LayoutMapper for Go modules.
type GoMapper struct{}

// NewGoMapper creates a new Go module layout mapper.
func NewGoMapper() *GoMapper {
    return &GoMapper{}
}

func (g *GoMapper) Type() string { return "go" }

// Matches returns true for Go module ingestions.
// Only matches when IngestionType is "go" — Git repos that happen to contain
// Go code are NOT matched (they don't need GOMODCACHE layout).
func (g *GoMapper) Matches(info buildlayout.RepoInfo) bool {
    return info.IngestionType == "go"
}

// MapPaths creates virtual entries under go-modules/pkg/mod/<module>@<version>/
//
// For a repo ingested as "github.com/google/uuid@v1.6.0" with files [uuid.go, go.mod]:
// Output:
//   VirtualEntry{
//     VirtualDisplayPath: "go-modules/pkg/mod/github.com/google/uuid@v1.6.0",
//     VirtualFilePath:    "uuid.go",
//     OriginalFilePath:   "uuid.go",
//   }
//
// The Go module cache uses case-insensitive encoding:
//   github.com/Azure/... → github.com/!azure/...
func (g *GoMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
    if len(files) == 0 {
        return nil, nil
    }

    // Parse module path and version from DisplayPath.
    // DisplayPath format: "github.com/google/uuid@v1.6.0"
    // OR without version if version is in Ref.
    modulePath, version := parseModuleVersion(info.DisplayPath, info.Ref)
    if modulePath == "" {
        return nil, fmt.Errorf("cannot parse module path from display path %q", info.DisplayPath)
    }
    if version == "" {
        return nil, fmt.Errorf("no version found for module %q (display_path=%q, ref=%q)",
            modulePath, info.DisplayPath, info.Ref)
    }

    // Apply Go module cache case encoding to module path.
    encodedModule := EncodePath(modulePath)

    // Virtual display path: go-modules/pkg/mod/<encoded_module>@<version>
    virtualDisplayPath := GoModCachePrefix + "/" + encodedModule + "@" + version

    entries := make([]buildlayout.VirtualEntry, 0, len(files))
    for _, f := range files {
        entries = append(entries, buildlayout.VirtualEntry{
            VirtualDisplayPath: virtualDisplayPath,
            VirtualFilePath:    f.Path,
            OriginalFilePath:   f.Path,
        })
    }

    return entries, nil
}

// ParseDependencyFile parses a go.mod file and returns all required dependencies.
//
// Handles:
//   - require ( ... ) blocks
//   - Single-line: require github.com/foo/bar v1.2.3
//   - Skips comments and "// indirect" markers (includes indirect deps)
//   - Skips replace/exclude/retract directives
func (g *GoMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
    var deps []buildlayout.Dependency
    scanner := bufio.NewScanner(bytes.NewReader(content))

    inRequireBlock := false

    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())

        // Skip empty lines and comments
        if line == "" || strings.HasPrefix(line, "//") {
            continue
        }

        // Handle require block start
        if strings.HasPrefix(line, "require (") || strings.HasPrefix(line, "require(") {
            inRequireBlock = true
            continue
        }

        // Handle block end
        if line == ")" {
            inRequireBlock = false
            continue
        }

        // Single-line require
        if strings.HasPrefix(line, "require ") && !inRequireBlock {
            dep := parseRequireLine(strings.TrimPrefix(line, "require "))
            if dep != nil {
                deps = append(deps, *dep)
            }
            continue
        }

        // Inside require block
        if inRequireBlock {
            dep := parseRequireLine(line)
            if dep != nil {
                deps = append(deps, *dep)
            }
        }
    }

    if err := scanner.Err(); err != nil {
        return nil, fmt.Errorf("error reading go.mod: %w", err)
    }

    return deps, nil
}

// parseRequireLine parses a single require line like:
//   github.com/google/uuid v1.6.0
//   github.com/google/uuid v1.6.0 // indirect
func parseRequireLine(line string) *buildlayout.Dependency {
    // Remove inline comments
    if idx := strings.Index(line, "//"); idx >= 0 {
        line = strings.TrimSpace(line[:idx])
    }
    if line == "" {
        return nil
    }

    parts := strings.Fields(line)
    if len(parts) < 2 {
        return nil
    }

    module := parts[0]
    version := parts[1]

    return &buildlayout.Dependency{
        Module:  module,
        Version: version,
        Source:  module + "@" + version,
    }
}

// parseModuleVersion extracts module path and version from a display path.
//
// Patterns:
//   "github.com/google/uuid@v1.6.0" → ("github.com/google/uuid", "v1.6.0")
//   "github.com/google/uuid" with ref="v1.6.0" → ("github.com/google/uuid", "v1.6.0")
func parseModuleVersion(displayPath, ref string) (modulePath, version string) {
    if idx := strings.LastIndex(displayPath, "@"); idx >= 0 {
        return displayPath[:idx], displayPath[idx+1:]
    }
    // No @ in displayPath — use ref as version
    return displayPath, ref
}

// EncodePath applies Go module cache case encoding.
// Uppercase letters are replaced with '!' + lowercase.
// Example: "github.com/Azure/go-autorest" → "github.com/!azure/go-autorest"
func EncodePath(s string) string {
    var buf strings.Builder
    buf.Grow(len(s))
    for _, r := range s {
        if unicode.IsUpper(r) {
            buf.WriteRune('!')
            buf.WriteRune(unicode.ToLower(r))
        } else {
            buf.WriteRune(r)
        }
    }
    return buf.String()
}

// DecodePath reverses EncodePath.
// Example: "github.com/!azure/go-autorest" → "github.com/Azure/go-autorest"
func DecodePath(s string) string {
    var buf strings.Builder
    buf.Grow(len(s))
    escape := false
    for _, r := range s {
        if escape {
            buf.WriteRune(unicode.ToUpper(r))
            escape = false
        } else if r == '!' {
            escape = true
        } else {
            buf.WriteRune(r)
        }
    }
    return buf.String()
}
```

**Lines**: ~180
**Depends on**: Step 1.1

### Step 1.4: Create `internal/buildlayout/golang/mapper_test.go`

```go
// filepath: internal/buildlayout/golang/mapper_test.go
package golang

import (
    "testing"

    "github.com/radryc/monofs/internal/buildlayout"
)

func TestGoMapper_Type(t *testing.T) {
    m := NewGoMapper()
    if m.Type() != "go" {
        t.Errorf("expected type 'go', got %q", m.Type())
    }
}

func TestGoMapper_Matches(t *testing.T) {
    m := NewGoMapper()

    tests := []struct {
        name          string
        ingestionType string
        want          bool
    }{
        {"go ingestion", "go", true},
        {"git ingestion", "git", false},
        {"empty", "", false},
        {"s3", "s3", false},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := m.Matches(buildlayout.RepoInfo{IngestionType: tt.ingestionType})
            if got != tt.want {
                t.Errorf("Matches() = %v, want %v", got, tt.want)
            }
        })
    }
}

func TestGoMapper_MapPaths_Standard(t *testing.T) {
    m := NewGoMapper()

    info := buildlayout.RepoInfo{
        DisplayPath:   "github.com/google/uuid@v1.6.0",
        StorageID:     "abc123",
        Source:        "github.com/google/uuid@v1.6.0",
        Ref:          "v1.6.0",
        IngestionType: "go",
        FetchType:     "gomod",
    }

    files := []buildlayout.FileInfo{
        {Path: "uuid.go", BlobHash: "hash1", Size: 100, Mode: 0644},
        {Path: "go.mod", BlobHash: "hash2", Size: 50, Mode: 0644},
        {Path: "internal/doc.go", BlobHash: "hash3", Size: 200, Mode: 0644},
    }

    entries, err := m.MapPaths(info, files)
    if err != nil {
        t.Fatalf("MapPaths failed: %v", err)
    }

    if len(entries) != 3 {
        t.Fatalf("expected 3 entries, got %d", len(entries))
    }

    expectedDisplayPath := "go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
    for i, e := range entries {
        if e.VirtualDisplayPath != expectedDisplayPath {
            t.Errorf("entry %d: VirtualDisplayPath = %q, want %q", i, e.VirtualDisplayPath, expectedDisplayPath)
        }
        if e.VirtualFilePath != files[i].Path {
            t.Errorf("entry %d: VirtualFilePath = %q, want %q", i, e.VirtualFilePath, files[i].Path)
        }
        if e.OriginalFilePath != files[i].Path {
            t.Errorf("entry %d: OriginalFilePath = %q, want %q", i, e.OriginalFilePath, files[i].Path)
        }
    }
}

func TestGoMapper_MapPaths_UppercaseModule(t *testing.T) {
    m := NewGoMapper()

    info := buildlayout.RepoInfo{
        DisplayPath:   "github.com/Azure/azure-sdk-for-go@v1.0.0",
        IngestionType: "go",
    }

    files := []buildlayout.FileInfo{
        {Path: "sdk.go", BlobHash: "h1"},
    }

    entries, err := m.MapPaths(info, files)
    if err != nil {
        t.Fatalf("MapPaths failed: %v", err)
    }

    // Azure → !azure in Go module cache
    expected := "go-modules/pkg/mod/github.com/!azure/azure-sdk-for-go@v1.0.0"
    if entries[0].VirtualDisplayPath != expected {
        t.Errorf("expected %q, got %q", expected, entries[0].VirtualDisplayPath)
    }
}

func TestGoMapper_MapPaths_VersionInRef(t *testing.T) {
    m := NewGoMapper()

    // DisplayPath has no @version, version is in Ref
    info := buildlayout.RepoInfo{
        DisplayPath:   "github.com/google/uuid",
        Ref:           "v1.6.0",
        IngestionType: "go",
    }

    files := []buildlayout.FileInfo{{Path: "uuid.go", BlobHash: "h1"}}

    entries, err := m.MapPaths(info, files)
    if err != nil {
        t.Fatalf("MapPaths failed: %v", err)
    }

    expected := "go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
    if entries[0].VirtualDisplayPath != expected {
        t.Errorf("expected %q, got %q", expected, entries[0].VirtualDisplayPath)
    }
}

func TestGoMapper_MapPaths_EmptyFiles(t *testing.T) {
    m := NewGoMapper()
    entries, err := m.MapPaths(buildlayout.RepoInfo{IngestionType: "go"}, nil)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(entries) != 0 {
        t.Errorf("expected 0 entries for empty files, got %d", len(entries))
    }
}

func TestGoMapper_MapPaths_NoVersion(t *testing.T) {
    m := NewGoMapper()
    _, err := m.MapPaths(buildlayout.RepoInfo{
        DisplayPath:   "github.com/google/uuid",
        Ref:           "", // no version
        IngestionType: "go",
    }, []buildlayout.FileInfo{{Path: "f.go"}})

    if err == nil {
        t.Error("expected error when no version is available")
    }
}

func TestGoMapper_ParseDependencyFile(t *testing.T) {
    gomod := []byte(`module myproject

go 1.21

require (
    github.com/google/uuid v1.6.0
    github.com/stretchr/testify v1.9.0 // indirect
    golang.org/x/sync v0.6.0
)

require github.com/pkg/errors v0.9.1

replace github.com/foo/bar => ../local

exclude github.com/old/thing v0.1.0
`)

    m := NewGoMapper()
    deps, err := m.ParseDependencyFile(gomod)
    if err != nil {
        t.Fatalf("ParseDependencyFile failed: %v", err)
    }

    if len(deps) != 4 {
        t.Fatalf("expected 4 deps, got %d: %+v", len(deps), deps)
    }

    expected := []buildlayout.Dependency{
        {Module: "github.com/google/uuid", Version: "v1.6.0", Source: "github.com/google/uuid@v1.6.0"},
        {Module: "github.com/stretchr/testify", Version: "v1.9.0", Source: "github.com/stretchr/testify@v1.9.0"},
        {Module: "golang.org/x/sync", Version: "v0.6.0", Source: "golang.org/x/sync@v0.6.0"},
        {Module: "github.com/pkg/errors", Version: "v0.9.1", Source: "github.com/pkg/errors@v0.9.1"},
    }

    for i, dep := range deps {
        if dep.Module != expected[i].Module {
            t.Errorf("dep %d: Module = %q, want %q", i, dep.Module, expected[i].Module)
        }
        if dep.Version != expected[i].Version {
            t.Errorf("dep %d: Version = %q, want %q", i, dep.Version, expected[i].Version)
        }
        if dep.Source != expected[i].Source {
            t.Errorf("dep %d: Source = %q, want %q", i, dep.Source, expected[i].Source)
        }
    }
}

func TestGoMapper_ParseDependencyFile_Empty(t *testing.T) {
    m := NewGoMapper()
    deps, err := m.ParseDependencyFile([]byte(`module myproject
go 1.21
`))
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(deps) != 0 {
        t.Errorf("expected 0 deps, got %d", len(deps))
    }
}

func TestEncodePath(t *testing.T) {
    tests := []struct {
        input string
        want  string
    }{
        {"github.com/google/uuid", "github.com/google/uuid"},
        {"github.com/Azure/go-autorest", "github.com/!azure/go-autorest"},
        {"github.com/BurntSushi/toml", "github.com/!burnt!sushi/toml"},
        {"", ""},
    }

    for _, tt := range tests {
        got := EncodePath(tt.input)
        if got != tt.want {
            t.Errorf("EncodePath(%q) = %q, want %q", tt.input, got, tt.want)
        }
    }
}

func TestDecodePath(t *testing.T) {
    tests := []struct {
        input string
        want  string
    }{
        {"github.com/google/uuid", "github.com/google/uuid"},
        {"github.com/!azure/go-autorest", "github.com/Azure/go-autorest"},
        {"github.com/!burnt!sushi/toml", "github.com/BurntSushi/toml"},
        {"", ""},
    }

    for _, tt := range tests {
        got := DecodePath(tt.input)
        if got != tt.want {
            t.Errorf("DecodePath(%q) = %q, want %q", tt.input, got, tt.want)
        }
    }
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
    paths := []string{
        "github.com/Azure/go-autorest",
        "github.com/BurntSushi/toml",
        "github.com/google/uuid",
        "golang.org/x/crypto",
    }

    for _, p := range paths {
        decoded := DecodePath(EncodePath(p))
        if decoded != p {
            t.Errorf("roundtrip failed: %q → encode → decode → %q", p, decoded)
        }
    }
}
```

**Lines**: ~220
**Run**: `go test -v ./internal/buildlayout/golang/...`

### Step 1.5: Wire Registry into Router (NO integration yet, just the field)

Add the layout registry field to the Router struct and a setter method.
This is a minimal change — no behavior change yet.

**File to modify**: `internal/router/router.go`

Add after the existing fields in the `Router` struct (around line 104):

```go
// Build layout mapper registry (for post-ingestion virtual path generation)
layoutRegistry *buildlayout.LayoutMapperRegistry
```

Add these methods anywhere in the file:

```go
// SetLayoutRegistry sets the layout mapper registry for post-ingestion hooks.
func (r *Router) SetLayoutRegistry(reg *buildlayout.LayoutMapperRegistry) {
    r.layoutRegistry = reg
}

// GetLayoutRegistry returns the layout mapper registry (may be nil).
func (r *Router) GetLayoutRegistry() *buildlayout.LayoutMapperRegistry {
    return r.layoutRegistry
}
```

Add import: `"github.com/radryc/monofs/internal/buildlayout"`

**File to modify**: `cmd/monofs-router/main.go`

Add after the existing storage backend registration (after the `init()` function
or in `main()` after creating the router):

```go
import (
    "github.com/radryc/monofs/internal/buildlayout"
    golangmapper "github.com/radryc/monofs/internal/buildlayout/golang"
)

// In main(), after router creation:
layoutRegistry := buildlayout.NewRegistry()
layoutRegistry.Register(golangmapper.NewGoMapper())
router.SetLayoutRegistry(layoutRegistry)
```

### Phase 1 Verification

```bash
# Must compile
make build

# Run new tests
go test -v ./internal/buildlayout/...
go test -v ./internal/buildlayout/golang/...

# Existing tests must still pass
go test -v ./internal/router/...
go test -v ./internal/server/...
go test -v ./internal/fuse/...
```

---

## Phase 2: Post-Ingestion Layout Hook

**Goal**: After successful ingestion, call LayoutMapper and ingest virtual entries
into the cluster using existing RPCs.

**Estimated effort**: 2-3 days

### Step 2.1: Create `internal/router/layout.go`

This file contains the orchestration logic. It is called from `IngestRepository()`
after the main ingestion completes successfully.

**Key design decision**: We collect `FileInfo` during the existing `WalkFiles()` loop
in `IngestRepository()` (Phase 2.3). This avoids an extra RPC round-trip to read
metadata back from server nodes.

```go
// filepath: internal/router/layout.go
package router

import (
    "context"
    "fmt"
    "log/slog"
    "time"

    pb "github.com/radryc/monofs/api/proto"
    "github.com/radryc/monofs/internal/buildlayout"
    "github.com/radryc/monofs/internal/sharding"
)

// generateLayouts is called after successful ingestion.
// It runs all matching layout mappers and ingests virtual entries as new repos.
//
// Failures are logged but NOT propagated — the original ingestion already succeeded.
// Virtual layout generation is best-effort.
func (r *Router) generateLayouts(
    ctx context.Context,
    info buildlayout.RepoInfo,
    files []buildlayout.FileInfo,
    sendProgress func(stage pb.IngestProgress_Stage, msg string),
) {
    if r.layoutRegistry == nil || !r.layoutRegistry.HasMappers() {
        return
    }

    start := time.Now()

    entries, err := r.layoutRegistry.MapAll(info, files)
    if err != nil {
        r.logger.Warn("layout mapper failed (non-fatal)",
            "display_path", info.DisplayPath,
            "error", err)
        return
    }

    if len(entries) == 0 {
        return
    }

    r.logger.Info("generating build layouts",
        "display_path", info.DisplayPath,
        "virtual_entries", len(entries))

    if sendProgress != nil {
        sendProgress(pb.IngestProgress_DISTRIBUTING,
            fmt.Sprintf("Generating %d virtual layout entries", len(entries)))
    }

    // Group entries by VirtualDisplayPath — each unique path becomes a virtual repo.
    grouped := make(map[string][]buildlayout.VirtualEntry)
    for _, e := range entries {
        grouped[e.VirtualDisplayPath] = append(grouped[e.VirtualDisplayPath], e)
    }

    // Build a lookup map from OriginalFilePath → FileInfo for fast access.
    fileIndex := make(map[string]*buildlayout.FileInfo, len(files))
    for i := range files {
        fileIndex[files[i].Path] = &files[i]
    }

    // Ingest each virtual repo.
    for virtualDisplayPath, virtualFiles := range grouped {
        if err := r.ingestVirtualRepo(ctx, info, virtualDisplayPath, virtualFiles, fileIndex); err != nil {
            r.logger.Warn("virtual repo ingestion failed (non-fatal)",
                "virtual_path", virtualDisplayPath,
                "error", err)
            // Continue with other virtual repos
        }
    }

    r.logger.Info("build layout generation complete",
        "display_path", info.DisplayPath,
        "virtual_repos", len(grouped),
        "duration", time.Since(start))
}

// ingestVirtualRepo registers a virtual repo on all nodes and distributes file metadata.
//
// This reuses the EXISTING IngestFileBatch and BuildDirectoryIndexes RPCs.
// The virtual files have the same BlobHash as the originals, so the fetcher
// serves identical content without duplication.
func (r *Router) ingestVirtualRepo(
    ctx context.Context,
    originalInfo buildlayout.RepoInfo,
    virtualDisplayPath string,
    virtualFiles []buildlayout.VirtualEntry,
    fileIndex map[string]*buildlayout.FileInfo,
) error {
    virtualStorageID := generateStorageID(virtualDisplayPath)

    r.logger.Info("ingesting virtual repo",
        "virtual_path", virtualDisplayPath,
        "virtual_storage_id", virtualStorageID,
        "files", len(virtualFiles))

    // Step 1: Get list of healthy nodes.
    r.mu.RLock()
    var healthyNodes []sharding.Node
    var nodeStates []*nodeState
    for id, ns := range r.nodes {
        if ns.status == NodeStatusActive || ns.status == NodeStatusOnboarding {
            healthyNodes = append(healthyNodes, sharding.Node{
                ID:     id,
                Weight: ns.weight,
            })
            nodeStates = append(nodeStates, ns)
        }
    }
    r.mu.RUnlock()

    if len(healthyNodes) == 0 {
        return fmt.Errorf("no healthy nodes available")
    }

    // Step 2: Register virtual repo on ALL healthy nodes.
    for _, ns := range nodeStates {
        regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
        _, err := ns.client.RegisterRepository(regCtx, &pb.RegisterRepositoryRequest{
            StorageId:   virtualStorageID,
            DisplayPath: virtualDisplayPath,
            Source:      originalInfo.Source,
            // Use original ingestion/fetch type so fetcher knows how to retrieve blobs.
            IngestionType: pb.IngestionType(pb.IngestionType_value["INGESTION_"+
                strings.ToUpper(originalInfo.IngestionType)]),
            FetchType: pb.FetchType(pb.FetchType_value["FETCH_"+
                strings.ToUpper(originalInfo.FetchType)]),
        })
        regCancel()

        if err != nil {
            r.logger.Warn("failed to register virtual repo on node",
                "node_id", ns.info.NodeId,
                "virtual_path", virtualDisplayPath,
                "error", err)
            // Non-fatal for individual node failures
        }
    }

    // Step 3: Build protobuf FileMetadata from VirtualEntry + original FileInfo.
    var batch []*pb.FileMetadata
    for _, vf := range virtualFiles {
        orig, ok := fileIndex[vf.OriginalFilePath]
        if !ok {
            r.logger.Debug("skipping virtual file: original not found",
                "original_path", vf.OriginalFilePath,
                "virtual_path", vf.VirtualFilePath)
            continue
        }

        fm := &pb.FileMetadata{
            Path:            virtualDisplayPath + "/" + vf.VirtualFilePath,
            StorageId:       virtualStorageID,
            DisplayPath:     virtualDisplayPath,
            Ref:             originalInfo.Ref,
            Size:            orig.Size,
            Mtime:           orig.Mtime,
            Mode:            orig.Mode,
            BlobHash:        orig.BlobHash,
            Source:          orig.Source,
            BackendMetadata: orig.BackendMetadata,
        }
        batch = append(batch, fm)
    }

    if len(batch) == 0 {
        return fmt.Errorf("no files could be mapped (all originals missing)")
    }

    // Step 4: Distribute files using HRW sharding (same as normal ingestion).
    sharder := sharding.NewHRW(healthyNodes)
    nodeBatches := make(map[string][]*pb.FileMetadata)
    for _, fm := range batch {
        shardKey := virtualStorageID + ":" + fm.Path
        targetNodes := sharder.GetNodes(shardKey, 1) // Primary only for virtual files
        if len(targetNodes) > 0 {
            nodeBatches[targetNodes[0].ID] = append(nodeBatches[targetNodes[0].ID], fm)
        }
    }

    // Step 5: Send batches to nodes (batch size 1000, same as normal ingestion).
    const batchSize = 1000
    for nodeID, fileBatch := range nodeBatches {
        ns := r.getNodeByID(nodeID)
        if ns == nil {
            continue
        }

        for i := 0; i < len(fileBatch); i += batchSize {
            end := i + batchSize
            if end > len(fileBatch) {
                end = len(fileBatch)
            }

            batchCtx, batchCancel := context.WithTimeout(ctx, 30*time.Second)
            _, err := ns.client.IngestFileBatch(batchCtx, &pb.IngestFileBatchRequest{
                Files:       fileBatch[i:end],
                StorageId:   virtualStorageID,
                DisplayPath: virtualDisplayPath,
                Source:      originalInfo.Source,
                Ref:         originalInfo.Ref,
            })
            batchCancel()

            if err != nil {
                r.logger.Warn("virtual batch ingest failed",
                    "node_id", nodeID,
                    "batch_start", i,
                    "batch_end", end,
                    "error", err)
            }
        }
    }

    // Step 6: Build directory indexes on all nodes that received files.
    for nodeID := range nodeBatches {
        ns := r.getNodeByID(nodeID)
        if ns == nil {
            continue
        }

        idxCtx, idxCancel := context.WithTimeout(ctx, 5*time.Minute)
        _, err := ns.client.BuildDirectoryIndexes(idxCtx, &pb.BuildDirectoryIndexesRequest{
            StorageId: virtualStorageID,
        })
        idxCancel()

        if err != nil {
            r.logger.Warn("virtual dir index build failed",
                "node_id", nodeID,
                "error", err)
        }
    }

    // Step 7: Mark virtual repo as onboarded on all nodes that have files.
    for nodeID := range nodeBatches {
        ns := r.getNodeByID(nodeID)
        if ns == nil {
            continue
        }

        onbCtx, onbCancel := context.WithTimeout(ctx, 10*time.Second)
        _, err := ns.client.MarkRepositoryOnboarded(onbCtx, &pb.MarkRepositoryOnboardedRequest{
            StorageId: virtualStorageID,
        })
        onbCancel()

        if err != nil {
            r.logger.Warn("virtual onboarding mark failed",
                "node_id", nodeID,
                "error", err)
        }
    }

    r.logger.Info("virtual repo ingested successfully",
        "virtual_path", virtualDisplayPath,
        "files", len(batch))

    return nil
}

// getNodeByID returns the nodeState for a given node ID, or nil.
// Caller must NOT hold r.mu.
func (r *Router) getNodeByID(nodeID string) *nodeState {
    r.mu.RLock()
    defer r.mu.RUnlock()
    return r.nodes[nodeID]
}
```

**Lines**: ~220
**Depends on**: Phase 1

**IMPORTANT NOTE FOR IMPLEMENTER**: Check what `sharding.NewHRW` is actually called
in this codebase. Look at `internal/sharding/` for the exact constructor name and
method signatures (`GetNodes`, `GetNode`, etc.). The code above uses placeholder
names — adapt to actual API. Also check if `r.nodes[nodeID]` returns `*nodeState`
with a `client` field of type `pb.MonoFSClient` and a `status` field, and what
the node status constants are named (`NodeStatusActive`, etc.). Look at the
existing `IngestRepository()` code in `internal/router/ingest.go` to see exactly
how it creates the sharder and accesses node clients.

Also check if `RegisterRepository` RPC takes `IngestionType` and `FetchType` as
protobuf enum values — the conversion above may need adjustment. Look at
`internal/router/ingest.go` line 337 for the existing usage pattern.

### Step 2.2: Modify `IngestRepository()` to Collect Files and Call Layout Hook

**File to modify**: `internal/router/ingest.go`

**Change 1**: Add a `collectedFiles` slice that is populated during the `WalkFiles()` loop.

Find the `WalkFiles()` callback (around line 427). Inside the callback, AFTER the
existing sharding/distribution code, add file collection:

```go
// Add this import at the top of the file:
import "github.com/radryc/monofs/internal/buildlayout"

// Add this variable BEFORE the WalkFiles call:
var collectedFiles []buildlayout.FileInfo

// Inside the WalkFiles callback, AFTER existing code that builds batches:
// (Add this at the end of the callback, before `return nil`)

    // Collect file info for layout generation (Phase 2)
    collectedFiles = append(collectedFiles, buildlayout.FileInfo{
        Path:            meta.Path,
        BlobHash:        meta.ContentHash,
        Size:            meta.Size,
        Mode:            meta.Mode,
        Mtime:           meta.ModTime.Unix(),
        Source:          sourceURL,
        BackendMetadata: meta.Metadata,
    })
```

**Change 2**: After the ingestion completes successfully (after `BuildDirectoryIndexes`
and `MarkRepositoryOnboarded` calls, near the end of the function), add:

```go
// Generate build layouts (virtual paths for build tools)
if r.layoutRegistry != nil && r.layoutRegistry.HasMappers() {
    layoutInfo := buildlayout.RepoInfo{
        DisplayPath:   displayPath,
        StorageID:     storageID,
        Source:        sourceURL,
        Ref:           ref,
        IngestionType: string(ingestionType),
        FetchType:     string(fetchType),
        Config:        req.IngestionConfig,
    }

    // sendProgress wrapper for layout generation
    layoutProgress := func(stage pb.IngestProgress_Stage, msg string) {
        if stream != nil {
            stream.Send(&pb.IngestProgress{
                Stage:   stage,
                Message: msg,
            })
        }
    }

    r.generateLayouts(stream.Context(), layoutInfo, collectedFiles, layoutProgress)
}
```

**HOW TO FIND THE RIGHT INSERTION POINTS**:

1. Open `internal/router/ingest.go`
2. Find `func (r *Router) IngestRepository(` (line 71)
3. Find `backend.WalkFiles(` — this is where the file iteration happens
4. Inside the `WalkFiles` callback, the last thing before `return nil` is where
   you add file collection
5. Find `BuildDirectoryIndexes` call near the end — the layout hook goes AFTER
   all the BuildDirectoryIndexes calls but BEFORE `return nil`

### Step 2.3: Add `strings` Import to `layout.go`

The `ingestVirtualRepo` function uses `strings.ToUpper`. Make sure this import
exists. (Listed here because it's easy to forget.)

### Phase 2 Verification

```bash
# Must compile
make build

# Existing tests must still pass
go test -v ./internal/router/...
go test -v ./internal/server/...

# Manual test with local cluster:
# 1. Start cluster (3 nodes + router + fetcher)
# 2. Ingest a Go module:
./bin/monofs-admin ingest \
  --router=localhost:9090 \
  --source=github.com/google/uuid@v1.6.0 \
  --ingestion-type=go --fetch-type=gomod

# 3. Check router logs for these messages:
#    "generating build layouts"
#    "ingesting virtual repo"
#    "virtual repo ingested successfully"

# 4. Verify virtual repo was registered:
./bin/monofs-admin repos --router=localhost:8080
# Expected: TWO repos:
#   display_path="github.com/google/uuid@v1.6.0"                     (original)
#   display_path="go-modules/pkg/mod/github.com/google/uuid@v1.6.0"  (virtual)

# 5. Mount FUSE and verify BOTH paths work:
mkdir -p /tmp/monofs-test
./bin/monofs-client --mount=/tmp/monofs-test --router=localhost:9090

ls /tmp/monofs-test/github.com/google/uuid@v1.6.0/
# Expected: uuid.go, go.mod, ...

ls /tmp/monofs-test/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/
# Expected: SAME files

cat /tmp/monofs-test/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/uuid.go
# Expected: actual Go source code

# 6. Verify content identity (same blob):
diff <(cat /tmp/monofs-test/github.com/google/uuid@v1.6.0/uuid.go) \
     <(cat /tmp/monofs-test/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/uuid.go)
# Expected: no differences

fusermount -u /tmp/monofs-test
```

---

## Phase 3: Bulk Dependency Ingestion (`ingest-deps` CLI)

**Goal**: Parse go.mod (or other manifest) and ingest ALL dependencies in one command.

**Estimated effort**: 1-2 days

**Note**: This phase does NOT depend on Phase 2. It only needs Phase 1's
`ParseDependencyFile()` method. But the virtual layout generation from Phase 2
makes it more useful (deps appear under `go-modules/pkg/mod/`).

### Step 3.1: Add `ingest-deps` Subcommand to Admin CLI

**File to modify**: `cmd/monofs-admin/main.go`

**HOW TO FIND WHERE TO ADD IT**: The admin CLI uses flag-based subcommand parsing.
Look at how `rebuildDirectoryIndex` (line 1184) or other commands are structured.
The main dispatch is in `main()` — look for a `switch` statement or `if` chain
on `os.Args[1]` or similar.

Add a new case for `"ingest-deps"`:

```go
case "ingest-deps":
    ingestDepsCmd := flag.NewFlagSet("ingest-deps", flag.ExitOnError)
    routerAddr := ingestDepsCmd.String("router", "localhost:9090", "Router gRPC address")
    filePath := ingestDepsCmd.String("file", "", "Path to dependency manifest (e.g., go.mod)")
    depType := ingestDepsCmd.String("type", "go", "Dependency type: go, bazel, npm")
    concurrency := ingestDepsCmd.Int("concurrency", 5, "Max concurrent ingestions")
    ingestDepsCmd.Parse(os.Args[2:])

    if *filePath == "" {
        fmt.Println("Error: --file is required")
        os.Exit(1)
    }

    if err := ingestDeps(*routerAddr, *filePath, *depType, *concurrency); err != nil {
        fmt.Printf("Error: %v\n", err)
        os.Exit(1)
    }
```

Add the `ingestDeps` function:

```go
import (
    golangmapper "github.com/radryc/monofs/internal/buildlayout/golang"
    "github.com/radryc/monofs/internal/buildlayout"
)

// ingestDeps reads a dependency manifest and ingests all dependencies.
func ingestDeps(routerAddr, filePath, depType string, concurrency int) error {
    // 1. Read the dependency file
    content, err := os.ReadFile(filePath)
    if err != nil {
        return fmt.Errorf("read dependency file: %w", err)
    }

    // 2. Select mapper by type
    mapper := selectMapper(depType)
    if mapper == nil {
        return fmt.Errorf("unknown dependency type: %q (supported: go)", depType)
    }

    // 3. Parse dependencies
    deps, err := mapper.ParseDependencyFile(content)
    if err != nil {
        return fmt.Errorf("parse dependencies: %w", err)
    }

    if len(deps) == 0 {
        fmt.Println("No dependencies found.")
        return nil
    }

    fmt.Printf("Found %d dependencies in %s\n\n", len(deps), filePath)

    // 4. Determine ingestion type and fetch type based on dep type
    ingestionType, fetchType := mapDepTypeToIngestionType(depType)

    // 5. Ingest each dependency with concurrency limit
    sem := make(chan struct{}, concurrency)
    var wg sync.WaitGroup
    var mu sync.Mutex
    var succeeded, failed, skipped int

    for _, dep := range deps {
        wg.Add(1)
        sem <- struct{}{}
        go func(d buildlayout.Dependency) {
            defer wg.Done()
            defer func() { <-sem }()

            fmt.Printf("  Ingesting %s@%s ...\n", d.Module, d.Version)

            // Call the existing ingest function (reuse existing admin CLI code)
            // The source format depends on dep type:
            //   Go: "github.com/google/uuid@v1.6.0"
            err := ingestSingleDep(routerAddr, d.Source, ingestionType, fetchType)

            mu.Lock()
            defer mu.Unlock()
            if err != nil {
                fmt.Printf("  ❌ %s@%s: %v\n", d.Module, d.Version, err)
                failed++
            } else {
                fmt.Printf("  ✅ %s@%s\n", d.Module, d.Version)
                succeeded++
            }
        }(dep)
    }
    wg.Wait()

    fmt.Printf("\n=== Results ===\n")
    fmt.Printf("Succeeded: %d\n", succeeded)
    fmt.Printf("Failed:    %d\n", failed)
    fmt.Printf("Skipped:   %d\n", skipped)
    fmt.Printf("Total:     %d\n", len(deps))
    return nil
}

// selectMapper returns the appropriate LayoutMapper for a dep type.
func selectMapper(depType string) buildlayout.LayoutMapper {
    switch depType {
    case "go":
        return golangmapper.NewGoMapper()
    default:
        return nil
    }
}

// mapDepTypeToIngestionType returns the ingestion and fetch types for a dep type.
func mapDepTypeToIngestionType(depType string) (string, string) {
    switch depType {
    case "go":
        return "go", "gomod"
    default:
        return "git", "git"
    }
}

// ingestSingleDep calls the router's IngestRepository RPC for one dependency.
func ingestSingleDep(routerAddr, source, ingestionType, fetchType string) error {
    // Connect to router
    conn, err := grpc.Dial(routerAddr, grpc.WithInsecure())
    if err != nil {
        return fmt.Errorf("connect: %w", err)
    }
    defer conn.Close()

    client := pb.NewMonoFSRouterClient(conn)

    // Build ingest request
    req := &pb.IngestRequest{
        Source: source,
    }

    // Set ingestion type
    switch ingestionType {
    case "go":
        req.IngestionType = pb.IngestionType_INGESTION_GO
    default:
        req.IngestionType = pb.IngestionType_INGESTION_GIT
    }

    // Set fetch type
    switch fetchType {
    case "gomod":
        req.FetchType = pb.FetchType_FETCH_GOMOD
    default:
        req.FetchType = pb.FetchType_FETCH_GIT
    }

    // Call IngestRepository (streaming RPC)
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()

    stream, err := client.IngestRepository(ctx, req)
    if err != nil {
        return fmt.Errorf("ingest: %w", err)
    }

    // Read progress until done
    for {
        progress, err := stream.Recv()
        if err != nil {
            if err == io.EOF {
                break
            }
            return fmt.Errorf("receive progress: %w", err)
        }
        if progress.Stage == pb.IngestProgress_FAILED {
            return fmt.Errorf("ingestion failed: %s", progress.Message)
        }
        if progress.Stage == pb.IngestProgress_COMPLETE {
            break
        }
    }

    return nil
}
```

**IMPORTANT FOR IMPLEMENTER**: The `ingestSingleDep` function above creates a new
gRPC connection per dependency. For better performance, you should create ONE
connection and reuse it for all dependencies. Refactor to pass the `pb.MonoFSRouterClient`
to the goroutines instead of the `routerAddr` string.

Also check what `pb.IngestionType_INGESTION_GO` is actually called in the protobuf
definitions. Look at `api/proto/` for the exact enum value names.

### Phase 3 Verification

```bash
make build

# Create test go.mod
cat > /tmp/test-go.mod << 'EOF'
module testproject

go 1.21

require (
    github.com/google/uuid v1.6.0
    golang.org/x/sync v0.6.0
)
EOF

# Run ingest-deps
./bin/monofs-admin ingest-deps \
  --router=localhost:9090 \
  --file=/tmp/test-go.mod \
  --type=go \
  --concurrency=2

# Expected output:
#   Found 2 dependencies in /tmp/test-go.mod
#   Ingesting github.com/google/uuid@v1.6.0 ...
#   ✅ github.com/google/uuid@v1.6.0
#   Ingesting golang.org/x/sync@v0.6.0 ...
#   ✅ golang.org/x/sync@v0.6.0
#   === Results ===
#   Succeeded: 2
#   Failed:    0
#   Total:     2
```

---

## Phase 4: End-to-End Go Build Test

**Goal**: Prove `go build` works with MonoFS as `GOMODCACHE`.

**Estimated effort**: 1 day

### Step 4.1: Manual End-to-End Test Script

Create a test script:

```bash
#!/bin/bash
# filepath: scripts/test-go-build.sh
# Test that go build works with MonoFS GOMODCACHE

set -e

MOUNT_POINT="${1:-/tmp/monofs-go-test}"
ROUTER="${2:-localhost:9090}"
PROJECT_DIR=$(mktemp -d)

echo "=== Step 1: Create test Go project ==="
mkdir -p "$PROJECT_DIR"
cat > "$PROJECT_DIR/go.mod" << 'EOF'
module testproject

go 1.21

require github.com/google/uuid v1.6.0
EOF

cat > "$PROJECT_DIR/main.go" << 'EOF'
package main

import (
    "fmt"
    "github.com/google/uuid"
)

func main() {
    fmt.Println(uuid.New())
}
EOF

echo "=== Step 2: Ingest dependency ==="
./bin/monofs-admin ingest-deps \
  --router="$ROUTER" \
  --file="$PROJECT_DIR/go.mod" \
  --type=go

echo "=== Step 3: Mount FUSE ==="
mkdir -p "$MOUNT_POINT"
./bin/monofs-client --mount="$MOUNT_POINT" --router="$ROUTER" &
CLIENT_PID=$!
sleep 3

echo "=== Step 4: Verify module cache structure ==="
ls "$MOUNT_POINT/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/"

echo "=== Step 5: Build with MonoFS GOMODCACHE ==="
GOMODCACHE="$MOUNT_POINT/go-modules/pkg/mod" \
GONOSUMDB='*' \
GONOSUMCHECK='*' \
GOFLAGS='-mod=mod' \
GOPATH=$(mktemp -d) \
  go build -v -o /dev/null "$PROJECT_DIR/..."

echo "=== Step 6: Cleanup ==="
fusermount -u "$MOUNT_POINT" 2>/dev/null || true
kill $CLIENT_PID 2>/dev/null || true
rm -rf "$PROJECT_DIR"

echo ""
echo "✅ Go build with MonoFS GOMODCACHE succeeded!"
```

### Step 4.2: Integration Test (Optional)

**File**: `test/build_go_integration_test.go`

This test is heavier (requires running cluster, FUSE, network access to Go proxy).
Skip in short mode.

```go
// filepath: test/build_go_integration_test.go
package test

import (
    "os"
    "os/exec"
    "testing"
)

func TestGoBuildWithMonoFS(t *testing.T) {
    if testing.Short() {
        t.Skip("requires running cluster and FUSE")
    }

    // Check if cluster is running
    if os.Getenv("MONOFS_ROUTER") == "" {
        t.Skip("set MONOFS_ROUTER to run this test")
    }

    // Run the test script
    cmd := exec.Command("bash", "scripts/test-go-build.sh")
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr

    if err := cmd.Run(); err != nil {
        t.Fatalf("Go build test failed: %v", err)
    }
}
```

---

## Phase 5: Bazel Layout Mapper

**Goal**: Add Bazel mapper to prove modularity. Zero changes to FUSE/server/router core.

**Estimated effort**: 1-2 days

### Step 5.1: Create `internal/buildlayout/bazel/mapper.go`

```go
// filepath: internal/buildlayout/bazel/mapper.go
package bazel

import (
    "fmt"
    "path"
    "regexp"
    "strings"

    "github.com/radryc/monofs/internal/buildlayout"
)

// BazelMapper implements LayoutMapper for Bazel repositories.
type BazelMapper struct{}

func NewBazelMapper() *BazelMapper {
    return &BazelMapper{}
}

func (b *BazelMapper) Type() string { return "bazel" }

// Matches returns true for git repos (Bazel repos are git repos).
// The actual check for Bazel files happens in MapPaths (returns empty if not Bazel).
func (b *BazelMapper) Matches(info buildlayout.RepoInfo) bool {
    return info.IngestionType == "git"
}

// MapPaths creates entries under bazel-repos/<module_name>@<version>/
// Only generates entries if the repo contains MODULE.bazel, WORKSPACE, or WORKSPACE.bazel.
func (b *BazelMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
    if len(files) == 0 {
        return nil, nil
    }

    // Check if this repo has Bazel build files.
    hasBazel := false
    for _, f := range files {
        base := path.Base(f.Path)
        if base == "MODULE.bazel" || base == "WORKSPACE" || base == "WORKSPACE.bazel" {
            if f.Path == base { // Only root-level files count
                hasBazel = true
                break
            }
        }
    }
    if !hasBazel {
        return nil, nil // Not a Bazel repo, skip silently
    }

    // Extract module name from display path.
    // "github.com/bazelbuild/rules_go" → "rules_go"
    moduleName := path.Base(strings.TrimSuffix(info.DisplayPath, "/"))

    // Version from ref, default to "latest"
    version := info.Ref
    if version == "" || version == "main" || version == "master" {
        version = "latest"
    }

    virtualDisplayPath := fmt.Sprintf("bazel-repos/%s@%s", moduleName, version)

    entries := make([]buildlayout.VirtualEntry, 0, len(files))
    for _, f := range files {
        entries = append(entries, buildlayout.VirtualEntry{
            VirtualDisplayPath: virtualDisplayPath,
            VirtualFilePath:    f.Path,
            OriginalFilePath:   f.Path,
        })
    }

    return entries, nil
}

// bazelDepRegex matches bazel_dep(name = "...", version = "...") in MODULE.bazel.
var bazelDepRegex = regexp.MustCompile(
    `bazel_dep\s*\(\s*name\s*=\s*"([^"]+)"\s*,\s*version\s*=\s*"([^"]+)"\s*\)`,
)

// ParseDependencyFile parses MODULE.bazel for bazel_dep declarations.
func (b *BazelMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
    matches := bazelDepRegex.FindAllStringSubmatch(string(content), -1)

    deps := make([]buildlayout.Dependency, 0, len(matches))
    for _, m := range matches {
        deps = append(deps, buildlayout.Dependency{
            Module:  m[1],
            Version: m[2],
            Source:  m[1] + "@" + m[2],
        })
    }

    return deps, nil
}
```

**Lines**: ~100

### Step 5.2: Create `internal/buildlayout/bazel/mapper_test.go`

```go
// filepath: internal/buildlayout/bazel/mapper_test.go
package bazel

import (
    "testing"

    "github.com/radryc/monofs/internal/buildlayout"
)

func TestBazelMapper_Type(t *testing.T) {
    m := NewBazelMapper()
    if m.Type() != "bazel" {
        t.Errorf("expected 'bazel', got %q", m.Type())
    }
}

func TestBazelMapper_Matches(t *testing.T) {
    m := NewBazelMapper()
    if !m.Matches(buildlayout.RepoInfo{IngestionType: "git"}) {
        t.Error("should match git repos")
    }
    if m.Matches(buildlayout.RepoInfo{IngestionType: "go"}) {
        t.Error("should not match go repos")
    }
}

func TestBazelMapper_MapPaths_WithModuleBazel(t *testing.T) {
    m := NewBazelMapper()

    info := buildlayout.RepoInfo{
        DisplayPath:   "github.com/bazelbuild/rules_go",
        Ref:           "v0.50.0",
        IngestionType: "git",
    }

    files := []buildlayout.FileInfo{
        {Path: "MODULE.bazel", BlobHash: "h1"},
        {Path: "BUILD.bazel", BlobHash: "h2"},
        {Path: "go/BUILD.bazel", BlobHash: "h3"},
    }

    entries, err := m.MapPaths(info, files)
    if err != nil {
        t.Fatalf("MapPaths failed: %v", err)
    }

    if len(entries) != 3 {
        t.Fatalf("expected 3 entries, got %d", len(entries))
    }

    expected := "bazel-repos/rules_go@v0.50.0"
    if entries[0].VirtualDisplayPath != expected {
        t.Errorf("VirtualDisplayPath = %q, want %q", entries[0].VirtualDisplayPath, expected)
    }
}

func TestBazelMapper_MapPaths_NotBazelRepo(t *testing.T) {
    m := NewBazelMapper()

    info := buildlayout.RepoInfo{
        DisplayPath:   "github.com/google/uuid",
        Ref:           "main",
        IngestionType: "git",
    }

    files := []buildlayout.FileInfo{
        {Path: "uuid.go", BlobHash: "h1"},
        {Path: "go.mod", BlobHash: "h2"},
    }

    entries, err := m.MapPaths(info, files)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(entries) != 0 {
        t.Errorf("non-Bazel repo should return 0 entries, got %d", len(entries))
    }
}

func TestBazelMapper_MapPaths_DefaultVersion(t *testing.T) {
    m := NewBazelMapper()

    entries, _ := m.MapPaths(
        buildlayout.RepoInfo{DisplayPath: "github.com/test/repo", Ref: "main", IngestionType: "git"},
        []buildlayout.FileInfo{{Path: "WORKSPACE", BlobHash: "h1"}},
    )

    if len(entries) == 0 {
        t.Fatal("expected entries for WORKSPACE repo")
    }
    if entries[0].VirtualDisplayPath != "bazel-repos/repo@latest" {
        t.Errorf("expected 'latest' version, got %q", entries[0].VirtualDisplayPath)
    }
}

func TestBazelMapper_ParseDependencyFile(t *testing.T) {
    moduleBazel := []byte(`
module(
    name = "rules_go",
    version = "0.50.0",
)

bazel_dep(name = "bazel_skylib", version = "1.7.1")
bazel_dep(name = "platforms", version = "0.0.10")
bazel_dep(name = "rules_proto", version = "6.0.2")

# This is a comment
bazel_dep(name = "protobuf", version = "27.1")
`)

    m := NewBazelMapper()
    deps, err := m.ParseDependencyFile(moduleBazel)
    if err != nil {
        t.Fatalf("ParseDependencyFile failed: %v", err)
    }

    if len(deps) != 4 {
        t.Fatalf("expected 4 deps, got %d: %+v", len(deps), deps)
    }

    expected := []struct{ name, version string }{
        {"bazel_skylib", "1.7.1"},
        {"platforms", "0.0.10"},
        {"rules_proto", "6.0.2"},
        {"protobuf", "27.1"},
    }

    for i, d := range deps {
        if d.Module != expected[i].name {
            t.Errorf("dep %d: Module = %q, want %q", i, d.Module, expected[i].name)
        }
        if d.Version != expected[i].version {
            t.Errorf("dep %d: Version = %q, want %q", i, d.Version, expected[i].version)
        }
    }
}

func TestBazelMapper_ParseDependencyFile_Empty(t *testing.T) {
    m := NewBazelMapper()
    deps, err := m.ParseDependencyFile([]byte(`module(name = "test", version = "1.0")`))
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(deps) != 0 {
        t.Errorf("expected 0 deps, got %d", len(deps))
    }
}
```

**Lines**: ~140

### Step 5.3: Register Bazel Mapper

**File to modify**: `cmd/monofs-router/main.go`

Add one line after Go mapper registration:

```go
import bazelmapper "github.com/radryc/monofs/internal/buildlayout/bazel"

// After: layoutRegistry.Register(golangmapper.NewGoMapper())
layoutRegistry.Register(bazelmapper.NewBazelMapper())
```

Also update `selectMapper` in `cmd/monofs-admin/main.go`:

```go
case "bazel":
    return bazelmapper.NewBazelMapper()
```

### Phase 5 Verification

```bash
make build

go test -v ./internal/buildlayout/bazel/...

# Manual test:
./bin/monofs-admin ingest \
  --router=localhost:9090 \
  --source=https://github.com/bazelbuild/rules_go \
  --ref=v0.50.0

# Mount and verify
mkdir -p /tmp/monofs-test
./bin/monofs-client --mount=/tmp/monofs-test --router=localhost:9090 &
sleep 3

ls /tmp/monofs-test/bazel-repos/rules_go@v0.50.0/
# Expected: MODULE.bazel, BUILD.bazel, go/, ...

fusermount -u /tmp/monofs-test
```

---

## Phase 6: Re-Ingestion & Version Management

**Goal**: When a repo is re-ingested (new version or updated ref), layouts are
regenerated correctly. Multiple versions coexist.

**Estimated effort**: 1 day

### How Re-Ingestion Already Works

The existing `IngestRepository()` handles re-ingestion:
- If the same `storageID` already exists, files are updated in-place
- `IngestFileBatch` overwrites existing metadata
- `BuildDirectoryIndexes` rebuilds the directory tree from scratch

### What Happens with Virtual Repos

Each version has a **different** `displayPath` and therefore a **different** `storageID`:

| Module | DisplayPath | StorageID | Virtual DisplayPath |
|--------|------------|-----------|-------------------|
| uuid v1.6.0 | `github.com/google/uuid@v1.6.0` | `sha256("github.com/google/uuid@v1.6.0")` | `go-modules/pkg/mod/github.com/google/uuid@v1.6.0` |
| uuid v1.5.0 | `github.com/google/uuid@v1.5.0` | `sha256("github.com/google/uuid@v1.5.0")` | `go-modules/pkg/mod/github.com/google/uuid@v1.5.0` |

**Result**: Multiple versions naturally coexist because they have different storageIDs.
No cleanup needed for version management.

### Re-Ingesting Same Version

If you re-ingest `github.com/google/uuid@v1.6.0` (same version, maybe updated content):
1. Original repo is updated (same storageID, files overwritten)
2. Layout hook runs again → generates same virtual displayPath → same virtual storageID
3. `IngestFileBatch` overwrites virtual metadata
4. `BuildDirectoryIndexes` rebuilds virtual directory tree

**Result**: Re-ingestion of same version works correctly without cleanup.

### Edge Case: Orphaned Files After Re-Ingestion

If a file is **removed** from the module between re-ingestions:
- The old file entry remains in `bucketOwnedFiles` and `bucketPathIndex`
- `BuildDirectoryIndexes` scans `bucketOwnedFiles` to build the directory tree
- So the removed file WILL still appear in directory listings

**For v1, this is acceptable**: Go modules are immutable (same version = same content).
Git repos with updated refs could have this issue, but it's a pre-existing limitation
of the ingestion pipeline, not specific to build layouts.

**Future fix**: Add a `ClearRepository` RPC that deletes all entries for a storageID
before re-ingesting. This is tracked as Phase 8 work.

### Phase 6 Verification

```bash
# Ingest two versions of the same module
./bin/monofs-admin ingest --router=localhost:9090 \
  --source=github.com/google/uuid@v1.6.0 \
  --ingestion-type=go --fetch-type=gomod

./bin/monofs-admin ingest --router=localhost:9090 \
  --source=github.com/google/uuid@v1.5.0 \
  --ingestion-type=go --fetch-type=gomod

# Mount and verify both versions exist
mkdir -p /tmp/monofs-test
./bin/monofs-client --mount=/tmp/monofs-test --router=localhost:9090 &
sleep 3

ls /tmp/monofs-test/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/
ls /tmp/monofs-test/go-modules/pkg/mod/github.com/google/uuid@v1.5.0/
# Both should exist with their respective files

fusermount -u /tmp/monofs-test
```

---

## Phase 7: Build CLI Wrapper (`monofs-build`)

**Goal**: Convenience CLI that sets env vars and runs build commands.

**Estimated effort**: 1 day

### Step 7.1: Create `cmd/monofs-build/main.go`

```go
// filepath: cmd/monofs-build/main.go
// MonoFS Build - Run builds using MonoFS as module/package cache
package main

import (
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
)

func main() {
    if len(os.Args) < 2 {
        printUsage()
        os.Exit(1)
    }

    buildType := os.Args[1]

    // Parse optional --mount flag
    mountPoint := "/mnt/monofs" // default
    dashIdx := -1
    for i, arg := range os.Args {
        if strings.HasPrefix(arg, "--mount=") {
            mountPoint = strings.TrimPrefix(arg, "--mount=")
        }
        if arg == "--" {
            dashIdx = i
            break
        }
    }

    if dashIdx < 0 || dashIdx+1 >= len(os.Args) {
        fmt.Println("Error: use -- to separate build arguments")
        fmt.Println("Example: monofs-build go --mount=/mnt/monofs -- build ./...")
        os.Exit(1)
    }

    buildArgs := os.Args[dashIdx+1:]

    var err error
    switch buildType {
    case "go":
        err = runGoBuild(mountPoint, buildArgs)
    case "bazel":
        err = runBazelBuild(mountPoint, buildArgs)
    default:
        fmt.Printf("Unknown build type: %s\n", buildType)
        printUsage()
        os.Exit(1)
    }

    if err != nil {
        fmt.Printf("Build failed: %v\n", err)
        os.Exit(1)
    }
}

func runGoBuild(mountPoint string, args []string) error {
    gomodcache := filepath.Join(mountPoint, "go-modules/pkg/mod")

    // Verify mount exists
    if _, err := os.Stat(gomodcache); os.IsNotExist(err) {
        return fmt.Errorf("GOMODCACHE directory not found at %s\n"+
            "Make sure:\n"+
            "  1. MonoFS is mounted at %s\n"+
            "  2. Dependencies are ingested (monofs-admin ingest-deps --file=go.mod --type=go)\n",
            gomodcache, mountPoint)
    }

    cmd := exec.Command("go", args...)
    cmd.Env = append(os.Environ(),
        "GOMODCACHE="+gomodcache,
        "GONOSUMDB=*",
        "GONOSUMCHECK=*",
        "GOFLAGS=-mod=mod",
    )
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    cmd.Stdin = os.Stdin
    return cmd.Run()
}

func runBazelBuild(mountPoint string, args []string) error {
    repoCache := filepath.Join(mountPoint, "bazel-repos")

    if _, err := os.Stat(repoCache); os.IsNotExist(err) {
        return fmt.Errorf("bazel-repos directory not found at %s\n"+
            "Make sure MonoFS is mounted and Bazel repos are ingested.\n",
            repoCache)
    }

    cmd := exec.Command("bazel", args...)
    cmd.Env = append(os.Environ(),
        "BAZEL_REPOSITORY_CACHE="+repoCache,
    )
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    cmd.Stdin = os.Stdin
    return cmd.Run()
}

func printUsage() {
    fmt.Println("Usage: monofs-build <go|bazel> [--mount=PATH] -- <build args...>")
    fmt.Println()
    fmt.Println("Examples:")
    fmt.Println("  monofs-build go -- build ./...")
    fmt.Println("  monofs-build go --mount=/tmp/monofs -- test ./...")
    fmt.Println("  monofs-build bazel -- build //...")
}
```

**Lines**: ~110

### Step 7.2: Add Build Target to Makefile

**File to modify**: `Makefile`

Add after existing binary build targets (around line 99):

```makefile
$(BIN_DIR)/monofs-build: $(BIN_DIR)
    $(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/monofs-build ./$(CMD_DIR)/monofs-build
    @echo "Built $(BIN_DIR)/monofs-build"
```

Also add `monofs-build` to the `build` target's dependency list.

---

## Phase 8: Documentation & Polish

**Goal**: Production docs, error handling review, minor improvements.

**Estimated effort**: 2 days

### Step 8.1: Create `docs/BUILD_INTEGRATION.md` (User Guide)

Cover:
- Overview of how build integration works
- Setting up Go module cache with MonoFS
- `ingest-deps` usage with examples
- `monofs-build` wrapper usage
- Setting up Bazel with MonoFS
- Troubleshooting (mount not found, files not appearing, etc.)
- Performance tips (pre-ingest all deps before build)

### Step 8.2: Create `docs/ADDING_BUILD_SYSTEM.md` (Developer Guide)

Cover:
- How LayoutMapper interface works
- Step-by-step: implementing npm/Maven/Cargo mapper
- Testing guidelines
- Registration in router
- What NOT to change (FUSE, server, proto)

### Step 8.3: Production Hardening Checklist

- [ ] All `generateLayouts()` errors logged with structured fields (slog)
- [ ] Layout generation time tracked and logged
- [ ] `monofs-admin repos` shows which repos are virtual (add `is_virtual` flag?)
- [ ] Verify behavior when all nodes are unhealthy during layout generation
- [ ] Verify behavior when ingestion succeeds but layout fails
- [ ] Verify `ingest-deps` handles duplicate deps gracefully
- [ ] Add `--dry-run` flag to `ingest-deps`

---

## Implementation Status

| Phase | Status | Effort | Dependencies |
|-------|--------|--------|--------------|
| Phase 1: Framework + Go Mapper | ⏸️ NOT STARTED | 2-3 days | None |
| Phase 2: Post-Ingestion Hook | ⏸️ NOT STARTED | 2-3 days | Phase 1 |
| Phase 3: Bulk Dep Ingestion | ⏸️ NOT STARTED | 1-2 days | Phase 1 |
| Phase 4: E2E Go Build Test | ⏸️ NOT STARTED | 1 day | Phase 2 + 3 |
| Phase 5: Bazel Mapper | ⏸️ NOT STARTED | 1-2 days | Phase 2 |
| Phase 6: Re-Ingestion | ⏸️ NOT STARTED | 1 day | Phase 2 |
| Phase 7: Build CLI Wrapper | ⏸️ NOT STARTED | 1 day | Phase 2 |
| Phase 8: Docs & Polish | ⏸️ NOT STARTED | 2 days | All |

**Estimated Total**: 2-3 weeks (1 developer)

---

## Files Summary

### New Files (create from scratch)

| File | Lines | Phase | Purpose |
|------|-------|-------|---------|
| `internal/buildlayout/types.go` | ~100 | 1.1 | Interfaces and registry |
| `internal/buildlayout/types_test.go` | ~100 | 1.2 | Registry unit tests |
| `internal/buildlayout/golang/mapper.go` | ~180 | 1.3 | Go module LayoutMapper |
| `internal/buildlayout/golang/mapper_test.go` | ~220 | 1.4 | Go mapper unit tests |
| `internal/router/layout.go` | ~220 | 2.1 | Layout orchestration |
| `internal/buildlayout/bazel/mapper.go` | ~100 | 5.1 | Bazel LayoutMapper |
| `internal/buildlayout/bazel/mapper_test.go` | ~140 | 5.2 | Bazel mapper unit tests |
| `cmd/monofs-build/main.go` | ~110 | 7.1 | Build CLI wrapper |
| `scripts/test-go-build.sh` | ~50 | 4.1 | E2E test script |
| `test/build_go_integration_test.go` | ~30 | 4.2 | Integration test |
| `docs/BUILD_INTEGRATION.md` | ~200 | 8.1 | User guide |
| `docs/ADDING_BUILD_SYSTEM.md` | ~200 | 8.2 | Developer guide |

### Modified Files (minimal edits)

| File | Change | Lines Changed | Phase |
|------|--------|--------------|-------|
| `internal/router/router.go` | Add `layoutRegistry` field + setter | ~10 | 1.5 |
| `internal/router/ingest.go` | Collect files during WalkFiles + call generateLayouts | ~20 | 2.2 |
| `cmd/monofs-router/main.go` | Create registry + register mappers | ~10 | 1.5, 5.3 |
| `cmd/monofs-admin/main.go` | Add `ingest-deps` subcommand | ~100 | 3.1 |
| `Makefile` | Add `monofs-build` target | ~5 | 7.2 |

### Files NOT Modified

| File | Why No Changes Needed |
|------|----------------------|
| `internal/server/server.go` | Virtual repos use existing buckets/RPCs |
| `internal/server/directory.go` | BuildDirectoryIndexes works on any storageID |
| `internal/fuse/node.go` | FUSE is a dumb client — no knowledge of virtual paths |
| `internal/fuse/overlay.go` | Overlay/write support unchanged |
| `internal/client/client.go` | Client routes via HRW, works for any storageID |
| `internal/sharding/*.go` | HRW sharding works with any storageID |
| `api/proto/*.proto` | No new RPCs needed |
| `internal/fetcher/*.go` | Fetcher resolves by blob_hash, works for virtual files |
| `internal/cache/*.go` | Cache keyed by path, works for virtual paths |
| `internal/search/*.go` | Search indexes all repos including virtual |

---

## Adding a New Build System (Example: npm)

To prove modularity, here's everything needed to add npm support:

### 1. Create `internal/buildlayout/npm/mapper.go` (~100 lines)

```go
package npm

import (
    "encoding/json"
    "fmt"
    "path"
    "strings"

    "github.com/radryc/monofs/internal/buildlayout"
)

type NpmMapper struct{}

func NewNpmMapper() *NpmMapper { return &NpmMapper{} }

func (n *NpmMapper) Type() string { return "npm" }

func (n *NpmMapper) Matches(info buildlayout.RepoInfo) bool {
    return info.IngestionType == "git"
}

func (n *NpmMapper) MapPaths(info buildlayout.RepoInfo, files []buildlayout.FileInfo) ([]buildlayout.VirtualEntry, error) {
    // Check for package.json in root
    hasPackageJSON := false
    for _, f := range files {
        if f.Path == "package.json" {
            hasPackageJSON = true
            break
        }
    }
    if !hasPackageJSON {
        return nil, nil
    }

    packageName := path.Base(strings.TrimSuffix(info.DisplayPath, "/"))
    version := info.Ref
    if version == "" || version == "main" || version == "master" {
        version = "latest"
    }

    virtualDisplayPath := fmt.Sprintf("node_modules/%s@%s", packageName, version)

    entries := make([]buildlayout.VirtualEntry, 0, len(files))
    for _, f := range files {
        entries = append(entries, buildlayout.VirtualEntry{
            VirtualDisplayPath: virtualDisplayPath,
            VirtualFilePath:    f.Path,
            OriginalFilePath:   f.Path,
        })
    }
    return entries, nil
}

func (n *NpmMapper) ParseDependencyFile(content []byte) ([]buildlayout.Dependency, error) {
    var pkg struct {
        Dependencies    map[string]string `json:"dependencies"`
        DevDependencies map[string]string `json:"devDependencies"`
    }
    if err := json.Unmarshal(content, &pkg); err != nil {
        return nil, fmt.Errorf("parse package.json: %w", err)
    }

    var deps []buildlayout.Dependency
    for name, version := range pkg.Dependencies {
        deps = append(deps, buildlayout.Dependency{
            Module:  name,
            Version: version,
            Source:  name + "@" + version,
        })
    }
    return deps, nil
}
```

### 2. Register in `cmd/monofs-router/main.go` (1 line)

```go
layoutRegistry.Register(npm.NewNpmMapper())
```

### 3. Add to `selectMapper()` in admin CLI (3 lines)

```go
case "npm":
    return npm.NewNpmMapper()
```

### 4. That's it.

No FUSE changes. No server changes. No proto changes. No fetcher changes. No sharding changes.

---

## Success Criteria

### Functional
- [ ] `go build` works offline with MonoFS GOMODCACHE
- [ ] Multiple Go module versions coexist in `go-modules/pkg/mod/`
- [ ] `ingest-deps` bulk-ingests all deps from go.mod
- [ ] Re-ingestion of same version updates layouts correctly
- [ ] Bazel repos appear under `bazel-repos/`
- [ ] Adding new build system = 1 file + 1 line registration + 3 lines in admin CLI
- [ ] Zero changes to FUSE, server, proto, fetcher, sharding, or client code

### Performance
- [ ] Layout generation < 5s per repository (just metadata, no blob I/O)
- [ ] Virtual file lookup: same speed as original (same NutsDB path resolution)
- [ ] Blob dedup: same blob_hash → fetcher serves once regardless of path count

### Testing
- [ ] `go test ./internal/buildlayout/...` — all pass
- [ ] `go test ./internal/buildlayout/golang/...` — all pass
- [ ] `go test ./internal/buildlayout/bazel/...` — all pass
- [ ] `go test ./internal/router/...` — existing tests still pass
- [ ] `go test ./internal/server/...` — existing tests still pass
- [ ] `make build` compiles cleanly
- [ ] Manual E2E: `go build` with MonoFS GOMODCACHE succeeds
