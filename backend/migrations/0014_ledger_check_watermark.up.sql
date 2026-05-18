-- Bound the hourly ledger invariant check to what has not been checked yet.
--
-- THE PROBLEM
--
-- reconcile.checkLedger aggregates the ENTIRE ledger_entries table with no
-- WHERE, no time bound and no LIMIT, every hour, to find transactions whose
-- debits do not equal their credits. Measured at 233,437 entries: 1,211 ms.
-- The cost grows linearly with all history ever recorded — ~50 seconds at 10M
-- entries, and past a certain size it would not finish before the next tick.
--
-- WHY IT CAN BE BOUNDED SAFELY
--
-- ledger_entries is append-only, enforced by trg_ledger_entries_no_update which
-- raises on any UPDATE or DELETE. A transaction that has once been verified
-- balanced therefore cannot become unbalanced later: nothing can change its
-- rows, and any new row carries a new transaction_id.
--
-- So "already checked" is a permanent property, and the check only has to
-- consider entries it has not seen.
--
-- THE SUBTLETY THAT MAKES A NAIVE WATERMARK WRONG
--
-- BIGSERIAL ids are handed out at INSERT time, but rows become visible at
-- COMMIT time. A long-running transaction can therefore commit an entry whose
-- id is BELOW a watermark recorded by a scan that ran while it was still open —
-- and that entry would never be checked.
--
-- The fix is an overlap rather than a bare high-water mark: each run examines
-- everything above the watermark AND everything from the last hour, whichever
-- is wider. The trailing window is bounded by throughput rather than by total
-- history, so the query stays O(recent) while covering anything that was in
-- flight during the previous run.
ALTER TABLE reconciliation_runs
    ADD COLUMN ledger_checked_through BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN reconciliation_runs.ledger_checked_through IS
    'Highest ledger_entries.id this run verified. The next run starts from the '
    'largest value across completed runs, and additionally re-checks a trailing '
    'window to cover transactions that were in flight.';

-- Supports the trailing-window half of the predicate. The existing
-- idx_ledger_account (account_id, created_at) leads with the wrong column for a
-- scan that filters on time alone.
CREATE INDEX idx_ledger_created_at ON ledger_entries (created_at);
