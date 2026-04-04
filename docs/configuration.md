# Configuration

All config lives in `~/.curlycatclaw/config.toml` (mounted as `/data/config.toml` inside Docker). Copy from the example and fill in your credentials. See [`config.toml.example`](../config.toml.example) for the full reference.

```toml
timezone = "America/Los_Angeles"

[claude]
cli_path      = "/usr/local/bin/claude"
oauth_token   = "sk-ant-oat01-..."          # from `claude setup-token`
model         = "claude-sonnet-4-6-20250514"
isolated_home = "/data/claude-home"

[telegram]
token = "123456:ABC-DEF..."
allowed_user_ids = [123456789]

[storage]
db_path = "/data/curlycatclaw.db"

[vector]
enabled     = true
qdrant_addr = "qdrant:6334"
embedder    = "ollama"
ollama_url  = "http://ollama:11434"
ollama_model = "bge-m3"
ollama_dim   = 1024

[memory]
enabled = true

[health]
enabled = true
port    = 8080
```

## Google Workspace (optional)

Add Gmail, Calendar, Drive, Sheets, Docs, Tasks access. On a machine with a browser:

```bash
gws auth login -s drive,gmail,calendar,sheets,docs,tasks
gws auth export --unmasked > ~/.curlycatclaw/gws-credentials.json
```

Then add to `config.toml`:

```toml
[[mcp.servers]]
name    = "gws"
command = "curlycatclaw-gws-mcp"
[mcp.servers.env]
GWS_PATH = "gws"
GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE = "/data/gws-credentials.json"
```

Rebuild and restart: `docker compose build curlycatclaw && docker compose up -d`

## GitHub (optional)

Access repos, PRs, CI status, issues, and releases from Telegram. Create a [Personal Access Token](https://github.com/settings/tokens) (classic PAT with `repo` scope recommended for private repo access), then add to `config.toml`:

```toml
[[mcp.servers]]
name    = "github"
command = "github-mcp-server"
args    = ["stdio", "--toolsets", "repos,issues,pull_requests,actions,users", "--read-only"]
[mcp.servers.env]
GITHUB_PERSONAL_ACCESS_TOKEN = "ghp_..."
```

Remove `--read-only` if you need write operations (create issues, comment on PRs). Rebuild and restart.

## Encrypted MCP Credentials

For encrypted MCP credentials, set `CURLYCATCLAW_MASTER_KEY` env var (64 hex chars = 32 bytes). MCP servers, Wasm plugins, cron tasks, and other advanced options are documented in `config.toml.example`.
