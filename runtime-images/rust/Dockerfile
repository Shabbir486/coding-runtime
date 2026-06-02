FROM rust:1.82-alpine

# Install required build dependencies for musl target
RUN apk add --no-cache \
    musl-dev \
    gcc \
    libc-dev \
    pkgconfig \
    openssl-dev \
    ca-certificates

# Add the musl target for fully static binaries
RUN rustup target add x86_64-unknown-linux-musl

# Pre-warm the Cargo registry cache (speeds up compilation inside sandbox)
RUN cargo install --quiet --locked cargo-script 2>/dev/null || true

# Create non-root sandbox user
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox

ENV RUST_BACKTRACE=1 \
    CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER=x86_64-linux-musl-gcc \
    RUSTFLAGS="-C target-feature=+crt-static" \
    CARGO_HOME=/sandbox/.cargo \
    RUSTUP_HOME=/usr/local/rustup

RUN mkdir -p /sandbox/.cargo && \
    chown -R sandbox:nobody /sandbox

USER sandbox

# Verify toolchain
RUN rustc --version && cargo --version && echo "Rust 1.82 runtime OK"
