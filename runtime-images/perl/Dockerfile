FROM perl:5.38-slim

# The upstream perl:5.38-slim image is multi-arch (Debian-based, runs natively
# on arm64 and amd64). This local wrapper exists only to drop the sandbox user
# and standardise the workdir so the worker can treat every language image the
# same way.

RUN useradd -r -u 10001 -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox \
    && mkdir -p /sandbox \
    && chown sandbox:nogroup /sandbox

WORKDIR /sandbox

USER sandbox

RUN perl --version && echo "Perl runtime OK"
