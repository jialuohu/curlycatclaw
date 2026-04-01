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

Before pushing, always run the full local CI checks to match what GitHub Actions runs:
```bash
golangci-lint run        # must show 0 issues
go test ./... -count=1   # must all pass
```

If `golangci-lint` is not installed, at minimum run `go vet ./...`. But CI uses `golangci-lint v2` with errcheck, staticcheck, and unused enabled, so `go vet` alone is not sufficient. Install it: `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`.

Test expectations:
- When writing new functions, write a corresponding test
- When fixing a bug, write a regression test
- When adding error handling, test both success and error paths
- Tests use stdlib `testing` package with `t.Fatal`/`t.Error` assertions
- Always run `golangci-lint run` and `go test` locally before committing to avoid CI failures
- Never merge a PR with failing CI. Fix lint issues first.

## Versioning

Version is tracked in the `VERSION` file (currently source of truth). Follows semver (`x.y.z`):
- **x (major)**: Reserved — only bumped when explicitly decided by the maintainer
- **y (minor)**: New features
- **z (patch)**: Bug fixes and patches

Goreleaser injects the version into the binary via `-X main.version={{.Version}}` from git tags. Update `VERSION` and `CHANGELOG.md` when releasing.

## Architecture

- **Actor model**: each component (Telegram, session, etc.) runs in its own goroutine with typed message channels
- **Supervision**: panic/recover with exponential backoff, resets after 60s healthy run, WaitGroup drain with 30s timeout on shutdown, configurable via `SupervisorConfig` (initial/max backoff, healthy period), indexing semaphore (10 slots) bounds concurrent vector indexing, dedicated summarization semaphore (2 slots) prevents summarization starvation under burst load
- **Health endpoint**: `GET /health` on 127.0.0.1, enabled by default via `[health]` config, returns 200/503 based on context cancellation, used by Docker healthcheck
- **Claude client**: two modes — (1) direct API via Go SDK (streaming + tool_use state machine, 120s timeout, `OnPartialText` callback) or (2) CLI subprocess mode via `CLIManager` (long-lived `claude` process per user, stream-json protocol, enables Claude subscription)
- **CLI subprocess**: spawns `claude --print --input-format stream-json --output-format stream-json` per user; auth via `CLAUDE_CODE_OAUTH_TOKEN` env var (long-lived token from `claude setup-token`, configured in `oauth_token` config field); CLI handles token exchange, LLM calls, and tool execution via MCP; curlycatclaw parses events for Telegram streaming and SQLite logging; persistent scan goroutine delivers events via channel for proper context cancellation; deferred cleanup in spawn() prevents zombie processes; optional `WorkDir` for project-scoped work, optional `HomeDir` for isolated Claude home (clean plugin environment)
- **Streaming responses**: text deltas streamed to Telegram via message edits (500ms debounce, strings.Builder accumulation), tool_use transitions start new messages, error mid-stream appends notice, pre-stream errors send visible feedback, msgID -1 sentinel handled at all sites, `flushing` state flag releases mutex during Telegram I/O to prevent lock contention, final flush converts markdown to Telegram HTML via `internal/mdhtml` (falls back to plain text on parse error)
- **Image support**: Telegram photos downloaded by channel actor, sent to Claude as base64 image blocks, stored as file_id references (not inline)
- **MCP manager**: persistent stdio server connections, tool namespacing (server__tool), allowlist-based env filtering, user context injection
- **Memory**: SQLite WAL mode, sliding window context (25 turns, ~150K tokens), conversations keyed by (userID, chatID), transactional check-and-create for active conversations
- **Hierarchical memory**: three-tier (user facts in system prompt, conversation summaries via Qdrant relevance search, current conversation sliding window), opt-in via `[memory]` config
- **Facts**: persistent per-user facts with category, sanitization (200-rune limit, control char strip, rune-safe UTF-8 truncation), IDOR-protected delete, proactive extraction via system prompt instruction
- **Conversation archival**: async summarization on conversation expiry (>4h idle) in both direct API and CLI modes (CLI uses `SpawnOneShot`), crash recovery retries `pending`/`failed`/`indexed_failed` conversations on startup (sequential, capped at 20, oldest first), head+tail transcript sampling (first 5000 + last 5000 runes), chat-type-aware retrieval (DM summaries user-scoped, group summaries chat-scoped), `IndexSummary` stores `chat_type` metadata in Qdrant, dedicated `curlycatclaw_summaries` Qdrant collection
- **Budget manager**: Haiku-powered context classification (keyword fast-path + cache + LLM), budget-aware context building via `BuildContextWithBudget`, opt-in, cache indexed on created_at for cleanup, rune-safe UTF-8 truncation (500-rune limit) for LLM classification prompts
- **Vector search**: Qdrant gRPC for semantic search, pluggable Embedder interface (FNV offline / Ollama local / Voyage AI paid), configurable search timeout via `vector_search_timeout_seconds` (default 5s), `BatchEmbed` for efficient bulk operations, `migrate-embedder` CLI command for wipe-and-rebuild migration when switching providers
- **Config validation**: startup validation of required fields (db_path, MCP server name/command, qdrant_addr when vector enabled, wasm skills_dir when wasm enabled, health port range, budget.model when budget enabled, embedder type fnv/ollama/voyage with required fields per type)
- **Skills**: built-in Go skills (search, note, remind, semantic_search, remember_fact, forget_fact, list_facts, list_summaries, delete_summary, install_plugin, uninstall_plugin, list_plugins, enable_plugin, disable_plugin, update_plugin, add_marketplace, remove_marketplace, list_marketplaces) + Wasm plugin runtime + external skill collections; note has title/content size limits (500 runes / 100KB), semantic_search capped at 50 results, remind validates cron expressions at input time, optional `prompt` field for Claude-powered cron tasks (clean context, facts only, full tool access); marketplace auto-bootstrap on first install, lazy auto-update for stale plugins (>7 days), autonomous web search for missing marketplaces on install failure
- **External skill collections**: `internal/skillloader` loads exec-based skills from directory trees via `[[skill_collections]]` config; `collection.toml` + per-skill `skill.toml` descriptors; exec adapter spawns per invocation with minimal env (PATH/HOME/TMPDIR only, no daemon secret leakage); fsnotify hot-reload; path traversal prevention; LD_PRELOAD/DYLD_* env blocklist
- **Project work**: `/project <name>` Telegram command switches CLI subprocess working directory; `[[projects]]` config declares allowed project paths; isolated Claude home (`isolated_home` config) gives subprocess a clean `~/.claude/` with no inherited plugins; plugin management skills validated against `allowed_plugins` config allowlist; file-based reload signal (`~/.curlycatclaw/claude-home/.curlycatclaw-reload-needed`) triggers subprocess respawn after plugin changes
- **Cron tasks**: `CronExecutor` runs scheduled prompts through Claude with ephemeral context (no conversation DB), user facts in system prompt, full skill/MCP tool access, 3-slot concurrency semaphore, rate limit retry, 5-minute timeout; CLI mode uses `SpawnOneShot` for isolated subprocess; `CronRunner` interface in skills package avoids circular imports
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
| `internal/session/cron.go` | CronExecutor for scheduled Claude-powered tasks |
| `skills/` | Built-in skill implementations |
| `skills/summary.go` | Summary management skills (list_summaries, delete_summary) |
| `skills/plugin.go` | Plugin management skills (install, uninstall, enable, disable, list) |
| `internal/mdhtml/convert.go` | Markdown to Telegram HTML converter |
| `internal/skillloader/loader.go` | External skill collection loader (exec adapter) |
| `cmd/curlycatclaw/migrate.go` | Embedder migration tool (wipe-and-rebuild) |
| `internal/security/sandbox_linux.go` | Landlock filesystem sandbox (Linux) |
| `Dockerfile` | Container build (CGO_ENABLED=0, Debian bookworm-slim) |
| `docker-compose.yml` | curlycatclaw + Qdrant orchestration |
| `.goreleaser.yml` | Release automation (binaries, checksums, Docker images) |

