# Docker Deployment

## Quick Start

```bash
# Your existing ~/.curlycatclaw/config.toml is used directly.
# Docker overrides paths via environment variables (see docker-compose.yml).
docker compose up -d
```

## Services

`docker compose up` starts both services:

- **curlycatclaw** -- the agent daemon (Debian bookworm-slim + Claude CLI via npm)
- **qdrant** -- vector search (Qdrant v1.14.0, ports 6333/6334)

## Environment Variable Overrides

Docker uses the same `config.toml` as local, with these env vars overriding paths:

| Env Var | Docker Value | Purpose |
|---------|-------------|---------|
| `CURLYCATCLAW_DB_PATH` | `/data/curlycatclaw.db` | SQLite path inside container |
| `CURLYCATCLAW_QDRANT_ADDR` | `qdrant:6334` | Compose networking |
| `CURLYCATCLAW_CLI_PATH` | `/usr/local/bin/claude` | Claude CLI installed via npm |
| `CURLYCATCLAW_ISOLATED_HOME` | `/data/claude-home` | Isolated Claude home for plugin management |
| `HOME` | `/data` | So CLI finds config at /data |

## MCP Servers

MCP servers are launched via `exec.Command` (stdio transport). The Debian-based
Docker image includes Node.js (for Claude CLI) but not Python or other runtimes.
MCP servers that require these will not work inside the container.

Options:
- Run MCP-dependent servers on the host and connect via network
- Use a custom Dockerfile with the required runtimes installed
- Disable MCP servers in your Docker config

## Data

The `~/.curlycatclaw` directory is bind-mounted at `/data`. SQLite uses WAL mode
and lives directly on the host filesystem. No named volumes needed.

## Backups

```bash
cp ~/.curlycatclaw/curlycatclaw.db ./backup.db
```

## Encrypted MCP Credentials

Create a `.env` file in the project root:

```
CURLYCATCLAW_MASTER_KEY=<64 hex chars for encrypted MCP credentials>
```
