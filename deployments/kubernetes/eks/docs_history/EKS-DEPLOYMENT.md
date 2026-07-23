# Code Runtime — End‑to‑End EKS Deployment Guide

This is the **complete, reproducible** record of how Code Runtime was deployed to
Amazon EKS — from an empty AWS account to a publicly reachable, autoscaling code
execution platform running all 40 languages. Every step, value, security group,
load balancer, and the bugs hit along the way are documented in order.

> Companion files in this directory:
> - `cluster.yaml` — eksctl cluster definition (VPC reuse, IRSA, nodegroup, addons)
> - `keda-scaledobject-eks.yaml`, `keda-triggerauth-irsa.yaml` — worker autoscaling
> - `cluster-autoscaler.yaml` — node autoscaling
> - `worker-dind-patch.yaml` — worker + Docker‑in‑Docker sidecar
> - `sqs-iam-policy.json` — SQS permissions reference
> - `aws.readme` — the raw running command log (Phases 0–8)

---

## 0. Target architecture

```
                       Internet
                          │  HTTP :80
                ┌─────────▼──────────┐
                │  Classic ELB        │  (Service type=LoadBalancer)
                │  api-gateway-lb     │
                └─────────┬──────────┘
                          │
  ┌───────────────────────────────────────────── EKS cluster "coderuntime" (ap-southeast-2, v1.32)
  │                       │
  │   ┌──────────────┐    │     ┌────────────────┐      ┌──────────────┐
  │   │ api-gateway  │────┼────▶│  Amazon SQS     │◀─────│  worker       │
  │   │ (2, HPA→20)  │  publish │  jobs queue     │ consume (KEDA 1→20)│
  │   └──────┬───────┘    │     │  + completed Q  │      │  + dind sidecar│
  │          │            │     └────────────────┘      │  + sandbox     │
  │          │ JWT/apikey │                              └──────┬────────┘
  │   ┌──────▼───────┐    │     ┌────────────────┐              │ runs each job in
  │   │ queue-manager│    │     │  redis (1, EBS) │              │ an ephemeral
  │   │ (2)          │    │     └────────────────┘              │ code-runtime-* container
  │   └──────────────┘    │
  │   nodes: managed nodegroup m7i-flex.large (cluster-autoscaler 2→12)
  └──────────────────────────────────────────────────────────────────
                          │                              │
                ┌─────────▼──────────┐         ┌─────────▼──────────┐
                │ RDS MySQL           │         │  Amazon ECR         │
                │ coderuntime-prod    │         │ service imgs + 36   │
                │ (private, same VPC) │         │ code-runtime-* imgs │
                └────────────────────┘         └────────────────────┘
```

**Key design choices**
- **Keyless AWS** via IRSA (IAM Roles for Service Accounts) — no static AWS keys in the cluster.
- **Queue‑driven** execution (SQS) decouples API ingestion from sandbox execution and is the
  signal KEDA uses to autoscale workers.
- **Worker offloads CPU to a sibling sandbox container**, so workers scale on **SQS backlog**,
  not CPU.
- **Docker‑in‑Docker (dind) sidecar**: AL2023 EKS nodes use containerd and expose no
  `/var/run/docker.sock`, so each worker pod ships its own Docker daemon.

### Account / region facts (this deployment)
| Item | Value |
|---|---|
| AWS account | `768054003064` |
| Region | `ap-southeast-2` (Sydney) — co‑located with RDS |
| Cluster | `coderuntime`, EKS `1.32` |
| VPC (reused) | `vpc-0c19448c00a89ccb8` (default VPC, public subnets) |
| RDS | `coderuntime-prod.cziyoeg0oioy.ap-southeast-2.rds.amazonaws.com` (MySQL, db `coderuntime`) |
| SQS jobs queue | `code-submission-processing-queue` |
| SQS completed queue | `code-submission-completed-queue` |
| Public endpoint | `http://aa6f9d7b568b4425f84ea155a296db33-71808099.ap-southeast-2.elb.amazonaws.com` |

---

