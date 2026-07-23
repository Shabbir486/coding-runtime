#!/usr/bin/env bash
# =============================================================================
# Code Runtime — EKS teardown (stop billing; keep the rebuild recipe).
#
# Deletes the EKS cluster and EVERY cluster-created billable resource — the
# managed nodegroup (EC2), the ELB fronting the api, and the EBS volumes behind
# the Redis PVCs — so nothing keeps costing money. setup.sh rebuilds it all.
#
# KEPT by default (so a rebuild is fast and lossless, and they cost ~nothing):
#   - RDS/Aurora MySQL   (your data)
#   - SQS queues         (basically free)
#   - ECR images         (a few cents/mo of storage)
# Pass --purge-ecr, --purge-sqs, and/or --purge-rds to also delete those.
# --purge-rds DELETES the DB with NO final snapshot (permanent data loss); the
# instance id defaults to RDS_INSTANCE_ID (coderuntime-prod). setup.sh does NOT
# recreate RDS, so you must provision a fresh instance before the next rebuild.
#
# Order matters: we delete the LoadBalancer Service and the namespace FIRST so
# AWS reclaims the ELB + EBS volumes, THEN delete the cluster; finally we sweep
# any orphans left tagged for this cluster.
#
# Usage:
#   ./teardown.sh                 # delete cluster + ELB + EBS (keep RDS/SQS/ECR)
#   ./teardown.sh --purge-rds     # delete cluster + ELB + EBS + RDS
#   ./teardown.sh --purge-ecr     # also delete the ECR repos
#   ./teardown.sh --purge-sqs     # also delete the SQS queues
#   ./teardown.sh --yes           # skip the confirmation prompt
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AWS_REGION="${AWS_REGION:-ap-southeast-2}"
CLUSTER="${CLUSTER:-coderuntime}"
NS="${NS:-coderuntime}"
PURGE_ECR=false; PURGE_SQS=false; PURGE_RDS=false; ASSUME_YES=false
RDS_INSTANCE_ID="${RDS_INSTANCE_ID:-coderuntime-prod}"
for a in "$@"; do case "$a" in
  --purge-ecr) PURGE_ECR=true ;; --purge-sqs) PURGE_SQS=true ;; --purge-rds) PURGE_RDS=true ;;
  --yes|-y) ASSUME_YES=true ;;
  *) echo "unknown arg: $a" >&2; exit 2 ;;
esac; done

