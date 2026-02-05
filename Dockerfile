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
    shadow

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
    echo 'export GITFS_OVERLAY_DIR=/var/lib/monofs/overlay' >> /home/monofs/.bashrc && \
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
