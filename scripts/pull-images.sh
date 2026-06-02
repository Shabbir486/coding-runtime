#!/usr/bin/env bash
# =============================================================================
# CodeRuntime — Build Custom Runtime Images + Pull Official Images
# =============================================================================
# Builds every Dockerfile under runtime-images/ as code-runtime-<dir>:latest,
# then pulls all official public images used by language.go.
#
# Usage:
#   bash scripts/pull-images.sh [--push] [--pull-only] [--parallel N]
#
# Options:
#   --push        Push built images to a registry (set REGISTRY env var)
#   --pull-only   Skip building; pull pre-built images from REGISTRY instead.
#                 Use this on production/deploy hosts so they never compile
#                 toolchains. Requires REGISTRY to be set.
#   --parallel N  Number of parallel docker build/pull jobs (default: 4)
#
# Environment:
#   REGISTRY      Registry prefix, e.g. <acct>.dkr.ecr.<region>.amazonaws.com
#   LANGUAGES     Space-separated allowlist of languages to build/pull.
#                 When unset, all languages under runtime-images/ are processed.
#                 Example: LANGUAGES="python nodejs golang java"
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
RUNTIME_IMAGES_DIR="${PROJECT_ROOT}/runtime-images"

REGISTRY="${REGISTRY:-}"
PUSH_IMAGES=false
PULL_ONLY=false
PARALLEL="${PARALLEL:-4}"
# Optional space-separated allowlist of languages to process.
LANGUAGES="${LANGUAGES:-}"

for arg in "$@"; do
    case "$arg" in
        --push)         PUSH_IMAGES=true ;;
        --pull-only)    PULL_ONLY=true ;;
        --parallel=*)   PARALLEL="${arg#--parallel=}" ;;
        --help|-h)
            sed -n '2,24p' "$0" | sed 's/^# \?//'
            exit 0 ;;
    esac
done

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; BLUE='\033[0;34m'; NC='\033[0m'
# _log avoids shadowing macOS's /usr/bin/log system command
_log()    { echo -e "${GREEN}[images]${NC} $*"; }
warn()    { echo -e "${YELLOW}[images]${NC} $*"; }
error()   { echo -e "${RED}[images]${NC} $*" >&2; exit 1; }
heading() { echo -e "\n${BLUE}─── $* ───${NC}"; }

BUILD_OK=0
BUILD_FAIL=0

# ─── 1. Build custom runtime images ──────────────────────────────────────────

heading "Building custom runtime images"

# Discovers all runtime-images/<lang>/Dockerfile directories and builds each
# as code-runtime-<lang>:latest (optionally also tagged with REGISTRY prefix).
build_one() {
    local lang="$1"
    local context="${RUNTIME_IMAGES_DIR}/${lang}"
    local dockerfile="${context}/Dockerfile"
    local local_tag="code-runtime-${lang}:latest"

    if [ ! -f "${dockerfile}" ]; then
        warn "  [${lang}] No Dockerfile — skipping"
        return
    fi

    local tags=("--tag" "${local_tag}")
    if [ -n "${REGISTRY}" ]; then
        tags+=("--tag" "${REGISTRY}/code-runtime-${lang}:latest")
    fi

    _log "  Building ${lang} → ${local_tag}"
    if docker build \
        "${tags[@]}" \
        --label "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        --label "org.opencontainers.image.title=code-runtime-${lang}" \
        "${context}" > /tmp/build_${lang}.log 2>&1; then
        _log "  ${lang}: OK"
        if [ "${PUSH_IMAGES}" = "true" ] && [ -n "${REGISTRY}" ]; then
            docker push "${REGISTRY}/code-runtime-${lang}:latest"
        fi
    else
        warn "  ${lang}: FAILED (see /tmp/build_${lang}.log)"
        return
    fi
}

# Pulls a pre-built image from REGISTRY and re-tags it as the local
# code-runtime-<lang>:latest that the worker expects. Used by --pull-only so
# deploy hosts download images instead of compiling toolchains.
pull_one() {
    local lang="$1"
    local local_tag="code-runtime-${lang}:latest"
    local remote_tag="${REGISTRY}/code-runtime-${lang}:latest"

    _log "  Pulling ${remote_tag}"
    if docker pull "${remote_tag}" > /tmp/pull_${lang}.log 2>&1; then
        docker tag "${remote_tag}" "${local_tag}"
        _log "  ${lang}: OK"
    else
        warn "  ${lang}: FAILED (see /tmp/pull_${lang}.log)"
        return
    fi
}

