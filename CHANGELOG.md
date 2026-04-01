# Changelog

## [0.14.3] - 2026-04-01

Plugins that need bun, python, or uvx now work out of the box. If a plugin needs a command that's missing, the bot tells you.

### Added
- **Docker: bun, python3, uv/uvx runtimes** — pre-installed in the Docker image so plugin MCP servers that use these commands work without manual setup.
- **Plugin command check on install** — after installing a plugin, checks if the required runtime command is available. Warns the user if the command is missing so they know before they try to use it. HTTP-based plugins are skipped (no local command needed).

## [0.14.2] - 2026-04-01

### Fixed
- **Docker: keep npm/npx for plugin MCP servers** — plugin MCP servers like context7 use `npx` to start. Previously purged after Claude CLI install, breaking all npx-based plugins.
- **Pre-tool text now gets Telegram HTML formatting** — text streamed before a tool call (e.g., "Let me look up `useEffect`...") now converts markdown to Telegram HTML. Previously sent as raw markdown because the pre-tool flush didn't enable HTML mode.

## [0.14.1] - 2026-04-01

After installing a plugin, the bot knows what it does and how to use it. No more "I don't know what context7 is" after you just installed it.

### Added
- **Plugin awareness in system prompt**: Installed plugins are listed with descriptions so Claude knows what they do and uses them proactively. Known plugins (context7, playwright) get specific guidance.
- **Enhanced install message**: After installing a plugin, Claude tells the user the tools will be ready on the next message.
- **Defensive pre-turn reload check**: Subprocess reload now also happens at the start of each message, guaranteeing the next message after a plugin install gets the updated tools.

### Removed
- **`.env.example`**: Unused file. `CURLYCATCLAW_MASTER_KEY` is documented in `deploy/docker.md` and README.

### Changed
- `deploy/docker.md`: Updated encrypted credentials section to use docker-compose environment instead of `.env` file.

## [0.14.0] - 2026-04-01

Plugin installs just work now. The bot auto-bootstraps the official marketplace on first install, keeps it fresh, and shows you what tools it's using in real time instead of freezing mid-sentence.

### Added
- **Marketplace auto-bootstrap**: First `install_plugin` call automatically registers the official Claude plugin marketplace. Subsequent installs skip the setup (idempotent). Marketplace data older than 24h is auto-updated via git pull. No config needed.
- **Real-time tool notifications**: `[tool]` messages now appear in Telegram the moment Claude starts using a tool, not after the entire response completes. Text streams, tool notification fires, tool executes, text resumes. No more frozen screens during tool calls.
- **git in Docker image**: The container now includes git, required for marketplace clone operations.

### Changed
- `Send()` in CLI subprocess now accepts an `onToolUse` callback for real-time tool start events
- Tool transparency messages moved from post-hoc event loop to streaming callback
- `parseStreamDelta()` now parses `content_block_start` events with `tool_use` type

## [0.13.1] - 2026-03-31

Plugins you install via Telegram now actually work. The bot reads the real plugin manifest and passes MCP servers to the CLI subprocess correctly. Previously, plugin discovery was broken since day one.

### Fixed
- **Plugin MCP server discovery**: `buildMCPConfig` now reads `installed_plugins.json` manifest and follows each plugin's `installPath` to find `.mcp.json` server declarations. Previously scanned wrong directory structure and expected wrong JSON format, resulting in zero plugins ever being discovered.
- **Collision guard**: Plugin servers that collide with built-in server names (e.g. `curlycatclaw-skills`) are skipped with a warning instead of silently overwriting.
- **HTTP-type MCP servers**: Extended `mcpServer` struct to support `type`, `url`, and `headers` fields for HTTP-based MCP servers (like Linear, GitHub).

### Added
- **`CURLYCATCLAW_ISOLATED_HOME` env override**: Docker deployments can now override `isolated_home` via environment variable, matching the pattern used by `cli_path`, `db_path`, and `qdrant_addr`.
- **Plugin skills in README**: Documented `install_plugin`, `uninstall_plugin`, `list_plugins`, `enable_plugin`, `disable_plugin` in the Built-in Skills table and Configuration example.
- **TODOS.md**: Created project TODO tracker.

## [0.13.0] - 2026-03-31

