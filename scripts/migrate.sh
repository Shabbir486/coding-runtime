#!/usr/bin/env bash
# =============================================================================
# CodeRuntime — Database Migration Script
# =============================================================================
# Applies schema migrations in order from scripts/migrations/
#
# Usage:
#   bash scripts/migrate.sh [options]
#
# Options:
#   --dry-run          Print SQL without executing
#   --rollback         Roll back the last applied migration
#   --target=VERSION   Migrate up/down to this version number (e.g. 003)
#
# Notes:
#   - GORM AutoMigrate handles the core schema (languages, submissions) at API
#     startup, so this script manages any supplementary migrations that live
#     under scripts/migrations/ (e.g. adding indexes, partitioning, etc.)
#   - Migration files must follow the naming convention:
#       <version>_<description>_up.sql     (forward)
#       <version>_<description>_down.sql   (rollback)
#     Example: 001_add_submission_index_up.sql
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATIONS_DIR="${SCRIPT_DIR}/migrations"

DB_HOST="${CODERUNTIME_DATABASE_HOST:-localhost}"
DB_PORT="${CODERUNTIME_DATABASE_PORT:-5432}"
DB_USER="${CODERUNTIME_DATABASE_USER:-coderuntime}"
DB_PASS="${CODERUNTIME_DATABASE_PASSWORD:-coderuntime123}"
DB_NAME="${CODERUNTIME_DATABASE_NAME:-coderuntime}"

DRY_RUN=false
ROLLBACK=false
TARGET_VERSION=""

for arg in "$@"; do
    case "$arg" in
        --dry-run)    DRY_RUN=true ;;
        --rollback)   ROLLBACK=true ;;
        --target=*)   TARGET_VERSION="${arg#--target=}" ;;
        --help|-h)
            sed -n '2,20p' "$0" | sed 's/^# \?//'
            exit 0 ;;
    esac
done

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log()   { echo -e "${GREEN}[migrate]${NC} $*"; }
warn()  { echo -e "${YELLOW}[migrate]${NC} $*"; }
error() { echo -e "${RED}[migrate]${NC} $*" >&2; exit 1; }

export PGPASSWORD="${DB_PASS}"
PSQL_ARGS="-h ${DB_HOST} -p ${DB_PORT} -U ${DB_USER} -d ${DB_NAME} --no-password"

# ── Wait for database ─────────────────────────────────────────────────────────
log "Connecting to ${DB_HOST}:${DB_PORT}/${DB_NAME} as ${DB_USER}..."
for i in $(seq 1 30); do
    if pg_isready -h "${DB_HOST}" -p "${DB_PORT}" -U "${DB_USER}" -d "${DB_NAME}" &>/dev/null; then
        log "Database is ready."
        break
    fi
    if [ "${i}" -eq 30 ]; then
        error "Database not reachable after 90 s."
    fi
    sleep 3
done

# ── Ensure migrations tracking table exists ───────────────────────────────────
psql ${PSQL_ARGS} -c "
CREATE TABLE IF NOT EXISTS schema_migrations (
    version         VARCHAR(16)  PRIMARY KEY,
    description     TEXT         NOT NULL DEFAULT '',
    applied_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    checksum        VARCHAR(64),
    execution_ms    INTEGER
);" 2>/dev/null || true

# ── Discover migration files ──────────────────────────────────────────────────
mkdir -p "${MIGRATIONS_DIR}"

UP_FILES=()
while IFS= read -r -d '' f; do
    UP_FILES+=("$f")
done < <(find "${MIGRATIONS_DIR}" -name '*_up.sql' -print0 2>/dev/null | sort -z)

if [ ${#UP_FILES[@]} -eq 0 ]; then
    log "No migration files found in ${MIGRATIONS_DIR}. Nothing to do."
    exit 0
fi

# ── Rollback mode ─────────────────────────────────────────────────────────────
if [ "${ROLLBACK}" = "true" ]; then
    LAST=$(psql ${PSQL_ARGS} -t -c \
        "SELECT version FROM schema_migrations ORDER BY applied_at DESC LIMIT 1;" \
        | tr -d ' ')
    if [ -z "${LAST}" ]; then
        warn "No applied migrations found; nothing to roll back."
        exit 0
    fi
    DOWN="${MIGRATIONS_DIR}/${LAST}_down.sql"
    [ -f "${DOWN}" ] || error "Rollback script not found: ${DOWN}"
    log "Rolling back migration ${LAST}..."
    psql ${PSQL_ARGS} -f "${DOWN}"
    psql ${PSQL_ARGS} -c "DELETE FROM schema_migrations WHERE version = '${LAST}';"
    log "Rolled back ${LAST}."
    exit 0
fi

# ── Apply pending migrations ──────────────────────────────────────────────────
APPLIED=0
SKIPPED=0

for migration_file in "${UP_FILES[@]}"; do
    filename="$(basename "${migration_file}")"
    version="${filename%%_*}"
    description="${filename#*_}"; description="${description%_up.sql}"; description="${description//_/ }"

    if [ -n "${TARGET_VERSION}" ] && [ "${version}" -gt "${TARGET_VERSION}" ]; then
        break
    fi

    already=$(psql ${PSQL_ARGS} -t -c \
        "SELECT COUNT(*) FROM schema_migrations WHERE version = '${version}';" \
        | tr -d ' ')
    if [ "${already}" = "1" ]; then
        SKIPPED=$((SKIPPED + 1))
        continue
    fi

    log "Applying ${version}: ${description}"
    START_MS=$(($(date +%s%N) / 1000000))

    if [ "${DRY_RUN}" = "true" ]; then
        log "  [DRY RUN] Would apply: ${migration_file}"
        cat "${migration_file}"
        continue
    fi

    psql ${PSQL_ARGS} -f "${migration_file}" || \
        error "Migration ${version} failed. Database may be in an inconsistent state."

    END_MS=$(($(date +%s%N) / 1000000))
    DURATION=$((END_MS - START_MS))
    CHECKSUM=$(sha256sum "${migration_file}" | awk '{print $1}')

    psql ${PSQL_ARGS} -c "
        INSERT INTO schema_migrations (version, description, checksum, execution_ms)
        VALUES ('${version}', '${description}', '${CHECKSUM}', ${DURATION})
        ON CONFLICT (version) DO NOTHING;"

    log "  Applied in ${DURATION} ms"
    APPLIED=$((APPLIED + 1))
done

# ── Status ────────────────────────────────────────────────────────────────────
CURRENT=$(psql ${PSQL_ARGS} -t -c \
    "SELECT COALESCE(MAX(version), 'none') FROM schema_migrations;" | tr -d ' ')

log "Done: ${APPLIED} applied, ${SKIPPED} already applied."
log "Current schema version: ${CURRENT}"