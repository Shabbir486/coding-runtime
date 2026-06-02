FROM alpine:3.20

# SQLite isn't a server — db_runtime.go runs `sqlite3 < query.sql` against a
# fresh /tmp/db.sqlite per submission. Install just the CLI.
RUN apk add --no-cache sqlite

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN sqlite3 -version && echo "SQLite runtime OK"
