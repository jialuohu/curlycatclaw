<p align="center">
  <img src="assets/logo.png" alt="CurlyCatClaw" width="400" />
</p>

<h1 align="center">🐈CurlyCatClaw🦞</h1>

<p align="center">
  A personal AI assistant that lives in your Telegram. Built in Go.
</p>

<p align="center">
  <a href="https://github.com/jialuohu/curlycatclaw/actions"><img src="https://github.com/jialuohu/curlycatclaw/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
  <a href="https://github.com/jialuohu/curlycatclaw/releases"><img src="https://img.shields.io/github/v/release/jialuohu/curlycatclaw" alt="Release" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/jialuohu/curlycatclaw" alt="License" /></a>
</p>

---

CurlyCatClaw is a long-running daemon that connects Claude to Telegram. You message your bot, it thinks with Claude, calls tools via MCP, and replies. SQLite keeps your conversation history. That's it.

## Features

### Core

- **Telegram-native** — message your bot like you'd message a friend, with proper HTML formatting (bold, italic, code blocks, links)
- **Claude-powered** — streaming responses with tool use, direct API or CLI subprocess mode (Claude subscription)
- **Real-time streaming** — text deltas streamed via message edits (500ms debounce), final message rendered as Telegram HTML
- **Image understanding** — send photos, Claude sees them via vision
- **Project work** — `/project <name>` to do coding tasks (plan, implement, review) in a repo via Telegram

### Memory & Context

- **Conversation memory** — SQLite (WAL mode), sliding window context (25 turns, ~150K tokens), conversation history injected on subprocess restart so Claude doesn't forget mid-conversation
- **Hierarchical memory** — three tiers: user facts in system prompt, conversation summaries via Qdrant relevance search, current sliding window
- **Vector search** — semantic retrieval via Qdrant with pluggable embeddings (Ollama local default with bge-m3, FNV offline fallback, Voyage AI paid), background migration when switching providers (zero-downtime, crash-resumable)

### Extensibility

- **Google Workspace** — Gmail, Calendar, Drive, Sheets, Docs, Tasks via the gws CLI. Ask "what's on my calendar today?" or "send an email to Alice" from Telegram. Standalone MCP server (`curlycatclaw-gws-mcp`) discovers tools dynamically with boolean flag detection, argument validation, and server-side flag allowlists
- **GitHub** — check CI status, review PRs, create issues, search code, and track releases from Telegram via GitHub's official MCP server (`github-mcp-server`). Read-only by default, configurable toolsets
- **MCP tool integration** — connect any MCP server (search, filesystem, APIs) via stdio, add/remove at runtime via Telegram, proxied through curlycatclaw-skills for reliable tool discovery in CLI mode
- **Runtime extension registry** — add MCP servers, exec skills, and prompt-based skills through Telegram chat (`add_extension`, `remove_extension`, `list_extensions`), persisted to disk, no config edits or restarts needed, MCP extensions hot-reloaded instantly without losing conversation context
- **Encrypted API key management** — set API keys for MCP extensions via chat (`set_extension_env`), encrypted at rest with AES-256-GCM, resolved at spawn time
- **Built-in skills** — web search, notes, reminders (cron), semantic search, user facts, summary management, plugin management, extension management
- **Wasm plugins** — extend with custom skills via WebAssembly, capability-based security, 10 MiB query result cap, quote-aware SQL parameter binding, atomic hot-reload
- **External skill collections** — load exec-based skills from directory trees (`skill.toml` descriptors), minimal sandboxed env, fsnotify hot-reload
- **Plugin management** — install/manage Claude Code plugins through Telegram, standard plugins pre-installed on first startup (context7, playwright, ui-ux-pro-max, superpowers, claude-md-management, hookify, skill-creator)

### Operations

- **Health endpoint** — `GET /health` on localhost for Docker/monitoring liveness checks
- **Supervision** — automatic restart with exponential backoff, graceful 30s drain on shutdown
- **Cron tasks** — scheduled prompts run through Claude with full tool access, ephemeral context, 5-minute timeout
- **Configurable logging** — level, format (text/json), file output with lumberjack rotation
- **Docker ready** — docker-compose with Qdrant + Ollama and optional env file for master key, one command to run

### Security

- **Landlock sandbox** — Linux filesystem restriction (opt-in)
- **Encrypted credentials** — AES-256-GCM for MCP server secrets
- **Secure defaults** — empty allowlist = no access, MCP env filtering, Wasm SSRF/DNS-rebinding protection, enforced `:user_id` scoping on user tables (UNION/INTERSECT/EXCEPT blocked), 50 MiB module cap, config fail-fast validation with embedder type checking
- **Tool transparency** — see what tools Claude calls; confirmation prompts for sensitive operations

