# TODOS

## Optimize buildMCPConfig caching

**What:** Only call `buildMCPConfig` when `GetOrCreate` needs to spawn a new subprocess, not on every message.

**Why:** `buildMCPConfig` reads `installed_plugins.json` + N `.mcp.json` files from disk on every incoming message. For existing alive subprocesses, the result is discarded by `GetOrCreate`. Currently < 1ms overhead with typical plugin counts, but wasteful.

**Context:** `handleWithCLI` (actor.go:846) always calls `buildMCPConfig`, but `GetOrCreate` (subprocess.go:234) only uses `params` when spawning. Could either: (a) move the call inside `GetOrCreate` when spawn is needed, or (b) cache the result and invalidate on reload signal.

**Depends on:** Plugin MCP discovery fix (this PR).
