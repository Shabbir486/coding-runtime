# Code Runtime — kubectl / aws CLI operational cheatsheet

Day-to-day commands for operating the `coderuntime` EKS cluster. Real values for
this deployment are baked in (region `ap-southeast-2`, account `768054003064`,
namespace `coderuntime`). Copy/paste friendly.

```bash
# Set these once per shell
export NS=coderuntime
export REGION=ap-southeast-2
export CLUSTER=coderuntime
export ACCOUNT=768054003064
export JOBS_Q=https://sqs.$REGION.amazonaws.com/$ACCOUNT/code-submission-processing-queue
export LB=http://aa6f9d7b568b4425f84ea155a296db33-71808099.ap-southeast-2.elb.amazonaws.com
```

---

## 0. Connect / context

```bash
# Point kubectl at the cluster (writes ~/.kube/config)
aws eks update-kubeconfig --name $CLUSTER --region $REGION

# Who am I / which cluster
kubectl config current-context
aws sts get-caller-identity
```

---

## 1. Pods — status, crashes, restarts

```bash
# All pods in the namespace (wide = node + IP)
kubectl -n $NS get pods -o wide

# Just the worker pods
kubectl -n $NS get pods -l app.kubernetes.io/name=worker -o wide

# Count pods by phase (quick health glance)
kubectl -n $NS get pods --no-headers | awk '{print $3}' | sort | uniq -c

# Worker pods by phase (CrashLoopBackOff / Pending / Running)
kubectl -n $NS get pods -l app.kubernetes.io/name=worker --no-headers \
  | awk '{print $3}' | sort | uniq -c

# Find crashing / errored pods
kubectl -n $NS get pods --no-headers | grep -E "CrashLoop|Error|ImagePull|Pending"

# Restart counts (spot flapping pods) — column 4 = RESTARTS
kubectl -n $NS get pods --no-headers | awk '$4+0>0 {print $1, "restarts="$4}'

# Per-container readiness + last-terminated reason/exit code for one pod
POD=<pod-name>
kubectl -n $NS get pod $POD -o jsonpath='{range .status.containerStatuses[*]}{.name}: ready={.ready} restarts={.restartCount} waiting={.state.waiting.reason} lastExit={.lastState.terminated.exitCode}/{.lastState.terminated.reason}{"\n"}{end}'

# initContainers too (dind is a native sidecar here -> initContainerStatuses)
kubectl -n $NS get pod $POD -o jsonpath='{range .status.initContainerStatuses[*]}init/{.name}: ready={.ready} started={.started} restarts={.restartCount}{"\n"}{end}'

# Why is a pod Pending / failing to schedule? (events at the bottom)
kubectl -n $NS describe pod $POD | sed -n '/Events:/,$p'
```

---

## 2. Logs

```bash
# Worker app logs (the worker container; dind is a separate container)
POD=$(kubectl -n $NS get pod -l app.kubernetes.io/name=worker -o jsonpath='{.items[0].metadata.name}')
kubectl -n $NS logs $POD -c worker --tail=50
kubectl -n $NS logs $POD -c worker --since=120s          # recent window
kubectl -n $NS logs $POD -c worker -f                     # follow

# Filter for the things that matter
kubectl -n $NS logs $POD -c worker --since=120s | grep -iE "error|fail|pull access|sandbox execute|completed"

# dind (Docker daemon) sidecar logs
kubectl -n $NS logs $POD -c dind --tail=50

# Previous (crashed) container's logs — vital for CrashLoopBackOff
kubectl -n $NS logs $POD -c worker --previous

# Across ALL worker pods at once (label selector; may include terminating pods)
kubectl -n $NS logs -l app.kubernetes.io/name=worker -c worker --tail=20

# api-gateway / queue-manager
kubectl -n $NS logs deploy/api-gateway --tail=50
kubectl -n $NS logs deploy/queue-manager --tail=50
```

---

## 3. Worker autoscaling (KEDA) + node autoscaling

