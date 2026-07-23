# Code Runtime on EKS — One‑Go Deploy

The single runbook to stand up the whole platform on Amazon EKS. Everything is in
**clean, apply‑able manifests** under [`manifests/`](manifests/) plus one
orchestration script — **no manual `kubectl patch`**.

> Deep dive / why each piece exists: [EKS-DEPLOYMENT.md](EKS-DEPLOYMENT.md).
> Scaling to 10k/s: [SCALING-10K.md](SCALING-10K.md). Day‑2 ops:
> [kubectl.commands.md](kubectl.commands.md).

---

## What gets deployed

```
Internet ──HTTP:80──▶ Classic ELB (api-gateway-lb)
                          │
   EKS "coderuntime" (ap-southeast-2, k8s 1.32, IRSA = keyless AWS)
     api-gateway (HPA)  ──▶ SQS jobs queue ──▶ worker (KEDA 1→20)
     queue-manager (HPA)                         └─ dind sidecar runs each job
     redis (EBS gp3)                                in a code-runtime-* sandbox
     cluster-autoscaler: nodes 1→12 on demand
                          │                         │
                    RDS MySQL (private)        ECR (service + 40 runtime images)
```

| File | Purpose |
|---|---|
| `cluster.yaml` | eksctl: VPC reuse, OIDC/IRSA SAs, managed nodegroup, addons (incl. EBS CSI) |
| `setup.sh` | one‑go orchestration (SQS → cluster → images → secrets → KEDA → CA → ECR‑refresher → workloads) |
| `teardown.sh` | delete the cluster + ELB + EBS to stop billing (keeps RDS/SQS/ECR); rebuild with `setup.sh` |
| `manifests/00-namespace.yaml` | namespace + **privileged** PodSecurity (for dind) |
| `manifests/01-storageclass.yaml` | `gp3` StorageClass for Redis |
| `manifests/02-configmaps.yaml` | common / api / worker config (latency‑tuned) |
| `manifests/03-secrets.example.yaml` | secret **template** (real values via `setup.sh`) |
| `manifests/10-redis.yaml` | Redis StatefulSet + Services |
| `manifests/20-api-gateway.yaml` | api Deployment + Service + **LoadBalancer** + HPA |
| `manifests/30-queue-manager.yaml` | queue-manager Deployment + Service + HPA |
| `manifests/40-worker.yaml` | worker Deployment (**dind native sidecar**) + **KEDA** ScaledObject + TriggerAuth |
| `cluster-autoscaler.yaml` + `ca-policy.json` | node autoscaler + its IAM policy |

All four hard‑won fixes are baked into the manifests (no patching):
1. **ECR pull** — worker `envFrom: ecr-registry-auth` (`CODERUNTIME_DOCKER_REGISTRY[_AUTH]`).
2. **Sandbox bind mount** — shared `sandbox` volume at **`/sandbox-host`** in worker + dind; `CODERUNTIME_WORKER_TMP_DIR=/sandbox-host`.
3. **DB languages** — handled in the `worker` image (`ImagePool.EnsureImage`), tag `v2+`.
4. **dind startup race** — dind is a **native sidecar** (`initContainer` + `restartPolicy: Always` + `startupProbe :2375`).

---

## Prerequisites

- `awscli` v2, `eksctl`, `kubectl`, `helm`, `docker` (+buildx); AWS admin creds.
- An RDS MySQL instance reachable from the cluster VPC, with the DB `coderuntime`
  created and its security group allowing **3306 from the EKS cluster SG**.
- Two SQS queues (jobs + completed) and their URLs.
- Edit `cluster.yaml` to your **VPC/subnet IDs, region, account, and SQS ARNs**.

---

## Deploy (one command)

```bash
cd deployments/kubernetes/eks

export AWS_REGION=ap-southeast-2
export AWS_ACCOUNT_ID=768054003064
export DB_HOST=coderuntime-prod.xxxxxx.ap-southeast-2.rds.amazonaws.com
export DB_USER=coderuntime
export DB_PASSWORD='********'
export JWT_SECRET='a-long-random-string'
export REDIS_PASSWORD='********'
export SQS_JOBS_QUEUE_URL=https://sqs.ap-southeast-2.amazonaws.com/768054003064/code-submission-processing-queue
export SQS_COMPLETED_QUEUE_URL=https://sqs.ap-southeast-2.amazonaws.com/768054003064/code-submission-completed-queue

./setup.sh
```

`setup.sh` is **idempotent** — re‑run it anytime (e.g. to refresh the ~12h ECR
token, push new images, or reconcile drift). To skip the slow 40‑image runtime
build on a re‑run: `BUILD_RUNTIME_IMAGES=false ./setup.sh`.

It prints the public endpoint at the end:
```
http://<elb-dns>.ap-southeast-2.elb.amazonaws.com
```

---

## Manual deployment — step by step

If you'd rather run each phase yourself (what `setup.sh` does, broken out). Set
these once, used throughout:

```bash
cd deployments/kubernetes/eks
export AWS_REGION=ap-southeast-2
export AWS_ACCOUNT_ID=768054003064
export CLUSTER=coderuntime
export NS=coderuntime
export REGISTRY=$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com
export IMAGE_TAG=v1
```

