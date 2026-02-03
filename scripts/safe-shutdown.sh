#!/bin/bash
# MonoFS Storage Nodes Safe Shutdown Script
# Safely drain and shutdown storage nodes only (keeps routers, search, and clients running)

set -e

ROUTER_ADDR="${ROUTER_ADDR:-localhost:9090}"
REASON="${REASON:-planned maintenance}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo ""
echo "╔══════════════════════════════════════════════════════════════════╗"
echo "║        MonoFS Storage Nodes Safe Shutdown Procedure              ║"
echo "╚══════════════════════════════════════════════════════════════════╝"
echo ""

# Step 1: Drain cluster
echo -e "${YELLOW}[1/3] Draining storage nodes...${NC}"
./bin/monofs-admin drain --router "$ROUTER_ADDR" --reason "$REASON"

if [ $? -ne 0 ]; then
    echo -e "${RED}✗ Failed to drain storage nodes${NC}"
    exit 1
fi

echo -e "${GREEN}✓ Storage nodes drained${NC}"
echo ""

# Step 2: Stop only storage node containers
echo -e "${YELLOW}[2/3] Stopping storage node containers...${NC}"
docker-compose stop node1 node2 node3 node4 node5

if [ $? -ne 0 ]; then
    echo -e "${RED}✗ Failed to stop storage node containers${NC}"
    exit 1
fi

echo -e "${GREEN}✓ Storage node containers stopped${NC}"
echo ""

# Step 3: Completion
echo -e "${GREEN}✓ Storage nodes shutdown complete!${NC}"
echo ""
echo "Storage nodes are now in drain mode and stopped."
echo "Routers, search service, and HAProxy remain running."
echo ""
echo "To restart storage nodes:"
echo "  1. Start storage nodes:  docker-compose start node1 node2 node3 node4 node5"
echo "  2. Exit drain mode:      ./bin/monofs-admin undrain --router $ROUTER_ADDR"
echo ""
