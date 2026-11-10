#!/bin/sh
set -eu

umask 077
data=${WHATSAPP_MCP_DATA_DIR:-/data}
mkdir -p "$data"
chmod 0700 "$data"

if [ -n "${WHATSAPP_MCP_WEBHOOK_CONFIG_JSON:-}" ] && [ "${WHATSAPP_MCP_WEBHOOK_CONFIG+x}" != x ]; then
    mkdir -p "$data/secrets"
    chmod 0700 "$data/secrets"
    tmp=$(mktemp "$data/secrets/.webhook.json.XXXXXX")
    trap 'rm -f "$tmp"' 0
    printf '%s' "$WHATSAPP_MCP_WEBHOOK_CONFIG_JSON" > "$tmp"
    chmod 0600 "$tmp"
    mv -f "$tmp" "$data/secrets/webhook.json"
    trap - 0
    export WHATSAPP_MCP_WEBHOOK_CONFIG="$data/secrets/webhook.json"
fi
unset WHATSAPP_MCP_WEBHOOK_CONFIG_JSON

exec whatsapp-mcp serve --transport http --http-addr "0.0.0.0:${PORT:-8080}" "$@"
