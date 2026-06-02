#!/usr/bin/env bash
# =============================================================================
# CodeRuntime — Build Custom Runtime Images + Pull Official Images
# =============================================================================
# Builds every Dockerfile under runtime-images/ as code-runtime-<dir>:latest
# (or pulls pre-built images from a registry with --pull-only), then pulls the
# infrastructure images. Every step's success/failure is tracked across
# parallel jobs and reported in a final summary; failed jobs print the tail of
# their log inline so the actual error is visible.
#
# Usage:
#   bash scripts/pull-images.sh [--push] [--pull-only] [--strict]
#                               [--no-infra] [--parallel N]
#
# Options:
#   --push        Build, then push each image to REGISTRY (requires REGISTRY).
#   --pull-only   Skip building; pull pre-built images from REGISTRY and re-tag
#                 them locally. For deploy hosts that must not compile. Requires
#                 REGISTRY.
#   --strict      Exit non-zero if ANY image (language or infra) fails. Default
#                 tolerates partial failures (e.g. arch-specific) and only exits
#                 non-zero when every language image fails or a precondition is
#                 unmet.
#   --no-infra    Do not pull the infrastructure images (redis/nats/etc.).
#   --parallel N  Number of parallel docker build/pull jobs (default: 4).
#
# Environment:
#   REGISTRY      Registry prefix, e.g. <acct>.dkr.ecr.<region>.amazonaws.com
#   LANGUAGES     Space-separated allowlist of languages to build/pull. When
#                 unset, all languages under runtime-images/ are processed.
#                 Example: LANGUAGES="python nodejs golang java"
#   PULL_RETRIES  Attempts per pull/push before giving up (default: 3).
#
# Exit codes:
#   0  all requested images succeeded (or partial failure without --strict)
#   1  precondition failed, every language image failed, or --strict + any fail
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
RUNTIME_IMAGES_DIR="${PROJECT_ROOT}/runtime-images"

REGISTRY="${REGISTRY:-}"
PUSH_IMAGES=false
PULL_ONLY=false
STRICT=false
SKIP_INFRA=false
PARALLEL="${PARALLEL:-4}"
PULL_RETRIES="${PULL_RETRIES:-3}"
# Optional space-separated allowlist of languages to process.
LANGUAGES="${LANGUAGES:-}"

for arg in "$@"; do
    case "$arg" in
        --push)         PUSH_IMAGES=true ;;
        --pull-only)    PULL_ONLY=true ;;
        --strict)       STRICT=true ;;
        --no-infra)     SKIP_INFRA=true ;;
        --parallel=*)   PARALLEL="${arg#--parallel=}" ;;
        --help|-h)
            sed -n '2,40p' "$0" | sed 's/^# \?//'
            exit 0 ;;
        *) echo "Unknown option: $arg (try --help)" >&2; exit 2 ;;
    esac
done

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; BLUE='\033[0;34m'; NC='\033[0m'
# _log avoids shadowing macOS's /usr/bin/log system command
_log()    { echo -e "${GREEN}[images]${NC} $*"; }
warn()    { echo -e "${YELLOW}[images]${NC} $*"; }
error()   { echo -e "${RED}[images]${NC} $*" >&2; exit 1; }
heading() { echo -e "\n${BLUE}─── $* ───${NC}"; }

# ─── Working dirs ─────────────────────────────────────────────────────────────
# STATUS_DIR holds one <lang>.status file per job so results survive the xargs
# subshells. LOG_DIR persists build/pull logs so they can be inspected after.
STATUS_DIR="$(mktemp -d "${TMPDIR:-/tmp}/coderuntime-status.XXXXXX")"
LOG_DIR="${TMPDIR:-/tmp}/coderuntime-image-logs"
rm -rf "${LOG_DIR}"; mkdir -p "${LOG_DIR}"
cleanup() { rm -rf "${STATUS_DIR}"; }
trap cleanup EXIT