## Configuration

Copy `config.toml.example` to `~/.curlycatclaw/config.toml` and fill in credentials.

Auth modes: `cli_path` + `oauth_token` (Claude subscription via CLI subprocess) or `api_key` (direct API). CLI mode uses `oauth_token` from `claude setup-token` injected as `CLAUDE_CODE_OAUTH_TOKEN` env var.

For encrypted MCP credentials, set `CURLYCATCLAW_MASTER_KEY` env var (64 hex chars = 32 bytes).

## gstack

Use the `/browse` skill from gstack for all web browsing. Never use `mcp__claude-in-chrome__*` tools.

Available skills: `/office-hours`, `/plan-ceo-review`, `/plan-eng-review`, `/plan-design-review`, `/design-consultation`, `/design-shotgun`, `/review`, `/ship`, `/land-and-deploy`, `/canary`, `/benchmark`, `/browse`, `/connect-chrome`, `/qa`, `/qa-only`, `/design-review`, `/setup-browser-cookies`, `/setup-deploy`, `/retro`, `/investigate`, `/document-release`, `/codex`, `/cso`, `/autoplan`, `/careful`, `/freeze`, `/guard`, `/unfreeze`, `/gstack-upgrade`.

## Skill routing

When the user's request matches an available skill, ALWAYS invoke it using the Skill
tool as your FIRST action. Do NOT answer directly, do NOT use other tools first.
The skill has specialized workflows that produce better results than ad-hoc answers.

Key routing rules:
- Product ideas, "is this worth building", brainstorming → invoke office-hours
- Bugs, errors, "why is this broken", 500 errors → invoke investigate
- Ship, deploy, push, create PR → invoke ship
- QA, test the site, find bugs → invoke qa
- Code review, check my diff → invoke review
- Update docs after shipping → invoke document-release
- Weekly retro → invoke retro
- Design system, brand → invoke design-consultation
- Visual audit, design polish → invoke design-review
- Architecture review → invoke plan-eng-review
