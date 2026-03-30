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

# Homebrew availability
if command -v brew >/dev/null 2>&1; then
  BREW_AVAILABLE="true"
else
  BREW_AVAILABLE="false"
fi

# Install method decision: Homebrew on macOS only; GitHub Releases on Linux always
# (Linux Homebrew tap does not publish prebuilt bottles, would compile from source)
if [ "$OS" = "darwin" ] && [ "$BREW_AVAILABLE" = "true" ]; then
  INSTALL_METHOD="homebrew"
else
  INSTALL_METHOD="github_releases"
fi

# Docker status (timeout prevents hanging on a malfunctioning daemon)
if timeout 5 docker info >/dev/null 2>&1; then
  DOCKER="running"
elif command -v docker >/dev/null 2>&1; then
  DOCKER="installed_not_running"
else
  DOCKER="not_found"
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
BREW_AVAILABLE=$BREW_AVAILABLE
INSTALL_METHOD=$INSTALL_METHOD
DOCKER=$DOCKER
DOCKER_SUDO=$DOCKER_SUDO
CURLYCATCLAW_INSTALLED=$CURLYCATCLAW_INSTALLED
CURLYCATCLAW_VERSION=$CURLYCATCLAW_VERSION
LATEST_VERSION=$LATEST_VERSION
QDRANT_RUNNING=$QDRANT_RUNNING
CONFIG_EXISTS=$CONFIG_EXISTS
PORT_8080=$PORT_8080
--- END ---
EOF
