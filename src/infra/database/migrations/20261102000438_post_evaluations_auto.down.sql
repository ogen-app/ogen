ALTER TABLE post_evaluations
    DROP COLUMN IF EXISTS trigger,
    DROP COLUMN IF EXISTS model_runs,
    DROP COLUMN IF EXISTS context_hash,
    DROP COLUMN IF EXISTS content_hash;