```bash
# KEDA ScaledObject (min/max, READY, ACTIVE)
kubectl -n $NS get scaledobject worker
kubectl -n $NS get scaledobject worker \
  -o jsonpath='min={.spec.minReplicaCount} max={.spec.maxReplicaCount} qLen={.spec.triggers[0].metadata.queueLength}{"\n"}'

# The HPA KEDA manages (current -> desired, current metric value)
kubectl -n $NS get hpa keda-hpa-worker
kubectl -n $NS get hpa keda-hpa-worker \
  -o jsonpath='cur={.status.currentReplicas} desired={.status.desiredReplicas} metric={.status.currentMetrics[0].external.current.averageValue}{"\n"}'

# Change the scale-out threshold (lower = scale sooner = lower latency)
kubectl -n $NS patch scaledobject worker --type=json \
  -p='[{"op":"replace","path":"/spec/triggers/0/metadata/queueLength","value":"8"}]'

# Change the max worker pods
kubectl -n $NS patch scaledobject worker --type=merge -p '{"spec":{"maxReplicaCount":20}}'

# KEDA operator logs (why is/ isn't it scaling?)
kubectl -n keda logs deploy/keda-operator --tail=50 | grep -iE "worker|error|scale"

# Nodes
kubectl get nodes
kubectl get nodes --no-headers | grep -cw Ready          # count Ready nodes

# cluster-autoscaler: is it adding nodes? (scale-up events)
kubectl -n kube-system logs deploy/cluster-autoscaler --tail=120 \
  | grep -iE "scale.?up|ScaledUpGroup|TriggeredScaleUp|max node|no.*capacity"
```

---

## 4. SQS — backlog / queue depth (the scaling signal)

```bash
# Visible (waiting) + in-flight (being processed) + delayed
aws sqs get-queue-attributes --queue-url $JOBS_Q --region $REGION \
  --attribute-names ApproximateNumberOfMessages \
                    ApproximateNumberOfMessagesNotVisible \
                    ApproximateNumberOfMessagesDelayed \
  --query 'Attributes' --output json

# One-liner backlog: visible+inflight
aws sqs get-queue-attributes --queue-url $JOBS_Q --region $REGION \
  --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
  --query 'Attributes.[ApproximateNumberOfMessages,ApproximateNumberOfMessagesNotVisible]' \
  --output text | tr '\t' '+'
```

---

## 5. Config & secrets

```bash
# Dump a ConfigMap's data (sorted)
kubectl -n $NS get cm worker-config -o json | python3 -c \
  "import sys,json;[print(k,'=',v) for k,v in sorted(json.load(sys.stdin)['data'].items())]"

# Patch a config value, then restart the consumers to pick it up
kubectl -n $NS patch cm worker-config --type=merge \
  -p '{"data":{"CODERUNTIME_WORKER_CONCURRENCY":"3"}}'
kubectl -n $NS rollout restart deploy/worker

# List secret KEYS (never the values)
kubectl -n $NS get secret api-secrets -o json | python3 -c \
  "import sys,json;print(', '.join(json.load(sys.stdin)['data'].keys()))"

# Refresh the ECR pull-auth secret (the X-Registry-Auth token expires ~12h)
TOKEN=$(aws ecr get-login-password --region $REGION)
REGISTRY=$ACCOUNT.dkr.ecr.$REGION.amazonaws.com
XAUTH=$(python3 -c "import json,base64,sys;print(base64.b64encode(json.dumps({'username':'AWS','password':sys.argv[1],'serveraddress':sys.argv[2]}).encode()).decode())" "$TOKEN" "$REGISTRY")
kubectl -n $NS create secret generic ecr-registry-auth \
  --from-literal=CODERUNTIME_DOCKER_REGISTRY="$REGISTRY" \
  --from-literal=CODERUNTIME_DOCKER_REGISTRY_AUTH="$XAUTH" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n $NS rollout restart deploy/worker
```

---

## 6. Deployments — rollout, scale, image, resources

```bash
# Rollout status / restart
kubectl -n $NS rollout status deploy/worker --timeout=180s
kubectl -n $NS rollout restart deploy/worker

# Manual scale (note: KEDA owns worker replicas; this is for api/queue)
kubectl -n $NS scale deploy/api-gateway --replicas=2

# Update a container image
kubectl -n $NS set image deploy/worker \
  worker=$ACCOUNT.dkr.ecr.$REGION.amazonaws.com/coderuntime/worker:v2

# Patch container resources (idx 0 = worker container)
kubectl -n $NS patch deploy worker --type=json -p='[
  {"op":"replace","path":"/spec/template/spec/containers/0/resources",
   "value":{"requests":{"cpu":"250m","memory":"256Mi"},"limits":{"cpu":"750m","memory":"1Gi"}}}]'

# Inspect a deployment's containers + resources
kubectl -n $NS get deploy worker -o json | python3 -c \
  "import sys,json;[print(c['name'],c.get('resources')) for c in json.load(sys.stdin)['spec']['template']['spec']['containers']]"
```

---

## 7. exec / shell / API access

