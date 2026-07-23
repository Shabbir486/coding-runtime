# Production EKS deployment (IRSA + Karpenter + KEDA)

This is the real "handle millions" target: an EKS cluster in your Aurora VPC,
keyless AWS via IRSA, KEDA scaling workers on SQS backlog, and Karpenter adding
nodes on demand. No `.env` AWS keys, no local kind workarounds.

```
SQS backlog ──KEDA──▶ scale worker pods ──Pending──▶ Karpenter adds nodes
   api/queue/worker ──IRSA──▶ SQS + Aurora (private, in-VPC)   RDS Proxy ─▶ Aurora
```

## Files
| File | Purpose |
|------|---------|
| `cluster.yaml` | eksctl config: OIDC, IRSA roles (SQS) for api/worker/queue-manager/keda, `system` nodegroup |
| `karpenter-nodepool.yaml` | NodePool + EC2NodeClass for tainted, spot-first worker compute |
| `keda-triggerauth-irsa.yaml` | keyless KEDA SQS auth (replaces the secret-based one) |
| `worker-eks-patch.yaml` | worker toleration + nodeSelector for Karpenter worker nodes |

## Sequence

```bash
cd deployments/kubernetes/eks
REGION=ap-southeast-2; CLUSTER=coderuntime

# 1. Cluster + IRSA (creates IAM roles bound to the app/keda ServiceAccounts)
eksctl create cluster -f cluster.yaml

# 2. Karpenter — follow the version-pinned quickstart for the IAM/CFN bits
#    (it creates KarpenterNodeRole-coderuntime + controller IRSA), then:
helm registry login public.ecr.aws
helm upgrade --install karpenter oci://public.ecr.aws/karpenter/karpenter \
  --version 1.0.6 -n kube-system \
  --set "settings.clusterName=${CLUSTER}"
kubectl apply -f karpenter-nodepool.yaml      # tag subnets/SGs: karpenter.sh/discovery=coderuntime

# 3. KEDA (its keda-operator SA already has the IRSA role from cluster.yaml)
helm repo add kedacore https://kedacore.github.io/charts && helm repo update
helm install keda kedacore/keda -n keda --create-namespace

# 4. metrics-server (api CPU HPA) + ingress-nginx + cert-manager as needed
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

# 5. App config/secrets — Aurora + SQS, but NO aws-credentials (IRSA provides them)
kubectl apply -f ../namespace.yaml
#   secrets: DB + JWT only (skip aws-credentials):
kubectl -n coderuntime create secret generic api-secrets \
  --from-literal=DATABASE_HOST=<aurora-endpoint> \
  --from-literal=DATABASE_USER=coderuntime \
  --from-literal=DATABASE_PASSWORD=<pw> \
  --from-literal=REDIS_HOST=<elasticache-endpoint> \
  --from-literal=REDIS_PASSWORD= \
  --from-literal=JWT_SECRET=<secret>
#   (worker-secrets likewise, minus JWT)

# 6. App (base manifests — Aurora + SQS, in-cluster MySQL already excluded)
kubectl apply -k ..

# 7. IRSA finalization — remove the static-key env so the SDK uses the role.
#    The base deployments inject AWS_ACCESS_KEY_ID/SECRET from a secret; with
#    IRSA those MUST be absent or they take precedence over the role.
for d in api-gateway worker queue-manager; do
  kubectl -n coderuntime patch deployment $d --type=json -p='[
    {"op":"remove","path":"/spec/template/spec/containers/0/env/<AWS_ACCESS_KEY_ID-index>"},
    {"op":"remove","path":"/spec/template/spec/containers/0/env/<AWS_SECRET_ACCESS_KEY-index>"}]'
done   # easier: maintain an EKS kustomize overlay that omits those env entries

# 8. Worker scaling via KEDA on SQS (keyless)
kubectl apply -f keda-triggerauth-irsa.yaml          # IRSA auth
kubectl apply -f ../keda-worker-scaledobject.yaml    # add identityOwner: operator to triggers
kubectl -n coderuntime patch deployment worker --patch-file worker-eks-patch.yaml
```

## Data tier (do not skip for millions)
- **RDS Proxy** in front of Aurora — with the worker pool already bounded to 5
  conns (`worker-config`), this keeps total connections sane at hundreds of pods.
- **ElastiCache Redis cluster mode** for the cache + rate limiter.
- Keep SQS visibility timeout > max job duration; size the DLQ.

## Region
`cluster.yaml` defaults to **ap-southeast-2** (Aurora's region) so per-request DB
latency is local; SQS is cross-region in us-east-2 (async, fine). For best
results, co-locate SQS + EKS + Aurora in one region and update the queue ARNs +
URLs accordingly.

## Why this scales to millions
KEDA sets worker count from the live SQS backlog; Karpenter turns the resulting
Pending pods into spot/on-demand nodes within seconds (up to the NodePool's
2000-vCPU cap); RDS Proxy + ElastiCache cluster keep the data tier off the
critical path. The two ceilings we hit locally — no second node, no connection
pooler — are exactly what Karpenter and RDS Proxy remove. See
[../AUTOSCALING.md](../AUTOSCALING.md) for the findings that motivated this.
