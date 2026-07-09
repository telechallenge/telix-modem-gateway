#!/bin/sh
set -e

CERT_DIR=/etc/nginx/certs
CERT="$CERT_DIR/telix.crt"
KEY="$CERT_DIR/telix.key"

mkdir -p "$CERT_DIR"

if [ ! -s "$CERT" ] || [ ! -s "$KEY" ]; then
    if ! command -v openssl >/dev/null 2>&1; then
        echo "Installing openssl (needed for one-time cert generation)..."
        apk add --no-cache openssl >/dev/null
    fi
    echo "Generating self-signed certificate for Cloudflare origin..."
    openssl req -x509 -nodes -newkey rsa:2048 \
        -days 3650 \
        -keyout "$KEY" \
        -out "$CERT" \
        -subj "/CN=telix-origin" \
        -addext "subjectAltName=DNS:telix-origin,DNS:localhost"
    chmod 600 "$KEY"
fi

exec nginx -g 'daemon off;'
