# Stage 0 — baseline

Unmodified code. Every later stage is measured against this.

## Configuration

| | |
|---|---|
| Hardware | 1 laptop, 8 cores, Windows |
| Topology | Postgres + Redis in Docker; vault, bank-sim, fraud-scorer, payments-api as host processes; **load generator on the same machine** |
| Replicas | 1 of each |
| `DB_MAX_CONNS` | 50 |
| `FRAUD_TIMEOUT` | 50ms |
| `BANK_TIMEOUT` | 5s |
| `LOG_LEVEL` | warn |
| Acquirer | simulator, 24ms p50, mix 95% success / 3% decline / 1.5% timeout / 0.5% network error |
| Dataset | 50,875 charges · 87,610 ledger entries · 27 merchants · 50,816 outbox rows |
| Workload | `-mode=charge -dupes=1`, distinct Luhn-valid PAN and source IP per charge, keys rotated across 24 merchants |
| Duration | 1,500 charges per level |

## Results

| Concurrency | Throughput | p50 | p95 | p99 |
|---|---|---|---|---|
| 50 | **80 req/s** | 251 ms | 1.125 s | 1.72 s |
| 100 | **84 req/s** | 665 ms | 2.156 s | 2.488 s |
| 200 | **91 req/s** | 1.582 s | 2.622 s | 2.864 s |

## Reading

Throughput is **flat at 80–91 TPS** across a 4× concurrency range while p50 rises
**6.3×** (251ms → 1,582ms). Added concurrency converts entirely into queueing.
That is the signature of a serialisation point, not a resource limit.

Corroborating evidence from earlier measurement on the same build:

- API CPU never exceeds 16% of 8 cores while saturated; it *falls* as concurrency
  rises because more goroutines block.
- Postgres reaches 214% CPU (vs 0.20% idle).
- `pg_stat_activity` shows individual `ledger_balances` upserts blocking
  **3.3–6.5 seconds**.
- Load spread across 1 / 4 / 12 / 24 merchants: **75 / 64 / 91 / 72 TPS** — flat,
  so the contended row is not per-merchant.
- Removing the platform-fee leg: **75 → 135 TPS**.

## Bottleneck

`applyBalance` (`internal/ledger/ledger.go:166`) — three `ledger_balances` row
locks per charge, the third on a platform-wide row shared by every merchant.
Held from the upsert until tx3 commits, which is after the charges UPDATE, the
outbox INSERT, the idempotency UPDATE and three deferred trigger scans.
