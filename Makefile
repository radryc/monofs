# MonoFS Makefile
# Distributed FUSE filesystem for Git

# Go parameters
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOMOD := $(GOCMD) mod
GOVET := $(GOCMD) vet
GOFMT := gofmt

# Binary names
SERVER_BINARY := monofs-server
CLIENT_BINARY := monofs-client
ROUTER_BINARY := monofs-router
ADMIN_BINARY := monofs-admin
SESSION_BINARY := monofs-session
SEARCH_BINARY := monofs-search
FETCHER_BINARY := monofs-fetcher

# Directories
BIN_DIR := bin
CMD_DIR := cmd
PROTO_DIR := api/proto
KMOD_DIR := monofs-kmod

# Version information
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Build flags
LDFLAGS := -ldflags "-s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)"
BUILD_FLAGS := -trimpath

# Proto tools
PROTOC := protoc
PROTOC_GEN_GO := protoc-gen-go
PROTOC_GEN_GO_GRPC := protoc-gen-go-grpc

# Docker
DOCKER := docker
DOCKER_COMPOSE := docker compose

# Load .env file if it exists (for MONOFS_ENCRYPTION_KEY and other env vars)
ifneq (,$(wildcard ./.env))
    include .env
    export
endif

# Router UI aggregation
PEER_ROUTERS ?=
ROUTER_A_PEERS ?= router-b=http://router-b:8080
ROUTER_B_PEERS ?= router-a=http://router-a:8080

# Default target
.DEFAULT_GOAL := build

# Phony targets
.PHONY: all build build-server build-client build-router build-admin build-session build-search build-fetcher build-loadtest build-modverify build-kmod clean clean-kmod proto proto-check \
        test test-unit test-e2e test-e2e-sudo test-smoke test-race test-coverage vet fmt fmt-check tidy \
        install-tools run-server run-client run-router run-cluster help \
        deploy deploy-s3 deploy-s3-external deploy-s3-clean deploy-stop deploy-clean deploy-restart deploy-local deploy-local-stop deploy-local-clean deploy-local-restart \
        mount mount-writable unmount mount-kmod umount-kmod \
        docker-build docker-up docker-down docker-logs docker-clean docker-restart

##@ General

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

all: clean proto build test-unit ## Clean, generate proto, build, and run unit tests

##@ Build

build: build-server build-client build-router build-admin build-session build-search build-fetcher build-loadtest ## Build all binaries

build-server: $(BIN_DIR) ## Build the server binary
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(SERVER_BINARY) ./$(CMD_DIR)/$(SERVER_BINARY)
	@echo "Built $(BIN_DIR)/$(SERVER_BINARY)"

build-client: $(BIN_DIR) ## Build the client binary
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(CLIENT_BINARY) ./$(CMD_DIR)/$(CLIENT_BINARY)
	@echo "Built $(BIN_DIR)/$(CLIENT_BINARY)"

build-router: $(BIN_DIR) ## Build the router binary
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(ROUTER_BINARY) ./$(CMD_DIR)/$(ROUTER_BINARY)
	@echo "Built $(BIN_DIR)/$(ROUTER_BINARY)"

build-admin: $(BIN_DIR) ## Build the admin CLI binary
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(ADMIN_BINARY) ./$(CMD_DIR)/$(ADMIN_BINARY)
	@echo "Built $(BIN_DIR)/$(ADMIN_BINARY)"

build-session: $(BIN_DIR) ## Build the session CLI binary
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(SESSION_BINARY) ./$(CMD_DIR)/$(SESSION_BINARY)
	@echo "Built $(BIN_DIR)/$(SESSION_BINARY)"

build-search: $(BIN_DIR) ## Build the search service binary
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(SEARCH_BINARY) ./$(CMD_DIR)/$(SEARCH_BINARY)
	@echo "Built $(BIN_DIR)/$(SEARCH_BINARY)"

build-fetcher: $(BIN_DIR) ## Build the fetcher service binary
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(FETCHER_BINARY) ./$(CMD_DIR)/$(FETCHER_BINARY)
	@echo "Built $(BIN_DIR)/$(FETCHER_BINARY)"

build-loadtest: $(BIN_DIR) ## Build the load test binary
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/monofs-loadtest ./$(CMD_DIR)/monofs-loadtest
	@echo "Built $(BIN_DIR)/monofs-loadtest"

