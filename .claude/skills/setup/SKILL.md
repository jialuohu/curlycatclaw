---
name: setup
description: |
  Set up curlycatclaw from scratch. Installs the binary, starts Qdrant,
  configures credentials, pulls the Ollama embedding model, and verifies
  everything works. Run this after cloning the repo. Triggers on "setup",
  "install", "configure", or first-time setup requests.
allowed-tools:
  - Bash
  - Read
  - Write
  - Edit
  - AskUserQuestion
---

# curlycatclaw Setup

Run setup steps automatically. Only pause when user action is required (authenticating
Telegram, pasting API keys). Helper scripts are in `.claude/skills/setup/` and emit
structured `--- STATUS ---` blocks for parsing.

**Principle:** When something is broken or missing, fix it. Don't tell the user to go
fix it themselves unless it genuinely requires their manual action (e.g. creating a
Telegram bot, pasting an API key). If a dependency is missing, install it. If a service
won't start, diagnose and repair.

**UX Note:** Use `AskUserQuestion` for multiple-choice questions only (e.g. "Foreground
or Docker?"). Do NOT use AskUserQuestion when free-text input is needed (API keys,
tokens, user IDs). Instead, ask the question in plain text and wait for the user's reply.

## 0. Introduction

Before asking for anything, explain what curlycatclaw is:

> **curlycatclaw** is a personal AI assistant that runs as a background service. It
> connects to Telegram as a chat interface, uses Claude as the LLM, and stores
> conversations + memories in SQLite with Qdrant for semantic search.
>
> This setup will install curlycatclaw, start Qdrant (vector database), select an
> embedding engine (GPU-aware auto-selection between Ollama, Voyage AI, or FNV
> hash), configure your credentials, and verify everything works.
>
> During configuration, you'll choose a setup profile:
> - **Quick start** — just the essentials, fastest path to first reply
> - **With memory** — adds semantic search and conversation memory (recommended)
> - **Full customization** — walk through every feature (MCP, voice, ingestion, etc.)
>
> **Prerequisites:** Docker (recommended) or Go 1.25+.
>
> You'll need three things ready:
> 1. An **Anthropic OAuth token** (from `claude setup-token`) or **API key** (from console.anthropic.com)
> 2. A **Telegram bot token** (from @BotFather)
> 3. Your **Telegram user ID** (from @userinfobot)

Then proceed to Step 1.

## 1. Pre-flight Detection

Run the detection script and parse the output:

```bash
bash .claude/skills/setup/detect.sh
```

Parse every `KEY=VALUE` line between `--- STATUS ---` and `--- END ---`. These values
drive all subsequent decisions. Key fields:

- `OS` / `ARCH` — platform identification
- `INSTALL_METHOD` — `docker` or `github_releases` (Docker preferred when available)
- `DOCKER` — `running`, `installed_not_running`, or `not_found`
- `DOCKER_SUDO` — whether `sudo docker` is needed for this session
- `CURLYCATCLAW_INSTALLED` / `CURLYCATCLAW_VERSION` / `LATEST_VERSION` — install/upgrade decision
- `CLAUDE_CLI_INSTALLED` / `CLAUDE_CLI_PATH` / `CLAUDE_CLI_VERSION` — Claude CLI for subprocess mode
- `HAS_BROWSER` — whether a browser is available (for OAuth flow)
- `QDRANT_RUNNING` — skip Qdrant setup if already running
- `CONFIG_EXISTS` — prompt before overwriting
- `PORT_18080` — health endpoint port availability
- `GPU_TYPE` — `nvidia`, `apple_silicon`, `nvidia_no_driver`, `amd_no_driver`, `intel_igpu`, or `none`
- `GPU_NAME` — human-readable GPU name (empty if none detected)
- `OLLAMA_RUNNING` — whether Ollama is already running

Tell the user what was detected: "Detected: [OS] [ARCH], Docker [status], curlycatclaw [installed/not]."

