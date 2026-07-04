# S3 Integration Specification — Phase 1 Audit Snapshots

## Overview

Phase 1 WAL compaction writes snapshots to object storage via the `internal/storage/auditstore/` package. This document specifies IAM permissions, lifecycle policies, availability expectations, and failure modes.

## Architecture

```
Router Process (--workspace-state-dir /var/lib/monofs/workspace/)
    ↓
    ├─ Local WAL + snapshots (hot)
    │   └─ wal-000001.log, wal-000002.log, ...
    │   └─ snapshots/snapshot-000100.json.gz (after compaction)
    │
    └─ S3 Backend (cold export)
        └─ s3://monofs-audit-{env}/
           └─ workspace-{id}/
              └─ snapshots/snapshot-000100.json.gz (versioned)
```

**Backup/recovery flow:**
1. Compaction writes local snapshot + uploads to S3
2. Local snapshot can be deleted after S3 upload confirms (with retention buffer)
3. If local state is lost, snapshots are recovered from S3

## S3 Bucket Configuration

### Bucket Naming and Location

| Environment | Bucket Name | Region | Rationale |
|-------------|-------------|--------|-----------|
| Development | `monofs-audit-dev` | `us-west-2` | Local region for fast iteration |
| Staging | `monofs-audit-staging` | `us-west-2` | Same region as staging cluster |
| Production | `monofs-audit-prod` | Multi-region (global accelerator) | High availability, low latency globally |

### Object Versioning

**Requirement: MANDATORY for all environments**

```bash
aws s3api put-bucket-versioning \
  --bucket monofs-audit-prod \
  --versioning-configuration Status=Enabled
```

**Rationale:**
- Compaction writes snapshots atomically; if a snapshot is corrupted post-write, S3 versioning allows recovery
- Deleted snapshots (after retention expiry) can be recovered via VersionID if needed for forensics
- Simplifies disaster recovery (no need for external backup service)

### Retention and Lifecycle Rules

**Retention policy:**

| Object | Hot Retention | Cold Archive | Deletion |
|--------|---------------|--------------|----------|
| Snapshot (compacted) | 30 days | 365 days (Glacier Deep Archive) | Day 366 |
| WAL segment (live) | Local only, not uploaded | N/A | Local compaction policy applies |
| Checkpoint metadata | Local only | N/A | N/A |

**Lifecycle configuration:**

```json
{
  "Rules": [
    {
      "Id": "archive-snapshots-after-30-days",
      "Status": "Enabled",
      "Filter": { "Prefix": "snapshots/" },
      "Transitions": [
        {
          "Days": 30,
          "StorageClass": "GLACIER"
        },
        {
          "Days": 90,
          "StorageClass": "DEEP_ARCHIVE"
        }
      ],
      "Expiration": { "Days": 366 }
    }
  ]
}
```

**Cost estimation (2K engineers):**

- Audit events: 80K/day = 2.4M/month
- Average snapshot size per compaction: ~50 MB
- Compactions per router per day: ~10 (every 5 min × 24h)
- Total monthly snapshots: ~300 GB
- S3 Standard (30 days): 300 GB × $0.023 = **~$7/month**
- Glacier (30–365 days): 300 GB × $0.004 = **~$1.20/month**
- Total: **~$8/month** (negligible)

### Bucket Encryption

**Requirement: MANDATORY**

```json
{
  "Rules": [
    {
      "ApplyServerSideEncryptionByDefault": {
        "SSEAlgorithm": "AES256"
      }
    }
  ]
}
```

Or use KMS if org policy requires:
```json
{
  "ApplyServerSideEncryptionByDefault": {
    "SSEAlgorithm": "aws:kms",
    "KMSMasterKeyID": "arn:aws:kms:us-west-2:123456789012:key/12345678-1234-1234-1234-123456789012"
  }
}
```

### Bucket Public Access Block

**Requirement: MANDATORY**

```bash
aws s3api put-public-access-block \
  --bucket monofs-audit-prod \
  --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
```

