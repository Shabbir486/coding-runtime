# Project Knowledge Base

> Companion doc to [readme.md](readme.md) and [deployments/DEPLOYMENT.md](deployments/DEPLOYMENT.md).
> The readme tells you **what** this project is; this file tells you **why**
> it's built the way it is, and the conventions that aren't obvious from the
> code alone.

---

## 1. The elevator pitch

CodeRuntime is a **production-grade, multi-language, distributed code-execution
engine** — submit any source code in 39 supported languages over a REST API,
get back stdout / stderr / exit code / time / memory. Inspired by
[Judge0](https://judge0.com/) but written from scratch in Go, with first-class
support for both **runtime languages** (Python, Go, Java, Rust, …) and
**database languages** (SQLite, MySQL, PostgreSQL, MongoDB).

The platform is built around three Go services:

- **api-gateway** — REST API, JWT auth, rate-limiting, persists submissions,
  publishes execution jobs to NATS.
- **worker** — consumes jobs, spawns a fresh Docker sandbox per job, streams
  stdout/stderr, updates MySQL + Redis.
- **queue-manager** — owns NATS JetStream topology (stream creation) and
  runs the Dead Letter Queue processor.

Plus infrastructure: MySQL (AWS RDS in prod), Redis (AWS ElastiCache in prod),
NATS JetStream, Prometheus, Grafana, Jaeger.

---

## 2. What actually makes this project unique

These aren't generic "we use Go and Docker" features — they're decisions you
won't find in most code-execution engines:

### 2.1 Per-language Docker images that operators control

Every language has its own [`runtime-images/<lang>/Dockerfile`](runtime-images/)
built locally as `code-runtime-<lang>:latest`. This is unusual — most
sandboxes use the upstream public image directly. Why we don't:

- **Apple Silicon support.** Many upstream community images (`crystallang/crystal`,
  `mono`, `nimlang/nim`, `gmazzo/kotlin`, `hseeberger/scala-sbt`, …) are
  `linux/amd64` only and run under emulation on M-series Macs. Local builds
  use multi-arch bases (`alpine:3.21`, `eclipse-temurin:21-jdk-alpine`,
  `debian:bookworm-slim`).
- **Hardening.** Each Dockerfile creates a non-root `sandbox` user (UID 10001),
  drops unnecessary packages, and verifies the toolchain in a final `RUN`
  step. Upstream images often run as root.
- **Pre-warming.** Each image pre-installs the most-used libraries (numpy/pandas
  for Python, common gems for Ruby, scientific R packages, …) so user code
  doesn't pay an apt-get / pip-install penalty on every run.
- **Pinned versions.** The image tag is exactly what's listed in
  [internal/models/language.go](internal/models/language.go) — no surprise
  drift from a `:latest` tag changing under you.

`make pull-images` builds every `runtime-images/<lang>/Dockerfile` plus
pre-pulls the remaining upstream language images.

### 2.2 Database languages are real, not a hack

[internal/runtime/db_runtime.go](internal/runtime/db_runtime.go) treats
SQLite, MySQL, PostgreSQL, and MongoDB as proper "languages" — each
submission spins up an **ephemeral DB container**, runs the user's
queries, and returns the result set as the submission output. The DB
runtime branches off from the regular sandbox in
[internal/runtime/manager.go](internal/runtime/manager.go) based on
`language.IsDatabase`.

This means a single API surface handles "run this Python script" and
"run this SQL query" — clients don't need to know which is which.

### 2.3 Dual sync/async API surface

`POST /submissions?wait=false` (default) returns immediately with an ID;
the client polls or websocket-subscribes for the result. `wait=true` blocks
the HTTP request until the worker finishes (capped by
`CODERUNTIME_WORKER_JOB_TIMEOUT`). Both paths share the same publisher,
worker, and Redis-poll completion logic — no duplicated code paths.

### 2.4 Queue topology has a dedicated owner

`cmd/queue-manager` is the **canonical owner** of NATS JetStream streams.
The api-gateway intentionally does NOT create streams — if you start the
API without queue-manager, the first publish fails with
`no stream matches subject`, which is the operator signal to bring up
queue-manager. This is enforced in
[internal/queue/publisher.go:225](internal/queue/publisher.go#L225) and
documented in the `cmd/queue-manager/main.go` package docstring.

### 2.5 Image pool, but no container pool — and that's deliberate

[internal/sandbox/image_pool.go](internal/sandbox/image_pool.go) ensures
each image is pulled exactly once even with many workers starting
concurrently. But **containers are not pooled** — every submission gets a
fresh container that's destroyed after the job. The trade-off:

- Reusing containers risks state leaks between users (filesystem, env vars,
  leftover processes) → security wins.
- Container start adds ~100–500 ms — small next to actual execution time.
- Cgroup cleanup is automatic when the container exits.

See section 4 of this doc for the per-submission flow.

### 2.6 Configuration via CODERUNTIME_ env prefix (Viper)

Every env var the binary reads is prefixed with `CODERUNTIME_`
([config.go:223](internal/config/config.go#L223)). Anything not prefixed is
**silently ignored**. This caught us multiple times across compose, k8s,
and Helm — see section 7.

### 2.7 Sandbox security defaults

Every sandbox container runs with:

- `--network none` — no internet access from user code
- `--read-only` root filesystem (writes go to `/sandbox` bind mount only)
- Non-root UID (10001) — set in every `runtime-images/*/Dockerfile`
- Memory + CPU caps via cgroups (per-language, in `language.go`)
- PID limit (default 64)
- seccomp default profile via Docker

The bind-mounted scratch dir at `/tmp/sandbox/<uuid>` is created per
submission and `os.RemoveAll`'d when the job ends.

### 2.8 Batches: creation and execution are DECOUPLED

A **batch** (`POST /batches`) groups up to 20 submissions under one
`batch_id` and persists a `batches` row carrying `total` and `completed`
counters ([internal/models/batch.go](internal/models/batch.go)). Each
submission row gets a nullable `batch_id` FK, and the `ExecutionJob` carries
it through the queue so the worker knows which batch a job belongs to.

**Critical invariant: `POST /batches` does NOT start execution.** It persists
the batch + all submissions **atomically** (`BatchRepository.CreateWithSubmissions`,
one tx) in `pending` state and returns `{batch_id, tokens}`. Execution begins
only when the batch is *started* — via `POST /batches/:id/start` or a
`START_BATCH_PROCESSING {batch_id}` event on the SQS start queue
([internal/batchproc/starter.go](internal/batchproc/starter.go)). This exists so
an external orchestrator (e.g. an Assessment Service) can commit its own
token/batch mappings **before** any result webhook can fire — without it,
execution would begin during creation and the completion webhook could race
ahead of the caller's persistence. `Starter.Start` is **idempotent**: it claims
the `pending → processing` transition with a conditional UPDATE
(`MarkProcessing`, returns true once), then fans the submissions out to the job
queue; duplicate START events are no-ops, and a failed fan-out rolls the batch
back to `pending` so the event can be retried. The SQS worker runs **two**
consumers: the per-submission job consumer *and* the batch-start consumer
([internal/queue/sqs/start_consumer.go](internal/queue/sqs/start_consumer.go)).

Completion detection (below) is unchanged — only the *trigger* moved out of
batch creation.

The non-obvious part is **how completion is detected**. There is no poller and
no "batch coordinator" service. Instead, every time a worker finishes a
submission (success *or* terminal failure — see `advanceBatch` in
[internal/worker/worker.go](internal/worker/worker.go)) it calls
`BatchRepository.IncrementCompleted`, which does an **atomic**
`UPDATE … SET completed = completed + 1` plus a `CASE` status transition in a
single transaction and re-reads the row. MySQL/InnoDB row-locking serialises
concurrent workers, so exactly one worker observes `completed == total` and
fires the webhook. This means batch completion is correct even with N workers
× M concurrency racing on the last few submissions, with zero extra
infrastructure.

The webhook itself (`POST /webhooks`) stores a URL plus an outbound `api_key`.
On delivery the platform POSTs the full `BatchResponse` and sets the
`X-API-Key` header to that key so the receiver can authenticate the call —
the key is **outbound**, not used to authenticate registration.

Delivery logic lives in [internal/webhook/dispatcher.go](internal/webhook/dispatcher.go),
deliberately shared by **both** the worker (automatic fire on completion) and
the api-gateway (`POST /batches/:id/callback` re-fires on demand). The
dispatcher assembles the payload from the DB, POSTs it, and records
`webhook_status` / `webhook_sent_at` on the batch regardless of outcome.

**Retries (configurable, `CODERUNTIME_WEBHOOK_*`).** The worker uses
`DispatchBatchWithRetry` — 1 + `WEBHOOK_MAX_RETRIES` attempts with exponential
backoff (`WEBHOOK_RETRY_DELAY` × `WEBHOOK_RETRY_BACKOFF`, capped at
`WEBHOOK_RETRY_MAX_DELAY`); only after all attempts fail is `webhook_status` set
to `failed`. It runs in a goroutine on a **detached context** (background +
`RetryBudget()` timeout) so multi-minute retries neither hold a worker slot nor
get cancelled when the job's context ends. The callback endpoint uses
`DispatchBatch` — a **single** immediate attempt — because the endpoint itself
*is* the manual retry, and the HTTP caller wants a prompt result. The callback
remains the fallback when auto-retries are exhausted (receiver was down the
whole window) without re-running any code.

---

## 3. Mental model — three layers

```
                ┌───────────────────────────────────────────┐
                │     api-gateway   (cmd/api-gateway)       │
                │  REST → publish ExecutionJob to NATS      │
                └─────────────────────┬─────────────────────┘
                                      │
                          NATS JetStream "submissions" stream
                          (topology owned by queue-manager)
                                      │
                ┌─────────────────────▼─────────────────────┐
                │       worker     (cmd/worker)             │
                │   pool of N Workers × M concurrency       │
                │   each Worker pulls a job, then …         │
                └─────────────────────┬─────────────────────┘
                                      │
                ┌─────────────────────▼─────────────────────┐
                │   sandbox.DockerSandbox (sandbox/docker.go)│
                │ - mkdir /tmp/sandbox/<uuid>                │
                │ - write source + stdin                     │
                │ - ImagePool.EnsureImage(lang.Image)        │
                │ - ContainerCreate / Start / Wait / Remove  │
                │ - stream stdout/stderr (size-capped)       │
                │ - rm -rf /tmp/sandbox/<uuid>               │
                └────────────────────────────────────────────┘
```

The **database** branch ([db_runtime.go](internal/runtime/db_runtime.go))
replaces the sandbox container with an ephemeral DB container
(`mysql:8.0`, `postgres:16-alpine`, …), runs the SQL through `mysql`/`psql`,
and returns the result table.

---

## 4. What happens during one submission, end-to-end

1. **HTTP arrives** at `POST /api/v1/submissions` →
   `internal/api/handlers/submission.go`.
2. **Validation** — language exists, source is non-empty, base64-decoded.
3. **DB insert** — `submissions` table, status = `IN_QUEUE`.
4. **NATS publish** — `ExecutionJob` JSON to subject `submissions.execute`.
5. **HTTP response** — if `wait=false`, return submission ID immediately. If
   `wait=true`, the API begins polling Redis for the completed result.
6. **Worker picks up** the job via `nats.QueueSubscribe` with queue group
   `workers` (NATS distributes one message to exactly one subscriber in the
   group → automatic load-balancing across replicas).
7. **Worker semaphore** — each `Worker` has a buffered channel of size
   `CODERUNTIME_WORKER_CONCURRENCY`. Acquire a slot or wait.
8. **Sandbox executes** (see diagram above). Compile step first if the
   language has a `CompileCommand` (`gcc`, `javac`, …).
9. **Worker updates** the MySQL `submissions` row, writes results to Redis
   with TTL, releases the semaphore slot.
10. **API completes** — if it was waiting, Redis poll succeeds and the HTTP
    response is sent. Otherwise the client polls `GET /submissions/<id>`.

Failed jobs that exhaust `CODERUNTIME_WORKER_MAX_RETRIES` (default 3)
get routed to the **Dead Letter Queue** stream and parked by
`queue-manager`'s DLQ handler.

If the submission belongs to a batch (`job.BatchID != ""`), the worker also
runs `advanceBatch` after the result is persisted (step 10b) — atomically
incrementing the batch's `completed` counter and, when it reaches `total`,
dispatching the linked webhook. See section 2.8.

---

## 5. Repository layout — what's where

| Path | What lives here |
|---|---|
| `cmd/api-gateway/` | REST API entrypoint |
| `cmd/worker/` | Worker entrypoint |
| `cmd/queue-manager/` | DLQ + stream-topology owner |
| `cmd/migrate/` | Standalone migrate+seed tool (`make migrate`) — same GORM logic as startup, for CI / pre-deploy against RDS |
| `internal/api/handlers/` | HTTP handlers (one file per resource) |
| `internal/api/middleware/` | JWT, rate-limit, logging, CORS, recovery |
| `internal/config/` | Viper config loading (single source of truth for env vars) |
| `internal/database/` | GORM models, migrations, repositories (incl. `batch_repository.go`) |
| `internal/webhook/` | Batch-completion webhook dispatcher (shared by worker + api) |
| `internal/cache/` | Redis client + `SubmissionCache` |
| `internal/queue/` | Queue abstraction (Publisher, JobConsumer) + NATS client/publisher/consumer/DLQ |
| `internal/queue/sqs/` | Amazon SQS publisher + consumer + DLQ drainer (token-only payload) |
| `internal/runtime/` | `manager.go` routes to sandbox or DB runtime |
| `internal/sandbox/` | Docker sandbox executor (`docker.go`, `image_pool.go`) |
| `internal/worker/` | `Pool` and `Worker` types — semaphore + NATS subscriber |
| `internal/models/language.go` | The 40-entry language registry — image, version, compile/run commands, resource limits |
| `runtime-images/<lang>/Dockerfile` | One per language; built as `code-runtime-<lang>:latest` |
| `docker/Dockerfile.{api,worker,queue-manager}` | Multi-stage builds for the three Go services |
| `docker-compose.yml` | Dev stack (single source of truth for local) |
| `docker-compose.prod.yml` | Prod overrides — swarm-aware |
| `deployments/kubernetes/` | Raw manifests (verbose by design — auditable) |
| `deployments/helm/` | Helm chart wrapping the same manifests |
| `deployments/monitoring/` | Prometheus + Grafana provisioning configs |
| `scripts/pull-images.sh` | Builds all `runtime-images/*` + pre-pulls upstream language images |

---

## 6. Adding a new language — checklist

1. Add an entry in `DefaultLanguages()` in
   [internal/models/language.go](internal/models/language.go) with:
   - `ID` constant
   - `Name`, `Version`, `SourceFile`
   - `CompileCommand` (optional, use `compilePtr(...)`) and `RunCommand`
   - `Image` — set to `code-runtime-<lang>:latest` if you're building locally
     (preferred), or a pinned upstream tag otherwise
   - `MaxMemory` / `MaxCPUTime` — match the language's typical needs
     (Kotlin/Scala/Crystal need ~1 GB+ for the compiler)
2. If you went the local-build route, create
   `runtime-images/<lang>/Dockerfile` matching the patterns of its
   neighbours (Alpine if possible, install minimal toolchain, create
   `sandbox` user, smoke-test in a final `RUN`).
3. If the upstream image has a non-default `ENTRYPOINT` or non-standard
   binary paths, extend the switches in
   [internal/runtime/manager.go](internal/runtime/manager.go) (search for
   `LanguageKotlin` for an example).
4. Run `make pull-images` so the new image is built locally.
5. Submit a test through the API or add a fixture to the integration
   suite (`test/`).
6. Restart the API — `SeedLanguages()` runs an UPSERT on startup, so the
   new row is added without a migration.

---

## 7. Non-obvious conventions ("tribal knowledge")

These are things that bit us in past sessions. Worth knowing up-front.

### CODERUNTIME_ env-var prefix is mandatory

The binary's config loader uses
[Viper with `SetEnvPrefix("CODERUNTIME")`](internal/config/config.go#L223).
Any env var without that prefix is silently ignored. The dev
`docker-compose.yml`, `docker-compose.prod.yml`, k8s manifests, and Helm
chart all correctly prefix env vars now — but if you copy-paste from any
docs/tutorials, **double-check the prefix**.

### Local-built images vs. upstream images

`code-runtime-<lang>:latest` images are NOT on Docker Hub. The worker's
[ImagePool.EnsureImage](internal/sandbox/image_pool.go#L38) will try to
`docker pull` whatever name is in `language.go`. For local images, that
will fail with `pull access denied …`. The only way to get them is
`make pull-images` (or `docker build` against
`runtime-images/<lang>/`). For k8s, pre-bake images into the node AMI or
run a `pull-images` DaemonSet.

### Stream creation is queue-manager's job, not api's

Start order matters: queue-manager → api → worker. Docker Compose
enforces this through `depends_on: queue-manager: service_healthy`. In
k8s, ensure the queue-manager Deployment is up before api/worker, or use
init-containers / Job hooks to gate the rollout.

### `make pull-images` must run before `make compose-up`

Compose only orchestrates the platform services; it doesn't build language
sandbox images. First time on a machine: `make pull-images && make
compose-up`. Otherwise the first submission of each language fails with
`pull access denied`.

### Worker needs Docker socket permission in k8s

In k8s the worker pod runs as UID 10001 with `readOnlyRootFilesystem: true`.
To let it talk to `/var/run/docker.sock` we add
`supplementalGroups: [<docker-gid>]` in
[worker-deployment.yaml](deployments/kubernetes/worker-deployment.yaml).
Common GIDs: 999 (Debian/Ubuntu), 998 (RHEL/CentOS). Run `getent group
docker` on a worker node to confirm.

### `WORKER_COUNT` vs `WORKER_CONCURRENCY`

- `WORKER_COUNT` = number of independent `Worker` subscribers inside one
  process.
- `WORKER_CONCURRENCY` = max in-flight jobs per Worker (semaphore size).
- Total = `COUNT × CONCURRENCY × replicas`. See readme §
  "Capacity planning".

### Grafana admin password is "sticky"

`GF_SECURITY_ADMIN_PASSWORD` is honoured **only on first launch** —
afterwards the password lives in the `grafana_data` volume. To reset:
`docker compose exec grafana grafana-cli admin reset-admin-password <new>`.

### Sandbox writable filesystem is exactly `/sandbox`

User code can write only to `/sandbox` (the bind-mounted scratch dir).
Anywhere else is read-only. Some languages (Go, OCaml, Clojure) need this
spelled out — see the `TMPDIR`/`HOME` overrides in
[internal/runtime/manager.go:284-308](internal/runtime/manager.go#L284-L308).

### `IsActive: false` languages exist intentionally

`language.go` ships ~10 entries with `IsActive: false` (C#, Elixir,
Erlang, Groovy, Haskell, Lisp, Lua, Nim, Python 2, Scala, Swift). Their
images are amd64-only or finicky to package; they're tracked in the
registry so the API surface is complete, but they won't appear in
`/api/v1/languages` until somebody productionises them.

---

## 8. Things to be careful with

- **Disk on worker nodes.** 39 languages × ~150–500 MB ≈ 10–15 GB of
  images. Each running sandbox adds ~10–50 MB of writable layer that's
  reclaimed on container exit. Plan node disk accordingly.
- **`MaxMemory` in `language.go` is in KB**, not bytes — see the
  `*1024` translation in `manager.go`.
- **`MaxCPUTime` is CPU-seconds, not wall-clock.** `WallTimeLimit` is
  set to `MaxCPUTime * 3` by the manager so the worker doesn't kill a
  job that's wall-time-blocked on IO.
- **`network: none` means no DNS either.** If a language needs to fetch
  packages at execution time (rare — most prefer pre-installed packages),
  it must be done at image build time in the Dockerfile.
- **MySQL `max_connections` cap (200 in compose; RDS default scales with
  instance class).** At ~5+ api pods with `database.max_open_conns: 25` you'll
  hit this. Use RDS Proxy / ProxySQL or raise the RDS `max_connections`
  parameter before scaling api beyond that.
- **MySQL migration constraints.** The app DB is MySQL (driver
  `gorm.io/driver/mysql`, see [internal/database/mysql.go](internal/database/mysql.go)).
  Unlike Postgres, MySQL has **no `CREATE INDEX IF NOT EXISTS`** and **no
  partial (`WHERE`) indexes**, so [migrations.go](internal/database/migrations.go)
  creates each secondary index via an `ensureIndex` helper that checks
  `information_schema.statistics` first and drops all partial predicates.
  `DATABASE_SSL_MODE` maps to the driver's `tls` param
  (`disable`/`true`/`skip-verify`/`preferred`) — use `true` for AWS RDS. The
  DSN needs `parseTime=true` (set in `mysql.go`) or `time.Time` columns fail to
  scan.
- **Redis / AWS ElastiCache.** Standalone client, no AUTH token in prod (leave
  `REDIS_PASSWORD` empty). ElastiCache **Serverless** (Valkey/Redis) only accepts
  **TLS** connections — set `CODERUNTIME_REDIS_TLS_ENABLED=true`
  ([config wiring](internal/config/config.go) → [cache client](internal/cache/redis.go)),
  otherwise the connection hangs/resets. The serverless endpoint
  (`*.serverless.<region>.cache.amazonaws.com`) resolves to **private VPC IPs**,
  so it's only reachable from inside the VPC — you can't smoke-test it from a
  laptop, only from the EC2/ECS host running the stack. Compose defaults to the
  local `redis` container (`REDIS_HOST=${REDIS_HOST:-redis}`); override
  `REDIS_HOST` + `REDIS_TLS_ENABLED=true` to point at ElastiCache.
- **Pluggable job queue: NATS or SQS.** `CODERUNTIME_QUEUE_PROVIDER` selects
  the backend (`nats` default / `sqs`). Both implement `queue.Publisher` and
  `queue.JobConsumer` ([internal/queue/transport.go](internal/queue/transport.go)),
  so api-gateway and worker swap implementations behind the interface — handlers
  are untouched. Non-obvious bits of the SQS path
  ([internal/queue/sqs/](internal/queue/sqs/)):
  - **Token-only messages.** SQS caps payloads at 256 KB but `ExecutionJob`
    embeds source code, so SQS publishes only `{token}` and the worker re-reads
    the job from MySQL via a `JobLoader`. (NATS still carries the full job.)
  - **Priority = two queues.** SQS has no priority; the consumer polls the
    priority queue (short long-poll) before the normal queue.
  - **Retries/DLQ are the SQS redrive policy**, not `dlq.go`. Retriable errors
    `ChangeMessageVisibility` for backoff; after `maxReceiveCount` SQS moves the
    message to the DLQ. When `provider=sqs`, queue-manager skips NATS entirely
    and instead runs an SQS **DLQ drainer** (marks dead-lettered submissions
    failed in MySQL) + a queue-depth metrics poller.
  - **Credentials** come from the standard AWS chain (IAM task/instance role) —
    never configured in the app.
- **`done:{token}` notifications are Redis, not NATS.** The worker publishes
  completion to the Redis `done:{token}` channel (`subCache.PublishDone`), which
  is what `?wait=true` waiters subscribe to ([internal/queue/wait.go](internal/queue/wait.go)).
  This keeps the wait path independent of the queue backend, so the worker needs
  no NATS connection under SQS. (Historically the worker published this to NATS,
  which nothing consumed — the waiter always used Redis.)
- **`go.sum` is enforced.** The Go Dockerfiles run `go mod verify` —
  modules added without `go mod tidy + verify` will break the image build.

---

## 9. Useful entry points when investigating

| To understand … | Open … |
|---|---|
| What languages exist and how each is run | [internal/models/language.go](internal/models/language.go) |
| How a job goes from HTTP to a running container | [internal/api/handlers/submission.go](internal/api/handlers/submission.go) → [internal/queue/publisher.go](internal/queue/publisher.go) → [internal/worker/worker.go](internal/worker/worker.go) → [internal/runtime/manager.go](internal/runtime/manager.go) → [internal/sandbox/docker.go](internal/sandbox/docker.go) |
| Why a language has `Entrypoint: []` or a custom PATH | [internal/runtime/manager.go](internal/runtime/manager.go) — search for the language ID |
| How DB languages differ from regular ones | [internal/runtime/db_runtime.go](internal/runtime/db_runtime.go) |
| Where retries / DLQ are implemented | [internal/queue/dlq.go](internal/queue/dlq.go) + [cmd/queue-manager/main.go](cmd/queue-manager/main.go) |
| Where seccomp / capabilities / cgroups are applied | [internal/sandbox/docker.go](internal/sandbox/docker.go) — search for `HostConfig` |
| Where prometheus metrics are defined | [internal/metrics/](internal/metrics/) |
| Why the worker can write to the Docker socket in k8s | [deployments/kubernetes/worker-deployment.yaml](deployments/kubernetes/worker-deployment.yaml) — `supplementalGroups` |

---

## 10. Related docs

- [readme.md](readme.md) — feature list, quick-start, API reference, full env-var table
- [deployments/DEPLOYMENT.md](deployments/DEPLOYMENT.md) — compose / k8s / Helm deployment guides + production checklist
- [scripts/pull-images.sh](scripts/pull-images.sh) — what gets built and pulled when you bootstrap
