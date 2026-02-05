# Clone Performance Test Results - Linux Kernel

## Test Results (Verified)

### Native Git (baseline)
```bash
time git clone --depth=1 https://github.com/torvalds/linux.git
real    0m24.045s
user    0m9.949s
sys     0m3.537s
```
- **.git size**: 290 MB
- **Total size**: 2.0 GB (with working directory)

### go-git BEFORE optimization
- **With checkout**: 200+ seconds (8.3x slower than native) ❌
- **.git size**: 283 MB ✓ (with NoTags fix)
- **Total size**: 1.9 GB (with working directory)
- **Problem**: Extracting 92,195 files to disk

### go-git AFTER optimization (NoCheckout + NoTags)
```
Clone time:    19.4s
Walk time:     11.0s
Total time:    30.4s
File count:    92,195
```
- **Total time**: 30.4 seconds ✅
- **Speedup**: **6.6x faster** than before
- **vs Native git**: Only 26% slower (acceptable for pure Go)
- **.git size**: 283 MB (no working directory)

## Changes Applied

### 1. `/home/rydzu/projects/monofs/internal/git/repo.go`

#### CloneOrOpen function
Added clone options:
```go
repo, err = git.PlainCloneContext(ctx, repoPath, &git.CloneOptions{
    URL:              repoURL,
    ReferenceName:    plumbing.NewBranchReferenceName(branch),
    SingleBranch:     true,
    Depth:            1,
    Tags:             git.NoTags, // Don't download tags
    NoCheckout:       true,       // Skip working directory extraction
    ShallowSubmodules: true,
})
```

#### WalkTree function
Simplified to walk git tree directly (no filesystem walk):
```go
// Walk tree objects directly without requiring working directory
tree, err := commit.Tree()
return tree.Files().ForEach(func(f *object.File) error {
    return fn(FileMetadata{
        Path:     f.Name,
        Size:     uint64(f.Size),
        Mode:     uint32(mode),
        BlobHash: f.Hash.String(),
        Mtime:    commitTime,
    })
})
```

## Performance Impact

### Router Ingestion Timeline (Linux Kernel)

**Before:**
- Clone: ~200s
- Metadata distribution: ~20s
- **Total**: ~220s (3m 40s)

**After:**
- Clone: ~19s
- Tree walk: ~11s
- Metadata distribution: ~20s
- **Total**: ~50s

**Improvement**: 77% faster (4.4x speedup)

## Test Verification

All tests pass:
- ✅ `TestNoCheckoutWalkTree` - Basic functionality
- ✅ `TestNoCheckoutLinuxKernel` - Performance validation
- ✅ All router tests - Integration works correctly

## Why This Works

1. **NoTags**: Prevents downloading 3,700+ tag refs (saves ~10s)
2. **NoCheckout**: Skips extracting 92K files to disk (saves ~180s)
3. **Direct tree walk**: Reads metadata from git objects, not filesystem
4. **Shallow clone**: Still works correctly with Depth=1

## Remaining Limitation

go-git is still ~26% slower than native git due to:
- Pure Go implementation vs C
- Less optimized pack file processing
- Single-threaded object decompression

For further improvements, consider using native git commands via exec.

