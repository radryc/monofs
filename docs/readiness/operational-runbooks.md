# Operational Runbooks — Phases 0–3

Reference playbooks for common failure modes and maintenance tasks. Follow the decision tree, not intuition.

---

## Phase 0: Feature Gates

### Runbook P0.1: Enable a Feature Gate in Production

**Use case:** Ops wants to enable `PolicyGateEnabled=true` for staging env.

**Prerequisites:**
- [ ] Policy YAML exists and has been validated locally (`--policy-config /etc/monofs/policy.yaml`)
- [ ] At least one test workspace has the gate enabled (dog-food test)
- [ ] Team has reviewed policy rules (at least 2 approvals)

**Steps:**

1. **Validate the config:**
   ```bash
   monofs-admin validate-config --policy-config /etc/monofs/policy.yaml
   # Expected: "config is valid, 5 rules loaded"
   ```

2. **Dry-run the change (canary):**
   ```bash
   # Enable on single router instance in a test namespace
   kubectl set env deployment/monofs-router \
     --env="WORKSPACE_POLICY_GATE_ENABLED=true" \
     --namespace monofs-canary
   kubectl rollout status deployment/monofs-router -n monofs-canary --timeout=5m
   ```

3. **Monitor for 5 minutes:**
   ```bash
   # Watch logs for policy-related errors
   kubectl logs -f deployment/monofs-router -n monofs-canary | grep -E 'policy|ERROR'
   
   # Check metrics
   curl http://localhost:9090/metrics | grep policy_evals
   # Expected: policy_evals_total{effect="allow"} ≥ 10
   ```

4. **If canary succeeds, roll out to staging:**
   ```bash
   kubectl set env deployment/monofs-router \
     --env="WORKSPACE_POLICY_GATE_ENABLED=true" \
     --namespace monofs-staging
   kubectl rollout status deployment/monofs-router -n monofs-staging --timeout=10m
   ```

5. **Communicate to team:**
   - Slack: `@monofs-team Policy gate is now ENABLED in staging. All pushes require policy evaluation. Report issues in #incidents.`
   - Runbook link: This page

**Rollback (if failures occur):**
   ```bash
   kubectl set env deployment/monofs-router \
     --env="WORKSPACE_POLICY_GATE_ENABLED=false" \
     --namespace monofs-staging
   kubectl rollout status deployment/monofs-router -n monofs-staging --timeout=5m
   ```

---

## Phase 1: Durable Jobs and Audit Trail

### Runbook P1.1: WAL Segment Integrity Error at Startup

**Symptom:** Router fails to start with:
```
ERROR: WAL recovery failed: seq gap detected
       expected seq=42, got seq=44 at line 1234 of wal-000002.log
       WAL_INTEGRITY_ERROR: corrupted segment, manual repair required
```

**Root causes:**
- Disk corruption (ECC error, flaky storage)
- Process crash during mid-write
- NFS/EBS filesystem glitch

**Diagnosis:**

1. **Identify the corrupted line:**
   ```bash
   sed -n '1234p' /var/lib/monofs/workspace/jobs/wal/wal-000002.log | jq .
   # If it's truncated JSON (no closing brace), it's a crash
   ```

2. **Check timestamps to assess data loss:**
   ```bash
   tail -20 /var/lib/monofs/workspace/jobs/wal/wal-000002.log | jq '.seq, .ts'
   ```

3. **Assess data loss:**
   - If gap is in audit events (kind=AUDIT): low-impact (observability only)
   - If gap is in job state (kind=JOB): medium-impact (may orphan pending jobs)

**Recovery:**

**Option A: Truncate and recover (preferred if safe)**

1. Back up corrupted segment:
   ```bash
   cp /var/lib/monofs/workspace/jobs/wal/wal-000002.log \
      /var/lib/monofs/workspace/jobs/wal/wal-000002.log.corrupted
   ```