## 2. Install curlycatclaw Binary

Based on the detection results:

**If `INSTALL_METHOD=docker`:**
Docker detected. curlycatclaw runs via `docker compose up -d --build`, no binary install
needed. Skip to Step 4 (Collect Credentials).

**If `CURLYCATCLAW_INSTALLED=true`:**
- Compare `CURLYCATCLAW_VERSION` with `LATEST_VERSION`
- If same: "curlycatclaw is up to date. Skipping install."
- If different: "curlycatclaw [current] is installed but [latest] is available. Update?"
  Use AskUserQuestion: A) Update B) Keep current version

**If `CURLYCATCLAW_INSTALLED=false` and `INSTALL_METHOD=github_releases`:**

Use `LATEST_VERSION`, `OS`, and `ARCH` to construct the download URL:
```bash
VERSION="<LATEST_VERSION without v prefix>"
curl -Lo /tmp/curlycatclaw.tar.gz "https://github.com/jialuohu/curlycatclaw/releases/download/v${VERSION}/curlycatclaw_${VERSION}_${OS}_${ARCH}.tar.gz"
tar xzf /tmp/curlycatclaw.tar.gz -C /tmp curlycatclaw
sudo mv /tmp/curlycatclaw /usr/local/bin/curlycatclaw
rm /tmp/curlycatclaw.tar.gz
```

**Verify:**
```bash
curlycatclaw --version
```

If install fails, diagnose: permission denied (need sudo), download URL 404 (check
release tag format). Fix and retry.

## 3. Docker + Qdrant + Ollama

**If `INSTALL_METHOD=docker`:** Qdrant and Ollama are bundled in docker-compose. Skip to Step 4.
After the service starts (Step 6), pull the embedding model:
```bash
docker compose exec ollama ollama pull bge-m3
```

**If `QDRANT_RUNNING=true`:** "Qdrant is already running. Skipping." Go to Step 4.

**If `DOCKER=not_found`:**

Use AskUserQuestion: "Docker is required to run Qdrant (the vector database for memory).
It requires sudo to install and is a ~500MB download. Install Docker?"
- A) Yes, install Docker
- B) I'll install it myself (link to https://docs.docker.com/engine/install/)

If A, on macOS: download Docker Desktop from https://www.docker.com/products/docker-desktop/ then `open -a Docker` and wait.
If A, on Linux: `curl -fsSL https://get.docker.com | sh` then `sudo usermod -aG docker $USER`.

Note: on Linux, the docker group change is not active in the current session.
Set `DOCKER_CMD="sudo docker"` for this session's Qdrant commands.

**If `DOCKER=installed_not_running`:**
- macOS: `open -a Docker` and wait 15s, re-check with `docker info`
- Linux: `sudo systemctl start docker`

**Start Qdrant:**

Determine docker command based on `DOCKER_SUDO`:
- If `DOCKER_SUDO=true` or docker group not yet active: use `DOCKER_CMD="sudo docker"`
- Otherwise: use `DOCKER_CMD="docker"`

```bash
DOCKER_CMD="<docker or sudo docker>" bash .claude/skills/setup/qdrant.sh start
```

If start fails, check:
- Port conflict: `ss -tlnp | grep ':6333\|:6334'`
- Existing container with different name: `docker ps -a | grep qdrant`
- Pull failure: network issue, retry

## 4. Collect Credentials

Ask for each credential one at a time in plain text. Trim whitespace from the user's
response before validating.

**4a. Anthropic authentication**

**If `CLAUDE_CLI_INSTALLED=true` AND `HAS_BROWSER=true`:**
Say: "Claude CLI detected. Claude subscription (recommended) gives you unlimited usage.
Type `! claude setup-token` below to start the OAuth flow. When it completes, paste the
token it shows. Or paste an API key from https://console.anthropic.com/settings/keys"

