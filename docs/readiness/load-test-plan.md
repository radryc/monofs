# Load Test Plan — Performance Validation for Phases 1–3

## Objective

Validate that MonoFS meets performance and scale targets for 2,000 engineers under Phase 1 (durability), Phase 2 (preserve mode), and Phase 3 (policy + auto-push). Establish baseline metrics for capacity planning.

## Target Scale (from README)

| Parameter | Target |
|-----------|--------|
| Engineers in monorepo | 2,000 |
| Local commits per engineer per day | 20 |
| Push jobs per engineer per day | 4 |
| Peak window | 4 hours (40% of daily load) |
| CI/automation workspace overhead | +50% |

**Derived load:**

| Metric | Daily | Peak (per second) |
|--------|-------|-------------------|
| Source-push jobs | 8,000 | ~0.6 sustained, ~2 burst |
| Audit events | 80,000 | ~5 sustained, ~50 burst |
| Concurrent push operations | — | ≤ 50 |
| WAL appends | 80,000 | ~5 sustained, ~50 burst |
| Policy evaluations | 8,000 | ~0.6 sustained, ~2 burst |

## Test Scenarios

### Scenario 1: Baseline Throughput (Phase 1)

**Objective:** Measure sustained WAL append rate and confirm it supports 5–50 events/sec.

**Setup:**

1. Single router instance, local workspace state dir (no S3)
2. 10 concurrent workspaces, each writing jobs at different rates
3. Enable Phase 1 durability (`WorkspaceStateDir` set, compaction enabled)
4. Disable Phase 3 policy (policy gate OFF)

**Load profile:**

```
Workspace ws-1: 10 jobs/sec (sustained)
Workspace ws-2–5: 2 jobs/sec each (8 total)
Workspace ws-6–10: 0.5 jobs/sec each (2.5 total)
Total: ~20 jobs/sec sustained (4x peak target; margin for spikes)
```

**Test duration:** 10 minutes

**Metrics to record:**

| Metric | Tool | Target | Alert |
|--------|------|--------|-------|
| WAL append latency (p50, p99) | Custom timer | <1ms, <10ms | >5ms avg |
| WAL append throughput | Counter | ≥20 jobs/sec | <15 jobs/sec |
| In-memory index lookup latency (GetJob) | Custom timer | <100µs | >500µs avg |
| Disk I/O wait (iowait %) | iostat | <10% | >20% |
| Router CPU usage | top | <40% | >60% |
| Memory growth | RSS | <500 MB | >1 GB |
| Compaction duration | Timer | <30s per cycle | >60s |

**Acceptance criteria:**

- [ ] All append latencies (p99) < 10ms
- [ ] Throughput ≥ 20 jobs/sec sustained
- [ ] Memory stable (no leaks)
- [ ] CPU < 50%

**Test code template:**

```go
// test/load-test-scenario-1.go
func TestScenario1_BaselineThroughput(t *testing.T) {
    store := setupLocalWorkspaceStore(t)
    defer store.Close()
    
    // 10 concurrent workspaces
    var wg sync.WaitGroup
    rates := []int{10, 2, 2, 2, 2, 1, 1, 1, 1, 1}  // jobs/sec
    
    startTime := time.Now()
    for i, rate := range rates {
        wg.Add(1)
        go func(idx, r int) {
            defer wg.Done()
            ticker := time.NewTicker(time.Second / time.Duration(r))
            defer ticker.Stop()
            
            for range ticker.C {
                if time.Since(startTime) > 10*time.Minute {
                    return
                }
                job := newTestJob(fmt.Sprintf("ws-%d", idx))
                start := time.Now()
                _ = store.UpsertJob(job)
                recordLatency("wal_append_latency", time.Since(start))
            }
        }(i, rate)
    }
    
    wg.Wait()
    
    // Verify metrics
    recordedCount := getMetric("wal_append_total")
    assert(recordedCount >= 10*60*20, "expected ≥ %d appends", 10*60*20)
}
```

---

### Scenario 2: Compaction Under Load (Phase 1)

**Objective:** Measure compaction throughput and verify it doesn't block writers.

**Setup:**

1. Single router, local workspace state dir
2. Pre-populate WAL with 500K entries (simulate 2 days of history)
3. Concurrent load: 10 jobs/sec writes
4. Trigger compaction manually

