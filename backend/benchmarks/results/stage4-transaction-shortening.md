# Stage 4 — shortening the charge transaction

Three optimisations, benchmarked individually. **One of them worked.**

The most important outcome of this stage is not a throughput number — it is that
the measurement method used in Stages 0–3 turned out to be unsound, and the
results below use a corrected one.

---

## The methodology problem

Every benchmark before this stage compared a measurement taken *now* against one
taken *earlier in the session*. But the dataset grows with every run:

| | Stage 0 | End of Stage 4 |
|---|---|---|
| `charges` | 50,875 | **101,984** |
| `ledger_entries` | 87,610 | **233,437** |
| table + index size | — | 200 MB |

The dataset **doubled**, with ~15,800 dead tuples accumulating between autovacuum
runs. So each stage was measured against a larger, more bloated table than the
one before it. Sequential comparisons conflate the change under test with that
drift, and there is no way to separate them after the fact.

**Corrected method:** build two binaries differing only in the change under test,
alternate them back to back against the same live dataset, and repeat. Drift
then affects both arms equally. Every result below uses this.

---

## 4.1 — Validate the ledger once per transaction, not once per leg

`trg_ledger_balanced` is `DEFERRABLE` and `FOR EACH ROW`, so a three-leg charge
runs the same balance aggregate three times at COMMIT.

**First, the cost was quantified** by disabling the trigger outright — a
diagnostic, immediately reverted:

| | runs | median |
|---|---|---|
| trigger on | 98, 105, 89 | 98 |
| trigger **entirely disabled** | 104, 109, 124 | **109** |

So the whole trigger is worth ~11%, and firing it once instead of three times
could recover at most two thirds of that.

**Implemented** as a transaction-local GUC memo (`set_config(..., is_local)`),
because PostgreSQL requires constraint triggers to be `FOR EACH ROW` — a
statement-level trigger cannot be `DEFERRABLE`, and deferral is what allows legs
to be inserted separately and validated as a unit.

**Result — no gain:**

| | runs | median |
|---|---|---|
| Stage 3, per-row | 98, 105, 89 | 98 |
| once per transaction | 94, 95, 109, 95, 84, 96, 105 | **95** |

Seven runs, no improvement. The cost is the **trigger invocation** — plpgsql
call overhead, three times per charge — not the aggregate it runs. The memo
avoids the query but not the call, so it captured none of the 11%.

**Reverted.** It added a GUC state machine to the most safety-critical function
in the system for zero measured benefit. Verified after reverting that a
single unbalanced leg is still rejected at COMMIT.

---

## 4.2 — Batch the ledger inserts

Three legs were three separate `INSERT` round trips inside the caller's
transaction. Replaced with one `unnest()`-based multi-row insert — one statement
whatever the leg count, so 2-leg charges, 3-leg charges and 4-leg disputes share
a single prepared statement.

**A/B, alternating, same dataset:**

| round | one INSERT per leg | batched |
|---|---|---|
| 1 | 87 | **98** |
| 2 | **88** | 87 |
| 3 | **102** | 98 |
| mean | 92.3 | 94.3 |

**Within noise, and the direction flips.** The sequential measurement had
suggested +14%; that was dataset drift.

**Kept anyway, with the reasoning stated so it can be revisited:** the round-trip
reduction is arithmetic rather than speculative — three statements become one.
It measures neutral here because Postgres is on loopback and a round trip costs
~0.1ms. Against RDS across an availability zone at ~0.5–1ms per round trip,
saving two per charge is 1–2ms of the ~9.5ms DB budget. That claim is **not
measured** and should be tested on real infrastructure before being believed.

---

## 4.3 — Cache `EnsureAccount` lookups

`EnsureAccount` was called once per ledger leg and cached nowhere — three
`SELECT`s per charge for rows that are immutable once created.

**A/B, alternating, same dataset:**

| round | no cache | cached |
|---|---|---|
| 1 | 78 | **87** |
| 2 | 94 | **96** |
| 3 | 84 | **91** |
| mean | 85.3 | **91.3** |

**+7%, and the cache wins every round.** The only unambiguous win in this stage.

Two correctness properties, both deliberate:

- **The cache key includes the database** (host, port, name), not just the
  account identity. Without that the integration tests poison it — testcontainers
  gives each test its own Postgres inside the same process, so a cached id from
  one test's database would be handed to another and fail a foreign key.
- **Only the lookup path populates the cache, never the creation path.** A newly
  inserted account belongs to an uncommitted transaction; caching it and then
  rolling back would leave a permanently wrong id in memory. The first use after
  creation pays one lookup, and it is cached from then on.

Safe to cache indefinitely because the data says so, not as a bet: account rows
are immutable, never deleted while entries reference them, and
`UNIQUE (merchant_id, account_type, currency)` means one identity maps to one id
for all time. There is nothing to invalidate.

---

## Correctness

