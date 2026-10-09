-- Presigned video uploads that have not been finalized yet.
--
-- Presign records the key it handed out; finalize deletes the row in the same
-- transaction that inserts the post attachment. A row still here past
-- expires_at plus a grace period is an upload the client abandoned, and the
-- sweep_pending_uploads job deletes its object, then the row. The sweep and
-- finalize both take the row lock, so a late finalize either wins (and the
-- sweep finds no row) or loses (and finalize answers upload_expired).
--
-- post_id is SET NULL on post delete: the row, and with it the object's key,
-- outlives the post until the sweep removes the object.
CREATE TABLE pending_uploads (
    id         TEXT        PRIMARY KEY,
    tenant_id  TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    post_id    TEXT        REFERENCES posts (id) ON DELETE SET NULL,
    s3_key     TEXT        NOT NULL UNIQUE,
    size_bytes BIGINT      NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_pending_uploads_expires_at ON pending_uploads (expires_at);
CREATE INDEX idx_pending_uploads_post_id    ON pending_uploads (post_id);
