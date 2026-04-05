# Architecture

## System Overview

```
┌───────────────────────────────────────────────────────┐
│                     Supervisor                        │
│          (panic/recover, backoff, 30s drain)          │
│                                                       │
│  ┌──────────┐   ┌───────────┐   ┌───────────┐         │
│  │ Channel  │◄─►│  Session  │   │ Reminder  │         │
│  │  Actor   │   │   Actor   │   │   Actor   │         │
│  └────┬─────┘   └─────┬─────┘   └─────┬─────┘         │
│       │               │               │               │
│       │               ├──► Claude     │               │
│       │               │    Direct API (stream+tools)  │
│       │               │    OR CLI subprocess (Max)    │
│       │               │               │               │
│       │               ├──► Tools      │               │
│       │               │    Skills / MCP / Wasm / Ext  │
│       │               │               │               │
│       │               └──► Memory ◄───┘               │
│       │                    SQLite / Vector             │
│       │                                               │
│       │◄── [tool] lines + [confirm?] previews         │
│       │                                               │
└───────┼───────────────────────────────────────────────┘
        │                  │
   Telegram            Landlock
   Bot API          (Linux sandbox)
```

Everything runs as goroutine-based actors under supervision. If an actor panics, it restarts with exponential backoff (1s → 30s), resetting after 60s healthy. On shutdown, actors get 30 seconds to drain before forced exit.

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

Each tool round produces a distinct Telegram message. Text edits respect Telegram's 4096-char limit -- long responses split automatically. The `flushing` state flag prevents lock contention during Telegram I/O.

## Memory System

Four-tier hierarchical memory:

```
Context Assembly (per request)
┌──────────────────────────────────────────────────────────┐
│  Tier 1 (always)    │ User Facts (SQLite)                │  system prompt
│  Tier 2 (semantic)  │ Observations (Qdrant + FTS5)        │  decisions, preferences, project state, commitments, discoveries, references
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
  Relations: supersedes/refines/contradicts (advisory, ranking boost not hiding)
  Model: extraction uses per-spawn model override (extraction_model config, e.g., haiku)
  Startup: FixObservationCollectionDimension + reindexMissingObservations (auto-heal)
  System prompt: "What I remember" section with dedup against facts

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