Talk to your bot in Telegram and get properly formatted replies. Tell it to work on your projects. Load custom skills from disk. Switch embedding providers without losing data.

### Added
- **Telegram HTML rendering**: Claude's markdown output is converted to Telegram-safe HTML on final message delivery. Falls back to plain text if Telegram rejects the formatting. New `internal/mdhtml` package.
- **CLI project work**: `/project <name>` command switches the Claude CLI subprocess to a project directory. Isolated Claude home directory prevents plugin inheritance from local setup.
- **Plugin management via Telegram**: `install_plugin`, `uninstall_plugin`, `list_plugins`, `enable_plugin`, `disable_plugin` skills manage Claude Code plugins in the isolated home. Plugin names validated against config allowlist. File-based reload signal triggers subprocess respawn.
- **External skill collections**: Load exec-based skills from directory trees via `[[skill_collections]]` config. Each skill is a `skill.toml` descriptor + executable. Minimal env (PATH/HOME/TMPDIR only) prevents secret leakage. fsnotify hot-reload.
- **Embedder migration tool**: `curlycatclaw migrate-embedder` command wipes and rebuilds Qdrant vector collections with the configured embedder. Supports `--dry-run`. Adds `BatchEmbed` to the Embedder interface (128-item batches for Voyage AI). Tests embedder connectivity before deleting collections.

### Changed
- CLI subprocess no longer blocks `ToolSearch` in `--disallowedTools`
- `replaceEnv` in subprocess.go now copies the slice before mutating (prevents data races)
- `VoyageEmbedder` and `OllamaEmbedder` now implement `BatchEmbed` for efficient bulk re-embedding

### Fixed
- URL attribute injection in markdown link conversion (quote escaping)
- Path traversal in external skill command resolution
- LD_PRELOAD/DYLD_* blocked in external skill environment
- Plugin skills now use minimal env (no CURLYCATCLAW_MASTER_KEY leakage)
- Migration preserves original `created_at` timestamps instead of stamping with current time

## [0.12.1] - 2026-03-31

Memory system hardening: startup warnings, safer concurrency, and user control over summaries.

### Added
- **Startup warning** when FNV embedder is used with memory enabled, recommending Ollama or Voyage for semantic search quality
- **`list_summaries` skill**: view all stored conversation summaries with IDs, dates, and previews
- **`delete_summary` skill**: remove incorrect or unwanted summaries by ID (IDOR-protected)
- **Dedicated summarization semaphore** (`sumSem`, capacity 2): summarization can no longer be silently dropped when message indexing fills the shared semaphore

### Changed
- **Summary prompt framing**: now warns Claude that summaries may contain errors from prior assistant responses, to use as hints only, and to tell the user if a summary seems wrong

## [0.12.0] - 2026-03-31

CLI mode now has full memory, conversation summaries survive crashes, and the bot remembers context across DMs without leaking into group chats.

### Added
- **CLI mode summarization**: conversations are now summarized when they expire in CLI mode (Claude Max subscription). Uses `SpawnOneShot` to make one-shot Claude calls, same pattern as cron tasks. Previously, CLI mode had zero cross-conversation memory beyond explicit user facts.
- **Crash recovery for summarizations**: on startup, the daemon retries conversations stuck in `pending`, `failed`, or `indexed_failed` states from previous runs. Sequential background processing, capped at 20 per restart, oldest first.
- **Summary index durability**: if Qdrant vector indexing fails after a summary is saved to SQLite, the conversation is marked `indexed_failed` and retried on next startup. Previously, these summaries were invisible to search forever.
- **Chat-type-aware summary retrieval**: DM summaries are user-scoped (searchable from any DM). Group/supergroup summaries are chat-scoped (stay in that group). Prevents private DM context from leaking into group chat responses. Includes provenance labels in the system prompt.
- **`IndexSummary`**: new vector store method that includes `chat_type` metadata in Qdrant payloads
- **`RecoverableSummarizations`** and **`GetSummaryText`**: new store methods for crash recovery
- **`ChatType` field** on `IncomingMessage` from Telegram's `Chat.Type`
- **`chat_type` column** on `conversations` and `conversation_summaries` tables (safe migration)

