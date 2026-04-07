# Architecture

## System Overview

```
┌────────────────────────────────────────────────────────────┐
│                       Supervisor                           │
│            (panic/recover, backoff, 30s drain)             │
│                                                            │
│  ┌──────────┐   ┌───────────┐   ┌───────────┐   ┌──────────┐│
│  │ Channel  │◄─►│  Session  │   │ Reminder  │   │  Email   ││
│  │  Actor   │   │   Actor   │   │   Actor   │   │  Ingest  ││
│  └────┬─────┘   └─────┬─────┘   └─────┬─────┘   └────┬─────┘│
│       │               │               │               │      │
│       │               ├──► Claude     │          Gmail │      │
│       │               │    Direct API (stream+tools)  via    │
│       │               │    OR CLI subprocess         MCP     │
│       │               │    + /effort /retry /debug     │      │
│       │               │               │               ▼      │
│       │               ├──► MCP Manager           Observations│
│       │               │    ├─ Config servers (gws, github)   │
│       │               │    ├─ Runtime extensions (proxy)     │
│       │               │    └─ Skills (built-in + Wasm)       │
│       │               │               │                      │
│       │               └──► Memory ◄───┘                      │
│       │                    SQLite / Qdrant / Ollama          │
│       │                                                      │
│       │◄── [tool] lines (/debug toggles visibility)          │
│       │                                                      │
└───────┼──────────────────────────────────────────────────────┘
        │
   Telegram
   Bot API
```

Everything runs as goroutine-based actors under supervision. If an actor panics, it restarts with exponential backoff (1s -> 30s), resetting after 60s healthy. On shutdown, actors get 30 seconds to drain before forced exit. The Email Ingest Actor (optional, `[email_ingest]` config) polls Gmail via the GWS MCP server, applies a two-stage filter (sender/label pre-filter, then Claude importance scoring), and extracts observations from important emails into SQLite + Qdrant.

A companion **updater sidecar** (`curlycatclaw-updater`) runs as a separate container alongside the main daemon. It holds the Docker socket and exposes an authenticated HTTP API for image pulls, container restarts, and rollbacks. The main container communicates with it via `internal/update/client.go`. This keeps Docker socket access out of the main container. Telegram commands `/update`, `/status`, and `/rollback` drive the sidecar through the session actor. An optional auto-update cron (gocron) checks for new images on a schedule.

The MCP Manager maintains persistent stdio connections to configured servers (Google Workspace, GitHub) and runtime extensions (scrapling-mcp, fetch, etc.). In CLI mode, extensions are proxied through the curlycatclaw-skills MCP subprocess with hot-reload via `AddTool()`/`RemoveTools()`. Environment variables pass through a three-layer allowlist (subprocess -> MCP server -> extension) to prevent secret leakage while allowing necessary config like `PLAYWRIGHT_BROWSERS_PATH`. The GWS MCP server supports multi-account mode via `GWS_ACCOUNT_*` env vars, with per-call credential switching (`cmd.Env` override) and optional per-account service filtering (`GWS_ACCOUNT_<NAME>_SERVICES`). A `gws_list_accounts` tool lets Claude discover available accounts and their service permissions.

## Streaming Pipeline

```
Telegram ──► Channel Actor ──► Session Actor ──► Claude API (streaming)
               (long-poll,       (context,           │
                photos)           tools)             │ content deltas
                                                     ▼
                                              onDelta() ── 500ms debounce
                                                     │
                                              flush() ── releases mutex during
                                                     │   Telegram I/O (flushing flag)
                                                     │
              Telegram ◄── send/edit ◄───────────────┘
                                                     │
                                              Tool calls? ─── No ──► done
                                                     │
                                                    Yes
                                                     │
                                              Execute tools, reset stream
                                              state, loop (max 10 rounds)
```

Each tool round produces a distinct Telegram message. Text edits respect Telegram's 4096-char limit. Long responses split at paragraph boundaries, and code blocks are closed/reopened across splits so both messages render correctly. The `flushing` state flag prevents lock contention during Telegram I/O. Rate-limited HTML edits retry once to prevent raw markdown display.

Thinking effort (`/effort low|medium|high|max`) controls extended thinking budget per request. In direct API mode, `high` and `max` enable `ThinkingConfigParamOfEnabled` with 10K/32K token budgets. Thinking block signatures are preserved in conversation history for multi-turn tool calls. `/retry` replays the last message at a different effort level (one-shot). `/debug on|off` toggles tool call visibility.

## Memory System

Four-tier hierarchical memory:

