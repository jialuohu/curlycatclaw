# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**curlycatclaw** is a personal AI agent assistant built in Go. It's a long-running daemon with a goroutine-based actor model, Telegram as the primary channel, Claude as the LLM (no multi-model abstraction), SQLite for storage, and MCP for tool integration.

## Build & Run

```bash
go build -o curlycatclaw ./cmd/curlycatclaw
./curlycatclaw --config ~/.curlycatclaw/config.toml
```

## Docker (primary way to run)

```bash
docker compose build curlycatclaw         # rebuild after code changes
docker compose up -d curlycatclaw         # start/restart
docker compose logs curlycatclaw --tail 20 # check logs
docker compose restart curlycatclaw       # restart without rebuild
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

Goroutine-based actor model under supervision. See [docs/architecture.md](docs/architecture.md) for full diagrams and details.

**Core pattern**: Supervisor runs Channel Actor (Telegram I/O), Session Actor (Claude + tools + memory), Reminder Actor (cron tasks), and Email Ingest Actor (background email-to-observation processing). Each actor panics safely and restarts with exponential backoff.

**Claude integration**: Two modes via `[claude]` config. Direct API (`api_key`) uses anthropic-sdk-go with streaming. CLI subprocess (`cli_path` + `oauth_token`) spawns long-lived `claude` processes per user. `thinking_effort` controls extended thinking (high=10K, max=32K budget tokens). `/effort`, `/retry`, `/debug` Telegram commands for runtime control.

**MCP & tools**: MCP Manager holds persistent stdio connections. Runtime extensions proxied through curlycatclaw-skills MCP subprocess with hot-reload (`AddTool`/`RemoveTools`). Three env allowlists in chain: subprocess.go -> mcp_server.go -> extension. `PLAYWRIGHT_BROWSERS_PATH` must be in all three for scrapling browser tools. GWS MCP supports multi-account via `GWS_ACCOUNT_*` env vars with per-account credential switching and optional `GWS_ACCOUNT_<NAME>_SERVICES` restrictions. `gws_list_accounts` tool for account discovery.

**Memory**: Four tiers: user facts (always), observations (Qdrant + FTS5 hybrid search), conversation summaries (Qdrant), sliding window (25 turns). Observation extraction auto-triggers after idle. Self-healing supersession detects stale project_state. Soft delete with archive/restore.

**Streaming**: Text deltas -> Telegram message edits (500ms debounce). Overflow splits at paragraph boundaries, closes/reopens code fences. Rate-limited HTML edits retry once. `flushing` flag prevents lock contention.

**Gotchas**:
- CLI subprocess `--effort` is spawn-time only. `/effort` kills+respawns the process.
- Thinking block signatures must be in conversation history for multi-turn tool calls (API requirement).
- `redacted_thinking` blocks need separate handling (`NewRedactedThinkingBlock`).
- `lastUserMsg` map stores full `IncomingMessage` including attachment bytes. Bounded by user count.
- `splitAtBoundary()` in actor.go handles message overflow. Searches backward for `\n\n`, detects unclosed code fences.
- Actor struct maps (`effortOverride`, `lastUserMsg`, `debugOverride`, `obsState`) do NOT need mutexes. `handleMessage` runs in a single goroutine from the actor's `Run()` loop. Only `activeProjects` has a mutex (defense-in-depth, not required).
- GWS multi-account: `GWS_ACCOUNT_<NAME>_SERVICES` env vars must not collide with account names. `parseAccountsFromEnv()` skips keys ending in `_SERVICES`. Account names validated as `[a-zA-Z0-9_-]+`. `"account"` is in `reservedFlags` to prevent LLM injection as a gws CLI flag. Credential paths must be absolute and exist at startup (fatal otherwise).

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
| `skills/fact.go` | User facts skills (remember, forget, list) |
| `skills/search.go` | Web search skill (DuckDuckGo) |
| `skills/semantic_search.go` | Semantic search skill (Qdrant vector search) |
| `skills/summary.go` | Summary management skills (list_summaries, delete_summary) |
| `skills/plugin.go` | Plugin management skills (install, uninstall, enable, disable, list) |
| `internal/extension/extension.go` | Runtime extension registry (MCP servers + exec skills) |
| `internal/mdhtml/convert.go` | Markdown to Telegram HTML converter |
| `internal/voice/stt.go` | OpenAI Whisper speech-to-text client |
| `internal/skillloader/loader.go` | External skill collection loader (exec adapter) |
| `internal/memory/migration.go` | Background embedding migration manager (backfill, catch-up, alias swap) |
| `cmd/curlycatclaw/migrate.go` | CLI embedder migration tool (manual fallback, versioned collections + aliases) |
| `internal/email/actor.go` | Email Ingest Actor (background Gmail polling, two-stage filter, Claude extraction to observations) |
| `skills/send_file.go` | Send file skill (Telegram document delivery) |
| `cmd/curlycatclaw-gws-mcp/main.go` | GWS MCP server entrypoint, multi-account env parsing (`GWS_ACCOUNT_*`, `_SERVICES`) |
| `cmd/curlycatclaw-gws-mcp/executor.go` | GWS CLI subprocess runner, account resolution, service validation, per-call env overrides |
| `cmd/curlycatclaw-gws-mcp/discovery.go` | GWS skill discovery, tool registration, account field injection, `gws_list_accounts` |
| `Dockerfile` | Container build (CGO_ENABLED=0, Debian bookworm-slim) |
| `docker-compose.yml` | curlycatclaw + Qdrant + Ollama orchestration |
| `.goreleaser.yml` | Release automation (binaries, checksums, Docker images) |

## Configuration

Copy `config.toml.example` to `~/.curlycatclaw/config.toml` and fill in credentials. All paths use Docker mount paths (`/data/...`). Docker Compose mounts `~/.curlycatclaw` as `/data`.

Auth modes: `cli_path` + `oauth_token` (Claude subscription via CLI subprocess) or `api_key` (direct API). CLI mode uses `oauth_token` from `claude setup-token` injected as `CLAUDE_CODE_OAUTH_TOKEN` env var.

For Google Workspace, export credentials on a machine with a browser (`gws auth export --unmasked > ~/.curlycatclaw/gws-credentials.json`). Single-account: set `GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE` in `[mcp.servers.env]`. Multi-account: use `GWS_ACCOUNT_<NAME>` env vars with optional `GWS_ACCOUNT_<NAME>_SERVICES` restrictions and `GWS_DEFAULT_ACCOUNT`. See `config.toml.example`.

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
