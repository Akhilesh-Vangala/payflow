# The payment transaction flow

Every database statement a charge executes, in order, with its transaction, its
locks, and what it costs. This is the reference the scaling work is measured
against — `docs/ARCHITECTURE.md` explains *why* the system is shaped this way;
this document records *exactly what it does*.

Written from an audit of the code, not from the comments. Two things the audit
corrected:

- The code comments describe **two** transactions. There are **three**.
- The operation count was believed to be ~15. It is **17 statements**, plus
  3 `BEGIN` and 3 `COMMIT` — **23 round trips** — plus three deferred trigger
  aggregate scans that run at commit.

---

## The charge path, statement by statement

Entry point `internal/payments/service.go:152`. Steady state: accounts already
exist, no idempotency conflict, fee > 0 (three ledger legs).

| # | Statement | Where | Tx | R/W | Lock taken |
|---|---|---|---|---|---|
| | `BEGIN` | `db.go:52` | **tx1** | | |
| 1 | `INSERT INTO idempotency_keys … ON CONFLICT (merchant_id, idempotency_key) DO NOTHING RETURNING …` | `idempotency.go:157` | tx1 | W | Speculative-insert lock on the unique index. **Concurrent same-key callers block here until tx1 commits.** Plus `FOR KEY SHARE` on the `merchants` row via FK. |
| | `COMMIT` | | tx1 | | |
| | *network — `vault.Metadata`* | `service.go:217` | — | | outside any transaction ✓ |
| | `BEGIN` | | **tx2** | | |
| 2 | `INSERT INTO charges (…) VALUES (…,'pending',…)` | `service.go:249` | tx2 | W | New tuple; conflict lock on `uq_charges_merchant_idempotency`; FK `FOR KEY SHARE` on `merchants`. |
| | `COMMIT` | | tx2 | | |
| 3 | `SELECT avg(amount_cents), stddev_samp(…) FROM (… WHERE card_fingerprint=$1 …)` | `service.go:732` | **none** | R | Index scan on `idx_charges_fingerprint`. Autocommit. |
| | *risk assessment — Redis pipeline + HTTP model call, no Postgres* | `service.go:313` | — | | |
| 4 | `UPDATE charges SET risk_score, risk_level, risk_rules_fired …` | `service.go:486` | **none** | W | Row-exclusive on the `charges` row, briefly. Autocommit. |
| 5 | `INSERT INTO risk_assessments … ON CONFLICT (charge_id) DO NOTHING` | `service.go:513` | **none** | W | Autocommit. FK `FOR KEY SHARE` on `charges`. |
| | *network — `vault.Detokenize`* | `service.go:360` | — | | outside any transaction ✓ |
| | *network — `bank.Charge`* | `service.go:374` | — | | outside any transaction ✓ |
| | `BEGIN` | `service.go:584` | **tx3** | | |
| 6 | `SELECT id FROM accounts …` — customer receivable | `ledger.go:237` | tx3 | R | none |
| 7 | `SELECT id FROM accounts …` — merchant balance | `ledger.go:239` | tx3 | R | none |
| 8 | `SELECT id FROM accounts … merchant_id IS NULL` — platform fees | `ledger.go:237` | tx3 | R | none |
| 9 | `INSERT INTO ledger_entries` — leg 1, debit receivable | `ledger.go:134` | tx3 | W | New tuple; FK `FOR KEY SHARE` on `accounts`; **queues a deferred trigger event**. |
| 10 | `INSERT INTO ledger_balances … ON CONFLICT (account_id) DO UPDATE` | `ledger.go:166` | tx3 | W | 🔒 **Row lock — merchant's receivable balance.** Held to COMMIT. |
| 11 | `INSERT INTO ledger_entries` — leg 2, credit merchant balance (net) | `ledger.go:134` | tx3 | W | as #9 |
| 12 | `INSERT INTO ledger_balances … DO UPDATE` | `ledger.go:166` | tx3 | W | 🔒 **Row lock — merchant's balance.** Held to COMMIT. |
| 13 | `INSERT INTO ledger_entries` — leg 3, credit platform fees | `ledger.go:134` | tx3 | W | as #9 |
| 14 | `INSERT INTO ledger_balances … DO UPDATE` | `ledger.go:166` | tx3 | W | 🔒🔒🔒 **Row lock — the platform-wide `platform_fees` row for this currency. Shared by every merchant on the platform.** Held to COMMIT. |
| 15 | `UPDATE charges SET status, processor_reference, ledger_transaction_id …` | `service.go:591` | tx3 | W | Row-exclusive; fires `trg_charges_terminal_is_final`. |
| 16 | `INSERT INTO outbox_events` | `service.go:627` | tx3 | W | New tuple. |
| 17 | `UPDATE idempotency_keys SET status, response_status, response_body …` | `idempotency.go:272` | tx3 | W | Row-exclusive. |
| | `COMMIT` → **`trg_ledger_balanced` fires 3×**, once per inserted entry, each running `GROUP BY currency HAVING SUM(…) <> 0` over `ledger_entries WHERE transaction_id = …` | `0001:145` | tx3 | R×3 | Runs **while all three balance locks are still held**. |

