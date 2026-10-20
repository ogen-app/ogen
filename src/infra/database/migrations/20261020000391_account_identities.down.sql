ALTER TABLE login_alert_tokens DROP COLUMN IF EXISTS method;

DROP TABLE IF EXISTS oauth_pending_signups;
DROP TABLE IF EXISTS oauth_states;

DROP INDEX IF EXISTS idx_accounts_email_lower;

-- An empty hash matches no password, like NULL did.
UPDATE accounts SET password_hash = '' WHERE password_hash IS NULL;
ALTER TABLE accounts ALTER COLUMN password_hash SET NOT NULL;

DROP TABLE IF EXISTS account_identities;
