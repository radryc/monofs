# Offline Build Support — Implementation TODO

## Overview

Add Go module download cache generation so MonoFS can support 100% offline `go build`.
During ingestion, generate metadata-only entries for `cache/download/` files.
Fetchers generate actual content on-demand when FUSE reads occur.

## Architecture

- **Ingestion time (Router)**: Generate `VirtualEntry` metadata for 6 cache files per module@version
- **Server nodes**: Store metadata only — no artifact data
- **Fetcher nodes**: Generate content on-demand from cached zip / proxy

## Status

- [x] Phase 1: Core buildcache interface (`internal/buildcache/types.go`, `registry.go`)
- [x] Phase 2: Go module artifact generator (`internal/buildcache/golang/generator.go`)
- [x] Phase 3: Extend VirtualEntry for synthetic entries (`internal/buildlayout/types.go`)
- [x] Phase 4: Extend GoMapper to emit cache/download entries (`internal/buildlayout/golang/mapper.go`)
- [x] Phase 5: Fetcher artifact generation (`internal/fetcher/gomod_artifacts.go`, `gomod_artifacts_hash.go`)
- [x] Phase 6: Route artifact_type in FetchBlob (`internal/fetcher/gomod_backend.go`)
- [x] Phase 7: Handle synthetic VirtualEntry in router (`internal/router/layout.go`)
- [x] Phase 8: Add golang.org/x/mod dependency for dirhash.HashZip
- [x] Phase 9: Unit tests (all passing)
- [ ] Phase 10: Integration test with real module
- [ ] Phase 11: npm stub generator
- [ ] Phase 12: cargo stub generator
- [ ] Phase 13: Documentation (OFFLINE_BUILD.md)

## Files Created / Modified

### New Files
| File | Purpose |
|------|---------|
| `internal/buildcache/types.go` | `ArtifactType`, `ArtifactEntry`, `ArtifactGenerator` interface, `ArtifactParams` |
| `internal/buildcache/registry.go` | `Registry` for generators |
| `internal/buildcache/registry_test.go` | Registry unit tests |
| `internal/buildcache/golang/generator.go` | Go module cache artifact metadata generator |
| `internal/buildcache/golang/generator_test.go` | Generator unit tests |
| `internal/fetcher/gomod_artifacts.go` | On-demand artifact content generation (6 types) |

### Modified Files
| File | Change |
|------|--------|
| `internal/buildlayout/types.go` | Add optional fields to `VirtualEntry` for synthetic (generated) entries |
| `internal/buildlayout/golang/mapper.go` | Add `generateCacheDownloadEntries()`, call from `MapPaths()` |
| `internal/router/layout.go` | Handle synthetic VirtualEntry in `ingestVirtualRepo()` |
| `internal/fetcher/gomod_backend.go` | Route `artifact_type` requests to `fetchCacheArtifact()` |
| `go.mod` / `go.sum` | Add `golang.org/x/mod` dependency |

## Cache Files Generated Per Module

For `github.com/google/uuid@v1.6.0`:

```
go-modules/pkg/mod/cache/download/github.com/google/uuid/@v/
├── list             → "v1.6.0\n"
├── v1.6.0.info      → {"Version":"v1.6.0","Time":"2024-..."}
├── v1.6.0.mod       → go.mod contents
├── v1.6.0.zip       → module source archive
├── v1.6.0.ziphash   → "h1:<base64-sha256>="
└── v1.6.0.lock      → empty file
```

## Usage

```bash
export GOMODCACHE=/mnt/monofs/go-modules/pkg/mod
export GONOSUMCHECK="*"
export GONOSUMDB="*"
export GOFLAGS="-mod=mod"
go build ./...
```

## Adding New Backends

1. Create `internal/buildcache/<backend>/generator.go` implementing `ArtifactGenerator`
2. Create `internal/fetcher/<backend>_artifacts.go` with content generators
3. Extend the corresponding layout mapper to call the generator
4. Route artifact_type in the fetcher's FetchBlob()