## 1. Prerequisites (local tooling)

| Tool | Version | Purpose |
|---|---|---|
| `awscli` | v2 | AWS API access |
| `eksctl` | 0.190+ | Cluster + IRSA provisioning |
| `kubectl` | 1.30+ | Cluster management |
| `helm` | 3.13+ | KEDA install |
| `docker` (+ buildx) | 27+ | Build/push images (amd64) |
| `python3` | 3.10+ | Test + load scripts |

```bash
aws sts get-caller-identity          # confirm account + identity
aws configure get region             # should be ap-southeast-2
```

### 1a. IAM hardening (do this first)
This deployment was initially run as the AWS **root** user — never operate as root.
Create an admin IAM user/role and delete root access keys before anything else:
```bash
aws iam create-user --user-name coderuntime-admin
aws iam attach-user-policy --user-name coderuntime-admin \
  --policy-arn arn:aws:iam::aws:policy/AdministratorAccess
aws iam create-access-key --user-name coderuntime-admin     # configure these locally
# then, in the console: delete the ROOT account's access keys.
```

---

## 2. Pre‑existing infrastructure to discover

The cluster **reuses** the VPC that already hosts RDS so pods reach the private DB
with no VPC peering. Discover the IDs you'll plug into `cluster.yaml`:

```bash
# default VPC + its public subnets (one per AZ)
aws ec2 describe-vpcs --filters Name=isDefault,Values=true \
  --query 'Vpcs[0].VpcId' --output text
aws ec2 describe-subnets --filters Name=vpc-id,Values=<vpc-id> \
  --query 'Subnets[].{id:SubnetId,az:AvailabilityZone}' --output table

# RDS endpoint + the security group it lives in
aws rds describe-db-instances --db-instance-identifier coderuntime-prod \
  --query 'DBInstances[0].{ep:Endpoint.Address,sg:VpcSecurityGroups,subnet:DBSubnetGroup.VpcId}'

# SQS queue URLs
aws sqs get-queue-url --queue-name code-submission-processing-queue
aws sqs get-queue-url --queue-name code-submission-completed-queue
```

---

## 3. Security groups & networking

EKS auto‑creates security groups, but two rules must be added by hand so pods can
reach RDS and the public can reach the API.

### 3a. Allow EKS nodes → RDS (MySQL 3306)
The RDS instance's SG must allow inbound 3306 from the EKS **node/cluster** security
group (same VPC, so no peering needed):
```bash
# find the cluster security group eksctl created
EKS_SG=$(aws eks describe-cluster --name coderuntime \
  --query 'cluster.resourcesVpcConfig.clusterSecurityGroupId' --output text)
RDS_SG=$(aws rds describe-db-instances --db-instance-identifier coderuntime-prod \
  --query 'DBInstances[0].VpcSecurityGroups[0].VpcSecurityGroupId' --output text)

aws ec2 authorize-security-group-ingress --group-id "$RDS_SG" \
  --protocol tcp --port 3306 --source-group "$EKS_SG"
```

### 3b. Public ingress (the ELB)
The Classic ELB created by the `LoadBalancer` Service (Section 14) manages its own
SG automatically, opening `:80` to `0.0.0.0/0` and forwarding to the node port.
No manual rule needed for the Classic ELB path. (For an ALB via the AWS Load
Balancer Controller you would instead manage target‑group SGs — see Section 14c.)

> **Hardening note:** the default VPC has only **public** subnets, so nodes get
> public IPs. For production, use a VPC with private subnets + a NAT gateway and set
> `privateNetworking: true` in the nodegroup.

---

## 4. ECR — container registries

Two classes of images live in ECR (region `ap-southeast-2`, account `768054003064`):

1. **Service images** — `coderuntime/api`, `coderuntime/worker`, `coderuntime/queue-manager`
2. **Runtime images** — one repo **per language**, `code-runtime-<lang>` (36 total)

