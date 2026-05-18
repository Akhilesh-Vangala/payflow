# PayFlow — Build Log

How this was built, in order, from an empty repository. What was created at
each step, why that order, what broke, and what it cost.

For the end state see [`ARCHITECTURE.md`](ARCHITECTURE.md). This is the
journey — including the parts that were wrong.

**Where it ended up:** 128 files, ~13,800 lines of Go, 6 services, 21 tables,
10 migrations, 7 test packages.

---

## Stage 0 — What existed at the start

One file:

```
payment-gateway-system-design.md      812 lines, no code
```

A design document. No repository, no Go module, no dependencies. The very
first decisions were about what to build and in what order.

### The first real decision: order of construction

Not alphabetical, not "easiest first". The order was chosen so that **each
stage could be proven before the next depended on it**:

```
ledger        →  nothing depends on it, and everything depends on it being right
idempotency   →  needs the ledger to write into
bank simulator→  needs to exist before the charge flow can be tested at all
charge flow   →  ties the three together
concurrency   →  proves the whole thing
```

Building the API first would have meant writing the hard parts (ledger,
idempotency) against a moving target.

### Toolchain check, before writing anything

```
go 1.25.5 · docker 29.1 · python 3.11.9 · node 24.13
```

Worth doing first: the plan assumed testcontainers (needs Docker), an XGBoost
model (needs Python) and a React dashboard (needs Node). Discovering a missing
toolchain three days in is expensive.

---

## Stage 1 — The schema, before any Go

**`migrations/0001_init.up.sql`** was the first file written. Deliberately.

The whole system rests on constraints the database enforces, so the schema is
the specification. Writing Go first would have meant encoding invariants in
application code and retrofitting the constraints later, which never happens.

Created: `merchants`, `api_keys`, `idempotency_keys`, `accounts`,
`ledger_entries`, `ledger_balances`, `charges`, `outbox_events`.

Three decisions made here that everything else inherits:

**`UNIQUE (merchant_id, idempotency_key)`** — this is the distributed lock.
Not Redis. `INSERT ... ON CONFLICT DO NOTHING` is atomic, so there is no
check-then-act window to race.

**A `DEFERRABLE INITIALLY DEFERRED` constraint trigger** for ledger balance.
Deferred matters: it runs at COMMIT, so a transaction can insert its debit and
credit legs as separate statements and still be validated as a unit.

**A trigger rejecting `UPDATE`/`DELETE` on `ledger_entries`.** Append-only
enforced, not documented. A bug in Go cannot corrupt the ledger.

---

## Stage 2 — Money, ledger, idempotency

```
internal/money/money.go          int64 minor units, never a float
internal/ledger/ledger.go        double-entry posting
internal/idempotency/*.go        claim and replay
internal/db/db.go                pool and transaction helper
internal/httpx/*.go              error envelope, auth, tracing
```

`money.Amount` refuses cross-currency arithmetic — converting requires a rate
that must be recorded on the ledger entry (§20), so it can never happen
implicitly.

`ledger.Post` takes a `pgx.Tx`, not a pool. That signature is load-bearing:
the caller must be able to commit the ledger write, the idempotency record and
the outbox row in **one** transaction. Owning its own transaction would
reintroduce exactly the inconsistency the outbox pattern exists to prevent.

---

## Stage 3 — The bank simulator

Built **before** the charge flow, because the charge flow cannot be tested
without something to charge.

Made deterministic by header — `X-Simulate-Outcome: success | decline |
timeout | delayed_success | network_error` — rather than random. A random
simulator makes every failure-path test flaky, and a test that only sometimes
exercises the dispute path is close to useless.

The `timeout` case is the important one: it **authorizes** and then withholds
the response. That is what makes the ambiguous case reachable, which is the
hardest problem in payments.

---

## Stage 4 — The charge flow and the proof

`internal/payments/service.go`, then the test that is the point of the
project:

```
TestConcurrentDuplicateIdempotencyKey
  100 goroutines · 1 idempotency key
  → 1 bank authorization, 1 charge, 99 got 409
```

### A deviation from the design doc, made explicitly

§25.4 says all 100 responses should be identical. That is true of *sequential*
retries but not a simultaneous burst: a request arriving while another is
genuinely mid-flight cannot be handed a response that does not exist yet, so
it gets 409. Stripe does the same. The invariant that matters — the bank was
called once — holds either way, and that is what the test asserts.

