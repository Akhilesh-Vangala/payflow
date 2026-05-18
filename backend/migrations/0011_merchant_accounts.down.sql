ALTER TABLE api_keys DROP COLUMN IF EXISTS last_used_at;
ALTER TABLE api_keys DROP COLUMN IF EXISTS created_by;
ALTER TABLE api_keys DROP COLUMN IF EXISTS name;

DROP TABLE IF EXISTS merchant_sessions;
DROP TABLE IF EXISTS merchant_users;
