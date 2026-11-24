-- A run can be read one source at a time. An incremental run holds the slot
-- while sources are added; each source is a read: a row of its own whose
-- read_of is the run, read on its own and stopped on its own. Coverage is
-- what the sources read so far give each section, kept on the run and on
-- each read.
ALTER TABLE brand_distill_runs
    ADD COLUMN incremental BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN read_of     TEXT    NULL REFERENCES brand_distill_runs (id) ON DELETE CASCADE,
    ADD COLUMN coverage    JSONB   NULL;

CREATE INDEX idx_brand_distill_runs_read_of
    ON brand_distill_runs (read_of, created_at) WHERE read_of IS NOT NULL;

-- Reads run beside their run, so only runs hold the one open slot.
DROP INDEX brand_distill_runs_open_uq;
CREATE UNIQUE INDEX brand_distill_runs_open_uq
    ON brand_distill_runs (tenant_id)
    WHERE read_of IS NULL AND status IN ('pending', 'collecting', 'reading', 'composing');
