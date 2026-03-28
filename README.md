<p align="center">
  <img src="assets/logo.png" alt="CurlyCatClaw" width="400" />
</p>

<h1 align="center">CurlyCatClaw</h1>

<p align="center">
  A personal AI assistant that lives in your Telegram. Built in Go.
</p>

---

CurlyCatClaw is a long-running daemon that connects Claude to Telegram. You message your bot, it thinks with Claude, calls tools via MCP, and replies. SQLite keeps your conversation history. That's it.

## Features

- **Telegram-native** ... message your bot like you'd message a friend
- **Claude-powered** ... streaming responses with tool use, 120s timeout per request
- **Streaming to Telegram** ... text streams in real-time via message edits, debounced at 500ms, new messages per tool-use round
- **Image support** ... send photos to your bot, Claude sees them via vision (photos downloaded and sent as base64 image blocks)
- **Conversation memory** ... SQLite with WAL mode, sliding window context (25 turns, ~150K tokens)
- **MCP tool integration** ... connect any MCP server (search, filesystem, APIs) via stdio
- **Built-in skills** ... web search, save/search notes, reminders, semantic search, persistent user facts
- **Hierarchical memory** ... three-tier: user facts always in system prompt, conversation summaries relevance-retrieved via Qdrant, sliding window for current conversation (opt-in)
- **Smart context** ... prompt budget manager classifies turn relevance via Haiku (opt-in)
- **Vector search** ... semantic memory via Qdrant with pluggable embeddings: FNV (offline), Ollama (free local), Voyage AI (paid) (opt-in)
- **Reminders** ... "remind me at 3pm" with persistent scheduler, timezone-aware, recurring
- **Wasm plugins** ... extend with custom skills via WebAssembly, capability-based security (opt-in)
- **Actor model** ... each component runs in its own goroutine with typed message channels
- **Supervision** ... automatic restart with exponential backoff, graceful shutdown with 30s drain timeout
- **Configurable logging** ... level, format (text/json), file output with rotation via lumberjack
- **Landlock sandbox** ... Linux filesystem restriction with BestEffort degradation (opt-in)
- **Tool transparency** ... see what tools Claude calls before seeing the response
- **Secure defaults** ... Telegram bot fails closed on empty user allowlist, MCP env filtering, Wasm private IP blocklist
- **Encrypted credentials** ... AES-256-GCM for MCP server secrets
- **Docker ready** ... Dockerfile + docker-compose with Qdrant, one command to run
- **Goreleaser** ... automated multi-platform binary releases with checksums and Docker images on ghcr.io

## Quick Start