### Changed
- **Transcript sampling**: `FormatTranscript` now uses head+tail sampling (first 5000 + last 5000 runes) instead of head-only truncation at 4000 chars. Long conversations no longer lose their endings in summaries.
- **`SearchSummaries`**: filter logic now depends on chat type instead of always filtering by `(user_id, chat_id)`
- **`ConversationMeta`**: now returns `chatType` from the conversations table
- **`GetActiveConversation`**: accepts and stores `chatType` parameter

## [0.11.0] - 2026-03-30

Reminders can now run Claude with a prompt at fire time, turning static text notifications into scheduled AI tasks.

### Added
- **Claude-powered cron tasks**: set a reminder with an optional `prompt` field and Claude executes it at the scheduled time with full tool access (web_search, notes, facts, semantic_search, MCP). Results are sent to your Telegram chat.
- **`CronExecutor`**: runs scheduled prompts with clean context (user facts only, no conversation history), 3-slot concurrency limiter, 5-minute timeout, rate limit retry
- **`SpawnOneShot`**: isolated CLI subprocess for cron tasks that doesn't interfere with your active conversation's CLI process
- **`CronRunner` interface**: decouples reminder actor from session package (avoids circular imports)
- **`[cron:status]` tags** in `list_reminders` output to distinguish Claude-powered reminders from static ones
- **Schema migration**: `prompt` column added to reminders table (idempotent, handles duplicate column gracefully)

### Changed
- **`set_reminder` skill**: accepts optional `prompt` parameter
- **`NewReminderActor`**: accepts optional `CronRunner` for Claude execution (nil = static text only, backwards compatible)
- **`fireReminder` refactored**: extracted `trySendTelegram` and `markFiredIfOneTime` helpers

## [0.10.5] - 2026-03-30

Clean up stale references across goreleaser config and application code.

### Fixed
- **Goreleaser archive**: removed reference to deleted `deploy/curlycatclaw.service` that would fail release builds
- **Goreleaser Docker**: renamed `dockers_v2` to `dockers` (correct key for goreleaser v2 config format, previously silently skipped)

### Removed
- **`auth_token` config field**: removed undocumented field from `ClaudeConfig` struct, validation, and `AuthOption()`. Config now accepts `cli_path` or `api_key` only, matching documentation since v0.10.4

## [0.10.4] - 2026-03-30

CLI subprocess auth, response delivery, and Docker unification.

### Fixed
- **Nil interface trap**: nil `*CLIManager` passed as `CLIClient` interface was non-nil, routing all messages to CLI mode and crashing with nil pointer panic on every message (Go nil-interface gotcha)
- **Silent error results**: CLI subprocess error results (auth failure, rate limit, max_turns) were logged but never sent to Telegram, leaving users in silence
- **finalFlush race condition**: when a streaming flush was in progress, `finalFlush()` returned early and the last chunk of text was lost
- **Panic stack traces**: supervisor now logs `debug.Stack()` on actor panics for debuggability

### Changed
- **OAuth auth via env var**: CLI subprocess receives `CLAUDE_CODE_OAUTH_TOKEN` from config's `oauth_token` field instead of reading short-lived token from `~/.claude/.credentials.json`
- **Unified config**: Docker and local use the same `config.toml` with env var overrides (`CURLYCATCLAW_DB_PATH`, `CURLYCATCLAW_QDRANT_ADDR`, `CURLYCATCLAW_CLI_PATH`) instead of separate `config.docker.toml`
- **Debian Docker base**: switched from Alpine to Debian bookworm-slim with Claude CLI installed via npm (Alpine's musl can't run the glibc-linked claude binary)
- **Removed `--bare` flag**: CLI subprocess no longer uses `--bare` mode, which blocked OAuth authentication

### Removed
- `config.docker.toml` — replaced by env var overrides on unified config
- `internal/claude/auth.go` — dead code with wrong credential schema
- `readOAuthToken()` — read wrong short-lived token from credentials.json

### Added
- `config.oauth_token` field for long-lived token from `claude setup-token`
- Env var overrides: `CURLYCATCLAW_DB_PATH`, `CURLYCATCLAW_QDRANT_ADDR`, `CURLYCATCLAW_MODEL`, `CURLYCATCLAW_CLI_PATH`
- Integration tests for CLI path: error result delivery, normal response, finalFlush race
- `ScanResult` and `NewTestProcess` exports for CLI subprocess testing