**Test profile:**

```
1. Write 500K entries (baseline state)
2. Start background writes: 10 jobs/sec
3. Trigger compaction
4. Measure:
   - Compaction duration
   - Writer latency during compaction (must not increase significantly)
   - Snapshot size and gzip ratio
```

**Duration:** 5 minutes total

**Metrics to record:**

| Metric | Tool | Target | Alert |
|--------|------|--------|-------|
| Compaction duration | Timer | <60s | >120s |
| Writer append latency (during compaction) | Custom timer | <2ms (baseline), <5ms (during) | >10ms avg |
| Snapshot size | du | 50–100 MB | >500 MB |
| Snapshot gzip ratio | zcat size / original | >90% compression | <70% |
| Concurrent writers blocked | Counter | 0 | any blocking |

**Acceptance criteria:**

- [ ] Compaction completes < 60s
- [ ] Writer latency does not increase > 2x during compaction
- [ ] No writers are blocked by compaction
- [ ] Snapshot compresses > 70%

**Test code:**

```go
// test/load-test-scenario-2.go
func TestScenario2_CompactionUnderLoad(t *testing.T) {
    store := setupLocalWorkspaceStore(t)
    defer store.Close()
    
    // Pre-populate with 500K entries
    for i := 0; i < 500_000; i++ {
        job := newTestJob("ws-baseline")
        _ = store.UpsertJob(job)
    }
    initialSeq := store.CurrentSeq()
    
    // Start background writes
    stop := make(chan struct{})
    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        defer wg.Done()
        ticker := time.NewTicker(100 * time.Millisecond)  // 10 jobs/sec
        for {
            select {
            case <-stop:
                return
            case <-ticker.C:
                job := newTestJob("ws-bg")
                start := time.Now()
                _ = store.UpsertJob(job)
                recordLatency("writer_latency_during_compaction", time.Since(start))
            }
        }
    }()
    
    // Trigger compaction
    compactStart := time.Now()
    _ = store.TriggerCompaction()
    compactDur := time.Since(compactStart)
    
    close(stop)
    wg.Wait()
    
    // Verify
    assert(compactDur < 60*time.Second, "compaction too slow: %v", compactDur)
}
```

---

### Scenario 3: Policy Evaluation Latency (Phase 3)

**Objective:** Measure policy engine latency with realistic rule complexity.

**Setup:**

1. Single router, policy gate enabled
2. Policy YAML with 20 rules (realistic governance)
3. Concurrent policy evaluations: 50 concurrent requests
4. Mix of allowed and denied outcomes

**Policy test fixture:**

```yaml
version: 1
default: deny
rules:
  - name: "allow ops team full access"
    match:
      principal_ids: ["ops-*"]
    effect: allow
  - name: "block direct push to main"
    match:
      logical_branches: ["main", "master"]
      actions: [SOURCE_PUSH]
    effect: deny
  # ... 18 more rules
```

**Test profile:**

```
1. Load policy (20 rules)
2. 50 concurrent goroutines, each evaluates 100 requests
3. Total: 5K policy evaluations
4. Measure latency per evaluation
```

**Duration:** 30 seconds

**Metrics to record:**

| Metric | Tool | Target | Alert |
|--------|------|--------|-------|
| Policy eval latency (p50, p99) | Custom timer | <1ms, <5ms | >10ms avg |
| Policy eval throughput | Counter | ≥50 evals/sec | <40 evals/sec |
| Rule match operations | Profiler | <50 comparisons per eval | >100 |
| Memory per eval | Profiler | <1 MB total | >10 MB |

**Acceptance criteria:**

- [ ] Policy eval latency (p99) < 5ms
- [ ] Throughput ≥ 50 evals/sec
- [ ] No memory leaks during 5K evaluations

**Test code:**

```go
// test/load-test-scenario-3.go
func TestScenario3_PolicyEvaluationLatency(t *testing.T) {
    policyYAML := loadTestPolicyYAML()
    engine := workspacepolicy.New(policyYAML)
    
    requests := generateTestRequests(5000)  // 5K test requests
    
    var wg sync.WaitGroup
    for i := 0; i < 50; i++ {  // 50 concurrent goroutines
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            for _, req := range requests[idx*100 : (idx+1)*100] {
                start := time.Now()
                _ = engine.Evaluate(req)
                recordLatency("policy_eval_latency", time.Since(start))
            }
        }(i)
    }
    
    wg.Wait()
}
```

