FROM alpine:3.20

# bash + coreutils give scripts a familiar GNU environment (Alpine ships busybox
# by default which is missing some flags).
RUN apk add --no-cache bash coreutils

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN bash --version && echo "Bash runtime OK"
