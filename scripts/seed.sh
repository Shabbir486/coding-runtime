#!/usr/bin/env bash
# NOTE: Legacy psql-based tooling. The canonical schema migration and
# reference-data seeding now run automatically via GORM AutoMigrate +
# SeedStatuses/SeedLanguages at service startup (database.MigrateAndSeed).
# This script targets PostgreSQL and is NOT MySQL-compatible; kept for
# historical reference only.
# =============================================================================
# CodeRuntime — Database Seed Verification Script
# =============================================================================
# Language data is seeded automatically by the API on every startup via
# SeedLanguages() (internal/models/language.go).  This script waits for the
# database to be ready, then prints a summary of what has been seeded so you
# can confirm the API has started and populated the languages table correctly.
#
# Usage:
#   bash scripts/seed.sh
# =============================================================================

set -euo pipefail

DB_HOST="${DATABASE_HOST:-localhost}"
DB_PORT="${DATABASE_PORT:-5432}"
DB_USER="${DATABASE_USER:-coderuntime}"
DB_PASS="${DATABASE_PASSWORD:-coderuntime123}"
DB_NAME="${DATABASE_NAME:-coderuntime}"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; CYAN='\033[0;36m'; NC='\033[0m'
log()   { echo -e "${GREEN}[seed]${NC} $*"; }
warn()  { echo -e "${YELLOW}[seed]${NC} $*"; }
error() { echo -e "${RED}[seed]${NC} $*" >&2; exit 1; }
info()  { echo -e "${CYAN}[seed]${NC} $*"; }

export PGPASSWORD="${DB_PASS}"
PSQL="psql -h ${DB_HOST} -p ${DB_PORT} -U ${DB_USER} -d ${DB_NAME} --no-password"

# ── Wait for the database ──────────────────────────────────────────────────────
log "Connecting to ${DB_HOST}:${DB_PORT}/${DB_NAME} ..."
for i in $(seq 1 20); do
    if pg_isready -h "${DB_HOST}" -p "${DB_PORT}" -U "${DB_USER}" -d "${DB_NAME}" &>/dev/null; then
        log "Database is ready."
        break
    fi
    if [ "${i}" -eq 20 ]; then
        error "Database not reachable after 60 s."
    fi
    sleep 3
done

# ── Check languages table exists ──────────────────────────────────────────────
TABLE_EXISTS=$(${PSQL} -t -c \
    "SELECT COUNT(*) FROM information_schema.tables
     WHERE table_schema = 'public' AND table_name = 'languages';" \
    | tr -d ' ')

if [ "${TABLE_EXISTS}" = "0" ]; then
    warn "languages table does not exist yet."
    warn "The API seeds the database automatically on first startup."
    warn "Start the API with: docker compose up -d api"
    exit 0
fi

# ── Language summary ───────────────────────────────────────────────────────────
TOTAL=$(${PSQL} -t -c "SELECT COUNT(*) FROM languages;" | tr -d ' ')
ACTIVE=$(${PSQL} -t -c "SELECT COUNT(*) FROM languages WHERE is_active = TRUE;" | tr -d ' ')

log "Languages in database: total=${TOTAL}  active=${ACTIVE}"

echo ""
info "ID  Active  Name"
info "──  ──────  ────────────────────────────────────────"
${PSQL} -t -c \
    "SELECT id, is_active, name
     FROM languages
     ORDER BY id;" \
    | awk -F'|' '{
        id=substr($1,1); gsub(/ /,"",id)
        active=substr($2,1); gsub(/ /,"",active)
        name=substr($3,1); gsub(/^ /,"",name)
        flag = (active == "t") ? "✓" : "✗"
        printf "%-3s  %-6s  %s\n", id, flag, name
    }'
echo ""

# ── Database languages ─────────────────────────────────────────────────────────
DB_LANGS=$(${PSQL} -t -c \
    "SELECT COUNT(*) FROM languages WHERE is_database = TRUE AND is_active = TRUE;" \
    | tr -d ' ')
log "Active database languages: ${DB_LANGS} (SQLite, MySQL, PostgreSQL)"

# ── Submission stats ───────────────────────────────────────────────────────────
SUB_TABLE=$(${PSQL} -t -c \
    "SELECT COUNT(*) FROM information_schema.tables
     WHERE table_schema = 'public' AND table_name = 'submissions';" \
    | tr -d ' ')

if [ "${SUB_TABLE}" = "1" ]; then
    SUBS=$(${PSQL} -t -c "SELECT COUNT(*) FROM submissions;" | tr -d ' ')
    log "Submissions in database: ${SUBS}"
fi

log "Seed check complete."