**Prerequisites:** Go 1.25+, a [Telegram bot token](https://t.me/BotFather), and a [Claude API key](https://console.anthropic.com/).

```bash
# Clone and build
git clone https://github.com/jialuohu/curlycatclaw.git
cd curlycatclaw
go build -o curlycatclaw ./cmd/curlycatclaw

# Set up config
mkdir -p ~/.curlycatclaw
cp config.toml.example ~/.curlycatclaw/config.toml
# Edit ~/.curlycatclaw/config.toml with your API keys

# Run
./curlycatclaw
```

Then message your Telegram bot. Done.

## Configuration

All config lives in `~/.curlycatclaw/config.toml`. Copy from the example:

```toml
timezone = "America/Los_Angeles"

[claude]
api_key = "sk-ant-..."
model   = "claude-sonnet-4-6-20250514"

[telegram]
token = "123456:ABC-DEF..."
allowed_user_ids = [123456789]  # your Telegram user ID (required unless allow_all = true)

[storage]
db_path = "/home/you/.curlycatclaw/curlycatclaw.db"

# Optional: add MCP servers for extra tools
[[mcp.servers]]
name    = "search"
command = "npx"
args    = ["-y", "@anthropic/mcp-server-brave-search"]
[mcp.servers.env]
BRAVE_API_KEY = "encrypted:ref:brave_api_key"
```

For encrypted MCP credentials, set the `CURLYCATCLAW_MASTER_KEY` env var (64 hex chars = 32 bytes).

## Architecture

```
┌──────────────────────────────────────────────────────┐
│                      Supervisor                      │
│            (panic/recover, backoff, 30s drain)       │
│                                                      │
│   ┌─────────┐   ┌───────────┐   ┌───────────┐        │
│   │ Channel │◄─►│  Session  │   │ Reminder  │        │
│   │  Actor  │   │   Actor   │   │   Actor   │        │
│   └────┬────┘   └─────┬─────┘   └─────┬─────┘        │
│        │              │               │              │
│        │              ├──► Claude API │              │
│        │              │    stream + tool_use         │
│        │              │               │              │
│        │              ├──► Tools      │              │
│        │              │    Skills     │              │
│        │              │    MCP        │              │
│        │              │    Wasm       │              │
│        │              │               │              │
│        │              └──► Memory ◄───┘              │
│        │                   SQLite / Budget / Vector  │
│        │                                             │
│        │◄── [tool] lines + [confirm?] previews       │
│        │                                             │
└────────┼─────────────────────────────────────────────┘
         │                  │
    Telegram            Landlock
    Bot API          (Linux sandbox)
```

Everything runs as goroutine-based actors under supervision. If an actor panics or errors, it restarts with exponential backoff (1s to 30s), resetting after 60s of healthy operation. On shutdown, actors get 30 seconds to drain in-flight work before forced exit.

| Component | File | What it does |
|-----------|------|-------------|
| Entrypoint | `cmd/curlycatclaw/main.go` | Config loading, actor bootstrap, signal handling |
| Session | `internal/session/actor.go` | Wires Telegram, Claude, MCP, memory, and skills |
| Interfaces | `internal/session/deps.go` | Testability interfaces for session dependencies |
| Claude client | `internal/claude/client.go` | Streaming API client with tool_use state machine |
| Telegram | `internal/telegram/channel.go` | Long-polling channel actor |
| Memory | `internal/memory/store.go` | SQLite storage, conversation management |
| Context | `internal/memory/context.go` | Sliding window context builder |
| MCP | `internal/mcp/manager.go` | MCP server lifecycle, tool namespacing |
| Skills | `skills/` | Built-in skill implementations |
| Facts | `internal/memory/facts.go` | Persistent user facts (CRUD, sanitization, IDOR protection) |
| Summarizer | `internal/memory/summarizer.go` | Conversation summarization via Claude |
| Budget | `internal/memory/budget.go` | Prompt budget manager (Haiku classification) |
| Embedder | `internal/memory/embedder.go` | Pluggable embedding providers (FNV, Ollama, Voyage AI) |
| Vector | `internal/memory/vectorstore.go` | Qdrant vector search |
| Wasm | `internal/wasm/runtime.go` | Wasm skill runtime (wazero) |
| Credentials | `internal/security/credential.go` | AES-256-GCM encrypted credential store |
| Sandbox | `internal/security/sandbox_linux.go` | Landlock filesystem sandbox (Linux) |
| Supervisor | `internal/actor/supervisor.go` | Panic recovery, graceful shutdown drain |
| systemd | `deploy/curlycatclaw.service` | Service unit with hardening directives |

## Built-in Skills

| Skill | Description |
|-------|-------------|
| `web_search` | Search the web via DuckDuckGo |
| `save_note` | Save a note (user-scoped, persisted to SQLite) |
| `search_notes` | Search saved notes by keyword |
| `set_reminder` | Set a reminder with time and optional recurrence |
| `list_reminders` | List your pending/fired reminders |
| `cancel_reminder` | Cancel a scheduled reminder |
| `semantic_search` | Search conversation history and notes by meaning (requires Qdrant) |
| `remember_fact` | Save a persistent fact about you, remembered across all conversations (requires `[memory]`) |
| `forget_fact` | Remove a previously saved fact by ID |
| `list_facts` | List all persistent facts Claude remembers about you |

Skills are registered alongside MCP tools. Claude sees them all as available tools and picks the right one. Wasm plugins are loaded from `~/.curlycatclaw/skills/*.wasm` when enabled.

## Deployment

### Docker (recommended)

```bash
# Copy and edit config
cp config.toml.example config.toml
# Set db_path = "/data/curlycatclaw.db" and qdrant_addr = "qdrant:6334"

# Start curlycatclaw + Qdrant
docker compose up -d
docker compose cp config.toml curlycatclaw:/data/config.toml
docker compose restart curlycatclaw
```

See [deploy/docker.md](deploy/docker.md) for details and MCP limitations.

### systemd (Linux)

1. Create a system user:

   ```bash
   sudo useradd --system --create-home --home-dir /var/lib/curlycatclaw curlycatclaw
   ```

2. Copy the binary and config:

   ```bash
   sudo cp curlycatclaw /usr/local/bin/
   sudo mkdir -p /etc/curlycatclaw
   sudo cp config.toml /etc/curlycatclaw/config.toml
   sudo chown -R curlycatclaw:curlycatclaw /etc/curlycatclaw
   ```

3. Install and enable the service:

   ```bash
   sudo cp deploy/curlycatclaw.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now curlycatclaw
   ```

4. View logs:

   ```bash
   journalctl -u curlycatclaw -f
   ```

See [deploy/UPGRADE.md](deploy/UPGRADE.md) for upgrade instructions.

## Testing

```bash
go test ./... -count=1 -race
```

Tests cover all subsystems across 11 packages with race detection.

## License

MIT
