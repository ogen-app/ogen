-- A brand distillation run reads a website, assets and/or answers and
-- proposes voices, audiences, guardrail lines and facts. Nothing it
-- proposes is brand material until the user commits it.
--
-- sources/answers/candidates/proposal are the run's working state; a periodic
-- sweep nulls candidates and proposal once an uncommitted run expires.
-- parent_run_id/child_run_id chain a refine (answers to gap questions) to the
-- run it superseded; depth counts the refines from the first run.
-- The partial unique index allows one open run per tenant.
CREATE TABLE brand_distill_runs (
    id             TEXT        PRIMARY KEY,
    tenant_id      TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    parent_run_id  TEXT        NULL REFERENCES brand_distill_runs (id) ON DELETE SET NULL,
    child_run_id   TEXT        NULL,
    depth          INTEGER     NOT NULL DEFAULT 0,
    status         TEXT        NOT NULL
        CHECK (status IN ('pending', 'collecting', 'reading', 'composing', 'ready',
                          'committed', 'superseded', 'discarded', 'expired', 'failed')),
    website_url    TEXT        NULL,
    website        JSONB       NULL,
    sources        JSONB       NOT NULL DEFAULT '[]',
    answers        JSONB       NOT NULL DEFAULT '[]',
    candidates     JSONB       NULL,
    proposal       JSONB       NULL,
    model_ids      JSONB       NOT NULL DEFAULT '{}',
    charged        BOOLEAN     NOT NULL DEFAULT false,
    failure_code   TEXT        NOT NULL DEFAULT '',
    failure_reason TEXT        NOT NULL DEFAULT '',
    created_by     TEXT        NULL,
    committed_by   TEXT        NULL,
    committed_at   TIMESTAMPTZ NULL,
    committed_ids  JSONB       NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ NULL
);

CREATE UNIQUE INDEX brand_distill_runs_open_uq
    ON brand_distill_runs (tenant_id) WHERE status IN ('pending', 'collecting', 'reading', 'composing');
CREATE INDEX idx_brand_distill_runs_tenant_created
    ON brand_distill_runs (tenant_id, created_at DESC);
CREATE INDEX idx_brand_distill_runs_ready_finished
    ON brand_distill_runs (finished_at) WHERE status = 'ready';

-- One row per charged run, for the brand_distills_per_month cap. Like the
-- other ledgers it has no FK to the run, so discarding a run does not hand it
-- back to the allowance.
CREATE TABLE brand_distill_ledger (
    id         TEXT        PRIMARY KEY,
    tenant_id  TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    run_id     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_brand_distill_ledger_tenant_created
    ON brand_distill_ledger (tenant_id, created_at);
