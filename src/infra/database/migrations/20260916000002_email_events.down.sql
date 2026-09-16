-- CON-298 rollback.
ALTER TABLE email_logs
    DROP COLUMN IF EXISTS last_event,
    DROP COLUMN IF EXISTS last_event_at,
    DROP COLUMN IF EXISTS delivered_at,
    DROP COLUMN IF EXISTS first_opened_at,
    DROP COLUMN IF EXISTS opens_count,
    DROP COLUMN IF EXISTS clicks_count;

DROP TABLE IF EXISTS email_events;