```bash
# Shell into the worker container
POD=$(kubectl -n $NS get pod -l app.kubernetes.io/name=worker -o jsonpath='{.items[0].metadata.name}')
kubectl -n $NS exec -it $POD -c worker -- sh

# Check an env var inside a running container
kubectl -n $NS exec $POD -c worker -- sh -c 'echo $CODERUNTIME_DOCKER_REGISTRY'

# Port-forward the API to localhost (then curl localhost:18002/...)
kubectl -n $NS port-forward deploy/api-gateway 18002:8002

# Public endpoint (Classic ELB) — login + a submission
JWT=$(curl -s -X POST $LB/auth/token -H 'Content-Type: application/json' \
  -d '{"email":"dev@coderuntime.io","password":"Admin123!"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
curl -s -X POST "$LB/submissions?wait=true" -H "Authorization: Bearer $JWT" \
  -H 'Content-Type: application/json' -d '{"language_id":1,"source_code":"echo hi"}'
curl -s $LB/health
```

---

## 8. RDS access from local (DB is private — tunnel through a pod)

```bash
# socat proxy pod -> RDS, then port-forward to your laptop
kubectl -n $NS run rds-tunnel --image=alpine/socat --restart=Never -- \
  tcp-listen:3306,fork,reuseaddr \
  tcp-connect:coderuntime-prod.cziyoeg0oioy.$REGION.rds.amazonaws.com:3306
kubectl -n $NS wait --for=condition=Ready pod/rds-tunnel --timeout=120s
kubectl -n $NS port-forward pod/rds-tunnel 3306:3306
# then: mysql -h 127.0.0.1 -P 3306 -u coderuntime -p coderuntime
kubectl -n $NS delete pod rds-tunnel        # cleanup
```

---

## 9. Pause / resume the cluster (cost control)

```bash
# PAUSE: stop the node autoscaler, then scale the nodegroup to 0
kubectl -n kube-system scale deploy/cluster-autoscaler --replicas=0
aws eks update-nodegroup-config --cluster-name $CLUSTER --nodegroup-name system \
  --scaling-config minSize=0,maxSize=12,desiredSize=0 --region $REGION

# RESUME: bring autoscaler back, restore the nodegroup baseline
kubectl -n kube-system scale deploy/cluster-autoscaler --replicas=1
aws eks update-nodegroup-config --cluster-name $CLUSTER --nodegroup-name system \
  --scaling-config minSize=2,maxSize=12,desiredSize=2 --region $REGION

# Check nodegroup scaling config
aws eks describe-nodegroup --cluster-name $CLUSTER --nodegroup-name system \
  --region $REGION --query 'nodegroup.scalingConfig'
```

---

## 10. ECR — runtime images

```bash
# List the code-runtime-* repos
aws ecr describe-repositories --region $REGION \
  --query 'repositories[?starts_with(repositoryName,`code-runtime-`)].repositoryName' --output text

# Is a given language image pushed? (push time)
aws ecr describe-images --repository-name code-runtime-java --image-ids imageTag=latest \
  --region $REGION --query 'imageDetails[0].imagePushedAt' --output text
```

---

## 11. Live load-test + watch scaling (what we run for reports)

```bash
# warm images first (avoids cold-pull noise), then the real test
python3 scripts/load_test_all_langs.py --base-url $LB --total 250 --concurrency 25 \
  --timeout 180 --lang-ids "$(python3 -c "import sys;sys.path.insert(0,'scripts');from test_languages import HELLO;print(','.join(map(str,sorted(HELLO))))")"

# watch scaling live, side terminal (every 8s)
watch -n8 'kubectl -n coderuntime get hpa keda-hpa-worker; \
  kubectl -n coderuntime get pods -l app.kubernetes.io/name=worker --no-headers | awk "{print \$3}" | sort | uniq -c; \
  kubectl get nodes --no-headers | grep -cw Ready'
```

---

## Quick troubleshooting index

| Symptom | First command |
|---|---|
| Pods `CrashLoopBackOff` | `kubectl -n $NS logs <pod> -c worker --previous` |
| Pods stuck `Pending` | `kubectl -n $NS describe pod <pod>` → Events; then check nodes/CA logs |
| Not scaling up | `kubectl -n $NS get hpa keda-hpa-worker` + SQS backlog (§4) |
| Nodes not added | `kubectl -n kube-system logs deploy/cluster-autoscaler` (§3) |
| `pull access denied` | check `ecr-registry-auth` secret keys / refresh token (§5) |
| Jobs `Internal Error` | worker logs `grep -iE "error|pull|bind|daemon"` (§2) |
| `transport`/reset on wait=true | API `SERVER_WRITE_TIMEOUT` must exceed the 120s wait window (§5/§6) |
```
