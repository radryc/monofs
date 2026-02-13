#!/bin/bash
# Test script to verify fetcher-generated files are properly distributed via HRW sharding

set -e

echo "====================================="
echo "Testing Fetcher-Generated File HRW Distribution"
echo "====================================="
echo ""

# Check if router is running
if ! pgrep -x "monofs-router" > /dev/null; then
    echo "Error: monofs-router not running"
    echo "Please start the router first"
    exit 1
fi

# Check if servers are running
SERVER_COUNT=$(pgrep -x "monofs-server" | wc -l)
if [ "$SERVER_COUNT" -lt 1 ]; then
    echo "Error: No monofs-server instances running"
    echo "Please start at least one server"
    exit 1
fi

echo "Found $SERVER_COUNT server node(s) running"
echo ""

# Example: Ingest a Go module which will create fetcher-generated cache metadata files
ROUTER_ADDR="${ROUTER_ADDR:-localhost:9090}"
MODULE="github.com/google/uuid@v1.6.0"

echo "Ingesting Go module: $MODULE"
echo "This will create both regular virtual files and fetcher-generated cache metadata"
echo ""

# Check for monofs-admin binary
if [ ! -f "./bin/monofs-admin" ]; then
    echo "Building monofs-admin..."
    make bin/monofs-admin
fi

echo "Running ingestion..."
echo "Watch the router logs to see HRW distribution:"
echo ""
echo "  Look for these log entries:"
echo "    - 'creating fetcher-generated file' - Shows cache metadata files being created"
echo "    - 'HRW sharding file to node' - Shows which node each file is assigned to"
echo "    - 'node file distribution breakdown' - Summary per node"
echo ""

# Run the ingestion
./bin/monofs-admin ingest \
    --router="$ROUTER_ADDR" \
    --source="$MODULE" \
    --ingestion-type=go \
    --fetch-type=gomod

echo ""
echo "====================================="
echo "Check the router logs above for:"
echo "====================================="
echo "1. 'fetcher_generated_files' count - number of cache metadata files"
echo "2. 'regular_virtual_files' count - number of regular virtual files"
echo "3. Per-file 'HRW sharding file to node' entries showing:"
echo "   - is_fetcher_generated: true/false"
echo "   - target_node: which node received the file"
echo "   - cache_type: go-module (for fetcher-generated files)"
echo "4. Per-node distribution showing breakdown:"
echo "   - fetcher_generated: count per node"
echo "   - regular_virtual: count per node"
echo ""
echo "VERIFICATION:"
echo "  - Each file should have a target_node assigned"
echo "  - Fetcher-generated files (cache metadata) should be distributed across nodes"
echo "  - HRW ensures consistent placement based on (storage_id + ':' + file_path)"
echo ""
