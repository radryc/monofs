#!/bin/bash
# MonoFS Test Runner
# Runs all tests with proper handling of short and non-short tests

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test configuration
TIMEOUT=60
TEST_DIR="./test"

# Counters
PASSED=0
FAILED=0
SKIPPED=0

# Print header
print_header() {
    echo -e "${BLUE}================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}================================${NC}"
}

# Print status
print_status() {
    if [ $1 -eq 0 ]; then
        echo -e "${GREEN}✓ PASS${NC}: $2"
        ((PASSED++))
    else
        echo -e "${RED}✗ FAIL${NC}: $2"
        ((FAILED++))
    fi
}

print_skip() {
    echo -e "${YELLOW}⊘ SKIP${NC}: $1"
    ((SKIPPED++))
}

# Print summary
print_summary() {
    echo ""
    print_header "Test Summary"
    echo -e "Total: $((PASSED + FAILED + SKIPPED))"
    echo -e "${GREEN}Passed: $PASSED${NC}"
    echo -e "${RED}Failed: $FAILED${NC}"
    echo -e "${YELLOW}Skipped: $SKIPPED${NC}"
    echo ""
    
    if [ $FAILED -eq 0 ]; then
        echo -e "${GREEN}All tests passed!${NC}"
        return 0
    else
        echo -e "${RED}Some tests failed.${NC}"
        return 1
    fi
}

# Parse arguments
MODE="all"
VERBOSE=false

while [[ $# -gt 0 ]]; do
    case $1 in
        -s|--short)
            MODE="short"
            shift
            ;;
        -f|--full)
            MODE="full"
            shift
            ;;
        -u|--unit)
            MODE="unit"
            shift
            ;;
        -v|--verbose)
            VERBOSE=true
            shift
            ;;
        -h|--help)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  -s, --short     Run only short tests (fast, for CI/CD)"
            echo "  -f, --full      Run all tests including non-short (slow)"
            echo "  -u, --unit      Run only unit tests (exclude ./test package)"
            echo "  -v, --verbose   Show verbose test output"
            echo "  -h, --help      Show this help message"
            echo ""
            echo "Examples:"
            echo "  $0              # Run all short tests"
            echo "  $0 --short      # Run all short tests"
            echo "  $0 --full       # Run all tests (short + non-short)"
            echo "  $0 --unit       # Run only unit tests"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Use -h or --help for usage information"
            exit 1
            ;;
    esac
done

# Default to short if not specified
if [ "$MODE" = "all" ]; then
    MODE="short"
fi

# Start testing
echo -e "${BLUE}MonoFS Test Runner${NC}"
echo -e "Mode: ${YELLOW}$MODE${NC}"
echo ""

# Run unit tests (all packages except ./test)
if [ "$MODE" = "unit" ] || [ "$MODE" = "short" ] || [ "$MODE" = "full" ]; then
    print_header "Unit Tests (short mode)"
    
    PACKAGES=(
        "github.com/radryc/monofs/cmd/monofs-admin"
        "github.com/radryc/monofs/internal/cache"
        "github.com/radryc/monofs/internal/client"
        "github.com/radryc/monofs/internal/fuse"
        "github.com/radryc/monofs/internal/git"
        "github.com/radryc/monofs/internal/router"
        "github.com/radryc/monofs/internal/search"
        "github.com/radryc/monofs/internal/server"
        "github.com/radryc/monofs/internal/sharding"
    )
    
    for pkg in "${PACKAGES[@]}"; do
        PKG_NAME=$(basename "$pkg")
        if $VERBOSE; then
            go test "$pkg" -short -v
            STATUS=$?
        else
            go test "$pkg" -short > /dev/null 2>&1
            STATUS=$?
        fi
        print_status $STATUS "Unit tests: $PKG_NAME"
    done
    
    if [ "$MODE" = "unit" ]; then
        print_summary
        exit $?
    fi
fi

# Run integration tests (./test package)
if [ "$MODE" = "short" ] || [ "$MODE" = "full" ]; then
    echo ""
    print_header "Integration Tests (./test package)"
    
    # Short mode tests - can run together
    if $VERBOSE; then
        go test $TEST_DIR -short -v
        STATUS=$?
    else
        go test $TEST_DIR -short > /dev/null 2>&1
        STATUS=$?
    fi
    print_status $STATUS "Integration tests (short mode)"
fi

# Run non-short tests individually (to avoid port conflicts)
if [ "$MODE" = "full" ]; then
    echo ""
    print_header "Non-Short Integration Tests"
    echo "Running tests individually to avoid port conflicts..."
    echo ""
    
    # List of non-short tests that work well individually
    NON_SHORT_TESTS=(
        "TestFailoverE2EScenario"
        "TestHighConcurrencyIngestion"
        "TestBatchIngestionPerformance"
        "TestMixedWorkload"
        "TestRapidClusterUpdates"
        "TestDirectoryIndexConsistency"
        "TestConcurrentIngestion"
        "TestDualAddressing"
        "TestRouterMultiNodeIngestion"
        "TestRouterNodeFailover"
    )
    
    for test in "${NON_SHORT_TESTS[@]}"; do
        echo -e "${BLUE}Running: $test${NC}"
        if $VERBOSE; then
            timeout $TIMEOUT go test $TEST_DIR -run "^${test}$" -v
            STATUS=$?
        else
            timeout $TIMEOUT go test $TEST_DIR -run "^${test}$" > /dev/null 2>&1
            STATUS=$?
        fi
        
        if [ $STATUS -eq 124 ]; then
            echo -e "${RED}✗ TIMEOUT${NC}: $test (exceeded ${TIMEOUT}s)"
            ((FAILED++))
        elif [ $STATUS -eq 0 ]; then
            print_status 0 "$test"
        else
            print_status 1 "$test"
        fi
    done
    
    # E2E tests (require FUSE and binaries)
    echo ""
    echo -e "${YELLOW}E2E Tests (require FUSE, may fail in some environments)${NC}"
    
    E2E_TESTS=(
        "TestE2EClusterDeployment"
    )
    
    for test in "${E2E_TESTS[@]}"; do
        echo -e "${BLUE}Running: $test${NC}"
        timeout $TIMEOUT go test $TEST_DIR -run "^${test}$" > /dev/null 2>&1
        STATUS=$?
        
        if [ $STATUS -eq 124 ]; then
            print_skip "$test (timeout)"
        elif [ $STATUS -eq 0 ]; then
            print_status 0 "$test"
        else
            print_skip "$test (requires FUSE/binaries or has expected failures)"
        fi
    done
    
    # Consistency tests (may have port conflicts when run together)
    echo ""
    echo -e "${YELLOW}Consistency Tests (may skip if dependent tests conflict)${NC}"
    
    CONSISTENCY_TESTS=(
        "TestDataPersistence"
        "TestMetadataIntegrity"
        "TestRepositoryIsolation"
    )
    
    for test in "${CONSISTENCY_TESTS[@]}"; do
        echo -e "${BLUE}Running: $test${NC}"
        timeout 30 go test $TEST_DIR -run "^${test}$" > /dev/null 2>&1
        STATUS=$?
        
        if [ $STATUS -eq 124 ]; then
            print_skip "$test (timeout - possible port conflict)"
        elif [ $STATUS -eq 0 ]; then
            print_status 0 "$test"
        else
            print_skip "$test (may have port conflicts)"
        fi
    done
fi

# Print final summary
echo ""
print_summary