## [0.10.3] - 2026-03-29

Concurrency, correctness, security, and reliability sweep across 15 files.

### Fixed
- **Streaming deadlock**: `streamState.flush()` releases mutex during Telegram I/O with `flushing` state flag to prevent duplicate messages and lock contention
- **CLI subprocess context cancellation**: persistent scan goroutine delivers events via channel, enabling proper `select` on ctx/done/scanCh (previously `scanner.Scan()` blocked context cancellation)
- **CLI subprocess stdout pipe leak**: `Kill()` now explicitly closes stdout pipe
- **CLI subprocess spawn cleanup**: deferred cleanup on error paths after `cmd.Start()` prevents zombie processes
- **AppendMessage atomicity**: INSERT + UPDATE now wrapped in a single transaction
- **Budget fallback caching**: LLM classification fallback "full" no longer cached permanently (prevents stale cache entries from transient LLM failures)
- **UTF-8 truncation**: fact sanitization and summarizer transcript truncation use rune-based slicing (prevents invalid UTF-8 from split multi-byte characters)
- **Wasm SQL scoping enforced**: `hostDBQuery()` now rejects (not just warns) queries on user-scoped tables without `:user_id` binding
- **Wasm UNION/INTERSECT/EXCEPT blocked**: `isSelectOnly()` prevents set-operation bypasses of user scoping
- **Wasm table detection**: `userScopedTableAccessed()` strips SQL comments and uses word-boundary matching (prevents false positives from comments and substrings)
- **Remind signal timeout**: set/cancel now block up to 5s with error on timeout (previously silently dropped signals)
- **Remind cancel error message**: corrected copy-paste error in cancel timeout message
- **Signal handler leak**: goroutine exits cleanly on shutdown via `shutdownComplete` channel
- **CLI cleanup ordering**: cleanup goroutine waits before `Shutdown()` to avoid races

### Changed
- **Indexing semaphore**: vector indexing, summarization, and fact updates bounded by 10-slot semaphore (prevents goroutine accumulation under load)
- **Config validation**: embedder type validated at config load (fnv/ollama/voyage with required fields)
- **Dead code removed**: unused `CLIProcess.scanner` struct field removed after scanCh refactor

## [0.10.2] - 2026-03-29

Performance, correctness, and reliability fixes across session actor, skills, and memory.

### Fixed
- **Streaming msgID sentinel**: all comparison sites now use `<= 0` / `> 0` to handle -1 timeout sentinel (prevents Telegram edit failures with invalid message ID)
- **Pre-stream error feedback**: users now see `[error: failed to get response]` when Claude errors before streaming starts (both API and CLI paths)
- **Cancelled reminders still firing**: `cancelJob` now calls `scheduler.RemoveJob` to actually stop gocron jobs
- **Cron validation**: invalid cron expressions rejected at input time with clear error message
- **Note input validation**: title capped at 500 chars, content at 100KB, search results at 100
- **Semantic search limit**: capped at 50 results to prevent resource exhaustion

### Changed
- **Streaming performance**: replaced string concatenation with `strings.Builder` + rune counter (eliminates O(n^2) copying per response)
- **Tool schema caching**: parsed MCP tool schemas cached and reused across messages
- **Tool loop pre-allocation**: slices pre-allocated based on tool call count
- **Context leak fix**: tool execution wrapped in IIFE for proper per-iteration cancel
- **Budget cache index**: added `idx_budget_cache_created` for O(log N) cleanup queries

## [0.10.1] - 2026-03-29

Codebase quality sweep: security hardening, correctness fixes, dead code cleanup.

### Fixed
- **Wasm query result size cap**: 10 MiB limit prevents OOM from unbounded db_read results
- **Wasm SQL placeholder collision**: `:user_id` inside SQL string literals no longer replaced (quote-aware parser)
- **Wasm hot-reload race**: Execute closure looks up module by name under RLock instead of capturing stale pointer
- **Wasm registry name mismatch**: UnloadModule/Close now unregister by skill name (from skill_info), not manifest name
- **Wasm compiled module leak**: Close() now calls compiled.Close() preventing JIT memory leak on restart
- **Wasm rows.Err() check**: scan loop now catches silent mid-query database errors
- **Wasm warning bypass**: scoping warning uses paramCount instead of strings.Contains to prevent bypass via quoted `:user_id`
- **Config validation**: Budget.Model now required when budget.enabled=true (fail-fast at config load)
- **VectorStore Close()**: nil guard + double-close protection (nil-out after close)

