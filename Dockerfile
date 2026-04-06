FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o /curlycatclaw ./cmd/curlycatclaw
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o /curlycatclaw-gws-mcp ./cmd/curlycatclaw-gws-mcp

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata curl git python3 pipx gnupg \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
       | gpg --dearmor -o /usr/share/keyrings/nodesource.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
       > /etc/apt/sources.list.d/nodesource.list \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends nodejs gh \
    && npm install -g @anthropic-ai/claude-code bun \
    && ln -sf /usr/bin/claude /usr/local/bin/claude \
    && PIPX_HOME=/usr/local/pipx PIPX_BIN_DIR=/usr/local/bin pipx install uv \
    && rm -rf /var/lib/apt/lists/* \
    && GWS_ARCH=$(dpkg --print-architecture | sed 's/amd64/x86_64/;s/arm64/aarch64/') \
    && curl -fsSL "https://github.com/googleworkspace/cli/releases/latest/download/google-workspace-cli-${GWS_ARCH}-unknown-linux-musl.tar.gz" \
       | tar -xz --strip-components=0 -C /usr/local/bin ./gws \
    && GH_MCP_ARCH=$(dpkg --print-architecture | sed 's/amd64/x86_64/') \
    && curl -fsSL "https://github.com/github/github-mcp-server/releases/download/v0.32.0/github-mcp-server_Linux_${GH_MCP_ARCH}.tar.gz" \
       | tar -xz -C /usr/local/bin github-mcp-server
# Install Playwright Chromium for scrapling-mcp browser-based tools (fetch, stealthy_fetch).
# Install to /opt/playwright so the runtime user (curlycatclaw) can access it.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/playwright
RUN /usr/local/bin/uv pip install --system --break-system-packages playwright \
    && playwright install --with-deps chromium \
    && chmod -R o+rX /opt/playwright
RUN useradd -m -d /data curlycatclaw
COPY --from=builder /curlycatclaw /usr/local/bin/curlycatclaw
COPY --from=builder /curlycatclaw-gws-mcp /usr/local/bin/curlycatclaw-gws-mcp
USER curlycatclaw
VOLUME /data
ENTRYPOINT ["curlycatclaw"]
CMD ["--config", "/data/config.toml"]
