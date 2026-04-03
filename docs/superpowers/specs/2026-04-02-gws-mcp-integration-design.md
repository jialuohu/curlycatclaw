# Google Workspace MCP Integration Design

## Context

curlycatclaw has zero Google Workspace integration today. The user wants to
access Gmail, Calendar, Drive, Docs, Sheets, Slides, Forms, Keep, Tasks, and
Maps from Telegram via the Claude-powered assistant.

Google's [`gws` CLI](https://github.com/googleworkspace/cli) is a Rust binary
that wraps the full Workspace API surface via the Discovery Service. It outputs
structured JSON, has 40+ curated helper commands, and is designed for AI agent
invocation. It has no MCP server — we build one.

## Decision

Build `curlycatclaw-gws-mcp`, a standalone Go MCP server binary that
dynamically discovers and proxies gws CLI commands as MCP tools.

### Why this approach

- **Minimal core changes**: curlycatclaw adds one `config.toml` entry. No Go
  code changes to the daemon.
- **Best Claude UX**: tools have typed JSON schemas and descriptions from gws's
  own skill metadata.
- **Future-proof**: `gws generate-skills` auto-discovers new helpers — no
  hardcoded tool list.
- **Consistent**: follows the same MCP server pattern as existing integrations.

### Alternatives rejected

- **Built-in Go skill (Approach C)**: tight coupling, each new command = Go
  code change, doesn't scale to hundreds of APIs.
- **Exec skill collection (Approach B)**: shell scripts are fragile, no
  streaming, each command needs its own script + skill.toml.

## Architecture

```
curlycatclaw (daemon)
  └─ spawns via MCP config ──→ curlycatclaw-gws-mcp (MCP stdio server)
                                   └─ spawns per tool call ──→ gws CLI
                                                                  └─ Google Workspace APIs
```

- Standalone binary at `cmd/curlycatclaw-gws-mcp/`
- Same Go module, shared `go.mod` (uses `modelcontextprotocol/go-sdk v0.8.0`)
- **Zero imports** from curlycatclaw's `internal/` packages
- Goreleaser builds both binaries

## Tool Discovery

At startup, the MCP server:

1. Runs `gws generate-skills --format json` (or equivalent)
2. Parses the output — each skill has: name, description, command template,
   input parameters
3. Registers each as an MCP tool with the parsed JSON schema
4. Always registers a generic `gws_api` escape hatch tool

### Filter config

An optional `GWS_FILTER` env var controls
which tools are exposed:

```toml
# Glob patterns — if set, only matching tools are registered
allow = ["gmail_*", "calendar_*", "drive_*", "sheets_*", "docs_*", "slides_*", "tasks_*"]
```

If no filter is configured, all discovered tools are exposed.

### Generic escape hatch: `gws_api`

Always registered regardless of filters. Accepts any gws command:

```json
{
  "service": "keep",
  "resource": "notes",
  "method": "list",
  "params": {},
  "body": {}
}
```

Translates to: `gws keep notes list --format json --params '{}'`

## Authentication

Delegated entirely to gws's own credential store.

**One-time setup** (on the server running curlycatclaw):
```bash
gws auth setup   # creates GCP project, enables Workspace APIs
gws auth login   # OAuth browser flow, stores creds in ~/.config/gws/
```

The MCP server inherits these credentials — no Google secrets in curlycatclaw's
config.

**Environment passthrough**: the MCP server passes through `HOME` (so gws finds
`~/.config/gws/`) and optionally `GWS_CONFIG_DIR` if credentials are stored
elsewhere.

## Config Integration

Single `config.toml` entry:

```toml
[[mcp.servers]]
name    = "gws"
command = "curlycatclaw-gws-mcp"
[mcp.servers.env]
GWS_PATH = "gws"  # optional, defaults to "gws" in PATH
# GWS_MCP_FILTER = "gmail_*,calendar_*,drive_*"  # optional inline filter
```

## Execution Pattern

Each MCP tool call:

1. **Build command**: map tool name + input → `gws <service> [+helper|resource method] --format json [--params '...'] [--json '...']`
2. **Run subprocess**: `exec.CommandContext` with 60s timeout, capture stdout/stderr
3. **Parse output**: JSON stdout → MCP tool result
4. **Handle errors**:
   - Non-zero exit: parse stderr, return as MCP error
   - Timeout: return timeout error with partial output
   - JSON parse failure: return raw stdout as text content

## Project Structure

```
cmd/curlycatclaw-gws-mcp/
├── main.go           # MCP server setup, stdio transport, config loading
├── discovery.go      # gws generate-skills parsing, tool registration
├── executor.go       # gws subprocess execution (service-agnostic)
└── executor_test.go  # Command building, output parsing, filter matching
```

## Goreleaser Update

Add second build target to `.goreleaser.yml`:

```yaml
builds:
  - id: curlycatclaw
    main: ./cmd/curlycatclaw
    # ... existing config ...

  - id: curlycatclaw-gws-mcp
    main: ./cmd/curlycatclaw-gws-mcp
    binary: curlycatclaw-gws-mcp
    env:
      - CGO_ENABLED=0
    goos: [linux, darwin]
    goarch: [amd64, arm64]
    ldflags:
      - -s -w
```

## Verification

1. **Build**: `go build ./cmd/curlycatclaw-gws-mcp`
2. **Unit tests**: command building, JSON output parsing, glob filter matching,
   error handling (timeout, bad JSON, non-zero exit)
3. **Integration test**: run MCP server with a mock `gws` script that returns
   canned JSON; verify tool discovery and call proxying work end-to-end
4. **Manual E2E**: add to curlycatclaw config, send a Telegram message like
   "what's on my calendar today?" and verify Claude calls `calendar_agenda` via
   gws and returns results

## Open Questions

None — all decisions made during brainstorming.
