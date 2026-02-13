# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Build arguments for version information
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build binaries with version information
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /bin/monofs-server ./cmd/monofs-server

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /bin/monofs-router ./cmd/monofs-router

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /bin/monofs-client ./cmd/monofs-client

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /bin/monofs-admin ./cmd/monofs-admin

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /bin/monofs-session ./cmd/monofs-session

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /bin/monofs-search ./cmd/monofs-search

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /bin/monofs-fetcher ./cmd/monofs-fetcher

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /bin/monofs-loadtest ./cmd/monofs-loadtest

# Server image
FROM alpine:3.19 AS server

RUN apk add --no-cache ca-certificates

COPY --from=builder /bin/monofs-server /usr/local/bin/monofs-server

EXPOSE 9000

ENTRYPOINT ["monofs-server"]
CMD ["--addr=:9000"]

# Router image
FROM alpine:3.19 AS router

RUN apk add --no-cache ca-certificates

COPY --from=builder /bin/monofs-router /usr/local/bin/monofs-router

EXPOSE 9090

ENTRYPOINT ["monofs-router"]
CMD ["--port=9090"]

# Search image
FROM alpine:3.19 AS search

# Install git for cloning repos and go for downloading Go modules
RUN apk add --no-cache ca-certificates git go

# Create non-root user
RUN addgroup -S monofs && adduser -S monofs -G monofs

# Create data directories
RUN mkdir -p /data/indexes /data/cache && chown -R monofs:monofs /data

COPY --from=builder /bin/monofs-search /usr/local/bin/monofs-search

USER monofs

EXPOSE 9100

ENTRYPOINT ["monofs-search"]
CMD ["--port=9100", "--index-dir=/data/indexes", "--cache-dir=/data/cache"]

# Fetcher image - External blob fetcher (runs in DMZ)
FROM alpine:3.19 AS fetcher

# Install git for cloning repos
RUN apk add --no-cache ca-certificates git

# Create non-root user
RUN addgroup -S monofs && adduser -S monofs -G monofs

# Create cache directories
RUN mkdir -p /data/cache/git /data/cache/gomod && chown -R monofs:monofs /data

COPY --from=builder /bin/monofs-fetcher /usr/local/bin/monofs-fetcher

USER monofs

EXPOSE 9200

ENTRYPOINT ["monofs-fetcher"]
CMD ["--port=9200", "--cache-dir=/data/cache"]

# Client image (interactive with SSH)
FROM alpine:3.19 AS client

RUN apk add --no-cache \
    ca-certificates \
    fuse3 \
    openssh-server \
    bash \
    sudo \
    shadow \
    curl \
    jq \
    coreutils \
    grep \
    findutils \
    diffutils \
    procps \
    tree \
    git \
    make \
    protobuf \
    protobuf-dev \
    g++ \
    musl-dev \
    wget

# Install Go 1.24.0 from official binaries
RUN wget -O go.tar.gz https://go.dev/dl/go1.24.0.linux-amd64.tar.gz && \
    tar -C /usr/local -xzf go.tar.gz && \
    rm go.tar.gz

ENV PATH=/usr/local/go/bin:$PATH
ENV GOPATH=/root/go
ENV PATH=$GOPATH/bin:$PATH

# Add Go to PATH for all users (including SSH sessions)
RUN echo 'export PATH=/usr/local/go/bin:$PATH' >> /etc/profile.d/go.sh && \
    echo 'export GOPATH=/root/go' >> /etc/profile.d/go.sh && \
    echo 'export PATH=$GOPATH/bin:$PATH' >> /etc/profile.d/go.sh && \
    chmod +x /etc/profile.d/go.sh

# Create monofs user
RUN adduser -D -s /bin/bash monofs && \
    echo "monofs:monofs" | chpasswd && \
    echo "monofs ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# Configure SSH
RUN ssh-keygen -A && \
    sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config && \
    sed -i 's/#PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config

COPY --from=builder /bin/monofs-client /usr/local/bin/monofs-client
COPY --from=builder /bin/monofs-admin /usr/local/bin/monofs-admin
COPY --from=builder /bin/monofs-session /usr/local/bin/monofs-session
COPY --from=builder /bin/monofs-loadtest /usr/local/bin/monofs-loadtest

# Copy source code for Go cache investigation
COPY --from=builder /app /home/monofs/monofs-src
RUN chown -R monofs:monofs /home/monofs/monofs-src

# Create script to examine Go cache structure
COPY <<-"EXAMINEGOMOD" /usr/local/bin/examine-go-cache
#!/bin/bash
set -e

echo "========================================="
echo "Go Module Cache Structure Examination"
echo "========================================="
echo ""

cd /home/monofs/monofs-src

echo "=== Go Environment ==="
go env | grep -E 'GOMODCACHE|GOCACHE|GOPATH'
echo ""

GOMODCACHE=$(go env GOMODCACHE)
GOCACHE=$(go env GOCACHE)

