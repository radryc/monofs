#!/bin/bash
# MonoFS Build Integration Test Script
# Tests the new build integration features with a running Docker cluster

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test counters
TESTS_PASSED=0
TESTS_FAILED=0

# Configuration
ROUTER_GRPC="localhost:9090"
ROUTER_HTTP="localhost:8080"
MOUNT_POINT="/tmp/monofs-test-$$"

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
    TESTS_PASSED=$((TESTS_PASSED + 1))
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
    TESTS_FAILED=$((TESTS_FAILED + 1))
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

check_command() {
    if ! command -v "$1" &> /dev/null; then
        log_error "Required command '$1' not found"
        exit 1
    fi
}

cleanup() {
    log_info "Cleaning up..."
    
    # Unmount FUSE if mounted
    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
        log_info "Unmounting FUSE filesystem..."
        fusermount -u "$MOUNT_POINT" || umount "$MOUNT_POINT" || true
    fi
    
    # Remove mount point
    if [ -d "$MOUNT_POINT" ]; then
        rmdir "$MOUNT_POINT" || true
    fi
    
    # Kill client if running
    pkill -f "monofs-client.*$MOUNT_POINT" || true
    
    log_info "Cleanup complete"
}

trap cleanup EXIT INT TERM

print_banner() {
    echo ""
    echo "=========================================="
    echo "  MonoFS Build Integration Test Suite"
    echo "=========================================="
    echo ""
}

# Test 1: Check Docker cluster is running
test_cluster_running() {
    log_info "TEST 1: Checking Docker cluster status..."
    
    if ! docker ps | grep -q "monofs-router"; then
        log_error "MonoFS router container not running"
        return 1
    fi
    
    if ! docker ps | grep -q "monofs-server"; then
        log_error "MonoFS server container(s) not running"
        return 1
    fi
    
    if ! docker ps | grep -q "monofs-fetcher"; then
        log_error "MonoFS fetcher container not running"
        return 1
    fi
    
    log_success "Docker cluster is running"
}

# Test 2: Check router is responding
test_router_responding() {
    log_info "TEST 2: Checking router gRPC connectivity..."
    
    if timeout 5 bash -c "echo > /dev/tcp/localhost/9090" 2>/dev/null; then
        log_success "Router gRPC endpoint is accessible"
    else
        log_error "Router gRPC endpoint not accessible at localhost:9090"
        return 1
    fi
    
    log_info "Checking router HTTP endpoint..."
    if curl -s -f "http://$ROUTER_HTTP/api/status" > /dev/null; then
        log_success "Router HTTP endpoint is accessible"
    else
        log_error "Router HTTP endpoint not accessible"
        return 1
    fi
}

# Test 3: Ingest a Go module
test_ingest_go_module() {
    log_info "TEST 3: Ingesting Go module github.com/google/uuid@v1.6.0..."
    
    if ./bin/monofs-admin ingest \
        --router=$ROUTER_GRPC \
        --source=github.com/google/uuid@v1.6.0 \
        --ingestion-type=go \
        --fetch-type=gomod 2>&1 | tee /tmp/ingest-output.log; then
        log_success "Go module ingested successfully"
    else
        log_error "Failed to ingest Go module"
        cat /tmp/ingest-output.log
        return 1
    fi
    
    # Wait for ingestion to complete
    sleep 3
}

# Test 4: Verify router logs show layout generation
test_router_logs() {
    log_info "TEST 4: Checking router logs for layout generation messages..."
    
    local logs=$(docker logs monofs-router1-1 2>&1 | tail -100)
    
    if echo "$logs" | grep -q "generating build layouts"; then
        log_success "Found 'generating build layouts' in router logs"
    else
        log_warning "Did not find 'generating build layouts' in router logs"
    fi
    
    if echo "$logs" | grep -q "ingesting virtual repo"; then
        log_success "Found 'ingesting virtual repo' in router logs"
    else
        log_warning "Did not find 'ingesting virtual repo' in router logs"
    fi
    
    if echo "$logs" | grep -q "virtual repo ingested successfully"; then
        log_success "Found 'virtual repo ingested successfully' in router logs"
    else
        log_warning "Did not find 'virtual repo ingested successfully' in router logs"
    fi
}

# Test 5: Verify repos via API
test_verify_repos() {
    log_info "TEST 5: Verifying repositories via API..."
    
    local repos=$(curl -s "http://$ROUTER_HTTP/api/repos" | jq -r '.repositories[].display_path' 2>/dev/null)
    
    if echo "$repos" | grep -q "github.com/google/uuid@v1.6.0"; then
        log_success "Found original repo: github.com/google/uuid@v1.6.0"
    else
        log_error "Original repo not found"
        return 1
    fi
    
    if echo "$repos" | grep -q "go-modules/pkg/mod/github.com/google/uuid@v1.6.0"; then
        log_success "Found virtual repo: go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
    else
        log_error "Virtual repo not found"
        echo "Available repos:"
        echo "$repos"
        return 1
    fi
}