### Changed
- Wasm json.Marshal error in hostDBQuery now returns error response instead of empty result
- subprocess.go BuildUserMessage/BuildImageMessage: documented marshal safety with nolint comments
- telegram/channel.go comments: "chars" corrected to "runes" (matches utf8.RuneCountInString)
- skills/fact_test.go: strengthened test assertion (assert result contains "Remembered")

## [0.10.0] - 2026-03-29

CLI subprocess mode for Claude Max subscription routing.

### Added
- **CLI subprocess mode**: spawn `claude` CLI as a long-lived subprocess per user, enabling Claude Max subscription ($100/month unlimited) instead of per-API-call billing
- `CLIManager` in `internal/claude/subprocess.go`: manages long-lived CLI processes keyed by (userID, chatID), handles spawn, cleanup, and graceful shutdown
- Stream-json event parser: parses `system`, `stream_event`, `assistant`, and `result` events from CLI stdout with tolerant handling of unknown event types
- `--mcp-server` mode: curlycatclaw can run as an MCP stdio server exposing all built-in skills (note, remind, facts, search, semantic_search) for the CLI to call
- `cli_path` config field in `[claude]` section as alternative to `api_key`/`auth_token`
- `handleWithCLI()` in session actor: delegates to CLI subprocess with streaming to Telegram, SQLite logging, and vector indexing
- `BuildUserMessage()` and `BuildImageMessage()` helpers for stream-json input format

### Changed
- `session.New()` accepts optional `CLIClient` parameter for CLI mode
- Config validation accepts `cli_path` OR `api_key`/`auth_token` (not both required)
- `config.toml.example` documents all three auth modes

## [0.9.0] - 2026-03-28

OAuth Bearer token support, Docker-first deployment, and setup improvements.

### Added
- OAuth Bearer token authentication via `auth_token` config field (uses SDK `option.WithAuthToken`)
- `ClaudeConfig.AuthOption()` method for centralized auth option selection
- Docker Compose as primary deployment option in setup skill
- Config validation tests for auth mutual exclusion (6 new test cases)

### Changed
- `NewClient` accepts `option.RequestOption` instead of raw API key string
- `config.toml.example` shows OAuth token as preferred auth method, API key as alternative
- README config example updated to OAuth-first
- Setup skill (`/setup`) presents OAuth token first, Docker Compose as primary deployment
- `config.sh` accepts either `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY`
- Docker Compose: Qdrant healthcheck uses TCP connect (no wget/curl in container image)
- Docker Compose: config mounted directly from `~/.curlycatclaw/config.docker.toml`

### Fixed
- Docker Compose Qdrant healthcheck failure (wget not available in qdrant/qdrant image)
- Docker Compose removed unused `.env` file dependency

## [0.8.0] - 2026-03-28

Phase 8 "Streaming, Vision & Hardening." Real-time streaming responses to Telegram, image/photo support via Claude vision, and a comprehensive security + reliability audit fixing 10 verified bugs.

### Added
- Streaming responses: text deltas streamed to Telegram via message edits with 500ms debounce, new messages per tool-use round, error mid-stream appends "[error: response incomplete]" notice
- `OutgoingMessage.MessageID` for Telegram message editing, `ResultCh` for message ID feedback
- `streamState` in session actor manages debounce timer, current message ID, and tool_use transitions
- Image/photo support: `IncomingMessage.Photos` carries downloaded image data from Telegram
- Channel actor downloads best-quality photo from Telegram API, attaches to incoming messages
- Claude vision: user messages with photos get `ImageBlockParam` content blocks with base64-encoded data
- `handleUpdate` now accepts photo-only messages and photos with captions
- `bgCtx()` method on session actor for shutdown-aware background goroutines
- DNS rebinding protection: connect-time IP verification via custom `net.Dialer.DialContext` in Wasm HTTP client
- `golangci-lint` and `govulncheck` steps in CI pipeline
- Docker compose `depends_on` with health condition for Qdrant, healthcheck for curlycatclaw service
- Regression tests: Wasm DB error sanitization, HTTP response limits, web search limits, streaming, images

