#!/usr/bin/env bash
# =============================================================================
# Code Runtime — one-go LOCAL deploy (kind + real KEDA, SQS mode).
#
# The local twin of eks/deploy.sh. Same platform, same SQS queue provider, same
# KEDA autoscaling — on a laptop, from nothing. Differences vs EKS are ONLY the
# things a laptop can't provide (all documented in README.md):
#   - kind single-node cluster instead of a managed EKS nodegroup
#   - in-cluster MySQL + Redis instead of RDS + ElastiCache
#   - the worker runs sandboxes via Docker-out-of-Docker (host Docker socket)
#     instead of a dind sidecar + ECR — so locally-built code-runtime-* images
#     are used directly, with no registry
#   - KEDA + the app authenticate to SQS with STATIC .env keys instead of IRSA
#   - no cluster-autoscaler (one node); KEDA still scales worker PODS on backlog
#
# Idempotent: safe to re-run. Prereqs: docker, kind, kubectl, helm, python3
# (with `bcrypt`), and a repo-root .env with AWS keys + SQS URLs (QUEUE_PROVIDER=sqs).
#
# Usage:
#   ./setup.sh              # full bring-up (default) — ends by exposing :18002
#   ./setup.sh expose       # port-forward the API to localhost:18002 (foreground)
#   ./setup.sh watch        # live KEDA ScaledObject / HPA / pods
#   ./setup.sh load [N]     # fire N jobs → watch KEDA scale the worker pods
#   ./setup.sh langs        # (re)build the active-language runtime images on host
#   ./setup.sh down         # delete the kind cluster
#   ./setup.sh redeploy [svc] # rebuilds one (or all) service image(s), reloads into kind, and rolls the deployment so the new code actually runs. svc: api | worker | queue-manager | all.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${CLUSTER:-coderuntime-local}"
KCTX="kind-${CLUSTER}"
NS="${NS:-coderuntime}"
NODE_IMAGE="${NODE_IMAGE:-kindest/node:v1.31.6}"
API_PORT="${API_PORT:-18002}"
DEV_EMAIL="dev@coderuntime.io"; DEV_PASSWORD="Admin123!"
# Active languages whose runtime images the worker needs on the HOST Docker.
ACTIVE_LANGS="${ACTIVE_LANGS:-python nodejs cpp java mysql postgresql web}"
# Local (dev-only) in-cluster datastore credentials.
DB_USER="coderuntime"; DB_PASS="coderuntime123"; DB_ROOT="rootpass123"; REDIS_PASS="coderuntime123"