**If `CLAUDE_CLI_INSTALLED=true` AND `HAS_BROWSER=false`:**
Say: "Claude CLI detected, but you're on a headless server (no browser detected).
Run `claude setup-token` on your local machine where you have a browser, then paste the
token here. Or paste an API key from https://console.anthropic.com/settings/keys"

**If `CLAUDE_CLI_INSTALLED=false`:**
Say: "No Claude CLI detected. Paste your Anthropic API key from
https://console.anthropic.com/settings/keys"

Wait for user response. Validate:
- Trim leading/trailing whitespace
- If starts with `sk-ant-oat`: treat as OAuth token from `claude setup-token`, store as
  `ANTHROPIC_AUTH_TOKEN`. Auto-set `CLAUDE_CLI_PATH` from the detect.sh `CLAUDE_CLI_PATH`
  value. Both `ANTHROPIC_AUTH_TOKEN` and `CLAUDE_CLI_PATH` go into the creds temp file.
- If starts with `sk-ant-`: treat as API key, store as `ANTHROPIC_API_KEY`
- If empty: "Please paste a valid token or API key. Try again."

**4b. Telegram bot token**

Say: "Paste your Telegram bot token. To create a bot:
1. Open Telegram and message @BotFather
2. Send `/newbot` and follow the prompts
3. Copy the token it gives you (looks like `123456789:ABCdef...`)"

Wait for user response. Validate:
- Trim leading/trailing whitespace
- Must match pattern: digits, colon, alphanumeric/dash/underscore
- If invalid: "That doesn't look like a Telegram bot token. It should look like `123456789:ABCdef...`. Try again."

**4c. Telegram user ID**

Say: "Paste your Telegram user ID. To find it:
1. Open Telegram and message @userinfobot
2. It will reply with your user ID (a number like `123456789`)"

Wait for user response. Validate:
- Trim leading/trailing whitespace
- Must be numeric (digits only)
- If invalid: "That doesn't look like a Telegram user ID (should be a number). Try again."

## 5. Choose Embedding Engine

Parse `GPU_TYPE` and `GPU_NAME` from the detect.sh output.

**Auto-selection with override:**

If `GPU_TYPE` is `nvidia`, `amd`, or `apple_silicon`:
  - Auto-select: Ollama bge-m3
  - Use AskUserQuestion: "Detected: [GPU_NAME]. Using Ollama bge-m3 for best quality embeddings (1024 dimensions, runs locally on your GPU, free)."
  - Options:
    - A) Keep Ollama (recommended)
    - B) Use FNV hash instead (384d, lower quality, zero dependencies)
    - C) Use Voyage AI instead (512d, cloud API, requires API key, costs money)

If `GPU_TYPE` is `nvidia_no_driver` or `amd_no_driver`:
  - Auto-select: FNV
  - Use AskUserQuestion: "Detected [GPU_NAME] but no GPU driver found. Ollama would run on CPU (slower). Recommending FNV for now. Install GPU drivers later for better embeddings."
  - Options:
    - A) Use Ollama anyway (runs on CPU)
    - B) Use FNV hash (recommended)
    - C) Use Voyage AI instead (512d, cloud API, requires API key, costs money)

If `GPU_TYPE` is `intel_igpu` or `none`:
  - Auto-select: FNV
  - Use AskUserQuestion: "No dedicated GPU detected. Using FNV hash embeddings (384d, instant, zero dependencies)."
  - Options:
    - A) Keep FNV hash (recommended)
    - B) Use Ollama anyway (runs on CPU, slower)
    - C) Use Voyage AI instead (512d, cloud API, requires API key, costs money)

If user picks C (Voyage): Ask for Voyage API key. Validate starts with `pa-`.

Store `EMBEDDER_CHOICE` (and `VOYAGE_API_KEY` if applicable) in the creds temp file.
Also store `INSTALL_METHOD=docker` or `INSTALL_METHOD=github_releases`.