```bash
REGION=ap-southeast-2; ACCOUNT=768054003064
REGISTRY=$ACCOUNT.dkr.ecr.$REGION.amazonaws.com
aws ecr get-login-password --region $REGION | docker login --username AWS --password-stdin $REGISTRY

# service images (cross-compiled to amd64 to match the nodes)
for svc in api worker queue-manager; do
  aws ecr create-repository --repository-name coderuntime/$svc --region $REGION 2>/dev/null || true
  docker buildx build --platform linux/amd64 -f docker/Dockerfile.$svc \
    -t $REGISTRY/coderuntime/$svc:v1 --push .
done
```

The Dockerfiles use `--platform=$BUILDPLATFORM` + `ARG TARGETARCH` / `GOARCH=${TARGETARCH}`
so they cross‑compile cleanly from an arm64 Mac to linux/amd64.

Runtime images are built later in **Section 13** (they depend on the registry‑auth wiring).

---

## 5. Create the cluster (eksctl)

The whole cluster — VPC reuse, OIDC/IRSA, the four service accounts with scoped SQS
policies, the managed nodegroup, and the core addons — is declared in `cluster.yaml`:

```bash
eksctl create cluster -f deployments/kubernetes/eks/cluster.yaml
```

What `cluster.yaml` provisions:
- **`iam.withOIDC: true`** — the OIDC provider that makes IRSA possible.
- **Service accounts** (keyless): `api-gateway` (SQS SendMessage), `worker`
  (Receive/Delete/ChangeVisibility), `queue-manager` (DLQ drain), `keda-operator`
  (GetQueueAttributes). Each is bound to an IAM role via OIDC — no access keys.
- **Managed nodegroup `system`**: `m7i-flex.large` (2 vCPU / 8 GB, **Free‑Tier‑eligible**),
  `desired 2 / min 2 / max 4` (raised to 12 in Section 15), 50 GB volumes, public subnets.
- **Addons**: `vpc-cni`, `coredns`, `kube-proxy`, **`aws-ebs-csi-driver`** (needed for the
  Redis PVC).

> **Why m7i-flex.large?** The account was restricted to Free‑Tier instance types.
> The first attempt with a non‑free type failed at the EC2 ASG stage
> (`not eligible for Free Tier`). Diagnosed by walking CloudFormation events →
> nodegroup health → ASG scaling activities (see `aws.readme` Phase 3).

Verify:
```bash
aws eks update-kubeconfig --name coderuntime --region ap-southeast-2
kubectl get nodes
kubectl get sa -n coderuntime          # IRSA-annotated service accounts
eksctl get addon --cluster coderuntime # ebs-csi-driver ACTIVE
```

If the EBS CSI driver is missing (Redis PVC stuck `Pending`):
```bash
eksctl create iamserviceaccount --cluster coderuntime --region ap-southeast-2 \
  --namespace kube-system --name ebs-csi-controller-sa \
  --attach-policy-arn arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy \
  --override-existing-serviceaccounts --approve
eksctl create addon --cluster coderuntime --name aws-ebs-csi-driver --force
```

---

## 6. Namespace & PodSecurity

The worker's dind sidecar is **privileged**, which the default `restricted`
PodSecurity profile blocks. Label the namespace `privileged`:
```bash
kubectl create namespace coderuntime
kubectl label namespace coderuntime \
  pod-security.kubernetes.io/enforce=privileged --overwrite
```

A `fast-ssd` / `gp3` StorageClass is needed for Redis:
```bash
kubectl apply -f - <<'YAML'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: { name: gp3 }
provisioner: ebs.csi.aws.com
parameters: { type: gp3 }
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
YAML
```

---

## 7. ConfigMaps & Secrets

Config is read by the Go binaries through Viper with the **`CODERUNTIME_`** prefix
(`docker.registry` → `CODERUNTIME_DOCKER_REGISTRY`, etc.). Non‑secret config lives in
ConfigMaps; credentials live in Secrets.