2. Truncate to last valid line:
   ```bash
   # Find line number of last complete JSON (ends with })
   grep -n "^{" /var/lib/monofs/workspace/jobs/wal/wal-000002.log | tail -1
   # Output: 1233:{"seq":41,...}
   
   # Truncate to that line
   head -1233 /var/lib/monofs/workspace/jobs/wal/wal-000002.log.corrupted > \
     /var/lib/monofs/workspace/jobs/wal/wal-000002.log
   ```

3. Restart router:
   ```bash
   systemctl restart monofs-router
   # Should succeed now
   ```

4. Verify recovery:
   ```bash
   monofs-admin audit-trail --workspace=ws-1 --count=5
   ```

**Option B: Discard corrupted segment (if data loss is acceptable)**

1. Delete the corrupted segment:
   ```bash
   rm /var/lib/monofs/workspace/jobs/wal/wal-000002.log
   ```

2. Restart router:
   ```bash
   systemctl restart monofs-router
   # Will replay from checkpoint, skip deleted segment
   ```

3. Alert data loss:
   ```bash
   # Check how many records are missing
   monofs-admin audit-trail --workspace=ws-1 --since 2026-07-03T10:00:00Z | wc -l
   ```

**Prevention:**

- Monitor storage health: `smartctl -a /dev/sda | grep Temperature`
- Use filesystems with checksums (btrfs, zfs) instead of ext4
- Run `fsck` weekly: `fsck.ext4 -n /dev/sda1` (dry-run, no repairs)

---

### Runbook P1.2: Snapshot Checksum Mismatch on Recovery

**Symptom:** Router fails to start with:
```
ERROR: WAL recovery failed: snapshot checksum mismatch
       checkpoint.json: last_compacted_seq=100, sha256=abc123
       snapshot file: sha256=def456 (file: snapshot-000100.json.gz)
```

**Root causes:**
- S3 download corruption (rare)
- Local snapshot file truncated
- Power loss during compaction write

**Diagnosis:**

1. **Verify snapshot file integrity:**
   ```bash
   file /var/lib/monofs/workspace/jobs/snapshots/snapshot-000100.json.gz
   # Should output: gzip compressed data, was "snapshot-000100.json"
   ```

2. **Check if file is truncated:**
   ```bash
   gzip -t /var/lib/monofs/workspace/jobs/snapshots/snapshot-000100.json.gz
   # If error: "unexpected end of file", it's corrupted
   ```

3. **Try to decompress:**
   ```bash
   zcat /var/lib/monofs/workspace/jobs/snapshots/snapshot-000100.json.gz | head -1
   # If error, it's corrupted; if output, try jq
   zcat /var/lib/monofs/workspace/jobs/snapshots/snapshot-000100.json.gz | jq . > /dev/null
   ```

**Recovery:**

**Option A: Recover from S3 backup**

1. List S3 snapshots:
   ```bash
   aws s3api list-object-versions \
     --bucket monofs-audit-prod \
     --prefix workspace-ws-1/snapshots/snapshot-000100 | jq '.Versions[0]'
   ```

2. Verify S3 snapshot is intact:
   ```bash
   aws s3api head-object \
     --bucket monofs-audit-prod \
     --key workspace-ws-1/snapshots/snapshot-000100.json.gz
   # Check: ContentLength, ETag
   ```

3. Download from S3:
   ```bash
   aws s3 cp \
     s3://monofs-audit-prod/workspace-ws-1/snapshots/snapshot-000100.json.gz \
     /var/lib/monofs/workspace/jobs/snapshots/snapshot-000100.json.gz
   ```

4. Restart router:
   ```bash
   systemctl restart monofs-router
   ```

**Option B: Discard snapshot and replay from earlier checkpoint**

1. Delete corrupted snapshot:
   ```bash
   rm /var/lib/monofs/workspace/jobs/snapshots/snapshot-000100.json.gz
   ```

2. Find earlier checkpoint:
   ```bash
   ls -lt /var/lib/monofs/workspace/jobs/checkpoints/
   # Pick the most recent one before the corrupted snapshot
   ```

