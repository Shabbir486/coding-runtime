FROM python:2.7-slim

# Upstream python:2.7-slim is multi-arch (arm64 + amd64). Python 2 is EOL —
# this image exists only for legacy / educational submissions and gets no
# preloaded scientific packages (use code-runtime-python for Python 3).
RUN useradd -r -u 10001 -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox \
    && mkdir -p /sandbox \
    && chown sandbox:nogroup /sandbox

WORKDIR /sandbox

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

USER sandbox

RUN python --version && echo "Python 2.7 runtime OK"
