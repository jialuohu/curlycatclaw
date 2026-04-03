# MCP Extension Proxy Through curlycatclaw-skills

## Context

When a user adds an MCP extension at runtime in CLI mode, it's persisted to
`extensions.json` and included in `--mcp-config` for the Claude CLI subprocess.
Due to a Claude CLI bug, the subprocess fails to discover tools from these
dynamically-added MCP servers — tools show as "registered but not appearing."

Users have worked around this by calling MCP servers directly via Bash, but
this bypasses the native tool interface. We need runtime MCP extensions to work
reliably in CLI mode without depending on the CLI subprocess's MCP connection
behavior.

## Solution

Proxy runtime MCP extension tools through the `curlycatclaw-skills` MCP server
subprocess. This subprocess already serves built-in skills and exec extensions
to the CLI. We extend it to also start MCP client connections for each `TypeMCP`
extension, discover their tools, and register them as proxy tools.

### Architecture change

**Before:**
```
CLI subprocess --> curlycatclaw-skills --> built-in skills + exec extensions
               --> runtime MCP server (FAILS due to CLI bug)
```

**After:**
```
CLI subprocess --> curlycatclaw-skills --> built-in skills + exec extensions
                                      --> proxy: runtime MCP extension tools
```

Runtime MCP extensions are no longer added to `--mcp-config`. They are only
accessed through the curlycatclaw-skills proxy.

## Design

### 1. MCP proxy in `mcp_server.go`

After registering built-in skills and exec extensions, `runMCPServer()` iterates
over `TypeMCP` extensions from the registry:

```
for each MCP extension in extReg.ByType(TypeMCP):
    1. Build filtered env (same dangerousEnvPrefixes blocklist)
    2. cmd := exec.CommandContext(ctx, ext.Command, ext.Args...)
    3. transport := &mcp.CommandTransport{Command: cmd}
    4. client := mcp.NewClient(...)
    5. session := client.Connect(ctx, transport)
    6. defer session.Close()
    7. tools := session.Tools(ctx)
    8. For each tool:
       - namespacedName := ext.Name + "__" + tool.Name
       - Register proxy handler on the MCP server
```

The proxy handler:
- Receives tool call from CLI subprocess via curlycatclaw-skills
- Injects `_user_context` (userID, chatID) into arguments
- Forwards to `session.CallTool()` on the extension's MCP session
- Returns formatted result

This reuses the exact MCP client pattern from `internal/mcp/manager.go:startServer`.

### 2. Remove runtime extensions from `buildMCPConfig()`

In `internal/session/actor.go`, remove lines 1260-1277 that add runtime MCP
extensions directly to the CLI's `--mcp-config`. This prevents:
- Duplicate tools (proxy + direct)
- Triggering the CLI bug

Config-level MCP servers (`cfg.MCP.Servers`) remain in `--mcp-config` unchanged —
they're static and not affected by the CLI bug.

### 3. Environment filtering

Duplicate the `dangerousEnvPrefixes` blocklist and `filterDangerousEnv` helper
in `mcp_server.go`. The blocklist is 3 entries (`LD_PRELOAD`, `LD_LIBRARY_PATH`,
`DYLD_*`) — too small to warrant a shared package.

## Namespacing

| Source | Name format | Example |
|--------|------------|---------|
| Built-in skills | Bare name | `note`, `remind` |
| Exec extensions | `ext__name` | `ext__my_script` |
| Proxied MCP extensions | `extname__toolname` | `paper-search__search_papers` |

No collisions: built-in skills use bare names (no `__`), exec extensions use
`ext__` prefix, and MCP extension names are unique per the registry.

## Lifecycle

### Startup

1. CLI subprocess starts, connects to curlycatclaw-skills
2. `runMCPServer()` loads `extensions.json`
3. For each `TypeMCP` extension: spawn process, handshake, discover tools
4. Register proxy tools, defer session cleanup
5. `server.Run()` blocks, serving tools over stdio

### Extension add/remove

1. `add_extension` skill persists to `extensions.json`
2. `extReloadFunc()` writes `.curlycatclaw-reload-needed`
3. Current turn completes with old tool set
4. Next message: `handleWithCLI()` detects reload flag
5. Kills old CLI subprocess (cascading: old curlycatclaw-skills dies, MCP connections close)
6. Spawns new CLI + new curlycatclaw-skills with updated extensions
7. New tools available

### Shutdown

- Normal: CLI exits, curlycatclaw-skills exits, deferred `session.Close()` runs, child processes killed
- Crash/SIGKILL: children get SIGPIPE when writing to broken pipe, same behavior as current design

### Connection failure

- If an MCP extension fails to connect: log warning, skip, continue
- Other tools remain available
- Retried automatically on next reload/restart

## Files to modify

| File | Change |
|------|--------|
| `cmd/curlycatclaw/mcp_server.go` | Add MCP proxy logic after exec extension registration |
| `internal/session/actor.go` | Remove runtime MCP extensions from `buildMCPConfig()` |
| Shared env filtering | Extract `filterDangerousEnv` or duplicate blocklist |

## Verification

1. **Unit test**: test proxy tool registration and namespacing in `mcp_server.go`
2. **Integration test**: add MCP extension via Telegram, verify tools appear after reload
3. **Manual test**:
   - Start curlycatclaw in CLI mode
   - Add an MCP extension: `add_extension(type=mcp, name=test, command=...)`
   - Send a new message
   - Verify the extension's tools appear and are callable
   - Remove the extension
   - Verify tools disappear after reload
4. **Lint + test**: `golangci-lint run && go test ./... -count=1`
