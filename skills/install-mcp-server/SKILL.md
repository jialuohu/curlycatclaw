---
name: install-mcp-server
description: Install an HTTP MCP server from a GitHub repo as a managed Docker service. Use this whenever the user asks to install, set up, or add an MCP server from a GitHub URL, or mentions installing a Docker-based MCP service. This skill handles the full pipeline: reading the repo for Docker image info, registering the service, starting it, connecting it, and installing companion prompt skills. Always use this instead of manually calling add_extension for HTTP MCP servers.
---

# Install MCP Server

This skill walks you through installing an HTTP MCP server from a GitHub repository
as a managed Docker service in curlycatclaw. The goal is a single smooth flow where
the user says "install X" and everything just works: the Docker service starts, the
MCP connection is live, and tools are available immediately.

## Why this matters

Without this skill, you'll call `add_extension` without the Docker image info, the
server won't be running, and the user sees "service not started" with manual setup
instructions. That's a bad experience. The `add_extension` tool has `image` and `ports`
fields that trigger automatic Docker service registration and startup. This skill
ensures you always use them.

## The Pipeline

```
GitHub URL → Read README → Extract image + port → add_extension(image, ports)
                                                        ↓
                                              auto-register → auto-start → connect
                                                        ↓
                                              Install companion prompt skills
                                                        ↓
                                              Tools available immediately
```

## Step 1: Read the GitHub repo

When the user gives you a GitHub URL for an MCP server:

1. Fetch the README (use `github__get_file_contents` or similar)
2. Look for these pieces of information:
   - **Docker image name** (e.g., `xpzouying/xiaohongshu-mcp`). Check:
     - Docker Hub badge or pull command (`docker pull ...`)
     - `docker-compose.yml` image field
     - Dockerfile with a published image reference
     - GitHub Container Registry (`ghcr.io/...`)
     - README installation section
   - **Port number** the MCP server listens on (e.g., `18060`). Check:
     - `EXPOSE` in Dockerfile
     - Port mapping in docker-compose.yml
     - README mentions of port/endpoint
     - Default MCP endpoint URL (usually `/mcp` or `/sse`)
   - **MCP endpoint path** (usually `/mcp` or `/sse`)

3. If you can't find the Docker image name, ask the user. Don't guess.

## Step 2: Register the MCP server extension

Call `add_extension` with ALL of these fields:

```json
{
  "name": "<service-name>",
  "type": "mcp",
  "transport": "http",
  "url": "http://<service-name>:<port><path>",
  "image": "<docker-image>",
  "ports": {"<host-port>": "<container-port>"}
}
```

The `image` and `ports` fields are what trigger the auto-register pipeline:
- If the Docker service isn't in the catalog, it gets auto-registered
- The service starts automatically
- The MCP connection retries after startup
- Tools become available immediately

**URL hostname:** Use the service name (not `localhost`) as the hostname. Docker
Compose networking resolves service names. For example:
`http://xiaohongshu:18060/mcp` not `http://localhost:18060/mcp`.

If the service name contains characters that aren't valid DNS names, simplify it
(e.g., `xiaohongshu-mcp` becomes the service name `xiaohongshu`).

## Step 3: Install companion prompt skills

If the user also provided a skills repository (or the MCP server repo has a `skills/`
directory), install the prompt skills:

1. Read the skills directory to find all SKILL.md files
2. For each skill, download the SKILL.md content
3. Save to `/data/extension-wrappers/<skill-name>/SKILL.md`
   - Create the directory first: use Bash `mkdir -p /data/extension-wrappers/<name>`
   - Write the SKILL.md file to that directory
   - The path for `add_extension` is the **directory**, not the file
4. Register each with `add_extension`:
   ```json
   {
     "name": "<skill-name>",
     "type": "prompt",
     "command": "/data/extension-wrappers/<skill-name>",
     "description": "<from the SKILL.md frontmatter>"
   }
   ```

**Path rules (important):**
- Always use absolute paths starting with `/data/`
- Never use `~` or `$HOME` (they don't expand reliably in the container)
- Never use `/root/` (the container user may not be root)
- The correct base directory is `/data/extension-wrappers/`

## Step 4: Verify and report

After installation, verify:
1. The MCP server extension shows in `list_extensions`
2. Report what was installed: MCP server name, URL, number of tools, prompt skills

If the MCP server connection failed even with auto-start, report the error clearly
and suggest the user check:
- Is the Docker image name correct?
- Is the port correct?
- Does the updater sidecar have `ALLOWED_IMAGES` configured to allow this image?

## Example

User says: "Install xiaohongshu MCP from https://github.com/xpzouying/xiaohongshu-mcp
and skills from https://github.com/autoclaw-cc/xiaohongshu-mcp-skills"

You do:
1. Read both repos, find image `xpzouying/xiaohongshu-mcp`, port `18060`, path `/mcp`
2. `add_extension(name:"xiaohongshu", type:"mcp", transport:"http",
   url:"http://xiaohongshu:18060/mcp", image:"xpzouying/xiaohongshu-mcp",
   ports:{"18060":"18060"})`
3. Download each SKILL.md, save to `/data/extension-wrappers/<name>/SKILL.md`
4. Register each prompt skill via `add_extension(type:"prompt", ...)`
5. Report: "MCP server running, N tools available, M prompt skills installed"