---

### Scenario 4: S3 Upload Latency Under Load (Phase 1)

**Objective:** Measure S3 snapshot upload performance and verify circuit breaker behavior.

**Setup:**

1. Single router, S3 backend enabled
2. Local compaction generates snapshots
3. Concurrent uploads: simulate multiple workspaces compacting
4. Network conditions: normal + throttled (tc on iptables)

**Test profile:**

```
Workspaces: 5
Compaction rate: 1 per workspace per minute (staggered)
Upload size per snapshot: ~50 MB (median)
Total: 5 uploads/minute, ~250 MB/minute
```

**Duration:** 10 minutes

**Metrics to record:**

| Metric | Tool | Target | Alert |
|--------|------|--------|-------|
| S3 upload latency (p50, p99) | Custom timer | <10s, <30s | >60s avg |
| S3 upload throughput | Counter | ≥1 upload/min | <1 upload/min |
| Circuit breaker open count | Counter | 0 (normal) | any opens |
| Retry count | Counter | <10 total | >50 total |
| ETag match rate | Counter | 100% | <99% |

**Network conditions test:**

Add latency and packet loss:
```bash
# On test machine: add 100ms latency + 1% loss
tc qdisc add dev eth0 root netem delay 100ms loss 1%

# Run test
go test -run TestScenario4

# Cleanup
tc qdisc del dev eth0 root
```

**Acceptance criteria:**

- [ ] S3 upload latency (p99) < 30s under normal network
- [ ] Circuit breaker opens on failure (throttling test) and recovers
- [ ] ETag match rate = 100% (no corruptions)
- [ ] No retries on successful uploads

---

### Scenario 5: Full End-to-End Workflow (Integration)

**Objective:** Validate Phase 2 (preserve mode) and Phase 3 (policy + auto-push) in a realistic workflow.

**Setup:**

1. Single router + fetcher
2. Enable: Phase 1 durability, Phase 2 preserve mode, Phase 3 policy + auto-push
3. Simulate 5 engineers working in 2 workspaces
4. Each engineer: local commits → publish → source push

**Workflow:**

```
1. Engineer A creates 3 local commits (repo: frontend)
2. Engineer B creates 2 local commits (repo: backend)
3. Both trigger source-push simultaneously
4. Policy evaluation: allow for both
5. Preserve mode: replay commits upstream (3 + 2 = 5 upstream commits)
6. Verify upstream history matches local
7. Check audit trail: all events recorded in order
```

**Duration:** 5 minutes

**Metrics to record:**

| Metric | Tool | Target | Alert |
|--------|------|--------|-------|
| End-to-end latency (commit → push success) | Custom timer | <30s | >60s |
| Commit preservation accuracy | Comparison | 100% (local == upstream) | <100% |
| Policy denial → audit event gap | Analyzer | 0 (seq monotonicity) | any gaps |
| Job state transitions | Analyzer | correct order (pending → pushing → done) | any out-of-order |

**Acceptance criteria:**

- [ ] All 5 commits appear upstream with correct author/message
- [ ] Audit trail has correct seq ordering
- [ ] No commits duplicated or lost

---

## Chaos and Resilience Tests

### Chaos.1: Random WAL Segment Corruption

**Trigger:** Randomly corrupt bytes in active WAL segment during load.

**Expected:** Router detects corruption on recovery, truncates to last valid entry, continues.

**Test:**

```go
func TestChaos1_WALSegmentCorruption(t *testing.T) {
    store := setupLocalWorkspaceStore(t)
    go loadGenerator(store, 10)  // background writes
    
    time.Sleep(2 * time.Second)
    
    // Corrupt a random byte in the active WAL segment
    corruptWALSegment(t, store.WALPath(), 1)
    
    // Restart store (simulates process crash)
    store.Close()
    store = openWorkspaceStore(t)
    defer store.Close()
    
    // Verify recovery succeeded
    assert(store.CurrentSeq() > 0, "recovery failed")
}
```

### Chaos.2: S3 Connection Drop During Upload

**Trigger:** Kill TCP connection mid-snapshot upload.

**Expected:** Retry with exponential backoff; eventually succeeds or fails gracefully.

