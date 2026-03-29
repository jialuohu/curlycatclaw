---
name: setup
description: |
  Set up curlycatclaw from scratch. Installs the binary, starts Qdrant,
  configures credentials, and verifies everything works. Run this after
  cloning the repo. Triggers on "setup", "install", "configure", or
  first-time setup requests.
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
or systemd?"). Do NOT use AskUserQuestion when free-text input is needed (API keys,
tokens, user IDs). Instead, ask the question in plain text and wait for the user's reply.

## 0. Introduction

Before asking for anything, explain what curlycatclaw is:

> **curlycatclaw** is a personal AI assistant that runs as a background service. It
> connects to Telegram as a chat interface, uses Claude as the LLM, and stores
> conversations + memories in SQLite with Qdrant for semantic search.
>
> This setup will install the curlycatclaw binary, start a Qdrant vector database
> (via Docker), configure your credentials, and verify everything works.
>
> You'll need three things ready:
> 1. An **Anthropic OAuth token** or **API key** (from console.anthropic.com)
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
- `INSTALL_METHOD` — `homebrew` or `github_releases` (decision is encoded in the script)
- `DOCKER` — `running`, `installed_not_running`, or `not_found`
- `DOCKER_SUDO` — whether `sudo docker` is needed for this session
- `CURLYCATCLAW_INSTALLED` / `CURLYCATCLAW_VERSION` / `LATEST_VERSION` — install/upgrade decision
- `QDRANT_RUNNING` — skip Qdrant setup if already running
- `CONFIG_EXISTS` — prompt before overwriting
- `SYSTEMD_AVAILABLE` — affects service start options
- `PORT_8080` — health endpoint port availability

Tell the user what was detected: "Detected: [OS] [ARCH], Docker [status], curlycatclaw [installed/not]."

## 2. Install curlycatclaw Binary

Based on the detection results:

**If `CURLYCATCLAW_INSTALLED=true`:**
- Compare `CURLYCATCLAW_VERSION` with `LATEST_VERSION`
- If same: "curlycatclaw is up to date. Skipping install."
- If different: "curlycatclaw [current] is installed but [latest] is available. Update?"
  Use AskUserQuestion: A) Update B) Keep current version

**If `CURLYCATCLAW_INSTALLED=false`:**

**If `INSTALL_METHOD=homebrew`:**
```bash
brew install jialuohu/homebrew-tap/curlycatclaw
```

**If `INSTALL_METHOD=github_releases`:**
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

If install fails, diagnose: permission denied (need sudo), brew not linked, download
URL 404 (check release tag format). Fix and retry.

## 3. Docker + Qdrant

**If `QDRANT_RUNNING=true`:** "Qdrant is already running. Skipping." Go to Step 4.

**If `DOCKER=not_found`:**

Use AskUserQuestion: "Docker is required to run Qdrant (the vector database for memory).
It requires sudo to install and is a ~500MB download. Install Docker?"
- A) Yes, install Docker
- B) I'll install it myself (link to https://docs.docker.com/engine/install/)

If A, on macOS: `brew install --cask docker` then `open -a Docker` and wait.
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

Say: "Paste your Anthropic OAuth token or API key. Get one at https://console.anthropic.com/settings/keys
if you don't have one.
- OAuth tokens are preferred (paste the token value directly)
- API keys start with `sk-ant-`"

Wait for user response. Validate:
- Trim leading/trailing whitespace
- If starts with `sk-ant-`: treat as API key, store as `ANTHROPIC_API_KEY`
- Otherwise: treat as OAuth token, store as `ANTHROPIC_AUTH_TOKEN`
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

## 5. Generate Config

**If `CONFIG_EXISTS=true`:**
Use AskUserQuestion: "Config file already exists at ~/.curlycatclaw/config.toml. What do you want to do?"
- A) Overwrite with new config
- B) Keep existing config (skip this step)

If B: skip to Step 6.

Write credentials to a temp file (secure, not visible in process list):

```bash
CREDS_FILE=$(mktemp /tmp/curlycatclaw-creds-XXXXXXXX.tmp)
chmod 600 "$CREDS_FILE"
cat > "$CREDS_FILE" << 'EOF'
ANTHROPIC_AUTH_TOKEN=<collected OAuth token, if applicable>
ANTHROPIC_API_KEY=<collected API key, if applicable>
TELEGRAM_TOKEN=<collected token>
TELEGRAM_USER_ID=<collected user id>
EOF
```

