-- sessions had only its primary key. Revoking an account's sessions (password
-- reset, secure-account) filters account_id, deleting a user cascades on
-- user_id, and the daily cleanup deletes by expires_at; each scanned the table.
CREATE INDEX IF NOT EXISTS idx_sessions_account_id ON sessions (account_id);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id    ON sessions (user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions (expires_at);
