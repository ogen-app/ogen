-- A job written with a dedupe key is written once: a second write with the
-- same key, while the first row is still kept (OUTBOX_RETENTION_DAYS), is
-- dropped. The receipt notification uses it so a webhook Resend redelivers
-- does not notify Harbor twice.
ALTER TABLE job_outbox ADD COLUMN IF NOT EXISTS dedupe_key text;

CREATE UNIQUE INDEX IF NOT EXISTS job_outbox_dedupe_key_idx
    ON job_outbox (dedupe_key)
    WHERE dedupe_key IS NOT NULL;
