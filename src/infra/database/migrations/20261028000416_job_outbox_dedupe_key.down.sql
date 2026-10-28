DROP INDEX IF EXISTS job_outbox_dedupe_key_idx;
ALTER TABLE job_outbox DROP COLUMN IF EXISTS dedupe_key;
