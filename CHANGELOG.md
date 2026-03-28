# Changelog

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
