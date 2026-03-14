#!/bin/bash
# Ingest random interesting repositories into MonoFS cluster
#
# Environment variables:
#   MONOFS_ROUTER    - Router address (default: localhost:9090)
#   MONOFS_ADMIN_BIN - Path to monofs-admin binary (default: ./bin/monofs-admin)
#   INGEST_DELAY     - Seconds between repos (default: 3)
#   BATCH_SIZE       - Repos per batch before long pause (default: 20)
#   BATCH_DELAY      - Seconds between batches (default: 60)

set -e

ROUTER="${MONOFS_ROUTER:-localhost:9090}"
ADMIN_BIN="${MONOFS_ADMIN_BIN:-./bin/monofs-admin}"

# Check if admin binary exists
if [ ! -f "$ADMIN_BIN" ]; then
    echo "❌ Error: monofs-admin binary not found at $ADMIN_BIN"
    echo "   Run 'make build' first or set MONOFS_ADMIN_BIN environment variable"
    exit 1
fi

echo "================================================"
echo "  MonoFS Repository Ingestion Script"
echo "================================================"
echo ""
echo "Router: $ROUTER"
echo "Admin:  $ADMIN_BIN"
echo "Delay between repos: ${DELAY_BETWEEN_REPOS:-3}s"
echo "Batch size: ${BATCH_SIZE:-20} repos"
echo "Batch delay: ${BATCH_DELAY:-60}s"
echo ""

# Array of interesting repositories to ingest
REPOS=(
    # Original repositories
    "https://github.com/docker/compose/tree/main"
    "https://github.com/kubernetes/kubernetes/tree/master"
    "https://github.com/golang/go/tree/master"
    "https://github.com/python/cpython/tree/main"
    "https://github.com/microsoft/vscode/tree/main"
    "https://github.com/nodejs/node/tree/main"
    "https://github.com/rust-lang/rust/tree/master"
    "https://github.com/redis/redis/tree/unstable"
    "https://github.com/postgres/postgres/tree/master"
    "https://github.com/torvalds/linux/tree/master"
    "https://github.com/elastic/elasticsearch/tree/main"
    "https://github.com/ansible/ansible/tree/devel"
    "https://github.com/hashicorp/terraform/tree/main"
    "https://github.com/llvm/llvm-project/tree/main"
    "https://github.com/arangodb/arangodb/tree/devel"
    "https://github.com/ceph/ceph/tree/main"
    "https://github.com/ClickHouse/ClickHouse/tree/master"
    "https://github.com/moby/moby/tree/master"
    "https://github.com/zephyrproject-rtos/zephyr/tree/main"
    "https://github.com/apache/spark/tree/master"
    "https://github.com/apache/kafka/tree/trunk"
    "https://github.com/apache/hadoop/tree/trunk"
    "https://github.com/apache/arrow/tree/main"
    "https://github.com/elastic/kibana/tree/main"
    "https://github.com/duckdb/duckdb/tree/main"
    "https://github.com/sqlite/sqlite/tree/master"
    "https://github.com/etcd-io/etcd/tree/main"
    "https://github.com/prometheus/prometheus/tree/main"
    "https://github.com/grafana/grafana/tree/main"
    "https://github.com/containerd/containerd/tree/main"
    "https://github.com/cilium/cilium/tree/main"
    "https://github.com/istio/istio/tree/master"
    "https://github.com/helm/helm/tree/main"
    "https://github.com/argoproj/argo-cd/tree/master"
    "https://github.com/spiffe/spire/tree/main"
    "https://github.com/hashicorp/consul/tree/main"
    "https://github.com/hashicorp/vault/tree/main"
    "https://github.com/pgbackrest/pgbackrest/tree/master"
    "https://github.com/cockroachdb/cockroach/tree/master"
    "https://github.com/flutter/flutter/tree/master"
    "https://github.com/facebook/react/tree/main"
    "https://github.com/twbs/bootstrap/tree/main"
    "https://github.com/tensorflow/tensorflow/tree/master"
    "https://github.com/electron/electron/tree/main"
    "https://github.com/pytorch/pytorch/tree/main"
    "https://github.com/microsoft/TypeScript/tree/main"
    "https://github.com/angular/angular/tree/main"
    "https://github.com/vuejs/core/tree/main"
    "https://github.com/django/django/tree/main"
    "https://github.com/rails/rails/tree/main"
    "https://github.com/spring-projects/spring-framework/tree/main"
    "https://github.com/pallets/flask/tree/main"
    "https://github.com/spring-projects/spring-boot/tree/main"
    "https://github.com/expressjs/express/tree/master"
    "https://github.com/ohmyzsh/ohmyzsh/tree/master"
    "https://github.com/Homebrew/brew/tree/master"
    "https://github.com/puppeteer/puppeteer/tree/main"
    "https://github.com/mui/material-ui/tree/master"
    "https://github.com/webpack/webpack/tree/main"
    "https://github.com/vercel/next.js/tree/canary"
)