# --pull-only requires a registry to pull from.
if [ "${PULL_ONLY}" = "true" ] && [ -z "${REGISTRY}" ]; then
    error "--pull-only requires REGISTRY to be set (e.g. <acct>.dkr.ecr.<region>.amazonaws.com)"
fi

# Returns 0 if the language is allowed by the LANGUAGES allowlist (or no
# allowlist is set), 1 otherwise.
lang_allowed() {
    [ -z "${LANGUAGES}" ] && return 0
    [[ " ${LANGUAGES} " == *" $1 "* ]]
}

# Collect all directories that have a Dockerfile, filtered by the allowlist.
LANGS=()
for dir in "${RUNTIME_IMAGES_DIR}"/*/; do
    lang="$(basename "${dir}")"
    if [ -f "${dir}Dockerfile" ] && lang_allowed "${lang}"; then
        LANGS+=("${lang}")
    fi
done

# Which worker function runs per language depends on the mode.
if [ "${PULL_ONLY}" = "true" ]; then
    WORK_FN="pull_one"; VERB="pull"
else
    WORK_FN="build_one"; VERB="build"
fi

if [ ${#LANGS[@]} -eq 0 ]; then
    if [ -n "${LANGUAGES}" ]; then
        warn "No matching Dockerfiles for LANGUAGES='${LANGUAGES}' under ${RUNTIME_IMAGES_DIR}"
    else
        warn "No Dockerfiles found under ${RUNTIME_IMAGES_DIR}"
    fi
else
    _log "Found ${#LANGS[@]} custom images to ${VERB}: ${LANGS[*]}"

    if [ "${PARALLEL}" -gt 1 ] && command -v xargs &>/dev/null; then
        # Export functions and variables so each child shell spawned by xargs has them
        export -f build_one pull_one _log warn error
        export GREEN YELLOW RED BLUE NC
        export RUNTIME_IMAGES_DIR REGISTRY PUSH_IMAGES
        printf '%s\n' "${LANGS[@]}" | \
            xargs -P "${PARALLEL}" -I{} bash -c "${WORK_FN} \"\$@\"" _ {}
    else
        for lang in "${LANGS[@]}"; do
            "${WORK_FN}" "${lang}"
        done
    fi
fi

# ─── 2. Pull official public images ──────────────────────────────────────────

heading "Pulling official language images"

# Every language listed in internal/models/language.go and every DB referenced
# by internal/runtime/db_runtime.go now ships as a local code-runtime-<lang>
# image under runtime-images/<lang>/. The builder loop above handles them all
# — there is nothing left to pull from Docker Hub here.
LANG_IMAGES=()

# `set -u` (enabled at top) treats an empty array as unbound, so we guard the
# loop on length rather than expanding the array directly.
if [ "${#LANG_IMAGES[@]}" -gt 0 ]; then
    for image in "${LANG_IMAGES[@]}"; do
        _log "  Pulling ${image}..."
        docker pull "${image}" 2>&1 | tail -1 || warn "  Failed to pull ${image}"
    done
else
    _log "  No upstream language images to pull — all built locally."
fi

# ─── 3. Pull infrastructure images ───────────────────────────────────────────

heading "Pulling infrastructure images"

INFRA_IMAGES=(
    "redis:7-alpine"
    "nats:2.10-alpine"
    "prom/prometheus:v2.55.0"
    "grafana/grafana:11.4.0"
    "jaegertracing/all-in-one:1.64.0"
    "prometheuscommunity/postgres-exporter:v0.15.0"
    "oliver006/redis_exporter:v1.65.0"
    "natsio/prometheus-nats-exporter:0.15.0"
)

for image in "${INFRA_IMAGES[@]}"; do
    _log "  Pulling ${image}..."
    docker pull "${image}" 2>&1 | tail -1 || warn "  Failed to pull ${image}"
done

# ─── Summary ──────────────────────────────────────────────────────────────────

heading "Summary"

_log "Custom images available:"
docker images --filter "reference=code-runtime-*" \
    --format "  {{.Repository}}:{{.Tag}}  ({{.Size}})" 2>/dev/null || true
