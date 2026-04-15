# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**curlycatclaw** is a personal AI agent assistant built in Go. It's a long-running daemon with a goroutine-based actor model, Telegram as the primary channel, Claude as the LLM (no multi-model abstraction), SQLite for storage, and MCP for tool integration. See [CONTRIBUTING.md](CONTRIBUTING.md) for bug reports and code contributions.

## Build & Run

```bash
go build -o curlycatclaw ./cmd/curlycatclaw
go build -o curlycatclaw-updater ./cmd/curlycatclaw-updater
./curlycatclaw --config ~/.curlycatclaw/config.toml
```

## Docker (primary way to run)

```bash
docker compose up -d                      # pulls pre-built images, starts services
docker compose logs curlycatclaw --tail 20 # check logs
docker compose restart curlycatclaw       # restart without rebuild
```

For dev (building from source), copy the override file first:
```bash
cp docker-compose.override.yml.example docker-compose.override.yml
docker compose build && docker compose up -d
```

Optional services use Compose profiles:
```bash
# Enable profiles by creating .env next to docker-compose.yml:
echo "COMPOSE_PROFILES=ollama,updater" > .env
docker compose up -d  # reads .env automatically
```

Fast iteration without release: `docker compose build curlycatclaw && docker compose up -d curlycatclaw` rebuilds from local source using the override file. No need to push, tag, or wait for goreleaser. Clean extensions between tests with a python one-liner and `docker compose restart curlycatclaw`.

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

CI also runs `govulncheck ./...` (continue-on-error) and tests with `-race` flag.

Test expectations:
- When writing new functions, write a corresponding test
- When fixing a bug, write a regression test
- When adding error handling, test both success and error paths
- Tests use stdlib `testing` package with `t.Fatal`/`t.Error` assertions
- Always run `golangci-lint run` and `go test` locally before committing to avoid CI failures
- Never merge a PR with failing CI. Fix lint issues first.

## Git workflow

This project ships directly on `main` (no feature-branch / PR workflow). `/ship` and similar skills that expect a feature branch will abort; commit and push from `main` directly.

## Versioning

Version is tracked in the `VERSION` file (currently source of truth). Follows semver (`x.y.z`):
- **x (major)**: Reserved — only bumped when explicitly decided by the maintainer
- **y (minor)**: New features
- **z (patch)**: Bug fixes and patches

Goreleaser injects the version into the binary via `-X main.version={{.Version}}` from git tags. Update `VERSION` and `CHANGELOG.md` when releasing.

## CLI Subcommands

```bash
curlycatclaw --config PATH       # path to config.toml (default: ~/.curlycatclaw/config.toml)
curlycatclaw --version            # print version and exit
curlycatclaw --mcp-server         # run as MCP stdio server (spawned by claude CLI)
curlycatclaw --migrate-embedder   # wipe and rebuild vector collections with configured embedder
curlycatclaw --migrate-embedder --dry-run  # count texts only, no modifications
curlycatclaw --eval-export               # export conversations for manual quality labeling
curlycatclaw --eval-seed                 # generate synthetic conversations for eval validation
curlycatclaw --eval-export --eval-hours 48  # export last 48 hours (default: 24)
curlycatclaw --health-check               # check health endpoint, exit 0/1 (Docker healthcheck)
curlycatclaw --validate-config            # validate config file and exit (for setup wizard)
```

## Architecture

Goroutine-based actor model under supervision. See [docs/architecture.md](docs/architecture.md) for full diagrams and details.

**Core pattern**: Supervisor runs Channel Actor (Telegram I/O), Session Actor (Claude + tools + memory), Reminder Actor (cron tasks), Eval Actor (background self-evaluation), and Ingest Actor (background multi-source knowledge ingestion). Each actor panics safely and restarts with exponential backoff. A separate **updater sidecar** (`curlycatclaw-updater`) runs alongside the main container, holding the Docker socket and managing image pulls, container restarts, rollbacks, and **companion service management** (dynamic Docker Compose service registration, start/stop, health polling) via an authenticated HTTP API. The `manage_service` skill gives the agent the ability to register and start companion Docker services (e.g., HTTP MCP servers) from Telegram chat.

