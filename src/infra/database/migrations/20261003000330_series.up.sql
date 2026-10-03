-- A series is a standing instruction for a recurring kind of post ("Weekly news
-- digest"). campaign_id NULL = the workspace library; set = defined inside that
-- campaign. A series is soft-deleted so posts keep the series_id that produced
-- them; its campaign runs are hard-deleted with it.
--
-- Rhythms are {times, per} jsonb, NULL meaning "occasional" (claims no plan
-- slots). The API validates them; the CHECKs only stop a malformed shape.
CREATE TABLE brand_series (
    id             TEXT        PRIMARY KEY,
    tenant_id      TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name           TEXT        NOT NULL,
    promise        TEXT        NOT NULL DEFAULT '',
    recipe         TEXT        NOT NULL DEFAULT '',
    supply         TEXT        NOT NULL CHECK (supply IN ('self', 'idea')),
    content_format TEXT        NULL,
    default_rhythm JSONB       NULL CHECK (
        default_rhythm IS NULL OR (
            jsonb_typeof(default_rhythm -> 'times') = 'number'
            AND default_rhythm ->> 'per' IN ('week', 'month')
        )
    ),
    campaign_id    TEXT        NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ NULL
);

CREATE INDEX idx_brand_series_tenant_live ON brand_series (tenant_id, created_at) WHERE deleted_at IS NULL;
CREATE INDEX idx_brand_series_campaign ON brand_series (campaign_id) WHERE campaign_id IS NOT NULL;

-- One campaign's run of one series. Written one row at a time (attach, detach,
-- set rhythm); nothing restates the set.
CREATE TABLE campaign_series (
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    campaign_id TEXT        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    series_id   TEXT        NOT NULL REFERENCES brand_series (id) ON DELETE CASCADE,
    rhythm      JSONB       NULL CHECK (
        rhythm IS NULL OR (
            jsonb_typeof(rhythm -> 'times') = 'number'
            AND rhythm ->> 'per' IN ('week', 'month')
        )
    ),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, series_id)
);

CREATE INDEX idx_campaign_series_series ON campaign_series (series_id);

-- Which series produced a post. Series are soft-deleted, so this survives a
-- series delete; SET NULL only covers a hard delete (tenant teardown).
ALTER TABLE posts ADD COLUMN series_id TEXT NULL REFERENCES brand_series (id) ON DELETE SET NULL;
CREATE INDEX idx_posts_series ON posts (series_id) WHERE series_id IS NOT NULL;
