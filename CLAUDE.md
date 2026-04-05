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
- **Thinking effort**: `thinking_effort` config field (low/medium/high/max) controls Claude's reasoning depth; direct API maps effort to `ThinkingConfigParamOfEnabled` with budget presets (10K high, 32K max) and model-aware MaxTokens cap (128K); CLI subprocess passes `--effort` flag at spawn time; `/effort` Telegram command overrides per session (kills+respawns CLI subprocess); `/retry` replays last message at specified effort (one-shot, restores previous override); thinking block signatures captured for API conversation history continuity; `CURLYCATCLAW_THINKING_EFFORT` env var override
- **CLI subprocess**: spawns `claude --print --input-format stream-json --output-format stream-json` per user; auth via `CLAUDE_CODE_OAUTH_TOKEN` env var (long-lived token from `claude setup-token`, configured in `oauth_token` config field); CLI handles token exchange, LLM calls, and tool execution via MCP; curlycatclaw parses events for Telegram streaming and SQLite logging; persistent scan goroutine delivers events via channel for proper context cancellation; deferred cleanup in spawn() prevents zombie processes; optional `WorkDir` for project-scoped work, optional `HomeDir` for isolated Claude home (clean plugin environment), optional `Model` for per-spawn model override (used by extraction with `extraction_model`, summarization with `summarize_model`, and per-reminder `model` field)
- **Streaming responses**: text deltas streamed to Telegram via message edits (500ms debounce, strings.Builder accumulation), tool_use transitions start new messages, error mid-stream appends notice, pre-stream errors send visible feedback, msgID -1 sentinel handled at all sites, `flushing` state flag releases mutex during Telegram I/O to prevent lock contention, final flush converts markdown to Telegram HTML via `internal/mdhtml` (falls back to plain text on parse error), overflow split (>3900 runes) enables HTML conversion before sealing the first message
- **Media support**: unified `Attachment` type in `internal/telegram/channel.go` handles photos, documents, voice, and audio; `AttachmentKind` enum (`AttachPhoto`, `AttachDocument`, `AttachVoice`, `AttachAudio`); `downloadFile` fetches any file type with size limit (`maxDownloadBytes = 20 MiB`) and truncation detection; `IncomingMessage.Photos()` filters photo attachments for backward compatibility; `SendDocument` sends files back to users via the channel actor's Run loop
- **Typing indicators**: `SendTyping` sends "typing..." chat action via buffered channel (fire-and-forget, drops if full); `startTypingLoop` in `internal/session/actor.go` refreshes every `typingRefreshInterval` (4.5s) via goroutine with context cancellation; started at `handleMessage` entry, stopped via `defer`
- **Voice transcription**: opt-in via `[voice]` config, uses OpenAI Whisper API (`internal/voice/stt.go`), `Transcriber` interface for pluggable STT providers, transcribes voice/audio attachments to text before sending to Claude, graceful degradation when disabled or on error, 30s timeout, STT-only (no TTS)
- **MCP manager**: persistent stdio server connections, tool namespacing (server__tool), allowlist-based env filtering, user context injection, runtime `AddServer`/`RemoveServer` for dynamic MCP management
- **Extension registry**: runtime-added MCP servers, exec skills, and prompt skills managed via Telegram (`add_extension`, `remove_extension`, `list_extensions`, `set_extension_env`, `unset_extension_env`), persisted to `extensions.json`, loaded at startup, works in both direct API and CLI modes, env var filtering (LD_PRELOAD/DYLD_* blocklist), `Update` method for in-place extension modifications with deep-copy rollback, auto-splits command strings with spaces when no args provided
- **MCP extension proxy**: in CLI mode, runtime MCP extensions are proxied through the curlycatclaw-skills MCP server subprocess (not via `--mcp-config`), bypassing a Claude CLI bug where dynamic servers fail tool discovery; proxy connects as MCP client, discovers tools, registers with namespaced names (`extname__toolname`), 30s connect timeout, zero-tool skip, env resolution for `encrypted:ref:` values via credential store
- **Encrypted extension env vars**: `set_extension_env` / `unset_extension_env` skills store API keys encrypted (AES-256-GCM) via credential store, referenced as `encrypted:ref:ext_<name>_<key>` in extensions.json, resolved at proxy spawn time; master key passed to MCP subprocess via temp file (avoids /proc/PID/cmdline exposure); dangerous env key validation rejects LD_PRELOAD/DYLD_* at set time
- **MCP extension hot-reload**: when adding/removing MCP extensions in CLI mode, proxy tools are registered/unregistered dynamically on the running MCP server using `Server.AddTool()`/`Server.RemoveTools()` instead of restarting the subprocess; mcp-go auto-sends `notifications/tools/list_changed` to Claude CLI; falls back to reload-flag on failure; connect-new-first pattern for env updates (zero tool downtime); stale tools cleaned up when extension tool set shrinks across reconnections; `MCPHotReloader` interface in `internal/extension/skills.go`, implemented by `mcpHotReloader` in `cmd/curlycatclaw/mcp_server.go`
- **Conversation history injection**: when a CLI subprocess is freshly spawned (plugin reload, idle timeout, crash) and an active conversation exists, recent turns from SQLite are prepended to the first user message as a text preamble (up to 10 turns, 2000 runes/message cap); `GetOrCreate` returns `isNew` bool so `handleWithCLI` can detect fresh spawns; `buildHistoryPreamble()` in actor.go formats the transcript
- **Prompt skills**: `type=prompt` extensions for markdown instruction files (SKILL.md + supporting files in a directory), `load_prompt_skill` reads instructions on demand, system prompt lists available prompt skills with descriptions, system prompt includes exec JSON protocol spec for Claude-driven CLI tool wrapper generation
- **Memory**: SQLite WAL mode, sliding window context (25 turns, ~150K tokens), conversations keyed by (userID, chatID), transactional check-and-create for active conversations
- **Hierarchical memory**: four-tier (user facts in system prompt, observations via Qdrant relevance search, conversation summaries via Qdrant relevance search, current conversation sliding window), opt-in via `[memory]` config
- **Observation memory**: automatic extraction of decisions, preferences, project state, commitments, discoveries, and references from conversations; `ObservationExtractor` in `internal/memory/observer.go` runs after idle detection (in-memory turn counter); SQLite tables (`observations`, `observation_facts`, `observation_extraction_state`, `observation_entities`, `observation_relations`) store structured observations; Qdrant collection `curlycatclaw_observations` for semantic search with multi-vector indexing (per-fact Qdrant points); FTS5 hybrid search merges keyword and vector results via Reciprocal Rank Fusion (RRF); progressive 3-layer retrieval (compact index, expanded top matches, on-demand full detail); entity extraction tracks people/projects/files/tools with FTS5 search; observation relations (supersedes/refines/contradicts) with active supersession wiring in extraction pipeline; system prompt "What I remember" section injects relevant observations with dedup against facts; parallel Qdrant queries for observations and summaries; opt-in via `[memory.observations]` config
- **Self-healing memory (Phase 3)**: extraction pipeline detects supersession of existing `project_state` observations with configurable confidence threshold (`supersession_threshold`, default 0.8); superseded observations filtered from search results via `GetSupersededObservationIDs`; `supersede_observation` skill enables Claude to act on corrections detected in conversation (real-time correction); soft delete via `archived_at` column (archive/restore instead of permanent delete); inline Telegram notifications when relations are created; `/keep_both`, `/revert`, `/forget_old` commands for user control; `update_observation` skill for manual edits; FTS5 UPDATE triggers keep search index in sync; `AddObservationRelation` uses `INSERT OR IGNORE` with confidence clamping for concurrency safety
- **Facts**: persistent per-user facts with category, sanitization (200-rune limit, control char strip, rune-safe UTF-8 truncation), IDOR-protected delete, proactive extraction via system prompt instruction
- **Conversation archival**: async summarization on conversation expiry (>4h idle) in both direct API and CLI modes (CLI uses `SpawnOneShot`), crash recovery retries `pending`/`failed`/`indexed_failed` conversations on startup (sequential, capped at 20, oldest first), head+tail transcript sampling (first 5000 + last 5000 runes), chat-type-aware retrieval (DM summaries user-scoped, group summaries chat-scoped), `IndexSummary` stores `chat_type` metadata in Qdrant, dedicated `curlycatclaw_summaries` Qdrant collection
- **Vector search**: Qdrant gRPC for semantic search, pluggable Embedder interface (Ollama local default / FNV offline fallback / Voyage AI paid), configurable search timeout via `vector_search_timeout_seconds` (default 5s), `BatchEmbed` for efficient bulk operations, default model `bge-m3` (1024d)
- **Background embedding migration**: when embedder config changes, `MigrationManager` re-embeds all vectors in the background while search continues serving from old collections; versioned Qdrant collections (`_v1`, `_v2`) with fixed names as aliases; `atomic.Pointer` dual-write during migration (new content goes to both old and new collections); catch-up convergence phase before atomic alias swap via `UpdateAliases`; crash-resumable via keyset pagination (cursor-based `WHERE id > last_seen_id`); `embedder_state` SQLite table tracks active/migrating embedder, version, and progress; `SwapEmbedder` atomically switches live embedder after alias swap; old embedder config stored as scalar columns (no secrets in SQLite); `--migrate-embedder` CLI updated as manual fallback
- **Config validation**: startup validation of required fields (db_path, MCP server name/command, qdrant_addr when vector enabled, wasm skills_dir when wasm enabled, health port range, embedder type fnv/ollama/voyage with required fields per type)
- **Skills**: built-in Go skills (search, note, remind, semantic_search, remember_fact, forget_fact, list_facts, list_summaries, delete_summary, search_observations, list_observations, get_observation, forget_observation, restore_observation, update_observation, supersede_observation, search_entities, install_plugin, uninstall_plugin, list_plugins, enable_plugin, disable_plugin, update_plugin, add_marketplace, remove_marketplace, list_marketplaces, add_extension, remove_extension, list_extensions, load_prompt_skill, set_extension_env, unset_extension_env) + Wasm plugin runtime + external skill collections; note has title/content size limits (500 runes / 100KB), semantic_search capped at 50 results, remind validates cron expressions at input time, optional `prompt` field for Claude-powered cron tasks (clean context, facts only, full tool access); marketplace auto-bootstrap on first install (two built-in: claude-plugins-official + ui-ux-pro-max-skill), lazy auto-update for stale plugins (>7 days), autonomous web search for missing marketplaces on install failure; standard plugins (context7, playwright, ui-ux-pro-max, superpowers, claude-md-management, hookify, skill-creator) pre-installed on first startup
- **External skill collections**: `internal/skillloader` loads exec-based skills from directory trees via `[[skill_collections]]` config; `collection.toml` + per-skill `skill.toml` descriptors; exec adapter spawns per invocation with minimal env (PATH/HOME/TMPDIR only, no daemon secret leakage); fsnotify hot-reload; path traversal prevention; LD_PRELOAD/DYLD_* env blocklist
- **Project work**: `/project <name>` Telegram command switches CLI subprocess working directory; `[[projects]]` config declares allowed project paths; isolated Claude home (`isolated_home` config) gives subprocess a clean `~/.claude/` with no inherited plugins; file-based reload signal (`~/.curlycatclaw/claude-home/.curlycatclaw-reload-needed`) triggers subprocess respawn after plugin changes (extension changes use hot-reload instead, falling back to respawn on failure)
- **Cron tasks**: `CronExecutor` runs scheduled prompts through Claude with ephemeral context (no conversation DB), user facts in system prompt, full skill/MCP tool access, 3-slot concurrency semaphore, rate limit retry, 5-minute timeout; CLI mode uses `SpawnOneShot` for isolated subprocess with per-reminder `model` override (e.g., haiku for cheap tasks); `CronRunner` interface in skills package avoids circular imports; `set_reminder` skill accepts optional `model` field persisted to SQLite `reminders.model` column
- **Wasm runtime**: wazero-based with capability model, JSON-over-shared-memory, hot-reload (atomic reload with map-based Execute lookup), chat-scoped send_message, db_read enforced user scoping via `:user_id` placeholder (queries on user-scoped tables without `:user_id` are rejected, not just warned; quote-aware replacement, 10 MiB result size cap, rows.Err check), UNION/INTERSECT/EXCEPT/WITH blocked in isSelectOnly, comment-stripped word-boundary table detection, HTTP private IP blocklist (SSRF prevention), connect-time IP verification (DNS rebinding protection), sanitized DB errors, 50 MiB module size cap, compiled module cleanup on unload, skill-name-based registry unregister
- **Tool transparency**: `[tool]` lines sent to user in Telegram, opt-out via `show_tool_calls`
- **Tool confirmation**: `confirm_tools` prefix list for sensitive operations, stateless via Claude re-ask
- **Logging**: configurable level/format/file via `[logging]` config, lumberjack rotation
- **Google Workspace MCP**: standalone `curlycatclaw-gws-mcp` binary bridges Claude to Google Workspace via the `gws` CLI; discovers tools dynamically from `gws generate-skills`; concurrent boolean flag detection from `--help` output; argument injection prevention (`validArg` regex + expanded reserved flags + server-side flag allowlist per helper tool); `_user_context` stripped; `generateSkills` uses `os.TempDir()` for Docker compatibility
- **Config MCP server proxy**: in CLI mode, config-based MCP servers (`[[mcp.servers]]`) are proxied through curlycatclaw-skills subprocess (same pattern as runtime extensions); system prompt lists discovered tools so Claude uses them proactively
- **GitHub MCP**: external `github-mcp-server` binary (maintained by github/github-mcp-server), configured via `[[mcp.servers]]`, default toolsets: repos/issues/pull_requests/actions/users with `--read-only`, system prompt includes workflow-specific guidance for CI status, PR review, issue creation, release tracking, code search when GitHub tools are detected
- **Default extension protection**: pre-installed extensions (scrapling-mcp, scrapling, humanizer) cannot be removed via `remove_extension`; `IsDefault()` guard in `internal/extension/defaults.go`
- **Unified capability listing**: system prompt instructs Claude to call both `list_plugins` and `list_extensions` for any capability query; built-in skills listed statically; fresh tool calls enforced every time

