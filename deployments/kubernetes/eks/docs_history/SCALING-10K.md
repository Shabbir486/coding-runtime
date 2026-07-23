# Scaling Code Runtime to 10,000 requests/second

This document sizes the cluster for **sustained 10k req/s with real‑time execution**,
grounded in what the current free‑tier deployment actually measured (see
`EKS-DEPLOYMENT.md` §18). It also clarifies the three different meanings of "10k".

---

## 1. What "10k" means (pick the row that matches your need)

| Interpretation | Current cluster | Verdict |
|---|---|---|
| 10k requests **total**, drained over time | SQS buffers, KEDA+CA drain | ✅ already works |
| 10k requests as a **burst**, eventual results | absorbed by SQS, ~min to drain | ✅ already works |
| **10k requests/second, executed in real time** | caps ~150–300 exec/s | ❌ needs the sizing below |

The architecture (SQS‑decoupled, KEDA on backlog, dind sandboxes) is already correct.
Reaching 10k/s real‑time is a **sizing + cost** change, not a redesign.

---

## 2. The bottleneck chain (measured)

| Stage | Current ceiling | Limit at 10k/s |
|---|---|---|
| API ingest | ~166 req/s (2 pods, 1 client); ~1.5k/s at HPA max 20 | needs more pods + NLB |
| **Worker execution** | 20 pods × 8 = **160 concurrent** → ~150–300 jobs/s | **the wall** — needs ~1,250 pods |
| Nodes | max 12 → ~20 worker pods (~2/node) | needs big instances |
| **RDS** | **db.t4g.micro**, ~90 connections | **breaks first** — needs big DB + proxy |
| SQS | effectively unlimited | fine ✅ |

Execution is the wall: `10,000 req/s ÷ 8 concurrency/pod = ~1,250 worker pods` (assuming
~1s average job). RDS is the *first* thing that fails — at full worker scale the pods alone
want ~100+ DB connections, well past `t4g.micro`'s ceiling.

---

## 3. Target sizing for 10k/s sustained

Assumptions: average job ≈ 1s wall, 8 sandboxes per worker pod, headroom ×1.3.

### 3a. Worker fleet
- **Worker pods needed:** `10000 / 8 × 1.3 ≈ 2,000 pods` (use 1,250 as the floor for ~1s jobs;
  scale up for heavier languages/compile steps).
- **KEDA:** `maxReplicaCount: 2000`, `queueLength: 50` (or tune to 30 for snappier scale‑up),
  `pollingInterval: 10s`.
- **Per‑pod requests:** worker 500m + dind 250m = 750m CPU, ~1Gi mem.

### 3b. Nodes (use large instances — far fewer to schedule)
| Instance | vCPU | ~worker pods/node | Nodes for 2,000 pods |
|---|---|---|---|
| `m7i-flex.large` (now) | 2 | ~2 | ~1,000 ❌ unmanageable |
| `m7i.2xlarge` | 8 | ~10 | ~200 |
| **`m7i.8xlarge`** | 32 | ~40 | **~50** ✅ recommended |
| `c7i.12xlarge` (CPU‑heavy) | 48 | ~60 | ~33 |

- Dedicated **worker nodegroup** (taint `workload=sandbox:NoSchedule`, label the worker pods to
  tolerate it) so sandboxes don't crowd api/redis/system pods.
- **cluster-autoscaler** (or **Karpenter** — better at this scale) with nodegroup `max ≈ 60`.
- Bump pod density limits: Bottlerocket/AL2023 with prefix delegation (`vpc-cni`
  `ENABLE_PREFIX_DELEGATION=true`) so a node can hold dozens of pods.

### 3c. Database (fix this first)
- `db.t4g.micro` → **Aurora MySQL** or `db.r6g.2xlarge`+ (Multi‑AZ).
- **RDS Proxy** in front — with ~2,000 worker pods you cannot give each a raw connection;
  the proxy multiplexes. Keep `CODERUNTIME_DATABASE_MAX_OPEN_CONNS` low per pod (2–5).
- Consider **not** writing every submission synchronously: batch result writes, or move
  results to Redis/S3 and persist asynchronously.

### 3d. API + ingress
- api-gateway HPA `max ≈ 50–100` pods; it's stateless and cheap (just validates + SQS publish).
- Replace the single **Classic ELB** with a **Network Load Balancer** (`aws-load-balancer-type:
  external`, `nlb-target-type: ip`) via the **AWS Load Balancer Controller** — NLBs handle
  millions of flows and don't need pre‑warming the way Classic ELBs do.
- **SQS `SendMessageBatch`** on the publish path and `ReceiveMessage` batching on consume to
  cut per‑message API overhead.

### 3e. Redis
- Single pod → **ElastiCache (clustered)** if Redis is on the hot path (idempotency/cache).

---

## 4. Rollout order (so you never hit the RDS wall mid‑test)

1. **RDS first** — upgrade instance + add RDS Proxy. (Otherwise everything else just
   overwhelms the DB.)
2. **Dedicated worker nodegroup** on large instances + Karpenter/CA `max`.
3. **KEDA max** → target pod count; **api HPA max** up.
4. **NLB** via the LB controller; enable SQS batching.
5. Load test in steps: 1k/s → 3k/s → 10k/s, watching RDS connections, SQS backlog age,
   and node provisioning latency at each step.

---

## 5. Rough monthly cost (order of magnitude, on‑demand, ap‑southeast‑2)

> Real‑time 10k/s is expensive because it implies a *large fleet running continuously*.
> If load is bursty, autoscaling keeps the average far lower.

| Component | Sizing | ~Cost/mo (full load) |
|---|---|---|
| Worker nodes | ~50 × `m7i.8xlarge` | ~$50,000 |
| EKS control plane | 1 | ~$73 |
| Aurora + RDS Proxy | `r6g.2xlarge` Multi‑AZ | ~$1,500 |
| NLB | 1 + LCUs | ~$30 + traffic |
| ElastiCache | clustered | ~$300 |

**Levers to cut cost:** Spot instances for worker nodes (sandboxes are stateless/retryable —
ideal for Spot), aggressive scale‑down when idle, smaller average job time, and right‑sizing
`queueLength` so you don't over‑provision. With bursty traffic the autoscaler means you pay
for ~50 nodes only during peaks, not 24/7.

---

## 6. Bottom line

- **Architecture:** ready — proven to autoscale 1→20 workers / 2→11 nodes, queue‑driven, 100%
  ingest.
- **For 10k/s real‑time:** upgrade **RDS (+proxy) → big worker nodegroup → KEDA/CA max → NLB**,
  in that order.
- **Current cluster** is a free‑tier‑scale deployment: great for **bursts** and **~150–300
  exec/s sustained**. Don't expect 10k/s real‑time from it without the changes above.
