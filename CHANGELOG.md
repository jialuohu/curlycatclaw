# Changelog

## [0.4.0] - 2026-03-27

Phase 4 "Trust Hardening." Closes the security and trust gaps flagged by adversarial reviews.

### Added
- MCP environment filtering: allowlist-based env var inheritance for child processes, preventing secret leakage (CURLYCATCLAW_MASTER_KEY, API keys). Per-server `env_inherit` config for additional vars.
- Telegram secure defaults: empty `allowed_user_ids` now fails validation unless `allow_all = true` is explicitly set. Prevents accidental public bots.
- Wasm send_message chat scoping: plugins can only send messages to the chat that invoked them. Cross-chat injection blocked with warning log.
- Tool transparency: `[tool]` lines sent to user in Telegram showing what tools Claude called. Opt-out via `show_tool_calls = false`.
- MCP user context: `_user_context` map (user_id, chat_id) injected into MCP tool arguments, enabling per-user access control in MCP servers.
- Tool confirmation: `confirm_tools` config lists tool name prefixes requiring user approval. Stateless via Claude re-ask pattern.

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