log(){ printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
need(){ command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need aws; need eksctl; need kubectl

if ! eksctl get cluster --name "$CLUSTER" --region "$AWS_REGION" >/dev/null 2>&1; then
  echo "cluster '$CLUSTER' not found in $AWS_REGION — nothing to delete."; exit 0
fi

if [ "$ASSUME_YES" != "true" ]; then
  echo "About to DELETE EKS cluster '$CLUSTER' ($AWS_REGION): nodegroup, ELB, EBS."
  echo "RDS $([ "$PURGE_RDS" = true ] && echo '(WILL DELETE — data loss)' || echo '(keep)')  |  SQS $([ "$PURGE_SQS" = true ] && echo '(WILL DELETE)' || echo '(keep)')  |  ECR $([ "$PURGE_ECR" = true ] && echo '(WILL DELETE)' || echo '(keep)')"
  read -r -p "Type 'delete' to proceed: " ans
  [ "$ans" = "delete" ] || { echo "aborted."; exit 1; }
fi

aws eks update-kubeconfig --name "$CLUSTER" --region "$AWS_REGION" >/dev/null 2>&1 || true

# ---- 1. remove LoadBalancer Services (frees the ELB + its SG) ---------------
log "1/4 delete LoadBalancer services (frees ELB)"
kubectl -n "$NS" delete svc -l app.kubernetes.io/name=api-gateway --field-selector spec.type=LoadBalancer --ignore-not-found 2>/dev/null || true
# fallback: any LoadBalancer service in the namespace
for s in $(kubectl -n "$NS" get svc -o jsonpath='{range .items[?(@.spec.type=="LoadBalancer")]}{.metadata.name} {end}' 2>/dev/null); do
  kubectl -n "$NS" delete svc "$s" --ignore-not-found 2>/dev/null || true
done
sleep 20  # let the AWS cloud controller delete the ELB before we drop the cluster

# ---- 2. delete namespace (frees Redis EBS volumes via PVC deletion) ---------
log "2/4 delete namespace (frees EBS volumes)"
kubectl delete namespace "$NS" --timeout=180s 2>/dev/null || true

# ---- 3. delete the cluster (nodegroup + control plane + IRSA stacks) --------
log "3/4 eksctl delete cluster (this takes ~15 min)"
eksctl delete cluster --name "$CLUSTER" --region "$AWS_REGION" --disable-nodegroup-eviction --wait

# ---- 4. sweep orphans tagged for this cluster -------------------------------
log "4/4 sweep orphaned ELBs + EBS volumes"
# Classic ELBs tagged kubernetes.io/cluster/<name>
for lb in $(aws elb describe-load-balancers --region "$AWS_REGION" \
      --query 'LoadBalancerDescriptions[].LoadBalancerName' --output text 2>/dev/null); do
  if aws elb describe-tags --region "$AWS_REGION" --load-balancer-names "$lb" \
      --query "TagDescriptions[].Tags[?Key=='kubernetes.io/cluster/${CLUSTER}']" --output text 2>/dev/null | grep -q .; then
    echo "deleting orphaned ELB $lb"; aws elb delete-load-balancer --region "$AWS_REGION" --load-balancer-name "$lb" || true
  fi
done
# Available (detached) EBS volumes tagged for the cluster
for vol in $(aws ec2 describe-volumes --region "$AWS_REGION" \
      --filters "Name=tag-key,Values=kubernetes.io/cluster/${CLUSTER}" "Name=status,Values=available" \
      --query 'Volumes[].VolumeId' --output text 2>/dev/null); do
  echo "deleting orphaned EBS volume $vol"; aws ec2 delete-volume --region "$AWS_REGION" --volume-id "$vol" || true
done

# ---- optional purges --------------------------------------------------------
if [ "$PURGE_SQS" = "true" ]; then
  log "purging SQS queues"
  for q in code-submission-processing-queue code-submission-completed-queue; do
    url="$(aws sqs get-queue-url --queue-name "$q" --region "$AWS_REGION" --query QueueUrl --output text 2>/dev/null)" \
      && [ -n "$url" ] && aws sqs delete-queue --queue-url "$url" --region "$AWS_REGION" && echo "deleted $q"
  done
fi
if [ "$PURGE_RDS" = "true" ]; then
  log "purging RDS instance '$RDS_INSTANCE_ID' (no final snapshot)"
  if aws rds describe-db-instances --db-instance-identifier "$RDS_INSTANCE_ID" --region "$AWS_REGION" >/dev/null 2>&1; then
    aws rds modify-db-instance --db-instance-identifier "$RDS_INSTANCE_ID" --region "$AWS_REGION" \
      --no-deletion-protection --apply-immediately >/dev/null 2>&1 || true
    aws rds delete-db-instance --db-instance-identifier "$RDS_INSTANCE_ID" --region "$AWS_REGION" \
      --skip-final-snapshot --delete-automated-backups >/dev/null && echo "delete initiated for $RDS_INSTANCE_ID (runs ~5-10 min in background)"
  else
    echo "RDS instance '$RDS_INSTANCE_ID' not found — skipping"
  fi
fi
if [ "$PURGE_ECR" = "true" ]; then
  log "purging ECR repos"
  for r in $(aws ecr describe-repositories --region "$AWS_REGION" \
        --query "repositories[?starts_with(repositoryName,'coderuntime/') || starts_with(repositoryName,'code-runtime-')].repositoryName" \
        --output text 2>/dev/null); do
    aws ecr delete-repository --repository-name "$r" --region "$AWS_REGION" --force >/dev/null && echo "deleted repo $r"
  done
fi

echo
log "DONE — cluster torn down."
KEPT="EKS: gone"
[ "$PURGE_RDS" = true ] || KEPT="$KEPT | RDS: kept"; [ "$PURGE_SQS" = true ] || KEPT="$KEPT | SQS: kept"; [ "$PURGE_ECR" = true ] || KEPT="$KEPT | ECR: kept"
echo "$KEPT"
echo "Rebuild any time:  deployments/kubernetes/eks/setup.sh"
