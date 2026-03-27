# Upgrading CurlyCatClaw

## Binary Upgrade

1. Download the new binary from [GitHub Releases](https://github.com/jialuohu/curlycatclaw/releases) or build from source:

   ```bash
   go build -ldflags "-X main.version=vX.Y.Z" -o curlycatclaw ./cmd/curlycatclaw
   ```

2. Stop the service:

   ```bash
   sudo systemctl stop curlycatclaw
   ```

3. Replace the binary:

   ```bash
   sudo cp curlycatclaw /usr/local/bin/curlycatclaw
   ```

4. Verify the new version:

   ```bash
   curlycatclaw --version
   ```

5. Start the service:

   ```bash
   sudo systemctl start curlycatclaw
   ```

6. Check logs:

   ```bash
   journalctl -u curlycatclaw -f
   ```

## Downtime

The Telegram bot is briefly unavailable during the restart (typically under 5 seconds). Pending messages from Telegram will be delivered on the next long-poll after the bot reconnects.

## Config Changes

If the new version introduces new config options, update `/etc/curlycatclaw/config.toml` before restarting. Refer to `config.toml.example` in the release for new options.

## Rollback

To roll back, stop the service, replace the binary with the previous version, and restart.
