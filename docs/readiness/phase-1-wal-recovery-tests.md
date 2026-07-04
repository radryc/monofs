# Phase 1 — WAL Recovery and Correctness Tests

> Note: these tests still apply under [Phase 1B — Storage-node WAL and router proxy](phase-1b-storage-node-wal-proxy.md), but the test harness should target storage-node ledger WAL and router proxy behavior instead of router-local ledger WAL ownership.

## Objective

Prove that the Phase 1 WAL + snapshot design recovers safely from crashes, corruptions, and concurrent access patterns. These tests are prerequisites for production deployment.

## Test Categories

### 1. WAL Segment Recovery

**Scenario: Process crashes mid-write**

```
write("{"seq":10,"ts":"...","op":"UPSERT",...}\n") → crash before \n is flushed
```

**Expected behavior:** Replay at startup detects truncated final line, discards it, next write uses `seq=10` (not duplicated).

**Test implementation:**
```go
func TestWALTruncatedLineRecovery(t *testing.T) {
    // 1. Create WAL with 10 complete entries (seq 1–10)
    // 2. Append incomplete JSON without final \n
    // 3. Instantiate store, replay WAL
    // 4. Verify in-memory state has exactly 10 entries
    // 5. Verify nextSeq == 11 (not 10, not 12)
    // 6. Write seq=11 entry, verify it appears in WAL
}
```

**Scenario: Multiple segments, recovery resumes from last checkpoint**

```
wal-000001.log: seq 1–100 (compacted, deleted)
wal-000002.log: seq 101–150 (partially compacted)
wal-000003.log: seq 151–200 (active)
checkpoint.json: last_compacted_seq = 150
```

**Expected behavior:** Replay loads checkpoint, scans only wal-000002.log and wal-000003.log, skips 1–150, reconstructs state from seq 151–200.

**Test implementation:**
```go
func TestMultiSegmentRecoveryWithCheckpoint(t *testing.T) {
    // 1. Create 3 segments with entries covering seq 1–200
    // 2. Write checkpoint for seq 150
    // 3. Delete wal-000001.log (compacted)
    // 4. Instantiate store, replay from checkpoint
    // 5. Verify in-memory state matches seq 151–200
    // 6. Verify nextSeq == 201
}
```

### 2. Snapshot + Checkpoint Integrity

**Scenario: Checkpoint file write fails mid-atomic-rename**

**Expected behavior:** Old checkpoint is still valid; compaction worker retries later. New snapshot is orphaned but harmless (S3 object versioning recovers it).

**Test implementation:**
```go
func TestCheckpointAtomicRenameFailure(t *testing.T) {
    // 1. Create compaction worker with mocked tempfile Rename
    // 2. Simulate Rename returning EACCES (permission denied)
    // 3. Verify checkpoint.json is unchanged
    // 4. Verify next compaction attempt succeeds
    // 5. Verify orphaned snapshot file is logged/monitored
}
```

**Scenario: Snapshot file is truncated (corruption during gzip write)**

```
snapshot-000100.json.gz: incomplete (mid-gzip stream)
checkpoint.json: {last_compacted_seq: 100, sha256: abc123}
```

**Expected behavior:** On recovery, SHA256 hash mismatch detected → replay halts with `WAL_INTEGRITY_ERROR`, router refuses to start until file is removed.

**Test implementation:**
```go
func TestSnapshotChecksumMismatchBlocksStartup(t *testing.T) {
    // 1. Create valid snapshot, compute SHA256, write checkpoint
    // 2. Corrupt snapshot file (truncate last 100 bytes)
    // 3. Instantiate store
    // 4. Verify store.Open() returns error containing "checksum mismatch"
    // 5. Verify error is non-recoverable (forces manual intervention)
}
```

### 3. Concurrent Reader/Writer Safety

**Scenario: Reader acquires RLock, writer concurrently acquires Lock and mutates**

**Expected behavior:** Reads see snapshot at time of RLock acquisition (no dirty reads), writers are serialized (no races).

**Test implementation:**
```go
func TestConcurrentReaderWriterNoRace(t *testing.T) {
    // 1. Create store with 10 initial entries
    // 2. Launch goroutine: reader loops GetJob (holding RLock)
    // 3. Launch goroutine: writer loops UpsertJob with incremented seq
    // 4. Run 5 seconds, verify:
    //    - No reader sees seq gaps or duplicates
    //    - All writes succeed
    //    - WAL contains all writes in order
}
```

**Scenario: Two writers try to append simultaneously**

**Expected behavior:** Only one acquires Lock at a time (mutex serialization). Both writes appear in WAL in order.

**Test implementation:**
```go
func TestConcurrentWritersSerializedByMutex(t *testing.T) {
    // 1. Create store
    // 2. Launch 10 goroutines, each writes 100 entries
    // 3. Verify:
    //    - Total of 1000 entries in WAL, seq 1–1000, no gaps
    //    - In-memory index has exactly 1000 entries
    //    - No corrupted JSON in WAL
}
```