### Changed
- `UpdateLastReferenced` goroutine now tracked by `indexWg` for clean shutdown
- All async operations (`asyncSummarize`, vector indexing, summary search) derive context from actor's root context via `bgCtx()` instead of `context.Background()`
- `SetSummarizationStatus` errors now logged via `slog.Warn` instead of silently suppressed
- Wasm `hostDBQuery` returns generic "query failed" instead of raw SQLite errors (prevents schema disclosure)
- Wasm HTTP response reading uses `io.LimitReader` instead of manual loop (prevents 1MB overshoot)
- `web_search` skill uses dedicated HTTP client with 30s timeout and 2MB body limit
- Session actor `toolUseLoop` wires `OnPartialText` callback for streaming, with fallback for non-streaming LLM clients

### Fixed
- Untracked goroutine in `buildSystemPrompt` could race with store close on shutdown
- `context.Background()` in async ops prevented shutdown signaling to background goroutines
- Wasm DB error messages leaked SQLite table names to guest plugins
- DNS rebinding TOCTOU: `isPrivateIP` check could be bypassed via DNS rebinding between lookup and connection
- Wasm HTTP response read loop could exceed 1MB limit due to Go slice growth
- `web_search` used `http.DefaultClient` with no explicit timeout
- `web_search` used `io.ReadAll` with no body size limit
- Docker compose curlycatclaw started without waiting for Qdrant health
- `handleUpdate` silently dropped all photo-only Telegram messages

## [0.7.0] - 2026-03-28

Phase 7 "Hierarchical Memory." Three-tier memory gives Claude persistent awareness across conversations: user facts always in system prompt, conversation summaries relevance-retrieved via Qdrant, current conversation unchanged.

### Added
- User facts: persistent per-user facts stored in SQLite, injected into every system prompt with XML content fencing
- Proactive fact extraction: system prompt instructs Claude to call `remember_fact` when it learns something lasting about the user
- Three new skills: `remember_fact` (with category + 200-char sanitization), `forget_fact` (IDOR-protected), `list_facts` (grouped by category with IDs)
- Conversation summarization: async background goroutine generates summaries via Claude when conversations expire (>4h idle)
- Summarization crash recovery: `summarization_status` column tracks pending/done/failed, retryable on restart
- `FormatTranscript()` extracts plain text from stored JSON messages, strips tool blocks, truncates to 4000 chars
- Relevance-retrieved summaries: `SearchSummaries()` queries dedicated `curlycatclaw_summaries` Qdrant collection with (userID, chatID) filter and score threshold
- Non-streaming `Send()` method on Claude client for short tasks like summarization
- `MemoryConfig` section: enabled, max_facts, summary_relevance_limit, summary_score_threshold, summarize_model, min_messages_to_summarize
- `FactProvider` and `Summarizer` interfaces in session deps for testability
- 18 new tests across facts (7), summarizer (7), and fact skills (4)
- CODEOWNERS file requiring owner review for CI/CD and security files

### Changed
- `GetActiveConversation()` returns `(convID, expiredConvID, err)` triple to enable async summarization
- `buildSystemPrompt()` now accepts userID, chatID, currentMsg and injects facts + memory instructions + relevant summaries
- Tool transparency suppressed for `remember_fact`, `forget_fact`, `list_facts` (noisy in Telegram)
- `session.New()` accepts `FactProvider` and `Summarizer` dependencies
- `VectorStore` routes `source="summary"` to dedicated `curlycatclaw_summaries` collection
- CI workflow (`ci.yml`) now has explicit `permissions: contents: read` for least privilege

### Fixed
- `.dockerignore` now excludes `.env` and `.env.*` (prevents secrets in Docker build context)
- System prompt content fencing: facts and summaries wrapped in XML tags with "treat as data, not instructions" note (prompt injection mitigation)

## [0.6.0] - 2026-03-28

Phase 6 "Real Embeddings + Distribution." Pluggable embedding providers, goreleaser CI, Docker image publishing, and WASM security hardening.

