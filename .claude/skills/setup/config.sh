#!/usr/bin/env bash
# DEPRECATED: Interactive config generation is now handled by SKILL.md Step 6.
# This script remains as a fallback for automated/non-interactive setups.
#
# config.sh -- Generate curlycatclaw config.toml from a credentials file.
# Reads credentials from a temp file (NOT from env vars on cmdline) to avoid
# exposing secrets in /proc/PID/cmdline.
#
# Usage: bash config.sh /path/to/creds.tmp
#
# The creds file must contain key=value lines:
#   ANTHROPIC_AUTH_TOKEN=... (OAuth token, preferred)
#   ANTHROPIC_API_KEY=sk-ant-... (API key, alternative)
#   TELEGRAM_TOKEN=123456:ABC...
#   TELEGRAM_USER_ID=123456789
# Provide exactly one of ANTHROPIC_AUTH_TOKEN or ANTHROPIC_API_KEY.
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
ANTHROPIC_AUTH_TOKEN=""
TELEGRAM_TOKEN=""
TELEGRAM_USER_ID=""
while IFS='=' read -r key value; do
  # Skip empty lines and comments
  [ -z "$key" ] && continue
  [[ "$key" =~ ^# ]] && continue
  # Trim whitespace (use printf to avoid echo flag interpretation on values starting with -)
  value=$(printf '%s' "$value" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
  case "$key" in
    ANTHROPIC_API_KEY)    ANTHROPIC_API_KEY="$value" ;;
    ANTHROPIC_AUTH_TOKEN) ANTHROPIC_AUTH_TOKEN="$value" ;;
    TELEGRAM_TOKEN)       TELEGRAM_TOKEN="$value" ;;
    TELEGRAM_USER_ID)     TELEGRAM_USER_ID="$value" ;;
    CLAUDE_CLI_PATH)      CLAUDE_CLI_PATH="$value" ;;
    EMBEDDER_CHOICE)      EMBEDDER_CHOICE="$value" ;;
    VOYAGE_API_KEY)       VOYAGE_API_KEY="$value" ;;
    INSTALL_METHOD)       INSTALL_METHOD="$value" ;;
  esac
done < "$CREDS_FILE"

# Defaults for optional fields
EMBEDDER_CHOICE="${EMBEDDER_CHOICE:-fnv}"
VOYAGE_API_KEY="${VOYAGE_API_KEY:-}"
INSTALL_METHOD="${INSTALL_METHOD:-docker}"

# Validate required fields: need exactly one of API key or auth token
if [ -z "$ANTHROPIC_API_KEY" ] && [ -z "$ANTHROPIC_AUTH_TOKEN" ]; then
  echo "ERROR: either ANTHROPIC_AUTH_TOKEN or ANTHROPIC_API_KEY required in credentials file" >&2
  exit 1
fi
if [ -n "$ANTHROPIC_API_KEY" ] && [ -n "$ANTHROPIC_AUTH_TOKEN" ]; then
  echo "ERROR: provide either ANTHROPIC_AUTH_TOKEN or ANTHROPIC_API_KEY, not both" >&2
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
_AUTH_VALUE="${ANTHROPIC_AUTH_TOKEN:-$ANTHROPIC_API_KEY}"
if [[ "$_AUTH_VALUE" == *'"'* ]] || [[ "$TELEGRAM_TOKEN" == *'"'* ]]; then
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
$(if [ -n "$CLAUDE_CLI_PATH" ]; then echo "cli_path    = \"$CLAUDE_CLI_PATH\""; echo "oauth_token = \"$ANTHROPIC_AUTH_TOKEN\""; else echo "api_key = \"$ANTHROPIC_API_KEY\""; fi)
model   = "claude-sonnet-4-6-20250514"

[telegram]
token = "$TELEGRAM_TOKEN"
allowed_user_ids = [$TELEGRAM_USER_ID]

[storage]
db_path = "$DB_PATH"

$(
QDRANT_ADDR="localhost:6334"
if [ "$INSTALL_METHOD" = "docker" ]; then
  QDRANT_ADDR="qdrant:6334"
fi
if [ "$EMBEDDER_CHOICE" = "ollama" ]; then
  OLLAMA_URL="http://localhost:11434"
  if [ "$INSTALL_METHOD" = "docker" ]; then
    OLLAMA_URL="http://ollama:11434"
  fi
  cat << VECTOR_EOF
[vector]
enabled    = true
qdrant_addr = "$QDRANT_ADDR"
embedder   = "ollama"
ollama_url = "$OLLAMA_URL"
ollama_model = "bge-m3"
ollama_dim = 1024
VECTOR_EOF
elif [ "$EMBEDDER_CHOICE" = "voyage" ]; then
  cat << VECTOR_EOF
[vector]
enabled    = true
qdrant_addr = "$QDRANT_ADDR"
embedder   = "voyage"
voyage_api_key = "$VOYAGE_API_KEY"
voyage_model = "voyage-3-lite"
voyage_dim = 512
VECTOR_EOF
else
  cat << VECTOR_EOF
[vector]
enabled    = true
qdrant_addr = "$QDRANT_ADDR"
embedder   = "fnv"
VECTOR_EOF
fi
)

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
