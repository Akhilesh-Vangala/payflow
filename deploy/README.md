# Deploying PayFlow

One EKS cluster, two namespaces, and a data tier outside the cluster.

```
                          ┌─────────────────────────────────────┐
   dashboard.example.com  │  S3 + CloudFront (static SPA)       │
                          └─────────────────────────────────────┘

                          ┌─────────────────────────────────────┐
   api.example.com  ──►   │  ALB ──► payments-api (3)           │
                          │            │                        │
                          │            ├─► fraud-service (2)    │
                          │            ├─► bank-simulator (2)   │
                          │          webhook-worker (2)         │
                          │          scheduler (1, never more)  │
                          │            namespace: payflow       │
                          └────────────┬────────────────────────┘
                                       │ detokenize, the only path in
                          ┌────────────▼────────────────────────┐
   vault.example.com ──►  │  ALB ──► vault (2)                  │
                          │    dedicated node group, tainted    │
                          │    NetworkPolicy: default-deny      │
                          │    IRSA: sole holder of kms:Decrypt │
                          │    namespace: payflow-cde  ★ PCI    │
                          └─────────────────────────────────────┘

   RDS Postgres 17 · ElastiCache Redis 7   (private subnets)
```

## The three things that are load-bearing

**1. The vault gets its own hostname.** Not a path on the API. The PCI-scope
argument is the browser's same-origin policy — card fields in a document from a
different origin, unreadable by the merchant's JavaScript. Collapse the two
origins and that protection disappears with no error, no failing test, and every
merchant's page suddenly able to read card numbers.

**2. `scheduler` runs exactly one replica.** It sweeps rather than claiming rows
with `SKIP LOCKED`, so two replicas run the payout batch twice, and a payout
batch run twice pays a merchant twice. The Deployment uses `strategy: Recreate`
for the same reason — a rolling update would briefly run two.

**3. Migrations run as a Job before the deploy, never as an initContainer.**
An initContainer runs per pod, so three replicas means three concurrent
migration runs blocking on each other's advisory lock while their readiness
probes fail.

## Prerequisites

```bash
aws --version          # v2
eksctl version
kubectl version --client
```

A registered domain with an ACM certificate covering `api.`, `vault.` and
`dashboard.`, in the same region as the cluster.

## 1. Cluster

The CDE node group is separate and tainted, so nothing lands on those nodes
without the matching toleration. That is what keeps a card-handling process off
a shared kernel with the dashboard API.

```bash
eksctl create cluster \
  --name payflow --region "$AWS_REGION" \
  --nodegroup-name general --node-type t3.medium --nodes 2 \
  --with-oidc --managed

eksctl create nodegroup \
  --cluster payflow --name cde --node-type t3.medium --nodes 2 \
  --node-labels "payflow.io/workload=cde" \
  --managed
```

Then taint them. Without this the label is a suggestion, not a boundary:

```bash
kubectl taint nodes -l payflow.io/workload=cde payflow.io/cde=true:NoSchedule
```

**Network policy enforcement is not on by default.** With the VPC CNI and
policy disabled, every NetworkPolicy in `k8s/01-network-policies.yaml` is
accepted by the API server and silently does nothing — the worst outcome,
because the cluster looks protected.

```bash
kubectl set env daemonset aws-node -n kube-system ENABLE_NETWORK_POLICY=true
```

Verify by trying something that should be blocked, not by `kubectl get
networkpolicy` — see the checklist at the bottom.

## 2. Data tier

Both in private subnets, reachable only from the cluster's security group.

```bash
aws rds create-db-instance \
  --db-instance-identifier payflow \
  --engine postgres --engine-version 17 \
  --db-instance-class db.t4g.small \
  --allocated-storage 20 --storage-encrypted \
  --backup-retention-period 7 \
  --no-publicly-accessible

aws elasticache create-cache-cluster \
  --cache-cluster-id payflow \
  --engine redis --cache-node-type cache.t4g.micro --num-cache-nodes 1
```

Redis holds velocity counters and rate-limit budgets. Losing it is survivable —
velocity rules go quiet, tokenize and confirm limits fail **open**, login and
register fail **closed** — but the degraded state logs at `ERROR` on every
request and should page someone.

## 3. Secrets

