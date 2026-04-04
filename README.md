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

💬 **Telegram-native** -- text, photos, documents, voice, audio, typing indicators, streaming responses with tool previews

🧠 **Smart memory** -- three-tier context (user facts, conversation summaries via Qdrant, sliding window), pluggable embeddings, voice messages transcribed via OpenAI Whisper

🔌 **Extensible** -- Google Workspace, GitHub, any MCP server, Wasm plugins, exec skills, Claude Code plugins, all manageable from chat

⏰ **Cron tasks** -- scheduled prompts through Claude with full tool access

🔒 **Secure** -- Landlock sandbox, AES-256-GCM encrypted credentials, SSRF protection, user scoping, tool confirmation

🐳 **Docker ready** -- one command to run with Qdrant + Ollama, health endpoint, supervised actors with auto-restart

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
