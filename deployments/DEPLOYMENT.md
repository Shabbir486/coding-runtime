# Deployment Guide

This document describes how to deploy CodeRuntime in three environments:

1. **Local / development** — Docker Compose on a single machine
2. **Production single-host** — Docker Compose with prod overrides + Swarm
3. **Kubernetes** — raw manifests or the Helm chart

All three share the same Go binaries (api-gateway, worker) and the same
sandbox runtime images under `runtime-images/`.

---

## 1. Prerequisites

| Component          | Version            | Notes                                              |
|--------------------|--------------------|----------------------------------------------------|
| Docker             | 24+                | Required everywhere; the worker uses the Docker socket to start sandbox containers |
| Docker Compose v2  | 2.20+              | For local + prod-compose flows                     |
| Go toolchain       | 1.24+              | Only needed to build binaries from source          |
| kubectl            | 1.28+              | For Kubernetes deployments                         |
| Helm               | 3.13+              | For the Helm chart path                            |
| Make               | any                | Convenience targets in the root `Makefile`         |

**Hardware (recommended dev baseline):** 4 vCPU, 8 GB RAM, 30 GB disk.
Sandbox runtime images alone consume ~10 GB once built.

---

## 2. Configuration model

The Go binaries (api-gateway, worker, queue-manager) read configuration
through [Viper](https://github.com/spf13/viper) with the **`CODERUNTIME_`**
env-var prefix (see [internal/config/config.go](../internal/config/config.go)).
This is the single most important deployment detail:

- `CODERUNTIME_DATABASE_HOST` → `database.host`
- `CODERUNTIME_SERVER_PORT` → `server.port`
- `CODERUNTIME_RATE_LIMIT_LIMIT` → `rate_limit.limit`

**Any env var without the `CODERUNTIME_` prefix is silently ignored.**
Every config file in this repo (compose, k8s, Helm) has been aligned to
use this prefix. If you fork manifests, keep the prefix.

### Key environment variables

| Variable                                | Purpose                                  | Default     |
|-----------------------------------------|------------------------------------------|-------------|
| `CODERUNTIME_SERVER_PORT`               | API HTTP port                            | `8002`      |
| `CODERUNTIME_DATABASE_HOST`             | MySQL hostname (RDS endpoint in prod)    | `localhost` |
| `CODERUNTIME_DATABASE_PORT`             | MySQL port                               | `3306`      |
| `CODERUNTIME_DATABASE_USER`             | MySQL user                               | `coderuntime` |
| `CODERUNTIME_DATABASE_PASSWORD`         | MySQL password                           | (empty)     |
| `CODERUNTIME_DATABASE_NAME`             | MySQL DB name                            | `coderuntime` |
| `CODERUNTIME_DATABASE_SSL_MODE`         | MySQL TLS: `disable`/`true`/`skip-verify`/`preferred` | `disable` |
| `CODERUNTIME_REDIS_HOST`                | Redis hostname (ElastiCache endpoint in prod) | `localhost` |
| `CODERUNTIME_REDIS_PASSWORD`            | Redis auth (empty for ElastiCache, no AUTH token) | (empty)     |
| `CODERUNTIME_REDIS_TLS_ENABLED`         | In-transit TLS — `true` for ElastiCache Serverless | `false`     |
| `CODERUNTIME_NATS_URL`                  | NATS connection URL (nats provider)      | `nats://localhost:4222` |
| `CODERUNTIME_QUEUE_PROVIDER`            | Job queue backend: `nats` or `sqs`       | `nats`      |
| `CODERUNTIME_SQS_REGION`                | AWS region (sqs provider)                | `us-east-1` |
| `CODERUNTIME_SQS_JOBS_QUEUE_URL`        | Per-submission execution-job queue URL   | (empty)     |
| `CODERUNTIME_SQS_PRIORITY_QUEUE_URL`    | High-priority SQS queue URL              | (empty)     |
| `CODERUNTIME_SQS_START_QUEUE_URL`       | START_BATCH_PROCESSING event queue URL   | (empty)     |
| `CODERUNTIME_SQS_DLQ_QUEUE_URL`         | Dead-letter SQS queue URL                | (empty)     |
| `CODERUNTIME_JWT_SECRET`                | HS256 signing secret (min 32 chars)      | (required)  |
| `CODERUNTIME_DOCKER_HOST`               | Worker → Docker daemon socket            | `unix:///var/run/docker.sock` |
| `CODERUNTIME_WORKER_COUNT`              | Concurrent jobs per worker process       | `4`         |
| `CODERUNTIME_WORKER_TMP_DIR`            | Host scratch dir bind-mounted into sandboxes | `/tmp/sandbox` |
| `CODERUNTIME_DOCKER_NETWORK_MODE`       | Sandbox network — almost always `none`   | `none`      |
| `CODERUNTIME_TRACING_ENABLED`           | OTLP traces on/off                       | `false`     |
| `CODERUNTIME_TRACING_ENDPOINT`          | OTLP collector URL                       | (empty)     |

See [internal/config/config.go](../internal/config/config.go) for the full
list (search for `v.SetDefault`).

---

## 3. Local development (Docker Compose)

The fastest path. Everything runs on one host (MySQL in a local container),
sandboxes use a bind-mounted `/tmp/sandbox`.

### 3.1 Build sandbox runtime images

This step builds every Dockerfile under [`runtime-images/`](../runtime-images/)
as `code-runtime-<lang>:latest` and pre-pulls the remaining upstream public
language images.

```bash
make pull-images
```

Expect 15–30 min on first run depending on bandwidth. Re-runs are fast — Docker
caches layers.

### 3.2 Bring up the platform

```bash
make compose-up
```

This runs `docker compose up -d --build` against [`docker-compose.yml`](../docker-compose.yml)
and starts:

| Service     | Port (host) | Purpose                                   |
|-------------|-------------|-------------------------------------------|
| api         | 8002        | REST API gateway                          |
| worker      | —           | Executes sandbox jobs (×2 replicas)       |
| mysql       | 3307→3306   | Submissions, languages, statuses          |
| redis       | 6379        | Rate-limit + caching                      |
| nats        | 4222, 8222  | Job queue (JetStream) + monitoring        |
| prometheus  | 9090        | Metrics scraping                          |
| grafana     | 3000        | Dashboards (admin/admin)                  |
| jaeger      | 16686       | Distributed traces                        |

### 3.3 Seed the database

The api-gateway auto-migrates the schema and seeds reference data on startup
(`database.MigrateAndSeed`, when `migrate_on_start=true`). To run the same
logic standalone — e.g. against **AWS RDS before** rolling out api/worker —
use the `cmd/migrate` tool via Make (connection comes from the same
`CODERUNTIME_*` env / `.env`):

```bash
make migrate          # schema migrations + seed statuses & languages (idempotent)
make migrate-schema   # schema migrations only
make seed             # seed statuses & languages only
```

### 3.4 Verify

```bash
curl -s localhost:8002/health        # {"status":"ok"}
curl -s localhost:8002/api/v1/languages | jq '.[] | .name'
```

Submit a one-off Python job:

```bash
curl -s -X POST localhost:8002/api/v1/submissions \
  -H 'Content-Type: application/json' \
  -d '{"language_id":29,"source_code":"print(\"hi\")"}'
```

### 3.5 Logs and shutdown

```bash
make compose-logs       # tail everything
make compose-down       # stop + remove containers (volumes persist)
make compose-restart    # full cycle
```

---

## 4. Production single-host (Compose + Swarm)

Layered config: dev compose + a prod override.

```bash
docker compose \
  -f docker-compose.yml \
  -f docker-compose.prod.yml \
  up -d
```

The prod override ([`docker-compose.prod.yml`](../docker-compose.prod.yml)):

- Pulls images from `${REGISTRY}/coderuntime-api:${IMAGE_TAG}` instead of building locally.
- Hides mysql / redis / nats / grafana / jaeger from the host — only the
  ingress (nginx / lb) should reach the API.
- Sets `deploy.replicas`, rolling-update policy, and resource limits.
- Switches the overlay network to encrypted mode for Swarm.

### Required env vars before `up`

Export these (or use a `.env` file next to `docker-compose.prod.yml`):

```bash
export REGISTRY=ghcr.io/yourorg
export IMAGE_TAG=1.0.0
export DATABASE_HOST=your-db.cluster-xxxx.us-east-1.rds.amazonaws.com  # AWS RDS endpoint (or mysql.internal)
export DATABASE_USER=coderuntime
export DATABASE_PASSWORD='...'
export REDIS_HOST=code-executor-xxxx.serverless.use2.cache.amazonaws.com  # AWS ElastiCache endpoint (or redis.internal)
export REDIS_PASSWORD=''                 # ElastiCache Serverless uses no AUTH token
export REDIS_TLS_ENABLED=true            # ElastiCache Serverless requires in-transit TLS
export NATS_URL=nats://nats.internal:4222
export JWT_SECRET="$(openssl rand -base64 48)"
export OTEL_ENDPOINT=http://otel-collector.internal:4318
export GRAFANA_ADMIN_PASSWORD='...'
export GRAFANA_ROOT_URL=https://grafana.example.com
```

For multi-host Swarm:

```bash
docker swarm init                       # on the manager
docker stack deploy -c docker-compose.yml -c docker-compose.prod.yml coderuntime
```

---

## 5. Kubernetes

The Kubernetes manifests are split into two **self-contained, separated**
environments under [`deployments/kubernetes/`](kubernetes/) — see
[kubernetes/README.md](kubernetes/README.md) for the routing table:

| Environment | Folder | One-command bring-up |
|---|---|---|
| **Local** (kind, in-cluster MySQL/Redis, DooD, static SQS keys) | [`kubernetes/local/`](kubernetes/local/) | `deployments/kubernetes/local/setup.sh` |
| **AWS / EKS** (the live cluster: RDS, dind+ECR, IRSA, cluster-autoscaler) | [`kubernetes/eks/`](kubernetes/eks/) | `deployments/kubernetes/eks/setup.sh` (teardown: `eks/teardown.sh`) |

Both run the SQS queue provider and KEDA worker autoscaling; both build from the
same `docker/Dockerfile.*` images and `runtime-images/` sandboxes. Full details:
[local/README.md](kubernetes/local/README.md) and [eks/DEPLOY.md](kubernetes/eks/DEPLOY.md).

> The old flat manifests (`namespace.yaml`, `configmap.yaml`, …) and the Helm
> chart under `deployments/helm/` have been removed in favor of these two
> curated, apply-able environment folders.

The notes below on ConfigMap layout, Secrets, and the worker Docker socket apply
to both environments.

### ConfigMap layout

[`configmap.yaml`](kubernetes/configmap.yaml) defines three application
ConfigMaps:

| ConfigMap       | Scope                                                          |
|-----------------|----------------------------------------------------------------|
| `common-config` | Shared by api + worker (DB host/port, Redis port, NATS URL, log settings) |
| `api-config`    | API-only: server timeouts, JWT, rate limits, CORS, metrics     |
| `worker-config` | Worker-only: sandbox dir, job timeouts, network mode           |

Each deployment mounts `common-config` plus its component-specific
ConfigMap via `envFrom`. To change a shared value once, edit `common-config`
and restart both deployments.

### Secrets

[`secrets.yaml`](kubernetes/secrets.yaml) ships with `<PLACEHOLDER_BASE64_*>`
markers. Replace them with real base64 values before applying. For real
deployments use [Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets),
[External Secrets Operator](https://external-secrets.io), or
[Vault](https://www.vaultproject.io) — do not commit real secrets to git.

The data key names in each Secret (e.g. `DATABASE_HOST`, `JWT_SECRET`) are
arbitrary labels. The Deployments map each key to a **prefixed env var name**
(`CODERUNTIME_DATABASE_HOST`, `CODERUNTIME_JWT_SECRET`) so the Go binary picks
them up via Viper.

### Image registry

The Deployments reference `ghcr.io/coderuntime/api-gateway:latest` and
`ghcr.io/coderuntime/worker:latest`. Override before applying or rebuild:

```bash
REGISTRY=ghcr.io/yourorg IMAGE_TAG=1.0.0 make docker-build docker-push
```

Then patch the image references:

```bash
kubectl -n coderuntime set image deployment/api-gateway \
  api-gateway=ghcr.io/yourorg/api-gateway:1.0.0
kubectl -n coderuntime set image deployment/worker \
  worker=ghcr.io/yourorg/worker:1.0.0
```

### Worker + Docker socket

The worker pod mounts `/var/run/docker.sock` to launch sandbox containers on
its host node. Two important caveats:

1. The worker runs as UID 10001 with `readOnlyRootFilesystem: true`. To let it
   write to the host's Docker socket, the pod sets `supplementalGroups: [999]`
   in [worker-deployment.yaml](kubernetes/worker-deployment.yaml). `999` is
   the docker group GID on Debian/Ubuntu nodes. **You must override this** to
   match your nodes: run `getent group docker | cut -d: -f3` on a worker node
   and edit the value. On RHEL/CentOS it's usually `998`. If GIDs differ
   across nodes, list all of them.
2. Sandbox containers therefore run on **the same node** as the worker pod —
   they are not scheduled by Kubernetes. The HPA scales worker pods, and each
   pod creates short-lived sibling containers via the Docker socket.

If you'd rather use Kubernetes-native sandboxing (Jobs / KEDA / kata), the
worker has a pluggable runtime interface in
[internal/runtime/](../internal/runtime/) — see `manager.go`.

### Sandbox runtime images

Worker nodes need the sandbox images locally (or reachable via a pull-through
mirror). On each node:

```bash
git clone <repo>
cd coding-runtime
bash scripts/pull-images.sh
```

Or pre-bake them into a node image / use DaemonSet image pre-pullers.

---

## 6. Observability

| Concern   | Path                                                        |
|-----------|-------------------------------------------------------------|
| Metrics   | API gateway `/metrics` (Prometheus format)                  |
| Traces    | OTLP → Jaeger collector (configured via `CODERUNTIME_TRACING_*`) |
| Logs      | stdout JSON in dev, JSON via Docker logging driver in prod  |
| Dashboards| [`deployments/monitoring/grafana/dashboards/`](monitoring/grafana/dashboards/) |

For Compose:

- Prometheus config: [`monitoring/prometheus.yml`](monitoring/prometheus.yml)
  (scrapes only API gateway — the worker does not currently expose an HTTP
  `/metrics` endpoint in compose mode).
- Grafana datasources: [`monitoring/grafana/datasources/prometheus.yaml`](monitoring/grafana/datasources/prometheus.yaml)
- Grafana dashboards: a provider YAML at
  [`monitoring/grafana/dashboards/dashboards.yaml`](monitoring/grafana/dashboards/dashboards.yaml)
  registers everything in the same directory.

For Kubernetes: the Helm chart's prometheus subchart auto-discovers pods
annotated with `prometheus.io/scrape: "true"` (already set on api-gateway and
worker pods).

---

## 8. Upgrades and rollbacks

### Compose

```bash
make docker-build           # rebuild api + worker images locally
make compose-restart        # rolling restart
```

### Kubernetes

```bash
kubectl -n coderuntime set image deployment/api-gateway api-gateway=$NEW
kubectl -n coderuntime rollout status deployment/api-gateway
kubectl -n coderuntime rollout undo deployment/api-gateway   # if needed
```

### Helm

```bash
helm upgrade coderuntime ./deployments/helm -n coderuntime -f my-values.yaml
helm rollback coderuntime 1 -n coderuntime
```

---

## 8b. Amazon SQS job queue (optional)

By default the platform uses the bundled NATS JetStream. To use **Amazon SQS**
instead, set `CODERUNTIME_QUEUE_PROVIDER=sqs` plus the `SQS_*` queue URLs on the
api, worker, **and** queue-manager. NATS is then unused (skip/retire it).

**Create three Standard queues** (DLQ first, then wire the redrive policy):

```bash
REGION=us-east-2
DLQ=$(aws sqs create-queue --queue-name coderuntime-jobs-dlq --region $REGION --query QueueUrl --output text)
DLQ_ARN=$(aws sqs get-queue-attributes --queue-url $DLQ --attribute-names QueueArn --region $REGION --query Attributes.QueueArn --output text)
REDRIVE="{\"deadLetterTargetArn\":\"$DLQ_ARN\",\"maxReceiveCount\":\"3\"}"
aws sqs create-queue --queue-name coderuntime-jobs          --region $REGION --attributes "{\"RedrivePolicy\":\"$(echo $REDRIVE | sed 's/"/\\"/g')\"}"
aws sqs create-queue --queue-name coderuntime-jobs-priority --region $REGION --attributes "{\"RedrivePolicy\":\"$(echo $REDRIVE | sed 's/"/\\"/g')\"}"
# START_BATCH_PROCESSING event queue (your orchestrator publishes {"batch_id":"..."} here)
aws sqs create-queue --queue-name coderuntime-batch-start    --region $REGION --attributes "{\"RedrivePolicy\":\"$(echo $REDRIVE | sed 's/"/\\"/g')\"}"
```

Set `VisibilityTimeout` on the job queues ≥ the max job execution time (≥330s).

**Decoupled batch flow.** `POST /batches` only persists the batch (PENDING) and
returns tokens. Your orchestrator then commits its own token mappings and
publishes `{"batch_id":"<id>"}` to the **start** queue (`SQS_START_QUEUE_URL`);
the worker's start consumer fans the batch out to the jobs queue and execution
begins. This guarantees no result webhook can fire before your persistence is
committed. (Locally / without an orchestrator, `POST /batches/:id/start` does the
same thing synchronously.)

**IAM policy** for the task/instance role (credentials are never put in config):

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "sqs:SendMessage", "sqs:SendMessageBatch",
      "sqs:ReceiveMessage", "sqs:DeleteMessage",
      "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"
    ],
    "Resource": [
      "arn:aws:sqs:us-east-2:<acct>:coderuntime-jobs",
      "arn:aws:sqs:us-east-2:<acct>:coderuntime-jobs-priority",
      "arn:aws:sqs:us-east-2:<acct>:coderuntime-batch-start",
      "arn:aws:sqs:us-east-2:<acct>:coderuntime-jobs-dlq"
    ]
  }]
}
```

Notes: messages carry only the submission **token** (the worker re-reads the job
from MySQL, avoiding the 256 KB SQS limit); the priority queue is polled before
the normal queue; retries and dead-lettering are handled by the **SQS redrive
policy**; queue-manager runs an SQS DLQ drainer (marks dead-lettered submissions
failed) and publishes queue-depth metrics. For **local** SQS, point
`CODERUNTIME_SQS_ENDPOINT` at ElasticMQ/LocalStack.

---

## 9. Troubleshooting

| Symptom                                                              | Likely cause                                                                                       |
|----------------------------------------------------------------------|----------------------------------------------------------------------------------------------------|
| API logs `database connection refused`                               | `CODERUNTIME_DATABASE_HOST` empty/wrong, RDS security group blocks the port, or mysql not ready yet |
| API logs `tls: ...` / handshake error connecting to RDS              | `CODERUNTIME_DATABASE_SSL_MODE` mismatch — RDS needs `true` (or `skip-verify` without the RDS CA bundle) |
| API ignores all your env config, falls back to defaults              | Env vars not prefixed with `CODERUNTIME_`                                                          |
| Worker logs `pull access denied for code-runtime-*:latest`           | `make pull-images` was skipped — locally-built sandbox images aren't on Docker Hub                 |
| Docker warning *Image may have poor performance under emulation*     | Running an `amd64`-only sandbox image on Apple Silicon. Build the local `code-runtime-*` variant.  |
| Grafana shows no dashboards                                          | Missing `dashboards.yaml` provider file in `provisioning/dashboards/` (fixed in this repo)         |
| Prometheus targets all DOWN                                          | Scrape config references services that don't exist in your compose stack                          |
| K8s worker pod stuck `CrashLoopBackOff` with EACCES on docker.sock   | `supplementalGroups` in worker pod doesn't match the host's docker group GID. Run `getent group docker` on a node and update [worker-deployment.yaml](kubernetes/worker-deployment.yaml) (or the `worker.securityContext.dockerGroupGid` value in Helm). |

---

## 10. Production checklist

- [ ] `CODERUNTIME_JWT_SECRET` is at least 32 random chars and stored in a real secret manager.
- [ ] MySQL has `CODERUNTIME_DATABASE_SSL_MODE=true` (TLS on); RDS automated backups / snapshots are enabled.
- [ ] Redis requires auth (`requirepass`) and is bound to a private network.
- [ ] NATS JetStream has persistent storage.
- [ ] Worker pods run on dedicated `node-type: compute-optimized` nodes (see worker affinity).
- [ ] `CODERUNTIME_DOCKER_NETWORK_MODE=none` — sandbox containers have no network.
- [ ] Sandbox containers run as non-root (verified in each `runtime-images/*/Dockerfile`).
- [ ] Resource quotas + LimitRange applied (already in `namespace.yaml`).
- [ ] NetworkPolicy denies external egress except DNS + 443 (already in `namespace.yaml`).
- [ ] PodDisruptionBudgets in place for API and worker.
- [ ] HPA min/max replicas tuned to your traffic.
- [ ] Image tags pinned (no `:latest` in production).
- [ ] Prometheus scrape config, alerting rules, and Grafana dashboards reviewed.

-- After local deployment expose the port
kubectl --context kind-coderuntime-local -n coderuntime port-forward deploy/api-gateway 18002:8002
