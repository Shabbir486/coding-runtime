# Deploying Code Runtime on AWS (EKS) — the complete path

A end-to-end, opinionated runbook for running Code Runtime on AWS Kubernetes at
scale, with the reasoning behind every component. Read top to bottom the first
time; later use it as a checklist.

> **Mental model.** Code Runtime is three stateless Go services
> (`api-gateway`, `queue-manager`, `worker`) around three managed AWS data
> services (a **SQS** job queue, an **Aurora/RDS** MySQL database, an
> **ElastiCache** Redis cache). The api accepts submissions and drops a job on
> SQS; the worker pulls jobs and runs each one in a throwaway **Docker sandbox**;
> results land back in MySQL/Redis and a webhook fires. Everything scales around
> that one fact: **one submission = one sandbox container**.

---

## 0. Target architecture & why

```
                          Route53 → ACM/cert-manager
                                   │
                          ┌────────▼─────────┐
   internet ──────────────▶  ALB / Ingress   │   (public subnets)
                          └────────┬─────────┘
                                   │
   ┌───────────────────────────────────────── EKS (private subnets) ──────────┐
   │  system nodegroup (managed)           Karpenter worker nodes (spot)       │
   │   ├ api-gateway  (HPA cpu)             ├ worker pods (KEDA scales on SQS)  │
   │   ├ queue-manager                      │   └ each runs Docker sandboxes   │
   │   ├ KEDA, metrics-server, ingress      └ tainted: workload=code-execution │
   └───────────┬───────────────┬───────────────────────┬──────────────────────┘
               │ IRSA          │ IRSA                   │ IRSA
        ┌──────▼─────┐   ┌─────▼──────┐          ┌──────▼───────┐
        │  Amazon    │   │ RDS Proxy  │          │ ElastiCache  │
        │    SQS     │   │   ─▶ Aurora│          │   Redis      │
        └────────────┘   │   MySQL    │          └──────────────┘
                         └────────────┘
```

**Why each piece exists:**

| Component | Why it's here (not an alternative) |
|-----------|-----------------------------------|
| **EKS** | Managed K8s control plane. The app is already containerized with HPAs/PDBs; EKS gives autoscaling + AZ spread without running masters yourself. |
| **SQS** | Decouples accept-rate from execute-rate. A 10k-submission exam-start spike lands on SQS in seconds; workers drain it steadily. Scales effectively infinitely; no brokers to run. |
| **Aurora/RDS MySQL** | Source of truth for submissions/results/languages. Managed backups, failover, read replicas. |
| **RDS Proxy** | **Mandatory at scale.** Hundreds of worker pods × a few connections each would blow past MySQL `max_connections`. RDS Proxy pools and multiplexes them. |
| **ElastiCache Redis** | Result cache + the rate limiter's shared store. Sub-ms reads keep the DB off the hot path. |
| **IRSA** | Pods assume IAM roles via the OIDC provider — **no static AWS keys** in the cluster. |
| **KEDA** | Scales workers on **SQS queue depth** — the only correct signal (see §9 / AUTOSCALING.md). |
| **Karpenter** | Turns Pending worker pods into right-sized **spot** nodes in seconds, removes them when idle. This is what makes "millions" affordable. |
| **ALB + cert-manager** | TLS termination + public ingress for the api. |

---

## 1. Prerequisites

```bash
# CLIs
aws --version          # v2
eksctl version         # >= 0.190
kubectl version --client
helm version
# Auth: an AWS profile/role with admin-ish rights for the initial build.
aws sts get-caller-identity
```

Decide **one region** up front. Your data services should live in the same
region as the cluster — cross-region DB calls add latency to *every request*.
This guide uses `ap-southeast-2`; change everywhere if needed.

```bash
export AWS_REGION=ap-southeast-2
export CLUSTER=coderuntime
export ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
```

---

## 2. Networking (VPC) — why first

EKS, RDS, and ElastiCache must share a VPC so the cluster can reach the database
on a **private** endpoint (never expose a DB publicly). `eksctl` will create a
VPC with public + private subnets across 3 AZs by default — fine for most. If
you have an existing VPC (e.g. where Aurora already lives), reuse it so the
cluster can reach the DB without peering.