### 7a. ConfigMaps
**`common-config`** (all services):
```
CODERUNTIME_QUEUE_PROVIDER          = sqs
CODERUNTIME_SQS_REGION              = ap-southeast-2
CODERUNTIME_SQS_JOBS_QUEUE_URL      = https://sqs.ap-southeast-2.amazonaws.com/768054003064/code-submission-processing-queue
CODERUNTIME_SQS_START_QUEUE_URL     = https://sqs.ap-southeast-2.amazonaws.com/768054003064/code-submission-completed-queue
CODERUNTIME_SQS_VISIBILITY_TIMEOUT  = 330         # > worker job timeout
CODERUNTIME_SQS_WAIT_TIME_SECONDS   = 20          # long polling
CODERUNTIME_SQS_MAX_CONCURRENCY     = 8
CODERUNTIME_DATABASE_NAME           = coderuntime
CODERUNTIME_DATABASE_PORT           = 3306
CODERUNTIME_DATABASE_SSL_MODE       = skip-verify # RDS TLS w/o cert pinning
CODERUNTIME_REDIS_PORT              = 6379
CODERUNTIME_LOG_FORMAT/LEVEL        = json / info
```
**`api-config`** (api‑gateway): server `:8002`, CORS, `RATE_LIMIT_ENABLED=false`,
DB pool `MAX_OPEN=50`, `JWT_ACCESS_TOKEN_EXP=24h`.
**`worker-config`** (worker): `WORKER_CONCURRENCY=8`, `WORKER_COUNT=8`,
DB pool `MAX_OPEN=5 / IDLE=2` (workers are many, keep per‑pod DB conns low),
`DOCKER_NETWORK_MODE=none`, `WORKER_TMP_DIR=/sandbox-host` (see Section 12c).

### 7b. Secrets
```bash
kubectl -n coderuntime create secret generic api-secrets \
  --from-literal=DATABASE_HOST=coderuntime-prod.cziyoeg0oioy.ap-southeast-2.rds.amazonaws.com \
  --from-literal=DATABASE_USER=coderuntime \
  --from-literal=DATABASE_PASSWORD='********' \
  --from-literal=JWT_SECRET='********' \
  --from-literal=REDIS_HOST=redis \
  --from-literal=REDIS_PASSWORD='********'
# worker-secrets: same DB+Redis keys (no JWT). redis-secrets: REDIS_PASSWORD.
```
`ecr-registry-auth` (for runtime image pulls) is created in **Section 12d**.

---

## 8. Database preparation (RDS)

The app auto‑migrates on start, but the database itself must exist and a user must be
seeded. Run a throwaway mysql pod inside the cluster (it can reach the private RDS):
```bash
kubectl -n coderuntime run mysql-cli --rm -it --image=mysql:8 --restart=Never -- \
  mysql -h coderuntime-prod.cziyoeg0oioy.ap-southeast-2.rds.amazonaws.com \
        -u coderuntime -p
# > CREATE DATABASE IF NOT EXISTS coderuntime;
```
> Hit `Error 1049 Unknown database 'coderuntime'` on first boot → created it here.

Seed an application user (so you can mint API keys / JWTs):
```sql
INSERT INTO users (email, password_hash, name, is_active, is_admin, created_at, updated_at)
VALUES ('dev@coderuntime.io', '<bcrypt-hash-of-Admin123!>', 'dev', 1, 1, NOW(), NOW());
```
Auth model: `POST /auth/token` (email+password → JWT), `Authorization: Bearer <jwt>`,
or an API key via the **`X-Auth-Token`** header (sha256 looked up in `api_keys`).

---

## 9. Deploy Redis + API + queue‑manager

```bash
kubectl -n coderuntime apply -f deployments/kubernetes/   # base manifests/kustomize
kubectl -n coderuntime rollout status deploy/api-gateway
kubectl -n coderuntime rollout status deploy/queue-manager
kubectl -n coderuntime get statefulset redis
```
Notes from the field:
- Redis crashed on an **empty `requirepass`** → always set `REDIS_PASSWORD`.
- `imagePullSecrets` were **removed** from manifests — ECR pulls use the node IAM role.
- Routes are served at **root** (`/submissions`, `/languages`, `/auth/token`, `/health`).
  The `/code-runtime` prefix seen elsewhere is added by an external ingress/gateway.

