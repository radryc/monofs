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

# Go modules from go.mod (will be ingested as Go modules, not Git repos)
GO_MODULES=(
    # Direct dependencies
    "github.com/go-git/go-git/v6@v6.0.0-20260123133532-f99a98e81ce9"
    "github.com/grafana/regexp@v0.0.0-20240607082908-2cb410fa05da"
    "github.com/hanwen/go-fuse/v2@v2.7.2"
    "github.com/nutsdb/nutsdb@v1.1.0"
    "github.com/sourcegraph/zoekt@v0.0.0-20260114143800-c747a3bccc2a"
    "google.golang.org/grpc@v1.75.0"
    "google.golang.org/protobuf@v1.36.8"
    # Indirect dependencies
    "github.com/Microsoft/go-winio@v0.6.2"
    "github.com/ProtonMail/go-crypto@v1.3.0"
    "github.com/RoaringBitmap/roaring@v1.9.4"
    "github.com/antlabs/stl@v0.0.2"
    "github.com/antlabs/timer@v0.1.4"
    "github.com/beorn7/perks@v1.0.1"
    "github.com/bits-and-blooms/bitset@v1.20.0"
    "github.com/bmatcuk/doublestar@v1.3.4"
    "github.com/bwmarrin/snowflake@v0.3.0"
    "github.com/cespare/xxhash/v2@v2.3.0"
    "github.com/cloudflare/circl@v1.6.1"
    "github.com/cyphar/filepath-securejoin@v0.6.1"
    "github.com/davecgh/go-spew@v1.1.1"
    "github.com/dustin/go-humanize@v1.0.1"
    "github.com/edsrzf/mmap-go@v1.2.0"
    "github.com/emirpasic/gods@v1.18.1"
    "github.com/fsnotify/fsnotify@v1.8.0"
    "github.com/go-enry/go-enry/v2@v2.9.1"
    "github.com/go-enry/go-oniguruma@v1.2.1"
    "github.com/go-git/gcfg/v2@v2.0.2"
    "github.com/go-git/go-billy/v6@v6.0.0-20260114122816-19306b749ecc"
    "github.com/gofrs/flock@v0.8.1"
    "github.com/golang/groupcache@v0.0.0-20241129210726-2c02b8208cf8"
    "github.com/grpc-ecosystem/go-grpc-middleware@v1.4.0"
    "github.com/kevinburke/ssh_config@v1.4.0"
    "github.com/klauspost/cpuid/v2@v2.3.0"
    "github.com/mschoch/smat@v0.2.0"
    "github.com/munnerz/goautoneg@v0.0.0-20191010083416-a7dc8b61c822"
    "github.com/opentracing/opentracing-go@v1.2.0"
    "github.com/pjbgf/sha1cd@v0.5.0"
    "github.com/pkg/errors@v0.9.1"
    "github.com/pmezard/go-difflib@v1.0.0"
    "github.com/prometheus/client_golang@v1.20.5"
    "github.com/prometheus/client_model@v0.6.1"
    "github.com/prometheus/common@v0.62.0"
    "github.com/prometheus/procfs@v0.15.1"
    "github.com/rs/xid@v1.6.0"
    "github.com/sergi/go-diff@v1.4.0"
    "github.com/sourcegraph/go-ctags@v0.0.0-20250729094530-349a251d78d8"
    "github.com/stretchr/testify@v1.11.1"
    "github.com/tidwall/btree@v1.6.0"
    "github.com/xujiajun/utils@v0.0.0-20220904132955-5f7c5b914235"
)

# Combine and shuffle both types
ALL_SOURCES=()
for repo in "${REPOS[@]}"; do
    ALL_SOURCES+=("git|$repo")
done
for mod in "${GO_MODULES[@]}"; do
    ALL_SOURCES+=("gomod|$mod")
done

SELECTED_SOURCES=($(printf '%s\n' "${ALL_SOURCES[@]}" | shuf))

echo "Selected sources to ingest (repositories + Go modules):"
echo "  Git repositories: ${#REPOS[@]}"
echo "  Go modules: ${#GO_MODULES[@]}"
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
    if [ "$type" = "gomod" ]; then
        # Go module ingestion
        cmd="$ADMIN_BIN ingest --router=\"$ROUTER\" --source=\"$source\" --ingestion-type=go --fetch-type=gomod"
    else
        # Git repository ingestion
        cmd="$ADMIN_BIN ingest --router=\"$ROUTER\" --source=\"$source\""
    fi
    
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
