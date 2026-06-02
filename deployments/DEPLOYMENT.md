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
| `CODERUNTIME_DATABASE_HOST`             | Postgres hostname                        | `localhost` |
| `CODERUNTIME_DATABASE_USER`             | Postgres user                            | `postgres`  |
| `CODERUNTIME_DATABASE_PASSWORD`         | Postgres password                        | (empty)     |
| `CODERUNTIME_DATABASE_NAME`             | Postgres DB name                         | `coderuntime` |
| `CODERUNTIME_DATABASE_SSL_MODE`         | `disable` / `require` / `verify-full`    | `disable`   |
| `CODERUNTIME_REDIS_HOST`                | Redis hostname                           | `localhost` |
| `CODERUNTIME_REDIS_PASSWORD`            | Redis auth                               | (empty)     |
| `CODERUNTIME_NATS_URL`                  | NATS connection URL                      | `nats://localhost:4222` |
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

The fastest path. Everything runs on one host, no auth on Postgres/Redis,
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
| postgres    | 5432        | Submissions, languages, statuses          |
| redis       | 6379        | Rate-limit + caching                      |
| nats        | 4222, 8222  | Job queue (JetStream) + monitoring        |
| prometheus  | 9090        | Metrics scraping                          |
| grafana     | 3000        | Dashboards (admin/admin)                  |
| jaeger      | 16686       | Distributed traces                        |

### 3.3 Seed the database

The API auto-seeds languages on startup (`SeedLanguages` in
[internal/models/language.go](../internal/models/language.go)), but you can
also run:

```bash
make migrate   # apply DB migrations
make seed      # idempotent language + status seed
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
- Hides postgres / redis / nats / grafana / jaeger from the host — only the
  ingress (nginx / lb) should reach the API.
- Sets `deploy.replicas`, rolling-update policy, and resource limits.
- Switches the overlay network to encrypted mode for Swarm.

### Required env vars before `up`

Export these (or use a `.env` file next to `docker-compose.prod.yml`):

```bash
export REGISTRY=ghcr.io/yourorg
export IMAGE_TAG=1.0.0
export DATABASE_HOST=postgres.internal
export DATABASE_USER=coderuntime
export DATABASE_PASSWORD='...'
export REDIS_HOST=redis.internal
export REDIS_PASSWORD='...'
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

## 5. Kubernetes — raw manifests

The manifests under [`deployments/kubernetes/`](kubernetes/) are intentionally
verbose (PodDisruptionBudget, HPA, NetworkPolicy, RBAC, PriorityClass) so they
can be diffed and audited. Apply them in this order:

```bash
kubectl apply -f deployments/kubernetes/namespace.yaml
# Fill in real base64 values first — see comments in the file.
kubectl apply -f deployments/kubernetes/secrets.yaml
kubectl apply -f deployments/kubernetes/configmap.yaml
kubectl apply -f deployments/kubernetes/postgres-statefulset.yaml
kubectl apply -f deployments/kubernetes/redis-statefulset.yaml
kubectl apply -f deployments/kubernetes/nats-statefulset.yaml
kubectl apply -f deployments/kubernetes/api-deployment.yaml
kubectl apply -f deployments/kubernetes/worker-deployment.yaml
```

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

## 6. Kubernetes — Helm chart

[`deployments/helm/`](helm/) packages the same workload plus Bitnami
postgres / redis / nats / prometheus / grafana subcharts.

```bash
cd deployments/helm
helm dependency update
helm install coderuntime . \
  --namespace coderuntime --create-namespace \
  --set global.imageRegistry=ghcr.io/yourorg \
  --set global.imageTag=1.0.0 \
  -f my-values.yaml
```

The chart renders these templates for the API + worker:

| Template               | Purpose                                       |
|------------------------|-----------------------------------------------|
| `_helpers.tpl`         | Shared name/label helpers                     |
| `deployment.yaml`      | API + worker Deployments                      |
| `configmap.yaml`       | common-config, api-config, worker-config      |
| `secret.yaml`          | api-secrets, worker-secrets (b64enc applied)  |
| `serviceaccount.yaml`  | One ServiceAccount per component              |
| `service.yaml`         | api ClusterIP Service + worker headless metrics Service |

PostgreSQL, Redis, NATS, Prometheus, Grafana, and Jaeger come from the
Bitnami / community subcharts declared in [`Chart.yaml`](helm/Chart.yaml).

`my-values.yaml` should override:

```yaml
global:
  imageRegistry: ghcr.io/yourorg
  imageTag: "1.0.0"

postgresql:
  auth:
    password: "..."
redis:
  auth:
    password: "..."

api:
  secrets:
    jwtSecret: "..."           # rendered through b64enc — pass plain text

worker:
  securityContext:
    dockerGroupGid: 999        # set to host's docker group GID
```

### Verifying the chart renders before install

```bash
cd deployments/helm
helm dependency update
helm template test . -f my-values.yaml | less
```

If `helm template` fails with `function "include" not defined for "X"`,
delete subchart-only references; if it fails on a missing value, add it to
`my-values.yaml`.

---

## 7. Observability

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

## 9. Troubleshooting

| Symptom                                                              | Likely cause                                                                                       |
|----------------------------------------------------------------------|----------------------------------------------------------------------------------------------------|
| API logs `database connection refused`                               | `CODERUNTIME_DATABASE_HOST` empty/wrong, or postgres not ready yet                                 |
| API ignores all your env config, falls back to defaults              | Env vars not prefixed with `CODERUNTIME_`                                                          |
| Worker logs `pull access denied for code-runtime-*:latest`           | `make pull-images` was skipped — locally-built sandbox images aren't on Docker Hub                 |
| Docker warning *Image may have poor performance under emulation*     | Running an `amd64`-only sandbox image on Apple Silicon. Build the local `code-runtime-*` variant.  |
| Grafana shows no dashboards                                          | Missing `dashboards.yaml` provider file in `provisioning/dashboards/` (fixed in this repo)         |
| Prometheus targets all DOWN                                          | Scrape config references services that don't exist in your compose stack                          |
| K8s worker pod stuck `CrashLoopBackOff` with EACCES on docker.sock   | `supplementalGroups` in worker pod doesn't match the host's docker group GID. Run `getent group docker` on a node and update [worker-deployment.yaml](kubernetes/worker-deployment.yaml) (or the `worker.securityContext.dockerGroupGid` value in Helm). |

---

## 10. Production checklist

- [ ] `CODERUNTIME_JWT_SECRET` is at least 32 random chars and stored in a real secret manager.
- [ ] Postgres has `SSL_MODE=require` or stricter; backups are configured.
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
