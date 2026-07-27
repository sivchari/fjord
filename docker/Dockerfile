# syntax=docker/dockerfile:1.7

# ========== Build Stage ==========
FROM golang:1.25-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /fjord-agent ./cmd/fjord-agent

# ========== Runtime Stage ==========
FROM alpine:3.20

# iproute2 provides the `ip` command, which the IMDS server uses to bind
# the 169.254.169.254 link-local address inside the node's network
# namespace (`ip addr add`). That operation requires the NET_ADMIN
# capability at runtime, which is incompatible with dropping to a non-root
# user, so this image intentionally stays root unlike fjord's other images.
RUN apk add --no-cache ca-certificates iproute2

COPY --from=builder /fjord-agent /usr/local/bin/fjord-agent

USER root

ENTRYPOINT ["fjord-agent"]
