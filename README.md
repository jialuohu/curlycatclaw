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

- 💬 **Telegram-native** -- text, photos, documents, voice, audio, typing indicators, streaming responses with tool previews, smart message splitting that preserves code blocks

- 🧠 **Smart memory** -- four-tier context (user facts, auto-extracted observations with entity tracking and self-healing supersession, conversation summaries via Qdrant, sliding window), FTS5 hybrid search, progressive retrieval, pluggable embeddings, voice messages transcribed via OpenAI Whisper

- 📧 **Knowledge ingest** -- background ingestion from Gmail, Obsidian vaults, and Notion with per-source cursors, daily caps, and trust levels

- 🔌 **Extensible** -- Google Workspace (multi-account with per-account service filtering), GitHub, any MCP server, Wasm plugins, exec skills, Claude Code plugins, file delivery, all manageable from chat

- 🧪 **Thinking effort control** -- configure Claude's reasoning depth (`/effort low|medium|high|xhigh|max`, where `xhigh` is Opus 4.7's mid-high level), replay messages at higher effort (`/retry`), stop in-flight work with `/stop`, extended thinking with budget presets, per-session override via Telegram

- ⏰ **Cron tasks** -- scheduled prompts through Claude with full tool access, per-reminder model + thinking-effort selection

- 🌍 **Runtime timezone** -- agent flips the daemon's effective timezone from chat (`set_timezone Asia/Tokyo`); pending recurring reminders reschedule in the new TZ within ~10s, no container restart

- 🔒 **Secure** -- AES-256-GCM encrypted credentials, SSRF protection, user scoping, tool confirmation, Docker isolation

- 🔄 **Self-updating** -- `/update` in Telegram to pull the latest image and restart, `/rollback` to revert, optional auto-update on a cron schedule, all via a lightweight updater sidecar

- 🐳 **Docker ready** -- one command to run with Qdrant + Ollama, health endpoint, supervised actors with auto-restart

## Quick Start

You need a [Telegram bot token](https://t.me/BotFather) and either a [Claude API key](https://console.anthropic.com/) or a [Claude subscription](https://claude.ai/code).

**With Claude Code** (recommended):

```bash
git clone https://github.com/jialuohu/curlycatclaw.git && cd curlycatclaw && claude
```

Type `/setup`. It handles Docker, config, credentials, and first run.

**Without Claude Code:**

```bash
git clone https://github.com/jialuohu/curlycatclaw.git && cd curlycatclaw
mkdir -p ~/.curlycatclaw && cp config.toml.example ~/.curlycatclaw/config.toml
# Edit config.toml with your Telegram token and Claude credentials
docker compose up -d
```

Message your bot. Done. See [Docker Deployment](docs/docker.md) for dev builds, optional services, and profiles.

## Architecture

```
┌────────────────────────────────────────────────────────────────┐
│                       Supervisor                               │
│             (panic/recover, backoff, 30s drain)                │
│                                                                │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────┐  ┌──────┐│
│  │ Channel  │◄►│ Session  │  │ Reminder │  │ Ingest │  │ Eval ││
│  │  Actor   │  │  Actor   │  │  Actor   │  │ Actor  │  │ Actor││
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └───┬────┘  └──┬───┘│
│       │             │             │            │          │    │
│       │             ├──► Claude   │       Gmail│    gocron│    │
│       │             │    Direct API (stream+tools) Obsidian    │
│       │             │    OR CLI subprocess  Notion via MCP     │
│       │             │    + /effort /retry /stop /debug /update │
│       │             │             │                   ▼        │
│       │             ├──► MCP Manager          Observations     │
│       │             │    ├─ Config servers (gws, github)       │
│       │             │    ├─ Runtime extensions (proxy)         │
│       │             │    └─ Skills (built-in + Wasm)           │
│       │             │             │                            │
│       │             └──► Memory ◄─┘                            │
│       │                  SQLite / Qdrant / Ollama              │
│       │                                                        │
│       │◄── [tool] lines (/debug toggles visibility)            │
│       │                                                        │
└───────┼────────────────────────────────────────────────────────┘
        │
   Telegram
   Bot API
```

Everything runs as goroutine-based actors under supervision. The Channel Actor handles Telegram I/O, the Session Actor orchestrates Claude conversations and tool execution, the Reminder Actor manages scheduled tasks, and the Ingest Actor ingests knowledge from configured sources (Gmail, Obsidian, Notion) and extracts observations into the memory system. See [docs/architecture.md](docs/architecture.md) for the full streaming pipeline, memory system, tool execution, and vector search diagrams.

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](docs/architecture.md) | System overview, streaming pipeline, memory, tool execution, vector search |
| [Configuration](docs/configuration.md) | Config reference, Google Workspace, GitHub, encrypted credentials |
| [Google Workspace Skills](docs/skills.md) | Google Workspace skills catalog |
| [Docker Deployment](docs/docker.md) | Services, data layout, backups, plugin runtimes |
| [Contributing](CONTRIBUTING.md) | Bug reports, feature requests, code contributions |

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