### Step 0 — prerequisites check
```bash
aws sts get-caller-identity                 # confirm the right account + admin creds
eksctl version && kubectl version --client && helm version && docker buildx version
```

### Step 1 — create the cluster (VPC reuse, IRSA, nodegroup, addons)
Edit `cluster.yaml` first (VPC/subnet IDs, region, account, SQS ARNs), then:
```bash
eksctl create cluster -f cluster.yaml        # ~15-20 min
aws eks update-kubeconfig --name $CLUSTER --region $AWS_REGION
kubectl get nodes                            # expect 2 Ready
eksctl get addon --cluster $CLUSTER --region $AWS_REGION | grep ebs   # ebs-csi ACTIVE
```

### Step 2 — open RDS to the cluster + create the DB
```bash
EKS_SG=$(aws eks describe-cluster --name $CLUSTER --region $AWS_REGION \
  --query 'cluster.resourcesVpcConfig.clusterSecurityGroupId' --output text)
RDS_SG=$(aws rds describe-db-instances --db-instance-identifier coderuntime-prod \
  --region $AWS_REGION --query 'DBInstances[0].VpcSecurityGroups[0].VpcSecurityGroupId' --output text)
aws ec2 authorize-security-group-ingress --group-id "$RDS_SG" \
  --protocol tcp --port 3306 --source-group "$EKS_SG" --region $AWS_REGION || true
# create the database (throwaway in-cluster mysql client; it reaches the private RDS)
kubectl run mysql-cli --rm -it --image=mysql:8 --restart=Never -- \
  mysql -h <RDS_ENDPOINT> -u <DB_USER> -p   # then: CREATE DATABASE IF NOT EXISTS coderuntime;
```
Seed an app user so you can mint tokens (bcrypt the password first):
```sql
INSERT INTO users (email,password_hash,name,is_active,is_admin,created_at,updated_at)
VALUES ('dev@coderuntime.io','<bcrypt-hash>','dev',1,1,NOW(),NOW());
```

### Step 3 — ECR login + push the 3 service images (amd64)
```bash
aws ecr get-login-password --region $AWS_REGION | docker login --username AWS --password-stdin $REGISTRY
cd ../../..                                   # repo root
for svc in api worker queue-manager; do
  aws ecr describe-repositories --repository-names coderuntime/$svc --region $AWS_REGION >/dev/null 2>&1 \
    || aws ecr create-repository --repository-name coderuntime/$svc --region $AWS_REGION
  docker buildx build --platform linux/amd64 -f docker/Dockerfile.$svc \
    -t $REGISTRY/coderuntime/$svc:$IMAGE_TAG --push .
done
```

### Step 4 — build + push the 40 runtime images (amd64)
```bash
LANGS="$(ls runtime-images)" JOBS=5 bash scripts/build-push-runtime-ecr-parallel.sh
cd deployments/kubernetes/eks                  # back to the eks dir
```

### Step 5 — namespace + storageclass
```bash
kubectl apply -f manifests/00-namespace.yaml
kubectl apply -f manifests/01-storageclass.yaml
```

### Step 6 — secrets (app creds + ECR pull auth)
```bash
kubectl -n $NS create secret generic api-secrets \
  --from-literal=DATABASE_HOST=<RDS_ENDPOINT> --from-literal=DATABASE_USER=<DB_USER> \
  --from-literal=DATABASE_PASSWORD=<DB_PASSWORD> --from-literal=JWT_SECRET=<JWT_SECRET> \
  --from-literal=REDIS_HOST=redis --from-literal=REDIS_PASSWORD=<REDIS_PASSWORD>
kubectl -n $NS create secret generic worker-secrets \
  --from-literal=DATABASE_HOST=<RDS_ENDPOINT> --from-literal=DATABASE_USER=<DB_USER> \
  --from-literal=DATABASE_PASSWORD=<DB_PASSWORD> \
  --from-literal=REDIS_HOST=redis --from-literal=REDIS_PASSWORD=<REDIS_PASSWORD>
kubectl -n $NS create secret generic redis-secrets --from-literal=REDIS_PASSWORD=<REDIS_PASSWORD>

# ECR X-Registry-Auth (the worker uses this to pull code-runtime-* — expires ~12h)
TOKEN=$(aws ecr get-login-password --region $AWS_REGION)
XAUTH=$(python3 -c "import json,base64,sys;print(base64.b64encode(json.dumps({'username':'AWS','password':sys.argv[1],'serveraddress':sys.argv[2]}).encode()).decode())" "$TOKEN" "$REGISTRY")
kubectl -n $NS create secret generic ecr-registry-auth \
  --from-literal=CODERUNTIME_DOCKER_REGISTRY="$REGISTRY" \
  --from-literal=CODERUNTIME_DOCKER_REGISTRY_AUTH="$XAUTH"
```