**Why private subnets for nodes:** worker nodes run untrusted user code; they
should have **no inbound** from the internet and only egress via NAT.

---

## 3. Container images → ECR

EKS nodes can't pull your locally-built `coding-runtime-*` images. Push them to
ECR (or GHCR) and reference that registry.

```bash
for svc in api worker queue-manager; do
  aws ecr create-repository --repository-name coderuntime/$svc --region $AWS_REGION || true
done
aws ecr get-login-password --region $AWS_REGION \
  | docker login --username AWS --password-stdin $ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com

# build for the cluster's arch (amd64 nodes here) and push
docker buildx build --platform linux/amd64 -f docker/Dockerfile.api \
  -t $ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/coderuntime/api:v1 --push .
docker buildx build --platform linux/amd64 -f docker/Dockerfile.worker \
  -t $ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/coderuntime/worker:v1 --push .
docker buildx build --platform linux/amd64 -f docker/Dockerfile.queue-manager \
  -t $ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/coderuntime/queue-manager:v1 --push .
```

Then point the kustomization at ECR:
```bash
cd deployments/kubernetes
kustomize edit set image \
  ghcr.io/coderuntime/api-gateway=$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/coderuntime/api:v1 \
  ghcr.io/coderuntime/worker=$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/coderuntime/worker:v1 \
  ghcr.io/coderuntime/queue-manager=$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/coderuntime/queue-manager:v1
```
**Why a real tag (`v1`) not `latest`:** immutable tags make rollbacks
deterministic and let the kubelet cache images.

> **The worker runs Docker (DooD).** On EKS the worker runs sandboxes via the
> **node's** container runtime. AL2023 nodes use containerd, not Docker, so the
> worker's `unix:///var/run/docker.sock` won't exist. Two production options:
> **(a)** install Docker (dockerd) on the Karpenter worker nodes via EC2NodeClass
> `userData` and keep the hostPath socket mount, or **(b)** point the sandbox at
> containerd. Option (a) is the smallest change from what runs today. Bake the 39
> language images into the node AMI (or a `pull-images` DaemonSet) so first-run
> pulls don't dominate latency.

---

## 4. Data tier (provision BEFORE the app needs it)

### 4a. Aurora/RDS MySQL
```bash
# (console or CLI) MySQL 8 / Aurora-MySQL, MULTI-AZ, in the cluster's VPC,
# Publicly accessible = NO, storage encrypted, initial DB name 'coderuntime'.
```
**Why Multi-AZ:** automatic failover; a single-AZ DB is a SPOF for the whole
platform. **Why initial DB name:** the app auto-migrates the schema + seeds 39
languages on first api-gateway boot (`migrate_on_start`) — no manual SQL.

### 4b. RDS Proxy (do not skip for scale)
Create an RDS Proxy targeting the Aurora cluster, in the same private subnets.
Point the app's `DATABASE_HOST` at the **proxy endpoint**, not the DB endpoint.
**Why:** the worker pool is bounded to 5 connections/pod (`worker-config`), but
at 200 pods that's still 1,000 connections — more than MySQL allows. RDS Proxy
multiplexes them onto a small pool. Without it, scaling workers crash-loops the
DB with `too many connections` (we reproduced this locally).

### 4c. ElastiCache Redis
Create a Redis (or Valkey) replication group / cluster-mode in the VPC, TLS on.
**Why cluster mode at scale:** the cache + rate-limiter is a hot single instance
otherwise; sharding spreads load and memory.

Capture the endpoints — they go into the app secrets in §8.

---

## 5. SQS queues

You already have `qa-code-submission-processing-queue` (jobs) and
`...-completed-queue` (batch starts) in `us-east-2`. For a new region create:
- **jobs queue** (standard) + a **dead-letter queue** with a redrive policy
  (maxReceiveCount ~5) — poison jobs land in the DLQ instead of looping forever.
- optional **priority queue** (polled first).
- **Visibility timeout > max job duration** (e.g. 330s for a 120s job) so a job
  isn't redelivered while still running.

**Why SQS over in-cluster NATS here:** zero brokers to operate, native DLQ, and
KEDA scales directly off its depth metric.

---

## 6. EKS cluster + IRSA

