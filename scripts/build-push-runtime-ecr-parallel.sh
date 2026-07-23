#!/usr/bin/env bash
# Build runtime-images/<lang> for linux/amd64 and push to ECR in parallel.
# Usage: LANGS="clojure cpp ..." JOBS=4 ./build-push-runtime-ecr-parallel.sh
# Idempotent: a lang whose :latest manifest already exists in ECR is skipped.
set -uo pipefail
REGION=ap-southeast-2; ACCOUNT=768054003064
REGISTRY=$ACCOUNT.dkr.ecr.$REGION.amazonaws.com
JOBS="${JOBS:-4}"
LANGS="${LANGS:-$(ls runtime-images)}"
LOGDIR="${LOGDIR:-/tmp/ecr-build-logs}"
mkdir -p "$LOGDIR"

aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$REGISTRY" >/dev/null 2>&1

build_one() {
  local l="$1" repo="code-runtime-$1"
  local log="$LOGDIR/$l.log"
  aws ecr describe-repositories --repository-names "$repo" --region "$REGION" >/dev/null 2>&1 \
    || aws ecr create-repository --repository-name "$repo" --region "$REGION" >/dev/null 2>&1
  # skip if already pushed
  if aws ecr describe-images --repository-name "$repo" --image-ids imageTag=latest --region "$REGION" >/dev/null 2>&1; then
    echo "SKIP   $l (already in ECR)"; return 0
  fi
  echo "BUILD  $l ..."
  if docker buildx build --platform linux/amd64 -t "$REGISTRY/$repo:latest" --push "runtime-images/$l" >"$log" 2>&1; then
    echo "OK     $l"
  else
    echo "FAIL   $l (see $log)"; tail -3 "$log" | sed "s/^/   $l| /"
  fi
}
export -f build_one
export REGION REGISTRY LOGDIR

printf '%s\n' $LANGS | xargs -P "$JOBS" -I{} bash -c 'build_one "$@"' _ {}
echo ">> parallel build complete"
