# PayFlow documentation

| Doc | Contents |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Services, database tables, and the full charge path |
| [BUILD_LOG.md](BUILD_LOG.md) | How the system was built and bugs that shaped it |
| [PAYMENT_TRANSACTION_FLOW.md](PAYMENT_TRANSACTION_FLOW.md) | End-to-end transaction sequence |
| [payment-gateway-system-design_updated.md](payment-gateway-system-design_updated.md) | Design note the code comments point at. [payment-gateway-system-design.md](payment-gateway-system-design.md) is the earlier draft. |
| [../backend/ml/MODEL_CARD.md](../backend/ml/MODEL_CARD.md) | Fraud model card (features, metrics, failure modes) |
| [../backend/examples/merchant-server/README.md](../backend/examples/merchant-server/README.md) | Sample merchant integration |

## Diagram

[Architecture](assets/diagrams/payflow-architecture.png): vault, payments API, ledger, risk, webhooks.

## Live vs local

- **Dashboard (Vercel):** [https://payflow-mu-amber.vercel.app/](https://payflow-mu-amber.vercel.app/) — merchant UI only; API calls expect a local or configured backend (`VITE_API_URL`).
- **Full gateway:** run `make up` + `make seed` + `make dashboard` from the repo root.
