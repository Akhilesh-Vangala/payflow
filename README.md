<p align="center"><img src="docs/assets/branding/payflow-banner.svg" alt="PayFlow banner" width="100%" /></p>

**Exactly-once charges. Correct ledger.** [Open the dashboard](https://payflow-mu-amber.vercel.app/) · [Architecture](docs/ARCHITECTURE.md) · [Docs index](docs/README.md)

<p align="center">Go · Postgres · Redis · React · Docker Compose</p>

# PayFlow

A payment gateway built so concurrent retries with one idempotency key produce **one bank authorization and one balanced ledger write** — not “once-ish.”

[**Open the dashboard ↗**](https://payflow-mu-amber.vercel.app/) · [Transaction flow](docs/PAYMENT_TRANSACTION_FLOW.md) · [System design](docs/payment-gateway-system-design_updated.md) · [Merchant integration example](backend/examples/merchant-server/README.md)

```
100 concurrent requests · 1 idempotency key → 1 bank authorization, 1 charge
```

## What you get

| Surface | What it is |
| --- | --- |
| **Public dashboard** | Merchant UI (sign-in, keys, charges, refunds, payment links) at [payflow-mu-amber.vercel.app](https://payflow-mu-amber.vercel.app/) |
| **Local full stack** | Payments API, vault, bank simulator, webhooks, fraud scoring, Postgres, Redis — via `make up` |
| **Not included** | Real money movement, sponsor bank, KYC vendor, or PCI QSA — bank is a deterministic simulator |

## Architecture

![PayFlow architecture](docs/assets/diagrams/payflow-architecture.svg)

Card data is entered only in the vault iframe. The Payments API accepts opaque tokens, claims work with `INSERT … ON CONFLICT` on `(merchant_id, idempotency_key)`, and commits the charge, ledger legs, and outbox row in **one** Postgres transaction. Risk rules run first; the fraud model can raise a verdict but never clears a hard block.

```mermaid
%%{init: {"theme": "neutral"}}%%
flowchart LR
  Checkout --> Vault[Vault tokenize]
  Vault --> API[Payments API]
  API --> Risk[Rules then model]
  Risk --> Bank[Bank authorize]
  Bank --> Ledger[Balanced ledger + outbox]
  Ledger --> Hooks[Webhook delivery]
  Hooks --> Recon[Reconcile ambiguous]
```

| Component | Responsibility |
| --- | --- |
| Vault | Only process that ever sees a PAN |
| Payments API | Idempotent charges, refunds, disputes, merchant APIs |
| Ledger | Debits = credits in Go, deferred DB triggers, and reconciliation |
| Risk / ML | Explainable rules first; XGBoost second; fail-open to rules |
| Webhooks / scheduler | Outbox delivery with retry/DLQ; hourly reconcile; daily payouts |
| Dashboard / demo | React merchant UI and checkout iframe demo |

## Design decisions

1. **Postgres is the lock** — not Redis. Claim, ledger, and outbox share one `pgx.Tx`.
2. **The ledger is enforced by the database** — Go check, deferred constraint trigger, append-only entries.
3. **Ambiguous is not failed** — processor timeouts mark `requires_reconciliation`; no blind retry.
4. **A raw card number exists in exactly one service** — vault origin + type system + Luhn scan on the charge API.

## Repository layout

```text
payflow/
├── backend/
│   ├── cmd/            payments-api, vault, bank-simulator, webhook-worker, scheduler
│   ├── internal/       money, ledger, idempotency, payments, vault, risk, reconcile
│   ├── migrations/     numbered SQL
│   ├── ml/             fraud training and serving
│   ├── examples/       merchant-server sample
│   └── test/load/      k6 idempotency script
├── frontend/
│   ├── dashboard/      React + TypeScript merchant UI (Vercel root)
│   └── demo/           checkout page (iframe boundary)
├── docs/               architecture, build log, design notes, assets
├── deploy/             Kubernetes manifests
├── docker-compose.yml
└── Makefile
```

## Run locally

```sh
git clone https://github.com/Akhilesh-Vangala/payflow.git
cd payflow
make up          # full stack
make seed        # prints sk_test_... once
make dashboard   # UI on :5173 (API on :8080)
make test        # unit + integration (Docker / testcontainers)
```

| Service | Port |
| --- | --- |
| Payments API | 8080 |
| Vault | 8081 |
| Fraud scoring | 8000 |
| Bank simulator | 8090 |
| Mailhog UI | 8025 |
| Dashboard | 5173 |

```sh
TOKEN=$(curl -s -X POST localhost:8081/vault/tokenize \
  -H 'Content-Type: application/json' \
  -d '{"number":"4242424242424242","exp_month":12,"exp_year":2030}' \
  | python -c 'import sys,json;print(json.load(sys.stdin)["token"])')

curl -X POST localhost:8080/v1/charges \
  -H "Authorization: Bearer $PAYFLOW_KEY" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H 'Content-Type: application/json' \
  -d "{\"amount\":4500,\"currency\":\"USD\",\"payment_token\":\"$TOKEN\"}"
```

Reuse the same `Idempotency-Key` → same charge body, no second authorization.

## Engineering evidence

| Check | Asserts |
| --- | --- |
| 100 concurrent, one key | 1 bank authorization |
| 10 concurrent refunds of 2000 on a 10000 charge | exactly 5 succeed |
| 20 concurrent chargeback redeliveries | funds reversed once |
| 30 concurrent detokenizations of a single-use token | exactly 1 succeeds |
| 50 concurrent ledger writes to one account | no lost updates |

```sh
make test-idempotency
make test-race
k6 run -e API_KEY=sk_test_... backend/test/load/idempotency.js
```

Fraud: 17 rules (~422ns) then IEEE-CIS XGBoost. Details in [`backend/ml/MODEL_CARD.md`](backend/ml/MODEL_CARD.md).

## Documentation

| Doc | Contents |
| --- | --- |
| [`docs/README.md`](docs/README.md) | Documentation index |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Services, tables, charge flow |
| [`docs/BUILD_LOG.md`](docs/BUILD_LOG.md) | Build order and bugs paid for |
| [`docs/PAYMENT_TRANSACTION_FLOW.md`](docs/PAYMENT_TRANSACTION_FLOW.md) | End-to-end payment path |
| [`backend/ml/MODEL_CARD.md`](backend/ml/MODEL_CARD.md) | Fraud model card |
| [`backend/examples/merchant-server/`](backend/examples/merchant-server/README.md) | Merchant integration sample |

## Contributors

- [Sai Akhilesh Vangala](https://github.com/Akhilesh-Vangala)

See [CONTRIBUTORS.md](CONTRIBUTORS.md).

## License

MIT — Copyright (c) 2026 Sai Akhilesh Vangala
