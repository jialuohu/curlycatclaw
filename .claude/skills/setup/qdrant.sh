#!/usr/bin/env bash
# qdrant.sh -- Qdrant container lifecycle management for curlycatclaw setup.
# Subcommands: check, start, stop, health
set -uo pipefail

CONTAINER_NAME="curlycatclaw-qdrant"
QDRANT_IMAGE="qdrant/qdrant:v1.14.0"
VOLUME_NAME="curlycatclaw_qdrant"
DOCKER_CMD="${DOCKER_CMD:-docker}"

check() {
  if $DOCKER_CMD ps --filter "name=$CONTAINER_NAME" --format '{{.Status}}' 2>/dev/null | grep -q "Up"; then
    echo "--- STATUS ---"
    echo "QDRANT_RUNNING=true"
    echo "QDRANT_CONTAINER=$CONTAINER_NAME"
    echo "--- END ---"
  elif $DOCKER_CMD ps -a --filter "name=$CONTAINER_NAME" --format '{{.Status}}' 2>/dev/null | grep -q .; then
    echo "--- STATUS ---"
    echo "QDRANT_RUNNING=false"
    echo "QDRANT_CONTAINER_EXISTS=true"
    echo "--- END ---"
  else
    echo "--- STATUS ---"
    echo "QDRANT_RUNNING=false"
    echo "QDRANT_CONTAINER_EXISTS=false"
    echo "--- END ---"
  fi
}

start() {
  # If container exists but is stopped, remove it first
  if $DOCKER_CMD ps -a --filter "name=$CONTAINER_NAME" --format '{{.ID}}' 2>/dev/null | grep -q .; then
    if $DOCKER_CMD ps --filter "name=$CONTAINER_NAME" --format '{{.ID}}' 2>/dev/null | grep -q .; then
      echo "Qdrant is already running."
      return 0
    fi
    echo "Removing stopped Qdrant container..."
    $DOCKER_CMD rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
  fi

  echo "Starting Qdrant..."
  if ! $DOCKER_CMD run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    -p 127.0.0.1:6333:6333 \
    -p 127.0.0.1:6334:6334 \
    -v "$VOLUME_NAME":/qdrant/storage \
    "$QDRANT_IMAGE" 2>&1; then
    echo "--- STATUS ---"
    echo "QDRANT_STARTED=false"
    echo "QDRANT_ERROR=docker run failed"
    echo "--- END ---"
    return 1
  fi

  echo "Waiting for Qdrant to be healthy..."
  local retries=0
  local max_retries=30
  while [ $retries -lt $max_retries ]; do
    if curl -sf http://127.0.0.1:6333/healthz >/dev/null 2>&1; then
      echo "--- STATUS ---"
      echo "QDRANT_STARTED=true"
      echo "--- END ---"
      return 0
    fi
    retries=$((retries + 1))
    sleep 1
  done

  echo "--- STATUS ---"
  echo "QDRANT_STARTED=false"
  echo "QDRANT_ERROR=timeout after ${max_retries}s"
  echo "--- END ---"
  return 1
}

stop() {
  if $DOCKER_CMD ps --filter "name=$CONTAINER_NAME" --format '{{.ID}}' 2>/dev/null | grep -q .; then
    echo "Stopping Qdrant..."
    $DOCKER_CMD stop "$CONTAINER_NAME" >/dev/null 2>&1
    echo "Qdrant stopped."
  else
    echo "Qdrant is not running."
  fi
}

health() {
  local retries=0
  local max_retries=5
  while [ $retries -lt $max_retries ]; do
    if curl -sf http://127.0.0.1:6333/healthz >/dev/null 2>&1; then
      echo "--- STATUS ---"
      echo "QDRANT_HEALTHY=true"
      echo "--- END ---"
      return 0
    fi
    retries=$((retries + 1))
    sleep 1
  done

  echo "--- STATUS ---"
  echo "QDRANT_HEALTHY=false"
  echo "--- END ---"
  return 1
}

case "${1:-help}" in
  check)  check ;;
  start)  start ;;
  stop)   stop ;;
  health) health ;;
  *)
    echo "Usage: $0 {check|start|stop|health}"
    echo "  DOCKER_CMD env var overrides docker binary (e.g., DOCKER_CMD='sudo docker')"
    exit 1
    ;;
esac