3. Edit checkpoint to point to earlier seq:
   ```bash
   cat /var/lib/monofs/workspace/jobs/checkpoints/checkpoint.json
   # Edit last_compacted_seq to an earlier value (e.g., 50)
   ```

4. Restart router:
   ```bash
   systemctl restart monofs-router
   # Will replay from seq 51 onwards
   ```

5. Verify:
   ```bash
   monofs-admin audit-trail --workspace=ws-1 --count=5
   ```

**Prevention:**

- Store checkpoint.json as a replica on another disk/NFS
- Periodic snapshot validation: `monofs-admin verify-audit --snapshots` (weekly)

---

### Runbook P1.3: Compaction Falling Behind (Unbounded WAL Growth)

**Symptom:** Disk usage grows rapidly, compaction isn't triggered:
```bash
du -sh /var/lib/monofs/workspace/jobs/wal/
# Output: 450G (should be <256M)
```

**Root causes:**
- Compaction worker crashed/hung
- S3 upload is timing out (slow network, throttled)
- Disk is full (ENOSPC on snapshot write)

**Diagnosis:**

1. **Check if compaction worker is running:**
   ```bash
   ps aux | grep monofs-router | grep -i compact
   # If no output, worker crashed
   ```

2. **Check router logs for compaction errors:**
   ```bash
   journalctl -u monofs-router -n 100 | grep -i compaction
   # Look for: "compaction_failed", "s3_upload_timeout", "snapshot write failed"
   ```

3. **Check disk space:**
   ```bash
   df -h /var/lib/monofs/
   # If close to 100%, that's your problem
   ```

4. **Check S3 connectivity:**
   ```bash
   curl -I https://s3.us-west-2.amazonaws.com
   # Should be 200 or 403 (auth issue), not timeout
   ```

**Recovery:**

**Option A: Trigger manual compaction**

1. SSH to router pod:
   ```bash
   kubectl exec -it deployment/monofs-router -c router -- bash
   ```

2. Trigger compaction:
   ```bash
   monofs-admin trigger-compaction --workspace-state-dir=/var/lib/monofs/workspace/
   # Monitor output for errors
   ```

3. Monitor disk:
   ```bash
   watch -n 5 'du -sh /var/lib/monofs/workspace/jobs/wal/'
   # Should shrink over 5–10 minutes
   ```

**Option B: If disk is full, emergency cleanup**

1. **CAUTION: Only if you have S3 backups**

   ```bash
   # Delete oldest WAL segments (but keep at least 3)
   ls -t /var/lib/monofs/workspace/jobs/wal/wal-*.log | tail -n +4 | xargs rm
   # This loses audit history but frees space
   ```

2. Restart router:
   ```bash
   systemctl restart monofs-router
   ```

**Option C: Increase compaction frequency (temporary)**

1. Edit compaction config:
   ```yaml
   # /etc/monofs/router.yaml
   workspace_state:
     compaction_interval: 1m    # reduced from 5m
     wal_segment_size: 64Mi
   ```

2. Reload config:
   ```bash
   monofs-admin reload-config
   ```

3. Monitor:
   ```bash
   watch 'du -sh /var/lib/monofs/workspace/jobs/wal/ && \
          grep compaction_total /proc/$(pgrep monofs-router)/fd/4'
   ```

**Prevention:**

- Alert on WAL size: `du -s /var/lib/monofs/workspace/jobs/wal/ | awk '{if ($1 > 200000000) exit 1}'` (200 GB threshold)
- Monitor S3 upload latency: alert if >30s
- Pre-allocate disk (keep 30% free)

---

## Phase 3: Policy and Auto-Push

### Runbook P3.1: Policy Denial Blocking All Pushes

**Symptom:** All push requests return `PERMISSION_DENIED`:
```
Error: policy evaluation denied: POLICY_DEFAULT_DENY
```

**Root causes:**
- Policy YAML has `default: deny` with no matching rules
- Policy config was hot-reloaded with incorrect syntax
- Someone updated policy rules with overly restrictive pattern

**Diagnosis:**