build-modverify: $(BIN_DIR) ## Build the module verification tool
	$(GOBUILD) $(BUILD_FLAGS) $(LDFLAGS) -o $(BIN_DIR)/modverify ./$(CMD_DIR)/modverify
	@echo "Built $(BIN_DIR)/modverify"

build-kmod: ## Build the out-of-tree kernel module scaffold
	@$(MAKE) -C $(KMOD_DIR) all
	@echo "Built $(KMOD_DIR)/monofs.ko"

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

clean: ## Remove build artifacts
	@rm -rf $(BIN_DIR)
	@rm -f coverage.out coverage.html
	@$(MAKE) -C $(KMOD_DIR) clean >/dev/null 2>&1 || true
	@echo "Cleaned build artifacts"

clean-kmod: ## Remove kernel module build artifacts
	@$(MAKE) -C $(KMOD_DIR) clean
	@echo "Cleaned $(KMOD_DIR) build artifacts"

##@ Proto

proto: proto-check ## Generate Go code from proto files
	$(PROTOC) \
		--go_out=. --go_opt=module=github.com/radryc/monofs \
		--go-grpc_out=. --go-grpc_opt=module=github.com/radryc/monofs \
		$(PROTO_DIR)/*.proto
	@echo "Generated proto files"

proto-check: ## Check if protoc and plugins are installed
	@which $(PROTOC) > /dev/null || (echo "Error: protoc not found. Install protobuf compiler." && exit 1)
	@which $(PROTOC_GEN_GO) > /dev/null || (echo "Error: protoc-gen-go not found. Run 'make install-tools'" && exit 1)
	@which $(PROTOC_GEN_GO_GRPC) > /dev/null || (echo "Error: protoc-gen-go-grpc not found. Run 'make install-tools'" && exit 1)

##@ Development

run-server: build-server ## Run the server with debug logging (NODE_ID required)
	@if [ -z "$(NODE_ID)" ]; then \
		echo "Usage: make run-server NODE_ID=node1"; \
		exit 1; \
	fi
	./$(BIN_DIR)/$(SERVER_BINARY) --node-id=$(NODE_ID) --debug

run-router: build-router ## Run the router with specified nodes
	./$(BIN_DIR)/$(ROUTER_BINARY) --port=9090 $(if $(PEER_ROUTERS),--peer-routers=$(PEER_ROUTERS),)

run-client: build-client ## Run the client (requires MOUNT_POINT env var)
	@if [ -z "$(MOUNT_POINT)" ]; then \
		echo "Usage: make run-client MOUNT_POINT=/path/to/mount"; \
		exit 1; \
	fi
	@mkdir -p $(MOUNT_POINT)
	./$(BIN_DIR)/$(CLIENT_BINARY) --server=localhost:9000 --mount=$(MOUNT_POINT) --debug

# Run local cluster (3 nodes + router)
run-cluster: build ## Run a local 3-node cluster
	@echo "Starting local cluster..."
	@./$(BIN_DIR)/$(SERVER_BINARY) --node-id=node1 --addr=:9001 &
	@./$(BIN_DIR)/$(SERVER_BINARY) --node-id=node2 --addr=:9002 &
	@./$(BIN_DIR)/$(SERVER_BINARY) --node-id=node3 --addr=:9003 &
	@sleep 1
	@./$(BIN_DIR)/$(ROUTER_BINARY) --port=9090 \
		--nodes=node1=localhost:9001,node2=localhost:9002,node3=localhost:9003
	@echo "Cluster started. Router at localhost:9090"

deploy: ## Rebuild and deploy Docker cluster (router + 3 nodes)
	@echo "======================================"
	@echo "MonoFS Docker Deployment"
	@echo "======================================"
	@echo ""
	@echo "Checking for encryption key..."
	@if [ -z "$(MONOFS_ENCRYPTION_KEY)" ]; then \
		echo "❌ ERROR: MONOFS_ENCRYPTION_KEY environment variable is not set!"; \
		echo ""; \
		echo "Generate a key with: openssl rand -hex 32"; \
		echo ""; \
		echo "Then either:"; \
		echo "  1. Export it: export MONOFS_ENCRYPTION_KEY=\$$(openssl rand -hex 32)"; \
		echo "  2. Add to .env file: echo MONOFS_ENCRYPTION_KEY=\$$(openssl rand -hex 32) >> .env"; \
		echo ""; \
		exit 1; \
	fi
	@echo "✅ Encryption key is set"
	@echo ""
	@echo "Version: $(VERSION) ($(COMMIT))"
	@echo "Build Time: $(BUILD_TIME)"
	@echo ""
	@echo "Building Docker images..."
	GIT_VERSION=$(VERSION) GIT_COMMIT=$(COMMIT) BUILD_TIME=$(BUILD_TIME) \
		ROUTER_A_PEERS=$(ROUTER_A_PEERS) ROUTER_B_PEERS=$(ROUTER_B_PEERS) \
		$(DOCKER_COMPOSE) build
	@echo ""
	@echo "Starting services..."
	@GIT_VERSION=$(VERSION) GIT_COMMIT=$(COMMIT) BUILD_TIME=$(BUILD_TIME) \
		ROUTER_A_PEERS=$(ROUTER_A_PEERS) ROUTER_B_PEERS=$(ROUTER_B_PEERS) \
		$(DOCKER_COMPOSE) up -d
	@sleep 2
	@echo ""
	@echo "======================================"
	@echo "✅ Deployment Complete!"
	@echo "======================================"
	@echo ""
	@echo "Architecture:"
	@echo "  🔀 HAProxy:      localhost:9090 (gRPC) / localhost:8080 (HTTP)"
	@echo "  📊 HAProxy Stats: http://localhost:8404/stats"
	@echo "  📡 Router 1:     Internal (load balanced)"
	@echo "  📡 Router 2:     Internal (load balanced)"
	@echo "  💾 Backend 1-5:  Internal cluster nodes"
	@echo "  🔍 Search:       Internal (Zoekt-based code search)"
	@echo ""
	@echo "Access Points:"
	@echo "  🌐 Web UI:       http://localhost:8080"
	@echo "  📡 gRPC API:     localhost:9090"
	@echo "  🖥️  SSH Client:   ssh -p 2222 monofs@localhost (auto-mounted)"
	@echo ""
	@echo "Web UI Pages:"
	@echo "  📊 Dashboard:     http://localhost:8080/"
	@echo "  🌐 Cluster:       http://localhost:8080/cluster"
	@echo "  🛡️  Replication:  http://localhost:8080/replication"
	@echo "  📦 Repositories:  http://localhost:8080/repositories"
	@echo "  ⬆️  Ingest:        http://localhost:8080/ingest"
	@echo "  🔍 Search:        http://localhost:8080/search"
	@echo ""
	@echo "Admin CLI:"
	@echo "  ./bin/monofs-admin status --router=localhost:9090"
	@echo "  ./bin/monofs-admin failover --router=localhost:9090"
	@echo "  ./bin/monofs-admin ingest --url=<repo-url>"
	@echo ""
	@echo "Logs & Management:"
	@echo "  make docker-logs    - View container logs"
	@echo "  make deploy-restart - Restart cluster"
	@echo "  make deploy-stop    - Stop cluster"
	@echo "  make deploy-clean   - Stop and remove all data"
	@echo "======================================"

deploy-s3: ## Deploy Docker cluster with MinIO S3 backend
	@echo "======================================"
	@echo "MonoFS Docker Deployment with MinIO S3"
	@echo "======================================"
	@echo ""
	@echo "Checking for encryption key..."
	@if [ -z "$(MONOFS_ENCRYPTION_KEY)" ]; then \
		echo "❌ ERROR: MONOFS_ENCRYPTION_KEY environment variable is not set!"; \
		echo ""; \
		echo "Generate a key with: openssl rand -hex 32"; \
		echo ""; \
		echo "Then either:"; \
		echo "  1. Export it: export MONOFS_ENCRYPTION_KEY=\$$(openssl rand -hex 32)"; \
		echo "  2. Add to .env file: echo MONOFS_ENCRYPTION_KEY=\$$(openssl rand -hex 32) >> .env"; \
		echo ""; \
		exit 1; \
	fi
	@echo "✅ Encryption key is set"
	@echo ""
	@echo "Building Docker images..."
	GIT_VERSION=$(VERSION) GIT_COMMIT=$(COMMIT) BUILD_TIME=$(BUILD_TIME) \
		ROUTER_A_PEERS=$(ROUTER_A_PEERS) ROUTER_B_PEERS=$(ROUTER_B_PEERS) \
		$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3.yml build
	@echo ""
	@echo "Starting services..."
	@GIT_VERSION=$(VERSION) GIT_COMMIT=$(COMMIT) BUILD_TIME=$(BUILD_TIME) \
		ROUTER_A_PEERS=$(ROUTER_A_PEERS) ROUTER_B_PEERS=$(ROUTER_B_PEERS) \
		$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3.yml up -d
	@sleep 2
	@echo ""
	@echo "======================================"
	@echo "✅ Deployment Complete!"
	@echo "======================================"
	@echo ""
	@echo "Architecture:"
	@echo "  🔀 HAProxy:      localhost:9090 (gRPC) / localhost:8080 (HTTP)"
	@echo "  📊 HAProxy Stats: http://localhost:8404/stats"
	@echo "  🪣 MinIO S3:     localhost:19000 (API) / localhost:19001 (Console)"
	@echo "  📡 Router 1-2:   Internal (load balanced)"
	@echo "  💾 Backend 1-5:  Internal cluster nodes"
	@echo "  🔍 Search:       Internal (Zoekt-based code search)"
	@echo ""
	@echo "Access Points:"
	@echo "  🌐 Web UI:       http://localhost:8080"
	@echo "  📡 gRPC API:     localhost:9090"
	@echo "  🪣 MinIO Console: http://localhost:19001 (minioadmin/minioadmin)"
	@echo "  🖥️  SSH Client:   ssh -p 2222 monofs@localhost (auto-mounted)"
	@echo ""
	@echo "Admin CLI:"
	@echo "  ./bin/monofs-admin status --router=localhost:9090"
	@echo "  ./bin/monofs-admin ingest --url=<repo-url>"
	@echo ""
	@echo "Logs & Management:"
	@echo "  make docker-logs    - View container logs"
	@echo "  make deploy-stop    - Stop cluster"
	@echo "======================================"

deploy-s3-external: ## Deploy Docker cluster using existing external MinIO
	@echo "======================================"
	@echo "MonoFS Docker Deployment (External MinIO)"
	@echo "======================================"
	@echo ""
	@echo "Using external MinIO at: ${MONOFS_S3_ENDPOINT:-http://localhost:19000}"
	@echo ""
	@if [ -z "$(MONOFS_ENCRYPTION_KEY)" ]; then \
		echo "❌ ERROR: MONOFS_ENCRYPTION_KEY environment variable is not set!"; \
		echo ""; \
		echo "Generate a key with: openssl rand -hex 32"; \
		echo ""; \
		echo "Then either:"; \
		echo "  1. Export it: export MONOFS_ENCRYPTION_KEY=\$$(openssl rand -hex 32)"; \
		echo "  2. Add to .env file: echo MONOFS_ENCRYPTION_KEY=\$$(openssl rand -hex 32) >> .env"; \
		echo ""; \
		exit 1; \
	fi
	@echo "✅ Encryption key is set"
	@echo ""
	@echo "Building Docker images..."
	GIT_VERSION=$(VERSION) GIT_COMMIT=$(COMMIT) BUILD_TIME=$(BUILD_TIME) \
		ROUTER_A_PEERS=$(ROUTER_A_PEERS) ROUTER_B_PEERS=$(ROUTER_B_PEERS) \
		$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3-external.yml build
	@echo ""
	@echo "Starting services..."
	@GIT_VERSION=$(VERSION) GIT_COMMIT=$(COMMIT) BUILD_TIME=$(BUILD_TIME) \
		ROUTER_A_PEERS=$(ROUTER_A_PEERS) ROUTER_B_PEERS=$(ROUTER_B_PEERS) \
		$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3-external.yml up -d
	@sleep 2
	@echo ""
	@echo "======================================"
	@echo "✅ Deployment Complete!"
	@echo "======================================"
	@echo ""
	@echo "Architecture:"
	@echo "  🔀 HAProxy:      localhost:9090 (gRPC) / localhost:8080 (HTTP)"
	@echo "  📊 HAProxy Stats: http://localhost:8404/stats"
	@echo "  🪣 External MinIO: ${MONOFS_S3_ENDPOINT:-http://localhost:19000}"
	@echo "  📡 Router 1-2:   Internal (load balanced)"
	@echo "  💾 Backend 1-5:  Internal cluster nodes"
	@echo "  🔍 Search:       Internal (Zoekt-based code search)"
	@echo ""
	@echo "Access Points:"
	@echo "  🌐 Web UI:       http://localhost:8080"
	@echo "  📡 gRPC API:     localhost:9090"
	@echo "  🪣 MinIO Console: http://localhost:19001 (minioadmin/minioadmin)"
	@echo "  🖥️  SSH Client:   ssh -p 2222 monofs@localhost (auto-mounted)"
	@echo ""
	@echo "Admin CLI:"
	@echo "  ./bin/monofs-admin status --router=localhost:9090"
	@echo "  ./bin/monofs-admin ingest --url=<repo-url>"
	@echo ""
	@echo "Logs & Management:"
	@echo "  make docker-logs    - View container logs"
	@echo "  make deploy-stop    - Stop cluster"
	@echo "======================================"

deploy-local: build ## Deploy local dev cluster (router + 3 nodes with proper directories)
	@echo "======================================"
	@echo "MonoFS Dev Deployment"
	@echo "======================================"
	@mkdir -p /tmp/monofs-dev/node1/{db,git}
	@mkdir -p /tmp/monofs-dev/node2/{db,git}
	@mkdir -p /tmp/monofs-dev/node3/{db,git}
	@mkdir -p /tmp/monofs-dev/router-cache
	@echo ""
	@echo "Starting backend nodes..."
	@./$(BIN_DIR)/$(SERVER_BINARY) \
		--node-id=node1 \
		--addr=:9001 \
		--router=localhost:9090 \
		--db-path=/tmp/monofs-dev/node1/db \
		--git-cache=/tmp/monofs-dev/node1/git \
		--debug \
		> /tmp/monofs-dev/node1.log 2>&1 &
	@./$(BIN_DIR)/$(SERVER_BINARY) \
		--node-id=node2 \
		--addr=:9002 \
		--router=localhost:9090 \
		--db-path=/tmp/monofs-dev/node2/db \
		--git-cache=/tmp/monofs-dev/node2/git \
		--debug \
		> /tmp/monofs-dev/node2.log 2>&1 &
	@./$(BIN_DIR)/$(SERVER_BINARY) \
		--node-id=node3 \
		--addr=:9003 \
		--router=localhost:9090 \
		--db-path=/tmp/monofs-dev/node3/db \
		--git-cache=/tmp/monofs-dev/node3/git \
		--debug \
		> /tmp/monofs-dev/node3.log 2>&1 &
	@sleep 2
	@echo "Starting router..."
	@./$(BIN_DIR)/$(ROUTER_BINARY) \
		--port=9090 \
		--http-port=8080 \
		$(if $(PEER_ROUTERS),--peer-routers=$(PEER_ROUTERS),) \
		--nodes=node1=localhost:9001,node2=localhost:9002,node3=localhost:9003 \
		--debug \
		> /tmp/monofs-dev/router.log 2>&1 &
	@sleep 2
	@echo ""
	@echo "======================================"
	@echo "✅ Deployment Complete!"
	@echo "======================================"
	@echo ""
	@echo "Services:"
	@echo "  📡 Router gRPC:  localhost:9090"
	@echo "  🌐 Router UI:    http://localhost:8080"
	@echo "  💾 Backend 1:    localhost:9001"
	@echo "  💾 Backend 2:    localhost:9002"
	@echo "  💾 Backend 3:    localhost:9003"
	@echo ""
	@echo "Data directories:"
	@echo "  📁 /tmp/monofs-dev/node1/"
	@echo "  📁 /tmp/monofs-dev/node2/"
	@echo "  📁 /tmp/monofs-dev/node3/"
	@echo ""
	@echo "Web UI Pages:"
	@echo "  📊 Dashboard:     http://localhost:8080/"
	@echo "  🌐 Cluster:       http://localhost:8080/cluster"
	@echo "  🛡️  Replication:  http://localhost:8080/replication"
	@echo "  📦 Repositories:  http://localhost:8080/repositories"
	@echo "  ⬆️  Ingest:        http://localhost:8080/ingest"
	@echo ""
	@echo "Admin CLI:"
	@echo "  ./bin/monofs-admin status --router=localhost:9090"
	@echo "  ./bin/monofs-admin failover --router=localhost:9090"
	@echo "  ./bin/monofs-admin ingest --url=<repo-url>"
	@echo ""
	@echo "View logs: tail -f /tmp/monofs-dev/*.log"
	@echo "Stop cluster: make deploy-local-stop"
	@echo "======================================"

deploy-stop: ## Stop dev deployment
	@echo "Stopping MonoFS deployment..."
	@$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3.yml down --remove-orphans 2>/dev/null || \
		$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3-external.yml down --remove-orphans 2>/dev/null || \
		$(DOCKER_COMPOSE) down --remove-orphans || true
	@if pgrep -x monofs-server > /dev/null; then echo "  - Stopping monofs-server"; pkill -9 -x monofs-server; fi
	@if pgrep -x monofs-router > /dev/null; then echo "  - Stopping monofs-router"; pkill -9 -x monofs-router; fi
	@if pgrep -x monofs-client > /dev/null; then echo "  - Stopping monofs-client"; pkill -9 -x monofs-client; fi
	@echo "✅ Stopped all services"

deploy-clean: ## Stop deployment and remove all data
	@echo "Cleaning deployment..."
	@$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3.yml down -v --remove-orphans 2>/dev/null || \
		$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3-external.yml down -v --remove-orphans 2>/dev/null || \
		$(DOCKER_COMPOSE) down -v --remove-orphans || true
	@docker ps -a --filter 'name=monofs-' -q | xargs -r docker rm -f 2>/dev/null || true
	@rm -rf /tmp/monofs-dev || true
	@echo "✅ Cleaned deployment data"

deploy-s3-clean: ## Stop S3 deployment and remove all data (including MinIO)
	@echo "======================================"
	@echo "MonoFS S3 Deployment Cleanup"
	@echo "======================================"
	@echo ""
	@echo "Stopping services and removing all data..."
	@$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3.yml down -v --remove-orphans 2>/dev/null || \
		$(DOCKER_COMPOSE) -f docker-compose.yml -f docker-compose.s3-external.yml down -v --remove-orphans 2>/dev/null || true
	@docker ps -a --filter 'name=monofs-' -q | xargs -r docker rm -f 2>/dev/null || true
	@echo ""
	@echo "✅ S3 deployment cleaned"
	@echo ""
	@echo "Removed:"
	@echo "  - All MonoFS containers (router, nodes, fetchers, search)"
	@echo "  - All volumes (node data, fetcher cache)"
	@echo ""
	@echo "Note: External MinIO data is preserved (not managed by this deployment)"
	@echo ""
	@echo "Kept:"
	@echo "  - .env file (with your encryption key)"
	@echo ""
	@echo "To redeploy: make deploy-s3-external"
	@echo "======================================"

deploy-restart: deploy-stop deploy ## Restart deployment

deploy-local-stop: ## Stop local dev deployment (keeps router running)
	@echo "Stopping local backend nodes and FUSE client..."
	@pkill -f monofs-server || true
	@pkill -f monofs-client || true
	@echo "✅ Stopped backend nodes and client (router still running)"

deploy-local-clean: deploy-local-stop ## Clean local dev deployment data
	@echo "Cleaning local dev deployment data..."
	@rm -rf /tmp/monofs-dev
	@echo "✅ Cleaned local deployment data"

deploy-local-restart: deploy-local-stop deploy-local ## Restart local dev deployment

deploy-client: ## Deploy FUSE client in Docker container with auto-mount at /mnt/monofs
	@echo "======================================"
	@echo "MonoFS Docker Client Deployment"
	@echo "======================================"
	@echo ""
	@echo "Building client image..."
	@$(DOCKER_COMPOSE) build client
	@echo ""
	@echo "Starting client container (without backend dependencies)..."
	@$(DOCKER_COMPOSE) up -d --no-deps client
	@sleep 3
	@echo ""
	@echo "======================================"
	@echo "✅ Client Deployment Complete!"
	@echo "======================================"
	@echo ""
	@echo "📦 Container Status:"
	@$(DOCKER_COMPOSE) ps client
	@echo ""
	@echo "🔗 Connect to client:"
	@echo "  ssh monofs@localhost -p 2222"
	@echo "  Password: monofs"
	@echo ""
	@echo "📂 Inside container:"
	@echo "  Mount point: /mnt"
	@echo "  View files:  ls -la /mnt/monofs"
	@echo "  View logs:   tail -f /var/log/monofs-client.log"
	@echo ""
	@echo "🔍 From host:"
	@echo "  View logs:   docker compose logs -f client"
	@echo "  Stop client: docker compose stop client"
	@echo "======================================"

deploy-client-local: build-client ## Deploy FUSE client locally (requires MOUNT_POINT)
	@if [ -z "$(MOUNT_POINT)" ]; then \
		echo "Usage: make deploy-client-local MOUNT_POINT=/path/to/mount"; \
		exit 1; \
	fi
	@echo "======================================"
	@echo "MonoFS Client Local Deployment"
	@echo "======================================"
	@mkdir -p $(MOUNT_POINT)
	@mkdir -p /tmp/monofs-dev/client-cache
	@echo ""
	@echo "Starting FUSE client..."
	@./$(BIN_DIR)/$(CLIENT_BINARY) \
		--router=localhost:9090 \
		--mount=$(MOUNT_POINT) \
		--cache=/tmp/monofs-dev/client-cache \
		--debug \
		> /tmp/monofs-dev/client.log 2>&1 &
	@sleep 1
	@echo ""
	@echo "======================================"
	@echo "✅ Client Deployment Complete!"
	@echo "======================================"
	@echo ""
	@echo "Mount point: $(MOUNT_POINT)"
	@echo "Cache dir:   /tmp/monofs-dev/client-cache"
	@echo "Log file:    /tmp/monofs-dev/client.log"
	@echo ""
	@echo "Commands:"
	@echo "  ls $(MOUNT_POINT)        - List files"
	@echo "  tail -f /tmp/monofs-dev/client.log - View logs"
	@echo "  make unmount MOUNT_POINT=$(MOUNT_POINT) - Unmount"
	@echo "======================================"

mount: build-client ## Mount FUSE client (requires MOUNT_POINT, supports WRITABLE=1)
	@if [ -z "$(MOUNT_POINT)" ]; then \
		echo "Usage: make mount MOUNT_POINT=/mnt/monofs [WRITABLE=1]"; \
		exit 1; \
	fi
	@mkdir -p $(MOUNT_POINT)
	@mkdir -p /tmp/monofs-dev/overlay
	@echo "Mounting MonoFS at $(MOUNT_POINT)..."
	@./$(BIN_DIR)/$(CLIENT_BINARY) \
		--router=localhost:9090 \
		--mount=$(MOUNT_POINT) \
		$(if $(WRITABLE),--writable --overlay=/tmp/monofs-dev/overlay,) \
		--debug

mount-writable: build-client ## Mount FUSE client with write support (requires MOUNT_POINT)
	@if [ -z "$(MOUNT_POINT)" ]; then \
		echo "Usage: make mount-writable MOUNT_POINT=/mnt/monofs"; \
		exit 1; \
	fi
	@mkdir -p $(MOUNT_POINT)
	@mkdir -p /tmp/monofs-dev/overlay
	@echo "Mounting MonoFS (writable) at $(MOUNT_POINT)..."
	@./$(BIN_DIR)/$(CLIENT_BINARY) \
		--router=localhost:9090 \
		--mount=$(MOUNT_POINT) \
		--writable \
		--overlay=/tmp/monofs-dev/overlay \
		--debug

unmount: ## Unmount FUSE client (requires MOUNT_POINT)
	@if [ -z "$(MOUNT_POINT)" ]; then \
		echo "Usage: make unmount MOUNT_POINT=/mnt/monofs"; \
		exit 1; \
	fi
	@fusermount -u $(MOUNT_POINT) || fusermount3 -u $(MOUNT_POINT) || true
	@echo "✅ Unmounted $(MOUNT_POINT)"

mount-kmod: ## Mount the kernel-module lower filesystem (requires MOUNT_POINT, optional GATEWAY)
	@if [ -z "$(MOUNT_POINT)" ]; then \
		echo "Usage: make mount-kmod MOUNT_POINT=/mnt/monofs-kmod [GATEWAY=host:port] [SEED_PATHS=a/b,c/d] [CLUSTER_VERSION=n] [DEBUG=1]"; \
		exit 1; \
	fi
	@bash ./scripts/mount-monofs-kmod.sh \
		--mount="$(MOUNT_POINT)" \
		$(if $(GATEWAY),--gateway="$(GATEWAY)",) \
		$(if $(SEED_PATHS),--seed-paths="$(SEED_PATHS)",) \
		$(if $(CLUSTER_VERSION),--cluster-version="$(CLUSTER_VERSION)",) \
		$(if $(DEBUG),--debug,)

umount-kmod: ## Unmount the kernel-module lower filesystem (requires MOUNT_POINT)
	@if [ -z "$(MOUNT_POINT)" ]; then \
		echo "Usage: make umount-kmod MOUNT_POINT=/mnt/monofs-kmod"; \
		exit 1; \
	fi
	@umount "$(MOUNT_POINT)"
	@echo "✅ Unmounted $(MOUNT_POINT)"

##@ Testing

test: test-unit ## Run all tests (alias for test-unit)

test-unit: ## Run unit tests only
	$(GOTEST) -v -count=1 ./internal/...

test-deadlock: ## Run deadlock detection tests
	@echo "Running deadlock detection tests..."
	$(GOTEST) -v -timeout=5m ./internal/server -run "Deadlock|Contention"

test-stress: build ## Run stress and edge case tests (requires FUSE)
	@echo "Running stress tests (may require sudo)..."
	@echo "Cleaning up test mounts..."
	@fusermount -u /tmp/monofs-stress-test 2>/dev/null || fusermount3 -u /tmp/monofs-stress-test 2>/dev/null || umount /tmp/monofs-stress-test 2>/dev/null || true
	@rm -rf /tmp/monofs-stress-test 2>/dev/null || true
	$(GOTEST) -v -count=1 -timeout=15m ./test/ -run "Concurrent|Empty|Backend|Graceful|Rapid|Timeout"

test-e2e: build ## Run E2E integration tests (requires FUSE)
	@echo "Running E2E tests (may require sudo)..."
	@echo "Cleaning up any existing test mounts..."
	@fusermount -u /tmp/monofs-e2e-test 2>/dev/null || fusermount3 -u /tmp/monofs-e2e-test 2>/dev/null || umount /tmp/monofs-e2e-test 2>/dev/null || true
	@rm -rf /tmp/monofs-e2e-test 2>/dev/null || true
	$(GOTEST) -v -count=1 -timeout=10m ./test/ -run "E2E"

test-e2e-sudo: build ## Run E2E tests with sudo (builds binaries and test as user, runs with sudo)
	@echo "Building test binary as regular user..."
	$(GOTEST) -c -o bin/e2e.test ./test/...
	@echo "Cleaning up any existing test mounts..."
	@sudo fusermount -u /tmp/monofs-e2e-test 2>/dev/null || sudo fusermount3 -u /tmp/monofs-e2e-test 2>/dev/null || sudo umount /tmp/monofs-e2e-test 2>/dev/null || true
	@sudo rm -rf /tmp/monofs-e2e-test 2>/dev/null || true
	@echo "Running E2E tests with sudo..."
	sudo -E ./bin/e2e.test -test.v -test.count=1 -test.timeout=10m

test-smoke: build ## Run quick smoke test
	$(GOTEST) -v -run=TestE2ESmoke ./test/...

test-all: test-unit test-deadlock test-e2e ## Run all tests (unit + deadlock + E2E)

test-race: ## Run tests with race detector
	$(GOTEST) -race -v ./...

test-coverage: ## Run tests with coverage report
	$(GOTEST) -coverprofile=coverage.out -covermode=atomic ./internal/...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

##@ Code Quality

vet: ## Run go vet
	$(GOVET) ./...

fmt: ## Format code
	$(GOFMT) -s -w .

fmt-check: ## Check if code is formatted
	@if [ -n "$$($(GOFMT) -l .)" ]; then \
		echo "Code is not formatted. Run 'make fmt'"; \
		$(GOFMT) -l .; \
		exit 1; \
	fi

##@ Dependencies

tidy: ## Tidy and verify go modules
	$(GOMOD) tidy
	$(GOMOD) verify

install-tools: ## Install required development tools
	$(GOCMD) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	$(GOCMD) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@echo "Installed protoc-gen-go and protoc-gen-go-grpc"
	@echo "Note: You also need to install protoc (protobuf compiler) separately"

##@ Docker

docker-build: ## Build Docker images
	$(DOCKER_COMPOSE) build

docker-up: ## Start the 3-node cluster with Docker
	$(DOCKER_COMPOSE) up -d
	@echo "Cluster started. Router at localhost:9090"
	@echo "Use 'make docker-logs' to view logs"

docker-down: ## Stop the Docker cluster
	$(DOCKER_COMPOSE) down

docker-logs: ## View cluster logs
	$(DOCKER_COMPOSE) logs -f

docker-clean: ## Remove Docker images and volumes
	$(DOCKER_COMPOSE) down -v --rmi local
	@echo "Cleaned Docker resources"

docker-restart: docker-down docker-up ## Restart the cluster
