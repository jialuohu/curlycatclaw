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
- **Supervision**: panic/recover with exponential backoff, resets after 60s healthy run, WaitGroup drain with 30s timeout on shutdown, configurable via `SupervisorConfig` (initial/max backoff, healthy period), indexing semaphore (10 slots) bounds concurrent vector/summarization goroutines
- **Health endpoint**: `GET /health` on 127.0.0.1, enabled by default via `[health]` config, returns 200/503 based on context cancellation, used by Docker healthcheck
- **Claude client**: two modes — (1) direct API via Go SDK (streaming + tool_use state machine, 120s timeout, `OnPartialText` callback) or (2) CLI subprocess mode via `CLIManager` (long-lived `claude` process per user, stream-json protocol, enables Claude Max subscription)
- **CLI subprocess**: spawns `claude --print --input-format stream-json --output-format stream-json` per user; auth via `CLAUDE_CODE_OAUTH_TOKEN` env var (long-lived token from `claude setup-token`, configured in `oauth_token` config field); CLI handles token exchange, LLM calls, and tool execution via MCP; curlycatclaw parses events for Telegram streaming and SQLite logging; persistent scan goroutine delivers events via channel for proper context cancellation; deferred cleanup in spawn() prevents zombie processes
- **Streaming responses**: text deltas streamed to Telegram via message edits (500ms debounce, strings.Builder accumulation), tool_use transitions start new messages, error mid-stream appends notice, pre-stream errors send visible feedback, msgID -1 sentinel handled at all sites, `flushing` state flag releases mutex during Telegram I/O to prevent lock contention
- **Image support**: Telegram photos downloaded by channel actor, sent to Claude as base64 image blocks, stored as file_id references (not inline)
- **MCP manager**: persistent stdio server connections, tool namespacing (server__tool), allowlist-based env filtering, user context injection
- **Memory**: SQLite WAL mode, sliding window context (25 turns, ~150K tokens), conversations keyed by (userID, chatID), transactional check-and-create for active conversations
- **Hierarchical memory**: three-tier (user facts in system prompt, conversation summaries via Qdrant relevance search, current conversation sliding window), opt-in via `[memory]` config
- **Facts**: persistent per-user facts with category, sanitization (200-rune limit, control char strip, rune-safe UTF-8 truncation), IDOR-protected delete, proactive extraction via system prompt instruction
- **Conversation archival**: async summarization on conversation expiry (>4h idle), crash recovery via `summarization_status` tracking, dedicated `curlycatclaw_summaries` Qdrant collection
- **Budget manager**: Haiku-powered context classification (keyword fast-path + cache + LLM), budget-aware context building via `BuildContextWithBudget`, opt-in, cache indexed on created_at for cleanup, rune-safe UTF-8 truncation (500-rune limit) for LLM classification prompts
- **Vector search**: Qdrant gRPC for semantic search, pluggable Embedder interface (FNV offline / Ollama local / Voyage AI paid), configurable search timeout via `vector_search_timeout_seconds` (default 5s)
- **Config validation**: startup validation of required fields (db_path, MCP server name/command, qdrant_addr when vector enabled, wasm skills_dir when wasm enabled, health port range, budget.model when budget enabled, embedder type fnv/ollama/voyage with required fields per type)
- **Skills**: built-in Go skills (search, note, remind, semantic_search, remember_fact, forget_fact, list_facts) + Wasm plugin runtime; note has title/content size limits (500 runes / 100KB), semantic_search capped at 50 results, remind validates cron expressions at input time
- **Wasm runtime**: wazero-based with capability model, JSON-over-shared-memory, hot-reload (atomic reload with map-based Execute lookup), chat-scoped send_message, db_read enforced user scoping via `:user_id` placeholder (queries on user-scoped tables without `:user_id` are rejected, not just warned; quote-aware replacement, 10 MiB result size cap, rows.Err check), UNION/INTERSECT/EXCEPT/WITH blocked in isSelectOnly, comment-stripped word-boundary table detection, HTTP private IP blocklist (SSRF prevention), connect-time IP verification (DNS rebinding protection), sanitized DB errors, 50 MiB module size cap, compiled module cleanup on unload, skill-name-based registry unregister
- **Tool transparency**: `[tool]` lines sent to user in Telegram, opt-out via `show_tool_calls`
- **Tool confirmation**: `confirm_tools` prefix list for sensitive operations, stateless via Claude re-ask
- **Logging**: configurable level/format/file via `[logging]` config, lumberjack rotation
- **Sandbox**: Landlock filesystem restriction (Linux-only, `//go:build linux`), opt-in via `[sandbox]` config