1. **Check current policy config:**
   ```bash
   kubectl exec -it deployment/monofs-router -- \
     cat /etc/monofs/policy.yaml | head -50
   ```

2. **Validate policy syntax:**
   ```bash
   monofs-admin validate-config --policy-config /etc/monofs/policy.yaml
   ```

3. **Check which rule is matching:**
   ```bash
   monofs-admin policy-debug \
     --principal-id client-uuid-1234 \
     --workspace-id ws-prod \
     --action SOURCE_PUSH
   # Output: "rule 'block push to main on production workspaces' matched (effect: deny)"
   ```

4. **Check policy hot-reload history:**
   ```bash
   journalctl -u monofs-router | grep -E 'policy.*reload|policy.*loaded'
   ```

**Recovery:**

**Option A: Temporarily disable policy gate (emergency)**

1. Disable policy evaluation:
   ```bash
   kubectl set env deployment/monofs-router \
     WORKSPACE_POLICY_GATE_ENABLED=false
   kubectl rollout status deployment/monofs-router --timeout=5m
   ```

2. Communicate to team:
   ```
   Policy gate is DISABLED. Pushes are allowed. We're investigating.
   ```

3. Investigate and fix policy:
   ```bash
   # Review recent policy changes
   git log -p -- config/policy.yaml | head -100
   
   # Revert if needed
   git checkout HEAD~1 -- config/policy.yaml
   ```

4. Re-enable policy:
   ```bash
   kubectl set env deployment/monofs-router \
     WORKSPACE_POLICY_GATE_ENABLED=true
   ```

**Option B: Fix policy and reload (no downtime)**

1. Edit policy YAML:
   ```bash
   vim /etc/monofs/policy.yaml
   # Add a rule to allow your blocked workspace/principal
   ```

2. Validate:
   ```bash
   monofs-admin validate-config --policy-config /etc/monofs/policy.yaml
   ```

3. Hot-reload:
   ```bash
   monofs-admin reload-policy --policy-config /etc/monofs/policy.yaml
   # Or: kill -SIGHUP $(pgrep monofs-router)
   ```

4. Verify:
   ```bash
   monofs-admin policy-debug \
     --principal-id client-uuid-1234 \
     --workspace-id ws-prod \
     --action SOURCE_PUSH
   # Should now show: "allowed"
   ```

---

### Runbook P3.2: Auto-Push Job Stuck in Pending State

**Symptom:** A workspace has pending commits, auto-push is enabled, but job never transitions:
```bash
monofs-admin list-jobs --workspace=ws-prod
# Output: job-xyz [state=PENDING, created 2 hours ago, status: still pending]
```

**Root causes:**
- Auto-push worker crashed/hung
- Policy is denying the auto-push attempt (denied audit event present)
- Repository is unavailable (network error on push)
- Concurrency cap (20 concurrent pushes) is at limit

**Diagnosis:**

1. **Check if auto-push worker is alive:**
   ```bash
   ps aux | grep monofs-router | grep -i "auto"
   ```

2. **Check job status in detail:**
   ```bash
   monofs-admin inspect-job --job-id job-xyz
   # Output: state, transitions, last_error, attempt_count
   ```

3. **Check audit trail for policy denials:**
   ```bash
   monofs-admin audit-trail --job-id job-xyz --event-type policy
   # If output shows "policy.denied", that's the blocker
   ```

4. **Check if concurrency cap is at limit:**
   ```bash
   monofs-admin list-jobs --filter state=PUSHING | wc -l
   # If ≥ 20, we're at the cap
   ```

5. **Check repo connectivity:**
   ```bash
   # Try a manual push to the target repo
   git clone --depth 1 https://github.com/org/repo /tmp/test-repo
   # If this times out, network is the issue
   ```

**Recovery:**

**Option A: Manual retry (if transient network failure)**

1. Manually trigger push:
   ```bash
   monofs-admin trigger-push --job-id job-xyz
   # Will increment retry counter
   ```

