#!/bin/bash
# MonoFS Cluster Safe Restart Script
# Restart cluster and exit drain mode

set -e

ROUTER_ADDR="${ROUTER_ADDR:-localhost:9090}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo ""
echo "╔══════════════════════════════════════════════════════════════════╗"
echo "║           MonoFS Cluster Safe Restart Procedure                  ║"
echo "╚══════════════════════════════════════════════════════════════════╝"
echo ""

# Step 1: Start containers
echo -e "${YELLOW}[1/3] Starting cluster...${NC}"
make deploy

if [ $? -ne 0 ]; then
    echo -e "${RED}✗ Failed to start cluster${NC}"
    exit 1
fi

echo -e "${GREEN}✓ Cluster started${NC}"
echo ""

# Step 2: Wait for router to be ready
echo -e "${YELLOW}[2/3] Waiting for router to be ready...${NC}"
sleep 5

# Try to connect (with retries)
for i in {1..10}; do
    if timeout 2 bash -c "echo > /dev/tcp/localhost/9090" 2>/dev/null; then
        echo -e "${GREEN}✓ Router is ready${NC}"
        break
    fi
    if [ $i -eq 10 ]; then
        echo -e "${RED}✗ Router not responding${NC}"
        exit 1
    fi
    sleep 2
done
echo ""

# Step 3: Exit drain mode
echo -e "${YELLOW}[3/3] Exiting drain mode...${NC}"
./bin/monofs-admin undrain --router "$ROUTER_ADDR"

if [ $? -ne 0 ]; then
    echo -e "${RED}✗ Failed to undrain cluster${NC}"
    echo "Cluster is running but still in drain mode"
    echo "Run manually: ./bin/monofs-admin undrain --router $ROUTER_ADDR"
    exit 1
fi

echo -e "${GREEN}✓ Drain mode exited${NC}"
echo ""

# Completion
echo -e "${GREEN}✓ Cluster restart complete!${NC}"
echo ""
echo "Cluster is running with failover enabled."
echo "Web UI: http://localhost:8080"
echo ""
