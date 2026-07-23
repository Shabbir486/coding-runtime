#!/usr/bin/env bash
# =============================================================================
# Code Runtime — one-go AWS / EKS PRODUCTION deploy.
#
# Reproduces EXACTLY the live cluster from nothing, using explicit AWS commands
# for every resource: SQS queues, ECR repos + images, the EKS cluster (eksctl:
# VPC reuse, IRSA, single 1->12 nodegroup, addons), Kubernetes secrets, KEDA,
# cluster-autoscaler, the ECR-token-refresher, and all workloads. No manual
# `kubectl patch`. Safe to re-run — every step is idempotent (skips existing).
#
# Prereqs: awscli v2, eksctl, kubectl, helm, docker(+buildx); AWS admin creds.
#   - An RDS/Aurora MySQL reachable from the cluster VPC, DB `coderuntime`
#     created, its SG allowing 3306 from the EKS cluster SG. (Kept across teardown
#     — it is NOT created/destroyed here.)
#   - Edit cluster.yaml for your VPC/subnet IDs if they differ.
#
# Required env (export, or `source` a .env):
#   DB_HOST                 RDS endpoint
#   DB_USER / DB_PASSWORD   RDS creds (DB `coderuntime` must exist)
#   JWT_SECRET              long random string
#   REDIS_PASSWORD          any strong string
# Optional (sensible live defaults):
#   AWS_REGION=ap-southeast-2  AWS_ACCOUNT_ID=768054003064
#   API_TAG=v4  WORKER_TAG=v2  QM_TAG=v1     (the currently-deployed image tags)
#   BUILD_RUNTIME_IMAGES=true|false (default true)
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"
M="$SCRIPT_DIR/manifests"

# ---- config -----------------------------------------------------------------
AWS_REGION="${AWS_REGION:-ap-southeast-2}"
AWS_ACCOUNT_ID="${AWS_ACCOUNT_ID:-768054003064}"
CLUSTER="${CLUSTER:-coderuntime}"
NS="${NS:-coderuntime}"
REGISTRY="$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com"
# Currently-deployed per-service image tags (do NOT collapse to one tag —
# api/worker/queue-manager are on DIFFERENT tags in the live cluster).
API_TAG="${API_TAG:-v4}"; WORKER_TAG="${WORKER_TAG:-v2}"; QM_TAG="${QM_TAG:-v1}"
BUILD_RUNTIME_IMAGES="${BUILD_RUNTIME_IMAGES:-true}"
JOBS_QUEUE="code-submission-processing-queue"
COMPLETED_QUEUE="code-submission-completed-queue"
CA_POLICY="coderuntime-cluster-autoscaler"
ECR_REFRESH_POLICY="coderuntime-ecr-token-refresh"

