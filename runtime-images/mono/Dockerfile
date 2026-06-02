FROM mono:6.12

# Known trade-off: mono:6.12 publishes only linux/amd64, so this image will
# emulate (and show a Docker warning) on Apple Silicon. We accept that
# because:
#   * mcs (C#) ships in apt's mono-complete on Debian Bookworm and works
#     natively on arm64, BUT
#   * vbnc (Visual Basic .NET, used by LanguageVBNet) was removed from Debian
#     for arm64 entirely (last available in Bullseye amd64-only).
# Building FROM the official mono:6.12 image is the only way to keep both
# C# AND VB.NET active without forking a custom toolchain. If you don't need
# VB.NET, flip LanguageVBNet IsActive=false and swap this image to a Debian
# bookworm + mono-complete build for native arm64 support.
RUN useradd -r -u 10001 -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox \
    && mkdir -p /sandbox \
    && chown sandbox:nogroup /sandbox

WORKDIR /sandbox

USER sandbox

RUN mono --version && mcs --version 2>&1 | head -1 && echo "Mono runtime OK"
