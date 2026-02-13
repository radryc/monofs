# Log Analysis: Fetcher-Generated Files HRW Distribution

## Summary

Based on the logs from your ingestion of `github.com/google/uuid@v1.6.0`, the fetcher-generated files **ARE** being properly distributed to HRW nodes. Here's the detailed analysis:

## Virtual Repos Created

The ingestion created **2 virtual repositories**:

### 1. Module Files Repo
```
virtual_path: go-modules/pkg/mod/github.com/google/uuid@v1.6.0
storage_id: 9165387d15f127ee143e3022dbf6a6bdcbf12ddf5bc4eb35756bc3a3f58a1e5e
files: 31 regular virtual files (hard links to original repo)
```

### 2. Cache Metadata Repo (Fetcher-Generated)
```
virtual_path: go-modules/pkg/mod/cache/download/github.com/google/uuid/@v
storage_id: 0767e697dd9620a431b1ef33c7aece1d35252d80cc8910c6d8a8cb5dd806dcf9
files: 5 fetcher-generated files
```

## Fetcher-Generated Files Detected

All 5 cache metadata files were correctly identified as fetcher-generated:

```
✓ v1.6.0.info    - marker_hash: 403a27d22795c17ea6e09ce587cce2a180f1fecf069b3ef3e887b7a1680e9d3e
✓ v1.6.0.mod     - marker_hash: fd1b1a14614c3738c122c83aa6cd5d1b5956b4ad12195b4a4e8d6fb390750011
✓ list           - marker_hash: 7d93c7634eedba2cb47d7715743dc2aa93a8429ff7c6f4f1377161340dd472b0
✓ v1.6.0.zip     - marker_hash: 8fcb918c4a58072a5482b9f31f11c48660da85d2084aca8eba54ecdabac443e9
✓ v1.6.0.ziphash - marker_hash: d2f0597d63f83f779b934377c2bf031ff3392d1601c707241705cbd528dfbf6b
```

All had:
- `cache_type=go-module` in metadata
- `cache_metadata_keys=5` or `6` (the .mod file has an extra `gomod_hash` field)

## HRW Distribution - Cache Metadata Files

The 5 fetcher-generated files were distributed across **3 nodes**:

### Node 2
```
v1.6.0.info
  shard_key: 0767e697dd9620a431b1ef33c7aece1d35252d80cc8910c6d8a8cb5dd806dcf9:v1.6.0.info
  → 1 file
```

### Node 4
```
v1.6.0.mod
  shard_key: 0767e697dd9620a431b1ef33c7aece1d35252d80cc8910c6d8a8cb5dd806dcf9:v1.6.0.mod
  → 1 file
```

### Node 5
```
list
v1.6.0.zip
v1.6.0.ziphash
  shard_keys: 
    - 0767e697dd9620a431b1ef33c7aece1d35252d80cc8910c6d8a8cb5dd806dcf9:list
    - 0767e697dd9620a431b1ef33c7aece1d35252d80cc8910c6d8a8cb5dd806dcf9:v1.6.0.zip
    - 0767e697dd9620a431b1ef33c7aece1d35252d80cc8910c6d8a8cb5dd806dcf9:v1.6.0.ziphash
  → 3 files
```

## HRW Distribution - Regular Virtual Files

The 31 regular virtual files (from the module itself) were distributed across **all 5 nodes**:

```
node1: 2 files
node2: 5 files  
node3: 4 files
node4: 7 files
node5: 13 files
Total: 31 files
```

## Verification Status

✅ **Each fetcher-generated file was assigned a target node** - No files with `target_node=none`

✅ **Files distributed across multiple nodes** - 3 out of 5 nodes received cache metadata files

✅ **Marker hashes computed correctly** - Each file has unique SHA256 hash based on its path

✅ **Cache metadata preserved** - All files show `has_cache_metadata=true` and `cache_type=go-module`

✅ **Consistent shard keys** - Format: `storage_id:file_path` (same for both types)

## Issue Found (Now Fixed)

The logs showed:
```
is_fetcher_generated=false
```

for all files, even though they were correctly identified earlier as fetcher-generated. 

**Root Cause**: The code was checking if `BlobHash` starts with `"fetcher-generated:"`, but the marker hash is actually a SHA256 hash (hex string), not a string prefixed with "fetcher-generated:".

**Fix Applied**: Now checking `fm.BackendMetadata["cache_type"] != ""` to identify fetcher-generated files.

## Expected Log Output After Fix

With the code fix, you should now see:

```
HRW sharding file to node
  file_path="v1.6.0.info"
  is_fetcher_generated=true  ← Fixed!
  cache_type="go-module"
  target_node="node2"

node file distribution breakdown
  node_id="node2"
  total_files=1
  fetcher_generated=1  ← Fixed!
  regular_virtual=0
```

## How to Re-Test

1. Rebuild the router:
   ```bash
   make bin/monofs-router
   ```

2. Restart the router with the new binary

3. Re-run the ingestion:
   ```bash
   ./test_fetcher_generated_distribution.sh
   ```

4. Look for these corrected log entries:
   - `is_fetcher_generated=true` for cache metadata files
   - `fetcher_generated=5` in the batch summary
   - Correct counts in per-node breakdown

## Conclusion

✅ **Fetcher-generated files ARE hitting proper HRW nodes**

The distribution is working correctly:
- Each file gets a deterministic target node based on HRW algorithm
- Files are spread across multiple nodes (3 of 5 for cache metadata)
- Same shard key will always map to same node (HRW consistency)
- The logging issue is now fixed to correctly identify file types

The HRW sharding is functioning as designed - fetcher-generated files are treated identically to regular virtual files in terms of distribution logic, which is correct.
