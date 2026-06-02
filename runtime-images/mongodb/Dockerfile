FROM mongo:7.0

# Thin re-tag of mongo:7.0 (multi-arch). db_runtime.go runs mongosh against
# an embedded mongod; the upstream entrypoint and `mongodb` OS user are
# inherited unchanged.

LABEL org.opencontainers.image.title="code-runtime-mongodb"
LABEL org.opencontainers.image.description="Ephemeral MongoDB 7.0 sandbox for DB submissions"
