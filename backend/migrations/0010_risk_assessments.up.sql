-- Durable record of every risk decision (§14.5, §14.4).
--
-- The charges table already carries a summary — level, score, rules fired —
-- but three things make that insufficient:
--
--   1. The evidence lives on the vault token, which is purged after 15
--      minutes. By the time a chargeback arrives weeks later, the behavioural
--      data that justified the decision is gone.
--   2. Reviewing flagged traffic means scanning charges, which is the hot
--      table the charge path writes to. A separate table keeps analyst
--      queries off it.
--   3. Chargeback outcomes are the fraud model's training labels (§14.3).
--      Without the feature values recorded at decision time, a confirmed
--      fraud case teaches nothing.
--
-- Append-only in spirit: rows are written once at decision time, and only the
-- review columns are ever updated.

CREATE TABLE risk_assessments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    charge_id    UUID        NOT NULL REFERENCES charges(id) ON DELETE CASCADE,
    merchant_id  UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,

    level        TEXT        NOT NULL CHECK (level IN ('low','medium','high')),
    rule_score   NUMERIC(6,2) NOT NULL DEFAULT 0,
    rules_fired  JSONB       NOT NULL DEFAULT '[]'::jsonb,
    reason       TEXT,

    model_score   NUMERIC(7,6),
    model_version TEXT,
    -- Distinguishes "the model said low" from "no model score contributed",
    -- which the summary on charges cannot express.
    model_skipped BOOLEAN     NOT NULL DEFAULT false,

    -- The evidence, snapshotted. Aggregates only, never per-keystroke timings.
    behavior     JSONB,
    velocity     JSONB,

    card_fingerprint TEXT,
    card_bin         CHAR(6),
    ip_address       INET,
    device_id        TEXT,

    -- What actually happened, so precision can be measured after the fact.
    action_taken TEXT        NOT NULL CHECK (action_taken IN ('allowed','declined','challenged')),
    latency_ms   NUMERIC(8,3),

    -- Human review. A decline that turns out to be wrong is how thresholds get
    -- tuned; a charge that is later disputed is a confirmed label.
    reviewed_at  TIMESTAMPTZ,
    reviewed_by  TEXT,
    verdict      TEXT CHECK (verdict IN ('confirmed_fraud','false_positive','inconclusive')),
    review_note  TEXT,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One assessment per charge; a retry replays rather than re-scoring, so a
-- second row would mean the idempotency guarantee was broken.
CREATE UNIQUE INDEX uq_risk_assessments_charge ON risk_assessments (charge_id);

-- The analyst worklist: flagged and not yet reviewed. Partial, so it stays
-- small as low-risk assessments accumulate into the millions.
CREATE INDEX idx_risk_review_queue ON risk_assessments (created_at DESC)
    WHERE level <> 'low' AND reviewed_at IS NULL;

CREATE INDEX idx_risk_merchant ON risk_assessments (merchant_id, created_at DESC);
-- Entity lookups for the graph work in §14.4: everything this card, IP or
-- device has ever been scored on.
CREATE INDEX idx_risk_fingerprint ON risk_assessments (card_fingerprint, created_at DESC);
CREATE INDEX idx_risk_device ON risk_assessments (device_id, created_at DESC)
    WHERE device_id IS NOT NULL;

-- The analyst's queue, and the same query a review UI would run.
CREATE VIEW risk_review_queue AS
SELECT a.id, a.charge_id, a.merchant_id, a.level, a.rule_score, a.rules_fired,
       a.reason, a.model_score, a.action_taken, a.behavior, a.velocity,
       a.card_fingerprint, a.ip_address, a.device_id, a.created_at,
       c.amount_cents, c.currency, c.status AS charge_status,
       c.card_brand, c.card_last4,
       -- A dispute is the ground truth this row was predicting.
       EXISTS (SELECT 1 FROM disputes d WHERE d.charge_id = a.charge_id) AS disputed
FROM risk_assessments a
JOIN charges c ON c.id = a.charge_id
WHERE a.reviewed_at IS NULL AND a.level <> 'low'
ORDER BY a.created_at DESC;
