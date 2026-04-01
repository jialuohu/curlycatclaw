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
    ca-certificates tzdata curl git nodejs npm \
    && npm install -g @anthropic-ai/claude-code \
    && apt-get purge -y npm && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -m -d /data curlycatclaw
COPY --from=builder /curlycatclaw /usr/local/bin/curlycatclaw
USER curlycatclaw
VOLUME /data
ENTRYPOINT ["curlycatclaw"]
CMD ["--config", "/data/config.toml"]