```bash
cd deployments/kubernetes/eks
# cluster.yaml: OIDC enabled, IRSA roles (scoped SQS policies) for
# api-gateway / worker / queue-manager / keda-operator, + a 'system' nodegroup.
eksctl create cluster -f cluster.yaml      # ~15-20 min
aws eks update-kubeconfig --name $CLUSTER --region $AWS_REGION
kubectl get nodes
```
**What IRSA does:** `eksctl` creates an OIDC identity provider for the cluster
and an IAM role per ServiceAccount; pods using that SA get temporary credentials
via a projected token. **Result: no AWS keys anywhere in the cluster.** The
scoped policies in `cluster.yaml` grant exactly the SQS actions each service
needs (publish / consume / read-depth) — least privilege.

---

## 7. Cluster add-ons

### 7a. Karpenter (worker node autoscaling)
Run the version-pinned Karpenter quickstart (it creates `KarpenterNodeRole` +
the controller IRSA via CloudFormation), then:
```bash
helm upgrade --install karpenter oci://public.ecr.aws/karpenter/karpenter \
  --version 1.0.6 -n kube-system --set "settings.clusterName=$CLUSTER"
# tag the cluster subnets + node security group so Karpenter can discover them:
#   karpenter.sh/discovery = coderuntime
kubectl apply -f karpenter-nodepool.yaml
```
**Why Karpenter not the Cluster Autoscaler:** it provisions *right-sized* nodes
directly from Pending pod requirements (no pre-defined ASGs), uses **spot** with
on-demand fallback, and consolidates aggressively — far cheaper for spiky,
bursty execution load.

### 7b. KEDA (worker scaling on SQS depth)
```bash
helm repo add kedacore https://kedacore.github.io/charts && helm repo update
helm install keda kedacore/keda -n keda --create-namespace
```
**Why:** the worker offloads all CPU to sibling sandbox containers, so its pod
CPU stays near-idle — a CPU HPA never fires. KEDA scales on
`ApproximateNumberOfMessagesVisible`, the real demand signal. (Full reasoning:
[../AUTOSCALING.md](../AUTOSCALING.md).)

### 7c. metrics-server, ingress, TLS
```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
# AWS Load Balancer Controller (provisions ALBs from Ingress) — install per AWS docs.
# cert-manager (Let's Encrypt) OR an ACM cert on the ALB for TLS.
```
**Why metrics-server:** the api HPA (CPU-bound, unlike the worker) needs it.

---

## 8. App config & secrets (Aurora + SQS, keyless)

```bash
kubectl apply -f ../namespace.yaml

# Point config at your real endpoints (edit ../configmap.yaml common-config):
#   CODERUNTIME_SQS_REGION / *_QUEUE_URL  → your queues
#   CODERUNTIME_DATABASE_SSL_MODE: "skip-verify" (or "true" with the RDS CA)
# DB host = the RDS PROXY endpoint.

# Secrets: DB + JWT only — NO aws-credentials (IRSA supplies SQS creds).
kubectl -n coderuntime create secret generic api-secrets \
  --from-literal=DATABASE_HOST=<rds-proxy-endpoint> \
  --from-literal=DATABASE_USER=coderuntime \
  --from-literal=DATABASE_PASSWORD=<pw> \
  --from-literal=REDIS_HOST=<elasticache-endpoint> \
  --from-literal=REDIS_PASSWORD=<pw-or-empty> \
  --from-literal=JWT_SECRET=$(openssl rand -base64 64 | tr -d '\n')
kubectl -n coderuntime create secret generic worker-secrets \
  --from-literal=DATABASE_HOST=<rds-proxy-endpoint> \
  --from-literal=DATABASE_USER=coderuntime \
  --from-literal=DATABASE_PASSWORD=<pw> \
  --from-literal=REDIS_HOST=<elasticache-endpoint> \
  --from-literal=REDIS_PASSWORD=<pw-or-empty>
```
The in-cluster MySQL StatefulSet is already excluded from `../kustomization.yaml`
(you're on Aurora), and NATS is excluded (you're on SQS).

---

## 9. Deploy the app + autoscaling

