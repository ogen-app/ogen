ALTER TABLE brand_distill_runs
    DROP COLUMN IF EXISTS run_token,
    DROP COLUMN IF EXISTS run_heartbeat_at,
    DROP COLUMN IF EXISTS run_done_at,
    DROP COLUMN IF EXISTS run_error,
    DROP COLUMN IF EXISTS run_error_code;

ALTER TABLE idea_distill_runs
    DROP COLUMN IF EXISTS run_token,
    DROP COLUMN IF EXISTS run_heartbeat_at,
    DROP COLUMN IF EXISTS run_done_at,
    DROP COLUMN IF EXISTS run_error,
    DROP COLUMN IF EXISTS run_error_code;