## IAM Policy for Router Process

**Role:** `MonoFSAuditStoreWriter`

**Trust policy:** Allows the router process (running under Kubernetes SA or EC2 IAM instance role).

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::123456789012:role/MonoFSRouterRole"
      },
      "Action": "sts:AssumeRole",
      "Condition": {
        "StringEquals": {
          "sts:ExternalId": "monofs-router-${ENVIRONMENT}"
        }
      }
    }
  ]
}
```

**Permissions policy:**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "WriteSnapshots",
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:PutObjectAcl",
        "s3:GetObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::monofs-audit-${ENVIRONMENT}",
        "arn:aws:s3:::monofs-audit-${ENVIRONMENT}/workspace-*/snapshots/*"
      ]
    },
    {
      "Sid": "DenyDeleteUnlessViaLifecycle",
      "Effect": "Deny",
      "Action": [
        "s3:DeleteObject",
        "s3:DeleteObjectVersion"
      ],
      "Resource": "arn:aws:s3:::monofs-audit-${ENVIRONMENT}/*",
      "Condition": {
        "StringNotEquals": {
          "aws:PrincipalOrgID": "${ORG_ID}"
        }
      }
    }
  ]
}
```

**Rationale:**
- PutObject: Write snapshots
- GetObject: Recover snapshots from S3 on demand
- ListBucket: Enumerate snapshots for recovery/diagnostics
- **No DeleteObject permission**: Router cannot delete snapshots; only S3 lifecycle policy can (after 366 days)

## Network Requirements

### Connectivity

**Requirement:** Router must reach S3 endpoint with low latency.

| Deployment | Connection | Latency Target | Rationale |
|-------------|-----------|---|---|
| K8s cluster in AWS | VPC Gateway Endpoint (no internet) | <10ms | No NAT, no data transfer charges |
| K8s cluster outside AWS | Public endpoint (via internet) | <50ms | CloudFront caching applies |
| On-premises | Public endpoint (via direct/VPN) | <100ms | May require proxy; consider local cache |

**S3 VPC Endpoint configuration (K8s in AWS):**

```bash
aws ec2 create-vpc-endpoint \
  --vpc-id vpc-12345 \
  --service-name com.amazonaws.us-west-2.s3 \
  --route-table-ids rtb-12345 rtb-67890
```

**DNS:** `s3.us-west-2.amazonaws.com` resolves to VPC-local IP (no data transfer charges).

### Timeout and Retry Configuration

```go
// internal/storage/auditstore/s3_client.go

type S3Config struct {
    Bucket            string
    Region            string
    Endpoint          string        // optional, for mocking/testing
    
    // Timeouts
    ConnectTimeout    time.Duration // default: 5s
    ReadTimeout       time.Duration // default: 30s
    WriteTimeout      time.Duration // default: 30s
    
    // Retries
    MaxRetries        int           // default: 3
    BackoffMultiplier float64       // default: 2.0 (exponential)
    
    // Circuit breaker
    CircuitBreakerThreshold int     // default: 5 consecutive failures → open
}
```

**Defaults for production:**
```go
const (
    ConnectTimeout = 5 * time.Second      // fail fast on network issues
    ReadTimeout    = 30 * time.Second     // snapshot reads are typically <100 MB
    WriteTimeout   = 60 * time.Second     // writes can be slower (single-threaded append)
    MaxRetries     = 3
    BackoffMult    = 2.0
)
```

## Failure Modes and Fallbacks

### Mode 1: S3 Upload Succeeds (Happy Path)

```
Compaction → Write local snapshot → Upload to S3 → Delete local snapshot
            (verified by ETag match)
```

### Mode 2: S3 Upload Fails (Transient)

```
Compaction → Write local snapshot → S3 PUT fails (throttling, timeout)
            → Retry with exponential backoff (3 attempts, max 60s total)
            → If all retries fail:
              - Log error
              - Keep local snapshot (fallback to local-only)
              - Continue operation (don't block writes)
              - Alert operator
```

