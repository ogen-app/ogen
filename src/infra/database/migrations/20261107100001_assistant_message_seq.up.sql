-- Insertion order within a thread. A turn's messages are written in one
-- batch, so created_at alone can tie; seq breaks the tie in batch order.
ALTER TABLE assistant_messages
    ADD COLUMN seq BIGSERIAL;

DROP INDEX IF EXISTS idx_assistant_messages_thread;
CREATE INDEX idx_assistant_messages_thread
    ON assistant_messages (tenant_id, scope_kind, scope_id, created_at DESC, seq DESC);