### Variants

| Case | Statements |
|---|---|
| Fee = 0 (two legs) | 15 |
| Idempotency conflict | +1 or +2 (`FOR UPDATE`, stale-lock steal) |
| Cold accounts | up to +6 (`INSERT … RETURNING` and a re-`SELECT` per missing account) |
| `failClaimed` / `releaseClaim` | each opens **an additional transaction** |
| Dispute (4 legs) | `Post` alone issues 8 statements; trigger runs 4× |

---

## Where the time goes

Measured at 100 concurrent, one merchant:

```
p50 charge                948 ms
  ├─ vault tokenize         7 ms
  ├─ vault metadata         2 ms
  ├─ vault detokenize      12 ms
  ├─ bank authorize        24 ms   (1,282/s in isolation)
  ├─ fraud scorer       ≤  50 ms   (timeout, fails open)
  └─ blocked on locks    ~850 ms   ← 89%
```

Observed directly in `pg_stat_activity` under load:

```
6527 ms  INSERT INTO ledger_balances (account_id, balance_cents, …)
5487 ms  INSERT INTO ledger_balances (account_id, balance_cents, …)
3320 ms  INSERT INTO ledger_balances (account_id, balance_cents, …)
```

---

## Why the ordering is what it is

Each boundary earns its place. None of these can be rearranged for speed without
losing a guarantee:

**tx1 claims the idempotency key and nothing else.** A retry must never
re-execute a precondition the first attempt may have changed. The single-use
vault token is exactly such a precondition — an earlier version fetched vault
metadata before claiming, so a retry of a *successful* charge failed with
"invalid token" instead of replaying.

**Every network call sits outside a transaction.** A call inside one holds row
locks for the duration of somebody else's latency; under load that alone
exhausts the pool. Verified: no network call anywhere in the money path is
inside a transaction.

**The charge is inserted as `pending` before the bank is called.** A crash
mid-flight then leaves a durable record to reconcile against rather than a charge
nobody knows happened.

**tx3 commits the ledger entries, the charge status, the outbox row and the
idempotency response together.** Splitting them permits a completed charge with
no ledger entry, or an event announcing money that never moved. This is the
transactional-outbox guarantee, and it is why there is no message broker.

---

## Locks, complete inventory

### Explicit `SELECT … FOR UPDATE`

| Where | Table | Contended row |
|---|---|---|
| `idempotency.go:185` | `idempotency_keys` | `(merchant_id, key)` — conflict path only |
| `refund.go:126` | `charges` | the charge being refunded |
| `dispute.go:94` | `charges` | the disputed charge — **contends with refunds on the same row** |
| `dispute.go:258` | `disputes` | the dispute |
| `payouts.go:333` | `payouts` | the payout — settlement callback only, not the batch |

### `ON CONFLICT DO UPDATE` — the hot one

`ledger.go:166`, `applyBalance`. **Three locks per charge**: merchant
receivable, merchant balance, and the platform-wide `platform_fees` row.

The third is the ceiling. Every successful charge, dispute and won-dispute in a
currency serialises on that one row for the remainder of its transaction.

**Measured:** load spread across 1 / 4 / 12 / 24 merchants gives
**75 / 64 / 91 / 72 TPS** — flat. Removing the fee leg: **75 → 135 TPS**.
Sharding by `merchant_id` cannot fix this, because the row is shared across
every shard.

### `ON CONFLICT DO NOTHING` — speculative-insert locks

`idempotency_keys` (`idempotency.go:157`), `charges` (`service.go:256`),
`risk_assessments` (`service.go:520`), `disputes` (`dispute.go:144`),
`payouts` (`payouts.go:193`), `accounts` (`ledger.go:253`, cold start only).

---

## Triggers

Three exist across all 12 migrations.

