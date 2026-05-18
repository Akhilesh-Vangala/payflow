-- Merchant self-service: people log in, and API keys are minted from a
-- logged-in session rather than by a seed script (§8).
--
-- The distinction that shapes both tables below: an API KEY authenticates a
-- merchant's SERVER, and a SESSION authenticates a PERSON. They are separate
-- credentials with separate lifetimes and separate blast radii, and the
-- middleware keeps them apart — a leaked API key must not be able to mint
-- another API key, or a read-only key escalates itself to a write key in one
-- request.

CREATE TABLE merchant_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id   UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    email         TEXT        NOT NULL,
    -- bcrypt, NOT the plain SHA-256 used for API keys. The reasoning is
    -- opposite in each case: an API key is a 256-bit random value with no
    -- dictionary to attack, so a fast hash is fine and a fast hash is required
    -- (you cannot index a per-row-salted digest). A password is low-entropy and
    -- human-chosen, so the slow, salted KDF is the whole defence, and there is
    -- no lookup-by-hash problem because we find the row by email first.
    password_hash TEXT        NOT NULL,
    name          TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

-- Case-insensitive: people do not remember whether they signed up with a
-- capital letter, and two accounts differing only in case is an account-takeover
-- vector rather than a feature.
CREATE UNIQUE INDEX idx_merchant_users_email ON merchant_users (lower(email));
CREATE INDEX idx_merchant_users_merchant ON merchant_users (merchant_id);

-- Opaque server-side sessions, not JWTs.
--
-- A JWT cannot be revoked without a server-side denylist, at which point it is
-- a session table with extra steps and a signature-verification CVE surface.
-- Every request in this API already touches Postgres, so the lookup is free in
-- practice, and "log out everywhere" becomes one UPDATE.
CREATE TABLE merchant_sessions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES merchant_users(id) ON DELETE CASCADE,
    -- Only the hash. A dump of this table must not yield working sessions, for
    -- the same reason api_keys stores a hash. SHA-256 is right here: the token
    -- is 256 bits of CSPRNG output, not a password.
    token_hash TEXT        NOT NULL UNIQUE,
    -- Set by the database, not the pod — clock drift on a pod would otherwise
    -- extend or truncate a session's life (see the "time comes from Postgres"
    -- rule).
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Recorded for the session list, so a merchant can spot a login they do not
    -- recognise. Truncated to a /24 would be friendlier to privacy; kept whole
    -- here because it is also the only forensic trail for a takeover.
    ip         TEXT,
    user_agent TEXT
);

CREATE INDEX idx_merchant_sessions_user ON merchant_sessions (user_id)
    WHERE revoked_at IS NULL;

-- Attribution for keys minted from the dashboard. Nullable because keys
-- created by `make seed` have no user behind them.
ALTER TABLE api_keys ADD COLUMN name       TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN created_by UUID REFERENCES merchant_users(id) ON DELETE SET NULL;
ALTER TABLE api_keys ADD COLUMN last_used_at TIMESTAMPTZ;