```
Context Assembly (per request)
┌──────────────────────────────────────────────────────────┐
│  Tier 1 (always)    │ User Facts (SQLite)                │  system prompt
│  Tier 2 (semantic)  │ Observations (Qdrant + FTS5)       │  decisions, preferences, project state, commitments, discoveries, references
│  Tier 3 (semantic)  │ Relevant Summaries (Qdrant)        │  cosine similarity
│  Tier 4 (window)    │ Recent Messages (SQLite)           │  25 turns, ~150K tokens
└──────────────────────────────────────────────────────────┘

Observation Extraction (idle detection, in-memory turn counter):
  Conversation turns ──► Turn threshold ──► ObservationExtractor
                                                    │
                              SQLite (structured) ◄─┤
                              Qdrant (embed) ◄──────┤
                              Entities (FTS5) ◄─────┘
  Types: decision, preference, project_state, commitment, discovery, reference
  Entities: person, project, file, tool (extracted alongside observations)
  Search: hybrid (RRF of FTS5 keyword + vector similarity), multi-vector (per-fact points)
  Retrieval: progressive 3-layer (compact index → expanded → full detail)
  Relations: supersedes/refines/contradicts, active filtering (confidence >= threshold hides superseded obs from search)
  Self-healing: extraction detects project_state supersession, supersede_observation skill for real-time correction
  Soft delete: archived_at column, forget archives instead of deletes, restore_observation to undo
  Notifications: inline Telegram messages on supersession with /keep_both, /revert, /forget_old commands
  Model: extraction uses per-spawn model override (extraction_model config, e.g., haiku)
  Startup: FixObservationCollectionDimension + reindexMissingObservations (auto-heal)
  System prompt: "What I remember" section with dedup against facts, supersede_observation instruction

Conversation Archival (>4h idle, both API and CLI modes):
  Expired conv ──► Load messages ──► Format (head+tail 12K) ──► Claude summarize
                                                                       │
                                                  SQLite (text) ◄──────┤
                                                  Qdrant (embed+type) ◄┘
  Model: summarization uses per-spawn model override (summarize_model config)
  Crash recovery: retries pending/failed/indexed_failed on startup
  Chat-type-aware: DM summaries cross-chat, group summaries stay scoped
```

## Tool Execution

Four tool sources unified under one routing layer:

```
Claude tool_use ──► skills.Registry.Get(name)
                     ├─ Found ──► Built-in Skill (with UserInfo ctx)
                     └─ Not found ──► MCP Manager (server__tool namespace)

┌──────────────────┬───────────────────┬──────────────────────┐
│  Built-in Skills │  MCP Servers      │  Wasm Plugins        │
├──────────────────┼───────────────────┼──────────────────────┤
│  web_search      │  Config servers:  │  Capability-gated:   │
│  save_note       │  server__tool     │  ├ http (SSRF block) │
│  send_file       │  (multi-account)  │                      │
│  set_reminder    │                   │  ├ db_read (enforced │
│  remember_fact   │  Runtime exts:    │  │  :user_id scoping,│
│  semantic_search │  ext__tool (proxy)│  │  UNION blocked)   │
│  list_summaries  │                   │                      │
│  delete_summary  │  Hot-reload: tools│  └ send_message      │
│  set_extension_* │  added/removed at │                      │
│                  │  runtime via MCP  │ Hot-reload (fsnotify)│
│  Deps: FactStore │  notifications    │                      │
│  DB, VectorStore │                   │                      │
└──────────────────┴───────────────────┴──────────────────────┘
                        │
                        ▼
               Tool result → Claude (next loop round)
```

In CLI subprocess mode, runtime MCP extensions are proxied through the curlycatclaw-skills MCP server. When you add/remove extensions, tools are registered dynamically via `Server.AddTool()`/`Server.RemoveTools()` without restarting the subprocess. This preserves your conversation context. For plugin installs (which do require a restart), recent conversation history is injected into the new subprocess's system prompt from SQLite.

## Vector Search

Pluggable embeddings with three Qdrant collections:

```
Embedder Interface: Embed(text) → vector
  ├─ FNV (384d, offline, no deps)
  ├─ Ollama (768d, local, nomic-embed-text)
  └─ Voyage AI (512d, API, voyage-3-lite)

Qdrant (gRPC, cosine similarity, user_id tenant isolation):
  ├─ curlycatclaw_messages      ◄── user messages
  ├─ curlycatclaw_notes         ◄── saved notes
  ├─ curlycatclaw_summaries     ◄── archived conversations
  └─ curlycatclaw_observations  ◄── extracted observations (6 types) + per-fact vectors

query → Embed(query) → Qdrant.Search(vector, user_id filter) → ranked results
```