Smoke test via port‑forward:
```bash
kubectl -n coderuntime port-forward deploy/api-gateway 18002:8002 &
curl -s localhost:18002/health     # {"status":"ok","services":{"mysql":"ok","redis":"ok"}}
```

---

## 10. Worker + Docker‑in‑Docker — the hard part

EKS AL2023 nodes run **containerd** and expose **no** `/var/run/docker.sock`, so the
classic "Docker‑outside‑of‑Docker" mount fails. Each worker pod therefore runs its own
Docker daemon as a **dind sidecar**; the worker talks to it over TCP.

Final shape (see `worker-dind-patch.yaml`):
- `worker` container: `DOCKER_HOST=tcp://localhost:2375` (the Docker SDK reads this).
- `dind` container: `docker:27-dind`, `--host=tcp://0.0.0.0:2375`, `privileged: true`,
  `runAsUser: 0` (NOT rootless), emptyDir at `/var/lib/docker`.
- Shared `sandbox` emptyDir for per‑execution source files (Section 12c).

This worked but surfaced **four bugs** — all fixed and documented in Section 16. The two
that matter for the worker manifest:

### 10a. dind must be a NATIVE SIDECAR (startup ordering)
On scale‑up, fresh worker pods `CrashLoopBackOff` with
`fatal: docker daemon unreachable at tcp://localhost:2375` — the worker started before
dockerd was ready. Fix: run dind as a **native sidecar** (an `initContainer` with
`restartPolicy: Always`) with a **startupProbe** on `tcp:2375`. Kubernetes then starts
dind and waits for the probe before starting the worker — zero crashes on scale‑up.
```yaml
initContainers:
  - name: init-sandbox      # prepares /sandbox-host perms
    ...
  - name: dind              # native sidecar
    image: docker:27-dind
    args: ["--host=tcp://0.0.0.0:2375"]
    restartPolicy: Always
    securityContext: { privileged: true, runAsUser: 0 }
    startupProbe:  { tcpSocket: { port: 2375 }, periodSeconds: 2, failureThreshold: 40 }
    readinessProbe:{ tcpSocket: { port: 2375 }, periodSeconds: 10 }
    volumeMounts:
      - { name: dind-storage, mountPath: /var/lib/docker }
      - { name: sandbox,      mountPath: /sandbox-host }   # see 12c
containers:
  - name: worker
    env:
      - { name: DOCKER_HOST, value: "tcp://localhost:2375" }
      - { name: CODERUNTIME_WORKER_TMP_DIR, value: "/sandbox-host" }
    volumeMounts:
      - { name: sandbox, mountPath: /sandbox-host }
```

---

## 11. KEDA — worker autoscaling on SQS backlog

Workers scale on **queue depth**, not CPU. Install KEDA and apply the ScaledObject:
```bash
helm repo add kedacore https://kedacore.github.io/charts && helm repo update
helm upgrade --install keda kedacore/keda -n keda --create-namespace \
  --set serviceAccount.operator.name=keda-operator   # SA pre-created by eksctl (IRSA)
kubectl apply -f deployments/kubernetes/eks/keda-triggerauth-irsa.yaml
kubectl apply -f deployments/kubernetes/eks/keda-scaledobject-eks.yaml
```
`keda-scaledobject-eks.yaml` (final): `minReplicaCount: 1`, **`maxReplicaCount: 20`**,
trigger `aws-sqs-queue`, `queueLength: 50` (msgs per pod ⇒ desired = ceil(backlog/50),
capped 20), `pollingInterval: 15s`, `cooldownPeriod: 120s`. Auth is IRSA via
`keda-triggerauth-irsa.yaml` (`podIdentity` / aws‑eks) — no keys.

> **Behaviour note:** with `wait=true` requests at client concurrency *C*, in‑flight SQS
> messages ≈ *C*, so backlog stays below `50/pod` and workers **don't** scale. Drive real
> scaling with an ingestion burst (`--no-wait`) that floods the queue.

