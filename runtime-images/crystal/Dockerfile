FROM alpine:3.21

# crystal + shards: Alpine 3.21 ships Crystal 1.14.0 with native arm64 + amd64 support
# (the upstream crystallang/crystal image is amd64-only and emulates on Apple Silicon)
# gcc + musl-dev + pcre2-dev + libevent-dev + gc-dev: required by Crystal's linker / stdlib
RUN apk add --no-cache \
    crystal \
    shards \
    gcc \
    musl-dev \
    pcre2-dev \
    libevent-dev \
    gc-dev \
    openssl-dev \
    zlib-dev \
    yaml-dev

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN crystal --version && echo "Crystal runtime OK"
