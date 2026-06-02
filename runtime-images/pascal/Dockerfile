FROM debian:bookworm-slim

# Debian Bookworm ships fp-compiler (Free Pascal) with native arm64 + amd64
# builds. Alpine's `fpc` package lives in testing/edge only and isn't
# available in stable repos, so Debian is the more reliable base.
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    fp-compiler \
    fp-units-base \
    fp-units-rtl \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -r -u 10001 -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox \
    && mkdir -p /sandbox \
    && chown sandbox:nogroup /sandbox

WORKDIR /sandbox

USER sandbox

RUN fpc -iV && echo "Pascal (FPC) runtime OK"
