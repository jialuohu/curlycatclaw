# TODOS

## Optimize buildMCPConfig caching

**What:** Only call `buildMCPConfig` when `GetOrCreate` needs to spawn a new subprocess, not on every message.

**Why:** `buildMCPConfig` reads `installed_plugins.json` + N `.mcp.json` files from disk on every incoming message. For existing alive subprocesses, the result is discarded by `GetOrCreate`. Currently < 1ms overhead with typical plugin counts, but wasteful.

**Context:** `handleWithCLI` (actor.go) always calls `buildMCPConfig`, but `GetOrCreate` (subprocess.go) only uses `params` when spawning. Now that `GetOrCreate` returns `isNew`, option (a) is easier: only build MCP config when `isNew` is true, or move the call inside `GetOrCreate` when spawn is needed. Option (b) cache + invalidate on reload signal is also viable.

**Depends on:** MCP extension proxy (v0.17.0, done). Now unblocked.
