FROM golang:1.24-alpine

# Install minimal build dependencies
RUN apk add --no-cache \
    git \
    ca-certificates \
    tzdata

# Pre-populate module proxy cache for common packages
# GOARCH/GOOS are intentionally unset so `go build` targets the container's
# native architecture (linux/arm64 on Apple Silicon, linux/amd64 elsewhere).
ENV GOPATH=/go \
    GOCACHE=/go/cache \
    GOFLAGS="-mod=mod" \
    GOTOOLCHAIN=local \
    CGO_ENABLED=0

# Configure module proxy and disable checksum DB lookups (sandbox has no network)
RUN go env -w GOPROXY="https://proxy.golang.org,direct" && \
    go env -w GOSUMDB=off

# Create non-root sandbox user
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox

RUN mkdir -p /go/cache /go/pkg && \
    chown -R sandbox:nobody /sandbox /go/cache

USER sandbox

# Verify toolchain
RUN go version && echo "Go 1.24 runtime OK"
