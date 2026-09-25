-- CON-312 rollback.
ALTER TABLE audio_extractions DROP COLUMN IF EXISTS failure_code;
ALTER TABLE assets
    DROP COLUMN IF EXISTS failure_reason,
    DROP COLUMN IF EXISTS failure_code;