**Ingest pipeline**: Generic `IngestActor` processes configured knowledge sources (Gmail via MCP, Obsidian via filesystem, Notion via MCP). Config-driven via `[[ingest.sources]]`. Each source implements `Source` interface (Discover/Read/Prefilter). Three extractors: LLM (trusted/untrusted prompts), Passthrough (YAML front matter), Hybrid. Per-source cursors, daily caps, trust levels. Content fingerprint tracking for mutable-source re-extraction. Stale "running" state recovery on startup. Deprecated `[email_ingest]` auto-migrates.

**Claude integration**: Two modes via `[claude]` config. Direct API (`api_key`) uses anthropic-sdk-go with streaming. CLI subprocess (`cli_path` + `oauth_token`) spawns long-lived `claude` processes per user. `thinking_effort` controls extended thinking (high=10K, max=32K budget tokens). `/effort`, `/retry`, `/stop`, `/debug` Telegram commands for runtime control. `/update`, `/status`, `/rollback` for self-update management. Document attachments: PDFs sent as native document blocks (both modes), text files inlined into message (CLI) or as text blocks (API).

**MCP & tools**: MCP Manager holds persistent connections via stdio (local subprocesses) or Streamable HTTP (remote servers). Config: `transport = "http"` + `url` + `[headers]` for remote; default stdio uses `command` + `args`. Runtime extensions support both stdio and HTTP transports via `add_extension` (with `transport`, `url`, `headers` fields for HTTP). Extensions are proxied through curlycatclaw-skills MCP subprocess with hot-reload (`AddTool`/`RemoveTools`). Three env allowlists in chain: subprocess.go -> mcp_server.go -> extension. `PLAYWRIGHT_BROWSERS_PATH` must be in all three for scrapling browser tools. GWS MCP supports multi-account via `GWS_ACCOUNT_*` env vars with per-account credential switching and optional `GWS_ACCOUNT_<NAME>_SERVICES` restrictions. `gws_list_accounts` tool for account discovery.

**Memory**: Four tiers: user facts (always), observations (Qdrant + FTS5 hybrid search), conversation summaries (Qdrant), sliding window (25 turns). Observation extraction auto-triggers after idle. Self-healing supersession detects stale project_state. Soft delete with archive/restore.

**Eval pipeline**: Background self-evaluation via EvalActor (gocron scheduler). Scores conversations using deterministic signals (tool errors, corrections, retries, effort overrides). Mines failure patterns. Generates memory candidates via Claude (read-only, no tools). Human approval via Telegram inline keyboards. Enabled via `[eval]` config section.

**Streaming**: Text deltas -> Telegram message edits (500ms debounce). Overflow splits at paragraph boundaries, closes/reopens code fences. Rate-limited HTML edits retry once. `flushing` flag prevents lock contention.

