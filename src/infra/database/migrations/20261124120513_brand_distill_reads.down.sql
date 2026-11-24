DELETE FROM brand_distill_runs WHERE read_of IS NOT NULL;
DROP INDEX brand_distill_runs_open_uq;
CREATE UNIQUE INDEX brand_distill_runs_open_uq
    ON brand_distill_runs (tenant_id) WHERE status IN ('pending', 'collecting', 'reading', 'composing');
DROP INDEX IF EXISTS idx_brand_distill_runs_read_of;
ALTER TABLE brand_distill_runs
    DROP COLUMN IF EXISTS coverage,
    DROP COLUMN IF EXISTS read_of,
    DROP COLUMN IF EXISTS incremental;