2. Monitor:
   ```bash
   watch -n 5 'monofs-admin inspect-job --job-id job-xyz | grep state'
   ```

**Option B: Cancel stuck job (if giving up)**

1. Cancel the job:
   ```bash
   monofs-admin cancel-job --job-id job-xyz --reason="manual cancel due to repo unavailability"
   ```

2. Optionally, re-create a new push job:
   ```bash
   monofs-admin trigger-push --workspace=ws-prod
   ```

**Option C: Check and fix policy (if denied)**

1. Review audit trail:
   ```bash
   monofs-admin audit-trail --job-id job-xyz | grep -A 5 "policy.denied"
   ```

2. Update policy to allow the push (see Runbook P3.1)

**Prevention:**

- Monitor job age: alert if job in PENDING > 1 hour
- Monitor policy denials: alert if `policy_denied_total` > 10/min
- Monitor repo availability: synthetic push test to main repos daily

---

## General Runbooks

### Runbook GEN.1: Diagnostics Snapshot for Post-Mortems

**Use case:** An incident occurred; need to gather state for analysis.

**Collection script:**

```bash
#!/bin/bash
set -e

WORKSPACE_STATE_DIR=${WORKSPACE_STATE_DIR:-/var/lib/monofs/workspace}
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
OUTPUT_DIR="/tmp/monofs-diagnostics-$TIMESTAMP"

mkdir -p "$OUTPUT_DIR"

echo "Collecting diagnostics..."

# Router logs
journalctl -u monofs-router -n 10000 > "$OUTPUT_DIR/router-logs.txt"

# WAL state
du -sh "$WORKSPACE_STATE_DIR"/jobs/wal/ >> "$OUTPUT_DIR/wal-size.txt"
ls -lah "$WORKSPACE_STATE_DIR"/jobs/wal/ | tail -5 >> "$OUTPUT_DIR/wal-files.txt"

# Snapshot state
ls -lah "$WORKSPACE_STATE_DIR"/jobs/snapshots/ >> "$OUTPUT_DIR/snapshots.txt"
cat "$WORKSPACE_STATE_DIR"/jobs/checkpoints/checkpoint.json >> "$OUTPUT_DIR/checkpoint.json"

# Active jobs
monofs-admin list-jobs --limit 100 > "$OUTPUT_DIR/jobs.txt"

# Audit trail (last 1000 entries)
monofs-admin audit-trail --limit 1000 > "$OUTPUT_DIR/audit-trail.txt"

# Metrics
curl -s http://localhost:9090/metrics | grep -E '^(monofs|process)' > "$OUTPUT_DIR/metrics.txt"

# Policy config
cp /etc/monofs/policy.yaml "$OUTPUT_DIR/policy.yaml" 2>/dev/null || echo "N/A" > "$OUTPUT_DIR/policy.yaml"

# Compress
tar -czf "/tmp/monofs-diagnostics-$TIMESTAMP.tar.gz" "$OUTPUT_DIR"
echo "Diagnostics saved to: /tmp/monofs-diagnostics-$TIMESTAMP.tar.gz"
```

**Usage:**
```bash
bash diagnostics.sh
# Output: /tmp/monofs-diagnostics-20260703-143022.tar.gz
```

---

## Escalation Paths

| Severity | Response Time | Escalate To | Runbook |
|----------|---|---|---|
| P1: Router won't start, audit trail inaccessible | 5 min | On-call SRE | P1.1, P1.2 |
| P2: Auto-push stuck, compaction falling behind | 15 min | Platform team lead | P1.3, P3.2 |
| P3: Policy denial blocking team, but services healthy | 30 min | Policy owner | P3.1 |
| P4: Observability gap (metrics missing) | 4 hours | On-call engineer | GEN.1 |

---

## Related Documents

- Phase 1 WAL Design: `docs/readiness/phase-1-durability-and-audit.md`
- Phase 3 Policy: `docs/readiness/phase-3-policy-and-autopush.md`
- Alert Thresholds: `docs/runbooks/alerting.md` (TBD)