## Quick Start

You need a [Telegram bot token](https://t.me/BotFather) and either a [Claude API key](https://console.anthropic.com/) or a [Claude subscription](https://claude.ai/code). CurlyCatClaw runs via Docker.

### Option 1: Claude Code (recommended)

If you have [Claude Code](https://claude.ai/code) installed, it handles everything: Docker setup, config, credentials, and first run.

```bash
git clone https://github.com/jialuohu/curlycatclaw.git && cd curlycatclaw && claude
```

Then type `/setup`. The skill detects your system, collects credentials, starts Docker Compose, and pulls the embedding model.

### Option 2: Docker (manual)

```bash
git clone https://github.com/jialuohu/curlycatclaw.git && cd curlycatclaw
mkdir -p ~/.curlycatclaw && cp config.toml.example ~/.curlycatclaw/config.toml
# Edit ~/.curlycatclaw/config.toml with your credentials
docker compose up -d
docker compose exec ollama ollama pull bge-m3  # first run only
```

Then message your Telegram bot. Done.

## Configuration

All config lives in `~/.curlycatclaw/config.toml` (mounted as `/data/config.toml` inside Docker). Copy from the example and fill in your credentials. See [`config.toml.example`](config.toml.example) for the full reference.

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

### Google Workspace (optional)

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

### GitHub (optional)

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

For encrypted MCP credentials, set `CURLYCATCLAW_MASTER_KEY` env var (64 hex chars = 32 bytes). MCP servers, Wasm plugins, cron tasks, and other advanced options are documented in `config.toml.example`.

## Architecture

### System Overview

```
┌───────────────────────────────────────────────────────┐
│                     Supervisor                        │
│          (panic/recover, backoff, 30s drain)          │
│                                                       │
│  ┌──────────┐   ┌───────────┐   ┌───────────┐         │
│  │ Channel  │◄─►│  Session  │   │ Reminder  │         │
│  │  Actor   │   │   Actor   │   │   Actor   │         │
│  └────┬─────┘   └─────┬─────┘   └─────┬─────┘         │
│       │               │               │               │
│       │               ├──► Claude     │               │
│       │               │    Direct API (stream+tools)  │
│       │               │    OR CLI subprocess (Max)    │
│       │               │               │               │
│       │               ├──► Tools      │               │
│       │               │    Skills / MCP / Wasm / Ext  │
│       │               │               │               │
│       │               └──► Memory ◄───┘               │
│       │                    SQLite / Vector             │
│       │                                               │
│       │◄── [tool] lines + [confirm?] previews         │
│       │                                               │
└───────┼───────────────────────────────────────────────┘
        │                  │
   Telegram            Landlock
   Bot API          (Linux sandbox)
```

Everything runs as goroutine-based actors under supervision. If an actor panics, it restarts with exponential backoff (1s → 30s), resetting after 60s healthy. On shutdown, actors get 30 seconds to drain before forced exit.

### Streaming Pipeline

```
Telegram ──► Channel Actor ──► Session Actor ──► Claude API (streaming)
               (long-poll,       (context,           │
                photos)           tools)             │ content deltas
                                                     ▼
                                              onDelta() ── 500ms debounce
                                                     │
                                              flush() ── releases mutex during
                                                     │   Telegram I/O (flushing flag)
                                                     │
              Telegram ◄── send/edit ◄───────────────┘
                                                     │
                                              Tool calls? ─── No ──► done
                                                     │
                                                    Yes
                                                     │
                                              Execute tools, reset stream
                                              state, loop (max 10 rounds)
```

Each tool round produces a distinct Telegram message. Text edits respect Telegram's 4096-char limit -- long responses split automatically. The `flushing` state flag prevents lock contention during Telegram I/O.

### Memory System

Three-tier hierarchical memory:

```
Context Assembly (per request)
┌──────────────────────────────────────────────────────────┐
│  Tier 1 (always)    │ User Facts (SQLite)                │  system prompt
│  Tier 2 (semantic)  │ Relevant Summaries (Qdrant)        │  cosine similarity
│  Tier 3 (window)    │ Recent Messages (SQLite)           │  25 turns, ~150K tokens
└──────────────────────────────────────────────────────────┘

Conversation Archival (>4h idle, both API and CLI modes):
  Expired conv ──► Load messages ──► Format (head+tail 12K) ──► Claude summarize
                                                                       │
                                                  SQLite (text) ◄──────┤
                                                  Qdrant (embed+type) ◄┘
  Crash recovery: retries pending/failed/indexed_failed on startup
  Chat-type-aware: DM summaries cross-chat, group summaries stay scoped
```

### Tool Execution

Four tool sources unified under one routing layer:

```
Claude tool_use ──► skills.Registry.Get(name)
                     ├─ Found ──► Built-in Skill (with UserInfo ctx)
                     └─ Not found ──► MCP Manager (server__tool namespace)

┌──────────────────┬───────────────────┬──────────────────────┐
│  Built-in Skills │  MCP Servers      │  Wasm Plugins        │
├──────────────────┼───────────────────┼──────────────────────┤
│  web_search      │  Config servers:  │  Capability-gated:   │
│  save_note       │  server__tool     │  ├ http (SSRF block) │
│  set_reminder    │                   │  ├ db_read (enforced │
│  remember_fact   │  Runtime exts:    │  │  :user_id scoping,│
│  semantic_search │  ext__tool (proxy)│  │  UNION blocked)   │
│  list_summaries  │                   │                      │
│  delete_summary  │  Hot-reload: tools│  └ send_message      │
│  set_extension_* │  added/removed at │                      │
│                  │  runtime via MCP  │ Hot-reload (fsnotify)│
│  Deps: FactStore │  notifications    │                      │
│  DB, VectorStore │                   │                      │
└──────────────────┴───────────────────┴──────────────────────┘
                        │
                        ▼
               Tool result → Claude (next loop round)
```

In CLI subprocess mode, runtime MCP extensions are proxied through the curlycatclaw-skills MCP server. When you add/remove extensions, tools are registered dynamically via `Server.AddTool()`/`Server.RemoveTools()` without restarting the subprocess. This preserves your conversation context. For plugin installs (which do require a restart), recent conversation history is injected into the new subprocess's system prompt from SQLite.

### Vector Search

Pluggable embeddings with three Qdrant collections:

```
Embedder Interface: Embed(text) → vector
  ├─ FNV (384d, offline, no deps)
  ├─ Ollama (768d, local, nomic-embed-text)
  └─ Voyage AI (512d, API, voyage-3-lite)

Qdrant (gRPC, cosine similarity, user_id tenant isolation):
  ├─ curlycatclaw_messages   ◄── user messages
  ├─ curlycatclaw_notes      ◄── saved notes
  └─ curlycatclaw_summaries  ◄── archived conversations

query → Embed(query) → Qdrant.Search(vector, user_id filter) → ranked results
```

## Built-in Skills

| Skill | Description |
|-------|-------------|
| `web_search` | Search the web via DuckDuckGo |
| `save_note` | Save a note (user-scoped, persisted to SQLite) |
| `search_notes` | Search saved notes by keyword |
| `set_reminder` | Set a reminder with time, optional recurrence, and optional Claude-powered prompt |
| `list_reminders` | List pending/fired reminders |
| `cancel_reminder` | Cancel a scheduled reminder |
| `semantic_search` | Search conversation history and notes by meaning (requires Qdrant) |
| `remember_fact` | Save a persistent fact about you across all conversations |
| `forget_fact` | Remove a saved fact by ID |
| `list_facts` | List all persistent facts Claude remembers about you |
| `list_summaries` | View all stored conversation summaries with IDs and previews |
| `delete_summary` | Remove an incorrect or unwanted conversation summary by ID |
| `install_plugin` | Install a Claude Code plugin (auto-searches for marketplace) |
| `uninstall_plugin` | Uninstall a Claude Code plugin |
| `list_plugins` | List installed Claude Code plugins |
| `enable_plugin` | Enable a previously disabled plugin |
| `disable_plugin` | Disable a plugin without uninstalling |
| `update_plugin` | Update one or all installed plugins to latest version |
| `add_marketplace` | Add a third-party plugin marketplace (GitHub repo) |
| `remove_marketplace` | Remove a marketplace and auto-uninstall its plugins |
| `list_marketplaces` | List configured plugin marketplaces |
| `add_extension` | Add a runtime MCP server, exec skill, or prompt skill |
| `remove_extension` | Remove a runtime extension by name |
| `list_extensions` | List all runtime-added extensions |
| `load_prompt_skill` | Load a prompt skill's SKILL.md instructions on demand |
| `set_extension_env` | Set an encrypted env var (API key) for an MCP extension |
| `unset_extension_env` | Remove an encrypted env var from an MCP extension |

Skills are registered alongside MCP tools — Claude sees them all and picks the right one. Plugin skills require `cli_path` and `isolated_home` in `[claude]` config. Extensions are persisted to `extensions.json` and survive restarts. Wasm plugins load from `~/.curlycatclaw/skills/*.wasm` when enabled.

## Testing

```bash
go test ./... -count=1
```

Before pushing, also run lint:

```bash
golangci-lint run
```

## License

[MIT](LICENSE)
