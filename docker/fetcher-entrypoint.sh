#!/bin/sh
# Entrypoint script for monofs-fetcher
# Validates required environment variables before starting

set -e

# Check for required encryption key
if [ -z "$MONOFS_ENCRYPTION_KEY" ]; then
    echo "ERROR: MONOFS_ENCRYPTION_KEY environment variable is required"
    echo ""
    echo "Please set MONOFS_ENCRYPTION_KEY to a 32-byte hex-encoded encryption key."
    echo "Generate one with: openssl rand -hex 32"
    echo ""
    echo "Example:"
    echo "  export MONOFS_ENCRYPTION_KEY=\$(openssl rand -hex 32)"
    echo "  docker run -e MONOFS_ENCRYPTION_KEY=\$MONOFS_ENCRYPTION_KEY ..."
    exit 1
fi

# Validate key format (64 hex characters)
key_len=$(echo -n "$MONOFS_ENCRYPTION_KEY" | wc -c)
if [ "$key_len" -ne 64 ]; then
    echo "ERROR: MONOFS_ENCRYPTION_KEY must be 64 hex characters (32 bytes)"
    echo "Current length: $key_len characters"
    echo ""
    echo "Generate a valid key with: openssl rand -hex 32"
    exit 1
fi

# Log configuration
if [ -n "$MONOFS_S3_BUCKET" ]; then
    echo "Starting MonoFS Fetcher with S3 backend"
    echo "  S3 Bucket: $MONOFS_S3_BUCKET"
    echo "  S3 Region: ${MONOFS_S3_REGION:-us-east-1}"
    echo "  S3 Endpoint: ${MONOFS_S3_ENDPOINT:-(default)}"
    echo "  Cache Directory: ${MONOFS_FETCHER_CACHE_DIR:-/data/cache}"
else
    echo "Starting MonoFS Fetcher with local storage"
    echo "  Cache Directory: ${MONOFS_FETCHER_CACHE_DIR:-/data/cache}"
fi

# Execute the main command
exec /usr/local/bin/monofs-fetcher "$@"