**Behavior:** Router continues; audit trail is still durable locally (WAL + snapshot on disk). S3 upload is best-effort. Next compaction attempt will retry.

**Metric to alert on:** `s3_upload_failures_total` > 10 in 5 minutes → PagerDuty

### Mode 3: S3 Unavailable (Outage)

```
S3 API returns 500, 503, or connection timeout
```

**Behavior:**
- First failure: alert operator
- Subsequent failures (within circuit-breaker threshold): fast-fail (open circuit)
- Continue writing to local WAL (not blocked)
- When S3 recovers: resume uploads automatically (closed circuit)

**Config for operator:**

```yaml
# /etc/monofs/auditstore.yaml
auditstore:
  enabled: true
  s3_enabled: true         # can be set to false to disable S3 (dev/test only)
  circuit_breaker:
    failure_threshold: 5    # failures before open
    timeout: 30s            # how long to keep circuit open
    half_open_requests: 1   # test with 1 request to recover
```

### Mode 4: Local Disk Full

```
Local snapshot write fails (ENOSPC)
```

**Behavior:**
- Error is fatal (compaction aborts)
- WAL continues to grow unboundedly until disk fills completely
- Router logs critical error, emits metric
- Operator must manually free disk space or extend volume

**Prevention:**
- Disk usage monitoring (alert at 80%)
- Compaction tuning (lower threshold from 256 MB to trigger more frequently)

### Mode 5: S3 Snapshot Corruption Detected on Recovery

```
Snapshot file exists, but SHA256 hash doesn't match checkpoint
```

**Recovery steps:**
1. Router startup fails with explicit error: `snapshot checksum mismatch`
2. Operator manually verifies: `monofs-admin verify-audit --snapshot <id>`
3. If corrupted, operator deletes snapshot: `rm workspace-state-dir/snapshots/snapshot-*`
4. Router replays from earlier checkpoint (or from WAL if no checkpoint)
5. If replay succeeds, operator can optionally re-trigger compaction

**Test scenario:**
```bash
# Simulate corruption:
truncate -s -100 /var/lib/monofs/workspace/snapshots/snapshot-000100.json.gz

# Router startup should fail:
$ ./monofs-router --workspace-state-dir /var/lib/monofs/workspace/
ERROR: WAL recovery failed: snapshot checksum mismatch
       checkpoint.json: last_compacted_seq=100, sha256=abc123
       snapshot file: sha256=def456
       Action: manually verify/delete snapshot and retry
```

## Monitoring and Observability

### Metrics to Export

| Metric | Type | Labels | Example |
|--------|------|--------|---------|
| `s3_upload_success_total` | Counter | `environment`, `workspace_id` | S3 uploads that succeeded |
| `s3_upload_failures_total` | Counter | `environment`, `error_type` | S3 uploads that failed (throttle, timeout, etc.) |
| `s3_upload_duration_seconds` | Histogram | `environment` | Time to upload one snapshot |
| `s3_circuit_breaker_state` | Gauge | `environment` | 0=closed, 1=open, 2=half-open |
| `s3_download_duration_seconds` | Histogram | `environment` | Time to recover snapshot from S3 |
| `local_snapshot_size_bytes` | Gauge | `environment` | Current size of uncompacted snapshots on disk |

### Logs

**Info level:**
```
compaction_complete: seq_start=1, seq_end=500, snapshot_size=48_576_000, s3_uploaded=true
s3_upload_initiated: workspace=ws-1, snapshot_id=snapshot-000100.json.gz, size_mb=49
s3_circuit_breaker_open: failures=5, next_retry_in=30s
```

**Error level:**
```
s3_upload_failed: error=RequestTimeout, attempt=1/3, retry_in=2s
s3_upload_failed_circuit_open: giving up temporarily
snapshot_checksum_mismatch: expected=abc123, got=def456, file=snapshot-000100.json.gz
local_snapshot_size_exceeded: size_mb=512, max_mb=256 (compaction backlog)
```

