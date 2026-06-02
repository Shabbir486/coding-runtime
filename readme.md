# Code Runtime

**A Production-Grade Distributed Code Execution Engine**

> Judge0-inspired, written in Go — execute untrusted code safely across 39 languages using Docker sandboxes, NATS JetStream, PostgreSQL, and Redis.

---

## Table of Contents

- [Architecture](#architecture)
- [Request Lifecycle](#request-lifecycle)
- [Project Layout](#project-layout)
- [Features](#features)
- [Prerequisites](#prerequisites)
- [Quick Start](#quick-start)
- [API Reference](#api-reference)
- [Language Support](#language-support)
- [Execution Status Codes](#execution-status-codes)
- [Configuration Reference](#configuration-reference)
- [Security](#security)
- [Monitoring](#monitoring)
- [Performance Targets](#performance-targets)
- [Development](#development)

---

## Architecture

```
                              ┌──────────────────────────────────────────────────┐
                              │                  Code Runtime                     │
                              │                                                    │
  HTTP Client                 │   ┌────────────────────────────────────────────┐  │
      │                       │   │              API Service (Gin)              │  │
      │  POST /submissions     │   │                                            │  │
      │  GET  /submissions/:t  │   │  Routes → JWT Middleware → Handlers        │  │
      └──────────────────────►│   │                    │                       │  │
                              │   │         ┌──────────┼──────────┐            │  │
                              │   │         ▼          ▼          ▼            │  │
                              │   │    PostgreSQL    Redis    NATS JetStream    │  │
                              │   │    (persist)   (cache/   (SUBMISSIONS       │  │
                              │   │                pub-sub)   stream)           │  │
                              │   └────────────────────┬───────────────────────┘  │
                              │                        │                           │
                              │              ┌─────────▼──────────┐               │
                              │              │  Worker Service     │  (N replicas) │
                              │              │                     │               │
                              │              │  NATS Consumer      │               │
                              │              │       │             │               │
                              │              │       ▼             │               │
                              │              │  Fetch Language     │               │
                              │              │  (PostgreSQL cache) │               │
                              │              │       │             │               │
                              │              │   is_database?      │               │
                              │              │    ╱         ╲      │               │
                              │              │  YES          NO    │               │
                              │              │   ▼            ▼    │               │
                              │              │ DBRuntime   Sandbox │               │
                              │              │ (MySQL/     (lang   │               │
                              │              │  PG/SQLite/ runtime │               │
                              │              │  Mongo)     image)  │               │
                              │              │       │             │               │
                              │              │       ▼             │               │
                              │              │  Write Result       │               │
                              │              │  PostgreSQL +       │               │
                              │              │  Redis pub/sub      │               │
                              │              └─────────────────────┘               │
                              └──────────────────────────────────────────────────┘
```

### Components

| Component | Technology | Role |
|-----------|-----------|------|
| **API Service** | Go + Gin | Validates requests, persists submissions, publishes to NATS, serves results from Redis cache |
| **Worker Service** | Go | Consumes NATS jobs, routes to Sandbox or DBRuntime, writes results back |
| **Sandbox** | Docker API | Ephemeral, isolated containers for general language execution (CPU, memory, PID limits) |
| **DBRuntime** | Docker API | Purpose-built runner for database languages (MySQL, PostgreSQL, SQLite, MongoDB) — spins up full DB engine containers |
| **NATS JetStream** | NATS 2.10 | Durable at-least-once message queue; `SUBMISSIONS` stream with dead-letter consumer |
| **PostgreSQL** | PG 16 | Source of truth for submissions and language config; GORM AutoMigrate on startup |
| **Redis** | Redis 7 | Submission result cache (TTL-based) + pub/sub channel for `?wait=true` long-polling |

---

## Request Lifecycle

### Asynchronous (`?wait=false`, default)

```
Client                API                  NATS             Worker         Docker
  │                    │                    │                 │               │
  │  POST /submissions │                    │                 │               │
  │───────────────────►│                    │                 │               │
  │                    │ INSERT submissions  │                 │               │
  │                    │ (status=In Queue)  │                 │               │
  │                    │ Seed Redis cache   │                 │               │
  │                    │ PublishJob ────────►                 │               │
  │  { token }         │                    │                 │               │
  │◄───────────────────│                    │                 │               │
  │                    │                    │ Consume job ────►               │
  │                    │                    │                 │ Spin up ctr ──►
  │                    │                    │                 │               │
  │                    │                    │                 │◄── stdout/err─│
  │                    │                    │                 │ Kill + remove │
  │                    │                    │                 │               │
  │                    │◄─────────────────── UPDATE + cache   │               │
  │                    │                    │                 │               │
  │  GET /submissions/:token                │                 │               │
  │───────────────────►│                    │                 │               │
  │  { result }        │ Redis hit          │                 │               │
  │◄───────────────────│                    │                 │               │
```

### Synchronous (`?wait=true`)

1. Same as above through `PublishJob`.
2. API subscribes to a Redis pub/sub channel keyed to the submission token.
3. API blocks, polling Redis with a 120-second timeout.
4. Worker publishes to that channel when the job completes.
5. API responds immediately with the full result — no client polling required.

### Database Submissions (MySQL, PostgreSQL, SQLite)

The worker detects `is_database = true` on the language row and routes to **DBRuntime** instead of the standard sandbox:

- **SQLite**: runs `sqlite3` directly inside the worker's `alpine:3.19` container.
- **MySQL**: starts an ephemeral `mysql:8.0` container, waits for `mysqladmin ping`, runs the SQL, then removes the container.
- **PostgreSQL**: starts an ephemeral `postgres:16-alpine` container as the `postgres` user, runs `initdb` + `pg_ctl`, executes the SQL, then removes the container.
- All DB containers run with `--network=none`, a 256-PID limit, and a memory cap.

---

## Project Layout

```
.
├── cmd/
│   ├── api-gateway/    # REST API entrypoint (main.go) — built by Makefile + docker/Dockerfile.api
│   ├── worker/         # Job-executing worker entrypoint — built by Makefile + docker/Dockerfile.worker
│   └── queue-manager/  # Optional DLQ processor + JetStream bootstrapper — built by Makefile + docker/Dockerfile.queue-manager
├── internal/
│   ├── api/
│   │   ├── handlers/   # HTTP handler functions (submission, language, health, auth)
│   │   ├── middleware/  # JWT auth, rate limiting, logging, CORS, recovery
│   │   └── router.go   # Gin route registration
│   ├── cache/          # Redis client, SubmissionCache (Get/Set/Delete/PollForCompletion)
│   ├── config/         # Viper-based config loading (env vars + config.yaml)
│   ├── database/       # PostgreSQL client (GORM), AutoMigrate, SubmissionRepository
│   ├── metrics/        # Prometheus counter/histogram/gauge definitions (APIMetrics, WorkerMetrics)
│   ├── models/         # Domain types: Language, Submission, ExecutionJob, status constants
│   │                   # DefaultLanguages() + SeedLanguages() (39 languages)
│   ├── queue/          # NATS JetStream publisher and consumer; DLQ handling
│   ├── runtime/
│   │   ├── manager.go  # Routes jobs to sandbox or DBRuntime based on language.IsDatabase
│   │   └── db_runtime.go # Ephemeral DB container execution (MySQL, PostgreSQL, SQLite, MongoDB)
│   ├── sandbox/        # Docker sandbox executor: container lifecycle, resource limits, seccomp
│   ├── tracing/        # OpenTelemetry setup (OTLP HTTP exporter)
│   └── worker/         # Worker pool, job processor, retry logic
├── runtime-images/     # Dockerfiles for custom language images
│   ├── coffeescript/   # → code-runtime-coffeescript:latest  (Node.js + coffeescript@2.7.0)
│   ├── cpp/            # → code-runtime-cpp:latest
│   ├── d/              # → code-runtime-d:latest             (Alpine + LDC2/LLVM D compiler)
│   ├── golang/         # → code-runtime-golang:latest
│   ├── java/           # → code-runtime-java:latest
│   ├── kotlin/         # → code-runtime-kotlin:latest
│   ├── nodejs/         # → code-runtime-nodejs:latest        (Node.js 22 + tsx + TypeScript)
│   ├── objc/           # → code-runtime-objc:latest          (Ubuntu 22.04 + GNUstep + gobjc)
│   ├── python/         # → code-runtime-python:latest
│   ├── r/              # → code-runtime-r:latest
│   ├── ruby/           # → code-runtime-ruby:latest
│   ├── rust/           # → code-runtime-rust:latest
│   └── swift/          # → code-runtime-swift:latest
├── scripts/
│   ├── init.sql        # PostgreSQL init: extensions + grants (runs once on container creation)
│   ├── migrate.sh      # Applies supplementary SQL migrations from scripts/migrations/
│   ├── pull-images.sh  # Builds all code-runtime-* images; pulls official language images
│   ├── seed.sh         # Verifies language seeding (API auto-seeds on startup)
│   ├── setup.sh        # Full dev-env bootstrap: prereqs → images → compose → verify
│   └── test_languages.py  # Integration test: submits Hello World for all 39 languages
├── deployments/
│   ├── helm/           # Helm chart for Kubernetes
│   ├── kubernetes/     # Raw K8s manifests
│   └── monitoring/     # Prometheus scrape configs + Grafana dashboard JSON
├── config/             # config.yaml defaults
├── docker-compose.yml  # Full local stack (api, worker, postgres, redis, nats, prometheus, grafana, jaeger)
├── Makefile
└── go.mod
```

---

## Features

- **39 languages** — Bash, C, C++, C#, Clojure, COBOL, CoffeeScript, Crystal, D, Elixir, Erlang, Fortran, Go, Groovy, Haskell, Java, JavaScript, Kotlin, Common Lisp, Lua, Nim, Objective-C, OCaml, Pascal, Perl, PHP, Prolog, Python 2, Python 3, R, Ruby, Rust, Scala, Swift, TypeScript, VB.Net, SQLite, MySQL, PostgreSQL
- **Docker sandboxing** — isolated, network-disabled containers with CPU, memory, PID, and file-size limits enforced by the Linux kernel
- **Database execution** — ephemeral MySQL 8.0, PostgreSQL 16, and SQLite containers for SQL submissions; MongoDB support via mongosh
- **NATS JetStream queue** — durable at-least-once delivery with dead-letter queue and configurable redelivery
- **Synchronous and asynchronous modes** — `?wait=true` blocks via Redis pub/sub; `?wait=false` returns a token for polling
- **Batch submissions** — up to 20 submissions in a single HTTP call
- **JWT authentication** — bearer-token auth on all submission endpoints
- **Redis caching** — submission results cached with configurable TTL to reduce database load
- **Prometheus metrics** — counters, histograms, and gauges for requests, queue depth, execution duration, and container lifecycle
- **OpenTelemetry tracing** — distributed traces exported via OTLP HTTP to Jaeger or any compatible backend
- **Structured JSON logging** — via `go.uber.org/zap`
- **Graceful shutdown** — in-flight requests and running jobs drain before process exit
- **Pre-warmed image pool** — worker pulls and caches runtime images on startup to minimise cold-start latency
- **Auto-seeding** — `SeedLanguages()` upserts all 39 language rows on every API restart using raw `ON CONFLICT DO UPDATE` SQL, ensuring config changes take effect immediately

---

## Prerequisites

| Tool | Version | Notes |
|------|---------|-------|
| Go | 1.24+ | `go version` |
| Docker | 24+ | daemon must be running |
| Docker Compose | v2.x | included with Docker Desktop |
| make | any | GNU or BSD make |

---

## Quick Start

```bash
# 1. Clone
git clone https://github.com/mdshabbir-ali/code-runtime.git
cd code-runtime

# 2. Full setup (builds images, starts services, verifies health)
bash scripts/setup.sh

# 3. Get a JWT token
TOKEN=$(curl -s -X POST http://localhost:8002/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"admin123"}' | jq -r .access_token)

# 4. Submit Python code (source_code is base64 of: print("Hello, World!"))
curl -s -X POST "http://localhost:8002/submissions?wait=true" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"language_id":29,"source_code":"cHJpbnQoIkhlbGxvLCBXb3JsZCEiKQ=="}' | jq .
```

Expected response:

```json
{
  "token": "550e8400-e29b-41d4-a716-446655440000",
  "status": { "id": 3, "description": "Accepted" },
  "stdout": "SGVsbG8sIFdvcmxkIQo=",
  "time": 0.042,
  "memory": 8192
}
```

```bash
# 5. Run integration tests for all 39 languages
python3 scripts/test_languages.py
```

---

## API Reference

All `/submissions` and `/auth/apikey` endpoints require `Authorization: Bearer <token>`.

### Authentication

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/auth/token` | None | Issue a short-lived JWT (email + password) |
| `POST` | `/auth/apikey` | Bearer | Create a long-lived API key |

### Submissions

| Method | Path | Auth | Query Params | Description |
|--------|------|------|-------------|-------------|
| `POST` | `/submissions` | Bearer | `wait=true\|false` | Create a single submission |
| `GET` | `/submissions` | Bearer | `page`, `per_page` | List paginated submissions |
| `GET` | `/submissions/:token` | Bearer | — | Get submission by UUID token |
| `DELETE` | `/submissions/:token` | Bearer | — | Delete a submission |
| `POST` | `/submissions/batch` | Bearer | — | Create up to 20 submissions |

### Languages & Statuses

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/languages` | None | List all supported languages |
| `GET` | `/languages/:id` | None | Get a language by ID |
| `GET` | `/statuses` | None | List execution status codes |

### Health & Observability

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/health` | None | Liveness: checks PostgreSQL, Redis, NATS connectivity |
| `GET` | `/readyz` | None | Readiness: returns 200 only when fully initialised |
| `GET` | `/metrics` | None | Prometheus metrics |

### Request Body: `POST /submissions`

```json
{
  "language_id":         29,
  "source_code":         "<base64>",
  "stdin":               "<base64>",
  "expected_output":     "<base64>",
  "cpu_time_limit":      5.0,
  "wall_time_limit":     10.0,
  "memory_limit":        262144,
  "stack_limit":         65536,
  "max_processes":       60,
  "max_file_size":       4096,
  "compiler_options":    "",
  "command_line_arguments": "",
  "callback_url":        "https://your-server.com/webhook",
  "wait":                false
}
```

`source_code`, `stdin`, and `expected_output` accept both plain text and standard Base64 — the API attempts to decode and falls back to plain text automatically.

### Response: `SubmissionResponse`

| Field | Type | Description |
|-------|------|-------------|
| `token` | string | UUID identifying this submission |
| `status` | object | `{ "id": N, "description": "..." }` |
| `stdout` | string\|null | Program standard output |
| `stderr` | string\|null | Program standard error |
| `compile_output` | string\|null | Compiler output (compiled languages only) |
| `exit_code` | int\|null | Process exit code |
| `time` | float\|null | CPU time in seconds |
| `wall_time` | float\|null | Wall-clock time in seconds |
| `memory` | float\|null | Peak memory in kilobytes |
| `source_code` | string\|null | Echo of submitted source (only on `?wait=true`) |
| `stdin` | string\|null | Echo of submitted stdin (only on `?wait=true`) |
| `created_at` | string | ISO 8601 |
| `finished_at` | string\|null | ISO 8601 |

---

## Language Support

Language IDs match the constants in [internal/models/language.go](internal/models/language.go). Custom images are built from [runtime-images/](runtime-images/). The ✓/✗ in the Active column reflects the current `is_active` flag seeded at startup.

| ID | Language | Version | Docker Image | Compiled | Active |
|----|----------|---------|-------------|---------|--------|
| 1 | Bash | 5.2 | `bash:5.2-alpine3.19` | No | ✓ |
| 2 | C (GCC) | 13.3 | `gcc:13.3` | Yes (`gcc`) | ✓ |
| 3 | C++ (GCC) | 13.3 | `gcc:13.3` | Yes (`g++ -std=c++17`) | ✓ |
| 4 | C# (Mono) | 6.12.0 | `mono:6.12` | Yes (`mcs`) | ✗ |
| 5 | Clojure | 1.11.1 | `clojure:temurin-21-tools-deps` | No | ✓ |
| 6 | COBOL (GnuCOBOL) | 3.1.2 | `dagui0/gnucobol:latest` | Yes (`cobc`) | ✓ |
| 7 | CoffeeScript | 2.7.0 | `code-runtime-coffeescript:latest` | No | ✓ |
| 8 | Crystal | 1.14 | `crystallang/crystal:1.14` | Yes (`crystal build`) | ✓ |
| 9 | D (LDC2) | 1.33.0 | `code-runtime-d:latest` | Yes (`ldc2`) | ✓ |
| 10 | Elixir | 1.15.4 | `elixir:1.15-alpine` | No | ✗ |
| 11 | Erlang (OTP) | 26.0 | `erlang:26-alpine` | Yes (`erlc`) | ✗ |
| 12 | Fortran (GFortran) | 13.3 | `gcc:13.3` | Yes (`gfortran`) | ✓ |
| 13 | Go | 1.24 | `golang:1.24-alpine` | Yes (`go build`) | ✓ |
| 14 | Groovy | 4.0.15 | `groovy:4.0-jdk21` | No | ✗ |
| 15 | Haskell (GHC) | 9.4.5 | `haskell:9.4` | No | ✗ |
| 16 | Java (OpenJDK) | 21 | `eclipse-temurin:21-jdk` | Yes (`javac`) | ✓ |
| 17 | JavaScript (Node.js) | 22 | `node:22-alpine` | No | ✓ |
| 18 | Kotlin | 2.3.21 | `gmazzo/kotlin:latest` | Yes (`kotlinc`) | ✓ |
| 19 | Common Lisp (SBCL) | 2.3.9 | `fukamachi/sbcl` | No | ✗ |
| 20 | Lua | 5.4 | `nickblah/lua:5.4-alpine` | No | ✗ |
| 21 | Nim | 2.0.2 | `nimlang/nim:2.0.2` | Yes (`nim c`) | ✗ |
| 22 | Objective-C (GCC / GNUstep) | 11.4 | `code-runtime-objc:latest` | Yes (`gcc` + GNUstep) | ✓ |
| 23 | OCaml | 5.4 | `ocaml/opam:alpine` | Yes (`ocamlopt`) | ✓ |
| 24 | Pascal (FPC) | 3.2.2 | `primeimages/freepascal:latest` | Yes (`fpc`) | ✓ |
| 25 | Perl | 5.38 | `perl:5.38-slim` | No | ✓ |
| 26 | PHP | 8.3 | `php:8.3-cli-alpine` | No | ✓ |
| 27 | Prolog (SWI-Prolog) | 10.0.2 | `swipl:stable` | No | ✓ |
| 28 | Python 2 | 2.7.18 | `python:2.7-slim` | No | ✗ |
| 29 | Python 3 | 3.12 | `python:3.12-slim` | No | ✓ |
| 30 | R | 4.3.2 | `r-base:4.3.2` | No | ✗ |
| 31 | Ruby | 3.3 | `ruby:3.3-alpine` | No | ✓ |
| 32 | Rust | 1.82 | `rust:1.82-alpine` | Yes (`rustc`) | ✓ |
| 33 | Scala | 3.3.1 | `hseeberger/scala-sbt:17.0.2_1.6.2_3.1.1` | No | ✗ |
| 34 | Swift | 5.9.2 | `swift:5.9` | No | ✗ |
| 35 | TypeScript | 5.6.3 | `code-runtime-nodejs:latest` | No (`npx tsx`) | ✓ |
| 36 | Visual Basic.Net (Mono) | 6.12.0 | `mono:6.12` | Yes (`vbnc`) | ✓ |
| 37 | SQLite | 3.53.0 | `keinos/sqlite3:latest` | No | ✓ |
| 38 | MySQL | 8.0 | `mysql:8.0` | No | ✓ |
| 39 | PostgreSQL | 16 | `postgres:16-alpine` | No | ✓ |

### Custom image notes

| Image | Base | Key packages |
|-------|------|-------------|
| `code-runtime-coffeescript:latest` | `node:22-alpine` | `coffeescript@2.7.0` globally installed |
| `code-runtime-d:latest` | `alpine:3.19` | `ldc` (LDC2 1.33.0 — LLVM D compiler, native arm64) |
| `code-runtime-nodejs:latest` | `node:22-alpine` | `tsx@4.x`, `typescript@5.6.3`, `ts-node` globally installed |
| `code-runtime-objc:latest` | `ubuntu:22.04` | `gobjc`, `libgnustep-base-dev`, `gnustep-base-runtime` |

---

## Execution Status Codes

| ID | Status | Description |
|----|--------|-------------|
| 1 | In Queue | Submission accepted, pending worker pickup |
| 2 | Processing | Worker is actively executing the submission |
| 3 | Accepted | Execution completed; output matched `expected_output` (or none was set) |
| 4 | Wrong Answer | Output did not match `expected_output` |
| 5 | Time Limit Exceeded | CPU or wall time exceeded the configured limit |
| 6 | Compilation Error | Compiler exited non-zero; check `compile_output` |
| 7 | Runtime Error | Process exited with a non-zero code (SIGSEGV, assert, etc.) |
| 8 | Internal Error | Platform-level error (Docker daemon, NATS, database) |

---

## Configuration Reference

Configuration is loaded by Viper: `config/config.yaml` → environment variables (env takes precedence).

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVER_HOST` | `0.0.0.0` | Bind address |
| `SERVER_PORT` | `8002` | HTTP port |
| `SERVER_MODE` | `release` | Gin mode: `debug`, `release`, `test` |
| `SERVER_READ_TIMEOUT` | `30s` | Maximum duration for reading a request |
| `SERVER_WRITE_TIMEOUT` | `130s` | Maximum duration for writing a response (must exceed wait timeout) |
| `SERVER_SHUTDOWN_TIMEOUT` | `30s` | Graceful shutdown drain period |

### Database

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_HOST` | `localhost` | PostgreSQL host |
| `DATABASE_PORT` | `5432` | PostgreSQL port |
| `DATABASE_USER` | `coderuntime` | Database user |
| `DATABASE_PASSWORD` | `coderuntime123` | Database password |
| `DATABASE_NAME` | `coderuntime` | Database name |
| `DATABASE_SSL_MODE` | `disable` | `disable`, `require`, `verify-full` |
| `DATABASE_MAX_OPEN_CONNS` | `25` | Connection pool size |
| `DATABASE_MAX_IDLE_CONNS` | `10` | Idle connections kept open |
| `DATABASE_CONN_MAX_LIFETIME` | `30m` | Maximum connection lifetime |

### Redis

| Variable | Default | Description |
|----------|---------|-------------|
| `REDIS_HOST` | `localhost` | Redis host |
| `REDIS_PORT` | `6379` | Redis port |
| `REDIS_PASSWORD` | _(empty)_ | Redis auth password |
| `REDIS_DB` | `0` | Redis database index |
| `REDIS_POOL_SIZE` | `20` | Connection pool size |

### NATS JetStream

| Variable | Default | Description |
|----------|---------|-------------|
| `NATS_URL` | `nats://localhost:4222` | NATS server URL |
| `NATS_SUBMISSION_STREAM` | `SUBMISSIONS` | JetStream stream name |
| `NATS_WORKER_SUBJECT` | `worker.execute` | Subject workers consume |
| `NATS_MAX_RECONNECTS` | `-1` | Maximum reconnection attempts (-1 = unlimited) |
| `NATS_RECONNECT_WAIT` | `2s` | Delay between reconnection attempts |

### Worker

All variables are read by Viper with the `CODERUNTIME_` prefix (e.g.
`CODERUNTIME_WORKER_COUNT`). The short names below are the viper keys.

| Variable | Default | Description |
|----------|---------|-------------|
| `WORKER_COUNT` | `4` | Number of independent `Worker` instances created **inside one worker process**. Each Worker subscribes to NATS on its own. |
| `WORKER_CONCURRENCY` | `4` | Per-Worker semaphore size — max in-flight jobs (goroutines) one Worker will handle at once. |
| `WORKER_MAX_RETRIES` | `3` | Retry attempts before routing to DLQ |
| `WORKER_JOB_TIMEOUT` | `120s` | Hard wall-time cap per job (must match API `waitTimeout`) |
| `WORKER_TMP_DIR` | `/tmp/sandbox` | Host path bind-mounted into ephemeral DB containers |

**Concurrency formula:**

```
max concurrent jobs cluster-wide = WORKER_COUNT × WORKER_CONCURRENCY × <worker replicas>
```

For the dev compose defaults (`COUNT=4`, `CONCURRENCY=4`, `replicas: 2`)
that's **32 concurrent sandbox executions**. See
[Capacity planning](#capacity-planning) for prod-scale tuning.

### Docker

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket |
| `DOCKER_NETWORK_MODE` | `none` | Container network isolation |
| `DOCKER_CPU_PERIOD` | `100000` | CPU CFS period in microseconds |
| `DOCKER_CPU_QUOTA` | `50000` | CPU CFS quota (50% of one core) |
| `DOCKER_PIDS_LIMIT` | `64` | Maximum processes per sandbox container |

### JWT

| Variable | Default | Description |
|----------|---------|-------------|
| `JWT_SECRET` | _(change in prod)_ | HMAC-SHA256 signing key (min 32 chars) |
| `JWT_ACCESS_TOKEN_EXP` | `15m` | Access token lifetime |

### Metrics & Tracing

| Variable | Default | Description |
|----------|---------|-------------|
| `METRICS_ENABLED` | `true` | Expose Prometheus `/metrics` |
| `TRACING_ENABLED` | `false` | Enable OpenTelemetry tracing |
| `TRACING_ENDPOINT` | `http://localhost:4318` | OTLP HTTP exporter endpoint |
| `TRACING_SAMPLE_RATE` | `1.0` | Trace sample rate (0.0–1.0) |

---

## Security

### Container isolation

Each sandbox container runs with:

- **No network** — `NetworkMode: none`, `NetworkDisabled: true`
- **Read-only filesystem** — source file written to `/sandbox` before execution
- **Resource limits** — `Memory`, `MemorySwap`, `CPUQuota`, `PidsLimit`, `MaxFileSize` all enforced by the Linux kernel
- **Dropped capabilities** — no capability grants beyond the Docker default
- **Seccomp** — custom allowlist profile restricts to the minimum syscall set needed for code execution

### API security

- Short-lived JWT tokens (15 minutes default)
- Per-IP rate limiting via Redis sliding window
- Secure HTTP headers middleware (`X-Frame-Options`, `X-Content-Type-Options`, `X-XSS-Protection`)
- CORS configured to allowed origins only

### Production checklist

- [ ] Set a strong `JWT_SECRET` (minimum 32 random bytes: `openssl rand -base64 32`)
- [ ] Set `DATABASE_SSL_MODE=require` or `verify-full`
- [ ] Set a Redis `REDIS_PASSWORD`
- [ ] Set `SERVER_MODE=release`
- [ ] Run behind a TLS-terminating reverse proxy (nginx, Caddy, ALB)
- [ ] Restrict the Docker socket (`/var/run/docker.sock`) to the `docker` group
- [ ] Enable Kubernetes Network Policies to isolate worker pods from each other

---

## Monitoring

All observability services are included in `docker-compose.yml` and start automatically.

| Service | URL | Credentials |
|---------|-----|-------------|
| Prometheus | http://localhost:9090 | — |
| Grafana | http://localhost:3000 | admin / admin |
| Jaeger (traces) | http://localhost:16686 | — |
| NATS monitoring | http://localhost:8222 | — |

### Key Prometheus metrics

| Metric | Type | Description |
|--------|------|-------------|
| `code_runtime_http_requests_total` | Counter | HTTP requests by method, path, status |
| `code_runtime_http_request_duration_seconds` | Histogram | Request latency |
| `code_runtime_submissions_total` | Counter | Submissions created |
| `code_runtime_submissions_batch_total` | Counter | Batch submission calls |
| `code_runtime_executions_total` | Counter | Completed executions by language and status |
| `code_runtime_execution_duration_seconds` | Histogram | Container wall time |
| `code_runtime_queue_publish_errors_total` | Counter | Failed NATS publish attempts |
| `code_runtime_cache_hits_total` | Counter | Redis cache hits by entity type |
| `code_runtime_cache_misses_total` | Counter | Redis cache misses by entity type |

---

## Performance Targets

| Metric | Target |
|--------|--------|
| API p95 latency (submit, `wait=false`) | < 20 ms |
| End-to-end p95 (Python, `wait=true`) | < 3 s |
| End-to-end p95 (Java, `wait=true`) | < 8 s |
| End-to-end p95 (MySQL, `wait=true`) | < 30 s |
| Container startup overhead (pre-warmed) | < 500 ms |
| Concurrent executions per worker process (`WORKER_COUNT × WORKER_CONCURRENCY`) | 16 (defaults) |

---

## Capacity planning

This is a single-formula service: **every code submission spawns one Docker
sandbox container**. Tuning is mostly about how many of those you can run at
once and how fast you can recycle them.

### The throughput formula

```
peak concurrent sandboxes = WORKER_COUNT × WORKER_CONCURRENCY × replicas
peak submissions/sec      ≈ peak concurrent sandboxes ÷ avg job duration
```

Example: 8 worker pods × `COUNT=8` × `CONCURRENCY=4` = 256 concurrent
sandboxes. If the average job runs for ~2 s, that's ~128 submissions/sec
sustained, with bursts higher absorbed by NATS JetStream.

### Recommended tiers

`WORKER_COUNT` is essentially the number of NATS subscriptions per process —
more = better message-pull parallelism, but each one costs a few MB. Keep it
near the pod's CPU count.

`WORKER_CONCURRENCY` is the semaphore that controls how many Docker
containers a single Worker creates simultaneously. The hard ceiling is your
node's CPU/RAM; sandboxes typically use ~0.5–1 vCPU and 256 MB–1 GB each.

| Tier | Audience | `WORKER_COUNT` | `WORKER_CONCURRENCY` | worker replicas | Cluster-wide concurrent jobs |
|------|----------|---------------:|---------------------:|----------------:|-----------------------------:|
| **Dev / local** | 1–10 users | 4  | 4 | 2 (compose)        | ~32 |
| **Small prod** | up to ~10 K MAU | 8  | 4 | 4 (HPA min)        | ~128 |
| **Mid prod** | up to ~100 K MAU | 8  | 8 | 8 (HPA min, max 20) | ~512 |
| **Large prod (millions MAU)** | 1 M+ MAU, contest spikes | 16 | 8 | HPA min 8, max 100 | up to ~12 800 |

These numbers assume each worker pod has dedicated CPU/RAM proportional to
its concurrency. For the large-prod row, plan ~16 vCPU and ~32 GB RAM per
worker node and pin one worker pod per node.

### What else has to scale with worker concurrency

Workers are rarely the first thing to break. The real ceilings at scale are
elsewhere — keep these in sync:

1. **Postgres `max_connections`.** Each api pod opens up to
   `database.max_open_conns` (default 25). At 20 api pods that's 500 — the
   default server cap is 200. Add [PgBouncer](https://www.pgbouncer.org)
   (transaction pooling) in front of Postgres above ~5 api pods, or you'll
   see `too many connections` long before you hit your worker ceiling.

2. **Docker daemon throughput per node.** A single Docker daemon
   comfortably starts ~50 containers/sec, but spinning up 100+ simultaneously
   on one node will saturate node CPU/IO. Use the existing
   `topologySpreadConstraints` in [worker-deployment.yaml](deployments/kubernetes/worker-deployment.yaml)
   to keep workers spread across nodes, and pre-pull all language images
   onto every node (DaemonSet or node-image bake).

3. **NATS JetStream.** A single-node JetStream comfortably handles
   ~100 K msg/sec — usually not your bottleneck. Run a 3-node cluster
   above ~50 worker pods so a single node can be evicted safely.

4. **Redis.** The submission cache is a single instance with 512 MB by
   default. Switch to a Redis Cluster (3+ shards) above ~1 M cached
   submissions, or set an aggressive TTL on submission results.

5. **Worker HPA signals.** The HPA in [worker-deployment.yaml](deployments/kubernetes/worker-deployment.yaml#L248)
   already scales on NATS queue depth and `worker_active_executions` — verify
   those custom metrics are actually being exported (requires
   prometheus-adapter installed).

6. **Submission rate limit.** `RATE_LIMIT_LIMIT` defaults to 100/min **per
   user**. At a million users this is more than the worker tier can handle;
   keep it conservative (e.g. 60/min) and lean on horizontal scaling for
   total throughput.

7. **Sandbox cold starts.** First execution of any language is a multi-second
   Docker image pull. In compose: `make pull-images` once. In k8s: pre-bake
   images into the node AMI, or run a `pull-images` DaemonSet.

### Quick sizing recipes

```bash
# Small prod (k8s)
helm upgrade coderuntime ./deployments/helm \
  --set worker.config.workerCount=8 \
  --set worker.config.workerConcurrency=4 \
  --set worker.replicaCount=4 \
  --set worker.autoscaling.minReplicas=4 \
  --set worker.autoscaling.maxReplicas=20

# Large prod (millions MAU)
helm upgrade coderuntime ./deployments/helm \
  --set worker.config.workerCount=16 \
  --set worker.config.workerConcurrency=8 \
  --set worker.replicaCount=8 \
  --set worker.autoscaling.minReplicas=8 \
  --set worker.autoscaling.maxReplicas=100 \
  --set api.autoscaling.maxReplicas=50 \
  --set postgresql.architecture=replication \
  --set redis.architecture=cluster
```

Before bumping these in prod, run `make load-test` and watch:

- worker pod CPU/memory utilization (HPA target is 75% CPU)
- NATS pending message count per stream
- Postgres `pg_stat_activity` total connections
- API p99 latency, worker job duration p99

---

## Development

### Common commands

```bash
make build              # Compile API and Worker binaries to ./bin/
make test               # Run unit tests with race detector + coverage
make lint               # Run golangci-lint
make fmt                # gofmt + goimports
make compose-up         # docker compose up -d (all services)
make compose-down       # docker compose down
make compose-logs       # Tail all service logs
make pull-images        # Build code-runtime-* images + pull official images

# Test all 39 languages via the API
python3 scripts/test_languages.py

# Test specific language IDs
python3 scripts/test_languages.py 9 22 35
```

### Adding a new language

1. Add an ID constant and a `Language` entry in `internal/models/language.go`.
2. If no suitable public Docker Hub image exists, create `runtime-images/<lang>/Dockerfile` and build it as `code-runtime-<lang>:latest`.
3. Set `IsActive: true` and verify the `CompileCommand` / `RunCommand` paths.
4. Run `docker compose build api worker && docker compose up -d api worker`.
5. Add a test case to `scripts/test_languages.py` and verify with `python3 scripts/test_languages.py <id>`.

### Adding a migration

Create paired files under `scripts/migrations/`:

```
scripts/migrations/001_add_submission_index_up.sql
scripts/migrations/001_add_submission_index_down.sql
```

Run with `bash scripts/migrate.sh` (or `--dry-run` to preview).

---

## License

MIT — see `LICENSE` for details.
