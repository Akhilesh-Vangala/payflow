DROP INDEX IF EXISTS idx_ledger_created_at;
ALTER TABLE reconciliation_runs DROP COLUMN IF EXISTS ledger_checked_through;
