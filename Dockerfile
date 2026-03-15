# Build stage
FROM golang:1.25-alpine AS builder

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
RUN mkdir -p /data/cache/git /data/cache/blob /etc/monofs && chown -R monofs:monofs /data /etc/monofs

COPY --from=builder /bin/monofs-fetcher /usr/local/bin/monofs-fetcher
COPY config/fetcher.json /etc/monofs/fetcher.json
COPY docker/fetcher-entrypoint.sh /usr/local/bin/fetcher-entrypoint.sh
RUN chmod +x /usr/local/bin/fetcher-entrypoint.sh

USER monofs

EXPOSE 9200

ENTRYPOINT ["/usr/local/bin/fetcher-entrypoint.sh"]
CMD ["monofs-fetcher", "--port=9200", "--cache-dir=/data/cache"]

# Client image (interactive with SSH)
FROM alpine:3.19 AS client

RUN apk add --no-cache \
    ca-certificates \
    fuse3 \
    openssh-server \
    bash \
    sudo \
    shadow \
    strace \
    jq \
    nodejs \
    npm \
    python3 \
    py3-pip \
    gcc \
    musl-dev \
    curl \
    gcompat \
    libstdc++ \
    libgcc

# Install Rust via rustup
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | \
    sh -s -- -y --default-toolchain stable --no-modify-path
ENV PATH=/root/.cargo/bin:$PATH

# Install Bazel via Bazelisk
RUN curl -fsSL https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-amd64 \
    -o /usr/local/bin/bazel && \
    chmod +x /usr/local/bin/bazel

# Copy Go 1.25 toolchain from the builder stage
COPY --from=builder /usr/local/go /usr/local/go
ENV GOROOT=/usr/local/go
ENV PATH=$GOROOT/bin:$PATH

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

# Create mount point, overlay directory, and log files (monofs-owned)
RUN mkdir -p /mnt/monofs && \
    chmod 777 /mnt/monofs && \
    mkdir -p /var/cache/monofs && \
    chmod 777 /var/cache/monofs && \
    mkdir -p /home/monofs/.monofs/overlay && \
    chmod 777 /home/monofs/.monofs/overlay && \
    chown -R monofs:monofs /mnt/monofs && \
    chown -R monofs:monofs /var/cache/monofs && \
    chown -R monofs:monofs /home/monofs/.monofs && \
    touch /var/log/monofs-client.log /var/log/monofs-client.json && \
    chown monofs:monofs /var/log/monofs-client.log /var/log/monofs-client.json

# Enable FUSE with user_allow_other
RUN echo "user_allow_other" >> /etc/fuse.conf

# Set overlay directory for monofs-session (also in user profile for SSH sessions)
ENV GITFS_OVERLAY_DIR=/home/monofs/.monofs/overlay
RUN echo 'export GITFS_OVERLAY_DIR=/home/monofs/.monofs/overlay' >> /etc/profile.d/monofs.sh && \
    echo 'export GOROOT=/usr/local/go' >> /etc/profile.d/monofs.sh && \
    echo 'export PATH=$GOROOT/bin:/root/.cargo/bin:$PATH' >> /etc/profile.d/monofs.sh && \
    echo 'export GITFS_OVERLAY_DIR=/home/monofs/.monofs/overlay' >> /home/monofs/.bashrc && \
    echo 'export GOROOT=/usr/local/go' >> /home/monofs/.bashrc && \
    echo 'export PATH=$GOROOT/bin:/root/.cargo/bin:$PATH' >> /home/monofs/.bashrc && \
    echo '' >> /home/monofs/.bashrc && \
    echo '# monofs-setup: export build-cache env vars from the mounted filesystem.' >> /home/monofs/.bashrc && \
    echo '# Called automatically at login; run manually after the mount appears.' >> /home/monofs/.bashrc && \
    echo 'monofs-setup() {' >> /home/monofs/.bashrc && \
    echo '  local _out' >> /home/monofs/.bashrc && \
    echo '  _out=$(monofs-session setup --mount /mnt/monofs 2>&1) || {' >> /home/monofs/.bashrc && \
    echo '    echo "[monofs] setup skipped: $_out" >&2; return 1; }' >> /home/monofs/.bashrc && \
    echo '  eval "$_out"' >> /home/monofs/.bashrc && \
    echo '}' >> /home/monofs/.bashrc && \
    echo 'monofs-setup 2>/dev/null || echo "[monofs] Run monofs-setup once filesystem mounts to configure build caches."' >> /home/monofs/.bashrc && \
    chown monofs:monofs /home/monofs/.bashrc && \
    echo '# .bash_profile: source .bashrc for login shells (SSH, su -).' >> /home/monofs/.bash_profile && \
    echo '# Bash login shells read .bash_profile but NOT .bashrc, so we' >> /home/monofs/.bash_profile && \
    echo '# must explicitly source it here.' >> /home/monofs/.bash_profile && \
    echo 'if [ -f "$HOME/.bashrc" ]; then' >> /home/monofs/.bash_profile && \
    echo '  . "$HOME/.bashrc"' >> /home/monofs/.bash_profile && \
    echo 'fi' >> /home/monofs/.bash_profile && \
    chown monofs:monofs /home/monofs/.bash_profile

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