If Ollama chosen: write `COMPOSE_PROFILES=ollama` to `.env` in the same directory as `docker-compose.yml`. Docker Compose reads `.env` automatically, so `docker compose up -d` starts Ollama with no flags needed. If the updater is also configured, use `COMPOSE_PROFILES=ollama,updater`.

## 6. Interactive Config Generation

<!-- MAINTENANCE: Keep TOML templates in sync with config/config.go and config.toml.example -->

This step builds config.toml interactively through conversation. Only write sections
the user explicitly enables — omitted sections use code defaults from config/config.go.

### 6a. Existing Config Handling

**If `CONFIG_EXISTS=true`:**
Back up the existing config first (timestamped to avoid clobbering previous backups):
```bash
cp ~/.curlycatclaw/config.toml ~/.curlycatclaw/config.toml.bak.$(date +%s)
```

Use AskUserQuestion: "Config file already exists at ~/.curlycatclaw/config.toml (backed up). What do you want to do?"
- A) Overwrite with new config
- B) Keep existing config (skip to Step 7)

If B: skip to Step 7.

### 6b. Profile Selection

Use AskUserQuestion: "How would you like to configure curlycatclaw?"
- A) **Quick start** — Just the essentials. Fastest path to your first Telegram reply.
- B) **With memory** (recommended) — Adds semantic search and conversation memory. Most users want this.
- C) **Full customization** — Walk through every feature: MCP integrations, voice, knowledge ingestion, self-update, and more.

**If B (With memory):** Follow up with AskUserQuestion:
- A) **Use defaults** — Write memory config with sensible defaults, no extra questions
- B) **Customize** — Walk through memory and vector search settings

After any profile selection, ask in plain text: "Would you also like to enable any of
these advanced features? (You can skip this entirely by saying 'no')"
List: MCP servers (GitHub, Google Workspace, Brave Search), voice transcription, knowledge
ingestion (Gmail, Obsidian, Notion), self-update system, self-evaluation pipeline.

If the user mentions any, include those sections in Step 6e below.

### 6c. Required Sections (all profiles)

Build these TOML sections from credentials collected in Steps 4-5. Use the exact values
the user provided. **Input validation rules:**
- Reject any value containing double-quote characters `"` (TOML injection prevention)
- Telegram user ID must be numeric (digits only)
- Trim leading/trailing whitespace from all values
- Strip any embedded newlines or spaces from OAuth tokens and API keys (pasting can introduce line breaks)

**Timezone:** Use the auto-detected timezone from detect.sh. Confirm with the user:
"Detected timezone: [TIMEZONE]. Is this correct?" If not, ask for the correct IANA
timezone (e.g., `America/New_York`, `Europe/London`).

**Thinking effort:** Ask in plain text: "Claude reasoning depth? Options: low, medium
(recommended, good balance), high (extended thinking, slower), max (maximum reasoning, slowest)"

**Show tool calls:** Use AskUserQuestion: "Show tool call notifications in Telegram?"
- A) Yes (recommended) — see what the bot is doing
- B) No — cleaner messages

**Determine paths based on `INSTALL_METHOD`:**
- If `docker`: `db_path = "/data/curlycatclaw.db"`, `cli_path = "/usr/local/bin/claude"` (container path, NOT the host path)
- If `github_releases`: `db_path = "$HOME/.curlycatclaw/curlycatclaw.db"` (expand `$HOME` to actual path), `cli_path` from detect.sh `CLAUDE_CLI_PATH` value

**Important:** For Docker installs with CLI subprocess mode, always use `/usr/local/bin/claude`
as the cli_path. The host machine's Claude CLI path (e.g., `/Users/you/.local/bin/claude`)
does not exist inside the container.

Assemble the required TOML:

```toml
timezone = "<TIMEZONE>"

[claude]
# For OAuth token mode:
cli_path    = "<CLI_PATH>"
oauth_token = "<ANTHROPIC_AUTH_TOKEN>"
# For API key mode:
api_key = "<ANTHROPIC_API_KEY>"
model   = "claude-opus-4-6"
thinking_effort = "<low|medium|high|max>"

[telegram]
token = "<TELEGRAM_TOKEN>"
allowed_user_ids = [<TELEGRAM_USER_ID>]
show_tool_calls = <true|false>

[storage]
db_path = "<DB_PATH>"

[health]
enabled = true
port    = 18080
```

Include only the applicable Claude auth fields (cli_path+oauth_token OR api_key, never both).

**Agent personality:** Use AskUserQuestion: "Choose your bot's personality:"
- A) **Default assistant** — "You are a helpful personal assistant." Clean and neutral.
- B) **Da Bao / 大宝** — A warm, goofy orange cat who speaks Chinese by default and mixes in English for tech terms. Competent but playful.
- C) **Custom** — Write your own personality in a markdown file.

**If A:** No personality section needed (the default is built in).

**If B:** Write the 大宝 personality from the bundled example file.

**Determine personality file path based on `INSTALL_METHOD`:**
- If `docker`: personality file path = `/data/personality.md`
- If `github_releases`: personality file path = `$HOME/.curlycatclaw/personality.md` (expand `$HOME`)

Copy `personality-dabao.md.example` to the personality file path (or write its content
directly). Then add to the config:

```toml
[personality]
file = "<PERSONALITY_FILE_PATH>"
```

**If C:** Tell the user: "Create a markdown file with your personality spec. See
`personality.md.example` for the format (identity, voice, language, behavioral rules).
Then add to config.toml:"

```toml
[personality]
file = "<absolute path to your personality.md>"
```

If using Docker, remind them: "The file must be inside the mounted config directory
(e.g., `~/.curlycatclaw/personality.md` on the host, which maps to `/data/personality.md`
in the container). Use the container path in config.toml."

### 6d. Memory Sections (With memory + Full customization)

**Determine addresses based on `INSTALL_METHOD`:**
- If `docker`: `qdrant_addr = "qdrant:6334"`, `ollama_url = "http://ollama:11434"`
- If `github_releases`: `qdrant_addr = "localhost:6334"`, `ollama_url = "http://localhost:11434"`

**If "With memory — Use defaults":** Write these sections with no further questions:

```toml
[vector]
enabled     = true
qdrant_addr = "<QDRANT_ADDR>"
embedder    = "<EMBEDDER_CHOICE from Step 5>"
# If ollama:
ollama_url   = "<OLLAMA_URL>"
ollama_model = "bge-m3"
ollama_dim   = 1024
# If voyage:
voyage_api_key = "<VOYAGE_API_KEY>"
voyage_model   = "voyage-3-lite"
voyage_dim     = 512
# If fnv: no extra fields needed

[memory]
enabled = true
```

**If "With memory — Customize" or Full customization:** Present memory settings with
defaults and let the user adjust. Ask about observations: "Enable automatic observation
extraction? This extracts facts from your conversations for long-term memory."
Use AskUserQuestion: A) Enable (recommended) B) Skip

If enabled, add:
```toml
[memory.observations]
enabled = true
```

### 6e. Advanced Sections (Full customization + "anything else" selections)

Present each section as "Enable X? Yes/Skip" using AskUserQuestion. **Dependency gating:**
check prerequisites before enabling a section.

**Logging:**
Use AskUserQuestion: "Configure logging? A) Defaults (info level) B) Customize C) Skip"
If A or B, write `[logging]` section. If B, ask for level (debug/info/warn/error) and
format (text/json).

**Voice transcription:**
Use AskUserQuestion: "Enable voice message transcription? Requires an OpenAI API key. A) Enable B) Skip"
If A: ask for OpenAI API key in plain text. Write:
```toml
[voice]
enabled = true
openai_api_key = "<KEY>"
stt_model = "whisper-1"
```