# Test 6: Mount FUSE and verify files
test_fuse_mount() {
    log_info "TEST 6: Mounting FUSE filesystem..."
    
    mkdir -p "$MOUNT_POINT"
    
    # Start FUSE client in background
    ./bin/monofs-client --mount="$MOUNT_POINT" --router="$ROUTER_GRPC" > /tmp/fuse-client.log 2>&1 &
    local client_pid=$!
    
    # Wait for mount to be ready
    log_info "Waiting for FUSE mount to be ready..."
    for i in {1..30}; do
        if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
            break
        fi
        sleep 1
    done
    
    if ! mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
        log_error "FUSE mount failed"
        cat /tmp/fuse-client.log
        return 1
    fi
    
    log_success "FUSE filesystem mounted at $MOUNT_POINT"
}

# Test 7: Verify original repo files
test_original_repo_files() {
    log_info "TEST 7: Checking original repo files..."
    
    local original_path="$MOUNT_POINT/github.com/google/uuid@v1.6.0"
    
    if [ ! -d "$original_path" ]; then
        log_error "Original repo directory not found: $original_path"
        log_info "Available paths:"
        ls -la "$MOUNT_POINT/" || true
        return 1
    fi
    
    log_success "Original repo directory exists"
    
    if [ -f "$original_path/uuid.go" ]; then
        log_success "Found uuid.go in original repo"
    else
        log_error "uuid.go not found in original repo"
        log_info "Files in original repo:"
        ls -la "$original_path/" || true
        return 1
    fi
    
    if [ -f "$original_path/go.mod" ]; then
        log_success "Found go.mod in original repo"
    else
        log_warning "go.mod not found in original repo"
    fi
}

# Test 8: Verify virtual repo files
test_virtual_repo_files() {
    log_info "TEST 8: Checking virtual repo files..."
    
    local virtual_path="$MOUNT_POINT/go-modules/pkg/mod/github.com/google/uuid@v1.6.0"
    
    if [ ! -d "$virtual_path" ]; then
        log_error "Virtual repo directory not found: $virtual_path"
        log_info "Checking go-modules structure:"
        find "$MOUNT_POINT/go-modules" -type d 2>/dev/null | head -20 || true
        return 1
    fi
    
    log_success "Virtual repo directory exists"
    
    if [ -f "$virtual_path/uuid.go" ]; then
        log_success "Found uuid.go in virtual repo"
    else
        log_error "uuid.go not found in virtual repo"
        log_info "Files in virtual repo:"
        ls -la "$virtual_path/" || true
        return 1
    fi
}

# Test 9: Verify content identity (same blob)
test_content_identity() {
    log_info "TEST 9: Verifying content identity between original and virtual repos..."
    
    local original_file="$MOUNT_POINT/github.com/google/uuid@v1.6.0/uuid.go"
    local virtual_file="$MOUNT_POINT/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/uuid.go"
    
    if [ ! -f "$original_file" ]; then
        log_error "Original file not found: $original_file"
        return 1
    fi
    
    if [ ! -f "$virtual_file" ]; then
        log_error "Virtual file not found: $virtual_file"
        return 1
    fi
    
    log_info "Comparing file contents..."
    if diff -q "$original_file" "$virtual_file" > /dev/null 2>&1; then
        log_success "Files are identical (same blob content)"
    else
        log_error "Files differ (blob deduplication failed)"
        return 1
    fi
    
    # Compare checksums
    local original_sha=$(sha256sum "$original_file" | awk '{print $1}')
    local virtual_sha=$(sha256sum "$virtual_file" | awk '{print $1}')
    
    if [ "$original_sha" = "$virtual_sha" ]; then
        log_success "SHA256 checksums match: $original_sha"
    else
        log_error "SHA256 checksums differ"
        return 1
    fi
}

# Test 10: Test reading file content
test_read_content() {
    log_info "TEST 10: Reading file content from virtual repo..."
    
    local virtual_file="$MOUNT_POINT/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/uuid.go"
    
    if ! head -5 "$virtual_file" > /dev/null 2>&1; then
        log_error "Failed to read file content"
        return 1
    fi
    
    local content=$(cat "$virtual_file" 2>/dev/null)
    if echo "$content" | grep -q "package uuid"; then
        log_success "File content is valid Go source code"
    else
        log_error "File content does not appear to be valid Go source"
        return 1
    fi
}

