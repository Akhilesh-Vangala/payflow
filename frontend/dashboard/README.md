# PayFlow merchant dashboard

React + TypeScript + Vite UI for merchants (sign-in, API keys, charges, refunds, payment links).

## Local

From the repo root (recommended):

```sh
make up
make seed
make dashboard
```

Or from this directory after the API is up:

```sh
npm ci
npm run dev
```

Default API base: `http://localhost:8080` (override with `VITE_API_URL`).

## Production (Vercel)

This folder is the Vercel project root for [payflow-mu-amber.vercel.app](https://payflow-mu-amber.vercel.app/). The hosted build is the UI only; charge APIs still require the local Compose stack (or a configured backend URL).
