# Code Runtime — LOCAL (kind + KEDA, SQS mode)

A one-command local twin of the production EKS deployment. Same platform, same
**SQS** queue provider, same **KEDA** worker autoscaling — running on a laptop
with a [kind](https://kind.sigs.k8s.io) cluster.

```
localhost ──port-forward──▶ api-gateway (HPA)
                                │
   kind "coderuntime-local" (1 node, k8s 1.31)
     api-gateway ──▶ AWS SQS jobs queue ──▶ worker (KEDA 1→20)
     queue-manager                            └─ runs each job in a
     redis (StatefulSet)                          code-runtime-* sandbox
     mysql (StatefulSet)                          on the HOST Docker (DooD)
```

## Quick start

```bash
# from repo root: needs a .env with AWS keys + SQS URLs (QUEUE_PROVIDER=sqs)
deployments/kubernetes/local/setup.sh          # full bring-up (~5–8 min first run)
deployments/kubernetes/local/setup.sh watch    # watch KEDA scale the worker pods
deployments/kubernetes/local/setup.sh load 60  # fire 60 jobs → see scaling
deployments/kubernetes/local/setup.sh down     # tear down the kind cluster
```

Prereqs: `docker`, `kind`, `kubectl`, `helm`, `python3` (+ `bcrypt`: `pip install bcrypt`).

Login after bring-up: **dev@coderuntime.io / Admin123!**
API + Swagger:
```bash
kubectl --context kind-coderuntime-local -n coderuntime port-forward deploy/api-gateway 18002:8002
# http://localhost:18002/swagger/index.html
```

## Files

| File | Purpose |
|---|---|
| `setup.sh` | one-command bring-up (`up`/`watch`/`load`/`langs`/`seed`/`down`) |
| `kind-cluster.yaml` | kind node config (host Docker socket + shared `/tmp/sandbox` mounts) |
| `namespace.yaml` | ns (**privileged** PSA) + SAs + RBAC + PriorityClasses + quota/limits |
| `storageclass.yaml` | `fast-ssd` aliased to kind's `local-path` provisioner |
| `configmap.yaml` | common/api/worker/queue/mysql/redis config — **SQS mode**, no NATS |
| `redis.yaml` / `mysql.yaml` | in-cluster datastore StatefulSets + Services |
| `api-gateway.yaml` / `queue-manager.yaml` | Deployments + ClusterIP Services + HPAs |
| `worker.yaml` | worker Deployment (**DooD** host socket) + Service + PDB |
| `keda-scaledobject.yaml` | KEDA ScaledObject + **static-creds** TriggerAuthentication |
| `kustomization.yaml` | `kubectl apply -k .` for the workloads (secrets/cluster/KEDA op excluded) |

## How local differs from EKS (and why)

Everything a laptop **can** match, it does (SQS, KEDA, all three services, same
config knobs). The differences are only where a laptop can't provide the real
thing:

| Concern | EKS (`../eks/`) | LOCAL |
|---|---|---|
| Cluster / nodes | managed nodegroup + **cluster-autoscaler** (1→12) | 1-node kind, no CA |
| Datastores | RDS MySQL + ElastiCache | in-cluster MySQL/Redis StatefulSets |
| Sandbox runtime | **dind sidecar** pulls `code-runtime-*` from **ECR** | **DooD**: worker → host Docker; images built locally, no registry |
| SQS / KEDA auth | **IRSA** (keyless) | **static** `.env` AWS keys in the `aws-credentials` Secret |
| Images | ECR, `imagePullPolicy: Always` | `ghcr.io/coderuntime/*:latest` loaded into kind, `IfNotPresent` |
| API exposure | Classic ELB (`api-gateway-lb`) | `kubectl port-forward` |

**Why DooD locally:** the EKS worker pulls runtime images from ECR into its dind
sidecar. A laptop has no registry, so instead the worker talks to the **host**
Docker daemon (mounted socket) where `setup.sh` has already built the
`code-runtime-*` images — they're used directly. This is the one intentional
manifest divergence; it's why `worker.yaml` mounts `/var/run/docker.sock` and
`CODERUNTIME_WORKER_TMP_DIR=/tmp/sandbox` (a path shared identically between host,
kind node, and worker container so bind-mounts resolve on macOS).

## KEDA autoscaling (same as prod)

`keda-scaledobject.yaml` scales the worker Deployment on SQS backlog:
`desired = ceil(ApproximateNumberOfMessages / queueLength)`, capped at 20 — the
same formula as EKS, just authenticated with static keys instead of IRSA. Watch
it with `./setup.sh load 60` in one terminal and `./setup.sh watch` in another.

## Troubleshooting

- **worker `CrashLoopBackOff` / can't reach Docker** — ensure Docker Desktop is
  running; the worker uses the host socket. `supplementalGroups: [0, 999]` covers
  the root-owned socket kind passes through.
- **jobs stay `queued`** — the worker needs AWS keys to consume SQS. Confirm
  `.env` has valid `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` and the queue URLs;
  re-run `./setup.sh` to refresh the `aws-credentials` Secret.
- **`No such image: code-runtime-…`** — run `./setup.sh langs` to build the
  active-language images on the host.
- **HPA shows `<unknown>`** — metrics-server needs ~30s after install to report.
