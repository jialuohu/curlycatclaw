# Contributing to curlycatclaw

curlycatclaw is a personal AI agent assistant built in Go. Contributions, bug reports, and feature requests are welcome.

## Report a bug

Two ways:

1. **GitHub Issue** -- Use the [bug report form](../../issues/new?template=bug_report.yml) to file an issue directly.
2. **Tell the bot** -- Message curlycatclaw in Telegram and it will create a GitHub issue for you. Example: *"I found a bug: when I ask you to search the web, you time out every time."*

## GitHub Integration Setup

If you're setting up the GitHub integration for the bot to create issues:

```bash
# Required: personal access token with repo scope
export GITHUB_PERSONAL_ACCESS_TOKEN=ghp_...

# Create the labels used by issue templates
gh label create bug --color d73a4a
gh label create enhancement --color a2eeef
gh label create alpha --color fbca04

# Enable write mode: remove --read-only from GitHub MCP args in config.toml
```

## Contribute code

1. Fork the repo and create a branch.
2. Make your changes.
3. Run tests and lint locally before pushing:
   ```bash
   go test ./... -count=1
   golangci-lint run
   ```
4. Submit a pull request.

See [CLAUDE.md](CLAUDE.md) for full build, test, and lint instructions.