### Testcontainers, not mocks

Integration tests run against real Postgres. Idempotency and ledger
correctness depend on unique constraints, row locks, deferred triggers and
`ON CONFLICT` semantics that a mock does not reproduce. **A mocked suite
passes while the system double-charges people.**

*First commit of code: `eceb767`.*

---

## Stage 5 — CLAUDE.md, agents, skills

Before building more, a pause to encode the rules.

```
CLAUDE.md                        Karpathy's four rules + non-negotiables
.claude/agents/test-validator    diagnoses failures to root cause
.claude/agents/money-auditor     the invariants ordinary review misses
.claude/agents/senior-architect  design review, added later
.claude/skills/                  run-stack, add-migration, add-payment-feature,
                                 train-fraud-model
```

The non-negotiables — money is never a float, the ledger is append-only,
ambiguous is not failed, PANs only in the vault, time comes from Postgres —
turned out to be the section that caught the most later.

*`59aa1a4`*

---

## Stage 6 — The card vault

Triggered by an update to the design doc adding §2.4. This **changed code
already written**: the Payments API had been taking `card.number` directly,
which §2.4 forbids.

```
cmd/vault/                       the only service that sees a PAN
internal/vault/crypto.go         AES-256-GCM envelope encryption
internal/vault/static/           hosted iframe, payflow.js, card.js
migrations/0002_vault.up.sql
```

The refactor removed the card number from `payments` entirely. `CardMetadata`
has no number field, so exposure is structurally impossible rather than
discouraged.

### The bug found by running it

Charge, then retry with the same key:

```
CHARGE 1: succeeded
CHARGE 2: {"code":"invalid_payment_token"}   ← WRONG
```

`CreateCharge` fetched vault metadata **before** claiming the idempotency key.
A successful charge consumes its single-use token, so the retry died on a
precondition the first attempt had legitimately changed. A merchant retrying a
lost response would conclude the charge failed when it had succeeded.

**Fix:** the idempotency claim became the outermost operation.

**The deeper finding:** `fakeVault.Metadata` did not reject consumed tokens
while the real vault did. That single divergence kept the suite green over the
bug. *A fake more permissive than the real thing is worse than no fake.*

Also wasted a cycle testing a stale binary — `pkill` silently fails on
Windows, so the old process kept serving :8080 and the fix appeared not to
work.

*`2b32fd3`*

---

## Stage 7 — Refunds and disputes

`migrations/0003`, `internal/payments/refund.go`, `dispute.go`.

The hard part is one race: "check the charge has room, then insert a refund"
is check-then-act. Two partial refunds arriving together both read the same
total and together overdraw the charge. Idempotency keys do not help — these
are genuinely different requests.

Closed with `SELECT ... FOR UPDATE` plus a view where **pending** refunds
count toward the committed total.

```
10 concurrent refunds of 2000 on a 10000 charge → exactly 5 succeed
```

Two decisions worth stating: the platform fee is **not** reversed on a refund
(real processors keep it), and a **lost** dispute posts nothing — the money
moved when the dispute opened, so posting again would double-debit.

*`c097229`*

---

## Stage 8 — Fraud: rules, then a model

```
internal/risk/rules.go           11 rules at 422ns
ml/download_data.py              Kaggle datasets
ml/features.py                   feature engineering
ml/training/train.py             XGBoost
ml/serving/app.py                FastAPI
```

### The model that scored at chance

First full run: **PR-AUC 0.0039, ROC-AUC 0.514.** Early stopping halted at
round 1.

Not a tuning problem. `build_features` sorted rows by `cc_num` for the rolling
windows and returned them in that order, while the caller paired the output
with labels in the **original** order.

| Mean amount | Correct | As trained |
|---|---|---|
| Fraud | $554.42 | $63.79 |
| Legitimate | $67.26 | $68.93 |

An 8× separation became noise. Nothing errored; the run completed and wrote
artifacts. After the fix: **0.9735**.

**The heuristic worth keeping:** a model that trains cleanly and scores near
the base rate almost always has a plumbing bug, not a hyperparameter problem.

### Then not believing the 0.97

0.97 on fraud is implausible. Checking the data:

- **84%** of Sparkov's fraud falls between 22:00–04:00, vs 23% of legitimate
- No legitimate `grocery_pos` charge in 1.85M rows exceeds **$290.88**, while
  fraudulent ones start at $237