# On Ctrl+C / SIGTERM, stop in-flight child jobs (xargs + docker build clients)
# before the EXIT trap removes the status dir, so we don't race a late record()
# write or leak orphaned build output after the prompt returns.
on_interrupt() {
    trap '' INT TERM            # ignore repeated signals while shutting down
    echo -e "\n${YELLOW}[images]${NC} Interrupted — stopping in-flight builds..." >&2
    pkill -TERM -P $$ 2>/dev/null || true
    wait 2>/dev/null || true
    exit 130
}
trap on_interrupt INT TERM

# ─── Preconditions ────────────────────────────────────────────────────────────
command -v docker >/dev/null 2>&1 || error "'docker' not found in PATH."
docker info >/dev/null 2>&1 \
    || error "Docker daemon is not reachable. Start Docker and retry (check: docker info)."

[[ "${PARALLEL}" =~ ^[0-9]+$ ]] && [ "${PARALLEL}" -ge 1 ] \
    || error "--parallel must be a positive integer (got '${PARALLEL}')."
[[ "${PULL_RETRIES}" =~ ^[0-9]+$ ]] && [ "${PULL_RETRIES}" -ge 1 ] \
    || error "PULL_RETRIES must be a positive integer (got '${PULL_RETRIES}')."

if [ "${PULL_ONLY}" = "true" ] && [ "${PUSH_IMAGES}" = "true" ]; then
    error "--pull-only and --push are mutually exclusive."
fi
if [ "${PULL_ONLY}" = "true" ] && [ -z "${REGISTRY}" ]; then
    error "--pull-only requires REGISTRY (e.g. <acct>.dkr.ecr.<region>.amazonaws.com)."
fi
if [ "${PUSH_IMAGES}" = "true" ] && [ -z "${REGISTRY}" ]; then
    error "--push requires REGISTRY (e.g. <acct>.dkr.ecr.<region>.amazonaws.com)."
fi

# Best-effort credential check for private registries.
if [ -n "${REGISTRY}" ] && { [ "${PUSH_IMAGES}" = "true" ] || [ "${PULL_ONLY}" = "true" ]; }; then
    registry_host="${REGISTRY%%/*}"
    if ! grep -q "${registry_host}" "${HOME}/.docker/config.json" 2>/dev/null; then
        warn "No stored credentials for '${registry_host}' in ~/.docker/config.json."
        warn "If it is private (e.g. ECR), run 'docker login ${registry_host}' first or pulls/pushes will fail."
    fi
fi

# ─── Helpers (exported for xargs subshells) ──────────────────────────────────
# retry <attempts> <cmd...> — re-runs cmd with linear backoff on failure.
retry() {
    local attempts="$1"; shift
    local n=1
    until "$@"; do
        if [ "${n}" -ge "${attempts}" ]; then return 1; fi
        warn "    attempt ${n}/${attempts} failed; retrying in $((n * 2))s..."
        sleep "$((n * 2))"
        n=$((n + 1))
    done
    return 0
}

# record <lang> <status> — persist a job's outcome (ok | skip | fail:<reason>).
# Tolerates the status dir having been removed (e.g. by the cleanup trap during
# an interrupt) so a late write never prints a "No such file or directory" error.
record() { echo "$2" > "${STATUS_DIR}/$1.status" 2>/dev/null || true; }