### Step 7 — config + SQS URLs
```bash
kubectl apply -f manifests/02-configmaps.yaml
kubectl -n $NS patch cm common-config --type=merge -p \
 "{\"data\":{\"CODERUNTIME_SQS_REGION\":\"$AWS_REGION\",\
\"CODERUNTIME_SQS_JOBS_QUEUE_URL\":\"https://sqs.$AWS_REGION.amazonaws.com/$AWS_ACCOUNT_ID/code-submission-processing-queue\",\
\"CODERUNTIME_SQS_START_QUEUE_URL\":\"https://sqs.$AWS_REGION.amazonaws.com/$AWS_ACCOUNT_ID/code-submission-completed-queue\"}}"
```

### Step 8 — KEDA (worker autoscaling on SQS)
```bash
helm repo add kedacore https://kedacore.github.io/charts && helm repo update
helm upgrade --install keda kedacore/keda -n keda --create-namespace \
  --set serviceAccount.operator.name=keda-operator        # reuse the IRSA SA from cluster.yaml
kubectl -n keda rollout status deploy/keda-operator --timeout=180s
```

### Step 9 — cluster-autoscaler (node autoscaling)
```bash
# IAM policy (idempotent)
POLICY_ARN=$(aws iam list-policies --scope Local \
  --query "Policies[?PolicyName=='coderuntime-cluster-autoscaler'].Arn" --output text)
[ -z "$POLICY_ARN" ] || [ "$POLICY_ARN" = None ] && POLICY_ARN=$(aws iam create-policy \
  --policy-name coderuntime-cluster-autoscaler --policy-document file://ca-policy.json \
  --query 'Policy.Arn' --output text)
# IRSA SA + deploy
eksctl create iamserviceaccount --cluster=$CLUSTER --region $AWS_REGION \
  --namespace=kube-system --name=cluster-autoscaler \
  --attach-policy-arn=$POLICY_ARN --override-existing-serviceaccounts --approve
kubectl apply -f cluster-autoscaler.yaml
kubectl -n kube-system rollout status deploy/cluster-autoscaler --timeout=120s
```

### Step 10 — deploy the workloads
```bash
kubectl apply -f manifests/10-redis.yaml
kubectl apply -f manifests/20-api-gateway.yaml
kubectl apply -f manifests/30-queue-manager.yaml
kubectl apply -f manifests/40-worker.yaml
# point the deployments at the images you pushed
kubectl -n $NS set image deploy/api-gateway   api-gateway=$REGISTRY/coderuntime/api:$IMAGE_TAG
kubectl -n $NS set image deploy/queue-manager queue-manager=$REGISTRY/coderuntime/queue-manager:$IMAGE_TAG
kubectl -n $NS set image deploy/worker        worker=$REGISTRY/coderuntime/worker:$IMAGE_TAG
```

### Step 11 — wait + get the public URL
```bash
kubectl -n $NS rollout status deploy/api-gateway --timeout=300s
kubectl -n $NS rollout status deploy/queue-manager --timeout=300s
kubectl -n $NS rollout status deploy/worker --timeout=300s
kubectl -n $NS get pods
kubectl -n $NS get svc api-gateway-lb -o jsonpath='http://{.status.loadBalancer.ingress[0].hostname}{"\n"}'
```

---

## Verify

```bash
LB=$(kubectl -n coderuntime get svc api-gateway-lb -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
curl http://$LB/health           # {"status":"ok","services":{"mysql":"ok","redis":"ok"}}

JWT=$(curl -s -X POST http://$LB/auth/token -H 'Content-Type: application/json' \
  -d '{"email":"dev@coderuntime.io","password":"Admin123!"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
curl -s -X POST "http://$LB/submissions?wait=true" -H "Authorization: Bearer $JWT" \
  -H 'Content-Type: application/json' -d '{"language_id":1,"source_code":"echo hi"}'
```
Swagger UI: `http://$LB/swagger/index.html` (Authorize attaches + persists; the
spec auto‑detects the server URL from the request).

> First request per language pulls its image into the worker's dind cache (slow
> once); subsequent runs are warm. Optionally pre‑warm via
> `CODERUNTIME_DOCKER_PREWARM_IMAGES`.

---

## Autoscaling (on by default)

| Layer | Knob | Default |
|---|---|---|
| Worker pods | KEDA `queueLength` / `min`/`max` in `40-worker.yaml` | 1→20, 50 msgs/pod |
| Nodes | nodegroup `max` + cluster-autoscaler | 1→12 |
| api / queue | HPA in their manifests | 1→10 / 1→4 |

**Pause to save cost** (keeps everything, ~0 node cost):
```bash
kubectl -n kube-system scale deploy/cluster-autoscaler --replicas=0
aws eks update-nodegroup-config --cluster-name coderuntime --nodegroup-name system \
  --scaling-config minSize=0,maxSize=12,desiredSize=0 --region ap-southeast-2
```
**Resume:** set `--replicas=1` and `minSize=1,desiredSize=1`.

---

## Teardown

```bash
kubectl delete -f manifests/ ; kubectl delete -f cluster-autoscaler.yaml
helm -n keda uninstall keda
eksctl delete cluster --name coderuntime --region ap-southeast-2   # removes nodes, ELB, IRSA roles
```
RDS, SQS, and ECR are **not** removed by eksctl — clean up separately if intended.