log(){ printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die(){ printf '\033[0;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }
need(){ command -v "$1" >/dev/null 2>&1 || die "missing prerequisite: $1"; }
k(){ kubectl --context "$KCTX" "$@"; }
envget(){ [ -f .env ] && sed -n -E "s/^$1=[\"']?([^\"'#]*).*/\1/p" .env | tail -1 | sed 's/[[:space:]]*$//'; }

# ---------------------------------------------------------------------------
ensure_langs(){
  need docker
  local missing=() present=0 l
  for l in $ACTIVE_LANGS; do
    if docker image inspect "code-runtime-$l:latest" >/dev/null 2>&1; then present=$((present+1))
    else missing+=("$l"); fi
  done
  if [ ${#missing[@]} -eq 0 ]; then log "all ${present} active runtime images present on host"; return 0; fi
  log "building missing runtime images on host via pull-images.sh: ${missing[*]}"
  LANGUAGES="${missing[*]}" bash "$REPO_ROOT/scripts/pull-images.sh" --no-infra
}

cmd_up(){
  need docker; need kind; need kubectl; need helm; need python3
  docker info >/dev/null 2>&1 || die "Docker daemon not running"
  [ -f .env ] || die "no .env at repo root (need AWS keys + SQS URLs; QUEUE_PROVIDER=sqs)"

  local AK SK RGN JOBSQ STARTQ JWT
  AK="$(envget AWS_ACCESS_KEY_ID)"; SK="$(envget AWS_SECRET_ACCESS_KEY)"
  RGN="$(envget SQS_REGION)"; RGN="${RGN:-ap-southeast-2}"
  JOBSQ="$(envget SQS_JOBS_QUEUE_URL)"; STARTQ="$(envget SQS_START_QUEUE_URL)"
  JWT="$(envget JWT_SECRET)"; [ -n "$JWT" ] || JWT="$(head -c48 /dev/urandom | base64 | tr -d '\n=/+')"
  [ -n "$AK" ] && [ -n "$SK" ] || die "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY missing in .env (SQS needs them locally)"

  # ---- 1. kind cluster -----------------------------------------------------
  log "1/9 kind cluster"
  mkdir -p /tmp/sandbox
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    echo "cluster $CLUSTER exists — reusing"
  else
    sed "s#image: kindest/node:.*#image: ${NODE_IMAGE}#" "$SCRIPT_DIR/kind-cluster.yaml" | kind create cluster --config -
  fi
  kubectl config use-context "$KCTX" >/dev/null
  k wait --for=condition=Ready node --all --timeout=120s

  # ---- 2. service images -> load into kind --------------------------------
  log "2/9 build + load service images"
  local build=false s img
  for s in api worker queue-manager; do
    img="ghcr.io/coderuntime/${s/api/api-gateway}:latest"
    if [ "${REBUILD:-false}" = "true" ] || ! docker image inspect "$img" >/dev/null 2>&1; then build=true; fi
  done
  if [ "$build" = "true" ] || [ "${REBUILD:-false}" = "true" ]; then
    docker build -f docker/Dockerfile.api           -t ghcr.io/coderuntime/api-gateway:latest .
    docker build -f docker/Dockerfile.worker        -t ghcr.io/coderuntime/worker:latest .
    docker build -f docker/Dockerfile.queue-manager -t ghcr.io/coderuntime/queue-manager:latest .
  else
    echo "service images already built (set REBUILD=true to force)"
  fi
  kind load docker-image --name "$CLUSTER" \
    ghcr.io/coderuntime/api-gateway:latest ghcr.io/coderuntime/worker:latest ghcr.io/coderuntime/queue-manager:latest

  # ---- 3. runtime images (active languages) on HOST Docker ----------------
  log "3/9 runtime images (host Docker, used via DooD)"
  [ "${SKIP_LANGS:-false}" = "true" ] && echo "skipped (SKIP_LANGS=true)" || ensure_langs

  # ---- 4. namespace + storageclass + metrics-server -----------------------
  log "4/9 namespace + storageclass + metrics-server"
  k apply -f "$SCRIPT_DIR/namespace.yaml"
  k apply -f "$SCRIPT_DIR/storageclass.yaml"
  k apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
  k -n kube-system patch deployment metrics-server --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' || true

  # ---- 5. secrets (local infra values + AWS keys from .env) ---------------
  log "5/9 secrets"
  mk_secret(){ local n="$1"; shift; k -n "$NS" create secret generic "$n" "$@" --dry-run=client -o yaml | k apply -f - >/dev/null; }
  mk_secret aws-credentials --from-literal=AWS_ACCESS_KEY_ID="$AK" --from-literal=AWS_SECRET_ACCESS_KEY="$SK"
  mk_secret api-secrets --from-literal=DATABASE_HOST=mysql --from-literal=DATABASE_USER="$DB_USER" \
    --from-literal=DATABASE_PASSWORD="$DB_PASS" --from-literal=REDIS_HOST=redis \
    --from-literal=REDIS_PASSWORD="$REDIS_PASS" --from-literal=JWT_SECRET="$JWT"
  mk_secret worker-secrets --from-literal=DATABASE_HOST=mysql --from-literal=DATABASE_USER="$DB_USER" \
    --from-literal=DATABASE_PASSWORD="$DB_PASS" --from-literal=REDIS_HOST=redis --from-literal=REDIS_PASSWORD="$REDIS_PASS"
  mk_secret mysql-secrets --from-literal=MYSQL_USER="$DB_USER" --from-literal=MYSQL_PASSWORD="$DB_PASS" \
    --from-literal=MYSQL_ROOT_PASSWORD="$DB_ROOT"
  mk_secret redis-secrets --from-literal=REDIS_PASSWORD="$REDIS_PASS"

  # ---- 6. KEDA (static creds via aws-credentials — no IRSA on kind) --------
  log "6/9 KEDA operator"
  helm repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
  helm repo update >/dev/null
  helm --kube-context "$KCTX" upgrade --install keda kedacore/keda -n keda --create-namespace >/dev/null
  k -n keda rollout status deploy/keda-operator --timeout=180s

  # ---- 7. (no cluster-autoscaler on a 1-node kind) ------------------------
  log "7/9 cluster-autoscaler — skipped (kind is single-node; KEDA still scales worker PODS)"

  # ---- 8. config + workloads ----------------------------------------------
  log "8/9 config + workloads"
  k apply -f "$SCRIPT_DIR/configmap.yaml"
  # Point SQS config + KEDA triggers at YOUR .env queues (override the defaults).
  if [ -n "$JOBSQ" ]; then
    k -n "$NS" patch cm common-config --type=merge -p \
      "{\"data\":{\"CODERUNTIME_SQS_JOBS_QUEUE_URL\":\"$JOBSQ\",\"CODERUNTIME_SQS_START_QUEUE_URL\":\"${STARTQ:-}\",\"CODERUNTIME_SQS_REGION\":\"$RGN\"}}"
  fi
  k apply -f "$SCRIPT_DIR/redis.yaml"
  k apply -f "$SCRIPT_DIR/mysql.yaml"
  log "waiting for data tier (mysql + redis) ..."
  k -n "$NS" rollout status statefulset/mysql --timeout=180s
  k -n "$NS" rollout status statefulset/redis --timeout=120s
  k apply -f "$SCRIPT_DIR/api-gateway.yaml"
  k apply -f "$SCRIPT_DIR/queue-manager.yaml"
  k apply -f "$SCRIPT_DIR/worker.yaml"
  # KEDA ScaledObject: substitute YOUR .env SQS queues into the trigger.
  local tmp; tmp="$(mktemp)"; cp "$SCRIPT_DIR/keda-scaledobject.yaml" "$tmp"
  [ -n "$JOBSQ" ]  && sed -i.bak -E "s#queueURL: .*code-submission-processing-queue#queueURL: ${JOBSQ}#" "$tmp"
  [ -n "$STARTQ" ] && sed -i.bak -E "s#queueURL: .*code-submission-completed-queue#queueURL: ${STARTQ}#" "$tmp"
  sed -i.bak -E "s#awsRegion: .*#awsRegion: ${RGN}#" "$tmp"
  k apply -f "$tmp"; rm -f "$tmp" "$tmp.bak"

  # ---- 9. wait + seed dev user + report -----------------------------------
  log "9/9 wait for rollout + seed dev admin"
  k -n "$NS" rollout status deploy/api-gateway --timeout=180s
  k -n "$NS" rollout status deploy/worker --timeout=180s
  local HASH; HASH="$(python3 -c "import bcrypt;print(bcrypt.hashpw(b'${DEV_PASSWORD}',bcrypt.gensalt(rounds=10)).decode())")"
  k -n "$NS" exec -i mysql-0 -- mysql -u "$DB_USER" -p"$DB_PASS" coderuntime 2>/dev/null <<SQL || echo "  (seed skipped — users table not ready yet; re-run: ./setup.sh seed)"
INSERT INTO users (email, password_hash, name, is_active, is_admin, created_at, updated_at)
VALUES ('${DEV_EMAIL}', '${HASH}', 'Dev Admin', 1, 1, NOW(), NOW())
ON DUPLICATE KEY UPDATE password_hash=VALUES(password_hash), is_active=1, is_admin=1, updated_at=NOW();
SQL

  cat <<EOF

$(log "DONE — local SQS + KEDA stack is up")
$(k -n "$NS" get pods -o wide)

  Login:   ${DEV_EMAIL} / ${DEV_PASSWORD}
  Swagger: http://localhost:${API_PORT}/swagger/index.html
  KEDA:    ./setup.sh watch      Load: ./setup.sh load 60
EOF

  # Auto-expose the API on ${API_PORT} (set EXPOSE=false to skip, e.g. in CI).
  [ "${EXPOSE:-true}" = "false" ] || cmd_expose
}

cmd_seed(){
  need python3
  local HASH; HASH="$(python3 -c "import bcrypt;print(bcrypt.hashpw(b'${DEV_PASSWORD}',bcrypt.gensalt(rounds=10)).decode())")"
  k -n "$NS" exec -i mysql-0 -- mysql -u "$DB_USER" -p"$DB_PASS" coderuntime <<SQL
INSERT INTO users (email, password_hash, name, is_active, is_admin, created_at, updated_at)
VALUES ('${DEV_EMAIL}', '${HASH}', 'Dev Admin', 1, 1, NOW(), NOW())
ON DUPLICATE KEY UPDATE password_hash=VALUES(password_hash), is_active=1, is_admin=1, updated_at=NOW();
SQL
  log "seeded ${DEV_EMAIL}"
}

# cmd_expose port-forwards the api-gateway to localhost:$API_PORT in the
# FOREGROUND (via exec, for clean Ctrl-C handling). Ctrl-C stops only the
# port-forward — the kind cluster keeps running.
cmd_expose(){
  need kubectl
  log "API exposed → http://localhost:${API_PORT}   (Swagger: http://localhost:${API_PORT}/swagger/index.html)"
  log "Ctrl-C stops the port-forward (the cluster keeps running; re-expose with: ./setup.sh expose)"
  exec kubectl --context "$KCTX" -n "$NS" port-forward deploy/api-gateway "${API_PORT}:8002"
}

cmd_watch(){
  need kubectl
  # macOS + k9s installed -> launch the k9s dashboard (best live view of KEDA
  # scaling: ':scaledobjects', ':hpa', ':pods', 'l' for logs). Otherwise fall
  # back to a portable refresh loop.
  if [ "$(uname)" = "Darwin" ] && command -v k9s >/dev/null 2>&1; then
    log "opening k9s dashboard (':scaledobjects' / ':hpa' / ':pods'; ':q' to quit)"
    exec k9s --context "$KCTX" -n "$NS"
  fi
  # `kubectl get -w` allows only ONE resource type, so refresh a combined view
  # on a loop instead (works everywhere; no `watch`/k9s needed).
  log "Ctrl-C to stop — SQS backlog drives KEDA (refreshing every 2s):"
  while true; do
    clear
    date
    k -n "$NS" get scaledobject,hpa,pods -o wide 2>/dev/null
    sleep 2
  done
}

cmd_load(){
  need curl; need python3
  local n="${1:-60}"
  k -n "$NS" port-forward deploy/api-gateway "${API_PORT}:8002" >/dev/null 2>&1 &
  local pf=$!; trap 'kill $pf 2>/dev/null || true' RETURN; sleep 4
  local tok
  tok="$(curl -fs -m 10 -X POST "http://localhost:${API_PORT}/auth/token" -H 'Content-Type: application/json' \
     -d "{\"email\":\"${DEV_EMAIL}\",\"password\":\"${DEV_PASSWORD}\"}" \
     | python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null)"
  [ -n "$tok" ] || die "no token (stack up? user seeded? try ./setup.sh seed)"
  log "firing ${n} jobs to build SQS backlog ..."
  local i
  for i in $(seq 1 "$n"); do
    curl -fs -m 10 -X POST "http://localhost:${API_PORT}/submissions" -H "Authorization: Bearer $tok" \
      -H 'Content-Type: application/json' \
      -d '{"language_id":29,"source_code":"import time;time.sleep(3);print(6*7)"}' >/dev/null 2>&1 &
    (( i % 20 == 0 )) && wait
  done
  wait
  log "submitted ${n} — now: ./setup.sh watch"
}

cmd_down(){ need kind; log "deleting kind cluster ${CLUSTER}"; kind delete cluster --name "$CLUSTER"; }

# cmd_redeploy rebuilds one (or all) service image(s), reloads into kind, and
# rolls the deployment so the new code actually runs. Needed because the images
# use the :latest tag with imagePullPolicy=IfNotPresent, so a plain re-apply
# won't restart a running pod. svc: api | worker | queue-manager | all.
cmd_redeploy(){
  need docker; need kind; need kubectl
  local svc="${1:-api}" list
  case "$svc" in
    api|api-gateway)     list="api" ;;
    worker)              list="worker" ;;
    queue|queue-manager) list="queue-manager" ;;
    all)                 list="api worker queue-manager" ;;
    *) die "redeploy: unknown service '$svc' (use api | worker | queue-manager | all)" ;;
  esac
  kubectl config use-context "$KCTX" >/dev/null
  local s name img
  for s in $list; do
    name="${s/api/api-gateway}"            # api -> api-gateway; others unchanged
    img="ghcr.io/coderuntime/${name}:latest"
    log "rebuilding ${img}"
    docker build -f "docker/Dockerfile.$s" -t "$img" .
    kind load docker-image --name "$CLUSTER" "$img"
    log "rolling deploy/${name}"
    k -n "$NS" rollout restart "deploy/${name}"
    k -n "$NS" rollout status "deploy/${name}" --timeout=180s
  done
  log "redeploy done: ${list}"
  # Auto-expose the API afterwards (set EXPOSE=false to skip).
  [ "${EXPOSE:-true}" = "false" ] || cmd_expose
}

case "${1:-up}" in
  up)                 cmd_up ;;
  langs|ensure-langs) ensure_langs ;;
  seed)               cmd_seed ;;
  redeploy)  shift;   cmd_redeploy "${1:-api}" ;;
  expose)             cmd_expose ;;
  watch)              cmd_watch ;;
  load)  shift;       cmd_load "${1:-60}" ;;
  down)               cmd_down ;;
  *) die "unknown command '${1}'. Use: up | redeploy [svc] | expose | langs | seed | watch | load [N] | down" ;;
esac
