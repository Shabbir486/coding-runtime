# Kubernetes deployments

Two fully-separated, self-contained environments. Pick one — they share nothing
at runtime and can't clobber each other.

| | [`local/`](local/) | [`eks/`](eks/) |
|---|---|---|
| **Target** | kind on a laptop | Amazon EKS (the live cluster) |
| **Bring-up** | `local/setup.sh` | `eks/setup.sh` |
| **Teardown** | `local/setup.sh down` | `eks/teardown.sh` (stops billing; keeps RDS/SQS/ECR) |
| **Queue** | AWS SQS | AWS SQS |
| **Autoscaling** | KEDA (worker pods 1→20) | KEDA (1→20) + cluster-autoscaler (nodes 1→12) |
| **Datastores** | in-cluster MySQL + Redis | RDS MySQL + in-cluster Redis |
| **Sandbox runtime** | DooD (host Docker, local images) | dind sidecar + ECR |
| **AWS auth** | static `.env` keys | IRSA (keyless) |
| **Docs** | [local/README.md](local/README.md) | [eks/DEPLOY.md](eks/DEPLOY.md) |

## Which one?

- **Develop / validate the whole platform (incl. KEDA scaling) on your machine** →
  [`local/`](local/). One command, no AWS cluster required (still talks to real SQS).
- **Deploy or operate the production cluster** → [`eks/`](eks/). This is what's
  currently deployed; see [eks/DEPLOY.md](eks/DEPLOY.md) for the one-go runbook and
  [eks/docs_history/](eks/docs_history/) for deep-dives (scaling, autoscaling, ops).

```bash
# local
deployments/kubernetes/local/setup.sh

# eks (see eks/DEPLOY.md for required env)
deployments/kubernetes/eks/setup.sh       # deploy / rebuild
deployments/kubernetes/eks/teardown.sh    # delete cluster to stop billing
```

Both use the same application images (built from `docker/Dockerfile.*`) and the
same `runtime-images/` sandboxes — only the surrounding infra and wiring differ.