## Key Files

| File | Purpose |
|------|---------|
| `cmd/curlycatclaw/main.go` | Binary entrypoint, config loading, actor bootstrap, health server |
| `config/config.go` | TOML config struct, defaults, validation |
| `internal/session/actor.go` | Central session actor wiring everything together |
| `internal/session/deps.go` | Testability interfaces (LLMClient, MessageStore, etc.) |
| `internal/claude/client.go` | Claude API streaming + non-streaming client (direct mode) |
| `internal/claude/subprocess.go` | CLI subprocess manager + stream-json parser (CLI mode) |
| `cmd/curlycatclaw/mcp_server.go` | MCP stdio server exposing built-in skills + proxy for runtime MCP extensions |
| `internal/telegram/channel.go` | Telegram channel actor |
| `internal/memory/store.go` | SQLite storage |
| `internal/mcp/manager.go` | MCP server lifecycle |
| `internal/memory/embedder.go` | Embedder interface + FNV/Ollama/Voyage implementations |
| `internal/memory/facts.go` | User facts CRUD (sanitization, IDOR protection) |
| `internal/memory/summarizer.go` | Conversation summarizer (transcript formatting + Claude) |
| `internal/memory/vectorstore.go` | Qdrant vector search (messages, notes, summaries, observations) |
| `internal/memory/observer.go` | ObservationExtractor (automatic observation extraction from conversations) |
| `internal/memory/observation.go` | Observation CRUD and SQLite storage |
| `skills/observation.go` | Observation skills (search, list, get, forget, search_entities) |
| `internal/wasm/runtime.go` | Wasm skill runtime (wazero) |
| `internal/session/cron.go` | CronExecutor for scheduled Claude-powered tasks |
| `skills/` | Built-in skill implementations |
| `skills/summary.go` | Summary management skills (list_summaries, delete_summary) |
| `skills/plugin.go` | Plugin management skills (install, uninstall, enable, disable, list) |
| `internal/extension/extension.go` | Runtime extension registry (MCP servers + exec skills) |
| `internal/mdhtml/convert.go` | Markdown to Telegram HTML converter |
| `internal/voice/stt.go` | OpenAI Whisper speech-to-text client |
| `internal/skillloader/loader.go` | External skill collection loader (exec adapter) |
| `internal/memory/migration.go` | Background embedding migration manager (backfill, catch-up, alias swap) |
| `cmd/curlycatclaw/migrate.go` | CLI embedder migration tool (manual fallback, versioned collections + aliases) |
| `cmd/curlycatclaw-gws-mcp/` | Standalone MCP server for Google Workspace via gws CLI |
| `Dockerfile` | Container build (CGO_ENABLED=0, Debian bookworm-slim) |
| `docker-compose.yml` | curlycatclaw + Qdrant + Ollama orchestration |
| `.goreleaser.yml` | Release automation (binaries, checksums, Docker images) |

## Configuration

Copy `config.toml.example` to `~/.curlycatclaw/config.toml` and fill in credentials. All paths use Docker mount paths (`/data/...`). Docker Compose mounts `~/.curlycatclaw` as `/data`.

Auth modes: `cli_path` + `oauth_token` (Claude subscription via CLI subprocess) or `api_key` (direct API). CLI mode uses `oauth_token` from `claude setup-token` injected as `CLAUDE_CODE_OAUTH_TOKEN` env var.

For Google Workspace, export credentials on a machine with a browser (`gws auth export --unmasked > ~/.curlycatclaw/gws-credentials.json`) and set `GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE = "/data/gws-credentials.json"` in `[mcp.servers.env]`.

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
