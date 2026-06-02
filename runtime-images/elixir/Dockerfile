FROM elixir:1.15-alpine

# Upstream elixir:1.15-alpine is multi-arch. We wrap it only to add the
# sandbox user and standardise the workdir layout.
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN elixir --version && echo "Elixir runtime OK"