**Gotchas**:
- CLI subprocess `--effort` is spawn-time only. `/effort` kills+respawns the process.
- Thinking block signatures must be in conversation history for multi-turn tool calls (API requirement).
- `redacted_thinking` blocks need separate handling (`NewRedactedThinkingBlock`).
- `lastUserMsg` map stores full `IncomingMessage` including attachment bytes. Bounded by user count.
- `splitAtBoundary()` in actor.go handles message overflow. Searches backward for `\n\n`, detects unclosed code fences.
- Actor struct maps (`effortOverride`, `lastUserMsg`, `debugOverride`, `obsState`) do NOT need mutexes. `handleMessage` runs in a single goroutine from the actor's `Run()` loop. Only `activeProjects` has a mutex (defense-in-depth, not required).
- GWS multi-account: `GWS_ACCOUNT_<NAME>_SERVICES` env vars must not collide with account names. `parseAccountsFromEnv()` skips keys ending in `_SERVICES`. Account names validated as `[a-zA-Z0-9_-]+`. `"account"` is in `reservedFlags` to prevent LLM injection as a gws CLI flag. Credential paths must be absolute and exist at startup (fatal otherwise).
- `send_file` in CLI mode queues to SQLite (`pending_files` table), delivered by session actor after tool loop ends. Direct API mode sends immediately via Telegram. Tool result says "File queued" to prevent Claude retries.
- CLI subprocess `bufio.Scanner` max is 16MB (for base64 PDF responses in stream-json). Default 64KB would crash on any document attachment.
- Health endpoint binds to `0.0.0.0:18080` (not `127.0.0.1`) so the updater sidecar can reach it across the Docker network for liveness checks.
- Reminder cancellation in CLI mode: `cancel_reminder` updates the DB via MCP subprocess, but the signal channel drains to /dev/null in `mcp_server.go`. `pollNewReminders` (every 10s) compensates by checking DB for cancelled jobs. `fireReminder` also re-checks DB status before sending as a safety net.
- HTTP MCP transport: `headerRoundTripper` skips reserved headers (`content-type`, `accept`, `mcp-session-id`) to avoid breaking SDK internals. Redirects are blocked (`http.ErrUseLastResponse`) to prevent API key leakage. 60s client timeout. `DisableStandaloneSSE: true` for stateless servers. Per-server 5s shutdown timeout prevents one hung HTTP DELETE from blocking the rest.
- HTTP MCP extensions: `add_extension` supports `transport: "http"` with `url` and `headers` fields. Persists to `extensions.json`, auto-resurrects on restart, hot-reloads. Headers are currently stored in plaintext (encrypted credential store integration planned).
- Companion service management: `manage_service` skill requires updater sidecar. ServiceCatalog persists to `/data/managed-services.json`. Compose overlay generated at `/data/docker-compose.managed.yml`. `ALLOWED_IMAGES` env var restricts registerable images (empty = allow all). Volume names validated against `isValidServiceName` regex to prevent host bind mounts.
- MCP subprocess env chain (CLI mode): THREE layers of env filtering. (1) `spawnEnvAllowlist` in subprocess.go filters parent→CLI subprocess. (2) `buildMCPConfig` in actor.go explicitly sets `mcpEnv` map for the MCP server subprocess — the Claude CLI does NOT inherit parent env to MCP children, only explicit JSON `env` fields are passed. (3) The MCP subprocess reads `os.Getenv()` from its own env. To pass a new env var to the MCP subprocess, add it to ALL THREE: spawnEnvAllowlist + mcpEnv in buildMCPConfig + the code that reads it in mcp_server.go.
- CLI subprocess reuse: `GetOrCreate` in subprocess.go reuses alive CLI processes. System prompt and MCP config are set at spawn time only — reused processes ignore new SpawnParams. After deploying a system prompt change, the subprocess must be killed (container restart or `/effort` command) for the new prompt to take effect.
- HTTP URL auto-detection in add_extension: When command field looks like `http://` or `https://`, it's auto-converted to transport="http" + url. This catches the common LLM mistake of passing a URL as a stdio command. See skills.go around line 110.
- New skills must be registered in BOTH main.go (API mode) AND mcp_server.go (CLI mode). The MCP subprocess has its own skill registry. Forgetting mcp_server.go means the skill is invisible in CLI mode deployments.
- HTTP MCP auto-register: `add_extension` accepts optional `image` and `ports` fields for HTTP extensions. When the server is unreachable and `image` is provided, the auto-starter registers the service in the updater sidecar catalog (`ServiceRegister`) before starting it. Security: `ALLOWED_IMAGES` on the updater sidecar gates which images can be registered. Extension struct persists image/ports to `extensions.json` for auto-start on restart.
- MCP subprocess debug log: `/data/mcp-debug.log` captures startup extension loading AND runtime hot-reload events (connect attempts, successes, failures). MCP subprocess stderr goes to Claude CLI (invisible in `docker compose logs`), so this file is the only way to debug MCP issues in CLI mode. Hot-reload entries appear live as `add_extension` fires because `mcpHotReloader` carries a `dbgLog` that the caller wires in after the file is opened.
- `loadProxyUpstreams` is **synchronous with parallel fanout**, not async. It runs in `runMCPServer` before `server.Run()` starts, using a `sync.WaitGroup` over per-extension goroutines with a 15s ctx timeout each. This shape is load-bearing because of two separate prior bugs: (1) a fully-sequential sync load took 25s+ on cold uvx caches and blew Claude CLI's MCP initialize timeout, killing the server on EOF; (2) an async load fixed that but raced `tools/list` — Claude CLI issues it within ~1s of spawn, caches the response, and ignores `notifications/tools/list_changed`, so proxied tools stay invisible forever. Parallel fanout with a per-upstream cap bounds total wall time at `max(upstream_times)`, fitting under CLI's init budget while still letting tools/list return the full set. Don't "optimize" this to async without solving the Claude-CLI-tool-list-cache problem first.
- `add_extension` always queues a CLI respawn on success: `addMCPExtension` fires `reloadFunc` after hot-reload regardless of transport, because Claude CLI caches its initial tool list at MCP initialize and ignores `list_changed` mid-session. Hot-reload still runs — it exercises the extension's connection, returns the real tool list for the agent's success message, and arms the proxy server so the fresh CLI subprocess picks up tools during its own startup `loadProxyUpstreams`. Consequence: the success message honestly says "will be available on your next message," never "immediately." The Execute handler's HTTP recovery branch detects a soft-failed hot-reload by grepping for `"hot-reload failed"` in the returned message (brittle but contained).
- `add_extension` strips matched wrapping quotes from every user-supplied string (cmd, args, url, env values, header values) before persisting to `extensions.json`. Agents frequently copy args from shell examples like `uvx --from 'git+https://...' foo` and pass the literal single-quoted string, which breaks uvx/npm argv parsing. The strip helper is `internal/extension/skills.go:stripSurroundingQuotes`; it only removes matched pairs so `'a"b'`-style values aren't mangled.
- Personality file is injected as system prompt prefix in `buildSystemPrompt()`. Operator-controlled only. CronExecutor (`cron.go`) intentionally uses a fixed prompt, not the configured personality. Config validates absolute path, regular file, and 20KB size limit. `personality.Load()` additionally validates UTF-8 and non-empty content. Hash is computed on trimmed content for consistent drift detection.
- `/stop` dispatch model: Actor.Run serializes work — at most one `handleMessage` goroutine per actor at a time. When in-flight, the Run loop's `select` keeps reading `tg.Updates()` so `/stop` is seen immediately (cancels the workCtx and calls `cliMgr.Remove` to kill the CLI subprocess); all other messages queue in `pendingMsgs` (cap 10, drop-oldest). Because only ONE handleMessage runs at a time, the "actor maps don't need mutexes" invariant still holds — the Run loop's side branches (handleReaction/handleCallback/stopActive) don't touch `lastUserMsg`/`effortOverride`/`debugOverride`/`obsState`. Cron-triggered runs are NOT interruptible.
- Cron/reminder scheduled-time drift: `CronExecutor.Execute` takes a mandatory `scheduledAt time.Time` (typically `reminderRow.FireAt`). `buildSystemPrompt` renders both the scheduled fire time and current execution time in the configured timezone and instructs Claude to reference the SCHEDULED time in replies. Without this, late-firing reminders misreport their own time.
- GitHub issue routing: bugs ABOUT curlycatclaw itself → forced to `[github] owner`/`repo` (defaults `jialuohu/curlycatclaw` via `GitHubConfig.OwnerOrDefault()`/`RepoOrDefault()`, never ask the user). Issues the user explicitly opens in their own projects → use the repo they named. The MCP `create_issue` confirmation preview always shows `Target: owner/repo` so the user can catch misdirection before submitting.
- Pre-push hook runs the full test suite (`go test ./... -race`) before `git push` completes. Expect a ~30-40s pause and a hard block on any failure. Don't bypass with `--no-verify`; fix the test.
- `CronRunner` interface lives in `skills/remind.go` (not `internal/session/`) to avoid the `skills` ↔ `session` import cycle. Changing its signature requires in-sync updates to three places: the interface in `remind.go`, `session.CronExecutor` (the impl), and `skills.mockCronRunner` in `remind_test.go`.
- `PersistentCLISender` (ingest LLM extraction) vs `SpawnOneShot` (cron, summarizer, observer): both log `cli: Send() called ... skip_write=true`, but PersistentCLISender is TRACKED in `CLIManager.processes` under synthetic `(ingestUserID, ingestTrustedChatID|ingestUntrustedChatID)` keys and is REUSED across ingest ticks (every 2 min). A long-lived `claude` subprocess with no MCP children is normal for ingest — it spawns with `SafeMode=true` and no MCP config. Don't diagnose it as a leak without checking `CLIManager.processes`. SpawnOneShot callsites: `cmd/curlycatclaw/main.go:507` (summarizer), `:650` (observer), `internal/session/cron.go:180`, `internal/eval/llm.go:68`.
- Root-owned files in `~/.curlycatclaw/`: the updater sidecar and the container (when running as root) write `docker-compose.managed.yml`, `managed-services.json`, `update-state.json`, and parts of `.claude/` as root. Host `sudo rm` needs a TTY password that's usually unavailable in scripted shells. When cleaning without TTY sudo, use a rootful container via the bind mount: `docker run --rm -v ~/.curlycatclaw:/data alpine rm -rf /data/<path>`.

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
| `internal/telegram/channel.go` | Telegram channel actor (go-telegram/bot v1.20.0, Bot API 9.5) |
| `internal/memory/store.go` | SQLite storage |
| `internal/mcp/manager.go` | MCP server lifecycle (stdio + Streamable HTTP) |
| `internal/memory/embedder.go` | Embedder interface + FNV/Ollama/Voyage implementations |
| `internal/memory/facts.go` | User facts CRUD (sanitization, IDOR protection) |
| `internal/memory/summarizer.go` | Conversation summarizer (transcript formatting + Claude) |
| `internal/memory/vectorstore.go` | Qdrant vector search (messages, notes, summaries, observations) |
| `internal/memory/observer.go` | ObservationExtractor (automatic observation extraction from conversations) |
| `internal/memory/observation.go` | Observation CRUD and SQLite storage |
| `skills/observation.go` | Observation skills (search, list, get, forget, search_entities) |
| `internal/wasm/runtime.go` | Wasm skill runtime (wazero) |
| `internal/session/cron.go` | CronExecutor for scheduled Claude-powered tasks |
| `skills/registry.go` | Skill struct, Registry, UserInfo, GetUser() context helpers |
| `skills/` | Built-in skill implementations |
| `skills/fact.go` | User facts skills (remember, forget, list) |
| `skills/search.go` | Web search skill (DuckDuckGo) |
| `skills/semantic_search.go` | Semantic search skill (Qdrant vector search) |
| `skills/summary.go` | Summary management skills (list_summaries, delete_summary) |
| `skills/plugin.go` | Plugin management skills (install, uninstall, enable, disable, list) |
| `skills/remind.go` | Reminder skills + ReminderActor (gocron scheduling, poll-based cancel detection) |
| `skills/diagnostics.go` | Diagnostics capture skill (version, MCP status, recent errors, health) for bug reports |
| `skills/personality.go` | get_personality / set_personality skills (read/update agent persona at runtime) |
| `internal/extension/extension.go` | Runtime extension registry (MCP servers + exec skills) |
| `internal/mdhtml/convert.go` | Markdown to Telegram HTML converter |
| `internal/voice/stt.go` | OpenAI Whisper speech-to-text client |
| `internal/skillloader/loader.go` | External skill collection loader (exec adapter) |
| `internal/memory/migration.go` | Background embedding migration manager (backfill, catch-up, alias swap) |
| `cmd/curlycatclaw/migrate.go` | CLI embedder migration tool (manual fallback, versioned collections + aliases) |
| `internal/ingest/source.go` | Source interface, ItemRef, Content types for generic ingest pipeline |
| `internal/ingest/actor.go` | Generic IngestActor (background multi-source knowledge ingestion) |
| `internal/ingest/mcp_source.go` | GmailSource (multi-account MCP) and NotionSource implementations |
| `internal/ingest/file_source.go` | FileSource for Obsidian vaults (directory walker, mtime cursor, symlink escape) |
| `internal/ingest/extract.go` | LLM/Passthrough/Hybrid extractors with trusted/untrusted prompt templates |
| `cmd/curlycatclaw/eval_export.go` | CLI eval export tool (conversation quality labeling) |
| `cmd/curlycatclaw/eval_seed.go` | CLI eval seeder (synthetic conversations for validation) |
| `internal/eval/actor.go` | EvalActor: supervised background eval with gocron scheduler |
| `internal/eval/scorer.go` | ConversationScorer: deterministic quality signals from SQLite |
| `internal/eval/miner.go` | FailureMiner: cluster low-scoring conversations by failure type |
| `internal/eval/candidate.go` | CandidateGenerator: Claude proposes memory fixes per failure |
| `internal/eval/gate.go` | CommitGate: confidence-based gating with approve/reject |
| `internal/personality/personality.go` | Personality loader (file reading, UTF-8/size validation, SHA-256 hashing) |
| `internal/security/credential.go` | AES-256-GCM encrypted credential store for MCP server secrets |
| `internal/memory/context.go` | Memory context builder for conversation priming |
| `skills/note.go` | Note management skills (create, list, read, delete) |
| `skills/send_file.go` | Send file skill (Telegram document delivery) |
| `cmd/curlycatclaw-gws-mcp/main.go` | GWS MCP server entrypoint, multi-account env parsing (`GWS_ACCOUNT_*`, `_SERVICES`) |
| `cmd/curlycatclaw-gws-mcp/executor.go` | GWS CLI subprocess runner, account resolution, service validation, per-call env overrides |
| `cmd/curlycatclaw-gws-mcp/discovery.go` | GWS skill discovery, tool registration, account field injection, `gws_list_accounts` |
| `cmd/curlycatclaw-updater/main.go` | Updater sidecar entrypoint, HTTP server, shared secret auth |
| `cmd/curlycatclaw-updater/handler.go` | Update/rollback/status HTTP handlers, companion service management, digest blacklist |
| `cmd/curlycatclaw-updater/docker.go` | Docker client wrapper for image pull, container restart, rollback, companion service compose commands |
| `cmd/curlycatclaw-updater/catalog.go` | ServiceCatalog for persistent companion service specs (JSON, atomic write) |
| `cmd/curlycatclaw-updater/compose_gen.go` | Docker Compose overlay generator from service catalog |
| `internal/update/client.go` | HTTP client for updater sidecar API (update + service management) |
| `skills/service.go` | manage_service skill (register/start/stop/status/list companion Docker services) |
| `Dockerfile` | Container build (CGO_ENABLED=0, Debian bookworm-slim) |
| `Dockerfile.updater` | Updater sidecar container build |
| `docker-compose.yml` | curlycatclaw + Qdrant + Ollama orchestration |
| `.goreleaser.yml` | Release automation (binaries, checksums, Docker images) |

