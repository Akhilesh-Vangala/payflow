-- Hosted payment links (§2.2 extension).
--
-- What this exists for: a merchant with no server at all. The normal
-- integration needs a backend, because charging requires a secret key and a
-- secret key cannot live in a browser. A payment link removes that requirement
-- by moving the server-side half onto PayFlow — the customer lands on a page we
-- serve, the card goes to our vault, and the charge is created by us, on the
-- merchant's behalf, from the merchant_id on the link row.
--
-- Two tables, not one, and the split is load-bearing.

-- A LINK is a reusable, merchant-authored artefact: a price and a description
-- with a public URL. It is not a payment.
CREATE TABLE payment_links (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    -- The public identifier that appears in the URL. Separate from the primary
    -- key on purpose: the id is internal and appears in dashboard URLs and
    -- logs, while this is handed to strangers. Random, not sequential — a
    -- guessable code would let anyone enumerate every merchant's prices.
    code        TEXT        NOT NULL UNIQUE,

    -- The price is stored HERE, and this is the single most important line in
    -- the file. The confirm endpoint is public and unauthenticated; if the
    -- amount came from the request body, anyone could pay one cent for
    -- anything. The client never sends an amount, and there is no code path
    -- that reads one.
    amount_cents BIGINT     NOT NULL CHECK (amount_cents > 0),
    currency     CHAR(3)    NOT NULL,

    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',

    -- Where the customer is sent afterwards. Optional: with neither set, the
    -- hosted page shows its own receipt, which is what a merchant without a
    -- website needs.
    success_url TEXT,
    cancel_url  TEXT,

    active      BOOLEAN     NOT NULL DEFAULT true,
    -- Deactivation rather than deletion. A link that has taken money is part of
    -- the audit trail for those charges and must remain resolvable.
    archived_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  UUID REFERENCES merchant_users(id) ON DELETE SET NULL
);

CREATE INDEX idx_payment_links_merchant ON payment_links (merchant_id, created_at DESC);

-- A CHECKOUT SESSION is one customer's one attempt at one link.
--
-- It exists so the attempt has an identity BEFORE the money moves. That
-- identity becomes the idempotency key for the charge, which is what makes a
-- double-clicked pay button, a refreshed tab, and a retried fetch all resolve
-- to a single charge. Generating a fresh key per submit would defeat exactly
-- the guarantee this system is built around.
CREATE TABLE checkout_sessions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    link_id     UUID        NOT NULL REFERENCES payment_links(id) ON DELETE CASCADE,
    merchant_id UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,

    -- Copied from the link at session creation, not read through at confirm
    -- time. A merchant editing the price must not change what a customer
    -- already saw on screen and agreed to pay.
    amount_cents BIGINT     NOT NULL CHECK (amount_cents > 0),
    currency     CHAR(3)    NOT NULL,

    -- 'processing' is the ambiguous case and it is not a convenience: it means
    -- a charge exists whose outcome the processor never confirmed. It must be
    -- distinguishable from both 'open' (nothing was attempted, safe to retry)
    -- and 'failed' (no money moved), because the money may in fact have moved
    -- and a retry from this state would take it twice. Reconciliation moves it
    -- to completed or failed.
    status      TEXT        NOT NULL DEFAULT 'open'
                CHECK (status IN ('open', 'processing', 'completed', 'failed', 'expired')),
    -- Set once the charge exists. Nullable while open, and never cleared.
    charge_id   UUID REFERENCES charges(id),

    -- From Postgres, never a pod clock.
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '30 minutes',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,

    ip          TEXT,
    user_agent  TEXT
);

CREATE INDEX idx_checkout_sessions_link ON checkout_sessions (link_id, created_at DESC);
CREATE INDEX idx_checkout_sessions_open ON checkout_sessions (expires_at)
    WHERE status = 'open';

-- One charge belongs to at most one checkout session. Without this a bug in the
-- confirm path could attach two sessions to the same charge and the link's
-- reported takings would double-count real money.
CREATE UNIQUE INDEX idx_checkout_sessions_charge
    ON checkout_sessions (charge_id) WHERE charge_id IS NOT NULL;
