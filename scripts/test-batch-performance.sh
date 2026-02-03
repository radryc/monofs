#!/bin/bash
# Quick Performance Comparison Test
# This script helps compare old vs new ingestion performance

set -e

NODE_ADDR="${1:-localhost:9001}"
REPO_URL="${2:-https://github.com/vuejs/vue.git}"
BRANCH="${3:-main}"

echo "=================================="
echo "MonoFS Batch Ingestion Performance Test"
echo "=================================="
echo "Node: $NODE_ADDR"
echo "Repo: $REPO_URL"
echo "Branch: $BRANCH"
echo ""

# Make sure router and server are running
echo "⚠️  Make sure monofs-router and monofs-server are running before testing"
echo ""
read -p "Press Enter to start test..."

echo ""
echo "🚀 Starting ingestion performance test..."
echo ""

./bin/bench-node-ingest \
    -node "$NODE_ADDR" \
    -url "$REPO_URL" \
    -branch "$BRANCH" \
    -parallel 100

echo ""
echo "=================================="
echo "Test Complete!"
echo "=================================="
echo ""
echo "Expected performance with batch optimization:"
echo "  - Small repos (< 1k files): 200-250 files/sec"
echo "  - Medium repos (1k-10k files): 200-300 files/sec"
echo "  - Large repos (> 10k files): 200-300 files/sec"
echo ""
echo "Previous performance (without batch):"
echo "  - All repos: ~10 files/sec"
echo ""
echo "Expected improvement: 20-30x faster"
echo ""
