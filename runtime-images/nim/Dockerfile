FROM alpine:3.20

# Alpine community ships nim with arm64 + amd64 builds (currently 2.0.x), so
# we avoid the nimlang/nim:2.0.2 image which is amd64-only.
# gcc + musl-dev: nim shells out to a C compiler when producing native binaries.
RUN apk add --no-cache nim gcc musl-dev

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN nim --version && echo "Nim runtime OK"
