FROM mysql:8.0

# Thin re-tag of mysql:8.0 (which is already multi-arch). The server's
# entrypoint, init scripts, and `mysql` OS user are inherited unchanged.
# db_runtime.go invokes mysqld directly with --user=root and skip-grant-tables,
# so we deliberately do NOT add a sandbox user here.

LABEL org.opencontainers.image.title="code-runtime-mysql"
LABEL org.opencontainers.image.description="Ephemeral MySQL 8.0 sandbox for DB submissions"