**MCP Servers:**
Use AskUserQuestion: "Add MCP server integrations? A) Add a server B) Skip"
If A, present presets:
- **GitHub** — repos, issues, PRs, CI status. Needs: GitHub Personal Access Token (classic).
- **Google Workspace** — Gmail, Calendar, Drive. Needs: GWS credentials.
  **Note:** GWS MCP requires the `curlycatclaw-gws-mcp` binary. In Docker, it must be
  built separately or mounted into the container. The pre-built GHCR image does NOT
  include it. If Docker, warn the user about this.
- **Google Maps** (experimental) — place search, weather, directions via remote HTTP MCP.
  Needs: Google Maps Platform API key from Google Cloud Console.
  Enable Places API (New), Routes API, Geocoding API. Free tier: $200/month credit.
- **Custom** — any MCP server. Needs: name, command, args, env vars.

For GitHub, first ask about access level using AskUserQuestion:
"How do you want to use GitHub from Telegram?
A) Read + write (recommended) — browse repos, check CI, AND create issues from Telegram
B) Read-only — browse repos, check CI, read issues/PRs only"

**If A (read + write):**
Ask for the PAT in plain text with this guidance:
"Create a Classic PAT at https://github.com/settings/tokens/new

Required scopes:
  - `repo` (full access — needed to create issues from Telegram)
  - `read:org` (read org membership, needed if your repos are in an org)

The `repo` scope with full access lets the bot create GitHub issues when you report
bugs through Telegram. Without it, issue creation will fail with a permission error.

Paste your token (starts with ghp_):"

Strip embedded whitespace. Write config WITHOUT `--read-only`:
```toml
[[mcp.servers]]
name    = "github"
command = "github-mcp-server"
args    = ["stdio", "--toolsets", "repos,issues,pull_requests,actions,users"]
[mcp.servers.env]
GITHUB_PERSONAL_ACCESS_TOKEN = "<token>"
```

Then ask for the repo: "What's your GitHub repo? (format: owner/repo, e.g. jialuohu/curlycatclaw)"
Parse owner and repo from the response. Write:
```toml
[github]
owner = "<owner>"
repo  = "<repo>"
```

**If B (read-only):**
Ask for the PAT in plain text with this guidance:
"Create a Classic PAT at https://github.com/settings/tokens/new

Required scopes for read-only mode:
  - `repo` (read access to private repos, issues, PRs, actions)
  - `read:org` (read org membership, needed if your repos are in an org)

Paste your token (starts with ghp_):"

Strip embedded whitespace. Write config with `--read-only`:
```toml
[[mcp.servers]]
name    = "github"
command = "github-mcp-server"
args    = ["stdio", "--toolsets", "repos,issues,pull_requests,actions,users", "--read-only"]
[mcp.servers.env]
GITHUB_PERSONAL_ACCESS_TOKEN = "<token>"
```
No `[github]` section needed for read-only mode (issue creation is not available).

For Google Workspace, ask: "How do you want to provide GWS credentials?"
- Paste the JSON content (for headless servers where you can't browse to a file)
- Provide the file path (if the credentials file is already on disk)
If the user pastes JSON content, write it to `~/.curlycatclaw/gws-credentials.json`
(with `chmod 600`) and use that path in the config. If they provide a path, use it
directly (deployment-aware: Docker uses `/data/gws-credentials.json`).

For Google Maps, ask for the API key in plain text. Strip embedded whitespace.
Write:
```toml
[[mcp.servers]]
name      = "google-maps"
transport = "http"
url       = "https://mapstools.googleapis.com/mcp"
[mcp.servers.headers]
X-Goog-Api-Key = "<key>"
```

Write the `[[mcp.servers]]` block for each server.
After each server: "Add another MCP server? A) Yes B) Done"

**Projects:**
Ask in plain text: "Add project directories for CLI work? Enter name and path, or 'skip'."
Paths must be deployment-aware (Docker: `/data/projects/...`, bare-metal: actual paths).
Write `[[projects]]` blocks. Validate paths exist before writing.

**Skill collections:**
Ask in plain text: "Add external skill collection paths? Enter path or 'skip'."
Write `[[skill_collections]]` blocks. Deployment-aware paths.

**Self-update system:**
**Gate:** "The self-update system requires the updater sidecar container and a shared
secret (UPDATER_SECRET in .env). Are these set up?"
Use AskUserQuestion: A) Yes, enable self-update B) Skip
If A: write:
```toml
[update]
enabled     = true
updater_url = "http://curlycatclaw-updater:8081"
auto_update = false
schedule    = "0 3 * * 0"
```
If Docker and updater profile not yet in COMPOSE_PROFILES, offer to add it.

