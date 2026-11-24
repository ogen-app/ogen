DROP INDEX IF EXISTS tenants_setup_status_idx;
ALTER TABLE tenants
    DROP COLUMN IF EXISTS setup_status,
    DROP COLUMN IF EXISTS setup_step,
    DROP COLUMN IF EXISTS setup_reached,
    DROP COLUMN IF EXISTS setup_role,
    DROP COLUMN IF EXISTS setup_run_id,
    DROP COLUMN IF EXISTS setup_updated_at,
    DROP COLUMN IF EXISTS setup_finished_by,
    DROP COLUMN IF EXISTS setup_finished_at;
