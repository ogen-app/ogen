ALTER TABLE assistant_messages
    DROP COLUMN IF EXISTS seq;

CREATE INDEX IF NOT EXISTS idx_assistant_messages_thread
    ON assistant_messages (tenant_id, scope_kind, scope_id, created_at DESC);