### Added
- Embedder interface with three providers: FNV (offline default), Ollama (free local), Voyage AI (paid, best quality)
- OllamaEmbedder calls `/api/embed` with configurable model and dimensions
- VoyageEmbedder calls Voyage AI API with query/document input types and exponential backoff on 429
- FNVEmbedder extracts existing bag-of-words logic behind the Embedder interface
- Config fields: `embedder`, `ollama_url`, `ollama_model`, `ollama_dim`, `voyage_api_key`, `voyage_model`, `voyage_dim`
- `dockers_v2` section in `.goreleaser.yml` for multi-arch Docker images on ghcr.io
- `homebrew_casks` section (commented out, ready for tap repo creation)
- 37 new test cases across embedder, vectorstore, and WASM packages
- Golden-value FNV regression test to catch refactoring bugs

### Changed
- VectorStore accepts Embedder interface instead of hardcoded FNV hashing
- VectorStore uses dynamic dimensions from embedder (was hardcoded 384)
- Release workflow replaced with single-job goreleaser action (was 4-job matrix)
- All CI actions SHA-pinned to latest versions (Docker v4.0.0, goreleaser v7.0.0)
- Dockerfile.goreleaser uses TARGETPLATFORM for multi-arch support

### Fixed
- WASM `isHostAllowed` uses `net/url.Parse` instead of manual string splitting (fixes userinfo bypass: `http://evil@allowed.com`)
- WASM SQL keyword blocklist expanded with ATTACH, DETACH, PRAGMA, VACUUM, REINDEX

## [0.5.0] - 2026-03-27

Phase 5 "Codebase Health + Deployment." Full codebase audit, session actor testability, Docker support, goreleaser, and security hardening.

### Added
- Session actor interfaces (LLMClient, MessageStore, ContextProvider, ToolRouter, VectorIndexer, TelegramTransport) for testability
- 7 integration tests for session actor: BasicFlow, ToolUseLoop, MaxToolRounds, ToolConfirmation, VectorIndexing, ClaudeError, ShutdownCleanup
- Dockerfile (CGO_ENABLED=0, Alpine, non-root) for containerized deployment
- docker-compose.yml with Qdrant as a core service (always starts)
- .goreleaser.yml for unified binary releases with checksums and changelog
- Dockerfile.goreleaser for distroless release images
- CI workflow (.github/workflows/ci.yml) with go vet + go test -race
- deploy/docker.md deployment guide with MCP limitation callout

### Fixed
- Wired dead budget code path: session actor now calls BuildContextWithBudget instead of BuildContext
- Goroutine leak in vector indexing: WaitGroup tracking, bounded timeout, atomic counter for unique IDs
- Ignored json.Marshal errors in session actor (lines 160, 225, 253)
- SQL comment stripping: full state machine handling single/double quotes and escaped quotes
- WASM send_message default-deny: blocks when context is missing or zero ChatID
- WASM JSON string injection: marshalError() replaces unescaped fmt.Sprintf in host functions
- WASM compiled module leak: compiled.Close(ctx) in error paths
- WASM HTTP redirect SSRF: CheckRedirect re-validates each redirect against host allowlist
- Budget turnText now unescapes JSON strings for cleaner Haiku classification
- context.Context threaded through ClassifyTurns and classifyViaLLM (no more context.Background)
- GitHub Actions SHA-pinned across ci.yml and release.yml

### Changed
- MCP NewManager accepts version parameter, reported in MCP handshake (was hardcoded "0.1.0")
- Qdrant ports bound to 127.0.0.1 in docker-compose (was exposed to all interfaces)
- goreleaser before-hook uses `go mod verify` instead of `go mod tidy`

## [0.4.0] - 2026-03-27

Phase 4 "Trust Hardening." Closes the security and trust gaps flagged by adversarial reviews.

### Added
- MCP environment filtering: child processes only inherit safe env vars (PATH, HOME, etc.). Your API keys and master key no longer leak to MCP servers. Per-server `env_inherit` config for additional vars.
- Telegram secure defaults: empty `allowed_user_ids` now fails validation unless `allow_all = true` is explicitly set. Prevents accidental public bots.
- Wasm send_message chat scoping: plugins can only send messages to the chat that invoked them. Cross-chat injection blocked with warning log.
- Tool transparency: you can now see what tools Claude calls (`[tool]` lines in Telegram). Opt-out via `show_tool_calls = false`.
- MCP user context: `_user_context` map (user_id, chat_id) injected into MCP tool arguments, enabling per-user access control in MCP servers.
- Tool confirmation: mark sensitive tools in config and Claude will ask before running them. Stateless via Claude re-ask pattern.

