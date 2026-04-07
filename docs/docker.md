# Docker Deployment

## Quick Start

```bash
git clone https://github.com/jialuohu/curlycatclaw.git && cd curlycatclaw
mkdir -p ~/.curlycatclaw && cp config.toml.example ~/.curlycatclaw/config.toml
# Edit ~/.curlycatclaw/config.toml with your credentials
docker compose up -d
```

This pulls pre-built images from GHCR and starts the services.

### Compose Override Pattern

The project uses a single `docker-compose.yml` as the base file. It references pre-built images by default (no `build:` directives). For local development, an override file adds `build:` directives so `docker compose build` works:

```bash
cp docker-compose.override.yml.example docker-compose.override.yml
docker compose build && docker compose up -d
```

When `docker-compose.override.yml` is present, Docker Compose automatically merges it with the base file. Remove or rename it to go back to pulling pre-built images.

### Profiles

Optional services are gated behind Compose profiles:

- **ollama** -- local embeddings (bge-m3). Enable with `COMPOSE_PROFILES=ollama`:
  ```bash
  COMPOSE_PROFILES=ollama docker compose up -d
  docker compose exec ollama ollama pull bge-m3  # first run only
  ```
- **updater** -- self-update sidecar. Enable with `COMPOSE_PROFILES=updater`.

Enable multiple profiles at once:
```bash
COMPOSE_PROFILES=ollama,updater docker compose up -d
```

## Services

`docker compose up` starts the core services (plus optional ones via profiles):

- **curlycatclaw** -- the agent daemon (Debian bookworm-slim + Claude CLI + gws CLI)
- **qdrant** -- vector search (Qdrant v1.17.1, ports 6333/6334)
- **ollama** (profile: `ollama`) -- local embeddings (bge-m3 by default, port 11434)
- **curlycatclaw-updater** (profile: `updater`) -- sidecar for self-update. Holds the Docker socket, exposes an authenticated HTTP API on port 8081 for image pulls, container restarts, and rollbacks. Enable via `[update]` config section.

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

3. Restart:
   ```bash
   docker compose up -d
   ```

### Multi-account GWS

For multiple Google accounts, replace `GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE` with `GWS_ACCOUNT_*` env vars. Export credentials for each account separately, then configure:

```toml
[mcp.servers.env]
GWS_PATH = "gws"
GWS_ACCOUNT_PERSONAL          = "/data/gws-credentials.json"
GWS_ACCOUNT_PERSONAL_SERVICES = "gmail,calendar,drive,sheets,docs,slides,tasks"
GWS_ACCOUNT_WORK              = "/data/gws-work-credentials.json"
GWS_ACCOUNT_WORK_SERVICES     = "gmail"
GWS_DEFAULT_ACCOUNT           = "personal"
```

See [configuration.md](configuration.md) for full details.

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

## Updater Sidecar

The updater sidecar enables self-update from Telegram (`/update`, `/status`, `/rollback`). It runs as a separate container with access to the Docker socket.

**Required env vars** (set in `~/.curlycatclaw/env` or docker-compose override):

| Variable | Purpose |
|----------|---------|
| `UPDATER_SECRET` | Shared secret for authentication between the main container and sidecar (required) |
| `CURLYCATCLAW_IMAGE` | Docker image reference, e.g. `ghcr.io/jialuohu/curlycatclaw:latest`. Used for rollback override support |

The sidecar checks GHCR for new image digests, pulls updates, and restarts the main container. Rollback keeps the 3 most recent digests. A digest blacklist (24h TTL) prevents retry loops on broken images.

To enable, add `[update]` to `config.toml`:

```toml
[update]
enabled     = true
updater_url = "http://curlycatclaw-updater:8081"  # default
auto_update = false                                # opt-in scheduled updates
schedule    = "0 3 * * 0"                          # cron (default: weekly Sunday 3am)
```

## Encrypted MCP Credentials

Generate a master key and store it in `~/.curlycatclaw/env` (loaded via `env_file` in docker-compose, not committed to git):

```bash
echo "CURLYCATCLAW_MASTER_KEY=$(openssl rand -hex 32)" > ~/.curlycatclaw/env
chmod 600 ~/.curlycatclaw/env
```

Then restart: `docker compose up -d`. Once configured, you can set API keys for MCP extensions via Telegram chat (e.g., "set CORE_API_KEY for paper-search-mcp") and they'll be encrypted at rest.