# Combine and shuffle
ALL_SOURCES=()
for repo in "${REPOS[@]}"; do
    ALL_SOURCES+=("git|$repo")
done

SELECTED_SOURCES=($(printf '%s\n' "${ALL_SOURCES[@]}" | shuf))

echo "Selected sources to ingest:"
echo "  Git repositories: ${#REPOS[@]}"
echo "  Total: ${#SELECTED_SOURCES[@]}"
echo ""

# Configuration for rate limiting
DELAY_BETWEEN_REPOS="${INGEST_DELAY:-3}"  # seconds between each ingestion
BATCH_SIZE="${BATCH_SIZE:-20}"            # number of repos per batch
BATCH_DELAY="${BATCH_DELAY:-60}"          # seconds between batches

# Ingest each source
SUCCESS=0
FAILED=0
COUNT=0

for source_entry in "${SELECTED_SOURCES[@]}"; do
    # Parse type and source
    IFS='|' read -r type source <<< "$source_entry"
    
    echo "----------------------------------------"
    echo "Ingesting [$type]: $source [$((COUNT + 1))/${#SELECTED_SOURCES[@]}]"
    echo "----------------------------------------"
    
    # Build command based on type
    # Git repository ingestion
    cmd="$ADMIN_BIN ingest --router=\"$ROUTER\" --source=\"$source\""
    
    if eval $cmd; then
        echo "✅ Successfully ingested: $source"
        SUCCESS=$((SUCCESS + 1))
    else
        echo "❌ Failed to ingest: $source"
        FAILED=$((FAILED + 1))
    fi
    
    COUNT=$((COUNT + 1))
    
    # Rate limiting: wait between repos
    if [ $COUNT -lt ${#SELECTED_SOURCES[@]} ]; then
        echo "⏳ Waiting ${DELAY_BETWEEN_REPOS}s before next ingestion..."
        sleep "$DELAY_BETWEEN_REPOS"
    fi
    
    # Batch delay: longer wait after every batch
    if [ $((COUNT % BATCH_SIZE)) -eq 0 ] && [ $COUNT -lt ${#SELECTED_SOURCES[@]} ]; then
        echo ""
        echo "📦 Completed batch of $BATCH_SIZE sources"
        echo "⏳ Waiting ${BATCH_DELAY}s for indexing to catch up..."
        echo ""
        sleep "$BATCH_DELAY"
    fi
    
    echo ""
done

echo "================================================"
echo "  Ingestion Complete"
echo "================================================"
echo "✅ Successful: $SUCCESS"
if [ $FAILED -gt 0 ]; then
    echo "❌ Failed: $FAILED"
fi
echo ""

# Show cluster status
echo "Cluster Status:"
echo "----------------------------------------"
$ADMIN_BIN status --router="$ROUTER"
