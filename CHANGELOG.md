# Changelog

## [0.33.2] - 2026-04-08

### Changed
- **Health endpoint port**: changed from 8080 to 18080 to avoid conflicts with common services.

## [0.33.1] - 2026-04-08

Bugfixes for HTTP MCP transport and issue creation UX.

### Fixed
- **HTTP MCP proxy in CLI mode**: config servers with `transport = "http"` were silently dropped by the MCP subprocess. Now proxied correctly via `ConnectHTTPAndRegister`.
- **`_user_context` injection**: external MCP servers (Google Maps) rejected unknown fields. Internal servers still get user context, external servers don't.
- **Duplicate Telegram messages**: streaming timeout caused the post-stream fallback to re-send the response. Now tracks delivery at enqueue time.

### Added
- **Issue creation confirmation gate**: `issue_write` calls return a draft preview and require `confirmed=true` before submitting to GitHub. Prevents accidental issue creation.
- **Dockerfile detection in install workflow**: Claude now detects container-only MCP servers and suggests `docker run` + HTTP transport config.
- **GitHub PAT scope guidance**: setup wizard explains required token permissions and recommends read+write for issue creation.
- **README Quick Start simplified**: detailed Docker instructions moved to `docs/docker.md`.

## [0.33.0] - 2026-04-08

Remote MCP servers, GitHub issue creation, and diagnostics. curlycatclaw can now connect to remote MCP servers over HTTP and create GitHub issues directly from Telegram.

### Added
- **Streamable HTTP transport**: set `transport = "http"` in `[[mcp.servers]]` to connect to remote MCP servers. Supports custom auth headers with credential encryption.
- **Google Maps MCP** (experimental): place search, weather, and directions via Google's managed MCP server. Add your API key and go.
- **Per-server shutdown timeout**: each MCP server gets 5 seconds to close gracefully. One hung connection no longer blocks the rest.
- **GitHub issue templates**: YAML-based forms for bug reports (with severity dropdown) and feature requests. Template chooser links to Telegram bot for non-technical testers. Blank issues disabled.
- **PR template**: Structured pull request template with summary, changes, testing checklist, and related issues sections.
- **CONTRIBUTING.md**: Minimal contribution guide with two paths (GitHub forms or Telegram bot), quickstart for GitHub integration setup, and example prompt for testers.
- **`capture_diagnostics` skill**: Bug reports now include version, MCP server status, recent errors, and health checks automatically. Credentials are never exposed. If a health check times out, the report continues without it.
- **GitHub issue creation guidance**: System prompt detects write-enabled GitHub MCP (via `create_issue` tool presence) and guides Claude through conversational issue creation with user confirmation.
- **Startup warning**: Logs a warning when GitHub MCP is registered but in read-only mode.
- **`[github]` config section**: Owner/repo settings for issue creation (defaults to jialuohu/curlycatclaw).

### Changed
- **MCP Manager architecture**: `startServer()` split into `startStdioServer()` and `startHTTPServer()` helpers for clean transport separation. Added `ServerNames()` and `IsRegistered()` methods for diagnostics integration.
- **Config validation**: transport-aware rules. HTTP servers require `url` (http/https only), stdio servers require `command`. Mixed configs are rejected.
- **Store**: Added `RecentToolErrors()` and `RecentToolCallsByUser()` query methods with user scoping and 24-hour window.
- **config.toml.example**: Expanded GitHub MCP comments with step-by-step write-mode setup instructions. Default remains `--read-only`.

## [0.32.0] - 2026-04-08

Interactive setup wizard. Claude Code now walks you through every config option instead of generating a rigid default.

### Added
- **Interactive config generation**: `/setup` Step 6 replaced with a conversational wizard. Three profiles: Quick start (essentials only), With memory (recommended, with "use defaults" fast-path), Full customization (every feature). Deployment-aware paths, dependency gating, config backup on overwrite, and post-write validation.
- **`--validate-config` CLI flag**: validates config.toml and exits. Safety net for the setup wizard and CI pipelines.

