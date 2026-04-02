FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o /curlycatclaw ./cmd/curlycatclaw

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata curl git python3 pipx gnupg \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends nodejs gh \
    && npm install -g @anthropic-ai/claude-code bun \
    && PIPX_HOME=/usr/local/pipx PIPX_BIN_DIR=/usr/local/bin pipx install uv \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -m -d /data curlycatclaw
COPY --from=builder /curlycatclaw /usr/local/bin/curlycatclaw
USER curlycatclaw
VOLUME /data
ENTRYPOINT ["curlycatclaw"]
CMD ["--config", "/data/config.toml"]
