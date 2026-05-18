# PayFlow — Architecture

Everything in the system, why each piece exists, and what it cost to get wrong.

Companion documents: [`README.md`](../README.md) for the summary,
[`payment-gateway-system-design_updated.md`](payment-gateway-system-design_updated.md)
for the original design (§ references throughout point there), and
[`../ml/MODEL_CARD.md`](../ml/MODEL_CARD.md) for the fraud model.

---

## Contents

1. [What the system is](#1-what-the-system-is)
2. [Services](#2-services)
3. [The database](#3-the-database) — every table, and why
4. [The charge flow, step by step](#4-the-charge-flow-step-by-step)
5. [Money movement](#5-money-movement)
6. [Fraud detection](#6-fraud-detection)
7. [The card vault](#7-the-card-vault)
8. [Webhooks](#8-webhooks)
9. [Reconciliation](#9-reconciliation)
10. [The stack, and why each choice](#10-the-stack-and-why-each-choice)
11. [Bugs this codebase paid for](#11-bugs-this-codebase-paid-for)
12. [What is not built](#12-what-is-not-built)
13. [Running it](#13-running-it)

---

## 1. What the system is

A payment gateway. Merchants integrate one API and one webhook endpoint and
get charges, refunds, disputes, fraud screening and payouts — the role Stripe
plays for the businesses built on it.

Two problems drive every design decision:

**Exactly-once processing.** A client retries a charge because the response
was lost. The customer must be charged once. Not once-ish.

**A financially correct ledger.** Money is never created or destroyed. Every
transaction balances, per currency, and history is never rewritten.

Everything else — the vault, the risk engine, reconciliation — exists to make
those two true under real conditions: crashes, timeouts, concurrency, and
people actively trying to steal.

**It does not move real money.** The acquiring bank is a simulator. That is
the one piece structurally impossible to build without a sponsor bank
relationship, so it is built as production software with deterministic test
hooks rather than faked with a random number (§25.2).

---

## 2. Services

Six binaries in `cmd/`, plus a Python scoring service. Each is independently
deployable.

| Service | Port | Responsibility |
|---|---|---|
| `payments-api` | 8080 | Public REST API and the merchant dashboard's read API |
| `vault` | 8081 | Card tokenization. **The only service that ever sees a PAN** |
| `bank-simulator` | 8090 | The acquiring bank, with deterministic test hooks |
| `webhook-worker` | — | Outbox poller plus delivery with retry, backoff and DLQ |
| `scheduler` | — | Reconciliation (hourly), payouts (daily) |
| `healthcheck` | — | A tiny probe binary, because distroless images have no shell |
| `fraud-service` (Python) | 8000 | XGBoost scoring behind a hard timeout |

Plus `web/dashboard` (React SPA, :5173) and `examples/merchant-server` — the
latter is what a *merchant* writes, not something PayFlow runs.

### Why the vault is separate

It is the entire PCI-scope argument. A raw card number exists in exactly one
process, in one network segment, with one KMS key. Every other service —
payments, ledger, dashboard, every log line — sees an opaque token,
`card_last4` and `card_bin`.

A service that cannot see a PAN even in principle is out of PCI scope
entirely, however much money flows through it. Merging the vault into
`payments-api` would put the whole system in scope.

### Why the bank simulator is real software

It is the load-bearing fake. Stateless, horizontally scalable, deterministic
by header (`X-Simulate-Outcome: success | decline | timeout | delayed_success
| network_error`). A random-outcome script would make every failure-path test
flaky, and a flaky test that only sometimes exercises the dispute path is
close to useless.

---

## 3. The database

PostgreSQL. **21 tables, 2 views, 3 triggers**, in 10 numbered migrations
applied in filename order.

Postgres does three jobs here that would otherwise need separate
infrastructure: it is the distributed lock, the correctness enforcer, and the
transaction boundary that makes the outbox pattern work.

### 3.1 Identity and access

**`merchants`** — name, email, country, currency, risk tier, business
category. `category` feeds the fraud model; `avg_amount_cents` backs the
amount-anomaly rule.

**`api_keys`** — how a merchant authenticates. Stores `key_hash` (SHA-256),
never the key. `key_prefix` is kept so a dashboard can show `sk_test_abc1…`.

> **Why plain SHA-256 and not bcrypt.** API keys are high-entropy random
> values, not user-chosen passwords, so there is no dictionary to attack and
> no need for a slow KDF. It also has to be *indexable*: a per-row salted hash
> cannot be looked up, and every request would scan the table. Passwords, when
> the auth service is built, need argon2 — the opposite reasoning.

### 3.2 Idempotency — the centerpiece

**`idempotency_keys`** — `UNIQUE (merchant_id, idempotency_key)`, plus
`request_hash`, `status`, `response_body`, `response_status`, `locked_at`,
`expires_at`.

That unique constraint **is** the lock. `INSERT ... ON CONFLICT DO NOTHING`
either succeeds (this process owns the request) or does not. There is no
window between checking and claiming, so there is nothing to race.

> **Why not Redis.** A distributed lock on Redis is hard to make correct and
> is not something to bet money on. The database already has to be in the
> transaction for the ledger write, so it does the job for free and with
> stronger guarantees. Redis is a read-through cache for replays and a
> velocity counter, never a system of record.

`request_hash` is a canonical-JSON SHA-256, so a client whose HTTP library
reorders JSON keys between retries is not falsely rejected. A *different* hash
under the same key is a client bug and returns 422.

`locked_at` is compared against Postgres `now()`, never a pod clock — wall
clocks drift across a fleet and this value decides whether work gets
duplicated.

### 3.3 The ledger

**`accounts`** — one per (merchant, type, currency). Types:
`merchant_balance`, `customer_receivable`, `platform_fees`, `in_transit`,
`paid_out`, `merchant_debt`, `reserve`. Platform accounts have
`merchant_id IS NULL`, enforced by a partial unique index.

**`ledger_entries`** — `transaction_id`, `account_id`, `direction`
(`debit`/`credit`), `amount_cents BIGINT CHECK (> 0)`, `currency`,
`entry_type`, `metadata`.

Two triggers make this safe:

```sql
trg_ledger_entries_no_update   -- rejects UPDATE and DELETE outright
trg_ledger_balanced            -- DEFERRABLE INITIALLY DEFERRED
```

> **Why the balance check is deferred.** It runs at COMMIT, not per statement,
> so a transaction can insert its debit leg and credit leg separately and
> still be validated as a unit. A non-deferred constraint would reject every
> legitimate write after the first leg.

> **Why append-only is a trigger, not a convention.** Corrections are new
> balanced entries referencing the original transaction. With the trigger, a
> bug in Go *cannot* corrupt the ledger. Without it, "we don't do that" is a
> policy, and policies get violated at 3am.

**`ledger_balances`** — the cached running balance per account, and the one
deliberate denormalization. The authoritative value is always
`SUM(ledger_entries)`; this exists so reads do not scan history.
Reconciliation re-derives and compares.

Updated with a single atomic `INSERT ... ON CONFLICT DO UPDATE SET balance =
balance + excluded`, which has no read-then-write window to go stale.

### 3.4 Charges and reversals

**`charges`** — amount, currency, status, failure code, card metadata
(`card_last4`, `card_bin`, `card_brand`, `card_fingerprint` — never a PAN),
risk summary, `payment_token`, `ledger_transaction_id`, `processor_reference`.

Statuses: `pending`, `requires_action`, `succeeded`, `failed`,
**`requires_reconciliation`**.

> **That last one is the whole ambiguity contract.** If the processor call
> times out, the money may have moved. The charge is parked, *no ledger
> entries are written*, and reconciliation resolves it against the processor's
> log. Never blind-retry, never assume failure.

Protected by `trg_charges_terminal_is_final`: once `succeeded` or `failed`, the
status cannot change. And `uq_charges_merchant_idempotency` — a partial unique
index — makes two charge rows for one idempotency key impossible.

**`refunds`** — full or partial, each with its own idempotency key.

**`charge_refund_totals`** (view) — `refunded_cents` and `committed_cents`.

> **The subtlety that cost a critical bug.** `committed_cents` counts
> everything `<> 'failed'`, including `requires_reconciliation`. An earlier
> version counted only `succeeded` and `pending`, so an ambiguous refund
> released its reservation and a second full refund could take a 10000 charge
> to 20000. The ledger stayed balanced throughout — only the second refund
> posted legs — so no balance assertion could have caught it. Reserve unless
> proven not to have moved.

**`disputes`** — chargebacks. Opening one reverses funds *immediately*, before
any evidence is reviewed, because that is how card networks work. Modelling it
otherwise would overstate available balance.

`processor_reference` is `NOT NULL UNIQUE`, which is what deduplicates
redelivered chargeback notifications. It was nullable once, and Postgres treats
every NULL as distinct, so `ON CONFLICT` never fired.

> **The asymmetry.** Winning reverses the reversal and returns the fee. Losing
> posts **nothing** — the money already moved when the dispute opened. Posting
> again would debit the merchant twice for one chargeback.

### 3.5 The vault

**`vault_tokens`** — `pan_ciphertext`, `encrypted_data_key`, `key_id`, plus
non-sensitive metadata and `behavior` (keystroke aggregates).

Envelope encryption: a random per-record AES-256-GCM data key, itself
encrypted under a KMS master key. Rotating the master key re-wraps small key
blobs instead of re-encrypting every stored card.

> **GCM, not CBC.** Authenticated encryption means tampered ciphertext fails to
> decrypt rather than silently returning a *different* card number.

> **CVCs are never stored.** No column, anywhere. PCI DSS prohibits it
> outright, with no exemption for encryption.

Tokens are single-use by default and consumed inside the same transaction that
reads them, so a leaked token cannot be replayed.

**`vault_access_log`** — every tokenize and detokenize, with caller and reason.
Both are required fields: an access record that does not say who asked or why
is not an audit trail.

### 3.6 Risk

**`risk_assessments`** — one row per charge: level, rule score, rules fired,
model score and version, `model_skipped`, and snapshots of `behavior` and
`velocity`, plus what action was taken and how long scoring took.

> **Why a separate table when `charges` already has a summary.** Three
> reasons. The evidence lives on the vault token, which is purged after 15
> minutes — by the time a chargeback arrives three weeks later it is gone.
> Analyst queries would otherwise scan `charges`, the hot table the charge
> path writes to. And chargeback outcomes are the model's training labels
> (§14.3): without the feature values recorded *at decision time*, a confirmed
> fraud case teaches nothing.

Review columns (`verdict`, `reviewed_by`, `review_note`) close the loop: a
decline that turns out to be a false positive is how thresholds get tuned.

**`risk_review_queue`** (view) — flagged and unreviewed, joined to the charge,
with a `disputed` flag showing whether the prediction was right.

### 3.7 Events and delivery

**`outbox_events`** — written in the **same transaction** as the ledger entry.

> This is the whole transactional-outbox point (§22.1). Charge → ledger →
> broker is a distributed transaction across a database and a message broker.
> If the ledger commits but the publish fails, money moved and no event fired.
> Writing both in one Postgres transaction makes that impossible; a poller
> publishes afterwards.

Indexed partially (`WHERE NOT published`) so the poller's index stays small as
published rows grow into millions.

**`webhook_endpoints`** — URL, signing secret, and `previous_secret` with an
expiry. Rotation needs a window where **both** secrets verify, or rotating
breaks every delivery in flight (§22.2).

**`webhook_deliveries`** — one row per (event, endpoint).
`UNIQUE (endpoint_id, event_id)` makes the outbox fan-out idempotent: a
re-read outbox row cannot create a second delivery, so "did I already publish
this?" never has to be answered correctly.

`next_attempt_at` implements backoff by being pushed forward rather than by
sleeping, so a worker holds no state between attempts and can die at any point.

**`webhook_delivery_attempts`** — per-attempt history for the merchant-facing
log. Merchants need to see *why* it failed three times, not just that it did.

### 3.8 Payouts

**`payout_accounts`** — bank details, stored the same way as cards: only
`account_last4` and a token.

**`payouts`** — `UNIQUE (merchant_id, currency, period_end)`.

> A payout batch has no client idempotency key, so that constraint plays the
> role: a crashed re-run over the same window conflicts instead of paying
> twice.

Funds move `merchant_balance → in_transit` at initiation and `in_transit →
paid_out` only when the bank confirms, because an ACH can fail days later on a
bad routing number. A failure returns the funds rather than losing them.

### 3.9 Reconciliation

**`reconciliation_runs`** and **`reconciliation_discrepancies`** — kinds:
`unbalanced_transaction`, `balance_drift`, `processor_mismatch`,
`orphaned_charge`.

> **Discrepancies are recorded and alerted, never auto-corrected.** Silently
> fixing a financial mismatch buries the bug instead of finding it (§24.3).
> Resolution is a human action; nothing clears itself.

---

## 4. The charge flow, step by step

`POST /v1/charges` with `Authorization: Bearer sk_...` and an
`Idempotency-Key`.

```
 1. Authenticate           SHA-256 the key -> api_keys -> merchant_id
 2. Validate               amount > 0, currency, payment_token present
                           reject any Luhn-valid digit run in the body
 3. tx1  CLAIM THE KEY     INSERT ... ON CONFLICT DO NOTHING
                              conflict + completed -> replay stored response, DONE
                              conflict + in flight -> 409
                              conflict + stale lock -> steal and proceed
 4. Card metadata          vault: brand, last4, BIN, fingerprint, behavior
                           NEVER a PAN
 5. RISK                   rules + velocity + model
                              high -> decline here. Bank never contacted,
                                      card never decrypted, no ledger entries
 6. tx2  Record intent     charge row as 'pending'
 7. Detokenize             the PAN's only appearance outside the vault,
                           held in one local variable
 8. Bank call              OUTSIDE any transaction
 9. tx3  Finalize          ledger + charge status + outbox + idempotency
                           response, ALL in one transaction
```

### Why this order

**The idempotency claim is first.** Before *any* validation that could fail.
A successful charge consumes its single-use vault token, so on a retry the
token is legitimately gone. Checking the token first meant a retry failed with
"invalid token" instead of replaying — the exact confusion idempotency exists
to prevent. **A retry must never re-execute a precondition the first attempt
may have changed.**

**Risk runs before the bank.** A declined-for-risk charge must never reach the
processor, and a retry must replay the stored decline rather than re-scoring —
velocity counters move between attempts, so a second assessment could reach a
different verdict and make the same request non-idempotent.

**The bank call is outside every transaction.** A network call inside a
transaction holds row locks for the duration of someone else's latency; under
load that alone exhausts the connection pool.

**Everything terminal commits together.** Ledger entries, charge status,
outbox row and idempotency response in one `pgx.Tx`. Splitting them allows a
completed charge with no ledger entry, which is unrecoverable.

### Transient vs terminal failures

A vault 503 is **not** the same as an invalid token. An earlier version
recorded any metadata error as a permanent `invalid_payment_token`, so a
client retrying correctly would replay that stored 400 forever — a valid token
permanently unusable because the vault blinked. Transient failures now
*release* the claim so a retry starts fresh.

---

## 5. Money movement

Every flow is a balanced set of legs. Fee is 290 bps.

| Event | Legs |
|---|---|
| Charge 10000 | debit `receivable` 10000 · credit `merchant_balance` 9710 · credit `platform_fees` 290 |
| Refund 10000 | debit `merchant_balance` 10000 · credit `receivable` 10000 |
| Dispute 10000 | debit `merchant_balance` 10000 · credit `receivable` 10000 · debit `merchant_balance` 1500 · credit `platform_fees` 1500 |
| Dispute won | the reverse, fee included |
| Dispute lost | **nothing** — the money moved when it opened |
| Payout | debit `merchant_balance` · credit `in_transit` |
| Payout confirmed | debit `in_transit` · credit `paid_out` |
| Payout failed | debit `in_transit` · credit `merchant_balance` |

> **The platform fee is not reversed on a refund.** A merchant taking 10000
> nets 9710, refunds 10000, and ends 290 down. That is what real processors
> do. Reversing it would silently make refunds free for merchants and leave
> the platform short.

### What "balance" means

Deliberately four numbers, not one:

- **Available** — settled, minus what is reserved against open disputes
- **Pending** — has not cleared the T+2 hold
- **Reserved** — held against disputes still open
- **In transit** — sent, awaiting bank confirmation

A merchant shown only a total will plan against money they cannot take.

---

## 6. Fraud detection

Two layers, and the ordering is the design.

### Rules first — `internal/risk`

**18 rules, ~450ns per transaction**, each explainable to a customer or a
regulator.

| Rule | Action | Signal |
|---|---|---|
| `card_testing_declines` | **block** | Repeated declines — working through stolen numbers |
| `device_card_cycling` | **block** | Many distinct cards from one device |
| `automated_card_entry` | **block** | Pasted *and* machine speed |
| `velocity_card_hourly` | challenge | Charge burst on one card |
| `superhuman_keystroke_rate` | score 35 | Mean keystroke gap under 30ms |
| `impossible_entry_speed` | score 35 | Whole form under 1.2s |
| `automation_typing_rhythm` | score 35 | Metronomic intervals |
| `high_risk_bin`, `geo_mismatch` | score 30, 25 | Issuer risk, incoherent geography |
| `velocity_ip_hourly`, `velocity_card_daily` | score 20, 25 | Burst traffic |
| `disposable_email`, `card_number_pasted`, `high_correction_rate` | score 20, 15, 15 | Weak signals |
| `large_amount`, `cvc_hesitation`, `odd_hour` | score 15, 10, 10 | Weaker still |

Thresholds: **medium ≥ 30, high ≥ 70.**

**Calibration is deliberately asymmetric.** A declined legitimate customer is a
lost sale and often a lost relationship; a missed fraudulent charge costs the
disputed amount plus a fee. For most merchants the first is worse, so
borderline cases challenge rather than block. Pasting a card alone does **not**
block — password managers paste.

Every rule runs even after one fires, so the stored explanation lists
everything suspicious rather than whichever was checked first.

### Behavioral biometrics

We own the checkout iframe, so we can see *how* a card was entered — nobody
else in the flow can.

Signals sent to the vault (never to the merchant, so a compromised integration
cannot forge or suppress them): paste detection, keystroke interval mean and
coefficient of variation, correction rate, CVC hesitation, total duration.

**Privacy line:** keystroke dynamics are biometric data under GDPR Art. 9 and
Illinois BIPA. Only non-invertible **aggregates** cross the boundary — never a
per-keystroke timing array, which would predict better and is deliberately not
collected.

> **What Selenium taught us.** Driving the real checkout with a bot showed the
> rhythm rule looking for the wrong thing. The bot's coefficient of variation
> was **1.06** — *higher* than the human's 0.40 — because at a 1ms mean
> interval, timer jitter is proportionally enormous. "Suspiciously even" never
> fires at machine speed. The mean is the signal that does not lie, and
> `superhuman_keystroke_rate` was added because of it. Current result: **2 of
> 2 bots blocked, human allowed through.**

### Model second — `ml/`

XGBoost on IEEE-CIS (real Vesta transactions). 21 features, **test PR-AUC
0.188**. Low, and honestly so — see the model card.

Two rules govern it:

- It can **raise** a verdict, never clear a rule block. A rule-blocked charge
  is not even sent for scoring.
- It is **never a hard dependency**. Timeout, outage or slowness falls open to
  rules behind a circuit breaker. Failing closed would turn a model outage into
  a total outage.

Velocity counters live in Redis (`INCR` + `EXPIRE NX`), not Postgres — an
aggregate query on the synchronous charge path cannot fit the sub-100ms budget
and gets slower exactly when traffic spikes.

---

## 7. The card vault

```
BROWSER                MERCHANT SERVER            PAYFLOW
card number ─────────────────────────────────────▶ vault (iframe on
 ◀────────────────────────────────────────────────  PayFlow's origin)
 token
 token ──────────────▶ holds the secret key
                       ──────────────────────────▶ POST /v1/charges
```

No two parties hold both halves. The browser has the card but no key. The
merchant has the key but never the card. Only the vault has both, in one small
audited service.

The iframe is served from the vault's origin, so the merchant's JavaScript
cannot reach into it — **that browser boundary is the PCI firewall**, not a
convention.

Enforcement is structural: `CardMetadata` has no number field, so the type
system prevents exposure. And `/v1/charges` scans request bodies for a
Luhn-valid digit run and rejects them, so a merchant cannot drag themselves
into PCI scope by mistake.

CSP on the frame: `script-src 'self'` (no inline scripts — an injected one
could read the PAN), `connect-src 'self'` (it could not exfiltrate anywhere),
`frame-ancestors` naming registered merchants.

---

## 8. Webhooks

**At-least-once, deliberately.** Exactly-once delivery to a third party we do
not control is not achievable: if their server accepts a request and dies
before we read the response, that is indistinguishable from never receiving
it, and one of "retry" or "give up" is wrong. So we retry and give merchants an
`event_id` to deduplicate on.

Signature: HMAC-SHA256 over `{timestamp}.{body}` — the timestamp is *inside*
the signature, so a captured webhook cannot be replayed forever. Verified with
a constant-time compare.

Retry ladder: 1s, 5s, 30s, 2m, 10m, 1h, 2h, 4h, 8h, 12h, then DLQ (`dead`).
Generous on purpose — a merchant being down for twenty minutes is normal, not
exceptional.

Jitter matters: without it every delivery queued during an outage retries at
the same instant and knocks the recovering server over again.

SSRF protection blocks loopback and private addresses; redirects are not
followed, since following one would let a merchant point us at internal
infrastructure after the URL was validated.

---

## 9. Reconciliation

Hourly. Two jobs, both answering "does our record match reality?"

1. **Resolve ambiguity.** Every charge in `requires_reconciliation` is queried
   against the processor's log. Authorized → post the ledger entries the
   charge path could not. Absent → the charge never landed, mark failed.
   Unreachable → leave parked and retry next run, rather than recording a
   false discrepancy.
2. **Re-derive the invariants.** Recompute per-transaction balance and cached
   vs derived account balances, **sharing no code** with the write path or the
   trigger — a check that reuses the logic it verifies only proves that logic
   is self-consistent.

Safe to re-run from scratch: `settle` is guarded on current status, so a
concurrent run or a late response cannot double-post.

---

## 10. The stack, and why each choice

| Choice | Why |
|---|---|
| **Go** | Cheap goroutines, fast compiles across six services, and what fintech infra teams actually run. Idempotency correctness comes from DB design, not language. |
| **PostgreSQL** | ACID, deferred constraints, `ON CONFLICT`, row locks, `SKIP LOCKED`. Doing three jobs here: lock, enforcer, transaction boundary. |
| **Redis** | Velocity counters and rate limits only. Never a system of record — losing it weakens detection but cannot lose money. |
| **Redpanda** | Kafka-compatible, far lighter locally. Multiple independent consumers of the same event is Kafka's shape; SQS is for point-to-point work queues. |
| **LocalStack** | The AWS SDK calls are *real*; only the endpoint changes. The integration code under test is the code that runs in production. |
| **React + TypeScript** | Dashboard only. TS because this project has twice shipped a silent contract mismatch where both sides compiled while disagreeing. |
| **Vanilla JS for `payflow.js`** | Merchants use React, Vue, WordPress, hand-written HTML. A widget requiring any of those forces a dependency on every merchant's stack. |
| **XGBoost, not deep learning** | <10ms inference, strong on tabular data, explainable enough to justify a decline. Every top solution on this dataset is boosted trees. |
| **testcontainers, not mocks** | Idempotency and ledger correctness depend on unique constraints, row locks, deferred triggers and `ON CONFLICT` semantics a mock does not reproduce. A mocked suite passes while the system double-charges people. |

### Scaling past this

- **Shard by `merchant_id`** — merchants do not share financial state, so
  cross-shard queries are rare. Trigger: sustained write IOPS > 70%.
- **PgBouncer, transaction mode** — hundreds of pods exhaust
  `max_connections` long before they exhaust the database.
- **Read replicas** for reporting; analytics must never touch the primary.
- **Redpanda → MSK, Postgres → Aurora** — configuration changes, which is the
  point of building against the real AWS SDK.

Zero-downtime migrations follow expand/contract: additive changes, batched
backfills, drop only once all readers have moved.

---

## 11. Bugs this codebase paid for

Kept because the failures are more instructive than the design.

**A refund left in `requires_reconciliation` released its reservation.** A
second refund under a different key could take a 10000 charge to 20000. The
ledger stayed balanced throughout, so no balance assertion could have caught
it. *Ambiguous is not failed* — a rule already applied on the charge path and
missed on the refund path.

**A test double more permissive than the real thing.** `fakeVault` accepted
consumed tokens while the real vault rejected them, so the suite stayed green
over an idempotency bug that broke on the first real retry.

**Feature/label misalignment.** `build_features` sorted rows by card and
returned them in that order while the caller paired them with labels in the
original order. Fraud's mean amount went from $554 to $64. The model trained
cleanly and scored at chance.

**Train/serve skew.** The model scored 0.97 offline and **0.29** through the
vector the Go client actually sent — six of eighteen features, the rest
defaulting silently.

**A sliding velocity window.** An unconditional `EXPIRE` instead of `EXPIRE NX`
meant a device used daily never expired and accumulated cards without bound,
eventually blocking a shared family tablet permanently.

**A CSP that broke its own page.** `script-src 'self'` with an inline script.
The frame loaded, the JavaScript never ran, tokenization hung until timeout.
Fixed by extracting the script, *not* by adding `unsafe-inline` — this is the
one document that can see a card number.

**Rules built but never wired.** The behavioral rules and their tests existed
for days while the pipeline that feeds them did not. The tests passed because
they constructed the struct directly.

Migration `0005_invariant_hardening` converts several of these from
conventions into database constraints.

---

## 12. What is not built

Per §0.1, documented rather than silently missing.

| | Status |
|---|---|
| Auth service (§8) — signup, email verification, JWT, self-serve keys | **Not built.** Merchants exist via SQL insert |
| 3DS step-up (§16) | Designed; `requires_action` exists in the idempotency state machine, flow not built |
| Sponsor bank, money transmitter licence | Not a technical problem |
| KYC / identity verification | Stubbed `always_approved`, documented |
| Sanctions/AML, PCI QSA audit | Require vendor contracts |
| Tax calculation and remittance | Avalara-shaped, out of scope |
| Entity graph (§14.4) | The path from PR-AUC 0.19 to ~0.49; needs production traffic |

---

## 13. Running it

```bash
make up            # 11 services
make seed          # prints an sk_test_... key, shown once
make test          # unit + integration, needs Docker

make dashboard     # merchant UI on :5173
export PAYFLOW_API_KEY=sk_test_...
make checkout      # example merchant checkout on :3000
```

Ports: payments 8080, vault 8081, fraud 8000, bank 8090, checkout 3000,
dashboard 5173, Mailhog 8025.

### Verifying the guarantees

```bash
make test-idempotency                     # 100 concurrent, one key -> 1 charge
./scripts/fraud-drill.sh                  # card testing, device cycling
python scratch/bot_attack.py --headed     # Selenium bot vs human
k6 run -e API_KEY=... test/load/idempotency.js
```

Ledger invariant, which must always return no rows:

```sql
SELECT transaction_id, currency,
       SUM(CASE WHEN direction='debit' THEN amount_cents ELSE -amount_cents END)
FROM ledger_entries GROUP BY 1,2 HAVING SUM(...) <> 0;
```

Every money path has a concurrency test, not just a happy-path one:

| Test | Asserts |
|---|---|
| 100 concurrent, one key | 1 bank authorization |
| 10 concurrent refunds of 2000 on a 10000 charge | exactly 5 succeed |
| 20 concurrent chargeback redeliveries | funds reversed once |
| 30 concurrent detokenizations of a single-use token | exactly 1 succeeds |
| 50 concurrent ledger writes to one account | no lost updates |

---

## 11. Current bottlenecks

Measured, not estimated. See [`PAYMENT_TRANSACTION_FLOW.md`](PAYMENT_TRANSACTION_FLOW.md)
for the statement-level map this comes from.

The system sustains **75 charges/second**. The cause is a single row.

Every charge calls `applyBalance` (`internal/ledger/ledger.go:166`) once per
ledger leg, and each call takes an exclusive lock on a `ledger_balances` row held
until the transaction commits. Three legs, three locks — and the third is the
platform-wide `platform_fees` row, shared by every merchant on the platform.

Two experiments locate it precisely:

| Test | Result |
|---|---|
| Identical load across 1 / 4 / 12 / 24 merchants | **75 / 64 / 91 / 72 TPS** — flat |
| Platform fee leg removed | **75 → 135 TPS** |

The flat result is the important one: **sharding by `merchant_id` does not fix
this**, because the contended row is shared across every shard. That is the
textbook answer for a payments system and it would have bought nothing here.

Under load, `pg_stat_activity` shows individual balance upserts blocking for
**3.3–6.5 seconds**. Of a 948ms p50 charge, roughly 850ms is lock wait —
downstream calls total only ~95ms. A CPU profile independently rules out the
application: 43% of samples sit in network wait, and the API never exceeds 16% of
eight cores while throughput is saturated.

### Second-order limits, not yet reached

- **Two unbounded hourly scans.** `reconcile.go:312` and `:340` aggregate the
  *entire* `ledger_entries` table with no `WHERE`, no time bound and no `LIMIT`,
  every hour. Cost grows with all history ever recorded.
- **`payouts.go:265`** sums a merchant's ledger from inception on every daily run.
- **No retention anywhere** except vault tokens. Seven tables grow without bound.
- **Reconciliation is capped at 500 charges per hourly run** regardless of
  backlog.

### Read path

Reads are a different system: **4,075 req/s** after two fixes — caching the
per-request API key lookup, and replacing an O(table) dashboard join measured at
1,607ms against 500,000 rows with a correlated subquery at 0.372ms.

`GetCharge` (`internal/dashboard/dashboard.go:530`) still joins the
`charge_refund_totals` view and carries the same shape of problem.