**Important:** Replace the `<collected ...>` placeholders with the actual values the
user provided. Include only the auth field that applies (ANTHROPIC_AUTH_TOKEN for OAuth
tokens, ANTHROPIC_API_KEY for API keys starting with `sk-ant-`). Use the Write tool
to create the temp file, or a heredoc in Bash.

Then run config.sh:

```bash
bash .claude/skills/setup/config.sh "$CREDS_FILE"
```

Parse the STATUS block. Verify `CONFIG_WRITTEN=true` and `PERMISSIONS=600`.

If permissions are not 600: `chmod 600 ~/.curlycatclaw/config.toml`

## 6. Start Service

**If `PORT_8080=in_use`:** Warn the user that port 8080 is already in use. Ask if they
want to pick a different port. If yes, use the Edit tool to change `port = 8080` in
the config file, then proceed.

Use AskUserQuestion: "How do you want to run curlycatclaw?"

**If `DOCKER=running` or `DOCKER=installed_not_running`:**
- A) Docker Compose (recommended, persistent, manages Qdrant too)
- B) Foreground (for testing, run in a separate terminal)
- C) systemd service (if `SYSTEMD_AVAILABLE=true` and `OS=linux`)

**Otherwise:**
- A) Foreground (run in a separate terminal)
- B) systemd service (if `SYSTEMD_AVAILABLE=true` and `OS=linux`)

**If Docker Compose (A with Docker):**

Generate a Docker-specific config file at `~/.curlycatclaw/config.docker.toml` that
uses `/data/curlycatclaw.db` for db_path and `qdrant:6334` for qdrant_addr. Mount it
into the container. Then:

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

If health check passes, proceed to Step 7. If timeout, check logs:
`docker compose logs curlycatclaw`.

**If Foreground (A):**

Tell the user: "Open a new terminal and run:
```
curlycatclaw --config ~/.curlycatclaw/config.toml
```
I'll check if it started..."

Then poll the health endpoint with a 30-second timeout:
```bash
for i in $(seq 1 30); do
  if curl -sf http://127.0.0.1:8080/health >/dev/null 2>&1; then
    echo "Health check passed!"
    break
  fi
  sleep 1
done
```

If health check passes, proceed to Step 7. If timeout, ask user to check the terminal
output for errors.

**If systemd (B):**

```bash
# Create system user
sudo useradd --system --create-home --home-dir /var/lib/curlycatclaw curlycatclaw 2>/dev/null || true

# Copy config
sudo mkdir -p /etc/curlycatclaw
sudo cp ~/.curlycatclaw/config.toml /etc/curlycatclaw/config.toml
sudo chown -R curlycatclaw:curlycatclaw /etc/curlycatclaw

# Update db_path for system user
sudo sed -i 's|db_path = .*|db_path = "/var/lib/curlycatclaw/curlycatclaw.db"|' /etc/curlycatclaw/config.toml
sudo chown -R curlycatclaw:curlycatclaw /var/lib/curlycatclaw

# Install and start service
sudo cp deploy/curlycatclaw.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now curlycatclaw
```

Verify with: `sudo systemctl status curlycatclaw`

If service fails to start, check: `sudo journalctl -u curlycatclaw -n 20`

## 7. Verify

Run the verification script:

```bash
bash .claude/skills/setup/verify.sh
```

Parse the STATUS block:

- **HEALTH=ok, QDRANT=ok, SERVICE=running:** "Setup complete! curlycatclaw is running.
  Send a message to your Telegram bot to test it."

- **HEALTH=fail:** Check if port is correct, if process is running, check logs.
  For systemd: `sudo journalctl -u curlycatclaw -n 20`
  For foreground: ask user to check terminal output.

- **QDRANT=fail:** Run `bash .claude/skills/setup/qdrant.sh health`. If unhealthy,
  restart: `bash .claude/skills/setup/qdrant.sh stop && bash .claude/skills/setup/qdrant.sh start`

- **SERVICE=stopped:** Re-check Step 6. Verify config path is correct.

- **CONFIG_PERMS_OK=false:** "Warning: config file permissions are too open (contains API keys).
  Fixing..." then `chmod 600 ~/.curlycatclaw/config.toml`

## Troubleshooting

**Binary not found after install:** Check PATH includes `/usr/local/bin`. On macOS with
Homebrew, the binary is in the brew prefix.

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