```bash
# 32 random bytes, base64. Everything encrypted under it becomes unrecoverable
# if it is lost — see the warning below.
VAULT_MASTER_KEY=$(openssl rand -base64 32)
CARD_HASH_SALT=$(openssl rand -hex 32)
VAULT_INTERNAL_SECRET=$(openssl rand -hex 32)

kubectl create secret generic payflow-db -n payflow \
  --from-literal=url="postgres://payflow:${DB_PASSWORD}@${RDS_ENDPOINT}:5432/payflow?sslmode=require"
kubectl create secret generic payflow-redis -n payflow \
  --from-literal=url="redis://${REDIS_ENDPOINT}:6379/0"

kubectl create secret generic payflow-vault -n payflow \
  --from-literal=internal-secret="$VAULT_INTERNAL_SECRET" \
  --from-literal=card-hash-salt="$CARD_HASH_SALT"

# The same three values again in the CDE namespace — secrets do not cross
# namespaces, which is the point.
kubectl create secret generic payflow-db -n payflow-cde --from-literal=url="..."
kubectl create secret generic payflow-redis -n payflow-cde --from-literal=url="..."
kubectl create secret generic payflow-vault -n payflow-cde \
  --from-literal=master-key="$VAULT_MASTER_KEY" \
  --from-literal=internal-secret="$VAULT_INTERNAL_SECRET" \
  --from-literal=card-hash-salt="$CARD_HASH_SALT"
```

> **`VAULT_MASTER_KEY` has no recovery path.** Every stored card is encrypted
> under a data key wrapped by it. Lose it and every token in the database is
> permanently undecryptable — not "restore from backup", *gone*. Put it in AWS
> Secrets Manager with rotation disabled and a deletion protection window
> **before** it encrypts anything real.

> **`CARD_HASH_SALT` must be identical in both namespaces.** The vault and
> payments-api each derive card fingerprints from it. Different values do not
> error — they silently stop every velocity rule from ever matching again.

## 4. Images

```bash
aws ecr get-login-password | docker login --username AWS --password-stdin "$ECR_REGISTRY"

for svc in payments-api vault bank-simulator webhook-worker scheduler migrate; do
  docker build --build-arg SERVICE=$svc -t "$ECR_REGISTRY/payflow-$svc:$IMAGE_TAG" .
  docker push "$ECR_REGISTRY/payflow-$svc:$IMAGE_TAG"
done

docker build -f backend/ml/Dockerfile -t "$ECR_REGISTRY/payflow-fraud:$IMAGE_TAG" ./backend
docker push "$ECR_REGISTRY/payflow-fraud:$IMAGE_TAG"
```

## 5. Deploy

The manifests use `${VAR}` placeholders. Substitute before applying:

```bash
export ECR_REGISTRY IMAGE_TAG DOMAIN ACM_CERT_ARN AWS_ACCOUNT_ID

render() { envsubst < "$1" | kubectl apply -f -; }

render k8s/00-namespaces.yaml
render k8s/01-network-policies.yaml

# Migrations first, and WAIT. Starting the services against an un-migrated
# database gives a confusing cascade of column-not-found errors.
render k8s/10-migrate-job.yaml
kubectl wait --for=condition=complete job/payflow-migrate -n payflow --timeout=10m

render k8s/20-vault.yaml
render k8s/30-services.yaml
render k8s/40-ingress.yaml
```

**Adopting a database built by `docker-entrypoint-initdb.d`** — it has the
tables but no `schema_migrations`. Uncomment `args: ["-baseline"]` in the Job
for the first run only. Baselining a genuinely empty database gives you a
schema-less system that believes it is fully migrated.

## 6. Dashboard

```bash
cd frontend/dashboard
VITE_API_URL=https://api.$DOMAIN VITE_VAULT_URL=https://vault.$DOMAIN npm run build
aws s3 sync dist/ "s3://payflow-dashboard-$DOMAIN/" --delete
aws cloudfront create-invalidation --distribution-id "$CF_DIST" --paths '/*'
```

## 7. DNS

Point `api.` and `vault.` at their ALBs (separate load balancers, on purpose)
and `dashboard.` at CloudFront.

---

## Verify before you send anyone the link

