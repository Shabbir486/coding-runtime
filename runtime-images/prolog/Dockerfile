FROM debian:bookworm-slim

# Debian Bookworm has swi-prolog in main for arm64 + amd64. Alpine's
# swi-prolog package lives in testing/edge only.
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    swi-prolog \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -r -u 10001 -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox \
    && mkdir -p /sandbox \
    && chown sandbox:nogroup /sandbox

WORKDIR /sandbox

USER sandbox

RUN swipl --version && echo "SWI-Prolog runtime OK"
