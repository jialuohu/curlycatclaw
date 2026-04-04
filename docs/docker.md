# Docker Deployment

## Quick Start

```bash
git clone https://github.com/jialuohu/curlycatclaw.git && cd curlycatclaw
mkdir -p ~/.curlycatclaw && cp config.toml.example ~/.curlycatclaw/config.toml
# Edit ~/.curlycatclaw/config.toml with your credentials
docker compose up -d
docker compose exec ollama ollama pull bge-m3  # first run only
```

## Services

`docker compose up` starts three services:

- **curlycatclaw** -- the agent daemon (Debian bookworm-slim + Claude CLI + gws CLI)
- **qdrant** -- vector search (Qdrant v1.14.0, ports 6333/6334)
- **ollama** -- local embeddings (bge-m3 by default, port 11434)

## Configuration

All config lives in `~/.curlycatclaw/config.toml`. Docker Compose mounts `~/.curlycatclaw` as `/data` inside the container, so all paths in config.toml use `/data/...`:

| Config field | Value | Notes |
|-------------|-------|-------|
| `db_path` | `/data/curlycatclaw.db` | SQLite database |
| `isolated_home` | `/data/claude-home` | Isolated Claude home for plugins |
| `qdrant_addr` | `qdrant:6334` | Docker Compose networking |
| `ollama_url` | `http://ollama:11434` | Docker Compose networking |

The only env var in docker-compose.yml is `HOME=/data` (so the Claude CLI finds its config). Everything else is in config.toml.

## Google Workspace

To add Gmail, Calendar, Drive, Sheets, Docs, Tasks access:

1. On a machine with a browser, install gws and authenticate:
   ```bash
   npm install -g @googleworkspace/cli
   gws auth login -s drive,gmail,calendar,sheets,docs,tasks
   gws auth export --unmasked > ~/.curlycatclaw/gws-credentials.json
   ```

2. Add to `config.toml`:
   ```toml
   [[mcp.servers]]
   name    = "gws"
   command = "curlycatclaw-gws-mcp"
   [mcp.servers.env]
   GWS_PATH = "gws"
   GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE = "/data/gws-credentials.json"
   ```

3. Rebuild and restart:
   ```bash
   docker compose build curlycatclaw && docker compose up -d
   ```

## MCP Servers & Plugin Runtimes

MCP servers are launched via `exec.Command` (stdio transport). The Docker image
includes runtimes for the most common plugin types:

- **npx** (Node.js) -- context7, firebase, playwright, sentry
- **bun** -- discord, imessage, fakechat
- **python3 / uvx** -- Python-based MCP servers (scrapling)
- **gws** -- Google Workspace CLI
- **git** -- marketplace clone operations

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

Generate a master key and store it in `~/.curlycatclaw/env` (loaded via `env_file` in docker-compose, not committed to git):

```bash
echo "CURLYCATCLAW_MASTER_KEY=$(openssl rand -hex 32)" > ~/.curlycatclaw/env
chmod 600 ~/.curlycatclaw/env
```

Then restart: `docker compose up -d`. Once configured, you can set API keys for MCP extensions via Telegram chat (e.g., "set CORE_API_KEY for paper-search-mcp") and they'll be encrypted at rest.
