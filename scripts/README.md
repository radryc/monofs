# MonoFS Scripts

This directory contains utility scripts for MonoFS development and operations.

## Test Scripts

### run-tests.sh

Comprehensive test runner that handles all MonoFS tests properly.

**Usage:**
```bash
./scripts/run-tests.sh [OPTIONS]
```

**Options:**
- `-s, --short` - Run only short tests (fast, recommended for CI/CD)
- `-f, --full` - Run all tests including non-short tests (slow, comprehensive)
- `-u, --unit` - Run only unit tests (excludes ./test package)
- `-v, --verbose` - Show verbose test output
- `-h, --help` - Show help message

**Examples:**
```bash
# Run short tests (default, recommended for CI/CD)
./scripts/run-tests.sh
./scripts/run-tests.sh --short

# Run all tests including slow integration tests
./scripts/run-tests.sh --full

# Run only unit tests
./scripts/run-tests.sh --unit

# Run with verbose output
./scripts/run-tests.sh --short --verbose
```

**Test Categories:**

1. **Unit Tests** (all packages except ./test)
   - Fast, no external dependencies
   - Run with `-s` or `-u` flag

2. **Integration Tests (short mode)** (./test package with `-short`)
   - Fast integration tests
   - Run together without issues
   - Included in `-s` flag

3. **Non-Short Integration Tests**
   - Slower, more comprehensive
   - Run individually to avoid port conflicts
   - Included in `-f` flag only
   - Examples: TestFailoverE2EScenario, TestHighConcurrencyIngestion

4. **E2E Tests**
   - Require FUSE and built binaries
   - May have expected failures in some environments
   - Included in `-f` flag only

## Deployment Scripts

### ingest-random-repos.sh

Ingests random repositories for testing.

### safe-restart.sh

Safely restarts MonoFS server nodes with graceful shutdown.

### safe-shutdown.sh

Performs graceful shutdown of MonoFS cluster.

## Test Scripts (Python)

### test_failover.py

Python-based failover testing script.

### test-batch-performance.sh

Tests batch ingestion performance.

## CI/CD Recommendations

For continuous integration, use:

```bash
./scripts/run-tests.sh --short
```

This runs all fast tests (unit + short integration) in ~15 seconds and is suitable for CI/CD pipelines.

For comprehensive pre-release testing:

```bash
./scripts/run-tests.sh --full
```

This runs all tests but may take several minutes and requires more resources.
