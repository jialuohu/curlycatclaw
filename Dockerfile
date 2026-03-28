FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o /curlycatclaw ./cmd/curlycatclaw

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -h /data curlycatclaw
COPY --from=builder /curlycatclaw /usr/local/bin/curlycatclaw
USER curlycatclaw
VOLUME /data
ENTRYPOINT ["curlycatclaw"]
CMD ["--config", "/data/config.toml"]
