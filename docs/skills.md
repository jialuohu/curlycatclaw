# Built-in Skills

| Skill | Description |
|-------|-------------|
| `web_search` | Search the web via DuckDuckGo |
| `save_note` | Save a note (user-scoped, persisted to SQLite) |
| `search_notes` | Search saved notes by keyword |
| `set_reminder` | Set a reminder with time, optional recurrence, Claude-powered prompt, and per-reminder model override |
| `list_reminders` | List pending/fired reminders |
| `cancel_reminder` | Cancel a scheduled reminder |
| `semantic_search` | Search conversation history and notes by meaning (requires Qdrant) |
| `remember_fact` | Save a persistent fact about you across all conversations |
| `forget_fact` | Remove a saved fact by ID |
| `list_facts` | List all persistent facts Claude remembers about you |
| `list_summaries` | View all stored conversation summaries with IDs and previews |
| `delete_summary` | Remove an incorrect or unwanted conversation summary by ID |
| `search_observations` | Search extracted observations by meaning (requires Qdrant) |
| `list_observations` | List all observations Claude has extracted about you |
| `get_observation` | Get full details of a specific observation by ID |
| `forget_observation` | Remove an extracted observation by ID |
| `search_entities` | Search for people, projects, files, or tools mentioned in observations |
| `install_plugin` | Install a Claude Code plugin (auto-searches for marketplace) |
| `uninstall_plugin` | Uninstall a Claude Code plugin |
| `list_plugins` | List installed Claude Code plugins |
| `enable_plugin` | Enable a previously disabled plugin |
| `disable_plugin` | Disable a plugin without uninstalling |
| `update_plugin` | Update one or all installed plugins to latest version |
| `add_marketplace` | Add a third-party plugin marketplace (GitHub repo) |
| `remove_marketplace` | Remove a marketplace and auto-uninstall its plugins |
| `list_marketplaces` | List configured plugin marketplaces |
| `add_extension` | Add a runtime MCP server, exec skill, or prompt skill |
| `remove_extension` | Remove a runtime extension by name |
| `list_extensions` | List all runtime-added extensions |
| `load_prompt_skill` | Load a prompt skill's SKILL.md instructions on demand |
| `set_extension_env` | Set an encrypted env var (API key) for an MCP extension |
| `unset_extension_env` | Remove an encrypted env var from an MCP extension |

Skills are registered alongside MCP tools -- Claude sees them all and picks the right one. Plugin skills require `cli_path` and `isolated_home` in `[claude]` config. Extensions are persisted to `extensions.json` and survive restarts. Wasm plugins load from `~/.curlycatclaw/skills/*.wasm` when enabled.

## Skill Types

CurlyCatClaw supports five types of skills:

- **Built-in** -- Go functions compiled into the binary. These include core skills like `web_search`, `save_note`, `set_reminder`, and all plugin/extension management skills. They have direct access to the database, vector store, and other internal services.

- **MCP** -- Tools exposed by external MCP servers connected via stdio transport. Configured in `[[mcp.servers]]` (e.g., Google Workspace, GitHub) or added at runtime via `add_extension`. Tools are namespaced as `server__tool` and discovered automatically.

- **Wasm** -- WebAssembly plugins loaded from `.wasm` files, running in a wazero sandbox with a capability-based security model. Capabilities include HTTP (with SSRF protection), database read (with enforced user scoping), and chat messaging. Hot-reloaded via fsnotify.

- **Exec** -- External executables loaded from directory trees via `[[skill_collections]]` config or `add_extension` with `type=exec`. Each invocation spawns a subprocess with a minimal sandboxed environment (PATH/HOME/TMPDIR only). Described by `skill.toml` descriptors.

- **Prompt** -- Markdown instruction files (SKILL.md + supporting files in a directory) added via `add_extension` with `type=prompt`. Loaded on demand via `load_prompt_skill`. Used for Claude-driven workflows that don't need code execution.
