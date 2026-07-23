#!/usr/bin/env bash
# =============================================================================
# Code Runtime — local Docker setup WITH worker scaling.
#
# Runs the whole platform on your machine via docker compose and scales the
# WORKER service the way KEDA does on EKS: a small autoscaler loop watches the
# job backlog and adds/removes worker containers.
#
# Queue provider is taken from QUEUE_PROVIDER (env or .env), default "nats":
#   - nats (default): fully self-contained — local MySQL + Redis + a NATS
#     container as the queue. No AWS needed. Backlog signal = DB pending count.
#   - sqs (e.g. QUEUE_PROVIDER=sqs in .env): uses your real SQS queues + AWS
#     creds from .env; NO nats container starts. Backlog signal = SQS depth
#     (needs the aws CLI). DB can be local or a remote DATABASE_HOST (RDS).
#
# Local scaling model (mirrors prod):
#   - Horizontal: number of `worker` CONTAINERS (this is what we autoscale,
#     like KEDA scales worker pods on SQS backlog).
#   - Vertical: WORKER_COUNT x WORKER_CONCURRENCY sandboxes PER container.
#   Workers use Docker-out-of-Docker (mount the host /var/run/docker.sock) to
#   launch sandbox containers on your host Docker.
#
# Usage:
#   scripts/setup-local-docker-scaling.sh up            # build + start stack
#   scripts/setup-local-docker-scaling.sh build-langs   # build runtime images locally (needed to EXECUTE code)
#   scripts/setup-local-docker-scaling.sh autoscale     # KEDA-like loop (foreground; Ctrl-C to stop)
#   scripts/setup-local-docker-scaling.sh load [N]      # fire N jobs to generate backlog
#   scripts/setup-local-docker-scaling.sh scale <N>     # manually set worker count
#   scripts/setup-local-docker-scaling.sh status        # containers + worker count + backlog
#   scripts/setup-local-docker-scaling.sh logs [svc]    # tail logs
#   scripts/setup-local-docker-scaling.sh down [--volumes]
#
# Tunables (env overrides):
#   WORKER_MIN=1  WORKER_MAX=8   # autoscaler bounds (worker containers)
#   QUEUE_LENGTH=5               # backlog jobs per worker (like KEDA queueLength)
#   INTERVAL=8                   # autoscaler poll seconds
#   WORKER_COUNT=2 WORKER_CONCURRENCY=2   # in-container concurrency
#   API_PORT=8002
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

# ---- config -----------------------------------------------------------------
WORKER_MIN="${WORKER_MIN:-1}"
WORKER_MAX="${WORKER_MAX:-8}"
QUEUE_LENGTH="${QUEUE_LENGTH:-5}"
INTERVAL="${INTERVAL:-8}"
export WORKER_COUNT="${WORKER_COUNT:-2}"
export WORKER_CONCURRENCY="${WORKER_CONCURRENCY:-2}"
API_PORT="${API_PORT:-8002}"
API_URL="http://localhost:${API_PORT}"

# Read one key from .env (compose auto-loads .env for interpolation; the script
# reads the few values its own logic needs). An exported env var wins over .env.
envget() { [ -f .env ] && sed -n -E "s/^$1=[\"']?([^\"'#]*).*/\1/p" .env | tail -1 | sed 's/[[:space:]]*$//'; }

# RESPECT the queue provider from env / .env — do NOT force nats.
QUEUE_PROVIDER="${QUEUE_PROVIDER:-$(envget QUEUE_PROVIDER)}"
QUEUE_PROVIDER="${QUEUE_PROVIDER:-nats}"
export QUEUE_PROVIDER
DB_HOST_EFF="${DATABASE_HOST:-$(envget DATABASE_HOST)}"
SQS_URL="${SQS_JOBS_QUEUE_URL:-$(envget SQS_JOBS_QUEUE_URL)}"
SQS_RGN="${SQS_REGION:-$(envget SQS_REGION)}"

# Runtime images: by DEFAULT built locally from runtime-images/ via
# scripts/pull-images.sh. Set REGISTRY to instead pull pre-built images
# (pull-images.sh --pull-only). Same mechanism/allowlist as pull-images.sh.
REGISTRY="${REGISTRY:-}"

# Local dev user (seeded so /auth/token works). Hash is bcrypt("Admin123!").
DEV_EMAIL="dev@coderuntime.io"
DEV_PASSWORD="Admin123!"
DEV_HASH='$2b$10$rhqFHNJI67ZX09lUJ7s9e.yckrM59wZW9rKr5/Vex1JrDHfQc8KAO'

# Active languages (must match DefaultLanguages active set) for build-langs.
ACTIVE_LANGS="python nodejs cpp java mysql postgresql web"

# Compose profiles: local mysql+redis (localinfra) for a self-contained run; the
# nats queue container (natsinfra) ONLY for the nats provider. Under sqs, no nats.
COMPOSE=(docker compose --profile localinfra)
[ "$QUEUE_PROVIDER" = "nats" ] && COMPOSE+=(--profile natsinfra)