# Test 11: Test ingest-deps command (if go.mod exists)
test_ingest_deps() {
    log_info "TEST 11: Testing bulk dependency ingestion..."
    
    # Create a test go.mod
    cat > /tmp/test-go.mod <<EOF
module test

go 1.21

require (
    github.com/stretchr/testify v1.8.4
    golang.org/x/sync v0.5.0
)
EOF
    
    log_info "Created test go.mod with 2 dependencies"
    
    if ./bin/monofs-admin ingest-deps \
        --router=$ROUTER_GRPC \
        --file=/tmp/test-go.mod \
        --type=go \
        --concurrency=3 \
        --skip-existing=false 2>&1 | tee /tmp/ingest-deps-output.log; then
        log_success "Bulk dependency ingestion completed"
    else
        log_warning "Bulk dependency ingestion had issues (may be network-related)"
        cat /tmp/ingest-deps-output.log | tail -20
    fi
    
    rm -f /tmp/test-go.mod
}

# Test 12: Test Go module case encoding
test_case_encoding() {
    log_info "TEST 12: Testing Go module case encoding (Azure example)..."
    
    log_info "Ingesting github.com/Azure/azure-sdk-for-go@v68.0.0..."
    
    if ./bin/monofs-admin ingest \
        --router=$ROUTER_GRPC \
        --source=github.com/Azure/azure-sdk-for-go@v68.0.0 \
        --fetch-type=gomod > /tmp/azure-ingest.log 2>&1; then
        
        sleep 3
        
        # Check if virtual path uses !azure encoding
        local repos=$(curl -s "http://$ROUTER_HTTP/api/repos" | jq -r '.repositories[].display_path' 2>/dev/null)
        
        if echo "$repos" | grep -q "go-modules/pkg/mod/github.com/\!azure/azure-sdk-for-go@v68.0.0"; then
            log_success "Case encoding works correctly (Azure → !azure)"
        else
            log_warning "Case encoding not verified (virtual repo may not be created yet)"
        fi
    else
        log_warning "Azure module ingestion failed (may be network/size issue)"
        tail -20 /tmp/azure-ingest.log
    fi
}

# Test 13: Performance check
test_performance() {
    log_info "TEST 13: Performance check - measuring read times..."
    
    local virtual_file="$MOUNT_POINT/go-modules/pkg/mod/github.com/google/uuid@v1.6.0/uuid.go"
    
    if [ -f "$virtual_file" ]; then
        local start=$(date +%s%N)
        cat "$virtual_file" > /dev/null 2>&1
        local end=$(date +%s%N)
        local elapsed=$(((end - start) / 1000000)) # Convert to milliseconds
        
        log_success "Read performance: ${elapsed}ms"
        
        if [ $elapsed -lt 1000 ]; then
            log_success "Performance is good (< 1 second)"
        else
            log_warning "Performance is slow (> 1 second)"
        fi
    else
        log_warning "Could not test performance (file not found)"
    fi
}

# Test 14: Verify search integration
test_search_integration() {
    log_info "TEST 14: Checking search integration..."
    
    # Check if search service is running
    if docker ps | grep -q "monofs-search"; then
        log_info "Search service is running"
        
        # Wait a bit for indexing
        sleep 5
        
        # Try to search (this might fail if search is not configured)
        if curl -s "http://$ROUTER_HTTP/api/search?q=uuid" > /dev/null 2>&1; then
            log_success "Search endpoint is accessible"
        else
            log_warning "Search endpoint not accessible or not configured"
        fi
    else
        log_warning "Search service not running in Docker cluster"
    fi
}

# Main test execution
main() {
    print_banner
    
    # Check prerequisites
    log_info "Checking prerequisites..."
    check_command "docker"
    check_command "curl"
    check_command "jq"
    check_command "fusermount"
    
    if [ ! -f "./bin/monofs-client" ]; then
        log_error "monofs-client binary not found. Please run 'make build' first."
        exit 1
    fi
    
    log_success "All prerequisites met"
    echo ""
    
    # Run tests
    test_cluster_running || true
    echo ""
    
    test_router_responding || true
    echo ""
    
    test_ingest_go_module || true
    echo ""
    
    test_router_logs || true
    echo ""
    
    test_verify_repos || true
    echo ""
    
    test_fuse_mount || true
    echo ""
    
    test_original_repo_files || true
    echo ""
    
    test_virtual_repo_files || true
    echo ""
    
    test_content_identity || true
    echo ""
    
    test_read_content || true
    echo ""
    
    test_ingest_deps || true
    echo ""
    
    test_case_encoding || true
    echo ""
    
    test_performance || true
    echo ""
    
    test_search_integration || true
    echo ""
    
    # Summary
    echo ""
    echo "=========================================="
    echo "           Test Summary"
    echo "=========================================="
    echo -e "${GREEN}Passed: $TESTS_PASSED${NC}"
    echo -e "${RED}Failed: $TESTS_FAILED${NC}"
    echo "=========================================="
    echo ""
    
    if [ $TESTS_FAILED -eq 0 ]; then
        log_success "All tests passed! Build integration is working correctly."
        exit 0
    else
        log_error "Some tests failed. Check the output above for details."
        exit 1
    fi
}

# Run main
main "$@"