---

## 12. Runtime images from a PRIVATE registry (ECR) — the execution path

The worker references runtime images by their bare tag `code-runtime-<lang>:latest`. The
`ImagePool` transparently pulls them from ECR and re‑tags to the bare name, so callers and
`ContainerCreate` keep using the short name.

### 12a. The registry env var (BUG #1)
Pulls failed with `pull access denied` because the pull ref wasn't ECR‑prefixed. The Viper
field is `DockerConfig.Registry` (`mapstructure:"registry"` under the `docker` section), so
the env keys are **`CODERUNTIME_DOCKER_REGISTRY`** and **`CODERUNTIME_DOCKER_REGISTRY_AUTH`**
(the `DOCKER_` segment is mandatory).

### 12b. Build + push all 36 runtime images (amd64)
```bash
# parallel build helper (idempotent, skips images already in ECR)
LANGS="$(ls runtime-images)" JOBS=5 \
  bash scripts/build-push-runtime-ecr-parallel.sh
```
This creates `code-runtime-<lang>` repos and pushes `:latest` for each (bash, python, java,
go, node, … 36 total). Sizes range ~5 MB (bash) to ~290 MB (mongodb).

### 12c. Sandbox bind‑mount across dind (BUG #2)
`invalid mount config: bind source path does not exist: /tmp/sandbox/<uuid>` — the worker
wrote source files to its own FS, but the **dind daemon** (which actually creates the bind
mount) couldn't see them. First fix (shared emptyDir at `/tmp/sandbox`) failed because dind
mounts a **tmpfs over `/tmp`** that shadows it. Final fix: put the host workdir **outside
`/tmp`** at `/sandbox-host`, shared into **both** worker and dind:
```
CODERUNTIME_WORKER_TMP_DIR = /sandbox-host
volumes: [{ name: sandbox, emptyDir: {} }]   # mounted /sandbox-host in worker AND dind
```

### 12d. ECR registry‑auth secret (X‑Registry‑Auth)
```bash
TOKEN=$(aws ecr get-login-password --region ap-southeast-2)
XAUTH=$(python3 -c "import json,base64,sys;print(base64.b64encode(json.dumps(\
{'username':'AWS','password':sys.argv[1],'serveraddress':sys.argv[2]}).encode()).decode())" \
  "$TOKEN" "$REGISTRY")
kubectl -n coderuntime create secret generic ecr-registry-auth \
  --from-literal=CODERUNTIME_DOCKER_REGISTRY="$REGISTRY" \
  --from-literal=CODERUNTIME_DOCKER_REGISTRY_AUTH="$XAUTH" \
  --dry-run=client -o yaml | kubectl apply -f -
# wire into the worker via envFrom: [{ secretRef: { name: ecr-registry-auth } }]
kubectl -n coderuntime rollout restart deployment/worker
```
> **The ECR token expires ~12h** — re‑run this to refresh. For production, run an
> initContainer/CronJob that refreshes the secret, or migrate to the ECR credential helper.

### 12e. DB‑language pull path (BUG #3)
mysql/postgresql/mongodb (IDs 38/39/40) failed with `No such image` because
`db_runtime.go` `runEphemeralDB()` called `ContainerCreate` directly and never pulled.
Fix: call `r.docker.ImagePool().EnsureImage(ctx, spec.image)` (registry‑aware pull+retag)
before creating the DB container. Rebuilt the worker image as `coderuntime/worker:v2`.

---

## 13. Build & deploy the worker image with all fixes

```bash
docker buildx build --platform linux/amd64 -f docker/Dockerfile.worker \
  -t $REGISTRY/coderuntime/worker:v2 --push .
kubectl -n coderuntime set image deployment/worker \
  worker=$REGISTRY/coderuntime/worker:v2
kubectl -n coderuntime rollout status deployment/worker
```

---

## 14. Public endpoint — LoadBalancer / ELB

