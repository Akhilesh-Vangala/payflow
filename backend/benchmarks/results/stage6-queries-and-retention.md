# Stage 6 — unbounded queries and retention

Stage 4 concluded that throughput cannot be measured reliably on this machine.
This stage therefore works on what **can** be measured: query execution time,
which is a clean signal where throughput is noise.

Dataset at time of measurement: **233,437 ledger entries, 101,984 charges**.

---

## The hourly full-ledger scan

`reconcile.checkLedger` aggregated the **entire** `ledger_entries` table — no
`WHERE`, no time bound, no `LIMIT` — every hour, to find transactions whose
debits do not equal their credits.

| | Execution time |
|---|---|
| Before | **1,211 ms** |
| After, first run (watermark 0) | 471 ms |
| After, steady state | **54.8 ms** |

**22× faster, and the shape changed from O(all history) to O(recent).** The old
query grew forever: ~50 seconds at 10M entries, and past some size it would stop
finishing before the next hourly tick.

### Why it can be bounded safely

`ledger_entries` is append-only, enforced by `trg_ledger_entries_no_update`,
which raises on any `UPDATE` or `DELETE`. A transaction verified balanced can
therefore never become unbalanced — nothing can change its rows, and any new row
carries a new `transaction_id`. "Already checked" is a permanent property.

### The subtlety a naive watermark gets wrong

`BIGSERIAL` ids are assigned at `INSERT`, but rows become visible at `COMMIT`. A
long-running transaction can commit an entry whose id sits **below** a watermark
recorded by a scan that ran while it was still open — and that entry would never
be checked again.

So the predicate is `id > watermark OR created_at > now() - INTERVAL '1 hour'`.
The trailing window covers anything in flight during the previous run, and its
cost scales with recent throughput rather than total history.

Two further guards:

- Candidate transactions are found from the bounded set, then **every leg** of
  each is summed. A transaction must be judged whole, never on whichever legs
  happen to fall inside the window.
- The watermark advances **only when the run finished without error**. A run that
  failed partway may not have examined what it claimed, and advancing past
  unverified entries would leave them unchecked forever — precisely the failure
  this check exists to prevent.

### Proof it still catches an imbalance

The trigger was disabled, a deliberately unbalanced single-leg transaction
injected to simulate a write-path bug, and the **bounded** query run:

```
              transaction_id            | currency | delta
 44444444-4444-4444-4444-444444444444   | USD      |  5000
```

Detected. The injected row was then removed, both triggers confirmed enabled, and
the ledger verified balanced across all **97,684** transactions.

---

## Queries measured and deliberately NOT changed

The audit flagged these as bad shapes. They measure fine at current volume, and
changing them would be speculative:

| Query | Measured | Verdict |
|---|---|---|
| `reconcile.go:340` drift check | 448 ms | Already bounded per account by Stage 3's `idx_ledger_account_id` |
| `payouts.go:265` merchant balance from inception | 12.6 ms | Uses the Stage 3 index; 17,153 rows for the largest merchant |
| Dashboard balances, no time bound | 12.2 ms | Fine |
| `payouts.go:120` — no index on `verified_at` | 0.78 ms | One row in the table; an index would be for nothing |

**One exception, changed on evidence rather than volume:** `GetCharge` still
joined the `charge_refund_totals` view. It measured 0.774 ms — but its plan
showed a sequential scan over `refunds`, cheap only because that table is nearly
empty. It is the identical shape that measured **0.28 ms at 5 rows and 1,607 ms
at 500,000** in the list path. Replaced with the correlated subquery already used
there: **0.774 ms → 0.196 ms**.

---

## Retention

Before this, the only scheduled deletion in the entire system was the vault's
token purge. Everything else grew forever.

`internal/retention` prunes seven tables on an hourly tick, each statement
bounded by both a predicate and a `LIMIT` of 10,000, each in its own transaction
so one failure cannot roll back the others.

| Table | Kept | Condition |
|---|---|---|
| `outbox_events` | 7 days | published only |
| `webhook_delivery_attempts` | 30 days | parent delivery terminal |
| `webhook_deliveries` | 30 days | `delivered` or `dead` only |
| `merchant_sessions` | 7 days | expired or revoked |
| `checkout_sessions` | 30 days | terminal states only |
| `vault_access_log` | 90 days | — |
| `reconciliation_runs` | 90 days | no unresolved discrepancies attached |

### What is deliberately not pruned

- **`ledger_entries`, `charges`, `refunds`, `disputes`, `payouts`** — the
  financial record. Growth is a partitioning problem, not a deletion one.
- **Unresolved `reconciliation_discrepancies`** — an open finding about
  unaccounted money.
- **`idempotency_keys`, and this one is the important call.**

`expires_at` and `idx_idem_expiry` exist on that table and look built for exactly
this job. But **nothing in `idempotency.Begin` ever reads `expires_at`** — a grep
confirms it. Keys are currently honoured *forever*, which is the safest possible
behaviour at the cost of unbounded growth.

Pruning them would change money behaviour: a retry carrying a pruned key creates
a **second charge** instead of replaying the first. Making that TTL real is a
product decision about how long a retry stays safe, and it belongs in the
idempotency layer where it can be enforced explicitly — not as an emergent
side effect of a cleanup job. **Left alone, and flagged.**

At 121,972 rows it is currently the second-largest table in the system.

### Verified against real data

All seven statements executed against the live database: 3 outbox events, 2
expired merchant sessions, 2 terminal checkout sessions pruned. `ledger_entries`,
`charges` and `idempotency_keys` untouched. Ledger balanced across all 97,684
transactions afterwards.

Note the counts are small because nothing in this database is old enough to
qualify — which is the correct behaviour, not a broken job.

---

## Correctness

Full suite green across all seven packages. Ledger balanced across **97,684**
transactions. Both ledger triggers verified enabled after the imbalance-injection
test.