**Self-evaluation pipeline:**
Use AskUserQuestion: "Enable self-evaluation? Scores conversations and suggests memory improvements. A) Enable B) Skip"
If A: write:
```toml
[eval]
enabled        = true
schedule       = "0 3 * * *"
lookback_hours = 24
score_threshold = 0.6
```

**Knowledge ingestion:**
Use AskUserQuestion: "Set up knowledge source ingestion? A) Gmail B) Obsidian vault C) Notion D) Skip"
Allow multiple selections.

- **Gmail:** **Gate:** requires GWS MCP server configured (check if `gws` is in the MCP servers
  written above). If not configured, warn and skip. Write `[[ingest.sources]]` with
  `type = "gmail"`, ask about trust level and importance threshold.
- **Obsidian:** ask for vault root directory path (deployment-aware). Write `[[ingest.sources]]`
  with `type = "file"`, `patterns = ["*.md"]`.
- **Notion:** **Gate:** requires Notion MCP server. Write `[[ingest.sources]]` with `type = "notion"`.

### 6f. Assemble, Validate, and Write

1. Assemble the complete TOML string from all sections above, in this order:
   `timezone` → `[claude]` → `[telegram]` → `[storage]` → `[vector]` → `[memory]` →
   `[memory.observations]` → `[[mcp.servers]]` → `[health]` →
   `[[projects]]` → `[logging]` → `[[skill_collections]]` → `[voice]` →
   `[[ingest.sources]]` → `[update]` → `[eval]`

2. Write the config using the Write tool:
   ```
   Write to ~/.curlycatclaw/config.toml
   ```

3. Secure permissions:
   ```bash
   chmod 600 ~/.curlycatclaw/config.toml
   ```

4. Validate if possible (binary must be installed for Docker builds, skip for first-time Docker):
   ```bash
   curlycatclaw --validate-config --config ~/.curlycatclaw/config.toml
   ```
   If validation fails: show the error to the user. Offer to fix the issue. If
   CONFIG_EXISTS was true, offer to restore the backup:
   Use AskUserQuestion: "Config validation failed. Restore your previous config from backup?"
   - A) Yes, restore backup
   - B) No, I'll fix the issue manually
   If A: `cp ~/.curlycatclaw/config.toml.bak.* ~/.curlycatclaw/config.toml` (use the most recent backup)

5. Report success: "Config written to ~/.curlycatclaw/config.toml (permissions: 600)."

**Non-interactive fallback:** If the user explicitly asks for non-interactive setup or
says "just generate defaults", fall back to config.sh. The script handles its own
credential cleanup via an EXIT trap:
```bash
bash .claude/skills/setup/config.sh "$CREDS_FILE"
```

## 7. Start Service

**If `PORT_18080=in_use`:** Warn the user that port 18080 is already in use. Ask if they
want to pick a different port. If yes, use the Edit tool to change `port = 18080` in
the config file, then proceed.

**If `INSTALL_METHOD=docker`:**

