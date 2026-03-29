#!/usr/bin/env bash
# config.sh -- Generate curlycatclaw config.toml from a credentials file.
# Reads credentials from a temp file (NOT from env vars on cmdline) to avoid
# exposing secrets in /proc/PID/cmdline.
#
# Usage: bash config.sh /path/to/creds.tmp
#
# The creds file must contain key=value lines:
#   ANTHROPIC_API_KEY=sk-ant-...
#   TELEGRAM_TOKEN=123456:ABC...
#   TELEGRAM_USER_ID=123456789
set -euo pipefail

CREDS_FILE="${1:-}"
if [ -z "$CREDS_FILE" ] || [ ! -f "$CREDS_FILE" ]; then
  echo "ERROR: credentials file required. Usage: $0 /path/to/creds.tmp" >&2
  exit 1
fi

# Ensure credentials file is cleaned up on any exit (security: contains API keys)
trap 'rm -f "$CREDS_FILE"' EXIT

# Read credentials from file
ANTHROPIC_API_KEY=""
TELEGRAM_TOKEN=""
TELEGRAM_USER_ID=""
while IFS='=' read -r key value; do
  # Skip empty lines and comments
  [ -z "$key" ] && continue
  [[ "$key" =~ ^# ]] && continue
  # Trim whitespace (use printf to avoid echo flag interpretation on values starting with -)
  value=$(printf '%s' "$value" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
  case "$key" in
    ANTHROPIC_API_KEY) ANTHROPIC_API_KEY="$value" ;;
    TELEGRAM_TOKEN)    TELEGRAM_TOKEN="$value" ;;
    TELEGRAM_USER_ID)  TELEGRAM_USER_ID="$value" ;;
  esac
done < "$CREDS_FILE"

# Validate required fields
if [ -z "$ANTHROPIC_API_KEY" ]; then
  echo "ERROR: ANTHROPIC_API_KEY not found in credentials file" >&2
  exit 1
fi
if [ -z "$TELEGRAM_TOKEN" ]; then
  echo "ERROR: TELEGRAM_TOKEN not found in credentials file" >&2
  exit 1
fi
if [ -z "$TELEGRAM_USER_ID" ]; then
  echo "ERROR: TELEGRAM_USER_ID not found in credentials file" >&2
  exit 1
fi
# Validate TELEGRAM_USER_ID is numeric (prevents TOML injection)
if ! [[ "$TELEGRAM_USER_ID" =~ ^[0-9]+$ ]]; then
  echo "ERROR: TELEGRAM_USER_ID must be numeric, got: $TELEGRAM_USER_ID" >&2
  exit 1
fi
# Reject double-quotes in credential values (prevents TOML injection)
if [[ "$ANTHROPIC_API_KEY" == *'"'* ]] || [[ "$TELEGRAM_TOKEN" == *'"'* ]]; then
  echo "ERROR: credential values must not contain double-quote characters" >&2
  exit 1
fi

# Resolve paths (expand $HOME, no ~ in TOML)
CONFIG_DIR="$HOME/.curlycatclaw"
CONFIG_PATH="$CONFIG_DIR/config.toml"
DB_PATH="$CONFIG_DIR/curlycatclaw.db"

# Auto-detect timezone (try timedatectl first, fall back to /etc/localtime symlink)
TIMEZONE="UTC"
TZ_DETECTED=""
if command -v timedatectl >/dev/null 2>&1; then
  TZ_DETECTED=$(timedatectl show -p Timezone --value 2>/dev/null || true)
fi
if [ -z "$TZ_DETECTED" ] || [ "$TZ_DETECTED" = "n/a" ]; then
  if [ -L /etc/localtime ]; then
    TZ_DETECTED=$(readlink /etc/localtime 2>/dev/null | sed 's|.*/zoneinfo/||' || true)
  fi
fi
if [ -n "$TZ_DETECTED" ] && [ "$TZ_DETECTED" != "n/a" ]; then
  TIMEZONE="$TZ_DETECTED"
fi

# Create config directory
mkdir -p "$CONFIG_DIR"

# Write config.toml
cat > "$CONFIG_PATH" << TOML_EOF
timezone = "$TIMEZONE"

[claude]
api_key = "$ANTHROPIC_API_KEY"
model   = "claude-sonnet-4-6-20250514"

[telegram]
token = "$TELEGRAM_TOKEN"
allowed_user_ids = [$TELEGRAM_USER_ID]

[storage]
db_path = "$DB_PATH"

[vector]
enabled    = true
qdrant_addr = "localhost:6334"
embedder   = "fnv"

[memory]
enabled = true
max_facts = 50
summary_relevance_limit = 3
summary_score_threshold = 0.3
min_messages_to_summarize = 4
vector_search_timeout_seconds = 5

[health]
enabled = true
port    = 8080
TOML_EOF

# Secure the config file (contains API keys)
chmod 600 "$CONFIG_PATH"

# Credentials file cleanup handled by EXIT trap

echo "--- STATUS ---"
echo "CONFIG_WRITTEN=true"
echo "CONFIG_PATH=$CONFIG_PATH"
echo "DB_PATH=$DB_PATH"
echo "TIMEZONE=$TIMEZONE"
echo "PERMISSIONS=$(stat -c '%a' "$CONFIG_PATH" 2>/dev/null || stat -f '%Lp' "$CONFIG_PATH" 2>/dev/null || echo 'unknown')"
echo "--- END ---"