Sparkov is a **synthetic generator**. The model had reverse-engineered a
simulator. `MODEL_CARD.md` was written to say so rather than quote the number.

A test also caught a real logic bug: `geo_mismatch` counted *pairwise*
disagreements, but one odd country out of three always yields two — so every
traveller would have been flagged. Counting *distinct* countries was the
intent.

*`4ba98a4`*

---

## Stage 9 — Wiring risk into the charge path

Risk placed after the idempotency claim, before the processor. A high-risk
charge never reaches the bank and never decrypts the card, and a retry replays
the stored decline rather than re-scoring against moved counters.

Everything degrades rather than blocks: Redis down silences velocity, a slow
model is treated as down, a circuit breaker sheds load (**20 charges → 5 model
calls**).

*`4f1f06a`*

---

## Stage 10 — Webhooks, and an architecture review

```
migrations/0004_webhooks
internal/webhook/                signing, outbox poller, delivery worker
cmd/webhook-worker/
```

Then a `senior-architect` review of everything built so far, which found
**one critical and five high** issues:

**Critical — a refund left in `requires_reconciliation` released its
reservation.** A second refund under a different key could take a 10000 charge
to 20000. The ledger stayed balanced throughout — only the second refund
posted legs — so no balance assertion could have caught it. It violated a rule
already written in CLAUDE.md: *ambiguous is not failed*, applied on the charge
path and missed on the refund path.

Also: disputes could reverse a non-succeeded charge; dispute dedupe relied on a
`UNIQUE` column permitting NULLs; a vault 503 was burned into a permanent
replayable failure; and the device-cards velocity key used `EXPIRE` instead of
`EXPIRE NX`, sliding its window forever and eventually blocking a shared family
tablet **permanently** — 15 lines below a comment explaining exactly why
`EXPIRE NX` was necessary.

`migrations/0005_invariant_hardening` turned several of these into database
constraints.

*`b32daf6`*

---

## Stage 11 — The ML rebuild, in four passes

A parallel investigation found the most expensive bug so far.

### Pass 1 — the deployed model was not the measured model

`internal/risk` sent **6 of 18 features**; the rest defaulted to `-1` or `NaN`
silently, including `category_encoded`, the second most important feature.

| Vector | Test PR-AUC |
|---|---|
| What the card reported | 0.9735 |
| What production sent | **0.2867** |
| …plus a UTC/local timestamp mismatch | **0.0488** |

Also corrected the *cause*: I had blamed the night concentration, citing
XGBoost's **gain** importance. **Permutation** importance put `is_night` at
0.022, not 0.192 — the real artifact was category-bounded amounts.

Retrained on the 14 features production could supply → 0.9477. *`a2db502`*

### Pass 2 — stop using fake data

Challenged on why a synthetic dataset at all. IEEE-CIS — 590k **real** Vesta
transactions with 20,663 fraud cases, more than twice Sparkov's — had been
downloaded and never used. Chosen for convenience, not judgement.

Real data: **0.1772** with 18 features. *`136a50a`*

### Pass 3 — use more of the data

18 of 434 columns was throwing the dataset away. Widening to ~110 gave
**0.4898**. Adding Vesta's 339 proprietary `V*` features on top gained
**+0.008** — so there is no ceiling being left on the table, and nothing for a
neural network to reach for.

But 0.4898 leaned on `C1`-`C14` and `id_*`, which Vesta **masked** — we can
build a counter of the same shape, but not `C8` specifically, and the model
learns a weight for `C8` specifically.

Trimming to genuinely reproducible columns: **0.1880**. *`3b8c332`*

### Pass 4 — deploy what was measured

21 clean features, Go sends every one, and the service **refuses to start** if
its column list disagrees with the trained model. *`751b6de`*

**Final: PR-AUC 0.188 on real data.** Low, and the model card says so.

---

## Stage 12 — Reconciliation, payouts

```
migrations/0007, 0008
internal/reconcile/              resolves ambiguity, re-derives invariants
internal/payouts/                money out, T+2 hold, dispute reserves
cmd/scheduler/
```

Reconciliation closes the ambiguity loop: the charge path refuses to guess,
this asks the processor afterwards. Discrepancies are **recorded and alerted,
never auto-corrected** — silently fixing a financial mismatch buries the bug.

Payouts were the missing half — everything until now modelled money coming
in. Three things make them unlike a charge: no client idempotency key (the
period constraint plays that role), the money is not gone when we say so
(`in_transit` until the bank confirms), and the payable amount is **not** the
ledger balance (T+2 hold, minus open-dispute reserves).