The same `~/.curlycatclaw/config.toml` is used. Docker overrides paths via environment
variables (`CURLYCATCLAW_DB_PATH`, `CURLYCATCLAW_QDRANT_ADDR`, `CURLYCATCLAW_OLLAMA_URL`,
`CURLYCATCLAW_CLI_PATH`) defined in `docker-compose.yml`. This handles everything
including Qdrant and Ollama:

```bash
docker compose up -d --build
```

Wait for health check:
```bash
for i in $(seq 1 60); do
  if docker compose ps | grep -q "healthy"; then
    echo "Health check passed!"
    break
  fi
  sleep 2
done
```

If health check passes:

If `EMBEDDER_CHOICE` is `ollama` and `INSTALL_METHOD` is `docker`:
```bash
docker compose exec ollama ollama pull bge-m3
```
If pull fails: warn the user and note that embeddings will use FNV fallback until the model is available.

Then proceed to Step 8. If timeout, check logs:
`docker compose logs curlycatclaw`.

**If `INSTALL_METHOD=github_releases`:**

Use AskUserQuestion: "How do you want to run curlycatclaw?"

**If `DOCKER=running` or `DOCKER=installed_not_running`:**
- A) Docker Compose (recommended, persistent, manages Qdrant too)
- B) Foreground (for testing, run in a separate terminal)

**Otherwise:**
- A) Foreground (run in a separate terminal)

**If Docker Compose (A with Docker):**

```bash
docker compose up -d --build
```

Wait for health check:
```bash
for i in $(seq 1 60); do
  if docker compose ps | grep -q "healthy"; then
    echo "Health check passed!"
    break
  fi
  sleep 2
done
```

If health check passes, proceed to Step 8. If timeout, check logs:
`docker compose logs curlycatclaw`.

**If Foreground (A or only option):**

Tell the user: "Open a new terminal and run:
```
curlycatclaw --config ~/.curlycatclaw/config.toml
```
I'll check if it started..."

Then poll the health endpoint with a 30-second timeout:
```bash
for i in $(seq 1 30); do
  if curl -sf http://127.0.0.1:18080/health >/dev/null 2>&1; then
    echo "Health check passed!"
    break
  fi
  sleep 1
done
```

If health check passes, proceed to Step 8. If timeout, ask user to check the terminal
output for errors.

## 8. Verify

Run the verification script:

```bash
bash .claude/skills/setup/verify.sh
```

Parse the STATUS block:

- **HEALTH=ok, QDRANT=ok, SERVICE=running:** "Setup complete! curlycatclaw is running.
  Send a message to your Telegram bot to test it."

- **HEALTH=fail:** Check if port is correct, if process is running, check logs.
  For Docker: `docker compose logs curlycatclaw --tail 20`
  For foreground: ask user to check terminal output.

- **QDRANT=fail:** Run `bash .claude/skills/setup/qdrant.sh health`. If unhealthy,
  restart: `bash .claude/skills/setup/qdrant.sh stop && bash .claude/skills/setup/qdrant.sh start`

- **SERVICE=stopped:** Re-check Step 7. Verify config path is correct.

- **CONFIG_PERMS_OK=false:** "Warning: config file permissions are too open (contains API keys).
  Fixing..." then `chmod 600 ~/.curlycatclaw/config.toml`

## Troubleshooting

**Binary not found after install:** Check PATH includes `/usr/local/bin`.

**Qdrant won't start:** Check Docker is running (`docker info`). Check port conflicts.
Check container logs: `docker logs curlycatclaw-qdrant`.

**Config validation error:** The binary validates config on startup. Common issues:
- Missing API key: re-run Step 4a
- Invalid timezone: edit config and set to a valid IANA timezone
- db_path uses `~`: must be an absolute path

**No response from Telegram bot:** Check the bot token is correct. Make sure your
user ID is in `allowed_user_ids`. Check logs for authentication errors.

**Docker permission denied:** If `docker` commands fail with permission denied:
- Use `sudo docker` as a workaround
- Or log out and back in (docker group membership activates on new session)
- Or run: `newgrp docker`