- Full suite green: ledger, payments, payouts, reconcile, risk, vault, webhook.
  *(One run showed all tests in two packages failing at 0.00s — Docker could not
  start containers with the disk near full. Both packages pass in isolation; it
  was resource exhaustion, not a regression.)*
- Ledger balanced across all **90,128** transactions.
- **0 drifted checkpoints** across all 53 accounts.
- Trigger re-verified after the 4.1 revert: single unbalanced leg rejected, a
  three-leg transaction off by one cent rejected, a correct three-leg
  transaction accepted.

---

## Where this leaves the numbers

**The honest position: throughput on this machine sits around 85–100 charges/sec,
and the stage-by-stage percentages from Stages 0–3 cannot be trusted** because
they were measured sequentially against a growing dataset.

What *is* trustworthy:

- **Stage 3's mechanism**, which does not depend on the throughput number at all:
  queries waiting on a lock went from **46 to 0**, the longest running query from
  **1,606ms to 59ms**, and the slowest statement from **3,320–6,527ms to 2.17ms**.
  The contention is gone whatever the TPS figure says.
- **Stage 4.3's +7%**, measured by controlled A/B.
- **Stage 4.1's zero**, measured by controlled A/B and acted on by reverting.

## Next bottleneck

Still unidentified, and the remaining candidates are now cheap to test with the
A/B method:

- Per-charge work outside the ledger — `recordRisk` issues two autocommit
  statements on the hot path for an audit record.
- Three transactions per charge, so three commits.
- The dataset itself: 200 MB of table and index with no partitioning and no
  retention, which is Stage 6.

---

## 4.4 — Fold the risk verdict into the charge insert

`recordRisk` ran two statements on the pool with no transaction: an `UPDATE
charges` for the verdict and an `INSERT INTO risk_assessments`. With tx1, tx2 and
tx3 that made **five commits per charge**.

Restructured so risk is scored *before* tx2 and the verdict is written by the
charge INSERT itself.

**A/B, alternating, same dataset:**

| round | 5 commits | 3 commits |
|---|---|---|
| 1 | **92** | 80 |
| 2 | 91 | **94** |
| 3 | **94** | 90 |
| mean | 92.3 | 88.0 |

**No throughput gain.** Kept anyway, for a correctness reason rather than a speed
one: the verdict now commits atomically with the charge. Previously a charge
existed for a window carrying no verdict at all, and if the UPDATE failed it
stayed that way — a decline nobody could explain afterwards.

**A semantic I broke and restored.** My first version put `risk_assessments`
inside tx2, which meant an audit-write failure would roll back and decline a
legitimate customer payment. The original code deliberately said *"the assessment
is an audit record, not a gate"*. That judgement is right and I reinstated it:
the verdict commits with the charge, the detailed audit row stays a non-fatal
write outside the transaction.

Also corrected the `CreateCharge` doc comment, which described **two**
transactions when the code has always had three.

---

## What this stage actually established

Four changes, benchmarked individually by controlled A/B:

| Change | Result | Kept? |
|---|---|---|
| Trigger once per transaction | **no gain** | reverted |
| Batched ledger inserts | within noise | kept, for real-network round trips |
| `EnsureAccount` cache | **+7%, wins every round** | kept |
| Risk verdict in the charge insert | no gain | kept, for correctness |

**The pattern is the finding.** Every optimisation that removes a database round
trip or a commit measures as noise on this machine. That is consistent and it
means something: **round trips and commits are not what limits this system
here.**

A concurrency sweep locates the real shape:

| concurrency | throughput | p50 |
|---|---|---|
| 5 | 31/s | **50 ms** |
| 10 | 41/s | 70 ms |
| 25 | 44/s | 239 ms |
| 50 | 51/s | 221 ms |
| 100 | 51/s | 305 ms |

A single charge costs **~50 ms**. Throughput plateaus by c≈25 and everything
beyond that becomes queue. With eight cores shared between Postgres, five
services and the load generator, the system is CPU-starved as a whole rather than
blocked on any one component.

**A second methodological flaw, found here:** these 400-charge runs measured 51/s
at c=100 where 1,200-charge runs measured ~90/s. Short runs are dominated by
process startup, connection-pool warm-up and prepared-statement priming. The A/B
tests above used n=1000 — about 11 seconds — so they are warm-up contaminated
too. The direction of the `EnsureAccount` result (3 out of 3 rounds) is more
trustworthy than its magnitude.

## Conclusion for the 5,000 TPS goal

**Further micro-optimisation cannot be measured on this hardware.** The remaining
differences are smaller than the noise floor, and the noise floor is set by
sharing eight cores with the load generator.

That is the actionable finding. The next real step is not another code change —
it is a measurement environment where the load generator is on a separate machine
and Postgres has its own CPU. Everything after that (PgBouncer, read replicas,
partitioning, sharding) needs a trustworthy baseline to be evaluated against, and
this machine cannot provide one.
