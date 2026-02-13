# Fetcher-Generated Files HRW Distribution Verification

## Overview

This document explains how to verify that fetcher-generated files (cache metadata) are properly distributed to HRW nodes.

## What Are Fetcher-Generated Files?

Fetcher-generated files are cache metadata files that don't exist in storage but are generated on-demand by the fetcher. Examples:

### Go Modules
- `v1.6.0.info` - JSON with version and timestamp
- `v1.6.0.mod` - Copy of go.mod (or minimal version)
- `list` - Available versions
- `v1.6.0.zip` - Module archive
- `v1.6.0.ziphash` - Checksum of the zip

### NPM Packages
- `package.json` - Package metadata
- `.npm-metadata.json` - NPM registry metadata
- `.package-lock.json` - Lock file

## HRW Sharding Logic

All files (both regular virtual files and fetcher-generated files) use the same HRW sharding:

```
shard_key = storage_id + ":" + file_path
target_node = HRW.GetNodes(shard_key, 1)[0]
```

**Key Point**: Fetcher-generated files are distributed just like regular files - no special treatment in the sharding logic.

## Enhanced Logging

The router now logs detailed information about fetcher-generated file distribution:

### 1. File Type Identification

```
creating fetcher-generated file
  virtual_path="v1.6.0.info"
  cache_type="go-module"
  marker_hash="<SHA256 hash>"
  cache_metadata_keys=5
```

### 2. Batch Summary

```
batch built for virtual repo
  virtual_path="go-modules/pkg/mod/cache/download/github.com/google/uuid/@v"
  batch_size=5
  fetcher_generated_files=5
  regular_virtual_files=0
```

### 3. Per-File HRW Assignment

```
HRW sharding file to node
  file_path="v1.6.0.info"
  is_fetcher_generated=true
  shard_key="<storage_id>:v1.6.0.info"
  storage_id="<SHA256 hash>"
  blob_hash_prefix="fetcher-generated:..."
  target_node="node-1"
  has_cache_metadata=true
  cache_type="go-module"
```

### 4. Per-Node Distribution Summary

```
node file distribution breakdown
  virtual_path="go-modules/pkg/mod/cache/download/github.com/google/uuid/@v"
  node_id="node-1"
  total_files=3
  fetcher_generated=3
  regular_virtual=0

node file distribution breakdown
  virtual_path="go-modules/pkg/mod/cache/download/github.com/google/uuid/@v"
  node_id="node-2"
  total_files=2
  fetcher_generated=2
  regular_virtual=0
```

## How to Test

### 1. Start MonoFS Components

```bash
# Start router
./bin/monofs-router --config router-config.yaml

# Start multiple server nodes
./bin/monofs-server --node-id node-1 --port 8081
./bin/monofs-server --node-id node-2 --port 8082
./bin/monofs-server --node-id node-3 --port 8083
```

### 2. Run Test Script

```bash
./test_fetcher_generated_distribution.sh
```

### 3. Watch Router Logs

Look for these key indicators:

**✓ Files are created correctly:**
```
creating fetcher-generated file virtual_path="v1.6.0.info" cache_type="go-module"
creating fetcher-generated file virtual_path="v1.6.0.mod" cache_type="go-module"
creating fetcher-generated file virtual_path="list" cache_type="go-module"
```

**✓ HRW assigns each file to a node:**
```
HRW sharding file to node file_path="v1.6.0.info" target_node="node-1" is_fetcher_generated=true
HRW sharding file to node file_path="v1.6.0.mod" target_node="node-2" is_fetcher_generated=true
HRW sharding file to node file_path="list" target_node="node-1" is_fetcher_generated=true
```

**✓ Files are distributed across nodes:**
```
node file distribution breakdown node_id="node-1" fetcher_generated=3
node file distribution breakdown node_id="node-2" fetcher_generated=2
```

## What to Verify

### ✅ Each fetcher-generated file has a target_node

If you see `target_node="none"`, there's a problem with node availability or HRW configuration.

### ✅ Files are distributed according to HRW

The same `shard_key` should always map to the same node. You can verify by:
1. Ingesting the same module twice
2. Check that each file goes to the same node both times

### ✅ Marker hash is present

Fetcher-generated files should have blob hash starting with the computed marker:
- Marker hash: `SHA256("fetcher-generated:" + file_path)`
- This signals the fetcher to generate content on-demand

### ✅ Cache metadata is passed through

Each fetcher-generated file should have `has_cache_metadata=true` and `cache_type` set.

## Troubleshooting

### Problem: All files go to one node

**Symptom:**
```
node file distribution breakdown node_id="node-1" fetcher_generated=5
# No other nodes show files
```

**Cause**: Only one healthy node in the cluster

**Solution**: Check node health status in router logs

### Problem: target_node="none"

**Symptom:**
```
HRW sharding file to node target_node="none"
```

**Cause**: No healthy nodes available

**Solution**: 
1. Check if server nodes are registered with router
2. Check network connectivity
3. Review node health checks

### Problem: Inconsistent node assignment

**Symptom**: Same file goes to different nodes on re-ingestion

**Cause**: 
- Node list changed between ingestions
- Storage ID changed (shouldn't happen)

**Solution**: HRW is consistent as long as the node list is stable. If nodes are added/removed, some files may remap.

## Implementation Details

### Marker Hash Computation

```go
markerHash := SHA256("fetcher-generated:" + virtualFilePath)
// Example: SHA256("fetcher-generated:v1.6.0.info")
```

### Shard Key Format

```go
shardKey := storageID + ":" + filePath
// Example: "abc123...def:v1.6.0.info"
```

### File Identification

Files are identified as fetcher-generated by:
1. `OriginalFilePath == ""` - No source file
2. `CacheMetadata != nil` - Has generation parameters
3. `BlobHash` starts with computed marker hash

During distribution, we check:
```go
isFetcherGenerated := strings.HasPrefix(fm.BlobHash, "fetcher-generated:")
```

## Next Steps

After verifying HRW distribution:

1. **Test retrieval**: Try reading fetcher-generated files through FUSE
2. **Verify content generation**: Check that fetcher generates correct content
3. **Test failover**: Remove a node and verify files remap correctly
4. **Load testing**: Ingest many modules and verify even distribution

## Log Level Configuration

To see all HRW distribution logs, set log level to `info` or `debug`:

```yaml
# router-config.yaml
log_level: info  # or debug for even more detail
```
