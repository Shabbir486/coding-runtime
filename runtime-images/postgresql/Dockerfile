FROM postgres:16-alpine

# Thin re-tag of postgres:16-alpine (multi-arch). The image's entrypoint,
# initdb scripts, and `postgres` OS user are inherited; db_runtime.go invokes
# initdb + pg_ctl directly under that user.

LABEL org.opencontainers.image.title="code-runtime-postgresql"
LABEL org.opencontainers.image.description="Ephemeral PostgreSQL 16 sandbox for DB submissions"
