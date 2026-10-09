-- LLM analysis of text-bearing assets. One row per lens: the
-- workspace lens (campaign_id NULL) and one campaign lens per campaign the
-- asset is attached to. The unique index keys a lens on
-- (asset_id, COALESCE(campaign_id, '')) so the NULL workspace lens is unique
-- too.
--
-- result holds the structured output (key points, topics, entities, the audio
-- meeting fields, the campaign-lens relevance) so the shape can grow without a
-- migration; summary stays a column because every reader needs it.
--
-- context_fingerprint hashes the source version and every prompt input, so a
-- re-run with nothing changed is skipped without a model call.
CREATE TABLE asset_analyses (
    id                  TEXT        PRIMARY KEY,
    tenant_id           TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    asset_id            TEXT        NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    campaign_id         TEXT        NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    status              TEXT        NOT NULL
        CHECK (status IN ('pending', 'running', 'ready', 'failed', 'skipped')),
    strategy            TEXT        NULL CHECK (strategy IN ('full', 'map_reduce')),
    summary             TEXT        NOT NULL DEFAULT '',
    result              JSONB       NOT NULL DEFAULT '{}',
    language            TEXT        NOT NULL DEFAULT '',
    partial_source      BOOLEAN     NOT NULL DEFAULT FALSE,
    truncated           BOOLEAN     NOT NULL DEFAULT FALSE,
    source_tokens       INTEGER     NOT NULL DEFAULT 0,
    context_fingerprint TEXT        NOT NULL DEFAULT '',
    model_id            TEXT        NOT NULL DEFAULT '',
    failure_code        TEXT        NOT NULL DEFAULT '',
    failure_reason      TEXT        NOT NULL DEFAULT '',
    analyzed_at         TIMESTAMPTZ NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX asset_analyses_asset_lens_uq
    ON asset_analyses (asset_id, COALESCE(campaign_id, ''));
CREATE INDEX idx_asset_analyses_campaign
    ON asset_analyses (campaign_id) WHERE campaign_id IS NOT NULL;

-- One row per analysis run that reached the model. A re-run overwrites its
-- lens, so the lens table cannot say how many runs a month had; this ledger
-- backs the asset_analyses_per_month cap. It holds no cost: spend lives in the
-- analytics usage events. analysis_id is not a FK so deleting an asset does
-- not hand its runs back to the monthly allowance.
CREATE TABLE asset_analysis_runs (
    id          TEXT        PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    analysis_id TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_asset_analysis_runs_tenant_created
    ON asset_analysis_runs (tenant_id, created_at);

-- Ideas distilled by an analysis link back to it. SET NULL keeps a triaged
-- idea when its analysis goes (asset deleted, asset detached); untriaged ones
-- are deleted explicitly by those paths before the analysis row goes.
ALTER TABLE ideas
    ADD COLUMN origin            TEXT NOT NULL DEFAULT ''
        CHECK (origin IN ('', 'asset_analysis')),
    ADD COLUMN asset_analysis_id TEXT NULL REFERENCES asset_analyses (id) ON DELETE SET NULL;

CREATE INDEX idx_ideas_asset_analysis
    ON ideas (asset_analysis_id) WHERE asset_analysis_id IS NOT NULL;