**Test:**

```go
func TestChaos2_S3ConnectionDrop(t *testing.T) {
    // Mock S3 with deliberate connection closes
    setupMockS3WithConnectionDrops(t)
    
    store := setupStoreWithS3(t)
    defer store.Close()
    
    // Trigger compaction; S3 upload will fail
    err := store.TriggerCompaction()
    
    // Should retry and eventually succeed
    time.Sleep(5 * time.Second)
    assert(store.CircuitBreakerOpen() == false, "circuit should recover")
}
```

### Chaos.3: Policy Config Hot-Reload During Active Push

**Trigger:** Change policy YAML mid-push (SIGHUP).

**Expected:** New push jobs use new policy; in-flight jobs use snapshot of old policy.

**Test:**

```go
func TestChaos3_PolicyReloadDuringPush(t *testing.T) {
    policyV1 := loadTestPolicyYAML()
    engine := workspacepolicy.New(policyV1)
    
    // Start a push request evaluation
    req := testPushRequest()
    ch := make(chan *workspacepolicy.Decision)
    go func() {
        ch <- engine.Evaluate(req)
    }()
    
    // Hot-reload policy (change from allow to deny)
    policyV2 := updatePolicyYAML(policyV1, "effect", "deny")
    engine.HotReload(policyV2)
    
    // In-flight request should complete with V1 policy
    decision := <-ch
    assert(decision.Allowed, "in-flight request must not see new policy")
    
    // New requests must use V2 policy
    decision2 := engine.Evaluate(req)
    assert(!decision2.Allowed, "new request must use reloaded policy")
}
```

---

## Load Test Execution Plan

### Environment Setup

1. **Test cluster:** 1 router + 1 fetcher (single node)
2. **Storage:** Local SSD (not NFS; simulates production)
3. **Network:** Simulate 50ms latency to S3 (use tc/iptables)
4. **Monitoring:** Prometheus + Grafana (scrape at 1s interval)

### Pre-test Checklist

- [ ] Router built with profiling enabled (`-cpuprofile`, `-memprofile`)
- [ ] S3 bucket/IAM ready (or moto mock running)
- [ ] Metrics are exported (Prometheus scrape config valid)
- [ ] Disk has ≥100 GB free (for WAL growth)

### Test Execution

```bash
# Run all scenarios
go test ./test -run "TestScenario" -v -timeout 30m \
    -cpuprofile=/tmp/cpu.prof \
    -memprofile=/tmp/mem.prof

# Generate report
go tool pprof -http=:8080 /tmp/cpu.prof
go tool pprof -http=:8081 /tmp/mem.prof
```

### Post-test Analysis

1. **Compare against targets:**
   - Append latency p99 vs <10ms target
   - Throughput vs 20 jobs/sec target
   - Memory stable vs no-leak target

2. **Identify bottlenecks:**
   - CPU profile: which functions consume most time?
   - Memory profile: which allocations dominate?
   - Disk I/O: iowait high?

3. **Document findings:**
   - File: `docs/load-test-results-{date}.md`
   - Include: graphs, latency distributions, recommendations

---

## Acceptance Criteria for Release

- [ ] Scenario 1 passes with all metrics under target
- [ ] Scenario 2 passes (compaction doesn't block writers)
- [ ] Scenario 3 passes (policy eval < 5ms p99)
- [ ] Scenario 4 passes with normal S3 latency (<30s)
- [ ] Scenario 5 passes (e2e workflow correct)
- [ ] All chaos tests pass (recovery is deterministic)
- [ ] Load test results documented and reviewed

---

## Capacity Planning Recommendations

Based on load test results, recommend:

| Component | Sizing | Rationale |
|-----------|--------|-----------|
| Router memory | TBD (from test) | Must hold 2,000 concurrent workspace indexes |
| Router CPU | TBD (from test) | Peak ~2 evals/sec = ~X% utilization |
| Local disk | TBD (from test) | WAL growth ~50 MB/day; compaction to 5 MB/day |
| S3 egress | TBD (from test) | ~Y GB/month for snapshots |

---

## Related Documents

- Performance targets: `docs/readiness/README.md`
- Monitoring: `docs/runbooks/monitoring.md` (TBD)
- CI integration: `.github/workflows/load-test.yml` (TBD)
