#!/bin/bash
# MonoFS Key Generation and Management Script
#
# Usage:
#   ./scripts/generate-key.sh           # Generate a new key
#   ./scripts/generate-key.sh --check   # Check if key is set
#   ./scripts/generate-key.sh --save    # Generate and save to .env

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${PROJECT_DIR}/.env"

generate_key() {
    openssl rand -hex 32
}

check_key() {
    if [ -n "$MONOFS_ENCRYPTION_KEY" ]; then
        echo "✅ MONOFS_ENCRYPTION_KEY is set"
        key_len=$(echo -n "$MONOFS_ENCRYPTION_KEY" | wc -c)
        if [ "$key_len" -eq 64 ]; then
            echo "   Length: 64 characters (32 bytes) - Valid"
            return 0
        else
            echo "   Length: $key_len characters - Invalid (should be 64)"
            return 1
        fi
    elif [ -f "$ENV_FILE" ] && grep -q "^MONOFS_ENCRYPTION_KEY=" "$ENV_FILE"; then
        key=$(grep "^MONOFS_ENCRYPTION_KEY=" "$ENV_FILE" | cut -d'=' -f2)
        if [ -n "$key" ]; then
            echo "✅ MONOFS_ENCRYPTION_KEY found in .env file"
            key_len=$(echo -n "$key" | wc -c)
            if [ "$key_len" -eq 64 ]; then
                echo "   Length: 64 characters (32 bytes) - Valid"
                return 0
            else
                echo "   Length: $key_len characters - Invalid (should be 64)"
                return 1
            fi
        fi
    fi
    
    echo "❌ MONOFS_ENCRYPTION_KEY is not set"
    echo ""
    echo "Generate a key with:"
    echo "  ./scripts/generate-key.sh --save"
    return 1
}

save_key() {
    local key=$(generate_key)
    
    if [ -f "$ENV_FILE" ]; then
        # Update existing key or add new one
        if grep -q "^MONOFS_ENCRYPTION_KEY=" "$ENV_FILE"; then
            # macOS sed requires different syntax
            if [[ "$OSTYPE" == "darwin"* ]]; then
                sed -i '' "s/^MONOFS_ENCRYPTION_KEY=.*/MONOFS_ENCRYPTION_KEY=$key/" "$ENV_FILE"
            else
                sed -i "s/^MONOFS_ENCRYPTION_KEY=.*/MONOFS_ENCRYPTION_KEY=$key/" "$ENV_FILE"
            fi
        else
            echo "MONOFS_ENCRYPTION_KEY=$key" >> "$ENV_FILE"
        fi
    else
        # Create new .env file
        cat > "$ENV_FILE" << EOF
# MonoFS Docker Compose Environment Variables
#
# This file was auto-generated. You can regenerate the key with:
#   ./scripts/generate-key.sh --save

# Required: 32-byte hex-encoded encryption key for packager archives
MONOFS_ENCRYPTION_KEY=$key
EOF
    fi
    
    echo "✅ Generated and saved new encryption key to .env"
    echo ""
    echo "Key: $key"
    echo ""
    echo "To use this key in your current shell:"
    echo "  export MONOFS_ENCRYPTION_KEY=$key"
    echo ""
    echo "Or load from .env file:"
    echo "  set -a && source .env && set +a"
}

case "${1:-}" in
    --check)
        check_key
        ;;
    --save)
        save_key
        ;;
    --help|-h)
        echo "MonoFS Key Management Script"
        echo ""
        echo "Usage:"
        echo "  $0           Generate a new key and print to stdout"
        echo "  $0 --check   Check if key is properly configured"
        echo "  $0 --save    Generate and save key to .env file"
        echo "  $0 --help    Show this help message"
        ;;
    *)
        key=$(generate_key)
        echo "Generated encryption key:"
        echo ""
        echo "  $key"
        echo ""
        echo "To use this key:"
        echo "  1. Export it:     export MONOFS_ENCRYPTION_KEY=$key"
        echo "  2. Or save it:    echo \"MONOFS_ENCRYPTION_KEY=$key\" >> .env"
        echo ""
        echo "To save to .env automatically:"
        echo "  $0 --save"
        ;;
esac
