#!/usr/bin/env bash
# =============================================================================
# CodeRuntime — Full Development Environment Setup
# =============================================================================
# Checks prerequisites, builds all images, starts Docker Compose services,
# runs migrations, and verifies the API is healthy.
#
# Usage:
#   bash scripts/setup.sh [options]
#
# Options:
#   --skip-images      Skip building/pulling runtime images
#   --skip-migrations  Skip running scripts/migrate.sh after startup
#   --pull-only        Deploy mode: pull pre-built images from REGISTRY instead
#                      of compiling anything. Skips the Go toolchain check and
#                      the api/worker docker build, and layers in the
#                      single-node override (docker-compose.single.yml).
#                      Requires REGISTRY to be set.
#
# Environment:
#   REGISTRY           Registry prefix for --pull-only (e.g. ECR URL)
#   LANGUAGES          Optional space-separated language allowlist (passed
#                      through to pull-images.sh)
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LOG_FILE="${PROJECT_ROOT}/setup.log"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
log()     { echo -e "${GREEN}[setup]${NC} $*" | tee -a "${LOG_FILE}"; }
warn()    { echo -e "${YELLOW}[warn ]${NC} $*" | tee -a "${LOG_FILE}"; }
error()   { echo -e "${RED}[error]${NC} $*" | tee -a "${LOG_FILE}" >&2; exit 1; }
heading() { echo -e "\n${BLUE}=== $* ===${NC}\n" | tee -a "${LOG_FILE}"; }

SKIP_IMAGES=false
SKIP_MIGRATIONS=false
PULL_ONLY=false
for arg in "$@"; do
    case "$arg" in
        --skip-images)     SKIP_IMAGES=true ;;
        --skip-migrations) SKIP_MIGRATIONS=true ;;
        --pull-only)       PULL_ONLY=true ;;
        --help|-h)
            sed -n '2,24p' "$0" | sed 's/^# \?//'
            exit 0 ;;
    esac
done

# Compose files: base dev stack, plus the single-node override in deploy mode.
COMPOSE_FILES=(-f docker-compose.yml)
if [ "${PULL_ONLY}" = "true" ]; then
    COMPOSE_FILES+=(-f docker-compose.single.yml)
fi

: > "${LOG_FILE}"
heading "CodeRuntime Setup"
log "Project root : ${PROJECT_ROOT}"
log "Log file     : ${LOG_FILE}"
log "Date         : $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

# ── 1. Check prerequisites ────────────────────────────────────────────────────
heading "Checking Prerequisites"

need_cmd() {
    command -v "$1" &>/dev/null || error "'$1' is not installed. Please install it and re-run."
    log "  $1: $("$1" --version 2>&1 | head -1)"
}

need_cmd docker
if [ "${PULL_ONLY}" = "true" ]; then
    [ -n "${REGISTRY:-}" ] || error "--pull-only requires REGISTRY to be set (e.g. <acct>.dkr.ecr.<region>.amazonaws.com)"
    log "  pull-only mode: skipping Go toolchain check (no local builds)"
else
    need_cmd go
fi

# docker compose v2 or legacy docker-compose
if docker compose version &>/dev/null 2>&1; then
    DC="docker compose"
elif command -v docker-compose &>/dev/null; then
    DC="docker-compose"
else
    error "Neither 'docker compose' nor 'docker-compose' found."
fi
log "  compose: $(${DC} version 2>&1 | head -1)"

docker info &>/dev/null || error "Docker daemon is not running. Start Docker Desktop and retry."

# Go 1.24+ (only needed when building locally)
if [ "${PULL_ONLY}" = "false" ]; then
    GO_VER=$(go version | awk '{print $3}' | sed 's/go//')
    awk -v v="${GO_VER}" -v r="1.24" 'BEGIN{
        split(v,a,"."); split(r,b,".")
        for(i=1;i<=2;i++) if(a[i]+0 < b[i]+0) exit 1
    }' || error "Go 1.24+ required (found ${GO_VER})"
fi

log "All prerequisites satisfied."

# ── 2. Build/pull runtime images ──────────────────────────────────────────────
if [ "${SKIP_IMAGES}" = "false" ]; then
    if [ "${PULL_ONLY}" = "true" ]; then
        heading "Pulling Runtime Images"
        bash "${SCRIPT_DIR}/pull-images.sh" --pull-only 2>&1 | tee -a "${LOG_FILE}"
    else
        heading "Building Runtime Images"
        bash "${SCRIPT_DIR}/pull-images.sh" 2>&1 | tee -a "${LOG_FILE}"
    fi
else
    warn "Skipping runtime image build (--skip-images)"
