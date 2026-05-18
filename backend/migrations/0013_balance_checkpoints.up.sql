-- Turn ledger_balances from a synchronously-maintained cache into a checkpoint.
--
-- WHY
--
-- Every charge called applyBalance once per ledger leg, and each call took an
-- exclusive row lock on ledger_balances held until the transaction committed.
-- Three legs meant three locks, and the third was the platform-wide
-- platform_fees row shared by EVERY merchant. Measured consequences:
--
--   * individual upserts blocking 3.3-6.5 seconds under 100 concurrent workers
--   * throughput flat at 75/64/91/72 TPS across 1/4/12/24 merchants, so
--     sharding by merchant_id could not have fixed it
--   * removing just the fee leg: 75 -> 135 TPS
--
-- The table was paying that cost for nothing. Outside tests, ledger.Balance had
-- no callers, and reconcile was the only reader — it reads the cache precisely
-- to compare it against the derived figure, which this design preserves.
--
-- WHAT CHANGES
--
-- ledger_entries remains the source of truth and remains append-only. The write
-- path now writes entries only. This table becomes a rollup: a balance that is
-- exact as of through_entry_id, advanced by the scheduler.
--
-- Reading a balance becomes:
--
--     checkpoint.balance_cents
--       + SUM(entries WHERE account_id = $1 AND id > checkpoint.through_entry_id)
--
-- which is EXACT, not eventually consistent. The checkpoint only bounds how much
-- of the tail has to be summed at read time. That distinction matters: payouts
-- must never pay out against a stale balance, and with this design they do not.
ALTER TABLE ledger_balances
    ADD COLUMN through_entry_id BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN ledger_balances.through_entry_id IS
    'balance_cents is exact as of this ledger_entries.id. Entries after it must '
    'be summed at read time. Advanced by the scheduler; never by the charge path.';

-- Rebuild every balance from the entries, so the checkpoint starts exact rather
-- than inheriting whatever the incremental cache had accumulated.
--
-- Snapshotting max(id) first is what makes this safe to run against a live
-- system: entries written during the migration land after the snapshot and are
-- picked up by the read-time sum, rather than being double-counted or lost.
WITH snapshot AS (
    SELECT COALESCE(max(id), 0) AS max_id FROM ledger_entries
),
sums AS (
    SELECT e.account_id,
           MIN(e.currency) AS currency,
           SUM(CASE WHEN e.direction = 'debit' THEN e.amount_cents
                    ELSE -e.amount_cents END) AS balance_cents
    FROM ledger_entries e, snapshot s
    WHERE e.id <= s.max_id
    GROUP BY e.account_id
)
INSERT INTO ledger_balances (account_id, balance_cents, currency, version, updated_at, through_entry_id)
SELECT sums.account_id, sums.balance_cents, sums.currency, 1, now(), snapshot.max_id
FROM sums, snapshot
ON CONFLICT (account_id) DO UPDATE
SET balance_cents    = EXCLUDED.balance_cents,
    through_entry_id = EXCLUDED.through_entry_id,
    version          = ledger_balances.version + 1,
    updated_at       = now();

-- Accounts that exist but have no entries yet: give them a checkpoint row so the
-- read path never has to distinguish "no balance row" from "zero balance".
INSERT INTO ledger_balances (account_id, balance_cents, currency, version, updated_at, through_entry_id)
SELECT a.id, 0, a.currency, 1, now(), COALESCE((SELECT max(id) FROM ledger_entries), 0)
FROM accounts a
ON CONFLICT (account_id) DO NOTHING;

-- The read-time tail sum is `WHERE account_id = $1 AND id > $2`. The existing
-- idx_ledger_account is (account_id, created_at) — wrong trailing column for
-- this predicate, which would force a filter over every entry for the account.
CREATE INDEX idx_ledger_account_id ON ledger_entries (account_id, id);