Each of these has failed silently in some deployment somewhere.

```bash
# The vault is a different origin. If this returns the API's response, the PCI
# boundary is gone.
curl -s https://vault.$DOMAIN/healthz
curl -s https://api.$DOMAIN/healthz

# NetworkPolicy actually enforces. This must TIME OUT, not connect.
kubectl run probe --rm -it --image=busybox -n payflow --restart=Never -- \
  wget -qO- --timeout=5 http://vault.payflow-cde.svc.cluster.local:8081/healthz

# Migrations are recorded, not merely applied.
kubectl run migrate-status --rm -it -n payflow --restart=Never \
  --image=$ECR_REGISTRY/payflow-migrate:$IMAGE_TAG \
  --env=DATABASE_URL=... -- -status

# Exactly one scheduler. Any other number is a financial bug.
kubectl get deploy scheduler -n payflow -o jsonpath='{.spec.replicas}'

# Rate limits hold. Expect 429s.
python scratch/verify_ratelimit.py
```

## Capacity

Measured on one developer laptop — 8 cores shared between Postgres in Docker,
three Go services, *and* the load generator. Treat every number as a floor.

| | throughput | p50 |
|---|---|---|
| `/healthz` — no auth, no database | 25,451/s | 6ms |
| read path, auth query per request | 2,661/s | 72ms |
| read path, **+ API key cache** | 4,075/s | 46ms |
| read path, + cache + query fix, **500k rows** | 3,340/s | 56ms |
| charge (write path) | 588/s | 23ms |

One million reads completed in 6m15s. Exactly-once held throughout: 20,000
concurrent requests across 2,000 distinct idempotency keys produced exactly
2,000 charges.

**The laptop is the limit, not the code.** Two API instances on the same box
measured 2,578/s combined against 2,779/s for one — adding processes made it
slightly worse, which is what saturation looks like. A CPU profile puts ~50% of
samples in `runtime.cgocall` and `runtime.stdcall`: Windows network syscalls
that do not exist on Linux.

### Reaching 10k req/s

Roughly 1.7ms of CPU per read, about half of it Windows-only overhead. On Linux
with RDS rather than a Docker-NAT'd Postgres, budget **1,500–3,000 req/s per
pod at 1 vCPU** — so 10k is 5–8 replicas, which is inside the HPA's range.

Two code-level barriers had to come out first, and neither would have been
fixable by adding hardware:

1. **An API key lookup on every request.** A Postgres round trip to read three
   columns that change once a year. Now cached in-process for 30s.
2. **An O(table) dashboard query.** `charge_refund_totals` aggregated every
   charge against every refund before the `WHERE` applied — 0.28ms at 5 rows,
   **1,607ms at 500,000**. At that cost a connection serves 0.6 req/s and no
   number of pods reaches 10k.

What comes next, in the order it starts to bite:

- **PgBouncer, transaction mode.** The binding constraint past ~3 pods.
  12 pods × 50 connections is 600 against a default `max_connections` of 100.
  This is required before the HPA's `maxReplicas` is reachable at all.
- **Read replicas.** Dashboard reads are ~90% of traffic and never need the
  primary. Point `ListCharges` and friends at a replica.
- **Shard by `merchant_id`.** Merchants share no financial state, so
  cross-shard queries are rare. Trigger: sustained write IOPS above ~70%.

The write path is different and does not respond to caching — 588/s is bounded
by tokenize plus the acquirer round trip plus three transactions. It scales by
adding pods, then by sharding.

## Known gaps

Documented rather than silently missing.

| | |
|---|---|
| Shared cluster control plane | `cluster-admin` can exec into a vault pod. A QSA will raise this; the fix is a separate AWS account for the CDE, which the vault's HTTP-and-a-shared-secret interface makes a config change. |
| No password reset | A merchant who forgets their password is locked out permanently. |
| No email verification | Anyone can register under any business name with an address they do not own. |
| Expired checkout sessions | Never swept. The partial index implies a job that does not exist yet. |
| `scheduler` has no advisory lock | Hence the single replica. Add one before scaling. |
| Secrets in `kubectl create secret` | Base64, not encrypted at rest by default. Enable envelope encryption on the cluster, or use External Secrets with AWS Secrets Manager. |
