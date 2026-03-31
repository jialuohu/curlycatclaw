#!/usr/bin/env bash
# detect.sh -- OS/arch/dependency detection for curlycatclaw setup.
# Outputs a structured STATUS block for Claude to parse.
set -euo pipefail

# OS detection
RAW_OS=$(uname -s)
case "$RAW_OS" in
  Linux)  OS="linux" ;;
  Darwin) OS="darwin" ;;
  *)      OS="unsupported" ;;
esac

# Architecture detection (translate to Go conventions)
RAW_ARCH=$(uname -m)
case "$RAW_ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  arm64)   ARCH="arm64" ;;
  *)       ARCH="unsupported" ;;
esac

# Docker status (timeout prevents hanging on a malfunctioning daemon)
if timeout 5 docker info >/dev/null 2>&1; then
  DOCKER="running"
elif command -v docker >/dev/null 2>&1; then
  DOCKER="installed_not_running"
else
  DOCKER="not_found"
fi

# Install method: Docker preferred, GitHub Releases as fallback
if [ "$DOCKER" = "running" ] || [ "$DOCKER" = "installed_not_running" ]; then
  INSTALL_METHOD="docker"
else
  INSTALL_METHOD="github_releases"
fi

# Docker sudo detection: can the user run docker without sudo?
if [ "$DOCKER" = "running" ]; then
  DOCKER_SUDO="not_needed"
elif [ "$DOCKER" = "installed_not_running" ]; then
  # Docker exists but daemon not running; check if user has docker group
  if groups 2>/dev/null | grep -qw docker; then
    DOCKER_SUDO="not_needed"
  elif sudo -n true 2>/dev/null; then
    DOCKER_SUDO="true"
  else
    DOCKER_SUDO="false"
  fi
elif [ "$DOCKER" = "not_found" ]; then
  DOCKER_SUDO="not_applicable"
fi

# Existing curlycatclaw installation
if command -v curlycatclaw >/dev/null 2>&1; then
  CURLYCATCLAW_INSTALLED="true"
  CURLYCATCLAW_VERSION=$(curlycatclaw --version 2>/dev/null | head -1 || echo "unknown")
else
  CURLYCATCLAW_INSTALLED="false"
  CURLYCATCLAW_VERSION=""
fi

# Claude CLI detection (needed for CLI subprocess mode)
if command -v claude >/dev/null 2>&1; then
  CLAUDE_CLI_INSTALLED="true"
  CLAUDE_CLI_PATH=$(command -v claude)
  CLAUDE_CLI_VERSION=$(claude --version 2>/dev/null | head -1 || echo "unknown")
else
  CLAUDE_CLI_INSTALLED="false"
  CLAUDE_CLI_PATH=""
  CLAUDE_CLI_VERSION=""
fi

# Headless detection (can we open a browser for OAuth?)
if [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ] || [ "$OS" = "darwin" ]; then
  HAS_BROWSER="true"
else
  HAS_BROWSER="false"
fi

# Latest release version (via GitHub API, best-effort)
if command -v gh >/dev/null 2>&1; then
  LATEST_VERSION=$(gh api repos/jialuohu/curlycatclaw/releases/latest --jq .tag_name 2>/dev/null || echo "unknown")
elif command -v curl >/dev/null 2>&1; then
  LATEST_VERSION=$(curl -s https://api.github.com/repos/jialuohu/curlycatclaw/releases/latest 2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//' || echo "unknown")
else
  LATEST_VERSION="unknown"
fi

# Qdrant container status
if docker ps --filter name=curlycatclaw-qdrant --format '{{.Status}}' 2>/dev/null | grep -q "Up"; then
  QDRANT_RUNNING="true"
else
  QDRANT_RUNNING="false"
fi

# Config file existence
if [ -f "$HOME/.curlycatclaw/config.toml" ]; then
  CONFIG_EXISTS="true"
else
  CONFIG_EXISTS="false"
fi

# Port 8080 availability (health endpoint default)
if command -v ss >/dev/null 2>&1; then
  if ss -tlnp 2>/dev/null | grep -q ':8080 '; then
    PORT_8080="in_use"
  else
    PORT_8080="free"
  fi
elif command -v lsof >/dev/null 2>&1; then
  if lsof -i :8080 >/dev/null 2>&1; then
    PORT_8080="in_use"
  else
    PORT_8080="free"
  fi
else
  PORT_8080="unknown"
fi

# Output structured status block
cat << EOF
--- STATUS ---
OS=$OS
ARCH=$ARCH
INSTALL_METHOD=$INSTALL_METHOD
DOCKER=$DOCKER
DOCKER_SUDO=$DOCKER_SUDO
CURLYCATCLAW_INSTALLED=$CURLYCATCLAW_INSTALLED
CURLYCATCLAW_VERSION=$CURLYCATCLAW_VERSION
CLAUDE_CLI_INSTALLED=$CLAUDE_CLI_INSTALLED
CLAUDE_CLI_PATH=$CLAUDE_CLI_PATH
CLAUDE_CLI_VERSION=$CLAUDE_CLI_VERSION
HAS_BROWSER=$HAS_BROWSER
LATEST_VERSION=$LATEST_VERSION
QDRANT_RUNNING=$QDRANT_RUNNING
CONFIG_EXISTS=$CONFIG_EXISTS
PORT_8080=$PORT_8080
--- END ---
EOF
