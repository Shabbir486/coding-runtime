FROM ubuntu:22.04

# Ubuntu has gnucobol in the universe repo with native arm64 + amd64 builds,
# unlike the older dagui0/gnucobol image which is amd64-only.
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    gnucobol \
    libcob4 \
    && rm -rf /var/lib/apt/lists/*

RUN adduser --system --uid 10001 --ingroup nogroup --no-create-home \
    --shell /bin/false --gecos "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nogroup /sandbox

USER sandbox

RUN cobc --version && echo "COBOL (GnuCOBOL) runtime OK"
