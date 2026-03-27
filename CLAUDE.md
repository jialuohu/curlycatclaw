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
- **Supervision**: panic/recover with exponential backoff, resets after 60s healthy run
- **Claude client**: streaming + tool_use state machine, 120s per-request timeout
- **MCP manager**: persistent stdio server connections, tool namespacing (server__tool)
- **Memory**: SQLite WAL mode, sliding window context (25 turns, ~150K tokens), conversations keyed by (userID, chatID)
- **Skills**: built-in Go skills (search, note) registered alongside MCP tools

## Key Files

| File | Purpose |
|------|---------|
| `cmd/curlycatclaw/main.go` | Binary entrypoint, config loading, actor bootstrap |
| `internal/session/actor.go` | Central session actor wiring everything together |
| `internal/claude/client.go` | Claude API streaming client |
| `internal/telegram/channel.go` | Telegram channel actor |
| `internal/memory/store.go` | SQLite storage |
| `internal/mcp/manager.go` | MCP server lifecycle |
| `skills/` | Built-in skill implementations |

## Configuration

Copy `config.toml.example` to `~/.curlycatclaw/config.toml` and fill in API keys.

For encrypted MCP credentials, set `CURLYCATCLAW_MASTER_KEY` env var (64 hex chars = 32 bytes).