| Trigger | Timing | Scope | Cost on the charge path |
|---|---|---|---|
| `trg_ledger_entries_no_update` | `BEFORE UPDATE OR DELETE` | row | Never fires — inserts only. A guard. |
| `trg_ledger_balanced` | `AFTER INSERT`, **`DEFERRABLE INITIALLY DEFERRED`** | **row** | **Runs the balance aggregate once per leg at COMMIT** — 3× per charge, 4× per dispute — while all balance locks are held. |
| `trg_charges_terminal_is_final` | `BEFORE UPDATE OF status` | row | Fires on `finalize`. Not on `recordRisk` (status isn't in that SET list). |

`trg_ledger_balanced` is one of three independent layers guaranteeing debits
equal credits — the others being the check in Go before the write, and
reconciliation re-deriving from entries. **It cannot be removed.** It can
reasonably be made to run once per transaction rather than once per row.

---

## Statement preparation

`internal/db/db.go` never sets `DefaultQueryExecMode`, so pgx v5 uses
`QueryExecModeCacheStatement` — extended protocol, server-side `PREPARE` per
unique SQL text, cached per connection.

Two consequences worth recording:

- **No `pgx.Batch` and no `CopyFrom` anywhere in the money path.** Every
  statement above is an individual round trip; three ledger legs are six serial
  round trips.
- `EnsureAccount` builds its SQL by string concatenation (`ledger.go:225`),
  producing two distinct texts — `merchant_id IS NULL` versus `merchant_id = $3`
  — which occupy two prepared-statement cache slots for one logical query.

**Unresolved conflict:** `README.md`, `deploy/README.md` and
`docs/ARCHITECTURE.md` all state PgBouncer runs in **transaction pooling mode**.
Transaction mode does not preserve per-connection prepared statements. The code's
exec mode and the documented deployment topology are not aligned, and nothing in
the codebase reconciles them.

---

## The charge state machine

What the code implements, which differs from a classic auth/capture gateway:

```
                    ┌─────────┐
                    │ pending │  ← inserted before the bank is called
                    └────┬────┘
          ┌──────────────┼──────────────┬─────────────────┐
          ▼              ▼              ▼                 ▼
    ┌───────────┐  ┌──────────┐  ┌──────────────┐  ┌──────────────┐
    │ succeeded │  │  failed  │  │  requires_   │  │  requires_   │
    │           │  │          │  │ reconciliation│  │   action     │
    └───────────┘  └──────────┘  └──────┬───────┘  └──────────────┘
                                        │                 ▲
                                        ▼                 └── 3DS, not built
                              hourly job asks the
                              processor, then posts
                              what it finds
```

**There is no authorize/capture split.** `bank.Charge` authorizes and settles in
one message, so there is no `AUTHORIZED` state and no batch capture. Separating
them is a genuine throughput lever — real gateways batch captures — but it
changes what a merchant sees from "succeeded" to "authorized", so it is a product
decision rather than an optimization.

`requires_reconciliation` is the ambiguous case and the reason the whole ordering
above exists: **no ledger entries are written**, the API returns 202, and the
merchant is told not to ship and not to retry.

---

## Duplicate-processing paths

Every route by which the same logical payment could be processed twice, and what
prevents it:

| Path | Prevented by |
|---|---|
| Customer double-clicks Pay | Same idempotency key → speculative-insert lock → replay |
| Merchant retries after a timeout | Same key → replay of the stored response, byte for byte |
| Two servers, same key, simultaneously | The unique constraint is the lock, and it is global across pods |
| Server crashes after charging, before responding | Idempotency record and ledger write commit together — both or neither |
| Bank succeeded but the response was lost | `requires_reconciliation`; **never blind-retried** |
| Chargeback notification redelivered | `disputes.processor_reference` is `UNIQUE NOT NULL` |
| Payout batch runs twice | `payouts UNIQUE (merchant_id, currency, period_end)` |
| Outbox event polled twice | `FOR UPDATE SKIP LOCKED`, then marked published in the same transaction |
| Webhook delivered twice | At-least-once **by design**; the merchant deduplicates on event id |

---

## Known defects found during this audit

Recorded here rather than fixed silently. All are money or delivery bugs.

1. **Webhook event IDs collide.** `outbox.go:236` derives the event id as
   `uuid.NewSHA1(ns, aggregate_id ‖ event_type)` — deterministic — and inserts
   with `ON CONFLICT (endpoint_id, event_id) DO NOTHING`. Two events of the same
   type on the same aggregate collapse into one delivery.
2. **Payout events are silently dropped.** `merchantFromPayload` routes on
   `payment` / `refund` / `dispute` prefixes only, so `payout.paid` and
   `payout.failed` are discarded — **and still marked published**.
3. **Reconciliation only looks at charges.** `refunds`, `payouts` and
   `checkout_sessions` all have unresolved states that nothing ever sweeps.
   `reconciliation_runs.refunds_checked` is never populated.
4. **`payouts.setStatus` has no status guard** (`payouts.go:439`), unlike
   `reverse` which does. A late callback can overwrite a terminal state.
5. **Webhook 4xx responses are retried** (`worker.go:225`), so a permanently
   rejecting endpoint consumes all 11 attempts across 27 hours.
6. **Nothing is pruned** except vault tokens. `outbox_events`,
   `webhook_deliveries`, `idempotency_keys`, `checkout_sessions`,
   `merchant_sessions` and `vault_access_log` grow without bound.
   `idx_idem_expiry` exists for a cleanup path that does not exist, and
   `outbox_cursor` is referenced by zero Go code.
