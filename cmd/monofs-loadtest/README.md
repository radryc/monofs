# MonoFS Load Test

A load testing tool for generating filesystem operations on a mounted MonoFS client.

## Usage

```bash
./bin/monofs-loadtest [flags]
```

## Flags

- `--mount <path>` - Path to mounted MonoFS filesystem (default: `/mnt/monofs`)
- `--duration <duration>` - Test duration (default: `60s`)
- `--concurrency <n>` - Number of concurrent workers (default: `10`)
- `--file-size <bytes>` - File size in bytes for write operations (default: `1024`)
- `--read-ratio <0.0-1.0>` - Ratio of read operations (default: `0.5`)
- `--write-ratio <0.0-1.0>` - Ratio of write operations (default: `0.3`)
- `--delete-ratio <0.0-1.0>` - Ratio of delete operations (default: `0.1`)
- `--mkdir-ratio <0.0-1.0>` - Ratio of mkdir operations (default: `0.05`)
- `--list-ratio <0.0-1.0>` - Ratio of list operations (default: `0.05`)
- `--verbose` - Enable verbose logging
- `--read-existing` - Read existing files from repositories instead of only reading files created during test
- `--repo-path <path>` - Path to repository for reading existing files (relative to mount point)
- `--max-scan-files <n>` - Maximum number of files to scan for read-existing mode (default: `10000`)
- `--read-only` - Run in read-only mode (100% reads on existing files, no writes)

## Examples

### Basic Load Test

Run a simple 60-second test with default settings:

```bash
./bin/monofs-loadtest --mount /mnt/monofs
```

### High Concurrency Test

Test with 50 concurrent workers for 5 minutes:

```bash
./bin/monofs-loadtest --mount /mnt/monofs --concurrency 50 --duration 5m
```

### Write-Heavy Workload

Focus on write operations (70% writes, 20% reads):

```bash
./bin/monofs-loadtest --mount /mnt/monofs \
  --write-ratio 0.7 \
  --read-ratio 0.2 \
  --delete-ratio 0.05 \
  --list-ratio 0.05
```

### Read-Heavy Workload

Focus on read operations (80% reads, 15% writes):

```bash
./bin/monofs-loadtest --mount /mnt/monofs \
  --read-ratio 0.8 \
  --write-ratio 0.15 \
  --delete-ratio 0.03 \
  --list-ratio 0.02
```

### Large File Test

Test with 10MB files:

```bash
./bin/monofs-loadtest --mount /mnt/monofs \
  --file-size 10485760 \
  --concurrency 5 \
  --duration 2m
```

### Quick Smoke Test

Run a quick 10-second test:

```bash
./bin/monofs-loadtest --mount /mnt/monofs --duration 10s --concurrency 3
```

### Read Existing Files (Recommended for Testing Fetchers)

Read existing files from repositories to test the full read path including fetchers:

```bash
# Read files from all repositories
./bin/monofs-loadtest --mount /mnt \
  --read-existing \
  --read-ratio 0.9 \
  --list-ratio 0.1 \
  --write-ratio 0 \
  --delete-ratio 0 \
  --concurrency 20

# Read files from a specific repository
./bin/monofs-loadtest --mount /mnt \
  --read-existing \
  --repo-path "github.com/kubernetes/kubernetes" \
  --read-only \
  --concurrency 50
```

### Pure Read-Only Test

100% reads on existing repository files (no writes, no test directory created):

```bash
./bin/monofs-loadtest --mount /mnt \
  --read-only \
  --repo-path "github.com/golang/go" \
  --concurrency 30 \
  --duration 5m
```

### Stress Test

High concurrency, longer duration, larger files:

```bash
./bin/monofs-loadtest --mount /mnt/monofs \
  --concurrency 100 \
  --duration 30m \
  --file-size 4096 \
  --verbose
```

## Sample Output

```
MonoFS Load Test
================
Mount Path:    /mnt/monofs
Test Dir:      /mnt/monofs/loadtest-1738512000
Duration:      1m0s
Concurrency:   10 workers
File Size:     1024 bytes
Read Ratio:    0.50
Write Ratio:   0.30
Delete Ratio:  0.10
Mkdir Ratio:   0.05
List Ratio:    0.05

[   5.0s] Ops:     5432 (1086.4 ops/s) | R:   2716 W:   1630 D:   543 M:   271 L:   272 | Errors:    0
[  10.0s] Ops:    10864 (1086.4 ops/s) | R:   5432 W:   3259 D:  1086 M:   543 L:   544 | Errors:    0
[  15.0s] Ops:    16296 (1086.4 ops/s) | R:   8148 W:   4889 D:  1630 M:   815 L:   814 | Errors:    0
...

Final Results
=============
Duration:          60.0s
Total Operations:  65184
Operations/sec:    1086.40

Operation Breakdown:
  Reads:           32592 (50.0%)
  Writes:          19555 (30.0%)
  Deletes:         6518 (10.0%)
  Mkdirs:          3259 (5.0%)
  Lists:           3260 (5.0%)
  Errors:          0 (0.0%)

Throughput:
  Read:            32.5 MB/s (33390 KB total)
  Write:           19.1 MB/s (20022 KB total)

✅ Test completed successfully with no errors
```

## Docker Usage

When using with Docker containers:

```bash
# SSH into a client container
ssh root@localhost -p 2222

# Run load test (assuming filesystem is mounted at /mnt/monofs)
/app/monofs-loadtest --mount /mnt/monofs --duration 2m
```

Or execute directly:

```bash
docker-compose exec client /app/monofs-loadtest --mount /mnt/monofs --duration 30s
```

## Understanding Results

### Operations per Second
- Measures throughput of filesystem operations
- Higher is generally better
- Compare across different configurations

### Operation Breakdown
- Shows percentage distribution of operations
- Should match configured ratios
- Helps identify bottlenecks

### Throughput
- Bytes read/written per second
- Important for data-intensive workloads
- Compare against baseline performance

### Error Rate
- Percentage of failed operations
- Should be 0% for healthy system
- Investigate if > 1%

## Use Cases

### Performance Baseline
Establish baseline performance metrics:
```bash
./bin/monofs-loadtest --mount /mnt/monofs --duration 5m > baseline.txt
```

### Stress Testing
Test system under heavy load:
```bash
./bin/monofs-loadtest --mount /mnt/monofs \
  --concurrency 100 \
  --duration 1h \
  --verbose
```

### Failover Testing
Run during node failover to measure impact:
```bash
# Terminal 1: Start load test
./bin/monofs-loadtest --mount /mnt/monofs --duration 10m --verbose

# Terminal 2: Trigger failover
docker-compose stop node1
```

### Capacity Planning
Determine maximum sustainable load:
```bash
for c in 10 20 50 100; do
  echo "Testing with $c workers"
  ./bin/monofs-loadtest --mount /mnt/monofs \
    --concurrency $c \
    --duration 1m | grep "Operations/sec"
done
```

### Rebalancing Impact
Measure performance during rebalancing:
```bash
# Start load test
./bin/monofs-loadtest --mount /mnt/monofs --duration 15m &

# Trigger rebalance
./bin/monofs-admin rebalance --storage-id <storage-id>
```

## Notes

- Creates a temporary test directory: `/mnt/monofs/loadtest-<timestamp>`
- Automatically cleans up test files on completion
- Each worker operates in its own subdirectory
- Uses random data for file content
- Reports progress every 5 seconds
- Safe to interrupt with Ctrl+C (will attempt cleanup)

## Building

```bash
make build-loadtest
```

The binary will be created at `bin/monofs-loadtest`.