fi

# ── 3. Create sandbox temp directory ─────────────────────────────────────────
heading "Creating Sandbox Directory"
mkdir -p /tmp/sandbox
chmod 1777 /tmp/sandbox
log "Sandbox directory: /tmp/sandbox"

# ── 4. Start Docker Compose services ─────────────────────────────────────────
heading "Starting Services"
cd "${PROJECT_ROOT}"

log "Pulling infrastructure images..."
${DC} "${COMPOSE_FILES[@]}" pull postgres redis nats 2>&1 | tee -a "${LOG_FILE}"

if [ "${PULL_ONLY}" = "true" ]; then
    log "Pulling API, Worker and Queue Manager images from ${REGISTRY}..."
    ${DC} "${COMPOSE_FILES[@]}" pull api worker queue-manager 2>&1 | tee -a "${LOG_FILE}"
else
    log "Building API and Worker images..."
    ${DC} "${COMPOSE_FILES[@]}" build --parallel api worker 2>&1 | tee -a "${LOG_FILE}"
fi

log "Starting all services..."
${DC} "${COMPOSE_FILES[@]}" up -d 2>&1 | tee -a "${LOG_FILE}"

# ── 5. Wait for services to be healthy ────────────────────────────────────────
heading "Waiting for Services"

wait_healthy() {
    local svc="$1" max="${2:-120}" elapsed=0
    log "Waiting for ${svc}..."
    while [ "${elapsed}" -lt "${max}" ]; do
        status=$(${DC} "${COMPOSE_FILES[@]}" ps "${svc}" --format json 2>/dev/null \
            | python3 -c "import sys,json; d=json.loads(sys.stdin.read()); print(d.get('Health',''))" \
            2>/dev/null || echo "")
        if [ "${status}" = "healthy" ]; then
            log "  ${svc}: healthy"
            return 0
        fi
        sleep 3; elapsed=$((elapsed + 3))
    done
    warn "${svc} did not become healthy within ${max}s — continuing anyway"
}

wait_healthy postgres 120
wait_healthy redis 60
wait_healthy api 90

# ── 6. Run supplementary migrations ──────────────────────────────────────────
if [ "${SKIP_MIGRATIONS}" = "false" ]; then
    heading "Running Supplementary Migrations"
    # GORM AutoMigrate runs inside the API container on startup.
    # scripts/migrate.sh applies any extra SQL files from scripts/migrations/.
    bash "${SCRIPT_DIR}/migrate.sh" 2>&1 | tee -a "${LOG_FILE}"
else
    warn "Skipping migrations (--skip-migrations)"
fi

# ── 7. Print seed summary ─────────────────────────────────────────────────────
heading "Seed Summary"
# The API seeds language data automatically — just verify it happened.
bash "${SCRIPT_DIR}/seed.sh" 2>&1 | tee -a "${LOG_FILE}" || warn "seed.sh reported an issue"

# ── 8. Verify API health ──────────────────────────────────────────────────────
heading "Verifying API"
API_URL="http://localhost:8002"
for i in $(seq 1 20); do
    if curl -sf "${API_URL}/health" &>/dev/null; then
        log "API is reachable at ${API_URL}/health"
        break
    fi
    if [ "${i}" -eq 20 ]; then
        warn "API health check timed out — check 'docker compose logs api'"
    fi
    sleep 3
done

# ── 9. Done ───────────────────────────────────────────────────────────────────
heading "Setup Complete"
echo -e "${GREEN}"
cat <<EOF
  ┌─────────────────────────────────────────────────────────────┐
  │  Services are running                                        │
  ├─────────────────────────────────────────────────────────────┤
  │  API               http://localhost:8002                     │
  │  Health check      http://localhost:8002/health              │
  │  Metrics           http://localhost:8002/metrics             │
  │                                                              │
  │  Grafana           http://localhost:3000   (admin / admin)   │
  │  Prometheus        http://localhost:9090                     │
  │  Jaeger (traces)   http://localhost:16686                    │
  │  NATS monitor      http://localhost:8222                     │
  │                                                              │
  │  PostgreSQL        localhost:5432  (coderuntime/coderuntime123)│
  │  Redis             localhost:6379                            │
  ├─────────────────────────────────────────────────────────────┤
  │  Stop:    docker compose down                                │
  │  Reset:   docker compose down -v                             │
  │  Logs:    docker compose logs -f [service]                   │
  │  Tests:   python3 scripts/test_languages.py                  │
  │  Log:     ${LOG_FILE}
  └─────────────────────────────────────────────────────────────┘
EOF
echo -e "${NC}"