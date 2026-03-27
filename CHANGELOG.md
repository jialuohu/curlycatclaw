# Changelog

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
