# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**curlycatclaw** is a personal AI agent assistant built in Go. It's a long-running daemon with a goroutine-based actor model, Telegram as the primary channel, Claude as the LLM (no multi-model abstraction), SQLite for storage, and MCP for tool integration.

## Build & Run

```bash
go build -o curlycatclaw ./cmd/curlycatclaw
./curlycatclaw --config ~/.curlycatclaw/config.toml
```

## Testing

```bash
go test ./... -count=1
```

Test expectations:
- When writing new functions, write a corresponding test
- When fixing a bug, write a regression test
- When adding error handling, test both success and error paths
- Tests use stdlib `testing` package with `t.Fatal`/`t.Error` assertions

## Architecture

- **Actor model**: each component (Telegram, session, etc.) runs in its own goroutine with typed message channels
- **Supervision**: panic/recover with exponential backoff, resets after 60s healthy run, WaitGroup drain with 30s timeout on shutdown
- **Claude client**: streaming + tool_use state machine, 120s per-request timeout
- **MCP manager**: persistent stdio server connections, tool namespacing (server__tool), allowlist-based env filtering, user context injection
- **Memory**: SQLite WAL mode, sliding window context (25 turns, ~150K tokens), conversations keyed by (userID, chatID)
- **Budget manager**: Haiku-powered context classification (keyword fast-path + cache + LLM), budget-aware context building via `BuildContextWithBudget`, opt-in
- **Vector search**: Qdrant gRPC for semantic search, pluggable Embedder interface (FNV offline / Ollama local / Voyage AI paid)
- **Skills**: built-in Go skills (search, note, remind, semantic_search) + Wasm plugin runtime
- **Wasm runtime**: wazero-based with capability model, JSON-over-shared-memory, hot-reload, chat-scoped send_message
- **Tool transparency**: `[tool]` lines sent to user in Telegram, opt-out via `show_tool_calls`
- **Tool confirmation**: `confirm_tools` prefix list for sensitive operations, stateless via Claude re-ask
- **Logging**: configurable level/format/file via `[logging]` config, lumberjack rotation
- **Sandbox**: Landlock filesystem restriction (Linux-only, `//go:build linux`), opt-in via `[sandbox]` config

## Key Files

| File | Purpose |
|------|---------|
| `cmd/curlycatclaw/main.go` | Binary entrypoint, config loading, actor bootstrap |
| `internal/session/actor.go` | Central session actor wiring everything together |
| `internal/session/deps.go` | Testability interfaces (LLMClient, MessageStore, etc.) |
| `internal/claude/client.go` | Claude API streaming client |
| `internal/telegram/channel.go` | Telegram channel actor |
| `internal/memory/store.go` | SQLite storage |
| `internal/mcp/manager.go` | MCP server lifecycle |
| `internal/memory/budget.go` | Prompt budget manager (Haiku classification) |
| `internal/memory/embedder.go` | Embedder interface + FNV/Ollama/Voyage implementations |
| `internal/memory/vectorstore.go` | Qdrant vector search |
| `internal/wasm/runtime.go` | Wasm skill runtime (wazero) |
| `skills/` | Built-in skill implementations |
| `internal/security/sandbox_linux.go` | Landlock filesystem sandbox (Linux) |
| `deploy/curlycatclaw.service` | systemd unit file with hardening |
| `Dockerfile` | Container build (CGO_ENABLED=0, Alpine) |
| `docker-compose.yml` | curlycatclaw + Qdrant orchestration |
| `.goreleaser.yml` | Release automation (binaries, checksums, Docker images) |

## Configuration

Copy `config.toml.example` to `~/.curlycatclaw/config.toml` and fill in API keys.

For encrypted MCP credentials, set `CURLYCATCLAW_MASTER_KEY` env var (64 hex chars = 32 bytes).
