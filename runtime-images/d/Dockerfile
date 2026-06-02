FROM alpine:3.19

# ldc: LLVM-based D compiler with native arm64 support
# gcc + musl-dev: C toolchain needed by ldc2 as backend linker
RUN apk add --no-cache ldc gcc musl-dev

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN ldc2 --version && echo "D (LDC2) runtime OK"