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

## MCP Servers & Plugin Runtimes

MCP servers are launched via `exec.Command` (stdio transport). The Docker image
includes runtimes for the most common plugin types:

- **npx** (Node.js) — context7, firebase, playwright, sentry
- **bun** — discord, imessage, fakechat
- **python3 / uvx** — Python-based MCP servers
- **git** — marketplace clone operations

Plugins that need `docker` or `php` are not supported inside the container.
The bot warns you after install if the required command is missing.

## Data

The `~/.curlycatclaw` directory is bind-mounted at `/data`. SQLite uses WAL mode
and lives directly on the host filesystem. No named volumes needed.

## Backups

```bash
cp ~/.curlycatclaw/curlycatclaw.db ./backup.db
```

## Encrypted MCP Credentials

Add the master key to the `environment` section in `docker-compose.yml`:

```yaml
environment:
  - CURLYCATCLAW_MASTER_KEY=<64 hex chars for encrypted MCP credentials>
```