### 4. Compaction Correctness

**Scenario: Compaction completes; readers/writers continue; no one sees partial state**

**Expected behavior:** 
- Readers that acquired RLock before compaction sees old in-memory index
- Readers that acquire RLock after compaction sees new snapshot-based index
- Writers are never blocked by compaction (separate mutex)

**Test implementation:**
```go
func TestCompactionDoesNotBlockWriters(t *testing.T) {
    // 1. Create store with 10K entries
    // 2. Launch goroutine: slow reader (RLock, sleep 100ms, read all, RUnlock)
    // 3. Trigger compaction mid-reader-hold
    // 4. Launch goroutine: concurrent writer during compaction
    // 5. Verify writer completes while compaction is in progress
    // 6. Verify both reader and compaction complete successfully
}
```

**Scenario: Compaction snapshots state at checkpoint seq N; new writes go to current WAL after snapshot**

**Expected behavior:** Replay from checkpoint loads snapshot (seq ≤ N) then WAL (seq > N). No double-counting.

**Test implementation:**
```go
func TestCompactionNoDoubleCountingOnReplay(t *testing.T) {
    // 1. Create store, write 1000 entries
    // 2. Trigger compaction at seq=500
    // 3. Verify snapshot contains only seq 1–500 (as deltas)
    // 4. Verify WAL after compaction starts at seq 501
    // 5. Write 500 more entries (seq 501–1000)
    // 6. Wipe in-memory state, replay from checkpoint
    // 7. Verify final state has exactly 1000 entries, seq 1–1000
}
```

### 5. Job/Bundle/Audit Consistency

**Scenario: Audit event must never appear in WAL before corresponding job transition**

**Attack**: Verify `deny-before-write` guarantee — if a policy denies a push, audit event appears at lower seq than any successful push commit.

**Test implementation:**
```go
func TestAuditDenyBeforeWrite(t *testing.T) {
    // 1. Create store
    // 2. Write job UpsertJob(job, state=pending)
    // 3. Write AuditEvent(policy.denied)
    // 4. Write job UpsertJob(job, state=pushed)
    // 5. Verify WAL order: UPSERT(state=pending) < AUDIT(denied) < UPSERT(state=pushed)
    // 6. Replay from checkpoint, verify in-memory job state reflects this ordering
}
```

### 6. Failure Mode Matrices

**WAL append failures:**

| Scenario | Trigger | Expected Outcome |
|----------|---------|------------------|
| Disk full (`ENOSPC`) | Write returns -1 | Error bubbles to RPC caller; job marked as `errored`; retry queued |
| Permission denied (`EACCES`) | File owned by root, no write perms | Critical error; router logs and refuses further writes to this store |
| Segment rotated during append | Size check passes, actual write exceeds size check | Ok (segment can be larger than threshold); next rotation triggers on next write |

**Tests:**
```go
func TestWALAppendDiskFull(t *testing.T) { ... }
func TestWALAppendPermissionDenied(t *testing.T) { ... }
func TestWALSegmentRotationRaceCondition(t *testing.T) { ... }
```

### 7. Recovery Determinism

**Scenario: Replay WAL twice → same in-memory state**

**Expected behavior:** Idempotent.

**Test implementation:**
```go
func TestWALReplayIdempotent(t *testing.T) {
    // 1. Create store, write 1000 entries
    // 2. Snapshot in-memory state (deep copy)
    // 3. Wipe in-memory state, replay WAL
    // 4. Verify state matches original
    // 5. Wipe again, replay WAL a second time
    // 6. Verify state matches original again
}
```

### 8. Integration Test: Full Lifecycle

**Scenario: Process lifetime with crashes and compaction**

```
1. Start router, write 1000 entries
2. Trigger compaction (snapshot seq 1–500)
3. Write 500 more entries (seq 501–1000)
4. Simulate crash (kill process)
5. Restart router
6. Verify final state has 1000 entries
7. Write 100 more entries (seq 1001–1100)
8. Trigger second compaction (snapshot seq 1–1000)
9. Kill process again
10. Restart
11. Verify final state has 1100 entries
```

**Test implementation:**
```go
func TestFullLifecycleWithCrashesAndCompaction(t *testing.T) {
    // Detailed multi-step test covering recovery at each restart
}
```

## Acceptance Criteria

- [ ] All 8 test categories have ≥2 scenarios each
- [ ] Tests are table-driven where possible
- [ ] Code coverage on `internal/storage/workspacestore/wal.go` ≥ 95%
- [ ] All tests pass with `-race` detector enabled
- [ ] Chaos tests (random segment corruption, slow I/O) added for pre-release validation
- [ ] Recovery test results documented in `RELEASING.md` as a mandatory release gate

## File Locations

- Unit tests: `internal/storage/workspacestore/wal_test.go`
- Integration tests: `test/phase-1-wal-recovery_test.go`
- Chaos test suite: `test/phase-1-wal-chaos_test.go` (run only with `--tags=chaos`)