# build_one always returns 0; outcome is captured via record() so neither
# `set -e` nor xargs aborts the run on an individual failure.
build_one() {
    local lang="$1"
    local context="${RUNTIME_IMAGES_DIR}/${lang}"
    local dockerfile="${context}/Dockerfile"
    local local_tag="code-runtime-${lang}:latest"
    local logf="${LOG_DIR}/build_${lang}.log"

    if [ ! -f "${dockerfile}" ]; then
        warn "  [${lang}] No Dockerfile — skipping"
        record "${lang}" "skip"
        return 0
    fi

    local tags=("--tag" "${local_tag}")
    [ -n "${REGISTRY}" ] && tags+=("--tag" "${REGISTRY}/code-runtime-${lang}:latest")

    _log "  Building ${lang} → ${local_tag}"
    if ! docker build "${tags[@]}" \
            --label "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
            --label "org.opencontainers.image.title=code-runtime-${lang}" \
            "${context}" > "${logf}" 2>&1; then
        warn "  ${lang}: BUILD FAILED (log: ${logf})"
        record "${lang}" "fail:build"
        return 0
    fi

    if [ "${PUSH_IMAGES}" = "true" ]; then
        if ! retry "${PULL_RETRIES}" docker push "${REGISTRY}/code-runtime-${lang}:latest" >> "${logf}" 2>&1; then
            warn "  ${lang}: PUSH FAILED (log: ${logf})"
            record "${lang}" "fail:push"
            return 0
        fi
    fi

    _log "  ${lang}: OK"
    record "${lang}" "ok"
    return 0
}

# pull_one pulls a pre-built image from REGISTRY and re-tags it to the bare
# local name the worker expects.
pull_one() {
    local lang="$1"
    local local_tag="code-runtime-${lang}:latest"
    local remote_tag="${REGISTRY}/code-runtime-${lang}:latest"
    local logf="${LOG_DIR}/pull_${lang}.log"

    _log "  Pulling ${remote_tag}"
    if ! retry "${PULL_RETRIES}" docker pull "${remote_tag}" > "${logf}" 2>&1; then
        warn "  ${lang}: PULL FAILED (log: ${logf})"
        record "${lang}" "fail:pull"
        return 0
    fi
    if ! docker tag "${remote_tag}" "${local_tag}" >> "${logf}" 2>&1; then
        warn "  ${lang}: TAG FAILED (log: ${logf})"
        record "${lang}" "fail:tag"
        return 0
    fi
    _log "  ${lang}: OK"
    record "${lang}" "ok"
    return 0
}

# show_log_tail <logfile> [lines] — print the tail of a failed job's log.
show_log_tail() {
    local logf="$1" lines="${2:-15}"
    [ -f "${logf}" ] || return 0
    echo -e "${RED}    ── last ${lines} lines of ${logf} ──${NC}"
    tail -n "${lines}" "${logf}" 2>/dev/null | sed 's/^/    /'
}

# ─── 1. Build / pull custom runtime images ───────────────────────────────────
heading "$([ "${PULL_ONLY}" = "true" ] && echo "Pulling pre-built runtime images" || echo "Building custom runtime images")"

lang_allowed() {
    [ -z "${LANGUAGES}" ] && return 0
    [[ " ${LANGUAGES} " == *" $1 "* ]]
}

[ -d "${RUNTIME_IMAGES_DIR}" ] || error "runtime-images dir not found: ${RUNTIME_IMAGES_DIR}"