# Mount filesystem as monofs user.
# Logging strategy:
#   --debug            → MonoFS layer DEBUG+ goes into --log-file (JSON)
#   --log-file         → /var/log/monofs-client.json (structured, DEBUG+)
#   stdout (INFO text) → /var/log/monofs-client.log
#   go-fuse C layer    → discarded (add --fuse-debug + --log-file to see it in .json.fuse)
su - monofs -c "/usr/local/bin/monofs-client \
  --router=${ROUTER_ADDR:-router:9090} \
  --mount=/mnt/monofs \
  --cache=/var/cache/monofs \
  --writable \
  --overlay=/home/monofs/.monofs/overlay \
  --debug \
  --log-file=/var/log/monofs-client.json" \
  > /var/log/monofs-client.log 2>&1 &

# Wait for mount to complete
for i in {1..10}; do
  if mountpoint -q /mnt/monofs 2>/dev/null; then
    echo "[$(date)] ✅ MonoFS mounted at /mnt/monofs"
    break
  fi
  echo "[$(date)] Waiting for mount... ($i/10)"
  sleep 1
done

if ! mountpoint -q /mnt/monofs 2>/dev/null; then
  echo "[$(date)] ⚠️  MonoFS not yet mounted (backends may be unavailable)"
  echo "[$(date)] FS_ERROR.txt will appear at mount root when backend is unreachable"
fi

# Make sure /mnt/monofs is accessible to all users
chmod 755 /mnt/monofs 2>/dev/null || true

echo "[$(date)] MonoFS Client Ready (Write Support Enabled)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  SSH Access: ssh monofs@localhost -p 2222"
echo "  Password:   monofs"
echo "  Mount:      /mnt/monofs (writable)"
echo "  Overlay:    /home/monofs/.monofs/overlay"
echo ""
echo "  Logs:"
echo "    INFO  text : tail -f /var/log/monofs-client.log"
echo "    DEBUG JSON : tail -f /var/log/monofs-client.json | jq ."
echo "    FUSE  C    : (add --fuse-debug to enable → /var/log/monofs-client.json.fuse)"
echo ""
echo "  Write Examples:"
echo "    mkdir /mnt/monofs/mydir            - Create user directory"
echo "    echo test > /mnt/monofs/mydir/f.txt - Create file"
echo "    ln -s /target /mnt/monofs/mydir/lnk - Create symlink"
echo ""
echo "  Session CLI: monofs-session start"
echo "               monofs-session status"
echo "               monofs-session commit"
echo ""
echo "  Admin CLI:  monofs-admin ingest --url=<repo-url>"
echo "              monofs-admin status"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# Keep container running - follow the human-readable INFO log.
# For debug-level detail: docker exec <container> tail -f /var/log/monofs-client.json | jq .
tail -f /var/log/monofs-client.log
STARTSCRIPT

RUN chmod +x /start.sh

USER root
CMD ["/start.sh"]