*`5c3498d`, `754eb0b`*

---

## Stage 13 — README, load test, dashboard

```
README.md
test/load/idempotency.js         k6, asserts no key yields two charge ids
scripts/seed/                    creates a merchant, prints the key once
internal/dashboard/              read-only API, cursor pagination
web/dashboard/                   React + TypeScript SPA
```

Two bugs found by *running* the dashboard, not by testing it: a malformed
cursor returned **500** instead of 400, and the CORS preflight sat inside the
authenticated group where chi answers `OPTIONS` with **405** before group
middleware runs — every browser request would have failed.

TypeScript was chosen deliberately: this project had twice shipped a silent
contract mismatch where both sides compiled while disagreeing.

*`9ff6202`, `b35b9a6`*

---

## Stage 14 — The merchant integration

A checkout page cannot complete a payment on its own: charging needs the
secret key, and a key in a browser can be read by anyone who opens devtools.

Initially built in `cmd/`, then moved to `examples/` after a fair challenge —
**a merchant needs zero changes to our code**, so their server is not
something PayFlow runs. It also revealed that `webhook.Verify` lives in
`internal/` and merchants cannot import it, so the example writes the
signature check out inline: ~10 lines they would actually have to write.

### The CSP that broke its own page

The frame hung on "Loading the secure card frame…" and then timed out.

`script-src 'self'` with an **inline** `<script type="module">`. CSP blocks
inline scripts, so the frame's JavaScript never ran. Fixed by extracting the
script to a file — **not** by adding `unsafe-inline`, since this is the one
document in the system that can see a card number.

---

## Stage 15 — Bots, and rules that were never wired

A Selenium script driving the real checkout, because that is what an attacker
actually does.

### First finding: the rules were dormant

`behavior.js` and `behavior.go` existed, with rules and passing tests. But the
iframe never collected keystrokes, the vault never passed them, and `payments`
never populated `risk.Behavior`. **Six rules, permanently inert.** The tests
passed because they constructed the struct directly — the same class of bug as
the ML feature vector, found the same way.

Wiring it went *through the vault*, not the merchant: only the vault observes
the keystrokes, so only the vault can attest to them.

### Second finding: the rule was looking for the wrong thing

With the chain wired, the paste bot was blocked but the **typing** bot was
not. The stored assessment showed why:

```
bot:    keystroke_interval_cv = 1.059,  mean = 1ms
human:  keystroke_interval_cv = 0.403,  mean = 262ms
```

The bot's coefficient of variation was **higher** than the human's. At a 1ms
mean interval, timer jitter is proportionally enormous, so "suspiciously even"
never fires at machine speed. The **mean** is the signal that does not lie.

`superhuman_keystroke_rate` was added, and two 35-point signals now combine
past the high threshold:

```
1. bot, machine speed   0.2s  →  BLOCKED  score 70
2. human, pauses + typo 8.3s  →  ALLOWED  score 0
3. bot, pasted number   0.1s  →  BLOCKED
```

`migrations/0010_risk_assessments` was added in the same pass — a durable
record of every decision, with the evidence snapshotted, because the vault
token carrying it is purged after 15 minutes and a chargeback arrives weeks
later. It paid for itself immediately: it is what showed the CV numbers above.

---

## What the sequence taught

**Schema first.** Every invariant that survived is one the database enforces.
Every bug that shipped was in application code the database did not constrain.

**Build the fake before the thing that needs it.** The bank simulator existed
before the charge flow, so the ambiguous-timeout path was testable from day
one rather than retrofitted.

**Run it, don't just test it.** Six real bugs were found by running the system
and none by adding assertions to a green suite: the idempotency ordering bug,
the 500-instead-of-400 cursor, the CORS preflight, the CSP inline script, the
dormant behavioral rules, and the CV-versus-mean inversion.

**Distrust a good number.** 0.97 was investigated *because* it was too good.
That investigation found synthetic data, a wrongly-attributed cause, and a
train/serve gap that made the deployed model a third as effective.

**A test double must not be more permissive than the real thing.** Twice, a
green suite covered a broken system for exactly this reason.

**Write the reason down.** The `EXPIRE NX` bug happened 15 lines below a
comment explaining why `EXPIRE NX` was necessary. The comment did not prevent
it — but it is why the reviewer spotted it immediately.