## Disaster Recovery

### Scenario: Local State Lost, Need to Recover from S3

**Prerequisites:**
- S3 bucket is intact
- Operator has AWS credentials and network access to S3

**Recovery procedure:**
```bash
# 1. List available snapshots
$ aws s3 ls s3://monofs-audit-prod/workspace-ws-1/snapshots/
2026-07-03 10:00:00   49576000 snapshot-000100.json.gz
2026-07-03 10:05:00   50000000 snapshot-000101.json.gz

# 2. Download latest snapshot
$ aws s3 cp s3://monofs-audit-prod/workspace-ws-1/snapshots/snapshot-000101.json.gz \
    /var/lib/monofs/workspace/snapshots/snapshot-000101.json.gz

# 3. Restore latest checkpoint metadata
$ aws s3 cp s3://monofs-audit-prod/workspace-ws-1/checkpoints/checkpoint.json \
    /var/lib/monofs/workspace/jobs/checkpoints/checkpoint.json

# 4. Restart router (it will replay from checkpoint)
$ systemctl restart monofs-router
```

**Verification:**
```bash
$ monofs-admin audit-trail --workspace=ws-1 --count=10
```

### Scenario: S3 Account Compromise, Need to Audit Access

**Steps:**
```bash
# 1. Enable S3 access logging
aws s3api put-bucket-logging \
  --bucket monofs-audit-prod \
  --bucket-logging-status LoggingEnabled={TargetBucket=s3-access-logs,TargetPrefix=monofs-audit-prod/}

# 2. Query CloudTrail for unauthorized API calls
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=ResourceName,AttributeValue=monofs-audit-prod \
  --max-results 50
```

## Testing

### Unit Tests

- [ ] S3 upload succeeds: ETag verified
- [ ] S3 upload fails: circuit breaker opens after N retries
- [ ] S3 unavailable: circuit half-open; one successful request closes circuit
- [ ] Timeout handling: connect, read, write timeouts all trigger retries
- [ ] Checksum validation: mismatch blocks startup

### Integration Tests

- [ ] Full lifecycle: write 100 snapshots, verify all in S3 with versioning
- [ ] Recovery from S3: delete local state, restore from S3, replay succeeds
- [ ] Lifecycle policy: verify snapshots are archived to Glacier after 30 days

### Manual Testing

- [ ] Test with moto (S3 mock): `docker run --rm motoapi/moto` + `--s3-endpoint http://localhost:5000`
- [ ] Chaos: kill S3 requests mid-stream (tc/iptables) → verify circuit opens
- [ ] Cost: estimate with AWS pricing calculator

## Configuration Template

```yaml
# /etc/monofs/auditstore.yaml
auditstore:
  # Enabled at startup; can be disabled via admin RPC
  enabled: true
  
  # S3 backend
  s3_backend:
    enabled: true
    bucket: monofs-audit-prod
    region: us-west-2
    prefix: ""  # e.g. "prod/" to shard by environment
    
  # Timeouts
  connect_timeout: 5s
  read_timeout: 30s
  write_timeout: 60s
  
  # Retries
  max_retries: 3
  backoff_multiplier: 2.0
  
  # Circuit breaker
  circuit_breaker:
    failure_threshold: 5
    timeout: 30s
    half_open_requests: 1
  
  # Local caching (fallback if S3 down)
  local_cache_enabled: true
  local_cache_max_size_mb: 512
```

## Acceptance Criteria

- [ ] IAM policy restricts router to write-only (no delete/list)
- [ ] S3 bucket has versioning enabled (verified in acceptance test)
- [ ] Lifecycle policy transitions snapshots to Glacier after 30 days
- [ ] Circuit breaker opens after 5 consecutive failures
- [ ] Recovery procedure tested and documented
- [ ] Cost estimate provided for 2K-engineer scale (target: <$10/month)
- [ ] All failure modes covered by integration tests
