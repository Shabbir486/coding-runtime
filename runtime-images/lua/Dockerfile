FROM alpine:3.20

# Alpine ships lua5.4 + luarocks with arm64 + amd64 builds.
RUN apk add --no-cache lua5.4 luarocks5.4 \
    && ln -sf /usr/bin/lua5.4 /usr/local/bin/lua

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN lua -v && echo "Lua runtime OK"