echo "=== Cleaning caches ==="
go clean -modcache -cache
echo "Caches cleaned"
echo ""

echo "=== Downloading dependencies with 'go mod download' ==="
go mod download
echo ""

echo "=== GOMODCACHE Directory Structure ==="
echo "Location: $GOMODCACHE"
echo ""

echo "--- Top-level structure ---"
ls -lh "$GOMODCACHE/" 2>/dev/null || echo "Empty or doesn't exist"
echo ""

echo "--- Sample module (github.com/hanwen/go-fuse/v2@v2.7.2) ---"
SAMPLE_MODULE="$GOMODCACHE/github.com/hanwen/go-fuse/v2@v2.7.2"
if [ -d "$SAMPLE_MODULE" ]; then
    echo "Module directory exists:"
    ls -la "$SAMPLE_MODULE/" | head -15
    echo ""
    echo "Sample files:"
    find "$SAMPLE_MODULE" -type f | head -10
else
    echo "Module not found in extracted form"
fi
echo ""

echo "=== GOMODCACHE/cache/download Directory ==="
CACHE_DOWNLOAD="$GOMODCACHE/cache/download"
if [ -d "$CACHE_DOWNLOAD" ]; then
    echo "Cache download directory exists at: $CACHE_DOWNLOAD"
    echo ""
    
    echo "--- Files for go-fuse module ---"
    find "$CACHE_DOWNLOAD/github.com/hanwen/go-fuse" -type f 2>/dev/null | sort || echo "Not found"
    echo ""
    
    echo "--- File types in cache ---"
    find "$CACHE_DOWNLOAD" -type f -name "*.mod" | head -5
    echo ""
    find "$CACHE_DOWNLOAD" -type f -name "*.zip" | head -5
    echo ""
    find "$CACHE_DOWNLOAD" -type f -name "*.ziphash" | head -5
    echo ""
    find "$CACHE_DOWNLOAD" -type f -name "*.info" | head -5
    echo ""
    
    echo "--- Examining one .info file ---"
    INFO_FILE=$(find "$CACHE_DOWNLOAD" -type f -name "*.info" | head -1)
    if [ -n "$INFO_FILE" ]; then
        echo "File: $INFO_FILE"
        cat "$INFO_FILE"
        echo ""
    fi
    
    echo "--- Examining one .mod file ---"
    MOD_FILE=$(find "$CACHE_DOWNLOAD" -type f -name "*.mod" | head -1)
    if [ -n "$MOD_FILE" ]; then
        echo "File: $MOD_FILE"
        head -20 "$MOD_FILE"
        echo ""
    fi
    
    echo "--- Examining one .ziphash file ---"
    ZIPHASH_FILE=$(find "$CACHE_DOWNLOAD" -type f -name "*.ziphash" | head -1)
    if [ -n "$ZIPHASH_FILE" ]; then
        echo "File: $ZIPHASH_FILE"
        cat "$ZIPHASH_FILE"
        echo ""
    fi
fi
echo ""

echo "=== Building monofs-client ==="
go build -v -o /tmp/monofs-client-test ./cmd/monofs-client 2>&1 | tail -30
echo ""
echo "Build completed"
echo ""

echo "=== Testing Makefile build ==="
echo "Running 'make' to test build system..."
echo "Note: Proto generation may fail without protoc-gen-go plugins installed"
make 2>&1 | tail -30 || echo "Make build failed or completed with warnings"
echo ""

echo "=== Verifying protoc installation ==="
protoc --version || echo "protoc not found"
echo "Note: Install protoc-gen-go plugins with:"
echo "  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest"
echo "  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest"
echo ""

echo "=== GOCACHE Directory (Build Cache) ==="
echo "Location: $GOCACHE"
if [ -d "$GOCACHE" ]; then
    echo "Build cache exists"
    echo "Top-level contents:"
    ls -lh "$GOCACHE/" | head -20
    echo ""
    echo "Number of cached items:"
    find "$GOCACHE" -type f | wc -l
else
    echo "Build cache doesn't exist"
fi
echo ""

echo "========================================="
echo "Summary of Files Created by Go"
echo "========================================="
echo ""

echo "1. Module Sources (GOMODCACHE/module@version/):"
echo "   - Extracted source files from .zip"
find "$GOMODCACHE" -maxdepth 3 -type d -name "*@v*" 2>/dev/null | head -5
echo ""

echo "2. Download Cache (GOMODCACHE/cache/download/):"
echo "   - *.mod files (module manifests)"
echo "   - *.zip files (source archives)"
echo "   - *.ziphash files (checksums)"
echo "   - *.info files (version metadata)"
find "$CACHE_DOWNLOAD" -type f | head -20 2>/dev/null
echo ""

echo "3. Build Cache (GOCACHE/):"
echo "   - Compiled packages and metadata"
echo "   Directory: $GOCACHE"
echo ""

echo "========================================="
echo "Complete cache tree (truncated):"
tree -L 4 "$GOMODCACHE" 2>/dev/null | head -100 || find "$GOMODCACHE" -type f | head -50
EXAMINEGOMOD