LANGS=()
for dir in "${RUNTIME_IMAGES_DIR}"/*/; do
    lang="$(basename "${dir}")"
    if [ -f "${dir}Dockerfile" ] && lang_allowed "${lang}"; then
        LANGS+=("${lang}")
    fi
done

# Warn about allowlist entries that matched no directory (likely a typo).
if [ -n "${LANGUAGES}" ]; then
    for want in ${LANGUAGES}; do
        [ -f "${RUNTIME_IMAGES_DIR}/${want}/Dockerfile" ] \
            || warn "LANGUAGES entry '${want}' has no runtime-images/${want}/Dockerfile — ignored."
    done
fi

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

    export -f build_one pull_one retry record _log warn
    export GREEN YELLOW RED BLUE NC
    export RUNTIME_IMAGES_DIR REGISTRY PUSH_IMAGES PULL_RETRIES STATUS_DIR LOG_DIR

    if [ "${PARALLEL}" -gt 1 ] && command -v xargs >/dev/null 2>&1; then
        # Functions always return 0, so xargs never aborts; results come from
        # the status files. `|| true` guards against any unexpected xargs code.
        printf '%s\n' "${LANGS[@]}" \
            | xargs -P "${PARALLEL}" -I{} bash -c "${WORK_FN} \"\$@\"" _ {} || true
    else
        for lang in "${LANGS[@]}"; do "${WORK_FN}" "${lang}"; done
    fi
fi

# ─── Tally language results ──────────────────────────────────────────────────
LANG_OK=0; LANG_SKIP=0
FAILED_LANGS=()
for lang in "${LANGS[@]}"; do
    st="$(cat "${STATUS_DIR}/${lang}.status" 2>/dev/null || echo "fail:no-status")"
    case "${st}" in
        ok)    LANG_OK=$((LANG_OK + 1)) ;;
        skip)  LANG_SKIP=$((LANG_SKIP + 1)) ;;
        *)     FAILED_LANGS+=("${lang}:${st#fail:}") ;;
    esac
done

# ─── 2. Pull infrastructure images ───────────────────────────────────────────
INFRA_OK=0
INFRA_FAILED=()
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

if [ "${SKIP_INFRA}" = "true" ]; then
    heading "Pulling infrastructure images (skipped: --no-infra)"
else
    heading "Pulling infrastructure images"
    for image in "${INFRA_IMAGES[@]}"; do
        safe="${image//[\/:]/_}"
        logf="${LOG_DIR}/infra_${safe}.log"
        _log "  Pulling ${image}..."
        if retry "${PULL_RETRIES}" docker pull "${image}" > "${logf}" 2>&1; then
            INFRA_OK=$((INFRA_OK + 1))
        else
            warn "  ${image}: FAILED (log: ${logf})"
            INFRA_FAILED+=("${image}")
        fi
    done
fi

# ─── Summary ──────────────────────────────────────────────────────────────────
heading "Summary"

_log "Language images: ${LANG_OK} ok, ${#FAILED_LANGS[@]} failed, ${LANG_SKIP} skipped (of ${#LANGS[@]} requested)"
if [ "${SKIP_INFRA}" = "false" ]; then
    _log "Infra images:    ${INFRA_OK} ok, ${#INFRA_FAILED[@]} failed (of ${#INFRA_IMAGES[@]})"
fi

if [ "${#FAILED_LANGS[@]}" -gt 0 ]; then
    warn "Failed language images:"
    for entry in "${FAILED_LANGS[@]}"; do
        lang="${entry%%:*}"; reason="${entry#*:}"
        warn "  - ${lang} (${reason})"
        show_log_tail "${LOG_DIR}/${VERB}_${lang}.log"
    done
fi
if [ "${#INFRA_FAILED[@]}" -gt 0 ]; then
    warn "Failed infra images: ${INFRA_FAILED[*]}"
fi

_log "Custom images now available:"
docker images --filter "reference=code-runtime-*" \
    --format "  {{.Repository}}:{{.Tag}}  ({{.Size}})" 2>/dev/null || true
_log "Logs: ${LOG_DIR}"

# ─── Exit policy ──────────────────────────────────────────────────────────────
TOTAL_FAILED=$(( ${#FAILED_LANGS[@]} + ${#INFRA_FAILED[@]} ))
if [ "${TOTAL_FAILED}" -eq 0 ]; then
    exit 0
fi
if [ "${STRICT}" = "true" ]; then
    error "Strict mode: ${TOTAL_FAILED} image(s) failed."
fi
if [ "${#LANGS[@]}" -gt 0 ] && [ "${LANG_OK}" -eq 0 ]; then
    error "Every language image failed (${#FAILED_LANGS[@]}/${#LANGS[@]}). Likely a config, auth, or disk problem — see logs above."
fi
warn "Completed with ${TOTAL_FAILED} non-fatal failure(s); see logs above."
exit 0
