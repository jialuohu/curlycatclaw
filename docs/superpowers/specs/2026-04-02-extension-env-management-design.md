# Extension Environment Variable Management

## Context

MCP extensions (like paper-search-mcp) often need API keys to unlock full
functionality. Currently there's no way to set env vars on an existing extension
via Telegram. The user must remove the extension, re-add it with new env vars,
and trigger a reload. API keys are stored as plaintext in extensions.json.

The user wants to tell San Bao "set CORE_API_KEY for paper-search-mcp" and have
it just work, with the key encrypted at rest.

## Solution

Add `set_extension_env` and `unset_extension_env` skills that store values in
the existing encrypted credential store (AES-256-GCM via CURLYCATCLAW_MASTER_KEY)
and update the extension's env map with `encrypted:ref:` references. At proxy
time, references are resolved to plaintext before passing to the extension process.

## Design

### New skills

**`set_extension_env`**
- Input: `{name: string, key: string, value: string}`
- Stores encrypted: `credStore.Set("ext_<name>_<key>", value)`
- Updates extension env: `ext.Env[key] = "encrypted:ref:ext_<name>_<key>"`
- Persists via `registry.Update(name, ext)`
- Triggers reload via `reloadFunc()`
- Returns confirmation (never echoes the value back)

**`unset_extension_env`**
- Input: `{name: string, key: string}`
- Deletes credential: `credStore.Delete("ext_<name>_<key>")`
- Removes key from `ext.Env`
- Persists and reloads

### Extension registry: Update method

Add `Update(name string, mutate func(*Extension) error) error` to the registry.
Acquires lock, calls mutate on the existing extension, persists atomically.
Returns error if extension not found.

### Credential store access in MCP server subprocess

The curlycatclaw-skills subprocess (`runMCPServer`) needs access to the credential
store to:
1. Encrypt values when `set_extension_env` is called
2. Resolve `encrypted:ref:` values when spawning MCP extension processes

**Mechanism:** The main daemon passes `CURLYCATCLAW_MASTER_KEY` to the
curlycatclaw-skills subprocess via the env map in `buildMCPConfig`. This key is
already filtered out by `buildMCPExtEnv` (not in `baselineEnvAllowlist`), so
MCP extension processes never see it.

In `runMCPServer()`:
1. Read `CURLYCATCLAW_MASTER_KEY` from env
2. If set, create a `CredentialStore` at the standard credentials path
3. Pass the store to `InitExtensionSkills` for the new env management skills
4. Pass the store to `connectMCPExtension` for env resolution

### Env resolution at proxy time

`buildMCPExtEnv` gains an optional `*security.CredentialStore` parameter.
When resolving env vars, if a value starts with `encrypted:ref:`, it calls
`credStore.ResolveEnv(extEnv)` first, then applies the existing baseline
allowlist and dangerous-prefix filtering on the resolved values.

If `credStore` is nil (master key not set), `encrypted:ref:` values are
skipped with a warning log. The extension still starts but without the
encrypted env vars.

### System prompt update

Add to the "Installed MCP extensions" section:
- Instruction to use `set_extension_env` when the user provides an API key
- Instruction to never echo API key values back to the user

### Security properties

| Concern | Mitigation |
|---------|-----------|
| Master key in MCP subprocess | curlycatclaw-skills is same binary, trusted |
| Master key in extension process | Filtered by buildMCPExtEnv (not in allowlist) |
| API key in Telegram history | Can't prevent (user typed it). Skill never echoes it back. |
| API key in extensions.json | Stored as `encrypted:ref:` pointer, not plaintext |
| API key in credentials.enc | AES-256-GCM encrypted at rest |
| API key in conversation DB | Present in the message where user typed it. Redaction out of scope. |

## Files to modify

| File | Change |
|------|--------|
| `internal/extension/extension.go` | Add `Update` method to Registry |
| `internal/extension/skills.go` | New `set_extension_env`, `unset_extension_env` skills; `InitExtensionSkills` gains `credStore` param |
| `cmd/curlycatclaw/mcp_server.go` | Init credential store, pass to skills and proxy; `buildMCPExtEnv` gains credential resolution |
| `internal/session/actor.go` | Pass `CURLYCATCLAW_MASTER_KEY` in `buildMCPConfig` curlycatclaw-skills env |
| `cmd/curlycatclaw/main.go` | Pass credential store to `InitExtensionSkills` (direct API mode) |

## Verification

1. `golangci-lint run` and `go test ./... -count=1`
2. Rebuild container, tell San Bao: "set CORE_API_KEY=test123 for paper-search-mcp"
3. Verify `credentials.enc` contains the encrypted value
4. Verify `extensions.json` shows `encrypted:ref:ext_paper-search-mcp_CORE_API_KEY`
5. Verify paper-search-mcp subprocess receives `CORE_API_KEY=test123` in its env
6. Remove the key: "unset CORE_API_KEY for paper-search-mcp"
7. Verify credential deleted and env var removed