RUN chmod +x /usr/local/bin/examine-go-cache

# Create mount point and overlay directory
RUN mkdir -p /mnt && \
    chmod 777 /mnt && \
    mkdir -p /var/cache/monofs && \
    chmod 777 /var/cache/monofs && \
    mkdir -p /var/lib/monofs/overlay && \
    chmod 777 /var/lib/monofs/overlay && \
    chown -R monofs:monofs /mnt && \
    chown -R monofs:monofs /var/cache/monofs && \
    chown -R monofs:monofs /var/lib/monofs

# Enable FUSE with user_allow_other
RUN echo "user_allow_other" >> /etc/fuse.conf

# Set overlay directory for monofs-session (also in user profile for SSH sessions)
ENV GITFS_OVERLAY_DIR=/var/lib/monofs/overlay
RUN echo 'export GITFS_OVERLAY_DIR=/var/lib/monofs/overlay' >> /etc/profile.d/monofs.sh && \
    echo 'export PATH=/usr/local/go/bin:$GOPATH/bin:$PATH' >> /etc/profile.d/monofs.sh && \
    echo 'export GOPATH=/root/go' >> /etc/profile.d/monofs.sh && \
    chmod +x /etc/profile.d/monofs.sh && \
    echo 'export GITFS_OVERLAY_DIR=/var/lib/monofs/overlay' >> /home/monofs/.bashrc && \
    echo 'export PATH=/usr/local/go/bin:$GOPATH/bin:$PATH' >> /home/monofs/.bashrc && \
    echo 'export GOPATH=$HOME/go' >> /home/monofs/.bashrc && \
    chown monofs:monofs /home/monofs/.bashrc

EXPOSE 22

# Create startup script that mounts filesystem automatically
COPY <<-"STARTSCRIPT" /start.sh
#!/bin/bash
set -e

# Start SSH server
/usr/sbin/sshd

# Wait a bit for router to be ready
sleep 2

echo "[$(date)] Starting GitFS client..."
echo "[$(date)] Connecting to router at: ${ROUTER_ADDR:-router:9090}"

# Mount filesystem as monofs user with allow_other option (using full path)
su - monofs -c "/usr/local/bin/monofs-client \
  --router=${ROUTER_ADDR:-router:9090} \
  --mount=/mnt \
  --cache=/var/cache/monofs \
  --writable \
  --overlay=/var/lib/monofs/overlay \
  --debug" \
  > /var/log/monofs-client.log 2>&1 &

# Wait for mount to complete
for i in {1..10}; do
  if mountpoint -q /mnt 2>/dev/null; then
    echo "[$(date)] ✅ GitFS mounted at /mnt"
    break
  fi
  echo "[$(date)] Waiting for mount... ($i/10)"
  sleep 1
done

if ! mountpoint -q /mnt 2>/dev/null; then
  echo "[$(date)] ⚠️  GitFS not yet mounted (backends may be unavailable)"
  echo "[$(date)] But filesystem is accessible - FS_ERROR.txt will show connection status"
fi

# Make sure /mnt is accessible to all users
chmod 755 /mnt 2>/dev/null || true

echo "[$(date)] GitFS Client Ready (Write Support Enabled)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  SSH Access: ssh monofs@localhost -p 2222"
echo "  Password:   monofs"
echo "  Mount:      /mnt (writable)"
echo "  Overlay:    /var/lib/monofs/overlay"
echo "  Logs:       tail -f /var/log/monofs-client.log"
echo ""
echo "  Write Examples:"
echo "    mkdir /mnt/mydir            - Create user directory"
echo "    echo test > /mnt/mydir/f.txt - Create file"
echo "    ln -s /target /mnt/mydir/lnk - Create symlink"
echo ""
echo "  Session CLI: monofs-session start"
echo "               monofs-session status"
echo "               monofs-session commit"
echo ""
echo "  Admin CLI:  monofs-admin ingest --url=<repo-url>"
echo "              monofs-admin status"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# Start load test after 1 minute if ENABLE_LOADTEST is set
if [ "${ENABLE_LOADTEST}" = "true" ]; then
  (
    echo "[$(date)] Load test scheduled to start in 60 seconds..."
    sleep 60
    echo "[$(date)] Starting load test..."
    su - monofs -c "/usr/local/bin/monofs-loadtest \
      --mount=/mnt \
      --duration=5m \
      --concurrency=10 \
      --file-size=4096 \
      --read-ratio=0.5 \
      --write-ratio=0.3 \
      --delete-ratio=0.1 \
      --mkdir-ratio=0.05 \
      --list-ratio=0.05 \
      --verbose" \
      > /var/log/monofs-loadtest.log 2>&1
    echo "[$(date)] Load test completed"
  ) &
fi

# Keep container running and show logs
tail -f /var/log/monofs-client.log
STARTSCRIPT

RUN chmod +x /start.sh

USER root
CMD ["/start.sh"]
