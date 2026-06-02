FROM swift:5.10-jammy

# Install additional build tools and libraries
RUN apt-get update && apt-get install -y --no-install-recommends \
    libsqlite3-dev \
    libxml2-dev \
    libcurl4-openssl-dev \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Pre-build a dummy Swift package to warm the toolchain cache
RUN mkdir -p /tmp/warmup && \
    cd /tmp/warmup && \
    swift package init --type executable --name warmup 2>/dev/null && \
    swift build -c release 2>/dev/null || true && \
    rm -rf /tmp/warmup

# Create non-root sandbox user
RUN useradd -r -u 10001 -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox

# Create sandbox directory with proper permissions
RUN mkdir -p /sandbox && chown sandbox:sandbox /sandbox

WORKDIR /sandbox

ENV SWIFT_DETERMINISTIC_HASHING=1 \
    SWIFT_BACKTRACE=enable=yes,sanitize=yes,limit=32,cache=yes

USER sandbox

# Verify toolchain
RUN swift --version && echo "Swift 5.10 runtime OK"
