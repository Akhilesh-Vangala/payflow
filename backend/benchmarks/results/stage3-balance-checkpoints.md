# Stage 3 — balance checkpoints

Removes the `ledger_balances` upsert from the payment path. Same configuration,
same workload, same dataset shape as [stage0-baseline](stage0-baseline.md).

## Change

`ledger.Post` previously followed every entry insert with
`INSERT … ON CONFLICT (account_id) DO UPDATE` on `ledger_balances`, taking an
exclusive row lock held until the caller's transaction committed — three per
charge, the third on a row shared by every merchant on the platform.

Balances are now **checkpoints**: `balance_cents` is exact as of
`through_entry_id`, and `ledger.Balance` sums the tail from `ledger_entries` at
read time. The result is exact, not eventually consistent — the checkpoint only
bounds how much tail there is to sum. The scheduler advances it via
`ledger.AdvanceCheckpoints`, which changes no answer and is safe to skip.

## Results

Two runs per level, because this machine is noisy — the load generator shares
eight cores with Postgres and five services.

| Concurrency | Baseline | Stage 3 | Median change |
|---|---|---|---|
| 50 | 80 req/s | 109, 120 | **+43%** |
| 100 | 84 req/s | 75, 112 | **+11%** |
| 200 | 91 req/s | 111, 125 | **+30%** |

Latency at c=50: p50 **251ms → ~190ms**, p95 **1.13s → ~340ms**.

The 5-second p99 outliers are the bank simulator's deliberate 1.5% timeout
injection, not a system fault.

## What actually changed, mechanically

This matters more than the throughput number, because throughput on this box is
noisy and the mechanism is not:

| | Baseline | Stage 3 |
|---|---|---|
| Queries **waiting on a lock**, sampled under load | **46** | **0** |
| Longest running query | 1,606 ms | 59 ms |
| Slowest statement by mean | balance upserts at 3,320–6,527 ms | 2.17 ms (`INSERT INTO risk_assessments`) |

Row-lock contention is gone, not merely reduced.

## Correctness

- Full suite green: ledger, payments, payouts, reconcile, risk, vault, webhook.
- Checkpoint exactness verified directly against the entries on all 53 accounts:
  **0 drifted checkpoints, 0 balance mismatches** between `checkpoint + tail` and
  a full derivation.
- Migration 0013 rebuilds every balance from `ledger_entries` and snapshots
  `max(id)` first, so entries written during the migration land after the
  checkpoint rather than being double-counted or lost.
- Reconciliation's drift check was rewritten to compare the checkpoint against
  entries **up to `through_entry_id`**. Comparing against all entries — as it did
  before — would have flagged every active account, which is a false alarm on a
  check whose whole value is that it never cries wolf.

Two bugs this stage introduced and fixed before measuring:

1. `Post` no longer created the balance row, so accounts created after the
   migration had no checkpoint and `Balance` returned `ErrAccountNotFound`.
   Fixed by seeding the row in `EnsureAccount` — once per account lifetime, not
   per charge.
2. A test helper read `balance_cents` directly rather than checkpoint + tail, so
   it saw 0 and reported a lost payment that had not been lost.

## The next bottleneck is not what I expected

Two hypotheses tested and **disproven**:

- **WAL fsync.** Three transactions per charge means three commits.
  `synchronous_commit = off` measured **106 req/s** against 112 with it on — no
  difference. Durability stays on. *(That setting was a diagnostic only; it
  trades away exactly the guarantee a payment ledger exists to provide.)*
- **A slow query.** No statement exceeds **2.17 ms mean**. Total attributable DB
  work is ~9.5 ms per charge, against Postgres burning ~210% CPU.

The gap between ~9.5ms of attributable statement time and 210% CPU points at
work that happens **at commit** and is therefore not attributed to any statement:
`trg_ledger_balanced` is `FOR EACH ROW` and `DEFERRABLE`, so it runs its
aggregate **three times per charge at COMMIT** — roughly 340 executions/second at
this rate. One sampled connection was indeed parked in `LWLock` on `commit`.

That is Stage 4's first target, alongside batching the ledger legs and the three
uncached `EnsureAccount` lookups.

---

## Correction, added during Stage 4

**The throughput percentages above are not trustworthy.** They compare a
measurement taken after this change against one taken earlier in the session,
and the dataset grew between them — from 50,875 charges at Stage 0 to 101,984 by
the end of Stage 4. Each stage was measured against a larger, more bloated table
than the one before it, so the change under test and the drift cannot be
separated after the fact.

Stage 4 introduced a controlled A/B method (two binaries differing only in the
change, alternated against the same live dataset) and every figure from that
stage onward uses it.

**What survives the correction is the mechanism evidence in the table above**,
which does not depend on throughput at all: lock waits 46 to 0, longest query
1,606ms to 59ms, slowest statement 3,320-6,527ms to 2.17ms. The row-lock
contention was real and it is gone. The "+43%" should be read as "an
improvement of unknown size".