## Key Files

| File | Purpose |
|------|---------|
| `cmd/curlycatclaw/main.go` | Binary entrypoint, config loading, actor bootstrap, health server |
| `config/config.go` | TOML config struct, defaults, validation |
| `internal/session/actor.go` | Central session actor wiring everything together |
| `internal/session/deps.go` | Testability interfaces (LLMClient, MessageStore, etc.) |
| `internal/claude/client.go` | Claude API streaming + non-streaming client (direct mode) |
| `internal/claude/subprocess.go` | CLI subprocess manager + stream-json parser (CLI mode) |
| `cmd/curlycatclaw/mcp_server.go` | MCP stdio server exposing built-in skills |
| `internal/telegram/channel.go` | Telegram channel actor |
| `internal/memory/store.go` | SQLite storage |
| `internal/mcp/manager.go` | MCP server lifecycle |
| `internal/memory/budget.go` | Prompt budget manager (Haiku classification) |
| `internal/memory/embedder.go` | Embedder interface + FNV/Ollama/Voyage implementations |
| `internal/memory/facts.go` | User facts CRUD (sanitization, IDOR protection) |
| `internal/memory/summarizer.go` | Conversation summarizer (transcript formatting + Claude) |
| `internal/memory/vectorstore.go` | Qdrant vector search (messages, notes, summaries) |
| `internal/wasm/runtime.go` | Wasm skill runtime (wazero) |
| `skills/` | Built-in skill implementations |
| `internal/security/sandbox_linux.go` | Landlock filesystem sandbox (Linux) |
| `deploy/curlycatclaw.service` | systemd unit file with hardening |
| `Dockerfile` | Container build (CGO_ENABLED=0, Alpine) |
| `docker-compose.yml` | curlycatclaw + Qdrant orchestration |
| `.goreleaser.yml` | Release automation (binaries, checksums, Docker images) |

## Configuration

Copy `config.toml.example` to `~/.curlycatclaw/config.toml` and fill in credentials.

Auth modes: `cli_path` + `oauth_token` (Claude Max subscription via CLI subprocess), `auth_token` (OAuth direct API), or `api_key` (direct API). CLI mode uses `oauth_token` from `claude setup-token` injected as `CLAUDE_CODE_OAUTH_TOKEN` env var.

For encrypted MCP credentials, set `CURLYCATCLAW_MASTER_KEY` env var (64 hex chars = 32 bytes).

## gstack

Use the `/browse` skill from gstack for all web browsing. Never use `mcp__claude-in-chrome__*` tools.

Available skills: `/office-hours`, `/plan-ceo-review`, `/plan-eng-review`, `/plan-design-review`, `/design-consultation`, `/design-shotgun`, `/review`, `/ship`, `/land-and-deploy`, `/canary`, `/benchmark`, `/browse`, `/connect-chrome`, `/qa`, `/qa-only`, `/design-review`, `/setup-browser-cookies`, `/setup-deploy`, `/retro`, `/investigate`, `/document-release`, `/codex`, `/cso`, `/autoplan`, `/careful`, `/freeze`, `/guard`, `/unfreeze`, `/gstack-upgrade`.