### Changed
- Existing configs with empty `allowed_user_ids` must add either user IDs or `allow_all = true` (breaking change, intentional for security)

## [0.3.0] - 2026-03-27

Phase 3 "Production Hardening." Reliable shutdown, configurable logging, Linux sandboxing, and deployment tooling.

### Added
- Graceful shutdown: `SuperviseAll` uses `sync.WaitGroup` with configurable drain timeout (30s default), replacing the previous 100ms fixed sleep
- Configurable logging: `[logging]` config section with level, file output, rotation (via lumberjack), and JSON format support
- Landlock filesystem sandbox: Linux-only (`//go:build linux`) with BestEffort degradation, allowlists for data dir, config, zoneinfo, CA certs, and log rotation dir
- No-op sandbox stub for non-Linux platforms
- Version flag: `--version` prints version and exits, stamped via ldflags in release builds
- systemd unit file (`deploy/curlycatclaw.service`) with hardening directives (NoNewPrivileges, ProtectSystem=strict, PrivateTmp)
- Deployment docs: `deploy/UPGRADE.md` and README Deployment section
- Second SIGTERM forces immediate exit (standard force-exit pattern)
- WAL checkpoint (`PRAGMA wal_checkpoint(TRUNCATE)`) on graceful database close

### Changed
- Release workflow uses environment variable for version to prevent shell injection via tag names

## [0.2.0] - 2026-03-27

Phase 2 "Intelligence Layer." Smart context, semantic memory, scheduling, and plugins.

### Added
- Remind skill: set_reminder, list_reminders, cancel_reminder with persistent gocron scheduler
- ReminderActor with past-due fire on startup, timezone-aware scheduling, cron recurring
- Prompt budget manager: 3-tier context classification (keyword fast-path, SHA256 cache, Haiku LLM)
- BuildContextWithBudget falls back to sliding window on classification error
- Vector search via Qdrant gRPC with semantic_search skill and user-scoped collections
- Async message indexing in session actor for vector search
- Wasm skill runtime (wazero) with JSON-over-shared-memory protocol
- Host functions: catclaw_log, catclaw_http_get (allow-list), catclaw_db_query (SELECT-only), catclaw_send_message
- Capability-based security model with manifest.json per Wasm plugin
- fsnotify hot-reload for Wasm skills directory
- Config sections: [budget], [vector], [wasm] (all opt-in, disabled by default)

### Fixed
- Skill Registry now thread-safe with RWMutex (prevents data race from fsnotify goroutine)
- Reminder uses blocking send with 5s timeout (prevents silent message drops)
- GitHub Actions release workflow pinned to SHA (supply chain hardening)
- .gstack/ and .idea/ added to .gitignore

## [0.1.0] - 2026-03-26

Phase 1 MVP. A personal AI assistant that lives in your Telegram, powered by Claude.

### Added
- Goroutine-based actor model with panic recovery and exponential backoff supervision
- Claude API streaming client with tool_use state machine (10-round max, 120s timeout)
- Telegram channel actor with long-polling, user allowlists, and UTF-8 safe message chunking
- SQLite storage with WAL mode, conversation persistence, and sliding window context (25 turns, ~150K tokens)
- MCP server manager with persistent stdio connections and tool namespacing
- Built-in skills: web_search (DuckDuckGo), save_note, search_notes (all user-scoped)
- AES-256-GCM encrypted credential store for MCP server secrets
- TOML-based configuration with timezone support
- GitHub Actions release workflow for cross-platform binaries

### Fixed
- Tool-result messages from DB are now correctly reconstructed for Claude API replay
- MCP tool errors (isError flag) now properly propagate to Claude as error results
- Assistant responses with tool calls but no text are preserved in conversation history
- Telegram message splitting respects paragraph boundaries and code fences
- Notes are scoped per-user to prevent cross-user data leakage
- Nil From guard on Telegram channel posts