GRN='\033[0;32m'; YLW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log()  { echo -e "${GRN}[local]${NC} $*"; }
warn() { echo -e "${YLW}[warn ]${NC} $*"; }
die()  { echo -e "${RED}[error]${NC} $*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "missing prerequisite: $1"; }

worker_count() { "${COMPOSE[@]}" ps -q worker 2>/dev/null | grep -c . || true; }

backlog() {
  if [ "$QUEUE_PROVIDER" = "sqs" ]; then
    # KEDA-style signal: messages on the jobs queue (visible + in-flight).
    [ -n "$SQS_URL" ] || { echo 0; return; }
    AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-$(envget AWS_ACCESS_KEY_ID)}" \
    AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-$(envget AWS_SECRET_ACCESS_KEY)}" \
    aws sqs get-queue-attributes --queue-url "$SQS_URL" --region "${SQS_RGN:-us-east-1}" \
      --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
      --query 'Attributes' --output json 2>/dev/null \
      | python3 -c "import sys,json;a=json.load(sys.stdin);print(int(a.get('ApproximateNumberOfMessages',0))+int(a.get('ApproximateNumberOfMessagesNotVisible',0)))" 2>/dev/null || echo 0
  else
    # NATS/local: jobs queued or in-progress (status In Queue=1, Processing=2).
    "${COMPOSE[@]}" exec -T mysql mysql -uroot -prootpass123 coderuntime -N \
      -e "SELECT COUNT(*) FROM submissions WHERE status_id IN (1,2);" 2>/dev/null | tr -d '[:space:]' || echo 0
  fi
}

clamp() { local v=$1 lo=$2 hi=$3; ((v<lo)) && v=$lo; ((v>hi)) && v=$hi; echo "$v"; }

scale_to() {
  local n; n="$(clamp "$1" "$WORKER_MIN" "$WORKER_MAX")"
  "${COMPOSE[@]}" up -d --no-recreate --scale "worker=$n" worker >/dev/null
  echo "$n"
}

wait_health() {
  log "waiting for API health at ${API_URL}/health ..."
  for _ in $(seq 1 60); do
    if curl -fs -m 3 "${API_URL}/health" >/dev/null 2>&1; then log "API healthy"; return 0; fi
    sleep 3
  done
  die "API did not become healthy — check: ${COMPOSE[*]} logs api"
}

seed_user() {
  # Only seed into a LOCAL mysql. A remote DATABASE_HOST (e.g. RDS) is expected
  # to already have the user, and the local mysql container isn't the app's DB.
  case "${DB_HOST_EFF:-}" in
    ""|mysql|localhost|127.0.0.1) : ;;
    *) log "DB is remote (${DB_HOST_EFF}) — skipping local user seed (login must already exist there)"; return 0 ;;
  esac
  log "seeding dev user ${DEV_EMAIL} ..."
  "${COMPOSE[@]}" exec -T mysql mysql -uroot -prootpass123 coderuntime \
    -e "INSERT INTO users (email,password_hash,name,is_active,is_admin,created_at,updated_at)
        VALUES ('${DEV_EMAIL}','${DEV_HASH}','dev',1,1,NOW(),NOW())
        ON DUPLICATE KEY UPDATE password_hash=VALUES(password_hash), is_active=1, is_admin=1;" \
    2>/dev/null && log "dev user ready (login: ${DEV_EMAIL} / ${DEV_PASSWORD})" \
    || warn "could not seed user (is the users table migrated yet?)"
}

token() {
  curl -fs -m 10 -X POST "${API_URL}/auth/token" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${DEV_EMAIL}\",\"password\":\"${DEV_PASSWORD}\"}" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null
}

# ---- subcommands ------------------------------------------------------------
cmd_up() {
  need docker; need curl; need python3
  [ "$QUEUE_PROVIDER" = "sqs" ] && need aws
  docker info >/dev/null 2>&1 || die "Docker daemon not running (start Docker Desktop)"
  log "queue provider: ${QUEUE_PROVIDER}  |  DB: ${DB_HOST_EFF:-local mysql}"
  log "building + starting stack (api, queue-manager, ${WORKER_MIN}x worker$([ "$QUEUE_PROVIDER" = nats ] && echo ", nats"))"
  WORKER_REPLICAS="$WORKER_MIN" "${COMPOSE[@]}" up -d --build --scale "worker=$WORKER_MIN"
  wait_health
  seed_user
  # Make sure the active-language runtime images exist locally (build or pull)
  # so submissions actually execute. Skip with SKIP_LANGS=true.
  [ "${SKIP_LANGS:-false}" = "true" ] || ensure_langs
  cat <<EOF

$(log "local stack is up")
  API:        ${API_URL}   (Swagger: ${API_URL}/swagger/index.html)
  login:      ${DEV_EMAIL} / ${DEV_PASSWORD}
  workers:    $(worker_count)   (autoscale bounds ${WORKER_MIN}..${WORKER_MAX}, queueLength=${QUEUE_LENGTH})
  queue:      ${QUEUE_PROVIDER}   (backlog signal: $([ "$QUEUE_PROVIDER" = sqs ] && echo "SQS depth" || echo "DB pending count"))
  Grafana:    http://localhost:3000$([ "$QUEUE_PROVIDER" = nats ] && echo "     NATS mon: http://localhost:8222")

Next:
  $0 autoscale &     # start the KEDA-like autoscaler (background)
  $0 load 40         # fire 40 jobs to watch it scale
  $0 status
  $0 langs           # (re)ensure runtime images (build, or pull if REGISTRY set)
EOF
}

