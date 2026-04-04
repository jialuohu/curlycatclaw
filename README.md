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

- **Telegram-native** -- message your bot like a friend, with HTML formatting, typing indicators, and inline tool previews
- **Claude-powered** -- streaming responses with tool use, direct API or CLI subprocess mode (Claude subscription)
- **Real-time streaming** -- text deltas streamed via message edits with 500ms debounce, final render as Telegram HTML
- **Media support** -- send photos, documents, voice messages, and audio files; Claude sees images via vision
- **Project work** -- `/project <name>` to plan, implement, and review code in a repo via Telegram

### Memory & Context

- **Conversation memory** -- SQLite WAL mode, sliding window (25 turns, ~150K tokens), history injected on subprocess restart
- **Hierarchical memory** -- user facts, conversation summaries via Qdrant, current sliding window
- **Vector search** -- Qdrant with pluggable embeddings (Ollama, FNV, Voyage AI), zero-downtime background migration

### Extensibility

- **Google Workspace** -- Gmail, Calendar, Drive, Sheets, Docs, Tasks via standalone MCP server
- **GitHub** -- CI status, PRs, issues, releases via GitHub's official MCP server
- **MCP tool integration** -- connect any MCP server via stdio, add/remove at runtime, hot-reloaded without restart
- **Runtime extensions** -- add MCP servers, exec skills, and prompt skills through Telegram chat, persisted to disk
- **Encrypted credentials** -- API keys for MCP extensions encrypted at rest with AES-256-GCM
- **Built-in skills** -- web search, notes, reminders, semantic search, user facts, plugin/extension management
- **Wasm plugins** -- custom skills via WebAssembly with capability-based security and hot-reload
- **External skill collections** -- exec-based skills from directory trees with sandboxed env and fsnotify hot-reload
- **Plugin management** -- install/manage Claude Code plugins through Telegram with standard plugins pre-installed

### Operations

- **Health endpoint** -- `GET /health` for Docker/monitoring liveness checks
- **Supervision** -- automatic restart with exponential backoff, graceful 30s drain on shutdown
- **Cron tasks** -- scheduled prompts through Claude with full tool access and ephemeral context
- **Configurable logging** -- level, format, file output with lumberjack rotation
- **Docker ready** -- docker-compose with Qdrant + Ollama, one command to run

### Security

- **Landlock sandbox** -- Linux filesystem restriction (opt-in)
- **Encrypted credentials** -- AES-256-GCM for MCP server secrets
- **Secure defaults** -- MCP env filtering, Wasm SSRF/DNS-rebinding protection, enforced user scoping, config validation
- **Tool transparency** -- see what tools Claude calls; confirmation prompts for sensitive operations

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

## Architecture

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

Everything runs as goroutine-based actors under supervision. The Channel Actor handles Telegram I/O, the Session Actor orchestrates Claude conversations and tool execution, and the Reminder Actor manages scheduled tasks. See [docs/architecture.md](docs/architecture.md) for the full streaming pipeline, memory system, tool execution, and vector search diagrams.

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](docs/architecture.md) | System overview, streaming pipeline, memory, tool execution, vector search |
| [Configuration](docs/configuration.md) | Config reference, Google Workspace, GitHub, encrypted credentials |
| [Built-in Skills](docs/skills.md) | All 24 skills and the 5 skill types |
| [Docker Deployment](docs/docker.md) | Services, data layout, backups, plugin runtimes |

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