The api‑gateway Service was `ClusterIP` (internal only) and the bundled `Ingress` had no
controller, so it had no address. Exposed it with a `type: LoadBalancer` Service:
```yaml
apiVersion: v1
kind: Service
metadata:
  name: api-gateway-lb
  namespace: coderuntime
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled: "true"
    service.beta.kubernetes.io/aws-load-balancer-connection-idle-timeout: "300"
spec:
  type: LoadBalancer
  selector: { app.kubernetes.io/name: api-gateway }
  ports: [{ name: http, port: 80, targetPort: 8002 }]
```

### 14a. Classic ELB vs NLB vs ALB (important)
- The **in‑tree AWS cloud provider** provisions a **Classic ELB** for a plain
  `type: LoadBalancer` Service — **no controller required**. This is what we use.
- The annotation `aws-load-balancer-type: external` (+ `nlb-target-type: ip`) makes the
  in‑tree controller **skip** the Service — it requires the **AWS Load Balancer Controller**,
  which is **not installed** here. Omit it, or the Service stays `<pending>` forever.

### 14b. Raise the idle timeout
The Classic ELB default idle timeout is **60s**; cold sandbox image pulls can exceed it and
get cut off. The `connection-idle-timeout: "300"` annotation above fixes this.

### 14c. (Optional) ALB via the AWS Load Balancer Controller
For HTTP routing/TLS at L7, install the controller and use an `Ingress` (`ingressClassName:
alb`) instead. You then manage the ALB's target‑group security groups, and the Service
becomes `ClusterIP` again. Not used in this deployment.

```bash
kubectl -n coderuntime get svc api-gateway-lb   # EXTERNAL-IP = ELB DNS (not a static IP)
# http://aa6f9d7b568b4425f84ea155a296db33-71808099.ap-southeast-2.elb.amazonaws.com
```

---

## 15. Node autoscaling — cluster‑autoscaler (for >~5 worker pods)

Each worker pod requests **worker 500m + dind 250m = 750m CPU**; an `m7i-flex.large` has
~1930m allocatable ⇒ **~2 worker pods/node**. 20 pods ⇒ **~10–11 nodes**. So KEDA's 20‑pod
ceiling needs node autoscaling, or pods sit `Pending`.

```bash
# 1) raise the managed nodegroup max 4 -> 12
aws eks update-nodegroup-config --cluster-name coderuntime --nodegroup-name system \
  --scaling-config minSize=2,maxSize=12,desiredSize=2 --region ap-southeast-2

# 2) IAM policy + IRSA SA for cluster-autoscaler
aws iam create-policy --policy-name coderuntime-cluster-autoscaler \
  --policy-document file://deployments/kubernetes/eks/ca-policy.json
eksctl create iamserviceaccount --cluster=coderuntime --region ap-southeast-2 \
  --namespace=kube-system --name=cluster-autoscaler \
  --attach-policy-arn=arn:aws:iam::768054003064:policy/coderuntime-cluster-autoscaler \
  --override-existing-serviceaccounts --approve

# 3) deploy cluster-autoscaler (image matches k8s 1.32; autodiscovery via ASG tags)
kubectl apply -f deployments/kubernetes/eks/cluster-autoscaler.yaml
```
The eksctl managed‑nodegroup ASG is already tagged
`k8s.io/cluster-autoscaler/enabled` and `k8s.io/cluster-autoscaler/coderuntime=owned`, which
the autoscaler uses for `--node-group-auto-discovery`. The CA RBAC must include
`storage.k8s.io/volumeattachments` and a kube‑system `Role` for the
`cluster-autoscaler-status` ConfigMap (both in `cluster-autoscaler.yaml`).

---

## 16. Bugs encountered & fixed (quick index)

| # | Symptom | Root cause | Fix |
|---|---|---|---|
| 1 | `pull access denied`, ref not ECR‑prefixed | wrong env name | use `CODERUNTIME_DOCKER_REGISTRY[_AUTH]` |
| 2 | `bind source path does not exist /tmp/sandbox/...` | dind can't see worker's files; tmpfs shadows `/tmp` | host workdir `/sandbox-host` shared into both containers |
| 3 | DB langs `No such image` | `db_runtime` bypassed the pull path | `ImagePool().EnsureImage()` before `ContainerCreate` (worker:v2) |
| 4 | new worker pods `CrashLoopBackOff` on scale‑up | worker starts before dind ready | dind as **native sidecar** + `startupProbe :2375` |