# ensure_langs makes each active language's runtime image available locally as
# code-runtime-<lang>:latest (what the worker references) by delegating to the
# canonical scripts/pull-images.sh — same as the main setup. By default it
# BUILDS the missing images from runtime-images/<lang>; if REGISTRY is set it
# pulls pre-built images instead (--pull-only). Only missing images are done.
ensure_langs() {
  need docker
  local missing=() present=0 l
  for l in $ACTIVE_LANGS; do
    if docker image inspect "code-runtime-$l:latest" >/dev/null 2>&1; then
      present=$((present + 1))
    else
      missing+=("$l")
    fi
  done
  if [ ${#missing[@]} -eq 0 ]; then
    log "all ${present} active runtime images already present locally"
    return 0
  fi
  local mode=()
  if [ -n "$REGISTRY" ]; then
    mode=(--pull-only); log "obtaining via pull-images.sh (--pull-only from ${REGISTRY}): ${missing[*]}"
  else
    log "obtaining via pull-images.sh (build from runtime-images/): ${missing[*]}"
  fi
  LANGUAGES="${missing[*]}" REGISTRY="$REGISTRY" bash "$SCRIPT_DIR/pull-images.sh" --no-infra "${mode[@]}"
}

cmd_scale() { [ -n "${1:-}" ] || die "usage: $0 scale <N>"; log "scaling worker -> $(scale_to "$1")"; }

cmd_autoscale() {
  need python3
  log "autoscaler running (min=${WORKER_MIN} max=${WORKER_MAX} queueLength=${QUEUE_LENGTH} every ${INTERVAL}s). Ctrl-C to stop."
  trap 'echo; log "autoscaler stopped (stack still running)"; exit 0' INT
  while true; do
    local b cur desired
    b="$(backlog)"; b="${b:-0}"
    cur="$(worker_count)"; cur="${cur:-0}"
    # desired = ceil(backlog / queueLength), clamped — same idea as KEDA.
    desired=$(( (b + QUEUE_LENGTH - 1) / QUEUE_LENGTH ))
    desired="$(clamp "$desired" "$WORKER_MIN" "$WORKER_MAX")"
    if [ "$desired" != "$cur" ]; then
      log "backlog=${b}  workers ${cur} -> ${desired}"
      scale_to "$desired" >/dev/null
    else
      printf '\r[local] backlog=%s workers=%s (steady)        ' "$b" "$cur"
    fi
    sleep "$INTERVAL"
  done
}

cmd_load() {
  need python3; need curl
  local n="${1:-40}"
  local tok; tok="$(token)"; [ -n "$tok" ] || die "could not get token (run '$0 up' first)"
  log "firing ${n} python jobs (no-wait) to build backlog ..."
  for i in $(seq 1 "$n"); do
    curl -fs -m 10 -X POST "${API_URL}/submissions" -H "Authorization: Bearer $tok" \
      -H 'Content-Type: application/json' \
      -d '{"language_id":29,"source_code":"import time; time.sleep(2); print(6*7)"}' >/dev/null 2>&1 &
    (( i % 20 == 0 )) && wait
  done
  wait
  log "submitted ${n} jobs — watch: $0 status   (or the autoscaler output)"
}

cmd_status() {
  echo "workers: $(worker_count)   backlog(queued+running): $(backlog)"
  "${COMPOSE[@]}" ps
}

cmd_logs() { "${COMPOSE[@]}" logs -f --tail=50 "${1:-}"; }

cmd_down() {
  if [ "${1:-}" = "--volumes" ]; then
    log "tearing down + removing volumes"; "${COMPOSE[@]}" down -v
  else
    log "tearing down (volumes kept; add --volumes to wipe data)"; "${COMPOSE[@]}" down
  fi
}

case "${1:-up}" in
  up)                       cmd_up ;;
  langs|ensure-langs|build-langs) ensure_langs ;;
  scale)       shift; cmd_scale "${1:-}" ;;
  autoscale)   cmd_autoscale ;;
  load)        shift; cmd_load "${1:-40}" ;;
  status)      cmd_status ;;
  logs)        shift; cmd_logs "${1:-}" ;;
  down)        shift; cmd_down "${1:-}" ;;
  *) die "unknown command '${1}'. See the header for usage." ;;
esac
