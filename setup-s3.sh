#!/bin/bash
# Setup script for MonoFS with MinIO S3 backend

echo "Setting up MonoFS with MinIO S3 backend..."

# Generate a 32-byte encryption key (64 hex chars) if not set
if [ -z "$MONOFS_ENCRYPTION_KEY" ]; then
    export MONOFS_ENCRYPTION_KEY=$(openssl rand -hex 32)
    echo "Generated encryption key: $MONOFS_ENCRYPTION_KEY"
    echo "IMPORTANT: Save this key - you'll need it for all fetcher instances"
else
    echo "Using existing MONOFS_ENCRYPTION_KEY"
fi

# MinIO S3 configuration
export MONOFS_S3_REGION="us-east-1"
export MONOFS_S3_BUCKET="monofs"
export MONOFS_S3_ENDPOINT="http://localhost:19000"
export MONOFS_S3_ACCESS_KEY_ID="minioadmin"
export MONOFS_S3_SECRET_ACCESS_KEY="minioadmin"
export MONOFS_S3_USE_PATH_STYLE="true"

# Fetcher settings
export MONOFS_FETCHER_PORT="9200"
export MONOFS_FETCHER_CACHE_DIR="/data/fetcher-cache"
export MONOFS_FETCHER_LOG_LEVEL="info"

echo ""
echo "Configuration:"
echo "  S3 Endpoint: $MONOFS_S3_ENDPOINT"
echo "  S3 Bucket: $MONOFS_S3_BUCKET"
echo "  S3 Region: $MONOFS_S3_REGION"
echo "  Fetcher Port: $MONOFS_FETCHER_PORT"
echo "  Cache Dir: $MONOFS_FETCHER_CACHE_DIR"
echo ""
echo "To run the fetcher with S3 backend:"
echo "  1. Source this script: source setup-s3.sh"
echo "  2. Run fetcher: ./bin/monofs-fetcher --config config/fetcher-s3.json"
echo ""
echo "Or run directly with flags:"
echo "  ./bin/monofs-fetcher \\"
echo "    --encryption-key \"\$MONOFS_ENCRYPTION_KEY\" \\"
echo "    --cache-dir /data/fetcher-cache \\"
echo "    --config config/fetcher-s3.json"
