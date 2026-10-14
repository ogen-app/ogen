ALTER TABLE asset_analyses
    DROP COLUMN IF EXISTS run_token,
    DROP COLUMN IF EXISTS run_heartbeat_at,
    DROP COLUMN IF EXISTS run_done_at,
    DROP COLUMN IF EXISTS run_error,
    DROP COLUMN IF EXISTS run_error_code,
    DROP COLUMN IF EXISTS last_activity_at;