### Changed
- **Setup introduction**: now previews the three config profiles so users know what to expect.
- **config.sh deprecated**: shell script preserved as fallback for non-interactive setups.
- **Default model**: `claude-opus-4-6` (was `claude-sonnet-4-6-20250514`, which the CLI didn't recognize).
- **Default thinking effort**: `medium` recommended (was `high`).

### Fixed
- **Phantom reminders after cancel**: cancelling a reminder in CLI mode updated the database but the main process's scheduler kept firing it. Now `pollNewReminders` detects cancelled jobs and removes them, and `fireReminder` re-checks DB status before sending.
- **CLI path in Docker**: setup wizard now uses `/usr/local/bin/claude` (container path) instead of the host machine path.
- **OAuth token whitespace**: pasted tokens with embedded newlines or spaces are now stripped.
- **GWS credentials on headless servers**: you can now paste the JSON content directly instead of providing a file path.

### Removed
- **`confirm_tools` config option**: was broken in CLI mode (MCP tool name prefix never matched). Not needed when running in a container.

## [0.31.5] - 2026-04-08

Better post-update UX. "All systems operational" now means it.

### Fixed
- **Premature "operational" notification**: the post-update message fired after a fixed 5-second sleep, before the bot was fully ready. Users sent messages that failed. Now polls the health endpoint until it returns 200 before notifying.

## [0.31.4] - 2026-04-08

Built-in Docker healthcheck. Works on any base image without external tools.

### Fixed
- **Docker healthcheck**: replaced `curl`-based healthcheck with `curlycatclaw --health-check` flag. The previous healthcheck failed on distroless images (no curl binary), causing Docker to report containers as "unhealthy" even though the app was running fine.

## [0.31.3] - 2026-04-08

Full-featured GHCR release image. Chat, MCP tools, and browser automation now work in production mode.

### Changed
- **Release Docker image**: switched from distroless (~50MB) to full Debian image (~1.2GB) matching the dev Dockerfile. Includes Node.js, Claude CLI, Playwright+Chromium, GitHub CLI, GWS CLI, Python/UV. The distroless image couldn't run Claude CLI mode, making chat non-functional after self-update.

## [0.31.2] - 2026-04-08

Fix secrets not available after self-update/rollback.

### Fixed
- **401 after update/rollback**: `env_file` path (`~/.curlycatclaw/env`) didn't resolve inside the updater sidecar when it recreated containers. Replaced with `environment` + `${VAR}` substitution from `.env` in the project directory, which is accessible via the compose-project mount.
- **Stale /status**: `/status` now does a fresh GHCR check instead of returning cached state.
- **HOME env in base compose**: moved from dev override to base so prod mode works.

## [0.31.1] - 2026-04-08

Fixes for production mode self-update. The GHCR pull flow now works end-to-end.

### Fixed
- **Compose project mount**: updater sidecar needs `docker-compose.yml` mounted in both prod and dev, not just dev override
- **Config path mismatch**: GHCR image defaults to `/etc/curlycatclaw/config.toml` but compose mounts at `/data`. Added explicit `command` override.
- **Permission denied on GHCR image**: distroless `nonroot` user (UID 65532) couldn't read host-owned config files. Compose now runs as host user's UID.
- **"vunknown" version display**: GHCR images had no OCI version label. GoReleaser now sets `org.opencontainers.image.version`. Fallback shows short digest instead of "unknown".
- **deploy/ directory**: removed stale redirect file left from v0.31.0 compose merge
- **.clawhub/ directory**: removed plugin lock file from repo, added to .gitignore
- **COMPOSE_PROFILES UX**: `.env` file pattern replaces ugly CLI prefix. Setup skill creates it automatically.

## [0.31.0] - 2026-04-07

Unified docker-compose and smart embedding setup. One compose file for everyone, GPU-aware embedder selection during first-time setup.

### Added
- **Unified docker-compose.yml**: single compose file for both production (pulls GHCR images) and development (override file adds local build). No more separate `deploy/docker-compose.yml`.
- **docker-compose.override.yml**: dev-only file that adds `build:` directives, auto-merged by Docker Compose when present. Gitignored.
- **GPU detection in setup**: detects NVIDIA (`nvidia-smi`), AMD (`rocm-smi`/`lspci`), Apple Silicon, and Intel iGPU. Recommends the best embedding engine based on hardware.
- **Smart embedding selection**: auto-selects Ollama for GPU users, FNV for no-GPU users, with manual override to any of the three options (Ollama/FNV/Voyage).
- **Ollama profile**: `COMPOSE_PROFILES=ollama` enables the Ollama service. Not a hard dependency, so FNV/Voyage users skip it entirely.
- **Updater profile**: `COMPOSE_PROFILES=updater` enables the self-update sidecar. Requires UPDATER_SECRET configuration.
- **Generated GWS skill folders gitignored**: 95 auto-generated `skills/gws-*/` directories no longer clutter `git status`.

### Changed
- `deploy/docker-compose.yml` replaced with redirect comment pointing to root compose file
- Setup skill (`config.sh`) generates embedder-specific vector config based on hardware detection
- `.dockerignore` expanded to exclude dev tools, generated skills, compiled binaries

## [0.30.1] - 2026-04-07

Bug fixes and security hardening for the self-update system, found by automated agent audits.

### Fixed
- **Data race**: `LatestVersion` read without mutex lock during update marker write
- **Rollback state**: rollback didn't persist `Updating=true` to disk before starting
- **Docker inspect**: `getCurrentDigest` inspected container ID instead of image ID for `RepoDigests`
- **Rollback client**: returned wrong type for async 202 response, causing false failure messages
- **Blocking sends**: auto-update cron used blocking channel sends that could stall gocron
- **Shutdown context**: cancelled parent context leaked into sidecar HTTP calls during shutdown
- **Compose env override**: `UPDATER_SECRET` in environment block overrode env_file value with empty string
- **Stop grace period**: Docker default 10s was too short for 30s drain timeout, causing SIGKILL mid-shutdown
- **Update history**: grew unbounded, now capped to 50 entries

### Security
- **Command injection**: validate `SERVICE_NAME` env var with regex to prevent targeting other compose services
- **Path traversal**: validate `STATE_PATH` resolves under `/data/` to prevent arbitrary file writes
- **Docker CLI injection**: validate digest and image ref strings before passing to `docker tag`/`docker inspect`
- **OOM prevention**: bound HTTP response body to 1 MiB in update client before JSON decoding
- **Symlink race**: master key temp file now uses per-PID path instead of predictable fixed path

### Changed
- Stale docs fixed: architecture diagrams, embedder dimensions, actor names, ingest pipeline description
- Generated GWS skill folders (95 dirs) added to `.gitignore` and `.dockerignore`
- Config code comments corrected for embedder defaults (ollama/bge-m3/1024d)

## [0.30.0] - 2026-04-07

Self-update system. Tell the bot `/update` in Telegram and it pulls the latest Docker image and restarts itself. No SSH, no terminal. Optional auto-update on a schedule with rollback on failure.

### Added
- **Updater sidecar**: new `curlycatclaw-updater` container manages Docker lifecycle. Holds the Docker socket so the main container stays unprivileged. Communicates via authenticated HTTP API on the internal Docker network.
- **`/update` command**: check for new version, confirm via inline keyboard, update with a single tap. Returns 202 Accepted immediately, sends "I'm back" notification after restart.
- **`/status` command**: shows current version, uptime, and whether an update is available.
- **`/rollback` command**: revert to a previous image (keeps 3 previous digests). Confirmation via inline keyboard.
- **Auto-update cron**: opt-in scheduled updates via `[update]` config section. Checks for active conversations before applying. Skips if someone is mid-conversation.
- **Post-update notification**: detects version change on startup and notifies all allowed users.
- **GHCR digest checking**: anonymous token negotiation with OCI label parsing for human-readable version strings.
- **Rollback safety**: digest blacklist (24h TTL) prevents retry loops on broken images. Stale update lock recovery (10min timeout).

### Changed
- Health endpoint now binds to `0.0.0.0:8080` (was `127.0.0.1`) so the updater sidecar can reach it across the Docker network.
- Deploy compose Qdrant bumped from v1.14.0 to v1.17.1 to match dev environment.
- Deploy compose image uses `${CURLYCATCLAW_IMAGE}` env var for rollback override support.

### Security
- Shared secret authentication (constant-time comparison) between main container and updater sidecar.
- Docker socket isolated in sidecar only, never in main container.

## [0.29.1] - 2026-04-07

Bug fixes for MCP server crash, eval scoring accuracy, and Qdrant version mismatch.

### Fixed
- **MCP server crash**: `backfill_days = -1` (unlimited backfill) failed config validation, crashing the curlycatclaw-skills MCP server subprocess and making `set_reminder` and all other built-in tools invisible to Claude
- **Eval score inflation**: `/effort` and `/retry` commands logged with empty conversation ID, causing the time-window fallback to attribute events to all overlapping conversations
- **Qdrant version mismatch**: Go client v1.17.1 was 3 minor versions ahead of server v1.14.0, triggering compatibility warnings
- **Gmail backfill cursor**: cursor now advances to newest email date after each batch, preventing re-scanning of already-processed emails

### Changed
- `RemoteTrigger` added to `--disallowedTools` for CLI subprocess, preventing Claude from using it as a scheduling fallback
- Qdrant Docker image bumped from v1.14.0 to v1.17.1

## [0.29.0] - 2026-04-06

Persistent CLI subprocesses for ingest extraction. Email processing drops from ~12s to ~0.6s per email by reusing a long-lived Claude process instead of spawning a new one each time. Separate processes for trusted and untrusted content prevent cross-contamination. Gmail account filtering lets you choose which accounts to ingest from.

### Added
- **PersistentCLISender**: reuses CLI subprocesses for ingest extraction, amortizing Node.js startup + OAuth overhead across batches
- **Trust-level separation**: untrusted emails and trusted notes route to separate persistent processes with proper system prompts
- **SafeMode spawn**: new `SpawnParams.SafeMode` omits `--dangerously-skip-permissions` for background extraction processes handling untrusted content
- **Gmail account filtering**: `accounts` field in `[[ingest.sources]]` config restricts which Google accounts are ingested
- **MCP account discovery fix**: `DiscoverGmailAccounts` now unwraps the MCP text envelope, enabling multi-account discovery

### Changed
- Ingest extraction model configurable via `extraction_model` (defaults to `claude-haiku-4-5` instead of main model)
- Dockerfile NodeSource install uses GPG key + apt repo instead of curl-pipe-bash

### Fixed
- Trust routing defaults to untrusted (safe default) instead of routing unknown prompts to trusted process
- MCP SDK upgraded v0.8.0 -> v1.4.1 (fixes cross-site tool execution vulnerability GO-2026-4773)
- golang.org/x/net upgraded v0.50.0 -> v0.52.0 (fixes HTTP/2 panic GO-2026-4559)

## [0.28.0] - 2026-04-05

Generic knowledge source ingest framework. curlycatclaw can now ingest knowledge from Gmail, Obsidian vaults, and Notion workspaces through a single pluggable pipeline. Each source has its own cursor, daily caps, and trust level. Adding a future source is config, not code.

### Added
- **Generic ingest pipeline**: `internal/ingest/` package with Source interface, per-source scheduling, and extraction
- **GmailSource**: multi-account Gmail via GWS MCP (replaces dedicated EmailIngestActor)
- **FileSource**: Obsidian vault ingestion via directory walker with mtime cursor, YAML front matter parsing, and symlink escape prevention
- **NotionSource**: Notion workspace via official MCP server
- **Trusted/untrusted extraction prompts**: email (untrusted) blocks preference/commitment types at validation layer; personal notes (trusted) allow all types with wiki-link entity extraction
- **Passthrough extractor**: structured Obsidian notes with YAML front matter skip LLM entirely, parsed directly into observations
- **Hybrid extractor**: passthrough for notes with front matter, LLM fallback for notes without
- **Content fingerprint tracking**: mutable sources (Obsidian, Notion) detect edits and re-extract changed content
- **`[[ingest.sources]]` config**: per-source name, type, interval, trust level, extraction mode, daily caps, and prefilter rules

### Changed
- **Config**: `[email_ingest]` deprecated but auto-migrates to first `[[ingest.sources]]` entry for backward compatibility
- **SQLite schema**: `email_ingest_state` and `email_processed_messages` renamed to generic `ingest_state` and `ingest_processed_items` with source+partition keys; legacy data auto-migrated
- **Stale state recovery**: ingest actor resets stuck "running" states on startup (prevents permanent source skipping after crash)

### Removed
- **`internal/email/` package**: replaced entirely by `internal/ingest/`

## [0.27.1] - 2026-04-05

### Added
- Test coverage for Gmail JSON parsing (array and wrapped object formats, fallback handling)
- Test coverage for eval failure miner (correction, retry, effort override clustering, tool error grouping, output truncation)
- Test coverage for eval reporter (formatting, WARN/OK markers, list truncation, full channel handling)
- Test coverage for eval export extractText (string, content blocks, mixed blocks, invalid JSON)

## [0.27.0] - 2026-04-05

Background email-to-observation processing. curlycatclaw now automatically reads your Gmail inbox, filters out noise, and extracts valuable information into durable observations. Email context becomes ambient knowledge the agent can reference in any future conversation.

### Added
- **EmailIngestActor**: supervised background actor that processes Gmail via existing GWS MCP tools
- **Two-stage filter**: cheap Gmail label/sender prefilter removes 60-80% of volume before LLM triage
- **Incremental sync**: polls for new emails every N minutes (default 15), deduplicates via processed messages table
- **Resumable backfill**: date-range windowed historical import with cursor persistence, configurable depth (default 30 days)
- **Cost controls**: per-account daily observation cap (100), LLM call circuit breaker (200/day), 7-day processed message cleanup
- **Prompt injection defense**: `preference` and `commitment` observation types blocked at validation layer for email-sourced content
- **`[email_ingest]` config section**: enabled, interval, backfill depth, batch size, label filters, sender skip patterns
- **SQLite tables**: `email_ingest_state` (per-account cursor/stats), `email_processed_messages` (dedup with retry separation)
- **Account discovery**: uses `gws_list_accounts` MCP tool at startup for multi-account support

### Changed
- **`store.go`**: added `EnsureConversation` helper for FK-safe synthetic conversation rows
- **`main.go`**: EmailIngestActor wired into supervisor alongside ReminderActor

## [0.26.1] - 2026-04-05

Self-evaluation pipeline foundation and Telegram library modernization. The bot can now track interaction events, receive reactions, and run background evaluation on conversation quality.

### Added
- **Eval pipeline (Phase 0+1)**: EvalActor with gocron scheduler, ConversationScorer (deterministic signals), FailureMiner (failure clustering), TelegramReporter (plain text summaries)
- **Interaction event logging**: `/effort` and `/retry` commands persisted as events for eval scoring
- **Telegram message mapping**: bot response messages mapped to conversations for reaction joining
- **Reaction support**: Telegram thumbs up/down reactions captured and stored with dedup
- **Eval export CLI**: `--eval-export` flag for manual conversation quality labeling
- **EvalConfig**: `[eval]` config section with schedule, lookback, threshold settings
- **New SQLite tables**: interaction_events, telegram_message_map, eval_reactions, eval_runs, failure_clusters, memory_candidates, eval_scores
- **Time-based indexes** on messages.created_at and tool_calls.created_at for eval scanning

### Changed
- **Telegram library migrated**: go-telegram-bot-api/v5 (dead, Dec 2021) replaced with go-telegram/bot v1.20.0 (Bot API 9.5)
- **Handler-based architecture**: bot.Start(ctx) with defaultHandler dispatching messages and reactions
- **context.Context on all Telegram API calls**
- **MessageStore interface extended** with eval methods (LogInteractionEvent, MapTelegramMessage, LogEvalReaction)

## [0.26.0] - 2026-04-05

Multi-account Google Workspace. Use multiple Google accounts through a single GWS MCP server with per-account service restrictions. Claude picks the right account for each operation.

### Added
- **Multi-account GWS support**: configure multiple Google accounts via `GWS_ACCOUNT_*` env vars instead of duplicating MCP server entries
- **Per-account service filtering**: `GWS_ACCOUNT_<NAME>_SERVICES` restricts which Google services each account can access (e.g., Gmail only for secondary accounts)
- **`gws_list_accounts` tool**: Claude can query available accounts, their default, and service permissions
- **Account parameter on all GWS tools**: optional `account` field on every tool for explicit account selection
- **`send_file` skill**: Claude can send files (documents, exports, reports) to the user in Telegram

### Changed
- **`ResolveAccount` returns resolved name**: callers always know which account was selected (needed for service validation)
- **`ExecuteHelper`/`ExecuteAPI` accept env overrides**: per-call `GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE` for account switching
- **`run()` builds deduped `cmd.Env`**: parent env vars overridden by account-specific values without duplicates
- **`reservedFlags` includes `account`**: defense-in-depth prevents `--account` from leaking as a gws CLI flag

## [0.25.0] - 2026-04-05

Thinking effort control. Configure Claude's reasoning depth via config or Telegram commands. Higher effort means deeper thinking on complex tasks.

### Added
- **`thinking_effort` config field**: set reasoning depth globally (`low`, `medium`, `high`, `max`) in `[claude]` section
- **`/effort` Telegram command**: override thinking effort per session (`/effort max`, `/effort reset`)
- **`/retry` Telegram command**: replay the last message at a specified effort level (`/retry high`)
- **Extended thinking support (direct API)**: maps effort levels to `ThinkingConfigParamOfEnabled` with budget token presets (10K for high, 32K for max) and model-aware MaxTokens cap (128K)
- **CLI subprocess effort**: passes `--effort` flag to spawned `claude` CLI processes
- **CLI respawn on effort change**: `/effort` kills and respawns the CLI subprocess so the new effort takes effect immediately
- **Environment variable override**: `CURLYCATCLAW_THINKING_EFFORT` overrides the config value
- **Thinking block signatures**: captured from API responses for conversation history continuity (signatures only, not full thinking text)

### Changed
- **`NewCLIManager`**: now accepts an `effort` parameter for default effort level
- **`SendParams`**: includes `ThinkingEffort` field for per-request effort control
- **`toolUseLoop`**: threads effort through all iterations of the tool use loop
- **Smart message splitting**: long responses now split at paragraph boundaries instead of mid-text, and code blocks are closed/reopened across splits so both messages render correctly
- **Telegram bot commands**: `/effort`, `/retry`, `/project` registered via `setMyCommands` for autocomplete

### Fixed
- **Thinking blocks in tool loop**: thinking block signatures now included in conversation history during multi-turn tool calls (without this, extended thinking + tools would fail)
- **Redacted thinking blocks**: `redacted_thinking` content blocks handled for conversation continuity when model reasoning is filtered
- **Rate-limited HTML edits**: final HTML-formatted message edit retries once after Telegram 429, preventing raw markdown display
- **`/effort` and `/retry` prefix matching**: tightened to exact match + space, preventing interception of messages like "/effortlessly"
- **`/retry` override restore**: one-shot effort override now properly restores previous session effort instead of deleting it
- **Voice STT response limit**: capped at 1 MiB to prevent memory exhaustion from malicious API responses
- **Missing `rows.Err()` checks**: added to observation fact iteration and reminder polling to catch silent database errors
- **Reminder input limits**: message capped at 2000 characters, prompt at 5000 characters

## [0.24.0] - 2026-04-04

Self-healing memory Phase 3. Observations can now be superseded, archived, restored, and updated. Stale project_state observations are automatically detected during extraction and filtered from future conversations. Users get inline notifications when memory changes and can undo with simple commands.

### Added
- **Supersession wiring**: extraction pipeline now detects when new observations supersede existing project_state observations, creating relations with confidence scores
- **Superseded observation filtering**: search results exclude observations that have been superseded with confidence >= configured threshold (default 0.8)
- **`supersede_observation` skill**: Claude calls this when it detects a user correcting outdated information in conversation, creating a replacement observation and archiving the old one
- **`restore_observation` skill**: restores a previously archived observation, bringing it back into search results
- **`update_observation` skill**: edit title, summary, type, or importance of an existing observation (FTS5 index updated via trigger; Qdrant vector unchanged until next extraction)
- **Soft delete**: `forget_observation` now archives instead of permanently deleting, with restore capability
- **Inline memory notifications**: Telegram notification when extraction creates supersession relations, with `/keep_both`, `/revert`, `/forget_old` commands
- **System prompt instruction**: Claude is instructed to call `supersede_observation` when user corrections match injected observations
- **FTS5 UPDATE trigger**: observations_fts index stays in sync when observations are updated

### Changed
- **`GetSupersededObservationIDs`**: uses `confidence >= threshold` instead of `confirmed = 1` (which was never set), with graceful degradation on DB error
- **`AddObservationRelation`**: uses `INSERT OR IGNORE` for concurrency safety, clamps confidence to [0.0, 1.0]
- **`SupersessionThreshold` default**: changed from 0.85 to 0.8
- **`get_observation` skill**: now returns backing facts alongside title and summary
- **Observation queries**: 8 queries updated with `archived_at IS NULL` filter to exclude soft-deleted observations
- **`Extract()` return type**: now returns `([]Observation, []ExtractedRelation, error)` with relation collection

### Fixed
- **`GetSupersededObservationIDs` was a no-op**: the `confirmed = 1` filter never matched because `confirmed` was never set to 1. Now uses configurable confidence threshold.

## [0.23.0] - 2026-04-04

Observation memory Phase 2. Your bot can now find memories by keyword, track who and what was discussed, show a compact memory index instead of dumping everything, and detect when newer observations supersede older ones.

### Added
- **FTS5 hybrid search**: keyword search via SQLite FTS5 virtual tables alongside vector search, merged with Reciprocal Rank Fusion (RRF, k=60). Includes `EscapeFTS5Query` for input safety and `RebuildFTS` for index maintenance
- **Three new observation types**: `commitment` (promises/follow-ups), `discovery` (things learned about user's world), `reference` (external resources mentioned)
- **Entity extraction**: people, projects, files, and tools extracted alongside observations with canonicalized names, stored in `observation_entities` table with FTS5 search
- **`search_entities` skill**: keyword search for entities across observations ("what do I know about X?")
- **Multi-vector indexing**: each observation fact gets its own Qdrant point with deterministic text-hash IDs, per-parent cap in retrieval, and `RebuildObservationIndex` for drift reconciliation
- **Progressive 3-layer retrieval**: compact index (top 15 titles) + expanded view (top 3 with facts) + on-demand full detail via `get_observation`. Config-gated via `progressive_retrieval`
- **Observation relations**: advisory supersession system with `observation_relations` table supporting supersedes/refines/contradicts relation types, IDOR-protected, used for ranking boost (not hiding)
- **Embedding migration integration**: `ObservationVersionedName`, observation dual-write during migration, `ObservationTextsAfter` for backfill
- **Retrieval instrumentation**: slog metrics for search quality (top scores, dedup rates), extraction stats (type distribution, importance), and injection stats

### Changed
- **ObservationsConfig**: five new fields (`hybrid_search`, `supersession_threshold`, `progressive_retrieval`, `compact_limit`, `expanded_limit`) with sensible defaults
- **DeleteObservation**: now cascades to entities and relations in the same transaction
- **Extraction prompt**: expanded with entity extraction guidance and examples for all 6 observation types
- **Fact dedup**: reuses facts fetched for Tier 1 injection instead of redundant DB query

### Fixed
- **MCP skill registration**: observation skills (search, list, get, forget, search_entities) now registered in MCP server subprocess so Claude CLI can discover and call them
- **Extraction prompt**: strengthened JSON-only instruction for reliable parsing with smaller models (Haiku)
- **search_entities output**: now returns full observation UUIDs instead of truncated 8-char IDs that get_observation rejected
- **search_observations output**: now includes observation ID so the bot can follow up with get_observation
- **Qdrant dimension auto-fix**: detects collection dimension mismatch on startup, deletes stale collection, and reindexes all observations from SQLite with correct embedder
- **Observation reindex**: loads full observations (with importance, type, facts) from SQLite instead of migration-text format that lost metadata
- **Per-spawn model override**: SpawnParams.Model allows extraction, summarization, and per-reminder model selection in CLI mode

## [0.22.0] - 2026-04-04

Observation memory system (Phase 1). The bot now automatically captures decisions, preferences, and project state from conversations and injects them into future sessions. No manual `/remember` needed.

### Added
- **Observation extractor**: async Claude-based extraction runs every 3 user turns (or after 5-min idle gaps), producing structured observations with title, summary, facts, and importance score
- **Three observation types**: `decision`, `preference`, `project_state` captured automatically from conversation flow
- **Qdrant observation search**: new `curlycatclaw_observations` collection with re-ranking by recency, importance, and vector similarity
- **System prompt injection**: relevant observations injected as "What I remember" section between user facts and conversation summaries
- **Four observation skills**: `search_observations`, `list_observations`, `get_observation`, `forget_observation` (all IDOR-protected)
- **Parallel Qdrant queries**: observation and summary searches run concurrently via sync.WaitGroup, no latency increase
- **FormatTranscriptWithLimit**: configurable char-limit extraction from existing FormatTranscript for observation and summarization use

### Changed
- **ObservationsConfig**: new `[memory.observations]` config section with extraction_interval, extraction_model, retrieval_limit, and score_threshold
- **Actor wiring**: in-memory turn counter avoids per-message DB writes, CAS lock prevents concurrent extraction, dedicated 3-slot semaphore bounds extraction goroutines
- **Content dedup**: SHA-256 hash scoped to (user_id, content_hash) with unique DB constraint
- **LLM output validation**: type whitelist, rune truncation, importance clamping, code fence stripping, control char replacement (spaces instead of stripping)
- **Memory dedup**: when observations are enabled, Claude is instructed to use `remember_fact` only for stable identity facts (name, role, timezone), not decisions or project state. Observations whose titles overlap >60% with existing facts are filtered from injection to avoid redundancy.

### Fixed
- **SaveObservation pointer**: observation IDs now propagate correctly to Qdrant indexing (was causing all observations to overwrite the same vector point)
- **Lazy collection creation**: replaced `sync.Once` with mutex+bool retry pattern so transient Qdrant failures don't permanently disable observation indexing

## [0.21.1] - 2026-04-04

Telegram media foundation and typing indicator. The bot now shows "typing..." while Claude thinks, and the message system supports documents, voice, and audio attachments (processing comes in a follow-up release).

### Added
- **Typing indicator**: Telegram shows "typing..." while Claude processes your message, refreshing every 4.5 seconds during long tool-use loops
- **Generic Attachment type**: unified media handling replaces the per-type Photo field, supporting photos, documents, voice messages, and audio files
- **Document/voice/audio download**: Telegram channel now downloads documents, voice messages, and audio files (processing by Claude comes next)
- **SendTyping and SendDocument**: new methods on the Telegram transport interface for goroutine-safe typing actions and file sending

### Changed
- **Message model**: `IncomingMessage.Photos` replaced with `IncomingMessage.Attachments` (backward-compatible `Photos()` method provided)
- **File download**: extracted generic `downloadFile()` from photo-specific `downloadPhoto()`

## [0.21.0] - 2026-04-04

GitHub MCP server integration. curlycatclaw can now interact with GitHub repos, PRs, CI, issues, and releases via Telegram using GitHub's official MCP server.

### Added
- **GitHub MCP server**: integrated `github-mcp-server` (github/github-mcp-server v0.32.0) as an external MCP server via `[[mcp.servers]]` config, with default toolsets: repos, issues, pull_requests, actions, users
- **GitHub workflow guidance**: system prompt dynamically lists available GitHub tools when the GitHub MCP server is configured, guiding Claude toward high-value dev workflows
- **Docker support**: both `Dockerfile` and `Dockerfile.goreleaser` download and include the github-mcp-server binary
- **Security default**: example config uses `--read-only` flag by default to prevent accidental write operations

### Fixed
- **Dockerfile.goreleaser multi-arch**: replaced hardcoded x86_64 architecture in gws download stage with dynamic detection via `uname -m`, enabling arm64 builds

## [0.20.4] - 2026-04-03

Remove unused budget manager. The Haiku-powered context classification system was dead code in CLI mode (the only mode in use). Removes ~1000 lines across 13 files.

### Removed
- **Budget manager**: `BudgetManager`, `BuildContextWithBudget`, `ClassifiedTurn`, and all supporting code (`internal/memory/budget.go`, `budget_test.go`)
- **Budget config**: `[budget]` TOML section, `BudgetConfig` struct, validation logic, config defaults
- **Budget wiring**: `session.New()` budget parameter, `ContextBuilder.SetBudget()`, `ContextProvider` interface method

### Changed
- **Context builder**: `BuildContext()` is now the only context-building path (no budget fork)
- **Session actor**: simplified `New()` signature, direct API mode calls `BuildContext()` instead of `BuildContextWithBudget()`

## [0.20.3] - 2026-04-03

Performance improvements: regex compilation and missing SQLite index.

### Changed
- **Streaming path**: `balancedTagRe` in `hasBalancedTags()` was compiled from the static `telegramTags` list on every call to `ConvertSafe()` (which runs on every streaming `finalFlush`). Now compiled once at package init. All 10 regex patterns in `mdhtml` are now module-level.
- **Summarization queries**: Added missing index on `conversations.summarization_status` for `PendingSummarizations()` and `RecoverableSummarizations()` queries. Column was added via ALTER TABLE migration but never indexed.
- **FNV embedder**: Reuse hash object with `Reset()` instead of allocating per word. Same deterministic output, fewer heap allocations.
- **Master key file**: Skip redundant `os.WriteFile` on every `buildMCPConfig` call. The key is immutable, write once on first message only.
- **Context builder**: Reduce message fetch multiplier from `*20` to `*4`. Loads ~100 messages instead of ~500 when building the 25-turn sliding window.

## [0.20.2] - 2026-04-03

Security hardening: CLI subprocess environment filtering.

### Fixed
- **Environment leak in CLI subprocess**: `spawn()` passed the full daemon environment (`os.Environ()`) to the Claude CLI subprocess, exposing `CURLYCATCLAW_MASTER_KEY`, Telegram tokens, and API keys to the child process and any MCP servers it connects to. Now uses an allowlist matching the pattern used by all other child processes (MCP servers, exec skills, extension proxy). Found by `/cso` security audit.

### Added
- Test verifying secret exclusion from CLI subprocess environment (`TestFilteredSpawnEnv_ExcludesSecrets`)

## [0.20.1] - 2026-04-03

Codebase hygiene pass. Removes dead code, fixes a file descriptor leak, strengthens a path traversal guard, and cleans up inconsistent error handling.

### Fixed
- **File descriptor leak**: `stdout` pipe not closed when `cmd.Start()` fails in CLI subprocess spawn (`internal/claude/subprocess.go`)
- **Path traversal guard**: `filepath.Abs()` error was silently discarded in skill loader, weakening the command-outside-directory check (`internal/skillloader/loader.go`)
- **Inconsistent error handling**: `RowsAffected()` error discarded in `DeleteSummary`, now checked like `DeleteFact` (`internal/memory/store.go`)
- **Misplaced doc comment**: `checkPluginCommand` doc was attached to `makePluginUpdateExecute` (`skills/plugin.go`)
- **Regex recompilation**: `simpleItalicRe` compiled per call instead of once at module level (`internal/mdhtml/convert.go`)

### Removed
- `DefaultMCPExtensions()`, `SkillFilesJSON()` from extension defaults (never called)
- `CreateVersionedCollections()` from vector store (migration uses `CreateCollection` directly)
- `VoyageEmbedder.EmbedQuery()` (all search paths use `Embed()` via interface)
- Custom `contains()`/`containsStr()` test helpers (replaced with `strings.Contains`)
- Unused `delay` variable in reminder scheduling
- Unused `_ int` parameter from migration `swapAliases`
- Stale `// indirect` marker on `cron/v3` dependency

## [0.20.0] - 2026-04-03

Google Workspace integration via gws CLI. Ask your Telegram bot to check your calendar, send emails, or search Drive. A standalone MCP server discovers Google Workspace tools dynamically and proxies them through the gws CLI.

### Added
- **curlycatclaw-gws-mcp**: standalone MCP server bridging Claude to Google Workspace via the gws CLI. Discovers tools dynamically from `gws generate-skills`.
- **Boolean flag detection**: parses `gws --help` output to correctly type boolean flags (e.g. `--html`, `--draft`) in tool schemas. Concurrent detection with bounded workers.
- **Argument injection prevention**: validates positional args with regex, expands reserved flags list to block gws global flags, and filters helper tool input through a server-side flag allowlist.
- **Docker gws support**: both Dockerfiles install the gws CLI binary. Goreleaser image uses multi-stage download.
- **Headless auth flow**: config supports `GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE` for exported OAuth credentials. No keyring needed in containers.
- **28 unit tests** for the gws-mcp package covering discovery, execution, boolean detection, argument validation, and filter matching.
- **Config MCP server proxy**: config-based MCP servers are now proxied through curlycatclaw-skills in CLI mode, so Claude can discover and use them.
- **System prompt tool awareness**: Claude's system prompt lists all available MCP tools, so it uses them proactively instead of saying "I don't have access."
- **Unified capability listing**: asking "what skills?" now shows plugins, extensions, config MCP servers, and built-in skills in one response.
- **Humanizer prompt skill**: pre-installed skill that removes AI writing patterns from text (29 patterns from Wikipedia's AI writing guide).
- **Default extension protection**: pre-installed extensions (scrapling-mcp, scrapling, humanizer) cannot be removed via Telegram.

### Changed
- **Docker-first config**: `config.toml.example` and `docker-compose.yml` use `/data/...` paths exclusively. Removed redundant `CURLYCATCLAW_*` env var overrides from docker-compose.yml.
- **Design spec updated**: `GWS_MCP_FILTER` corrected to `GWS_FILTER` in the integration design doc.

## [0.19.0] - 2026-04-02

Background embedding migration. Switch embedding models without downtime. Change your config, restart, and the system migrates vectors in the background while search keeps working. Ollama with bge-m3 is now the default embedder.

### Added
- **Background embedding migration**: when the embedder config changes, vectors are re-embedded in the background while search continues serving from old vectors. Atomic alias swap when done. Crash-resumable with keyset pagination.
- **Dual-write during migration**: new messages are indexed with both old and new embedders, so nothing is lost during the migration window.
- **Catch-up phase**: after backfill completes, a convergence scan catches any rows created during migration before the alias swap.
- **Ollama as default embedder**: default changed from FNV (offline hash) to Ollama with bge-m3 (1024d). Real semantic search out of the box.
- **Ollama Docker service**: `docker-compose.yml` now includes Ollama with health check and persistent model storage. First run: `docker compose exec ollama ollama pull bge-m3`.
- **Environment variable overrides**: `CURLYCATCLAW_EMBEDDER` and `CURLYCATCLAW_OLLAMA_URL` for Docker deployments.
- **Embedder state tracking**: SQLite `embedder_state` table tracks active embedder, migration progress, and crash recovery state.

### Changed
- **Qdrant collections are now versioned**: `curlycatclaw_messages_v1`, etc. Fixed names become aliases. Enables atomic zero-downtime migration swaps.
- **`--migrate-embedder` CLI updated**: works with versioned collections and aliases. Serves as manual fallback for failed background migrations.
- **Ollama default model**: `nomic-embed-text` (768d) replaced by `bge-m3` (1024d).
- **Config validation**: Ollama embedder config no longer requires explicit `ollama_url` (defaults to `http://localhost:11434`).

## [0.18.0] - 2026-04-02

MCP extension hot-reload. Installing or removing MCP extensions no longer restarts the CLI subprocess, preserving conversation context. Tools appear instantly via MCP protocol notifications.

### Added
- **MCP extension hot-reload**: install or remove MCP extensions without losing your conversation. Tools appear instantly, no subprocess restart needed. Falls back to restart on failure.
- **Zero-downtime env updates**: changing an extension's API key reconnects seamlessly (new session connects before old one closes, so tools never disappear).
- **Stale tool cleanup**: if an extension's tool set changes across reconnections (e.g., version upgrade removes a tool), orphaned tools are automatically unregistered.
- **Conversation history injection**: when the CLI subprocess does restart (plugin install, idle timeout, crash), Claude now remembers your recent conversation from SQLite. No more "I don't have context from a previous chat."

### For contributors
- MCP server creation moved earlier in `runMCPServer()` for hot-reloader initialization ordering.
- Startup MCP extension loading unified through the hot-reloader (same code path as runtime `add_extension`).
- `CLIManager.GetOrCreate()` now returns `isNew bool` for fresh spawn detection.

## [0.17.0] - 2026-04-02

MCP extension proxy and encrypted API key management. Runtime MCP extensions now work reliably in CLI mode, and users can configure API keys via Telegram chat with encryption at rest.

### Added
- **MCP extension proxy**: runtime MCP extensions are proxied through the curlycatclaw-skills subprocess instead of relying on the Claude CLI's --mcp-config (which has a bug discovering dynamic servers). Tools appear with namespaced names (e.g. `paper-search-mcp__search_papers`).
- **Encrypted extension env vars**: `set_extension_env` and `unset_extension_env` skills let users configure API keys for MCP extensions via chat. Values are encrypted at rest using AES-256-GCM via the credential store.
- **Extension registry Update method**: in-place modification of extension metadata with atomic persistence and rollback.
- **MCP extensions in system prompt**: installed MCP extensions listed with descriptions and "prefer these tools" instruction, so Claude uses scrapling/paper-search over spawning subagents.
- **Master key temp file**: master key passed to MCP subprocess via temp file instead of CLI argument to avoid /proc/PID/cmdline exposure.
- **Command auto-splitting**: `add_extension` automatically splits command strings with spaces when no args provided (handles Claude passing "uvx foo" as one string).
- **Dangerous env key validation**: `set_extension_env` rejects library-injection env keys (LD_PRELOAD, DYLD_*) at set time.

### Changed
- **docker-compose.yml**: added `env_file` support for loading `CURLYCATCLAW_MASTER_KEY` from `~/.curlycatclaw/env` (gitignored).
- **buildMCPConfig**: runtime MCP extensions removed from --mcp-config (proxied instead).
- **System prompt**: added instructions for `list_extensions` verification and `set_extension_env` usage.

### Fixed
- **Extension env override**: `buildMCPExtEnv` now prevents extension env vars from overriding baseline vars (PATH, HOME).
- **Zero-tool extension**: proxy skips and closes extensions that discover zero tools instead of keeping the process alive.
- **30s connect timeout**: prevents a hanging MCP extension from blocking all tools.

## [0.16.1] - 2026-04-02

Docker and skill infrastructure improvements. Scrapling MCP server and agent skill pre-installed by default for AI-powered web scraping.

### Added
- **Scrapling MCP server**: pre-installed as default extension, 9 scraping tools available immediately for AI-powered web scraping with server-side extraction (fewer tokens, faster).
- **Scrapling agent skill**: prompt skill with full framework reference, examples, and MCP server docs, auto-downloaded from GitHub on first startup.
- **Default extension pre-seeding**: `EnsureDefaults()` in `internal/extension/defaults.go` handles first-run setup for built-in extensions (download skill files, register MCP servers).
- **GitHub CLI in Docker**: `gh` now available inside the container for authenticated GitHub API access.
- **Remote skill import hints**: system prompt teaches Claude to install skills from GitHub URLs (sparse checkout for subdirectories) and ClawHub (`npx clawhub@latest`).
- **Extension skill logging**: all add/remove/load operations now logged at INFO level with name, type, and path.

### Changed
- **Docker Node.js upgraded**: v18 → v22 (NodeSource) so ClawHub CLI and modern npm packages work.
- **Standard plugins updated**: added frontend-design, code-review, code-simplifier, security-guidance, ralph-loop, serena. Removed LSP plugins and playground.
- **docker-compose.yml**: removed redundant `CURLYCATCLAW_CLI_PATH` env override (config file now has correct path).

### Fixed
- **Claude CLI path in Docker**: NodeSource installs npm globals to `/usr/bin/`, added symlink to `/usr/local/bin/claude` so existing configs work.

## [0.16.0] - 2026-04-01

Runtime extension registry and plugin system overhaul. Add and remove MCP servers and exec-based skills through Telegram chat, no config edits or restarts needed. Plugins are now unrestricted and pre-installed automatically.

### Added
- **Extension registry**: `add_extension`, `remove_extension`, `list_extensions` skills let you manage MCP servers and exec skills at runtime via Telegram. Persisted to `extensions.json`, survives restarts.
- **Dynamic MCP management**: `AddServer`/`RemoveServer` methods on the MCP manager enable runtime addition and removal of MCP servers.
- **Standard plugin pre-installation**: context7, playwright, ui-ux-pro-max, superpowers, claude-md-management, hookify, and skill-creator are auto-installed on first startup.
- **Two built-in marketplaces**: `anthropics/claude-plugins-official` and `nextlevelbuilder/ui-ux-pro-max-skill` are bootstrapped automatically and cannot be removed.
- **Env var filtering**: MCP extension environment variables are filtered to block `LD_PRELOAD`/`DYLD_*` injection vectors.
- **Extension system prompt**: Claude is directed to use `add_extension`/`remove_extension` instead of manually editing `.mcp.json` files.
- **Prompt-based skills**: new `type=prompt` extension for markdown instruction files (SKILL.md). `load_prompt_skill` skill reads instructions on demand. System prompt lists available prompt skills for discovery.
- **Smart CLI tool import**: system prompt teaches Claude the exec JSON protocol and wrapper generation workflow. Non-conforming CLI tools get auto-wrapped.

### Changed
- **Plugin allowlist removed**: any plugin can now be installed via chat. The `allowed_plugins` config field is gone, replaced by hardcoded standard plugins that auto-install on first startup.

### Fixed
- **Extension file permissions**: `extensions.json` written with 0600 (not 0644) since it may contain API keys.
- **Extension removal ordering**: persistence is updated before runtime state, preventing inconsistency if disk write fails.
- **Extension name validation**: 128-character limit and `json.Valid()` check on input schemas.
- **Streaming message split rendering**: long messages that overflow Telegram's 4096-char limit now get proper HTML conversion before being sealed, instead of showing raw markdown.

## [0.15.0] - 2026-04-01

Full plugin and marketplace management via Telegram. Add third-party marketplaces, update plugins, and the bot auto-searches for missing marketplaces when you try to install something new.

### Added
- **Marketplace management skills**: `add_marketplace`, `remove_marketplace`, `list_marketplaces` let you manage plugin sources via Telegram. Remove auto-uninstalls plugins from that marketplace. Default marketplace (claude-plugins-official) is protected.
- **Plugin update skill**: `update_plugin` updates a specific plugin or all installed plugins. Uses full `name@marketplace` keys for reliable updates.
- **Lazy plugin auto-update**: stale plugins (>7 days since last update) are automatically updated when you install a new plugin. Non-blocking, failures logged.
- **Autonomous marketplace discovery**: when a plugin isn't found, the bot searches the web for its marketplace repo, adds it, and retries the install automatically.
- **Skills include plugins**: when you ask "what can you do?", the bot includes installed plugins in its answer.

### Fixed
- **Bulk plugin update**: uses full `name@marketplace` keys instead of stripped names. Previously failed because the CLI needs the marketplace qualifier.

## [0.14.5] - 2026-04-01

### Changed
- **Telegram formatting guidance in system prompt** — Claude now uses bullet points and lists instead of markdown tables. Tables render poorly on mobile Telegram, so the system prompt tells Claude to avoid them.

## [0.14.4] - 2026-04-01

### Changed
- **Skip redundant web_search in CLI mode** — the CLI subprocess already has a built-in WebSearch tool. The custom DuckDuckGo-based `web_search` MCP skill is now only registered in direct API mode where no CLI is available.

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
