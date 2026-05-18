DROP INDEX IF EXISTS idx_ledger_account_id;
ALTER TABLE ledger_balances DROP COLUMN IF EXISTS through_entry_id;
