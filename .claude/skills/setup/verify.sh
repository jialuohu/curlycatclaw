#!/usr/bin/env bash
# verify.sh -- End-to-end verification for curlycatclaw setup.
# Checks health endpoint, Qdrant, service status, and config validity.
set -euo pipefail

CONFIG_PATH="${1:-$HOME/.curlycatclaw/config.toml}"
HEALTH_PORT="${2:-18080}"

# Check config file exists and has correct permissions
CONFIG_VALID="false"
CONFIG_PERMS="unknown"
if [ -f "$CONFIG_PATH" ]; then
  CONFIG_PERMS=$(stat -c '%a' "$CONFIG_PATH" 2>/dev/null || stat -f '%Lp' "$CONFIG_PATH" 2>/dev/null || echo "unknown")
  CONFIG_VALID="true"
fi

CONFIG_PERMS_OK="false"
if [ "$CONFIG_PERMS" = "600" ]; then
  CONFIG_PERMS_OK="true"
fi

# Check health endpoint
HEALTH="fail"
if curl -sf "http://127.0.0.1:${HEALTH_PORT}/health" >/dev/null 2>&1; then
  HEALTH="ok"
fi

# Check Qdrant health
QDRANT="fail"
if curl -sf http://127.0.0.1:6333/healthz >/dev/null 2>&1; then
  QDRANT="ok"
fi

# Check if curlycatclaw process is running
SERVICE="stopped"
if pgrep -x curlycatclaw >/dev/null 2>&1; then
  SERVICE="running"
elif docker compose ps 2>/dev/null | grep -q "curlycatclaw.*Up"; then
  SERVICE="running"
fi

# Output structured status block
cat << EOF
--- STATUS ---
HEALTH=$HEALTH
QDRANT=$QDRANT
SERVICE=$SERVICE
CONFIG_VALID=$CONFIG_VALID
CONFIG_PATH=$CONFIG_PATH
CONFIG_PERMS=$CONFIG_PERMS
CONFIG_PERMS_OK=$CONFIG_PERMS_OK
--- END ---
EOF