Plus infra fixes: free‑tier instance type (`m7i-flex.large`), EBS CSI driver for the Redis
PVC, namespace `privileged` for dind, `RDS_SG` inbound 3306 from the EKS SG, Redis
`REDIS_PASSWORD`, removed `imagePullSecrets`.

---

## 17. Verification

```bash
LB=http://aa6f9d7b568b4425f84ea155a296db33-71808099.ap-southeast-2.elb.amazonaws.com
JWT=$(curl -s -X POST $LB/auth/token -H 'Content-Type: application/json' \
  -d '{"email":"dev@coderuntime.io","password":"Admin123!"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

# one submission per language (by numeric language_id), wait for the verdict
curl -s -X POST "$LB/submissions?wait=true" -H "Authorization: Bearer $JWT" \
  -H 'Content-Type: application/json' \
  -d '{"language_id":1,"source_code":"echo hi"}'

# full per-language suite
python3 scripts/test_languages.py        # targets localhost:18002 (port-forward) by default
```
Verified Accepted: bash, go, node, python, java, …, and the DB engines
mysql(38)/postgresql(39)/mongodb(40).

---

## 18. Load test + autoscaling results (measured)

```bash
# ingestion burst that actually drives scaling (all langs except r)
python3 scripts/load_test_all_langs.py --base-url $LB \
  --total 1000 --concurrency 50 --no-wait --lang-ids "<all-but-30>"
```
| Metric | Result |
|---|---|
| Ingestion | 1000 submissions → **1000 accepted (100%)**, 166 req/s, p50 0.28s |
| KEDA | worker **1 → 20 pods** (backlog ~950 ÷ 50) |
| cluster‑autoscaler | ASG **2 → 11 nodes** (max 12) |
| Drain | backlog **946 → 0**, 16 workers running, **0 crashes** (post native‑sidecar) |
| Scale‑down | KEDA cooldown 120s → worker → 1; CA scale‑down‑unneeded 2m → nodes → 2 |

> A `wait=true` test at concurrency 25 kept backlog < 50/pod, so workers stayed at 1 —
> expected (see §11). Early cold‑start failures in wait mode were image‑pull latency on a
> single worker, not a system fault.

---

## 19. Teardown

```bash
# app
kubectl delete namespace coderuntime
kubectl -n kube-system delete -f deployments/kubernetes/eks/cluster-autoscaler.yaml
helm -n keda uninstall keda
# cluster (also removes the nodegroup, IRSA roles, and the ELB it created)
eksctl delete cluster --name coderuntime --region ap-southeast-2
# leftover ECR repos / IAM policy / the RDS SG rule are NOT removed by eksctl — clean up:
aws iam delete-policy --policy-arn arn:aws:iam::768054003064:policy/coderuntime-cluster-autoscaler
```
Do **not** delete RDS or SQS unless intended — they pre‑existed the cluster.

---

## 20. Cost & production notes

- **Free tier**: `m7i-flex.large` is the only free‑eligible type here. Scaling to ~11 nodes
  under load is **beyond free tier** — cluster‑autoscaler scales back to 2 when idle to cap cost.
- **Bigger nodes** (e.g. `m7i.2xlarge`, 8 vCPU ⇒ ~10 worker pods/node) cut node count and
  scheduling overhead at higher per‑node cost — a good alternative to many small nodes.
- **TLS**: the ELB serves plain HTTP:80. For production, terminate TLS via an ACM cert on the
  ELB/ALB and serve `:443`.
- **Private subnets + NAT** for nodes; restrict the RDS SG to the EKS SG only (done).
- **ECR auth**: replace the 12h X‑Registry‑Auth secret with an automated refresh.
```
