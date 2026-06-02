#!/usr/bin/env bash
# =============================================================================
# CodeRuntime — Refresh ECR auth for on-demand image pulls
# =============================================================================
# The worker pulls missing code-runtime-* images in-process via the Docker
# API, which does NOT read `docker login` — it needs an explicit
# X-Registry-Auth value (CODERUNTIME_DOCKER_REGISTRY_AUTH). ECR tokens expire
# after 12h, so this script mints a fresh token, writes it to an env file, and
# restarts the worker so the new auth takes effect.
#
# Run it once before first deploy, then on a cron (every ~8h) to stay ahead of
# the 12h expiry. Already-pulled images keep working regardless; only NEW
# on-demand pulls need valid auth.
#
# Usage:
#   AWS_REGION=us-east-1 REGISTRY=<acct>.dkr.ecr.us-east-1.amazonaws.com \
#     bash scripts/ecr-refresh-auth.sh
#
# Environment:
#   REGISTRY     Required. ECR registry prefix (the account/region host).
#   AWS_REGION   Required. Region passed to `aws ecr get-login-password`.
#   ENV_FILE     Optional. Env file to write (default: <project root>/.env).
#   COMPOSE_FILES Optional. Compose files used to restart the worker
#                (default: "-f docker-compose.yml -f docker-compose.single.yml").
#   NO_RESTART   Optional. Set to any value to skip restarting the worker.
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

REGISTRY="${REGISTRY:-}"
AWS_REGION="${AWS_REGION:-}"
ENV_FILE="${ENV_FILE:-${PROJECT_ROOT}/.env}"
COMPOSE_FILES="${COMPOSE_FILES:--f docker-compose.yml -f docker-compose.single.yml}"

GREEN='\033[0;32m'; RED='\033[0;31m'; NC='\033[0m'
log()   { echo -e "${GREEN}[ecr-auth]${NC} $*"; }
error() { echo -e "${RED}[ecr-auth]${NC} $*" >&2; exit 1; }

[ -n "${REGISTRY}" ]   || error "REGISTRY is required (e.g. <acct>.dkr.ecr.<region>.amazonaws.com)"
[ -n "${AWS_REGION}" ] || error "AWS_REGION is required"
command -v aws    >/dev/null 2>&1 || error "'aws' CLI not found"
command -v base64 >/dev/null 2>&1 || error "'base64' not found"

log "Requesting ECR token for ${REGISTRY} (${AWS_REGION})..."
TOKEN="$(aws ecr get-login-password --region "${AWS_REGION}")" \
    || error "aws ecr get-login-password failed — check IAM role / credentials"

# X-Registry-Auth is base64(JSON{username,password,serveraddress}). Use -w0
# where supported (GNU); fall back to tr-stripping newlines (BSD/macOS).
JSON="$(printf '{"username":"AWS","password":"%s","serveraddress":"%s"}' "${TOKEN}" "${REGISTRY}")"
if base64 --help 2>&1 | grep -q -- '-w'; then
    AUTH="$(printf '%s' "${JSON}" | base64 -w0)"
else
    AUTH="$(printf '%s' "${JSON}" | base64 | tr -d '\n')"
fi

# Upsert REGISTRY and REGISTRY_AUTH in the env file without disturbing the rest.
touch "${ENV_FILE}"
tmp="$(mktemp)"
grep -vE '^(REGISTRY|REGISTRY_AUTH)=' "${ENV_FILE}" > "${tmp}" || true
{
    echo "REGISTRY=${REGISTRY}"
    echo "REGISTRY_AUTH=${AUTH}"
} >> "${tmp}"
mv "${tmp}" "${ENV_FILE}"
chmod 600 "${ENV_FILE}"
log "Wrote REGISTRY + REGISTRY_AUTH to ${ENV_FILE} (mode 600)"

if [ -n "${NO_RESTART:-}" ]; then
    log "NO_RESTART set — skipping worker restart. New auth applies on next 'up'."
    exit 0
fi

log "Restarting worker to pick up the new auth..."
cd "${PROJECT_ROOT}"
# shellcheck disable=SC2086
docker compose ${COMPOSE_FILES} up -d worker
log "Done. Worker is using a fresh ECR token (valid ~12h)."
