# Docker Deployment

## Quick Start

```bash
# Copy and edit your config
cp config.toml.example config.toml
# Edit config.toml: set API keys, db_path = "/data/curlycatclaw.db",
# qdrant_addr = "qdrant:6334" (for compose networking)

# Copy config into the data volume
docker compose up -d
docker compose cp config.toml curlycatclaw:/data/config.toml
docker compose restart curlycatclaw
```

## Services

`docker compose up` starts both services:

- **curlycatclaw** -- the agent daemon
- **qdrant** -- vector search (Qdrant v1.14.0, ports 6333/6334)

## MCP Servers

MCP servers are launched via `exec.Command` (stdio transport). The Alpine-based
Docker image does not include Node.js, Python, or other runtimes. MCP servers
that require these will not work inside the container.

Options:
- Run MCP-dependent servers on the host and connect via network
- Use a custom Dockerfile with the required runtimes installed
- Disable MCP servers in your Docker config

## SQLite + Docker Volumes

The SQLite database uses WAL mode. Use Docker named volumes (the default in
docker-compose.yml), not bind mounts on networked filesystems (NFS, CIFS).
WAL mode does not work correctly over network filesystems.

## Backups

```bash
# Copy the database out of the volume
docker compose cp curlycatclaw:/data/curlycatclaw.db ./backup.db
```

## Environment Variables

Create a `.env` file (see `.env.example`):

```
CURLYCATCLAW_MASTER_KEY=<64 hex chars for encrypted MCP credentials>
```
