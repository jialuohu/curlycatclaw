# Configuration

All config lives in `~/.curlycatclaw/config.toml` (mounted as `/data/config.toml` inside Docker). Copy from the example and fill in your credentials. See [`config.toml.example`](../config.toml.example) for the full reference.

```toml
timezone = "America/Los_Angeles"

[claude]
cli_path      = "/usr/local/bin/claude"
oauth_token   = "sk-ant-oat01-..."          # from `claude setup-token`
model         = "claude-sonnet-4-6-20250514"
# thinking_effort = "high"               # low/medium = standard, high/max = extended thinking
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
# summarize_model = "claude-haiku-4-5"  # cheaper model for conversation summarization (CLI mode)

[memory.observations]
enabled = true                 # enable automatic observation extraction
# extraction_interval = 3     # extract every N user turns (default: 3)
# cooldown_seconds = 60       # min seconds between extractions (default: 60)
# retrieval_limit = 8         # max observations in system prompt (default: 8)
# hybrid_search = false       # enable FTS5 + vector hybrid search (default: false)
# progressive_retrieval = false # enable 3-layer compact/expanded/detail retrieval (default: false)
# supersession_threshold = 0.8  # confidence threshold for auto-filtering superseded observations (default: 0.8)

[health]
enabled = true
port    = 8080
```

## Email Ingest (optional)

Background email-to-observation processing. Polls Gmail via the GWS MCP server, scores emails by importance with Claude, and extracts observations from important ones. Requires a GWS MCP server with Gmail-enabled accounts.

```toml
[email_ingest]
enabled = false
interval_minutes = 15       # poll interval for new emails
backfill_days = 30           # days of history to backfill on first run
batch_size = 20              # emails per backfill batch
max_daily_observations = 100 # per-account daily cap
max_daily_llm_calls = 200    # cost circuit breaker
min_importance = 3           # minimum importance to index (1-10)
labels = ["INBOX"]           # Gmail labels to process
skip_senders = ["noreply@", "no-reply@", "notifications@", "mailer-daemon@"]
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

### Multi-account GWS

To use multiple Google accounts through a single GWS MCP server, replace `GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE` with `GWS_ACCOUNT_*` env vars:

```toml
[mcp.servers.env]
GWS_PATH = "gws"
GWS_ACCOUNT_PERSONAL          = "/data/gws-credentials.json"
GWS_ACCOUNT_PERSONAL_SERVICES = "gmail,calendar,drive,sheets,docs,slides,tasks"
GWS_ACCOUNT_WORK              = "/data/gws-work-credentials.json"
GWS_ACCOUNT_WORK_SERVICES     = "gmail"
GWS_DEFAULT_ACCOUNT           = "personal"
GWS_FILTER = "gmail_*,calendar_*,drive_*,sheets_*,docs_*,slides_*,tasks_*"
```

Export credentials for each account separately. `_SERVICES` is optional (omit for full access). Claude picks the right account per request via the `account` parameter on every tool, or uses the default. Use `gws_list_accounts` to query available accounts.

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
