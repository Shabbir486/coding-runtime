#!/usr/bin/env bash
# Build runtime-images/<lang> for linux/amd64 and push to ECR as
# <registry>/code-runtime-<lang>:latest. Usage: LANGS="bash golang" ./build-push-runtime-ecr.sh
set -euo pipefail
REGION=ap-southeast-2; ACCOUNT=768054003064
REGISTRY=$ACCOUNT.dkr.ecr.$REGION.amazonaws.com
LANGS="${LANGS:-$(ls runtime-images)}"
aws ecr get-login-password --region $REGION | docker login --username AWS --password-stdin $REGISTRY >/dev/null 2>&1
for l in $LANGS; do
  repo="code-runtime-$l"
  aws ecr describe-repositories --repository-names "$repo" --region $REGION >/dev/null 2>&1 \
    || aws ecr create-repository --repository-name "$repo" --region $REGION >/dev/null
  echo ">> build+push $repo (amd64)"
  docker buildx build --platform linux/amd64 -t "$REGISTRY/$repo:latest" \
    --push "runtime-images/$l" 2>&1 | tail -1
done
echo ">> done: $LANGS"
