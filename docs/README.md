# PayFlow documentation

| Doc | Contents |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Services, database tables, and the full charge path |
| [BUILD_LOG.md](BUILD_LOG.md) | How the system was built and bugs that shaped it |
| [PAYMENT_TRANSACTION_FLOW.md](PAYMENT_TRANSACTION_FLOW.md) | End-to-end transaction sequence |
| [payment-gateway-system-design_updated.md](payment-gateway-system-design_updated.md) | Original system design (§ references in code point here) |
| [../backend/ml/MODEL_CARD.md](../backend/ml/MODEL_CARD.md) | Fraud model card (features, metrics, failure modes) |
| [../backend/examples/merchant-server/README.md](../backend/examples/merchant-server/README.md) | Sample merchant integration |

## Assets

| Path | Use |
| --- | --- |
| [assets/branding/payflow-banner.png](assets/branding/payflow-banner.png) | README banner |
| [assets/diagrams/payflow-architecture.png](assets/diagrams/payflow-architecture.png) | Architecture diagram |

## Live vs local

- **Dashboard (Vercel):** [https://payflow-mu-amber.vercel.app/](https://payflow-mu-amber.vercel.app/) — merchant UI only; API calls expect a local or configured backend (`VITE_API_URL`).
- **Full gateway:** run `make up` + `make seed` + `make dashboard` from the repo root.