log(){ printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
need(){ command -v "$1" >/dev/null 2>&1 || { echo "missing prerequisite: $1" >&2; exit 1; }; }
need aws; need eksctl; need kubectl; need helm; need docker
: "${DB_HOST:?set DB_HOST}"; : "${DB_USER:?set DB_USER}"; : "${DB_PASSWORD:?set DB_PASSWORD}"
: "${JWT_SECRET:?set JWT_SECRET}"; : "${REDIS_PASSWORD:?set REDIS_PASSWORD}"

policy_arn(){ aws iam list-policies --scope Local --query "Policies[?PolicyName=='$1'].Arn" --output text; }
ensure_policy(){ # name file -> echoes ARN
  local arn; arn="$(policy_arn "$1")"
  if [ -z "$arn" ] || [ "$arn" = "None" ]; then
    arn="$(aws iam create-policy --policy-name "$1" --policy-document file://"$2" --query 'Policy.Arn' --output text)"
  fi
  echo "$arn"
}

# ---- 1. SQS queues ----------------------------------------------------------
log "1/11 SQS queues"
queue_url(){ # name -> creates if missing, echoes URL
  aws sqs get-queue-url --queue-name "$1" --region "$AWS_REGION" --query QueueUrl --output text 2>/dev/null \
    || aws sqs create-queue --queue-name "$1" --region "$AWS_REGION" --query QueueUrl --output text
}
SQS_JOBS_QUEUE_URL="$(queue_url "$JOBS_QUEUE")"
SQS_COMPLETED_QUEUE_URL="$(queue_url "$COMPLETED_QUEUE")"
echo "jobs:      $SQS_JOBS_QUEUE_URL"
echo "completed: $SQS_COMPLETED_QUEUE_URL"

# ---- 2. cluster (eksctl: VPC reuse, IRSA, 1->12 nodegroup, addons) ----------
log "2/11 cluster"
if eksctl get cluster --name "$CLUSTER" --region "$AWS_REGION" >/dev/null 2>&1; then
  echo "cluster $CLUSTER exists — skipping create"
else
  eksctl create cluster -f "$SCRIPT_DIR/cluster.yaml"
fi
aws eks update-kubeconfig --name "$CLUSTER" --region "$AWS_REGION"

# ---- 3. ECR repos + service images (per-service live tags) ------------------
log "3/11 service images -> ECR"
aws ecr get-login-password --region "$AWS_REGION" | docker login --username AWS --password-stdin "$REGISTRY" >/dev/null
build_push(){ # svc tag
  aws ecr describe-repositories --repository-names "coderuntime/$1" --region "$AWS_REGION" >/dev/null 2>&1 \
    || aws ecr create-repository --repository-name "coderuntime/$1" --region "$AWS_REGION" >/dev/null
  docker buildx build --platform linux/amd64 -f "docker/Dockerfile.$1" \
    -t "$REGISTRY/coderuntime/$1:$2" --push .
}
build_push api           "$API_TAG"
build_push worker        "$WORKER_TAG"
build_push queue-manager "$QM_TAG"

# ---- 4. runtime images (40 languages) -> ECR --------------------------------
if [ "$BUILD_RUNTIME_IMAGES" = "true" ]; then
  log "4/11 runtime images -> ECR (parallel)"
  LANGS="$(ls runtime-images)" JOBS="${JOBS:-5}" bash scripts/build-push-runtime-ecr-parallel.sh
else
  echo "4/11 runtime images — skipped (BUILD_RUNTIME_IMAGES=false)"
fi

# ---- 5. namespace + storageclass -------------------------------------------
log "5/11 namespace + storageclass"
kubectl apply -f "$M/00-namespace.yaml"
kubectl apply -f "$M/01-storageclass.yaml"

# ---- 6. secrets (app creds + ECR pull auth) --------------------------------
log "6/11 secrets"
kubectl -n "$NS" create secret generic api-secrets \
  --from-literal=DATABASE_HOST="$DB_HOST" --from-literal=DATABASE_USER="$DB_USER" \
  --from-literal=DATABASE_PASSWORD="$DB_PASSWORD" --from-literal=JWT_SECRET="$JWT_SECRET" \
  --from-literal=REDIS_HOST=redis --from-literal=REDIS_PASSWORD="$REDIS_PASSWORD" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" create secret generic worker-secrets \
  --from-literal=DATABASE_HOST="$DB_HOST" --from-literal=DATABASE_USER="$DB_USER" \
  --from-literal=DATABASE_PASSWORD="$DB_PASSWORD" \
  --from-literal=REDIS_HOST=redis --from-literal=REDIS_PASSWORD="$REDIS_PASSWORD" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" create secret generic redis-secrets \
  --from-literal=REDIS_PASSWORD="$REDIS_PASSWORD" \
  --dry-run=client -o yaml | kubectl apply -f -
# ECR X-Registry-Auth (worker reads it at start; then self-refreshes via IRSA,
# and the CronJob in step 10 rolls it every 6h as a belt-and-braces safety net).
TOKEN=$(aws ecr get-login-password --region "$AWS_REGION")
XAUTH=$(python3 -c "import json,base64,sys;print(base64.b64encode(json.dumps({'username':'AWS','password':sys.argv[1],'serveraddress':sys.argv[2]}).encode()).decode())" "$TOKEN" "$REGISTRY")
kubectl -n "$NS" create secret generic ecr-registry-auth \
  --from-literal=CODERUNTIME_DOCKER_REGISTRY="$REGISTRY" \
  --from-literal=CODERUNTIME_DOCKER_REGISTRY_AUTH="$XAUTH" \
  --dry-run=client -o yaml | kubectl apply -f -

# ---- 7. KEDA (uses the IRSA keda-operator SA from cluster.yaml) -------------
log "7/11 KEDA"
helm repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
helm repo update >/dev/null
helm upgrade --install keda kedacore/keda -n keda --create-namespace \
  --set serviceAccount.operator.name=keda-operator
kubectl -n keda rollout status deploy/keda-operator --timeout=180s

# ---- 8. cluster-autoscaler (IAM policy + IRSA SA + deploy) ------------------
log "8/11 cluster-autoscaler (nodes 1->12)"
CA_ARN="$(ensure_policy "$CA_POLICY" "$SCRIPT_DIR/ca-policy.json")"
eksctl create iamserviceaccount --cluster="$CLUSTER" --region "$AWS_REGION" \
  --namespace=kube-system --name=cluster-autoscaler --attach-policy-arn="$CA_ARN" \
  --override-existing-serviceaccounts --approve
kubectl apply -f "$SCRIPT_DIR/cluster-autoscaler.yaml"

# ---- 9. ECR-token-refresher IRSA SA ----------------------------------------
log "9/11 ecr-token-refresher service account (IRSA)"
ECR_ARN="$(ensure_policy "$ECR_REFRESH_POLICY" "$SCRIPT_DIR/ecr-token-refresh-policy.json")"
eksctl create iamserviceaccount --cluster="$CLUSTER" --region "$AWS_REGION" \
  --namespace="$NS" --name=ecr-token-refresher --attach-policy-arn="$ECR_ARN" \
  --override-existing-serviceaccounts --approve

# ---- 10. config + workloads (in order) -------------------------------------
log "10/11 config + workloads"
kubectl apply -f "$M/02-configmaps.yaml"
kubectl -n "$NS" patch cm common-config --type=merge -p \
  "{\"data\":{\"CODERUNTIME_SQS_JOBS_QUEUE_URL\":\"$SQS_JOBS_QUEUE_URL\",\"CODERUNTIME_SQS_START_QUEUE_URL\":\"$SQS_COMPLETED_QUEUE_URL\",\"CODERUNTIME_SQS_REGION\":\"$AWS_REGION\"}}"
kubectl apply -f "$M/10-redis.yaml"
kubectl apply -f "$M/20-api-gateway.yaml"
kubectl apply -f "$M/30-queue-manager.yaml"
kubectl apply -f "$M/40-worker.yaml"
kubectl apply -f "$M/60-ecr-token-refresh.yaml"
# Pin images to the exact currently-deployed tags.
kubectl -n "$NS" set image deploy/api-gateway   api-gateway="$REGISTRY/coderuntime/api:$API_TAG"
kubectl -n "$NS" set image deploy/queue-manager queue-manager="$REGISTRY/coderuntime/queue-manager:$QM_TAG"
kubectl -n "$NS" set image deploy/worker        worker="$REGISTRY/coderuntime/worker:$WORKER_TAG"

# ---- 11. wait + report ------------------------------------------------------
log "11/11 wait for rollout"
kubectl -n "$NS" rollout status deploy/api-gateway --timeout=300s
kubectl -n "$NS" rollout status deploy/queue-manager --timeout=300s
kubectl -n "$NS" rollout status deploy/worker --timeout=300s
echo
echo "Public endpoint (ELB DNS — may take ~2 min to resolve):"
kubectl -n "$NS" get svc api-gateway-lb -o jsonpath='  http://{.status.loadBalancer.ingress[0].hostname}{"\n"}' 2>/dev/null || true
echo "Done — live cluster reproduced (nodegroup 1->12, KEDA 1->20)."
