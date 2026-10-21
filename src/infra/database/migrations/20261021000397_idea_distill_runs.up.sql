-- A distill run asks one ready analysis lens for more ideas, on top of the
-- ones it already has. It belongs to the lens and goes with it. The partial
-- unique index allows one waiting or running run per lens.
--
-- focus is the user's optional direction for the run; requested_count is how
-- many ideas were asked for and ideas_count how many were kept.
CREATE TABLE idea_distill_runs (
    id              TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    analysis_id     TEXT        NOT NULL REFERENCES asset_analyses (id) ON DELETE CASCADE,
    asset_id        TEXT        NOT NULL,
    campaign_id     TEXT        NULL,
    status          TEXT        NOT NULL
        CHECK (status IN ('pending', 'running', 'ready', 'failed', 'skipped')),
    focus           TEXT        NOT NULL DEFAULT '',
    requested_count INTEGER     NOT NULL CHECK (requested_count BETWEEN 1 AND 10),
    ideas_count     INTEGER     NOT NULL DEFAULT 0,
    model_id        TEXT        NOT NULL DEFAULT '',
    failure_code    TEXT        NOT NULL DEFAULT '',
    failure_reason  TEXT        NOT NULL DEFAULT '',
    created_by      TEXT        NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ NULL
);

CREATE UNIQUE INDEX idea_distill_runs_active_uq
    ON idea_distill_runs (analysis_id) WHERE status IN ('pending', 'running');
CREATE INDEX idx_idea_distill_runs_analysis_created
    ON idea_distill_runs (analysis_id, created_at DESC);

-- One row per distill run that reached the model, for the
-- idea_distills_per_month cap. Like asset_analysis_runs it has no FK to the
-- run, so deleting an asset does not hand runs back to the allowance.
CREATE TABLE idea_distill_ledger (
    id         TEXT        PRIMARY KEY,
    tenant_id  TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    run_id     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_idea_distill_ledger_tenant_created
    ON idea_distill_ledger (tenant_id, created_at);

-- A distilled idea names its run. A full re-run of the lens replaces only the
-- ideas without one, so ideas the user asked for are kept.
ALTER TABLE ideas
    ADD COLUMN distill_run_id TEXT NULL REFERENCES idea_distill_runs (id) ON DELETE SET NULL;

CREATE INDEX idx_ideas_distill_run
    ON ideas (distill_run_id) WHERE distill_run_id IS NOT NULL;