## Configuration

Copy `config.toml.example` to `~/.curlycatclaw/config.toml` and fill in credentials. All paths use Docker mount paths (`/data/...`). Docker Compose mounts `~/.curlycatclaw` as `/data`.

Auth modes: `cli_path` + `oauth_token` (Claude subscription via CLI subprocess) or `api_key` (direct API). CLI mode uses `oauth_token` from `claude setup-token` injected as `CLAUDE_CODE_OAUTH_TOKEN` env var.

For Google Workspace, export credentials on a machine with a browser (`gws auth export --unmasked > ~/.curlycatclaw/gws-credentials.json`). Single-account: set `GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE` in `[mcp.servers.env]`. Multi-account: use `GWS_ACCOUNT_<NAME>` env vars with optional `GWS_ACCOUNT_<NAME>_SERVICES` restrictions and `GWS_DEFAULT_ACCOUNT`. See `config.toml.example`.

For encrypted MCP credentials, set `CURLYCATCLAW_MASTER_KEY` env var (64 hex chars = 32 bytes).

For self-update, generate a shared secret: `echo "UPDATER_SECRET=$(openssl rand -hex 32)" >> .env` (in the same directory as `docker-compose.yml`). Docker Compose reads `.env` automatically and injects the secret into both the main container and the updater sidecar.

For self-evaluation, add `[eval]` section: `enabled = true`, `schedule = "0 3 * * *"` (cron), `lookback_hours = 24`, `score_threshold = 0.6`. Use `--eval-export` to dump conversations for manual labeling, `--eval-seed` to generate synthetic test data.

Optional config sections: `[[projects]]` for CLI work context (`/project` command, `name` + `path`), `[[skill_collections]]` for external skill paths, `[wasm]` for wazero plugin runtime, `[voice]` for OpenAI Whisper STT, `[logging]` for log level/file/format, `[update]` for self-update system (`enabled`, `updater_url`, `auto_update`, `schedule`), `[github]` for issue creation settings (`owner`, `repo`, used by `capture_diagnostics` + system prompt when GitHub MCP has write access), `[personality]` for agent persona via markdown file (`file` path, replaces default system prompt personality).

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