```bash
# app: api (HPA), queue-manager, worker, redis-NOT-needed (using ElastiCache)
kubectl apply -k ..

# IRSA finalize: remove the static-key env so the SDK uses the role.
# (Base deployments inject AWS_* from a secret; with IRSA they MUST be absent.)
# Best done as an eks kustomize overlay; quick patch:
for d in api-gateway worker queue-manager; do
  kubectl -n coderuntime get deploy $d -o json \
   | jq '(.spec.template.spec.containers[0].env) |= map(select(.name|test("^AWS_")|not))' \
   | kubectl apply -f -
done

# worker autoscaling on SQS (keyless via IRSA)
kubectl apply -f keda-triggerauth-irsa.yaml
# edit ../keda-worker-scaledobject.yaml: drop secretTargetRef auth, add
# `identityOwner: operator` to each trigger, then:
kubectl apply -f ../keda-worker-scaledobject.yaml
kubectl -n coderuntime patch deployment worker --patch-file worker-eks-patch.yaml
```

---

## 10. DNS + TLS

Point a Route53 record (e.g. `api.yourdomain.com`) at the ALB the Ingress
created. Use an ACM cert on the ALB (or cert-manager). The base
[../api-deployment.yaml](../api-deployment.yaml) Ingress has the host + TLS
annotations — update the host and issuer.

---

## 11. Verify

```bash
kubectl get pods -n coderuntime
kubectl get scaledobject,hpa -n coderuntime
# api health
curl -s https://api.yourdomain.com/health      # {"status":"ok","services":{"mysql":"ok","redis":"ok"}}
# scaling: push load, watch workers + nodes grow
kubectl get pods -n coderuntime -l app.kubernetes.io/name=worker -w
kubectl get nodes -l node-type=compute-optimized -w
```

---

## 12. Capacity & cost knobs

```
per worker pod          = WORKER_COUNT(8) × WORKER_CONCURRENCY(8) = 64 sandboxes
throughput              = (worker_pods × 64) / avg_job_seconds
worker_pods             = ceil(SQS_backlog / KEDA queueLength)   [KEDA]
nodes                   = auto, until NodePool cpu limit (2000 vCPU)  [Karpenter]
```
- **Scale wider, not hotter:** raise KEDA `maxReplicaCount` + Karpenter `limits.cpu`.
- **Cost control:** spot instances (NodePool), consolidation, and the vCPU cap.
- **DB is the silent ceiling:** keep the worker pool small (5) + RDS Proxy; add
  Aurora read replicas if status polling is heavy.
- **Prefer batches + webhook over polling** to collapse read load.

---

## 13. Security checklist
- Private subnets for nodes; DB/cache not publicly accessible.
- IRSA (no static keys); least-privilege SQS policies per service.
- Worker nodes tainted + isolated; sandbox containers run with `network=none`,
  dropped caps, read-only rootfs, non-root (already set in the manifests).
- PodSecurity: `restricted` for api/queue; `privileged` only where the worker
  needs the host socket — ideally isolate workers in their own namespace.
- Secrets via AWS Secrets Manager / External Secrets Operator instead of plain
  K8s Secrets for production rotation.
- Rotate/scope any previously-used static AWS keys; IRSA makes them unnecessary.

---

## 14. Observability & day-2
- Prometheus scrapes the `/metrics` on each service (annotations already set);
  Grafana dashboards in `deployments/monitoring/`.
- Watch: SQS `ApproximateNumberOfMessagesVisible` + age of oldest message,
  worker `in_flight_jobs`, RDS Proxy connection borrow latency, node spot
  interruptions, HPA/ScaledObject events.
- Alarms: queue age rising (workers not keeping up), DLQ depth > 0, DB
  connections near limit, p99 job duration.
- Rollouts: immutable image tags + `kubectl rollout undo` for fast rollback;
  PodDisruptionBudgets already protect availability during node consolidation.

---

### TL;DR ordering
VPC → ECR images → Aurora + RDS Proxy + ElastiCache → SQS (+DLQ) → `eksctl`
(IRSA) → Karpenter → KEDA → add-ons → config/secrets → `kubectl apply -k ..` →
IRSA env-strip → KEDA ScaledObject + worker patch → DNS/TLS → verify → tune
KEDA/Karpenter limits.
