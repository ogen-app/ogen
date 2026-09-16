-- CON-298: persist Resend delivery/open/click events + a denormalised
-- engagement rollup on email_logs, so Harbor's per-tenant Emails tab (CON-192)
-- can answer "delivered? opened? how many times? when?" without the Resend key.

-- Append-only event timeline. One row per distinct Resend webhook event, keyed
-- for idempotency by the Svix message id (the svix-id header, globally unique and
-- stable across a redelivery of the SAME event) so a redelivery is a no-op.
-- ON DELETE CASCADE means the existing retention sweep (DELETE FROM email_logs
-- WHERE created_at < cutoff) reaps a log's events with it — no separate cleanup.
CREATE TABLE email_events (
    id                  TEXT        PRIMARY KEY,
    email_log_id        TEXT        NOT NULL REFERENCES email_logs (id) ON DELETE CASCADE,
    provider_message_id TEXT,
    type                TEXT        NOT NULL,
    occurred_at         TIMESTAMPTZ NOT NULL,
    svix_id             TEXT        NOT NULL UNIQUE,
    metadata            JSONB,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Timeline reads (GetTenantEmail) and the per-log rollup recompute both scan by
-- (email_log_id, occurred_at).
CREATE INDEX idx_email_events_log_occurred ON email_events (email_log_id, occurred_at);

-- Denormalised engagement rollup for cheap list queries (ListTenantEmails):
-- avoids an events scan per row. Recomputed from email_events on every webhook.
ALTER TABLE email_logs
    ADD COLUMN last_event      TEXT,
    ADD COLUMN last_event_at   TIMESTAMPTZ,
    ADD COLUMN delivered_at    TIMESTAMPTZ,
    ADD COLUMN first_opened_at TIMESTAMPTZ,
    ADD COLUMN opens_count     INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN clicks_count    INTEGER NOT NULL DEFAULT 0;
